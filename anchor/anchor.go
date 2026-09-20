// Package anchor holds the anchor lifecycle: an anchor records where
// something sits and which revision it was recorded against, and it is
// re-resolved when the underlying content moves.
//
// The lifecycle is shared; the addressing scheme is not. ocre anchors
// on diff lines, lore anchors on OKF locations, so the payload stays
// opaque here and each app supplies a Resolver.
//
// An anchor is never mutated in place. Remap returns a new anchor, so
// the original record of "where this was said" survives.
package anchor

import (
	"bytes"
	"encoding/json"
)

// Anchor addresses a location at a known revision.
type Anchor struct {
	Scheme       string          `json:"scheme"`
	Payload      json.RawMessage `json:"payload"`
	BaseRevision string          `json:"base_revision"`
}

// Status is the outcome of re-resolving an anchor against head.
type Status string

const (
	// Resolved: the anchor still addresses the same content.
	Resolved Status = "resolved"
	// Moved: the content shifted; the returned anchor addresses it.
	Moved Status = "moved"
	// Outdated: the content is gone and the anchor cannot be placed.
	Outdated Status = "outdated"
)

// Resolver maps an anchor forward onto new content. Implementations
// must not modify the anchor they are given.
type Resolver interface {
	Resolve(a Anchor, headRevision string, head []byte) (Status, Anchor)
}

// ResolverFunc adapts a function to Resolver.
type ResolverFunc func(a Anchor, headRevision string, head []byte) (Status, Anchor)

// Resolve implements Resolver.
func (f ResolverFunc) Resolve(a Anchor, headRevision string, head []byte) (Status, Anchor) {
	return f(a, headRevision, head)
}

// Remap re-resolves an anchor against head. An anchor recorded at the
// head revision resolves unchanged without consulting the resolver.
// With no resolver, a stale anchor is Outdated rather than guessed.
//
// Remap never mutates its input: on Moved it returns a new anchor
// carrying the head revision.
func Remap(r Resolver, a Anchor, headRevision string, head []byte) (Status, Anchor) {
	if a.BaseRevision == headRevision {
		return Resolved, a
	}
	if r == nil {
		return Outdated, a
	}
	status, next := r.Resolve(a, headRevision, head)
	if status == Moved {
		next.BaseRevision = headRevision
		if next.Scheme == "" {
			next.Scheme = a.Scheme
		}
	}
	return status, next
}

// LineRange locates a 1-based inclusive line range from old content
// inside new content by matching the anchored text exactly.
//
// This is the generic half of the renumbering problem a knowledge or
// review document hits whenever an edit inserts or removes lines above
// an anchor. It returns the new range and true when the text occurs
// exactly once; when the text is gone or ambiguous it returns false and
// the caller marks the anchor Outdated.
func LineRange(old, next []byte, start, end int) (int, int, bool) {
	oldLines := splitLines(old)
	if start < 1 || end < start || end > len(oldLines) {
		return 0, 0, false
	}
	target := oldLines[start-1 : end]
	newLines := splitLines(next)
	found, at := 0, 0
	for i := 0; i+len(target) <= len(newLines); i++ {
		if equalLines(newLines[i:i+len(target)], target) {
			found++
			at = i
			if found > 1 {
				return 0, 0, false // ambiguous
			}
		}
	}
	if found != 1 {
		return 0, 0, false
	}
	return at + 1, at + len(target), true
}

func splitLines(b []byte) [][]byte { return bytes.Split(b, []byte("\n")) }

func equalLines(a, b [][]byte) bool {
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}
