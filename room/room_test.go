package room

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

type capture struct {
	mu     sync.Mutex
	events []Event
}

func (c *capture) Send(_ context.Context, data []byte) error {
	var ev Event
	if err := json.Unmarshal(data, &ev); err != nil {
		return err
	}
	c.mu.Lock()
	c.events = append(c.events, ev)
	c.mu.Unlock()
	return nil
}

func (c *capture) types() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.events))
	for i, e := range c.events {
		out[i] = e.Type + "@" + e.Resource
	}
	return out
}

// A room subscriber hears only its own room; a scope subscriber hears
// every room. The agent bridge depends on the second rule to follow a
// whole space over one socket.
func TestBroadcastAddressing(t *testing.T) {
	s := NewHub().NewScope("")
	docA, docB, scopeWide := &capture{}, &capture{}, &capture{}
	s.Attach(docA, Actor{ID: "a"}, "doc-a")
	s.Attach(docB, Actor{ID: "b"}, "doc-b")
	s.Attach(scopeWide, Actor{ID: "bridge"}, "")

	s.Broadcast("doc-a", Event{Type: "x"})

	if got := docA.types(); len(got) != 1 || got[0] != "x@doc-a" {
		t.Errorf("room subscriber for doc-a got %v, want [x@doc-a]", got)
	}
	if got := docB.types(); len(got) != 0 {
		t.Errorf("room subscriber for doc-b got %v, want none", got)
	}
	if got := scopeWide.types(); len(got) != 1 || got[0] != "x@doc-a" {
		t.Errorf("scope subscriber got %v, want [x@doc-a]", got)
	}
}

// BroadcastScope reaches every client whatever room it is in.
func TestBroadcastScopeReachesAllRooms(t *testing.T) {
	s := NewHub().NewScope("")
	docA, docB := &capture{}, &capture{}
	s.Attach(docA, Actor{ID: "a"}, "doc-a")
	s.Attach(docB, Actor{ID: "b"}, "doc-b")

	s.BroadcastScope(Event{Type: "space.closed"})

	if len(docA.types()) != 1 || len(docB.types()) != 1 {
		t.Errorf("scope broadcast reached %v and %v, want both", docA.types(), docB.types())
	}
}

func TestDetachStopsDelivery(t *testing.T) {
	s := NewHub().NewScope("")
	c := &capture{}
	cl := s.Attach(c, Actor{ID: "a"}, "doc")
	s.Detach(cl)
	s.Broadcast("doc", Event{Type: "x"})
	if got := c.types(); len(got) != 0 {
		t.Errorf("detached client received %v", got)
	}
}

// An agent never exceeds the scope's ceiling, and the ceiling defaults
// to suggester so agent writes stay proposals.
func TestAgentCappedAtScopeCeiling(t *testing.T) {
	s := NewHub().NewScope("") // default cap
	agent, _ := s.Join("agent", "claude", "claude", "")
	if agent.Role != RoleSuggester {
		t.Errorf("agent role = %q, want %q", agent.Role, RoleSuggester)
	}
	if AtLeast(agent.Role, RoleEditor) {
		t.Error("suggester must not satisfy the editor requirement")
	}

	strict := NewHub().NewScope(RoleCommenter)
	quiet, _ := strict.Join("agent", "claude", "claude", "")
	if quiet.Role != RoleCommenter {
		t.Errorf("agent role = %q, want %q under a commenter ceiling", quiet.Role, RoleCommenter)
	}
}

// Two tabs of one person are one actor, so presence does not double.
func TestHumanJoinIsIdempotent(t *testing.T) {
	s := NewHub().NewScope("")
	a1, t1 := s.Join("human", "Geoff", "", "")
	a2, t2 := s.Join("human", "Geoff", "", "")
	if a1.ID != a2.ID || t1 != t2 {
		t.Errorf("second join minted a new actor: %s/%s vs %s/%s", a1.ID, t1, a2.ID, t2)
	}
	if len(s.Members()) != 1 {
		t.Errorf("members = %d, want 1", len(s.Members()))
	}
}

func TestActorByTokenIsolatesCallers(t *testing.T) {
	s := NewHub().NewScope("")
	_, tok := s.Join("human", "Geoff", "", "")
	if got := s.ActorByToken(tok); got == nil || got.DisplayName != "Geoff" {
		t.Fatalf("token did not resolve to its actor: %+v", got)
	}
	if got := s.ActorByToken("not-a-token"); got != nil {
		t.Errorf("unknown token resolved to %+v, want nil", got)
	}
	if got := s.ActorByToken(""); got != nil {
		t.Errorf("empty token resolved to %+v, want nil", got)
	}
}

// Policies run at the action boundary and the first rejection wins.
func TestCheckRunsPoliciesInOrder(t *testing.T) {
	s := NewHub().NewScope("")
	s.Use(func(a Actor, action string, p map[string]any) error {
		if action == "post" && p["severity"] == "" {
			return errors.New("severity required")
		}
		return nil
	})
	s.Use(func(Actor, string, map[string]any) error { return errors.New("second policy") })

	if err := s.Check(Actor{}, "post", map[string]any{"severity": ""}); err == nil || err.Error() != "severity required" {
		t.Errorf("first rejection = %v, want severity required", err)
	}
	if err := s.Check(Actor{}, "other", nil); err == nil || err.Error() != "second policy" {
		t.Errorf("later policy = %v, want second policy", err)
	}
}

func TestRateBucketRefusesBurstThenRecovers(t *testing.T) {
	s := NewHub().NewScope("")
	for i := range 3 {
		if !s.RateAllow("agent", 3, 50*time.Millisecond) {
			t.Fatalf("call %d refused inside the budget", i+1)
		}
	}
	if s.RateAllow("agent", 3, 50*time.Millisecond) {
		t.Error("fourth call allowed; the bucket did not refuse")
	}
	if !s.RateAllow("other-agent", 3, 50*time.Millisecond) {
		t.Error("bucket is not per actor")
	}
	time.Sleep(60 * time.Millisecond)
	if !s.RateAllow("agent", 3, 50*time.Millisecond) {
		t.Error("bucket did not recover after the window")
	}
}

func TestPresenceIsPerRoom(t *testing.T) {
	s := NewHub().NewScope("")
	s.Attach(&capture{}, Actor{ID: "a"}, "doc-a")
	s.Attach(&capture{}, Actor{ID: "b"}, "doc-b")
	if got := len(s.Presence("doc-a")); got != 1 {
		t.Errorf("presence(doc-a) = %d, want 1", got)
	}
	if got := len(s.Presence("")); got != 2 {
		t.Errorf("scope presence = %d, want 2", got)
	}
}

// TestRestoreScopeKeepsIdentity defends the durable half of a scope. A
// persistent app restarts, and the share links it handed out plus the
// agent token it configured into a bridge must still work. Minting
// fresh ones would silently unpair every bridge.
func TestRestoreScopeKeepsIdentity(t *testing.T) {
	h := NewHub()
	orig := h.NewScope(RoleSuggester)
	id, tok := orig.ID(), orig.AgentToken()

	// The process dies; a new hub comes up from the app's store.
	h2 := NewHub()
	got := h2.RestoreScope(id, tok, RoleSuggester)
	if got.ID() != id {
		t.Fatalf("restored id = %q, want %q", got.ID(), id)
	}
	if got.AgentToken() != tok {
		t.Fatalf("restored agent token = %q, want %q", got.AgentToken(), tok)
	}
	if h2.Scope(id) != got {
		t.Fatal("a restored scope must be reachable by id from the hub")
	}
	// Restoring twice is the same scope, not a second one that would
	// split the live clients of one room across two objects.
	if again := h2.RestoreScope(id, tok, RoleSuggester); again != got {
		t.Fatal("RestoreScope must be idempotent for one id")
	}
	// Live state is per-connection and is not restored.
	if len(got.Members()) != 0 {
		t.Fatalf("restored scope carried %d members, want 0", len(got.Members()))
	}
}
