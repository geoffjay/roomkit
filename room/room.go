// Package room is the domain-free session layer of roomkit.
//
// It never learns what the participants are discussing. A scope owns
// identity, permission, policy, and rate state; rooms beneath a scope
// are addressed by an opaque resource string.
//
//	ocre: scope = review,  resource = ""          (one room per review)
//	lore: scope = space,   resource = concept id  (one room per document)
//
// # Delivery contract
//
// Subscribers choose their address when they attach:
//
//   - A client attached to resource R receives events broadcast to R.
//   - A client attached to the empty resource is a scope subscriber and
//     receives every event in the scope. That is how an agent bridge
//     follows a whole space over one socket.
//   - BroadcastScope reaches every client regardless of address.
//
// This contract is load-bearing for bridges, so it is stated here and
// covered by tests rather than left implicit in a conditional.
package room

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// ---------- identity and permission ----------

// Actor is a participant in a scope. Agents carry the runtime that
// backs them and the owner who connected the bridge.
type Actor struct {
	Kind        string `json:"kind"` // human | agent
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Runtime     string `json:"runtime,omitempty"`
	OwnerID     string `json:"owner_id,omitempty"`
	Role        string `json:"role"`
}

// Roles, lowest to highest. Apps may use a subset.
const (
	RoleReader    = "reader"
	RoleCommenter = "commenter"
	RoleSuggester = "suggester"
	RoleEditor    = "editor"
	RoleOwner     = "owner"
)

var rank = map[string]int{
	RoleReader: 0, RoleCommenter: 1, RoleSuggester: 2, RoleEditor: 3, RoleOwner: 4,
}

// AtLeast reports whether role meets need.
func AtLeast(role, need string) bool { return rank[role] >= rank[need] }

// Min returns the lower of two roles. Use it to cap an agent at its
// owner's permission.
func Min(a, b string) string {
	if rank[a] <= rank[b] {
		return a
	}
	return b
}

// ---------- events ----------

// Event is the wire envelope. Payload is app-defined and opaque here.
type Event struct {
	Type     string `json:"type"`
	Scope    string `json:"scope"`
	Resource string `json:"resource,omitempty"`
	Actor    *Actor `json:"actor,omitempty"`
	Payload  any    `json:"payload,omitempty"`
	TS       string `json:"ts"`
}

// ---------- governance mechanism ----------

// Policy is an app-registered check at the action boundary. roomkit
// runs policies; the app names the actions and writes the reasons. No
// governance vocabulary ("verdict", "suggestion") appears here.
type Policy func(actor Actor, action string, payload map[string]any) error

// ---------- transport ----------

// Sink receives serialized events. A websocket connection satisfies it
// through the adapter returned by Serve; tests supply their own.
type Sink interface {
	Send(ctx context.Context, data []byte) error
}

// Client is one attached subscriber.
type Client struct {
	sink     Sink
	actor    Actor
	resource string
}

// Actor returns the client's actor.
func (c *Client) Actor() Actor { return c.actor }

// Resource returns the address the client attached to. The empty
// string means the client is a scope subscriber.
func (c *Client) Resource() string { return c.resource }

// ---------- scopes ----------

// Scope owns membership, policy, rate state, and the clients attached
// to any room beneath it.
type Scope struct {
	mu         sync.Mutex
	id         string
	agentToken string
	agentCap   string

	members  map[string]*Actor // token -> actor
	clients  map[*Client]bool
	rates    map[string][]time.Time
	policies []Policy
}

// Hub holds every scope.
type Hub struct {
	mu     sync.Mutex
	scopes map[string]*Scope
}

// NewHub creates an empty hub.
func NewHub() *Hub { return &Hub{scopes: map[string]*Scope{}} }

// NewID mints a short random identifier.
func NewID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Now is the shared timestamp format.
func Now() string { return time.Now().UTC().Format(time.RFC3339) }

// NewScope creates a scope with a fresh agent token. agentCap is the
// ceiling for agents in this scope; pass "" for RoleSuggester.
func (h *Hub) NewScope(agentCap string) *Scope {
	if agentCap == "" {
		agentCap = RoleSuggester
	}
	s := &Scope{
		id:         NewID()[:10],
		agentToken: NewID(),
		agentCap:   agentCap,
		members:    map[string]*Actor{},
		clients:    map[*Client]bool{},
		rates:      map[string][]time.Time{},
	}
	h.mu.Lock()
	h.scopes[s.id] = s
	h.mu.Unlock()
	return s
}

// Scope returns a scope by id, or nil.
func (h *Hub) Scope(id string) *Scope {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.scopes[id]
}

// ID returns the scope identifier.
func (s *Scope) ID() string { return s.id }

// AgentToken returns the token a bridge presents to join as an agent.
func (s *Scope) AgentToken() string { return s.agentToken }

// Use registers a policy at the action boundary.
func (s *Scope) Use(p Policy) {
	s.mu.Lock()
	s.policies = append(s.policies, p)
	s.mu.Unlock()
}

// Check runs every registered policy in order and returns the first
// rejection. Apps call it inside the action handler, server-side, so a
// bridge cannot bypass it.
func (s *Scope) Check(actor Actor, action string, payload map[string]any) error {
	s.mu.Lock()
	ps := append([]Policy(nil), s.policies...)
	s.mu.Unlock()
	for _, p := range ps {
		if err := p(actor, action, payload); err != nil {
			return err
		}
	}
	return nil
}

// Join registers an actor and returns it with its token. Human joins
// are idempotent by display name, so one person in two tabs is one
// actor. Agents share the scope's agent token and are capped at the
// scope's agent ceiling.
func (s *Scope) Join(kind, name, runtime, role string) (Actor, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if kind == "agent" {
		if a, ok := s.members[s.agentToken]; ok {
			return *a, s.agentToken
		}
		a := &Actor{
			Kind: "agent", ID: "agent-" + s.id[:4], DisplayName: name,
			Runtime: runtime, OwnerID: "owner",
			Role: Min(RoleOwner, s.agentCap),
		}
		s.members[s.agentToken] = a
		return *a, s.agentToken
	}
	for tok, a := range s.members {
		if a.Kind == "human" && a.DisplayName == name {
			return *a, tok
		}
	}
	if role == "" {
		role = RoleEditor
	}
	tok := NewID()
	a := &Actor{Kind: "human", ID: "h-" + tok[:6], DisplayName: name, Role: role}
	s.members[tok] = a
	return *a, tok
}

// ActorByToken resolves the acting participant from its token.
func (s *Scope) ActorByToken(tok string) *Actor {
	if tok == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.members[tok]; ok {
		cp := *a
		return &cp
	}
	return nil
}

// Members lists the distinct actors of the scope.
func (s *Scope) Members() []Actor {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	out := []Actor{}
	for _, a := range s.members {
		if !seen[a.ID] {
			seen[a.ID] = true
			out = append(out, *a)
		}
	}
	return out
}

// Presence lists actors attached to one room. The empty resource lists
// every actor attached anywhere in the scope.
func (s *Scope) Presence(resource string) []Actor {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	out := []Actor{}
	for c := range s.clients {
		if resource != "" && c.resource != resource {
			continue
		}
		if !seen[c.actor.ID] {
			seen[c.actor.ID] = true
			out = append(out, c.actor)
		}
	}
	return out
}

// Attach registers a sink as a client of (scope, resource). Pass the
// empty resource to subscribe to the whole scope.
func (s *Scope) Attach(sink Sink, actor Actor, resource string) *Client {
	c := &Client{sink: sink, actor: actor, resource: resource}
	s.mu.Lock()
	s.clients[c] = true
	s.mu.Unlock()
	return c
}

// Detach removes a client.
func (s *Scope) Detach(c *Client) {
	s.mu.Lock()
	delete(s.clients, c)
	s.mu.Unlock()
}

// Broadcast delivers an event to one room and to every scope
// subscriber. See the package delivery contract.
func (s *Scope) Broadcast(resource string, ev Event) {
	s.deliver(ev, func(c *Client) bool {
		return c.resource == "" || c.resource == resource
	}, resource)
}

// BroadcastScope delivers an event to every client in the scope,
// whatever room they are attached to.
func (s *Scope) BroadcastScope(ev Event) {
	s.deliver(ev, func(*Client) bool { return true }, "")
}

func (s *Scope) deliver(ev Event, match func(*Client) bool, resource string) {
	ev.Scope = s.id
	if ev.Resource == "" {
		ev.Resource = resource
	}
	if ev.TS == "" {
		ev.TS = Now()
	}
	data, _ := json.Marshal(ev)
	s.mu.Lock()
	targets := make([]*Client, 0, len(s.clients))
	for c := range s.clients {
		if match(c) {
			targets = append(targets, c)
		}
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, c := range targets {
		wg.Add(1)
		go func(c *Client) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			c.sink.Send(ctx, data)
		}(c)
	}
	wg.Wait()
}

// RateAllow is the per-actor token bucket. It reports whether one more
// action fits inside the window.
func (s *Scope) RateAllow(actorID string, max int, window time.Duration) bool {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	times := s.rates[actorID]
	kept := times[:0]
	for _, t := range times {
		if now.Sub(t) < window {
			kept = append(kept, t)
		}
	}
	if len(kept) >= max {
		s.rates[actorID] = kept
		return false
	}
	s.rates[actorID] = append(kept, now)
	return true
}

// ---------- websocket adapter ----------

type wsSink struct{ conn *websocket.Conn }

func (w wsSink) Send(ctx context.Context, data []byte) error {
	return w.conn.Write(ctx, websocket.MessageText, data)
}

// Serve accepts a WebSocket, attaches it to (scope, resource), sends
// the app's initial state, announces presence, and blocks until the
// socket drops.
func (s *Scope) Serve(w http.ResponseWriter, req *http.Request, actor Actor, resource string, initial any) error {
	conn, err := websocket.Accept(w, req, nil)
	if err != nil {
		return err
	}
	c := s.Attach(wsSink{conn}, actor, resource)
	if initial != nil {
		data, _ := json.Marshal(initial)
		ctx, cancel := context.WithTimeout(req.Context(), 5*time.Second)
		conn.Write(ctx, websocket.MessageText, data)
		cancel()
	}
	s.Broadcast(resource, Event{Type: "presence.joined", Actor: &actor})
	defer func() {
		s.Detach(c)
		s.Broadcast(resource, Event{Type: "presence.left", Actor: &actor})
		conn.CloseNow()
	}()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		_, _, err := conn.Read(ctx)
		cancel()
		if err != nil {
			return nil
		}
	}
}
