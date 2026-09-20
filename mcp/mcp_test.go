package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

func roundTrip(t *testing.T, s *Server, requests ...string) []map[string]any {
	t.Helper()
	var out strings.Builder
	s.ServeStdio(strings.NewReader(strings.Join(requests, "\n")+"\n"), &out)
	replies := []map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("reply is not JSON: %q", line)
		}
		replies = append(replies, m)
	}
	return replies
}

// The lore spike found the copied server advertising the other app's
// name from its own binary. Identity is a parameter now.
func TestInitializeReportsConfiguredIdentity(t *testing.T) {
	s := NewServer("lore-bridge", "0.2.0")
	replies := roundTrip(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if len(replies) != 1 {
		t.Fatalf("got %d replies, want 1", len(replies))
	}
	info := replies[0]["result"].(map[string]any)["serverInfo"].(map[string]any)
	if info["name"] != "lore-bridge" || info["version"] != "0.2.0" {
		t.Errorf("serverInfo = %v, want lore-bridge/0.2.0", info)
	}
	if got := replies[0]["result"].(map[string]any)["protocolVersion"]; got != ProtocolVersion {
		t.Errorf("protocolVersion = %v, want %v", got, ProtocolVersion)
	}
}

func TestToolsListReturnsRegisteredTools(t *testing.T) {
	s := NewServer("x", "1")
	s.Tool("read_doc", "read one doc", Obj(map[string]any{"doc_id": "string"}), nil)
	s.Tool("post_comment", "comment", Obj(map[string]any{"body": "string"}), nil)

	replies := roundTrip(t, s, `{"jsonrpc":"2.0","id":7,"method":"tools/list","params":{}}`)
	tools := replies[0]["result"].(map[string]any)["tools"].([]any)
	names := []string{}
	for _, tl := range tools {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	if len(names) != 2 || names[0] != "post_comment" || names[1] != "read_doc" {
		t.Errorf("tools = %v, want sorted [post_comment read_doc]", names)
	}
}

func TestToolsCallDispatchesAndPassesArgs(t *testing.T) {
	s := NewServer("x", "1")
	var got map[string]any
	s.Tool("post_comment", "", Obj(nil), func(a map[string]any) (any, error) {
		got = a
		return map[string]any{"id": "c1"}, nil
	})
	replies := roundTrip(t, s,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"post_comment","arguments":{"body":"hi"}}}`)
	if got["body"] != "hi" {
		t.Errorf("handler args = %v, want body=hi", got)
	}
	text := replies[0]["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
	if !strings.Contains(text.(string), `"id":"c1"`) {
		t.Errorf("tool result = %v, want the handler payload", text)
	}
}

// A failing tool is a result with isError, not a JSON-RPC error: the
// agent must be able to read the rejection and try again.
func TestToolErrorIsAResultNotAProtocolError(t *testing.T) {
	s := NewServer("x", "1")
	s.Tool("propose_edit", "", Obj(nil), func(map[string]any) (any, error) {
		return nil, errString("severity required")
	})
	replies := roundTrip(t, s,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"propose_edit","arguments":{}}}`)
	res, ok := replies[0]["result"].(map[string]any)
	if !ok {
		t.Fatalf("tool failure produced %v, want a result", replies[0])
	}
	if res["isError"] != true {
		t.Errorf("isError = %v, want true", res["isError"])
	}
}

func TestUnknownToolIsAProtocolError(t *testing.T) {
	s := NewServer("x", "1")
	replies := roundTrip(t, s,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"nope","arguments":{}}}`)
	if _, ok := replies[0]["error"]; !ok {
		t.Errorf("unknown tool gave %v, want a JSON-RPC error", replies[0])
	}
}

func TestNotificationsGetNoReply(t *testing.T) {
	s := NewServer("x", "1")
	replies := roundTrip(t, s, `{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`)
	if len(replies) != 0 {
		t.Errorf("notification produced %v, want silence", replies)
	}
}

type errString string

func (e errString) Error() string { return string(e) }
