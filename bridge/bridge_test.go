package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/geoffjay/roomkit/room"
	"time"
)

// The ocre spike verified that a stray API key silently overrides the
// owner's subscription auth. Stripping those variables is the fix, so
// it stays covered.
func TestSanitizeEnvStripsAuthOverrides(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"ANTHROPIC_API_KEY=sk-should-not-survive",
		"CLAUDE_CODE_USE_FOUNDRY=1",
		"ANTHROPIC_FOUNDRY_URL=https://gateway.internal",
		"HOME=/Users/geoff",
		"ANTHROPIC_MODEL=claude-sonnet",
	}
	got := SanitizeEnv(in)

	for _, banned := range []string{"ANTHROPIC_API_KEY=sk-should-not-survive", "CLAUDE_CODE_USE_FOUNDRY=1", "ANTHROPIC_FOUNDRY_URL=https://gateway.internal"} {
		if slices.Contains(got, banned) {
			t.Errorf("%q survived sanitization", banned)
		}
	}
	for _, kept := range []string{"PATH=/usr/bin", "HOME=/Users/geoff", "ANTHROPIC_MODEL=claude-sonnet"} {
		if !slices.Contains(got, kept) {
			t.Errorf("%q was stripped; only auth overrides should go", kept)
		}
	}
}

func TestDriveRejectsUnknownRuntime(t *testing.T) {
	if _, err := Drive(t.Context(), "not-a-runtime", Invocation{Prompt: "hi"}); err == nil {
		t.Error("unknown runtime accepted")
	}
}

// A non-2xx from the action boundary is an error the caller can see,
// carrying the server's reason: governance rejections must not look
// like success.
func TestActionSurfacesGovernanceRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Lore-Token") != "tok" {
			t.Errorf("token header missing: %v", r.Header)
		}
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "agent contributions require severity"})
	}))
	defer srv.Close()

	c := New(srv.URL, "space1", "tok", "X-Lore-Token")
	out, err := c.Action("/api/contributions", map[string]any{"body": "x"})
	if err == nil {
		t.Fatal("rejection reported as success")
	}
	if out["error"] != "agent contributions require severity" {
		t.Errorf("body = %v, want the server's reason", out)
	}
}

func TestActionSucceedsAndDecodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"id": "c1"})
	}))
	defer srv.Close()

	c := New(srv.URL, "space1", "tok", "")
	out, err := c.Action("/api/contributions", nil)
	if err != nil {
		t.Fatalf("Action: %v", err)
	}
	if out["id"] != "c1" {
		t.Errorf("out = %v, want id c1", out)
	}
}

func TestDefaultTokenHeader(t *testing.T) {
	if got := New("http://x", "s", "t", "").TokenHead; got != "X-Roomkit-Token" {
		t.Errorf("default token header = %q", got)
	}
}

// TestClaudeArgsGrantExactlyTheGrantedTools holds the invocation
// contract for the one runtime lore drives. A tool-using run must pin
// its model, load only the caller's MCP server, and allow only the
// named tools.
func TestClaudeArgsGrantExactlyTheGrantedTools(t *testing.T) {
	args := Runtimes["claude"].Args(Invocation{
		Prompt:     "do the thing",
		Model:      "claude-sonnet-4-6",
		MCPConfig:  "/tmp/lore-mcp.json",
		AllowTools: []string{"mcp__lore__read_doc", "mcp__lore__propose_edit"},
	})
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-p do the thing",
		"--model claude-sonnet-4-6",
		"--mcp-config /tmp/lore-mcp.json",
		"--strict-mcp-config",
		"--allowedTools mcp__lore__read_doc mcp__lore__propose_edit",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args missing %q\ngot: %s", want, joined)
		}
	}
}

// TestModelIsOnlyOmittedWhenUnset: an empty Model must not produce a
// bare --model flag, which would make the runtime fail rather than
// fall back.
func TestModelIsOnlyOmittedWhenUnset(t *testing.T) {
	for name := range Runtimes {
		args := Runtimes[name].Args(Invocation{Prompt: "p"})
		for _, a := range args {
			if a == "--model" || a == "-m" {
				t.Fatalf("%s emitted a model flag with no model: %v", name, args)
			}
		}
	}
}

// TestDriveRefusesToolsOnANonMCPRuntime is the anti-silent-degrade
// rule. Running without the granted tools would return a plausible
// answer that did none of the work.
func TestDriveRefusesToolsOnANonMCPRuntime(t *testing.T) {
	_, err := Drive(context.Background(), "gemini", Invocation{
		Prompt: "p", MCPConfig: "/tmp/cfg.json",
	})
	if err == nil {
		t.Fatal("granting tools to a runtime with no MCP support must be refused, not silently dropped")
	}
	if !strings.Contains(err.Error(), "MCP") {
		t.Fatalf("error should name the cause; got %v", err)
	}
	// The same runtime without tools is fine to attempt.
	if _, err := Drive(context.Background(), "gemini", Invocation{Prompt: "p"}); err != nil {
		if strings.Contains(err.Error(), "cannot be given an MCP server") {
			t.Fatalf("a tool-free invocation must not be refused: %v", err)
		}
	}
}

// TestListenSendsTheCredentialAsAHeader: a bridge authenticates with a
// durable token, and a token in a query string is written to every
// access log, proxy history and referrer between the bridge and the
// app. It must ride in the header instead.
func TestListenSendsTheCredentialAsAHeader(t *testing.T) {
	got := make(chan *http.Request, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Clone(context.Background())
		w.WriteHeader(http.StatusBadRequest) // refuse the upgrade; the request is the assertion
	}))
	defer srv.Close()

	c := New(srv.URL, "space-1", "tok-secret", "X-Lore-Token")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go c.Listen(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", 50*time.Millisecond, func(room.Event) {})

	select {
	case r := <-got:
		if h := r.Header.Get("X-Lore-Token"); h != "tok-secret" {
			t.Fatalf("X-Lore-Token header = %q, want the bridge token", h)
		}
		if strings.Contains(r.URL.RawQuery, "tok-secret") {
			t.Fatalf("the credential is in the query string: %q", r.URL.RawQuery)
		}
	case <-ctx.Done():
		t.Fatal("the bridge never dialled")
	}
}

// TestGetIsAuthenticatedAndReportsRefusals: a read carries the same
// credential as an action, and a refusal is an error rather than an
// empty success. Without both, an app that authenticates its reads
// sees the bridge as a stranger and the caller notices nothing.
func TestGetIsAuthenticatedAndReportsRefusals(t *testing.T) {
	var sawToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawToken = r.Header.Get("X-Lore-Token")
		if sawToken == "" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"sign in"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "s", "tok-secret", "X-Lore-Token")
	out, err := c.Get("/api/thing")
	if err != nil {
		t.Fatalf("an authenticated read failed: %v", err)
	}
	if sawToken != "tok-secret" {
		t.Fatalf("read sent token %q, want the bridge token", sawToken)
	}
	if out["ok"] != true {
		t.Fatalf("body = %v", out)
	}

	// An unauthenticated client must be told it was refused.
	anon := New(srv.URL, "s", "", "X-Lore-Token")
	body, err := anon.Get("/api/thing")
	if err == nil {
		t.Fatal("a 401 read returned success")
	}
	if body["error"] != "sign in" {
		t.Fatalf("the app's reason was dropped: %v", body)
	}
}
