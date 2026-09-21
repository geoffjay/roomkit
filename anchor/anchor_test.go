package anchor

import (
	"encoding/json"
	"testing"
)

func mk(rev string, payload string) Anchor {
	return Anchor{Scheme: "test", Payload: json.RawMessage(payload), BaseRevision: rev}
}

func TestAnchorAtHeadResolvesWithoutResolver(t *testing.T) {
	a := mk("rev1", `{"line":3}`)
	status, got := Remap(nil, a, "rev1", nil, nil)
	if status != Resolved {
		t.Errorf("status = %q, want %q", status, Resolved)
	}
	if got.BaseRevision != "rev1" {
		t.Errorf("anchor changed: %+v", got)
	}
}

// With no resolver a stale anchor is outdated, never guessed.
func TestStaleAnchorWithoutResolverIsOutdated(t *testing.T) {
	a := mk("rev1", `{"line":3}`)
	status, _ := Remap(nil, a, "rev2", nil, nil)
	if status != Outdated {
		t.Errorf("status = %q, want %q", status, Outdated)
	}
}

// Remap returns a new anchor carrying head, and leaves the original
// record of where something was said untouched.
func TestRemapDoesNotMutateTheOriginal(t *testing.T) {
	orig := mk("rev1", `{"line":3}`)
	r := ResolverFunc(func(a Anchor, head string, _, _ []byte) (Status, Anchor) {
		return Moved, Anchor{Payload: json.RawMessage(`{"line":9}`)}
	})
	status, next := Remap(r, orig, "rev2", nil, nil)
	if status != Moved {
		t.Fatalf("status = %q, want %q", status, Moved)
	}
	if next.BaseRevision != "rev2" {
		t.Errorf("remapped anchor revision = %q, want rev2", next.BaseRevision)
	}
	if next.Scheme != "test" {
		t.Errorf("remapped anchor lost its scheme: %q", next.Scheme)
	}
	if orig.BaseRevision != "rev1" || string(orig.Payload) != `{"line":3}` {
		t.Errorf("original anchor was mutated: %+v", orig)
	}
}

// The spike's renumbering defect: an edit removed three lines above an
// anchor, so every later line shifted. LineRange finds the new range
// instead of forcing the anchor outdated.
func TestLineRangeFollowsShiftedText(t *testing.T) {
	old := []byte("---\ntype: Decision\nverified:\n  by: human:geoff\n  at: t\n---\n\nthe anchored line\ntail\n")
	next := []byte("---\ntype: Decision\n---\n\nthe anchored line\ntail\n")
	start, end, ok := LineRange(old, next, 8, 8)
	if !ok {
		t.Fatal("anchored line not found after the verified block was dropped")
	}
	if start != 5 || end != 5 {
		t.Errorf("remapped range = %d-%d, want 5-5", start, end)
	}
}

func TestLineRangeMultiLineBlock(t *testing.T) {
	old := []byte("a\nb\nfirst\nsecond\nz\n")
	next := []byte("header\nadded\na\nb\nfirst\nsecond\nz\n")
	start, end, ok := LineRange(old, next, 3, 4)
	if !ok || start != 5 || end != 6 {
		t.Errorf("range = %d-%d ok=%v, want 5-6 true", start, end, ok)
	}
}

func TestLineRangeRefusesMissingAndAmbiguous(t *testing.T) {
	old := []byte("a\nunique line\nb\n")
	if _, _, ok := LineRange(old, []byte("a\nb\n"), 2, 2); ok {
		t.Error("deleted text reported as found")
	}
	if _, _, ok := LineRange(old, []byte("unique line\nunique line\n"), 2, 2); ok {
		t.Error("ambiguous match reported as found; the caller must mark it outdated")
	}
	if _, _, ok := LineRange(old, old, 0, 2); ok {
		t.Error("out-of-range start accepted")
	}
}

// TestResolverComposesWithLineRange is finding #5 from the second
// consumer. Resolve used to receive head alone while LineRange needs
// the base content too, so both apps closed over their own revision
// store inside the resolver to feed a helper that ships here. A
// resolver now gets both contents, so a line scheme is a package-level
// value with no state of its own.
var lineScheme = ResolverFunc(func(a Anchor, _ string, base, head []byte) (Status, Anchor) {
	var at struct {
		Line int `json:"line"`
	}
	if json.Unmarshal(a.Payload, &at) != nil {
		return Outdated, a
	}
	start, _, ok := LineRange(base, head, at.Line, at.Line)
	if !ok {
		return Outdated, a
	}
	if start == at.Line {
		return Resolved, a
	}
	at.Line = start
	payload, _ := json.Marshal(at)
	return Moved, Anchor{Scheme: a.Scheme, Payload: payload}
})

func TestResolverComposesWithLineRange(t *testing.T) {
	base := []byte("alpha\nbeta\nthe anchored line\n")
	head := []byte("added\nalpha\nbeta\nthe anchored line\n")
	a := mk("rev1", `{"line":3}`)

	status, next := Remap(lineScheme, a, "rev2", base, head)
	if status != Moved {
		t.Fatalf("status = %q, want %q", status, Moved)
	}
	if string(next.Payload) != `{"line":4}` {
		t.Errorf("payload = %s, want {\"line\":4}", next.Payload)
	}
	if next.BaseRevision != "rev2" {
		t.Errorf("remapped anchor revision = %q, want rev2", next.BaseRevision)
	}

	// The same anchor against content that no longer holds the text.
	if status, _ := Remap(lineScheme, a, "rev2", base, []byte("gone\n")); status != Outdated {
		t.Errorf("status = %q, want %q when the anchored text is gone", status, Outdated)
	}
}
