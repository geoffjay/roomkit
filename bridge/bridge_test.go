package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
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
