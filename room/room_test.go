package room

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
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

// TestPresenceMatchesDelivery holds presence and Broadcast to the same
// rule. A bridge attaches scope-wide and therefore receives every
// room's events; reporting it absent from those rooms would make it a
// participant nobody can address.
func TestPresenceMatchesDelivery(t *testing.T) {
	h := NewHub()
	s := h.NewScope(RoleSuggester)
	reader, _ := s.Join("human", "Reader", "", RoleEditor)
	agent, _ := s.Join("agent", "bridge", "claude", RoleSuggester)

	var inRoom, scopeWide capture
	s.Attach(&inRoom, reader, "doc-a")
	s.Attach(&scopeWide, agent, "") // a bridge listens to the whole scope

	// Delivery: the scope-wide subscriber gets doc-a's event.
	s.Broadcast("doc-a", Event{Type: "thing.happened"})
	if got := len(scopeWide.types()); got != 1 {
		t.Fatalf("scope-wide subscriber received %d events for doc-a, want 1", got)
	}

	// Presence must agree with that.
	names := map[string]bool{}
	for _, a := range s.Presence("doc-a") {
		names[a.DisplayName] = true
	}
	if !names["bridge"] {
		t.Fatal("a scope-wide subscriber receives doc-a events but is absent from doc-a presence")
	}
	if !names["Reader"] {
		t.Fatal("the in-room client is missing from presence")
	}

	// A client in a different room is still excluded.
	var other capture
	third, _ := s.Join("human", "Elsewhere", "", RoleEditor)
	s.Attach(&other, third, "doc-b")
	for _, a := range s.Presence("doc-a") {
		if a.DisplayName == "Elsewhere" {
			t.Fatal("a client attached to doc-b must not appear in doc-a presence")
		}
	}
}

// TestShortScopeIDSurvivesAnAgentJoin is finding #15 from the second
// consumer. RestoreScope takes whatever id an app's store holds, and
// the agent id was cut out of it unguarded, so a scope restored under
// a short id crashed the server on the first agent join.
func TestShortScopeIDSurvivesAnAgentJoin(t *testing.T) {
	s := NewHub().RestoreScope("x", "tok-1", RoleSuggester)
	agent, tok := s.Join("agent", "claude", "claude", "")
	if tok != "tok-1" {
		t.Errorf("agent token = %q, want the restored one", tok)
	}
	if agent.ID != "agent-x" {
		t.Errorf("agent id = %q, want agent-x", agent.ID)
	}
}

// TestAgentTokenResolvesWithoutASocket is finding #10. A join is a
// connection event, but the agent token is configured into a bridge
// out of band and is valid before the bridge connects. Resolving it
// only after a socket exists refused the first action of a bridge
// that raced its own connection.
func TestAgentTokenResolvesWithoutASocket(t *testing.T) {
	s := NewHub().NewScope(RoleSuggester)

	a := s.ActorByToken(s.AgentToken())
	if a == nil {
		t.Fatal("the scope's agent token did not resolve before the agent connected")
	}
	if a.Kind != "agent" || a.Role != RoleSuggester {
		t.Fatalf("resolved actor = %+v, want an agent capped at suggester", a)
	}

	// The socket arrives later and names the actor the token minted,
	// rather than starting a second one.
	joined, tok := s.Join("agent", "claude", "claude", "")
	if tok != s.AgentToken() || joined.ID != a.ID {
		t.Fatalf("join minted a second agent: %+v after %+v", joined, a)
	}
	if joined.DisplayName != "claude" || joined.Runtime != "claude" {
		t.Errorf("join did not name the agent: %+v", joined)
	}
	if after := s.ActorByToken(s.AgentToken()); after == nil || after.DisplayName != "claude" {
		t.Errorf("token resolves to %+v, want the named agent", after)
	}
	if len(s.Members()) != 1 {
		t.Errorf("members = %d, want 1", len(s.Members()))
	}
}

// TestRefusalCarriesItsStatus is finding #7. Every consumer wrote the
// same error type to turn a refusal into a status, so the type lives
// here and Check hands it back intact.
func TestRefusalCarriesItsStatus(t *testing.T) {
	s := NewHub().NewScope("")
	s.Use(func(_ Actor, action string, _ map[string]any) error {
		if action == "post" {
			return Refusal{Code: 429, Reason: "max 5 per 10s"}
		}
		return errors.New("no opinion")
	})

	err := s.Check(Actor{}, "post", nil)
	var r Refusal
	if !errors.As(err, &r) {
		t.Fatalf("refusal did not survive Check: %v (%T)", err, err)
	}
	if r.Code != 429 || err.Error() != "max 5 per 10s" {
		t.Errorf("recovered refusal = %+v, message %q", r, err.Error())
	}
	if got := RefusalCode(err, 400); got != 429 {
		t.Errorf("RefusalCode = %d, want 429", got)
	}

	// A plain error still refuses, and says nothing about status.
	plain := s.Check(Actor{}, "other", nil)
	if plain == nil {
		t.Fatal("a plain error must still refuse the action")
	}
	if got := RefusalCode(plain, 400); got != 400 {
		t.Errorf("RefusalCode of a plain error = %d, want the caller's fallback", got)
	}
	if got := RefusalCode(nil, 400); got != 400 {
		t.Errorf("RefusalCode(nil) = %d, want the fallback", got)
	}
}

// served runs one scope's Serve on a test server and reports when the
// handler returns.
func served(t *testing.T, s *Scope) (url string, done <-chan struct{}) {
	t.Helper()
	ch := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		actor, _ := s.Join("human", "Reader", "", RoleReader)
		_ = s.Serve(w, req, actor, "doc-a", nil)
		close(ch)
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http"), ch
}

// TestServeKeepsASilentSocket is finding #11, the one both consumers
// saw in every run and read as noise. The read loop carried a
// one-minute deadline and nothing pinged, so a client with nothing to
// say was disconnected on a timer and reconnected, churning presence
// for every other client in the room.
//
// The keepalive is shortened here rather than waiting out a minute.
func TestServeKeepsASilentSocket(t *testing.T) {
	s := NewHub().NewScope("")
	s.SetKeepalive(20*time.Millisecond, 80*time.Millisecond)
	url, done := served(t, s)

	conn, _, err := websocket.Dial(t.Context(), url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	// The client reads, which is how it answers a ping, and says
	// nothing of its own for many ping intervals.
	got := make(chan string, 8)
	go func() {
		for {
			_, data, err := conn.Read(t.Context())
			if err != nil {
				return
			}
			var ev Event
			if json.Unmarshal(data, &ev) == nil {
				got <- ev.Type
			}
		}
	}()
	<-time.After(300 * time.Millisecond) // ~15 ping intervals, ~4 idle windows

	select {
	case <-done:
		t.Fatal("a silent client was dropped; it answered every ping")
	default:
	}

	s.Broadcast("doc-a", Event{Type: "thing.happened"})
	for {
		select {
		case typ := <-got:
			if typ == "thing.happened" {
				return
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the surviving socket received nothing")
		}
	}
}

// TestServeDropsAnUnansweredSocket is the other half of #11: the ping
// is what detects a peer that has stopped answering, so a connection
// nobody is reading is closed inside the idle window instead of being
// held until the next arbitrary deadline.
func TestServeDropsAnUnansweredSocket(t *testing.T) {
	s := NewHub().NewScope("")
	s.SetKeepalive(20*time.Millisecond, 80*time.Millisecond)
	url, done := served(t, s)

	// This client never reads, so it never pongs.
	conn, _, err := websocket.Dial(t.Context(), url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("an unanswered socket was never dropped: nothing pinged it")
	}
}
