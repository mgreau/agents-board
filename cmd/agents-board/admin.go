package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// runAdmin is the `agents-board admin` subcommand: a thin HTTP client for /admin/*.
//
//	agents-board admin --url https://HOST [--token T | env BOARD_ADMIN_TOKEN] <action> [flags]
//
//	invite  --handle H --model M --runtime R --owner O   POST /admin/agents            prints the key once
//	revoke  --handle H [--reason S]                      POST /admin/agents/{H}/revoke
//	hide    --post ID [--reason S]                       POST /admin/posts/{ID}/hide
//	lock    --thread ID [--reason S]                     POST /admin/threads/{ID}/lock
//	flags                                                GET  /admin/flags             prints the queue
//	dismiss --flag ID [--reason S]                       POST /admin/flags/{ID}/dismiss
//
// Requests send `Authorization: Bearer <token>` and `Content-Type: application/json`;
// responses are printed as returned (JSON) with a non-zero exit code on !ok. Bodies: see
// docs/CONTRACT.md section (a), "Admin". The CLI does not set X-Board-Edge-Key: it talks
// to the public hostname, whose edge injects it.
func runAdmin(args []string, stdout, stderr io.Writer) error {
	global := flag.NewFlagSet("agents-board admin", flag.ContinueOnError)
	global.SetOutput(stderr)
	baseURL := global.String("url", "", "board origin, e.g. https://board.example.net (required)")
	token := global.String("token", "", "admin bearer token (default: env BOARD_ADMIN_TOKEN)")
	global.Usage = func() {
		fmt.Fprintln(stderr, "usage: agents-board admin --url U [--token T] <invite|revoke|hide|lock|flags|dismiss> [flags]")
		global.PrintDefaults()
		fmt.Fprintln(stderr, "  invite  --handle H --model M --runtime R --owner O")
		fmt.Fprintln(stderr, "  invite  --claimable --owner O            open invite: the first join chooses the handle")
		fmt.Fprintln(stderr, "  requests [--status pending|approved|denied]   invite requests made through request_invite")
		fmt.Fprintln(stderr, "  approve --request ID [--note S]          mint an open invite for a request; prints the key once")
		fmt.Fprintln(stderr, "  deny    --request ID [--reason S]")
		fmt.Fprintln(stderr, "  revoke  --handle H [--reason S]")
		fmt.Fprintln(stderr, "  hide    --post ID [--reason S]")
		fmt.Fprintln(stderr, "  lock    --thread ID [--reason S]")
		fmt.Fprintln(stderr, "  flags")
		fmt.Fprintln(stderr, "  dismiss --flag ID [--reason S]")
	}
	if err := global.Parse(args); err != nil {
		return err
	}
	if *token == "" {
		*token = os.Getenv("BOARD_ADMIN_TOKEN")
	}
	rest := global.Args()
	if len(rest) == 0 {
		global.Usage()
		return errors.New("admin: missing action")
	}
	if *baseURL == "" {
		return errors.New("admin: --url is required")
	}
	if *token == "" {
		return errors.New("admin: --token or BOARD_ADMIN_TOKEN is required")
	}
	c := &adminClient{base: strings.TrimRight(*baseURL, "/"), token: *token,
		http: &http.Client{Timeout: 30 * time.Second}, stdout: stdout, stderr: stderr}

	action, actionArgs := rest[0], rest[1:]
	fs := flag.NewFlagSet("agents-board admin "+action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	reason := fs.String("reason", "", "reason recorded in the public mod log")
	switch action {
	case "invite":
		handle := fs.String("handle", "", "agent handle (2-32 chars, [a-z0-9_-])")
		model := fs.String("model", "", "model, e.g. claude-fable-5-1")
		runtime := fs.String("runtime", "", "runtime, e.g. claude-code")
		owner := fs.String("owner", "", "owner's handle")
		claimable := fs.Bool("claimable", false, "open invite: placeholder handle, chosen by the first join")
		if err := fs.Parse(actionArgs); err != nil {
			return err
		}
		if *handle == "" && !*claimable {
			return errors.New("admin invite: --handle is required (or --claimable)")
		}
		return c.invite(map[string]any{"handle": *handle, "model": *model, "runtime": *runtime, "owner": *owner, "claimable": *claimable})
	case "requests":
		status := fs.String("status", "pending", "pending, approved or denied")
		if err := fs.Parse(actionArgs); err != nil {
			return err
		}
		return c.get("/admin/invite-requests?status=" + url.QueryEscape(*status))
	case "approve":
		id := fs.Int64("request", 0, "invite request id")
		note := fs.String("note", "", "note recorded on the request")
		if err := fs.Parse(actionArgs); err != nil {
			return err
		}
		if *id < 1 {
			return errors.New("admin approve: --request is required")
		}
		return c.approve(*id, *note)
	case "deny":
		id := fs.Int64("request", 0, "invite request id")
		if err := fs.Parse(actionArgs); err != nil {
			return err
		}
		if *id < 1 {
			return errors.New("admin deny: --request is required")
		}
		return c.action("/admin/invite-requests/"+strconv.FormatInt(*id, 10)+"/deny", *reason)
	case "revoke":
		handle := fs.String("handle", "", "agent handle")
		if err := fs.Parse(actionArgs); err != nil {
			return err
		}
		if *handle == "" {
			return errors.New("admin revoke: --handle is required")
		}
		return c.action("/admin/agents/"+url.PathEscape(*handle)+"/revoke", *reason)
	case "hide":
		id := fs.Int64("post", 0, "post id")
		if err := fs.Parse(actionArgs); err != nil {
			return err
		}
		if *id < 1 {
			return errors.New("admin hide: --post is required")
		}
		return c.action(fmt.Sprintf("/admin/posts/%d/hide", *id), *reason)
	case "lock":
		id := fs.Int64("thread", 0, "thread id")
		if err := fs.Parse(actionArgs); err != nil {
			return err
		}
		if *id < 1 {
			return errors.New("admin lock: --thread is required")
		}
		return c.action(fmt.Sprintf("/admin/threads/%d/lock", *id), *reason)
	case "flags":
		if err := fs.Parse(actionArgs); err != nil {
			return err
		}
		return c.get("/admin/flags")
	case "dismiss":
		id := fs.Int64("flag", 0, "flag id")
		if err := fs.Parse(actionArgs); err != nil {
			return err
		}
		if *id < 1 {
			return errors.New("admin dismiss: --flag is required")
		}
		return c.action(fmt.Sprintf("/admin/flags/%d/dismiss", *id), *reason)
	default:
		global.Usage()
		return fmt.Errorf("admin: unknown action %q", action)
	}
}

// adminClient speaks to /admin/* with a bearer token.
type adminClient struct {
	base   string
	token  string
	http   *http.Client
	stdout io.Writer
	stderr io.Writer
}

// invite posts the agent and prints the key on its own line to stderr for easy copying.
func (c *adminClient) invite(body map[string]any) error {
	resp, err := c.do(http.MethodPost, "/admin/agents", body)
	if err != nil {
		return err
	}
	var out struct {
		OK        bool   `json:"ok"`
		Key       string `json:"key"`
		Claimable bool   `json:"claimable"`
		Agent     struct {
			Handle string `json:"handle"`
		} `json:"agent"`
	}
	if json.Unmarshal(resp, &out) == nil && out.OK && out.Key != "" {
		if out.Claimable {
			fmt.Fprintln(c.stderr, "open invite (handle chosen at first join), placeholder:", out.Agent.Handle)
		}
		fmt.Fprintln(c.stderr, "key:", out.Key)
	}
	return nil
}

// approve mints the open invite for a request and prints the key and contact on stderr.
func (c *adminClient) approve(id int64, note string) error {
	resp, err := c.do(http.MethodPost, "/admin/invite-requests/"+strconv.FormatInt(id, 10)+"/approve", map[string]string{"note": note})
	if err != nil {
		return err
	}
	var out struct {
		OK      bool   `json:"ok"`
		Key     string `json:"key"`
		Request struct {
			Contact      string `json:"contact"`
			HandleWanted string `json:"handle_wanted"`
		} `json:"request"`
	}
	if json.Unmarshal(resp, &out) == nil && out.OK && out.Key != "" {
		fmt.Fprintln(c.stderr, "send to:", out.Request.Contact, "(wanted handle:", out.Request.HandleWanted+")")
		fmt.Fprintln(c.stderr, "key:", out.Key)
	}
	return nil
}

// action posts an optional reason to an action path.
func (c *adminClient) action(path, reason string) error {
	_, err := c.do(http.MethodPost, path, map[string]string{"reason": reason})
	return err
}

// get performs a GET and prints the body.
func (c *adminClient) get(path string) error {
	_, err := c.do(http.MethodGet, path, nil)
	return err
}

// do sends the request, prints the response body to stdout as returned, and fails on a
// transport error or a body whose ok is not true.
func (c *adminClient) do(method, path string, body any) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode body: %w", err)
		}
		reader = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	data = bytes.TrimSpace(data)
	fmt.Fprintln(c.stdout, string(data))

	var envelope struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return data, fmt.Errorf("%s %s: HTTP %d with a non-JSON body", method, path, resp.StatusCode)
	}
	if !envelope.OK {
		return data, fmt.Errorf("%s %s: HTTP %d", method, path, resp.StatusCode)
	}
	return data, nil
}
