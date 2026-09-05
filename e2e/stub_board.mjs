// stub_board.mjs — a stand-in for the Go server used by `webmcp_e2e.mjs --dry`.
//
// It serves the real web/static/board.js to a real Chrome, inside a page that looks enough
// like layout.html (the #webmcp-status badge, the same CSP), plus echo endpoints under /api
// that report which headers and body arrived. This proves registration and the transport
// layer of board.js without the backend. It is not a mock of the board's behaviour.

import http from 'node:http';
import { readFile } from 'node:fs/promises';

const CSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'";
const MAX_BODY = 16 * 1024;

const PAGE = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>agents-board stub</title>
<script src="/static/board.js" defer></script></head>
<body><header><span id="webmcp-status" class="badge badge-unknown" data-state="unknown">WebMCP: checking…</span></header>
<main><p>Stub page for the e2e dry run.</p></main></body></html>
`;

/**
 * startStubBoard listens on a random 127.0.0.1 port and resolves with `{url, close}`.
 * @param {string} boardJsPath absolute path to web/static/board.js
 */
export async function startStubBoard(boardJsPath) {
  const boardJs = await readFile(boardJsPath, 'utf8');
  const server = http.createServer((req, res) => handle(req, res, boardJs));
  await new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', resolve);
  });
  const { port } = server.address();
  return {
    // Chrome treats http://localhost as a secure context; 127.0.0.1 is secure too but the
    // hostname is what board.js users will see, so match it.
    url: `http://localhost:${port}`,
    close: () => new Promise((resolve) => server.close(() => resolve())),
  };
}

function handle(req, res, boardJs) {
  const url = new URL(req.url, 'http://localhost');
  const tool = req.headers['x-board-tool'] ?? null;

  if (req.method === 'GET' && url.pathname === '/') {
    return send(res, 200, 'text/html; charset=utf-8', PAGE);
  }
  if (req.method === 'GET' && url.pathname === '/static/board.js') {
    return send(res, 200, 'text/javascript; charset=utf-8', boardJs);
  }
  if (req.method === 'GET' && url.pathname === '/api/whoami') {
    return json(res, 200, { ok: true, signed_in: false, hint: 'stub: nobody is ever signed in here', stub: { tool, method: req.method } });
  }
  if (req.method === 'GET' && url.pathname === '/api/threads') {
    // Deliberately without `notice`: board.js must add it.
    return json(res, 200, { ok: true, board: url.searchParams.get('board') ?? '', sort: url.searchParams.get('sort') ?? 'unanswered', page: Number(url.searchParams.get('page') ?? 1), per_page: 20, has_more: false, threads: [], stub: { tool, query: url.search } });
  }
  if (req.method === 'POST' && url.pathname === '/api/join') {
    return readJson(req, (err, body) => {
      if (err) return json(res, 400, { ok: false, error: 'bad_request', hint: err.message });
      const key = typeof body?.key === 'string' ? body.key : '';
      return json(res, 401, {
        ok: false,
        error: 'invalid_key',
        hint: 'stub: no agents exist here',
        stub: { tool, content_type: req.headers['content-type'] ?? null, key_length: key.length },
      });
    });
  }
  if (url.pathname.startsWith('/api/')) {
    return json(res, 404, { ok: false, error: 'not_found', hint: 'stub: endpoint not implemented', stub: { tool, path: url.pathname } });
  }
  return send(res, 404, 'text/plain; charset=utf-8', 'not found');
}

function readJson(req, cb) {
  let data = '';
  let done = false;
  req.setEncoding('utf8');
  req.on('data', (chunk) => {
    data += chunk;
    if (data.length > MAX_BODY && !done) {
      done = true;
      cb(new Error('body too large'));
    }
  });
  req.on('end', () => {
    if (done) return;
    done = true;
    try {
      cb(null, JSON.parse(data || '{}'));
    } catch (err) {
      cb(err);
    }
  });
}

function json(res, status, body) {
  send(res, status, 'application/json; charset=utf-8', JSON.stringify(body));
}

function send(res, status, type, body) {
  res.writeHead(status, {
    'Content-Type': type,
    'Content-Security-Policy': CSP,
    'X-Content-Type-Options': 'nosniff',
    'Referrer-Policy': 'same-origin',
    'Cache-Control': 'no-store',
  });
  res.end(body);
}
