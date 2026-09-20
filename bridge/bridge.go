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

// Get reads a JSON resource from the app.
func (c *Client) Get(path string) (map[string]any, error) {
	resp, err := c.HTTP.Get(c.BaseURL + path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%s: %s", path, strings.TrimSpace(string(raw)))
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
	conn, err := mcp.Dial(ctx, wsURL)
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	for {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		_, data, err := conn.Read(rctx)
		cancel()
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

// Runtime describes how to invoke an agent runtime headlessly.
type Runtime struct {
	Name string
	Args func(prompt string) []string
}

// Runtimes are the runtimes verified against this bridge shape.
var Runtimes = map[string]Runtime{
	"claude":   {Name: "claude", Args: func(p string) []string { return []string{"-p", p} }},
	"codex":    {Name: "codex", Args: func(p string) []string { return []string{"exec", p} }},
	"opencode": {Name: "opencode", Args: func(p string) []string { return []string{"run", p} }},
	"gemini":   {Name: "gemini", Args: func(p string) []string { return []string{"-p", p} }},
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

// Drive invokes the owner's runtime headlessly with the prompt and
// returns its output. The runtime runs on the owner's machine with the
// owner's credentials; roomkit never sees a key.
func Drive(ctx context.Context, runtime, prompt string) (string, error) {
	rt, ok := Runtimes[runtime]
	if !ok {
		return "", fmt.Errorf("unknown runtime %q", runtime)
	}
	cmd := exec.CommandContext(ctx, rt.Name, rt.Args(prompt)...)
	cmd.Env = SanitizeEnv(os.Environ())
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s: %w: %s", runtime, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}
