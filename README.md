# roomkit

The shared substrate behind [ocre](https://github.com/geoffjay/ocre)
(code review) and lore (knowledge building): the domain-free half of a
platform where humans and their own AI agents work together on one
live surface.

roomkit carries bytes between a room and the owner's runtime. It never
learns what the participants are discussing.

## The boundary

One question decides what belongs here: **would this code change if the
artifact being discussed were knowledge instead of code?** If no, it
belongs in roomkit. If yes, it belongs in the app.

| Package | Answers | 
|---|---|
| `room` | Who is here, what may they do, and who hears an event? |
| `anchor` | Where was this said, and where is that now? |
| `mcp` | How does a runtime discover and call tools? |
| `bridge` | How does a desktop process join a room and drive a runtime? |

Staying out, by rule: comment and contribution types, verdicts,
suggestions, diffs, document formats, attribution and trust ledgers.
roomkit has no governance vocabulary — only the mechanism that runs an
app's policies.

## Room addressing

A scope owns identity, permission, policy, and rate state. Rooms
beneath it are addressed by an opaque resource string.

```
ocre: scope = review,  resource = ""          one room per review
lore: scope = space,   resource = concept id  one room per document
```

Delivery is explicit and tested:

- A client attached to resource `R` receives events broadcast to `R`.
- A client attached to the empty resource is a **scope subscriber** and
  receives every event in the scope. An agent bridge follows a whole
  space over one socket that way.
- `BroadcastScope` reaches every client regardless of address.

## Governance: mechanism, not vocabulary

```go
scope.Use(func(a room.Actor, action string, p map[string]any) error {
    if a.Kind == "agent" && action == "contribution.create" && p["severity"] == "" {
        return errors.New("agent findings require severity")
    }
    return nil
})
```

The app names the actions and writes the reasons. roomkit runs the
policies, keeps per-actor rate buckets, and caps an agent at its
owner's permission (`room.Min`). Enforcement happens server-side at the
action boundary, so a bridge cannot bypass it.

A refusal may carry the status the app's transport should report:
`return room.Refusal{Code: 429, Reason: "..."}`, recovered with
`errors.As` or `room.RefusalCode(err, fallback)`. A plain error still
refuses and means "no opinion on the status".

## Keepalive

A served socket is pinged every `room.DefaultPing` and dropped once a
ping goes unanswered for `room.DefaultIdle`. A client with nothing to
say stays attached; a client that stopped answering does not.
`Scope.SetKeepalive` overrides both. An app that accepts the socket
itself calls `Scope.ReadLoop` instead of writing the loop again.

## Anchors

An anchor records where something sits and which revision it was
recorded against. The lifecycle is shared; the addressing scheme is
not, so apps supply a `Resolver`.

`Remap(resolver, a, headRevision, base, head)` never mutates an
anchor: a moved anchor comes back as a new value, and the original
record of where something was said survives. A `Resolver` receives
both contents — `base` at `a.BaseRevision` and `head` at
`headRevision` — so `anchor.LineRange`, the generic half of the
renumbering problem any document edit causes, composes with it
without the resolver holding a revision store of its own.

## Bridge

One outbound WebSocket, a reconnect loop, an action client, and
headless runtime driving for `claude`, `codex`, `opencode`, and
`gemini`. `Drive` returns the runtime's stdout and puts stderr in the
error, so a caller that parses the answer never parses a warning. An
`Invocation` is bounded by `Timeout`, or by `bridge.DefaultTimeout`
when it sets none. `SanitizeEnv` strips the variables that would
silently replace the owner's own authentication with API-key billing
on another account — a trap verified in practice, so it is covered by
a test.

## Status

Extracted on 2026-09-20 from the two spikes that proved the shape,
under the rule "extract on the second running consumer". The API is
young and will move with its two consumers.

v0.3.0 is the first move. The second consumer migrated onto v0.2.3 and
filed what running on it taught: the keepalive, the composed resolver,
the typed refusal, the agent that exists before its socket, stdout
without stderr, a bounded run, and a scope id short enough to crash
the one before it. A paper review found none of them.

```sh
go get github.com/geoffjay/roomkit
go test ./...
```
