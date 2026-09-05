// mcp_client.mjs — a minimal MCP client over stdio for chrome-devtools-mcp.
//
// Speaks JSON-RPC 2.0, one message per line, with no dependencies (Node 20.19+). Also holds
// the parsers for the two WebMCP tools of chrome-devtools-mcp 1.8.0, whose results are
// text: `list_webmcp_tools` prints `name="x", description="...", inputSchema=..., annotations=...`
// lines under a `## WebMCP tools` heading, and `execute_webmcp_tool` prints
// JSON.stringify({status, output, errorText}) where `output` is the string Chrome produced
// by serializing the page tool's return value. Everything is parsed defensively.

import { spawn } from 'node:child_process';

/** Package spec passed to `npx -y`. Pinned: the result formats above are version specific. */
export const CHROME_DEVTOOLS_MCP = 'chrome-devtools-mcp@1.8.0';

/** Protocol version offered in `initialize`; the 1.8.0 server accepts 2025-06-18 and newer. */
export const PROTOCOL_VERSION = '2025-06-18';

const DEFAULT_TIMEOUT_MS = 60_000;
const KILL_GRACE_MS = 2_000;

/**
 * McpClient owns one chrome-devtools-mcp child process and multiplexes JSON-RPC requests
 * over its stdin/stdout. Create it with McpClient.spawn and always call close().
 */
export class McpClient {
  #child;
  #nextId = 1;
  #pending = new Map();
  #buffer = '';
  #stderr = [];
  #exited = null;
  #verbose;

  /**
   * spawn starts `npx -y chrome-devtools-mcp@1.8.0 <args>` with usage statistics disabled.
   * @param {string[]} args CLI flags for chrome-devtools-mcp.
   * @param {{verbose?: boolean, env?: Record<string,string>}} [options]
   */
  static spawn(args, { verbose = false, env = {} } = {}) {
    const child = spawn('npx', ['-y', CHROME_DEVTOOLS_MCP, ...args], {
      stdio: ['pipe', 'pipe', 'pipe'],
      env: { ...process.env, CHROME_DEVTOOLS_MCP_NO_USAGE_STATISTICS: '1', ...env },
    });
    return new McpClient(child, { verbose });
  }

  constructor(child, { verbose = false } = {}) {
    this.#child = child;
    this.#verbose = verbose;
    child.stdout.setEncoding('utf8');
    child.stdout.on('data', (chunk) => this.#onStdout(chunk));
    child.stderr.setEncoding('utf8');
    child.stderr.on('data', (chunk) => {
      this.#stderr.push(chunk);
      if (this.#stderr.length > 200) this.#stderr.shift();
      if (verbose) process.stderr.write('[mcp stderr] ' + chunk);
    });
    child.on('exit', (code, signal) => {
      this.#exited = { code, signal };
      const err = new Error(`chrome-devtools-mcp exited (code=${code}, signal=${signal})\n${this.stderrTail()}`);
      for (const p of this.#pending.values()) p.reject(err);
      this.#pending.clear();
    });
    child.on('error', (err) => {
      for (const p of this.#pending.values()) p.reject(err);
      this.#pending.clear();
    });
  }

  /** stderrTail returns the last lines the server wrote to stderr, for diagnostics. */
  stderrTail(lines = 20) {
    return this.#stderr.join('').split('\n').filter(Boolean).slice(-lines).join('\n');
  }

  #onStdout(chunk) {
    this.#buffer += chunk;
    let nl;
    while ((nl = this.#buffer.indexOf('\n')) >= 0) {
      const line = this.#buffer.slice(0, nl).trim();
      this.#buffer = this.#buffer.slice(nl + 1);
      if (!line) continue;
      let msg;
      try {
        msg = JSON.parse(line);
      } catch {
        if (this.#verbose) process.stderr.write('[mcp stdout, not JSON] ' + line + '\n');
        continue;
      }
      if (this.#verbose) process.stderr.write('[mcp <-] ' + line.slice(0, 2000) + '\n');
      if (msg.id !== undefined && this.#pending.has(msg.id)) {
        const p = this.#pending.get(msg.id);
        this.#pending.delete(msg.id);
        clearTimeout(p.timer);
        if (msg.error) {
          p.reject(new Error(`${p.method}: ${msg.error.message ?? JSON.stringify(msg.error)}`));
        } else {
          p.resolve(msg.result);
        }
      }
      // Notifications and server-initiated requests are ignored in this client.
    }
  }

  #send(msg) {
    if (this.#exited) throw new Error('chrome-devtools-mcp already exited');
    const line = JSON.stringify(msg);
    if (this.#verbose) process.stderr.write('[mcp ->] ' + line.slice(0, 2000) + '\n');
    this.#child.stdin.write(line + '\n');
  }

  /** request sends a JSON-RPC request and resolves with its `result`. */
  request(method, params = {}, timeoutMs = DEFAULT_TIMEOUT_MS) {
    const id = this.#nextId++;
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        this.#pending.delete(id);
        reject(new Error(`${method}: no response after ${timeoutMs} ms\n${this.stderrTail()}`));
      }, timeoutMs);
      this.#pending.set(id, { method, resolve, reject, timer });
      try {
        this.#send({ jsonrpc: '2.0', id, method, params });
      } catch (err) {
        clearTimeout(timer);
        this.#pending.delete(id);
        reject(err);
      }
    });
  }

  /** notify sends a JSON-RPC notification (no id, no response). */
  notify(method, params = {}) {
    this.#send({ jsonrpc: '2.0', method, params });
  }

  /** initialize performs the MCP handshake and returns the server's initialize result. */
  async initialize() {
    const result = await this.request('initialize', {
      protocolVersion: PROTOCOL_VERSION,
      capabilities: {},
      clientInfo: { name: 'agents-board-e2e', version: '1.0.0' },
    });
    this.notify('notifications/initialized', {});
    return result;
  }

  /** listTools returns the server's tool definitions. */
  async listTools() {
    const result = await this.request('tools/list', {});
    return result?.tools ?? [];
  }

  /**
   * callTool invokes a server tool and returns `{result, text}`. Throws when the server
   * flags the call `isError` so scenario code reads failures as exceptions.
   */
  async callTool(name, args = {}, timeoutMs = DEFAULT_TIMEOUT_MS) {
    const result = await this.request('tools/call', { name, arguments: args }, timeoutMs);
    const text = textOf(result);
    if (result?.isError) {
      throw new Error(`${name} failed: ${text.slice(0, 800)}`);
    }
    return { result, text };
  }

  /** close terminates the child process (SIGTERM, then SIGKILL after a grace period). */
  async close() {
    if (this.#exited) return;
    for (const p of this.#pending.values()) clearTimeout(p.timer);
    this.#pending.clear();
    try {
      this.#child.stdin.end();
    } catch {
      // stdin may already be closed
    }
    this.#child.kill('SIGTERM');
    await new Promise((resolve) => {
      const timer = setTimeout(() => {
        this.#child.kill('SIGKILL');
        resolve();
      }, KILL_GRACE_MS);
      this.#child.once('exit', () => {
        clearTimeout(timer);
        resolve();
      });
    });
  }
}

/** textOf joins the text parts of an MCP tool result. */
export function textOf(result) {
  const content = Array.isArray(result?.content) ? result.content : [];
  return content.filter((c) => c && c.type === 'text' && typeof c.text === 'string').map((c) => c.text).join('\n');
}

/**
 * extractJsonObjects finds every top-level `{...}` in free text and returns the ones that
 * parse as JSON. Braces inside string literals are skipped. Used because chrome-devtools-mcp
 * wraps JSON in prose and markdown fences.
 */
export function extractJsonObjects(text) {
  const out = [];
  let depth = 0;
  let start = -1;
  let inString = false;
  let escaped = false;
  for (let i = 0; i < text.length; i++) {
    const ch = text[i];
    if (inString) {
      if (escaped) escaped = false;
      else if (ch === '\\') escaped = true;
      else if (ch === '"') inString = false;
      continue;
    }
    if (ch === '"' && depth > 0) {
      inString = true;
    } else if (ch === '{') {
      if (depth === 0) start = i;
      depth++;
    } else if (ch === '}' && depth > 0) {
      depth--;
      if (depth === 0) {
        try {
          out.push(JSON.parse(text.slice(start, i + 1)));
        } catch {
          // not JSON after all; keep scanning
        }
        start = -1;
      }
    }
  }
  return out;
}

/**
 * parseSelectedPageId returns the id of the page chrome-devtools-mcp marks `[selected]` in a
 * result's `## Pages` section (`<id>: <title> (<url>) [selected]`), or from
 * `structuredContent.pages`. chrome-devtools-mcp 1.8.0 routes page-scoped tools by this id.
 */
export function parseSelectedPageId({ result, text }) {
  const pages = result?.structuredContent?.pages;
  if (Array.isArray(pages)) {
    const selected = pages.find((p) => p && (p.selected === true || p.isSelected === true));
    if (selected && Number.isInteger(selected.id)) return selected.id;
  }
  const m = /^(\d+): .*\[selected\]/m.exec(text);
  return m ? Number(m[1]) : undefined;
}

/**
 * parseWebmcpToolList returns the page tools reported by `list_webmcp_tools` as
 * `[{name, description, inputSchema, annotations}]`. Prefers structuredContent when the
 * server sends it, else parses the text lines.
 */
export function parseWebmcpToolList({ result, text }) {
  const structured = result?.structuredContent?.webmcpTools;
  if (Array.isArray(structured)) {
    return structured.map((t) => ({ ...t, inputSchema: parseSchema(t.inputSchema) }));
  }
  const tools = [];
  const re = /^name="([A-Za-z0-9_.-]+)", description="((?:[^"\\]|\\.)*)", inputSchema=(.*?), annotations=(\{.*\})\s*$/gm;
  let m;
  while ((m = re.exec(text)) !== null) {
    tools.push({
      name: m[1],
      description: m[2],
      inputSchema: parseSchema(m[3]),
      annotations: parseSchema(m[4]),
    });
  }
  return tools;
}

function parseSchema(value) {
  if (typeof value !== 'string') return value;
  try {
    const parsed = JSON.parse(value);
    // Chrome 152 reports inputSchema as a JSON string; the MCP server may stringify it again.
    return typeof parsed === 'string' ? JSON.parse(parsed) : parsed;
  } catch {
    return value;
  }
}

/**
 * parseWebmcpExecution turns an `execute_webmcp_tool` result into
 * `{status, output, errorText}` where `output` is the page tool's return value as an
 * object when it was JSON (agents-board tools always return objects).
 */
export function parseWebmcpExecution({ text }) {
  // JSON.stringify drops undefined members, so a Canceled/Error envelope may carry `status`
  // alone; `status` at the top level is what identifies it.
  const envelope = extractJsonObjects(text).find((o) => o && typeof o === 'object' && 'status' in o);
  if (!envelope) {
    return { status: 'unparsed', output: undefined, errorText: 'no {status, output, errorText} object in: ' + text.slice(0, 300) };
  }
  let output = envelope.output;
  if (typeof output === 'string') {
    try {
      output = JSON.parse(output);
    } catch {
      // Chrome returns primitives as plain strings; leave as is.
    }
  }
  return { status: envelope.status, output, errorText: envelope.errorText };
}
