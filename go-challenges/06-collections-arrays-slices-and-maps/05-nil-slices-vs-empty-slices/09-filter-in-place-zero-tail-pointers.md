# Exercise 9: Filter a slice of pointers in place without leaking memory

A cleanup pass removes expired sessions from a `[]*Session` using the standard
in-place filter idiom. Because the elements are pointers, the removed ones sit in
the truncated tail and keep their sessions alive against the garbage collector.
This exercise zeroes the tail so the memory can be reclaimed — and shows that
`slices.DeleteFunc` does this for you — while tying the empty result back to
nil-vs-empty.

This module is fully self-contained: its own `go mod init`, its own `sessions`
package, its own demo and tests.

## What you'll build

```text
sessioncleanup/               independent module: example.com/sessioncleanup
  go.mod
  sessions/sessions.go        Session, FilterActive (s[:0] + clear), FilterActiveDeleteFunc
  sessions/sessions_test.go   keeps-unexpired, zeroed-tail, empty-non-nil, DeleteFunc equivalence
  cmd/demo/main.go            filters a batch, shows the zeroed tail and empty result
```

Files: `sessions/sessions.go`, `sessions/sessions_test.go`, `cmd/demo/main.go`.
Implement: `FilterActive` using the `s[:0]` idiom followed by `clear` on the
vacated tail, and `FilterActiveDeleteFunc` using `slices.DeleteFunc`.
Test: unexpired sessions are kept in order; the tail is zeroed (removed pointers
dropped); filtering everything out yields an empty non-nil slice; `DeleteFunc`
matches the manual filter and also zeroes the tail.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/05-nil-slices-vs-empty-slices/09-filter-in-place-zero-tail-pointers/sessions go-solutions/06-collections-arrays-slices-and-maps/05-nil-slices-vs-empty-slices/09-filter-in-place-zero-tail-pointers/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/05-nil-slices-vs-empty-slices/09-filter-in-place-zero-tail-pointers
```

### Filtering in place, and the leak it hides

The in-place filter idiom is `kept := s[:0]`, then loop over `s` and `append` the
elements to keep back into the same backing array; because `kept` shares `s`'s
storage and never outruns the read cursor, this compacts the survivors to the
front and returns `s[:len(kept)]`. It allocates nothing, which is exactly why it
is the go-to for a cleanup pass. For a slice of *values* it is also complete.

For a slice of *pointers* it has a subtle leak. Truncating to `len(kept)` only
moves the length; the removed pointers still physically sit in the backing array,
in the capacity beyond the new length. Every one of those `*Session` pointers
keeps its `Session` reachable, so the garbage collector cannot reclaim the
sessions you just "removed." Over a long-running process that repeatedly filters a
reused buffer, that is a slow leak of exactly the objects you meant to drop.

The fix is to zero the vacated tail so those pointers become nil and stop pinning
anything: `clear(s[len(kept):])` sets every element from the new length to the old
capacity to the zero value, which for a pointer is nil. `FilterActive` does this.
`slices.DeleteFunc` is the standard-library filter that removes matching elements
in place *and* zeroes the freed tail as part of its contract, so
`FilterActiveDeleteFunc` gets the same memory-safe result with no manual clear —
prefer it in real code, and reach for the manual idiom only when you need the
explicit control.

Either way the result of filtering everything out is an empty but non-nil slice:
`s[:0]` of a non-nil slice is still non-nil, and `slices.DeleteFunc` returns the
same backing truncated to zero length. That is the nil-vs-empty tie-in — "removed
everything" is a known-empty collection, not an absent one, and it serializes to
`[]`, not `null`.

Create `sessions/sessions.go`:

```go
package sessions

import (
	"slices"
	"time"
)

// Session is a live login. The slice holds *Session, so a removed element left
// in the tail keeps the pointed-to Session alive against the garbage collector.
type Session struct {
	ID        string
	ExpiresAt time.Time
}

// Expired reports whether the session is no longer valid at now.
func (s *Session) Expired(now time.Time) bool {
	return !s.ExpiresAt.After(now)
}

// FilterActive keeps only unexpired sessions, filtering in place with the s[:0]
// idiom, then zeroes the vacated tail with clear so the removed *Session
// pointers no longer pin their sessions in memory. The result may legitimately
// be an empty, non-nil slice.
func FilterActive(sessions []*Session, now time.Time) []*Session {
	kept := sessions[:0]
	for _, s := range sessions {
		if !s.Expired(now) {
			kept = append(kept, s)
		}
	}
	clear(sessions[len(kept):]) // nil out the tail so dead pointers are dropped
	return kept
}

// FilterActiveDeleteFunc is the stdlib equivalent. slices.DeleteFunc removes
// matching elements in place AND zeroes the freed tail, so it does the same
// memory-safe cleanup without the manual clear.
func FilterActiveDeleteFunc(sessions []*Session, now time.Time) []*Session {
	return slices.DeleteFunc(sessions, func(s *Session) bool {
		return s.Expired(now)
	})
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/sessioncleanup/sessions"
)

func main() {
	now := time.Now()
	live := []*sessions.Session{
		{ID: "a", ExpiresAt: now.Add(time.Hour)},
		{ID: "b", ExpiresAt: now.Add(-time.Hour)}, // expired
		{ID: "c", ExpiresAt: now.Add(time.Minute)},
		{ID: "d", ExpiresAt: now.Add(-time.Minute)}, // expired
	}

	kept := sessions.FilterActive(live, now)
	ids := make([]string, len(kept))
	for i, s := range kept {
		ids[i] = s.ID
	}
	fmt.Printf("kept=%v len=%d\n", ids, len(kept))

	// The tail of the original backing array was zeroed.
	full := live[:cap(live)]
	fmt.Printf("tail[%d]=%v tail[%d]=%v\n", len(kept), full[len(kept)] == nil, len(kept)+1, full[len(kept)+1] == nil)

	// Filtering everything out gives an empty, non-nil slice.
	allExpired := []*sessions.Session{{ID: "x", ExpiresAt: now.Add(-time.Hour)}}
	empty := sessions.FilterActive(allExpired, now)
	fmt.Printf("all-expired: len=%d nil=%v\n", len(empty), empty == nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
kept=[a c] len=2
tail[2]=true tail[3]=true
all-expired: len=0 nil=false
```

The two `true`s are the proof: the slots that held the removed sessions are now
nil, so nothing pins those sessions anymore.

### Tests

`TestFilterActiveKeepsUnexpired` checks the survivors and their order.
`TestFilterZeroesTailPointers` is the leak guard: it walks the backing array from
the new length to the capacity and asserts every tail slot is nil, which is what
lets the GC reclaim the removed sessions. `TestFilterAllExpiredIsEmptyNonNil`
pins the nil-vs-empty contract of the result. `TestDeleteFuncEquivalentToManual`
proves the stdlib version matches, and `TestDeleteFuncZeroesTail` confirms
`slices.DeleteFunc` performs the same tail-zeroing.

Create `sessions/sessions_test.go`:

```go
package sessions

import (
	"slices"
	"testing"
	"time"
)

func idsOf(ss []*Session) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.ID
	}
	return out
}

func fixture(now time.Time) []*Session {
	return []*Session{
		{ID: "a", ExpiresAt: now.Add(time.Hour)},
		{ID: "b", ExpiresAt: now.Add(-time.Hour)},
		{ID: "c", ExpiresAt: now.Add(time.Minute)},
		{ID: "d", ExpiresAt: now.Add(-time.Minute)},
	}
}

func TestFilterActiveKeepsUnexpired(t *testing.T) {
	t.Parallel()
	now := time.Now()
	got := FilterActive(fixture(now), now)
	if want := []string{"a", "c"}; !slices.Equal(idsOf(got), want) {
		t.Fatalf("kept = %v, want %v", idsOf(got), want)
	}
}

func TestFilterZeroesTailPointers(t *testing.T) {
	t.Parallel()
	now := time.Now()
	in := fixture(now)
	kept := FilterActive(in, now)

	full := in[:cap(in)]
	for i := len(kept); i < len(full); i++ {
		if full[i] != nil {
			t.Fatalf("tail index %d not zeroed: %v (removed pointer still referenced)", i, full[i])
		}
	}
}

func TestFilterAllExpiredIsEmptyNonNil(t *testing.T) {
	t.Parallel()
	now := time.Now()
	in := []*Session{
		{ID: "x", ExpiresAt: now.Add(-time.Hour)},
		{ID: "y", ExpiresAt: now.Add(-time.Minute)},
	}
	got := FilterActive(in, now)
	if len(got) != 0 {
		t.Fatalf("len = %d, want 0", len(got))
	}
	if got == nil {
		t.Fatal("in-place filter of a non-nil slice must stay non-nil")
	}
}

func TestDeleteFuncEquivalentToManual(t *testing.T) {
	t.Parallel()
	now := time.Now()
	manual := FilterActive(fixture(now), now)
	stdlib := FilterActiveDeleteFunc(fixture(now), now)
	if !slices.Equal(idsOf(manual), idsOf(stdlib)) {
		t.Fatalf("manual %v != DeleteFunc %v", idsOf(manual), idsOf(stdlib))
	}
}

func TestDeleteFuncZeroesTail(t *testing.T) {
	t.Parallel()
	now := time.Now()
	in := fixture(now)
	kept := FilterActiveDeleteFunc(in, now)
	full := in[:cap(in)]
	for i := len(kept); i < len(full); i++ {
		if full[i] != nil {
			t.Fatalf("DeleteFunc left tail index %d referenced: %v", i, full[i])
		}
	}
}
```

## Review

The filter is correct when the survivors are kept in order and, crucially, when
the tail of the backing array is nil afterward — that zeroing is what turns
"removed from the slice" into "reclaimable by the GC" for a slice of pointers.
Without it the in-place filter compiles, passes a length check, and quietly pins
every removed session for as long as the backing array lives. Prefer
`slices.DeleteFunc`, which does the removal and the tail-zeroing together; use the
manual `s[:0]` plus `clear` when you need the explicit form. And note the result:
filtering everything out leaves an empty, non-nil slice — a known-empty
collection that serializes to `[]`, closing the loop back to where the lesson
started.

## Resources

- [slices.DeleteFunc](https://pkg.go.dev/slices#DeleteFunc) — removes matching elements in place and zeroes the freed tail.
- [The clear builtin](https://pkg.go.dev/builtin#clear) — zeroes all elements of a slice (or empties a map).
- [Go blog: Arrays, slices (and strings): the mechanics of append](https://go.dev/blog/slices) — backing arrays, capacity, and why the tail persists.

---

Back to [00-concepts.md](00-concepts.md) | Next: [10-feature-flag-map-json-boundary.md](10-feature-flag-map-json-boundary.md)
