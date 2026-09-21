// Package bridge is the desktop agent-bridge core: one outbound
// WebSocket to a scope, a reconnect loop, the action client that calls
// the app's REST boundary, and headless runtime driving.
//
// The bridge never sees the domain. It carries bytes between a room and
// the owner's runtime, which keeps the no-hosting and no-token-billing
// constraints true by construction: model inference happens inside the
// owner's runtime, on the owner's credentials.
package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/geoffjay/roomkit/mcp"
	"github.com/geoffjay/roomkit/room"
)

// Client calls an app's action boundary with the bridge's token.
type Client struct {
	BaseURL   string
	Scope     string
	Token     string
	TokenHead string // header name; defaults to X-Roomkit-Token
	HTTP      *http.Client
}

// New creates a client with sane defaults.
func New(baseURL, scope, token, tokenHeader string) *Client {
	if tokenHeader == "" {
		tokenHeader = "X-Roomkit-Token"
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"), Scope: scope, Token: token,
		TokenHead: tokenHeader, HTTP: &http.Client{Timeout: 120 * time.Second},
	}
}

// Action posts to the app's action API. Governance runs server-side at
// this boundary, so a rejection arrives here as a non-2xx status.
func (c *Client) Action(path string, body any) (map[string]any, error) {
	buf, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, c.BaseURL+path, strings.NewReader(string(buf)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(c.TokenHead, c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	json.Unmarshal(raw, &out)
	if resp.StatusCode >= 300 {
		return out, fmt.Errorf("action %s rejected (%d): %s", path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return out, nil
}

// Get reads a JSON resource from the app, carrying the bridge's
// credential and reporting a refusal as an error.
//
// It previously sent no token and ignored the status code, so a read
// was anonymous and a 401 arrived looking like an empty success. An
// app that authenticates its reads would have seen the bridge as a
// stranger, and the caller would have seen nothing wrong.
func (c *Client) Get(path string) (map[string]any, error) {
	req, err := http.NewRequest(http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set(c.TokenHead, c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%s: %s", path, strings.TrimSpace(string(raw)))
	}
	if resp.StatusCode >= 300 {
		return out, fmt.Errorf("read %s rejected (%d): %s", path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return out, nil
}

// Listen keeps one outbound socket to wsURL and calls handle for every
// event. It reconnects with a fixed backoff until ctx ends. Subscribe
// to the empty resource to follow a whole scope over one socket.
func (c *Client) Listen(ctx context.Context, wsURL string, backoff time.Duration, handle func(room.Event)) error {
	if backoff <= 0 {
		backoff = 2 * time.Second
	}
	for {
		if err := c.listenOnce(ctx, wsURL, handle); err != nil && ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "roomkit: socket dropped: %v; reconnecting in %s\n", err, backoff)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
}

func (c *Client) listenOnce(ctx context.Context, wsURL string, handle func(room.Event)) error {
	// The credential travels as a header. A token in a query string is
	// a token in every access log between here and the app.
	h := http.Header{}
	if c.Token != "" {
		h.Set(c.TokenHead, c.Token)
	}
	conn, err := mcp.DialHeader(ctx, wsURL, h)
	if err != nil {
		return err
	}
	defer conn.CloseNow()

	// A bridge that hears nothing is not a bridge that has gone away.
	// The read carried an arbitrary deadline, which dropped a healthy
	// socket on a timer; liveness is the ping's job, and the ping also
	// keeps every NAT between here and the app from forgetting the
	// connection.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		room.Keepalive(ctx, conn, room.DefaultPing, room.DefaultIdle)
		cancel()
	}()

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		var ev room.Event
		if json.Unmarshal(data, &ev) != nil {
			continue
		}
		handle(ev)
	}
}

// ---------- runtime driving ----------

// Invocation is one headless run of an owner's runtime.
type Invocation struct {
	// Prompt is the instruction the runtime receives.
	Prompt string
	// Model pins the model for this run. An empty Model leaves the
	// choice to the runtime's own default, which is a silent
	// dependency on whatever that default becomes — a deprecated
	// model can be substituted underneath a working bridge without
	// any signal. Callers that care should set it.
	Model string
	// MCPConfig is a path to a JSON MCP server configuration. When it
	// is set the runtime is also told to ignore every other MCP
	// source, so a run carries exactly the tools the caller granted
	// and never inherits the owner's unrelated servers.
	MCPConfig string
	// AllowTools is the exact set of tool names the run may call.
	AllowTools []string
	// Timeout bounds the run. Zero means DefaultTimeout: a runtime
	// that hangs would otherwise pin the goroutine that drove it for
	// as long as the process lives, and the caller that remembers to
	// set its own deadline is the one that did not need the default.
	Timeout time.Duration
}

// Runtime describes how to invoke an agent runtime headlessly.
type Runtime struct {
	Name string
	Args func(Invocation) []string
	// MCP reports whether this runtime accepts an MCP server on the
	// command line. Driving a tool-using invocation through a runtime
	// that cannot is an error, not a silent run without tools.
	MCP bool
}

// Runtimes are the runtimes verified against this bridge shape.
var Runtimes = map[string]Runtime{
	"claude": {Name: "claude", MCP: true, Args: func(in Invocation) []string {
		a := []string{"-p", in.Prompt}
		if in.Model != "" {
			a = append(a, "--model", in.Model)
		}
		if in.MCPConfig != "" {
			a = append(a, "--mcp-config", in.MCPConfig, "--strict-mcp-config", "--permission-mode", "dontAsk")
		}
		if len(in.AllowTools) > 0 {
			a = append(a, append([]string{"--allowedTools"}, in.AllowTools...)...)
		}
		return a
	}},
	"codex": {Name: "codex", Args: func(in Invocation) []string {
		a := []string{"exec"}
		if in.Model != "" {
			a = append(a, "--model", in.Model)
		}
		return append(a, in.Prompt)
	}},
	"opencode": {Name: "opencode", Args: func(in Invocation) []string {
		a := []string{"run"}
		if in.Model != "" {
			a = append(a, "--model", in.Model)
		}
		return append(a, in.Prompt)
	}},
	"gemini": {Name: "gemini", Args: func(in Invocation) []string {
		a := []string{"-p", in.Prompt}
		if in.Model != "" {
			a = append(a, "-m", in.Model)
		}
		return a
	}},
}

// overrideVars are environment variables that silently replace the
// owner's own authentication with API-key billing on another account.
// The ocre spike verified that a stray one breaks subscription auth, so
// the bridge strips them before invoking a runtime.
func overrideVar(key string) bool {
	switch {
	case key == "ANTHROPIC_API_KEY", key == "CLAUDE_CODE_USE_FOUNDRY":
		return true
	case strings.HasPrefix(key, "ANTHROPIC_FOUNDRY_"):
		return true
	}
	return false
}

// SanitizeEnv returns env without the auth-override variables. The
// owner's configured credentials must win.
func SanitizeEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if overrideVar(strings.SplitN(kv, "=", 2)[0]) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// DefaultTimeout bounds a run that set no Timeout of its own.
const DefaultTimeout = 120 * time.Second

// Drive invokes the owner's runtime headlessly and returns what it
// wrote to stdout. The runtime runs on the owner's machine with the
// owner's credentials; roomkit never sees a key.
//
// stderr is not part of the answer. A runtime writes progress lines,
// deprecation notices and warnings there, and a caller that parses
// the output would parse those too. It arrives in the error instead,
// where it is diagnosis rather than content.
//
// An invocation that grants tools through a runtime with no MCP
// support is refused. Running it anyway would produce a plausible
// answer with none of the tool calls the caller asked for, which is
// the worst of both outcomes: it looks like it worked.
func Drive(ctx context.Context, runtime string, in Invocation) (string, error) {
	rt, ok := Runtimes[runtime]
	if !ok {
		return "", fmt.Errorf("unknown runtime %q", runtime)
	}
	if in.MCPConfig != "" && !rt.MCP {
		return "", fmt.Errorf("runtime %q cannot be given an MCP server on the command line", runtime)
	}
	timeout := in.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var stdout, stderr strings.Builder
	cmd := exec.CommandContext(ctx, rt.Name, rt.Args(in)...)
	cmd.Env = SanitizeEnv(os.Environ())
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return stdout.String(), fmt.Errorf("%s: gave up after %s: %s", runtime, timeout, strings.TrimSpace(stderr.String()))
	}
	if err != nil {
		return stdout.String(), fmt.Errorf("%s: %w: %s", runtime, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
