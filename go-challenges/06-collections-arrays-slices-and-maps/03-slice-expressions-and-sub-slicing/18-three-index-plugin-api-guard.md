# Exercise 18: Guard a Plugin's Header View with a Three-Index Cap

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A pipelined proxy keeps several in-flight requests' headers in one shared
arena to avoid allocating a fresh `[]Header` per request. Each request gets
a view into its own segment of the arena -- and a plugin API hands that view
straight to third-party middleware for inspection and mutation, the same
shape as an auth plugin or a tracing plugin that injects a header before
forwarding the request upstream. If that view is a plain two-index
sub-slice, it inherits capacity all the way to the end of the arena, which
by the time the plugin runs may already hold the *next* request's headers.
A plugin doing nothing more suspicious than `append(headers, traceHeader)`
then silently overwrites data belonging to a request it has never seen.

This exercise builds `pluginguard`, a package whose `Pool.Alloc` is the only
way to get a view into the shared arena, and that view is always cut with
the three-index expression that caps capacity to length -- the unguarded,
corrupting version never appears in the package at all. It exists only in
the test file, as the thing a corruption test reproduces on purpose and then
proves `Alloc` prevents.

The same shape recurs anywhere a system hands out a mutable view into memory
it does not fully own on behalf of the caller: a scripting sandbox given a
slice of a host process's buffer, a WASM host function passed a view of
guest-controlled memory, a middleware chain in any language runtime where
"the framework" and "the plugin" are different trust domains compiled into
the same address space. The failure mode is always the same one this module
reproduces -- correct-looking code on both sides of the API, corruption that
shows up somewhere else entirely, at a time determined by scheduling rather
than by either side's own logic.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
pluginguard/                   module example.com/pluginguard
  go.mod                       go 1.24
  pluginguard.go                ErrInvalidCapacity, ErrPoolExhausted; Header; Pool with Alloc; InjectTraceID
  pluginguard_test.go            corruption reproduction, guarded protection, capacity-clip table, exhaustion, ExamplePool_Alloc
```

- Files: `pluginguard.go`, `pluginguard_test.go`.
- Implement: `NewPool(capacity int) (*Pool, error)`, a fixed-capacity arena of `Header` reused across sequential requests, rejecting a negative capacity with `ErrInvalidCapacity`; `(*Pool).Alloc(headers []Header) ([]Header, error)`, which copies `headers` into the arena and returns the guarded three-index view `arena[start:start+n:start+n]`, or `ErrPoolExhausted` if the arena has no room left; `InjectTraceID(headers []Header, traceID string) []Header`, standing in for a plugin that appends a header to whatever view it is handed.
- Test: allocate request A then request B from the same `Pool` through an unexported unguarded helper, run `InjectTraceID` on A's view, and assert B's already-stored header in the arena has been overwritten by the leaked trace header; repeat through the real `Alloc` and assert B's header survives untouched; a table proving `Alloc`'s result always has `cap == len` regardless of how many headers a request has, including zero; `Alloc` rejecting a request that exceeds the arena's remaining room; `NewPool` rejecting a negative capacity; `ExamplePool_Alloc` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the corruption needs a specific ordering to show up

The bug is easy to miss in a quick manual test because it depends on
*timing*, not just on an unguarded slice existing. A two-index allocation
copies a request's headers into the next free segment of `arena` and
returns `arena[start : start+n]` -- a two-index expression whose capacity is
`cap(arena) - start`, not `n`. If nothing else is ever written into the rest
of the arena, that spare capacity sits harmless and unreachable. The
corruption only becomes observable once a *second* request has already been
allocated into the slots the first request's view still has capacity to
reach -- exactly what happens on a pipelined connection, where several
requests are read and queued for processing before any of their plugins
run. `TestUnguardedAllocCorruptsNextRequest` reproduces that ordering
explicitly through an unexported `allocUnguarded` helper: it allocates
request A, allocates request B right after (so B's header now really does
live in the arena slot A's view has spare capacity over), and only *then*
runs the plugin on A. `InjectTraceID`'s `append` sees A's view has room and
writes the new header directly into `arena[2]` -- which is B's slot -- with
no reallocation, no panic, and no signal that anything went wrong.
`p.arena[2]` no longer holds `{Host, other.com}`; it holds A's leaked trace
header.

`Alloc` fixes this by construction, not as an alternate mode: it always cuts
`arena[start : start+n : start+n]`, setting `cap` equal to `len`, so the
identical `append` inside `InjectTraceID` finds no spare room and is forced
to allocate a fresh backing array for the result. The plugin still gets its
injected header back in `withTrace` -- functionally nothing changes from its
point of view -- but the write physically cannot land anywhere inside
`arena`, so request B's slot is untouched no matter what order requests are
processed in. `TestAllocAlwaysClipsCapacity` is the table that generalizes
this beyond the one worked example: whatever the size of a request's header
list, including zero headers, the guarded view's `cap` always equals its
`len`.

Create `pluginguard.go`:

```go
// Package pluginguard hands each request in a shared header arena a view
// that a third-party plugin can safely append to without reaching into a
// later request's slot. Every returned view is cut with the three-index
// full-slice expression, so its capacity always equals its length and any
// append a plugin performs is forced to allocate a fresh backing array
// instead of writing past the view into memory it does not own.
package pluginguard

import (
	"errors"
	"fmt"
)

// Sentinel errors returned by NewPool and Pool.Alloc.
var (
	// ErrInvalidCapacity means a negative capacity was requested.
	ErrInvalidCapacity = errors.New("pluginguard: capacity must not be negative")
	// ErrPoolExhausted means the arena has no room left for the request.
	ErrPoolExhausted = errors.New("pluginguard: not enough room left in the arena")
)

// Header is a single HTTP-style header pair.
type Header struct {
	Name  string
	Value string
}

// Pool is a fixed-capacity arena of headers reused across sequential
// requests on a pipelined connection, the way a high-throughput proxy
// avoids allocating a fresh []Header per request. Each request's headers
// occupy a contiguous segment of the same backing array, one after another,
// so an in-flight request's segment always has later requests' segments
// living right behind it in memory.
//
// A Pool is not safe for concurrent use: Alloc mutates the arena and the
// next-free-index cursor without synchronization, so requests must be
// allocated from a single goroutine (or under a caller-held lock).
type Pool struct {
	arena []Header
	next  int // next free index in arena
}

// NewPool allocates an arena with room for capacity headers total across
// every request currently in flight. It returns ErrInvalidCapacity if
// capacity is negative; a capacity of zero is valid and simply rejects
// every non-empty Alloc request.
func NewPool(capacity int) (*Pool, error) {
	if capacity < 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidCapacity, capacity)
	}
	return &Pool{arena: make([]Header, capacity)}, nil
}

// Alloc copies headers into the next free segment of the arena and returns
// a guarded view of it: the three-index expression
// arena[start:start+n:start+n], which forces len == cap. Any append a
// plugin performs on the result is therefore guaranteed to allocate a fresh
// backing array instead of writing into the arena slot reserved for the
// next request. It returns ErrPoolExhausted, allocating nothing, if headers
// does not fit in the arena's remaining room.
//
// The returned slice aliases Pool's own arena for reading (mutating an
// element in place is visible to anyone else holding the same view), but
// its capped capacity means append can never extend that aliasing to
// another request's segment.
func (p *Pool) Alloc(headers []Header) ([]Header, error) {
	if len(headers) > len(p.arena)-p.next {
		return nil, fmt.Errorf("%w: need %d, have %d", ErrPoolExhausted, len(headers), len(p.arena)-p.next)
	}
	start := p.next
	n := copy(p.arena[start:], headers)
	p.next = start + n
	return p.arena[start : start+n : start+n], nil
}

// InjectTraceID stands in for a plugin middleware that appends a header to
// whatever view of a request it is handed -- the same shape as a real auth
// or tracing plugin adding X-Trace-Id before forwarding a request upstream.
func InjectTraceID(headers []Header, traceID string) []Header {
	return append(headers, Header{Name: "X-Trace-Id", Value: traceID})
}
```

### Using it

`NewPool` is the only constructor, and it validates the one thing that can
be wrong about its configuration: a negative capacity. `Alloc` is the only
way to get a view into the arena, and every view it returns already carries
the three-index guard -- there is no unguarded alternative to reach for by
mistake, because that version simply does not exist in the package's
exported surface. A caller that exhausts the arena's remaining room gets
`ErrPoolExhausted`, checkable with `errors.Is`, rather than a silently
truncated write or an out-of-range panic. Because `Pool` mutates its arena
and cursor on every call without synchronization, it is not safe for
concurrent use: a server allocating requests from multiple goroutines onto
the same `Pool` needs its own lock around each `Alloc` call.

The module has no `main.go`, because a shared-arena allocator is a library
component, not a tool. Its executable demonstration is `ExamplePool_Alloc`:
`go test` runs it and compares its standard output against the `// Output:`
comment, so the usage shown below cannot drift away from the code.

```go
func ExamplePool_Alloc() {
	p, err := NewPool(10)
	if err != nil {
		panic(err)
	}

	a, err := p.Alloc([]Header{{Name: "Host", Value: "example.com"}, {Name: "Accept", Value: "*/*"}})
	if err != nil {
		panic(err)
	}
	b, err := p.Alloc([]Header{{Name: "Host", Value: "other.com"}})
	if err != nil {
		panic(err)
	}

	fmt.Printf("request B view before A's plugin runs: %v\n", b)
	withTrace := InjectTraceID(a, "trace-A")
	fmt.Printf("request B view after A's plugin runs:  %v\n", b)
	fmt.Printf("request A's own result: %v\n", withTrace)

	// Output:
	// request B view before A's plugin runs: [{Host other.com}]
	// request B view after A's plugin runs:  [{Host other.com}]
	// request A's own result: [{Host example.com} {Accept */*} {X-Trace-Id trace-A}]
}
```

`b`'s slice header never changes -- it is still `arena[2:3:3]` before and
after -- and now neither does the data at that address: `InjectTraceID`'s
`append` on `a` is forced to allocate elsewhere, so request B's own header
is exactly what it was allocated with. That absence of drama is the point:
a correctly guarded handoff produces no interesting output at all, which is
exactly why the unguarded version needs its own dedicated corruption test
rather than relying on a reader to notice something is missing from this
example.

### Tests

`allocUnguarded` is the antipattern this module is built around: it
performs the identical bookkeeping as `Alloc` but returns a plain two-index
sub-slice instead of the guarded three-index one. It is never exported and
never reachable from `Pool`; it exists solely so
`TestUnguardedAllocCorruptsNextRequest` can reproduce the bug with the exact
ordering that exposes it -- allocate A, allocate B, then run A's plugin --
and check that B's arena slot no longer holds B's own data.
`TestAllocProtectsNextRequest` runs the identical sequence through the real
`Alloc` and checks the opposite: B's data survives, and A's plugin result
still carries the injected header. `TestAllocAlwaysClipsCapacity`
generalizes the fix's guarantee across a table of request shapes, including
the zero-header edge case. `TestAllocRejectsExhaustion` and
`TestNewPoolRejectsNegativeCapacity` pin the two sentinel errors, checked
with `errors.Is`.

Create `pluginguard_test.go`:

```go
package pluginguard

import (
	"errors"
	"fmt"
	"testing"
)

// allocUnguarded is the antipattern this module warns against: identical to
// Alloc's bookkeeping, but its return is a plain two-index sub-slice, which
// inherits capacity all the way to the end of the arena. It advances the
// pool's next-free-index cursor correctly -- the corruption comes entirely
// from the capacity a plugin's append can silently reach into through the
// returned view. Never exported, never reachable from Pool; it exists only
// to reproduce the bug this module's design prevents.
func allocUnguarded(p *Pool, headers []Header) []Header {
	start := p.next
	n := copy(p.arena[start:], headers)
	p.next = start + n
	return p.arena[start : start+n]
}

// TestUnguardedAllocCorruptsNextRequest reproduces the bug: request B is
// allocated (and its header written into the arena) after request A's view
// was already handed to a plugin. When the plugin appends to A's view, the
// two-index sub-slice's inherited capacity lets that append write straight
// into B's already-stored header.
func TestUnguardedAllocCorruptsNextRequest(t *testing.T) {
	t.Parallel()

	p, err := NewPool(10)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	a := allocUnguarded(p, []Header{{Name: "Host", Value: "example.com"}, {Name: "Accept", Value: "*/*"}})
	allocUnguarded(p, []Header{{Name: "Host", Value: "other.com"}}) // request B, written right after A

	// A plugin processes request A after B has already landed in the arena
	// -- a normal ordering on a pipelined connection with several requests
	// in flight at once.
	_ = InjectTraceID(a, "trace-A")

	got := p.arena[2] // the slot request B's header was written into
	want := Header{Name: "Host", Value: "other.com"}
	if got == want {
		t.Fatal("expected the unguarded handoff to corrupt request B's header, but it survived untouched")
	}
	if got.Name != "X-Trace-Id" {
		t.Fatalf("arena[2] = %+v, want the X-Trace-Id header that leaked out of request A's plugin", got)
	}
}

// TestAllocProtectsNextRequest is the fix side of the same scenario: Alloc's
// three-index cap means the identical plugin append cannot reach request
// B's slot.
func TestAllocProtectsNextRequest(t *testing.T) {
	t.Parallel()

	p, err := NewPool(10)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	a, err := p.Alloc([]Header{{Name: "Host", Value: "example.com"}, {Name: "Accept", Value: "*/*"}})
	if err != nil {
		t.Fatalf("Alloc: %v", err)
	}
	if _, err := p.Alloc([]Header{{Name: "Host", Value: "other.com"}}); err != nil { // request B
		t.Fatalf("Alloc: %v", err)
	}

	withTrace := InjectTraceID(a, "trace-A")

	want := Header{Name: "Host", Value: "other.com"}
	if p.arena[2] != want {
		t.Fatalf("guarded handoff let request A's plugin corrupt request B: arena[2] = %+v, want %+v", p.arena[2], want)
	}
	if len(withTrace) != 3 || withTrace[2].Name != "X-Trace-Id" {
		t.Fatalf("InjectTraceID result = %+v, want a 3rd header named X-Trace-Id", withTrace)
	}
}

// TestAllocAlwaysClipsCapacity is the table: whatever the request looks
// like, Alloc's returned view must always have len == cap, so any append by
// a plugin is forced to reallocate.
func TestAllocAlwaysClipsCapacity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		headers []Header
	}{
		{"single header", []Header{{Name: "A", Value: "1"}}},
		{"multiple headers", []Header{{Name: "A", Value: "1"}, {Name: "B", Value: "2"}, {Name: "C", Value: "3"}}},
		{"zero headers", []Header{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p, err := NewPool(20)
			if err != nil {
				t.Fatalf("NewPool: %v", err)
			}
			got, err := p.Alloc(tc.headers)
			if err != nil {
				t.Fatalf("Alloc: %v", err)
			}
			if cap(got) != len(got) {
				t.Fatalf("Alloc(%v) len=%d cap=%d, want cap == len", tc.headers, len(got), cap(got))
			}
		})
	}
}

func TestAllocRejectsExhaustion(t *testing.T) {
	t.Parallel()

	p, err := NewPool(2)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	headers := []Header{{Name: "A", Value: "1"}, {Name: "B", Value: "2"}, {Name: "C", Value: "3"}}
	if _, err := p.Alloc(headers); !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("Alloc error = %v, want ErrPoolExhausted", err)
	}
}

func TestNewPoolRejectsNegativeCapacity(t *testing.T) {
	t.Parallel()

	if _, err := NewPool(-1); !errors.Is(err, ErrInvalidCapacity) {
		t.Fatalf("NewPool(-1) error = %v, want ErrInvalidCapacity", err)
	}
}

// ExamplePool_Alloc is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExamplePool_Alloc() {
	p, err := NewPool(10)
	if err != nil {
		panic(err)
	}

	a, err := p.Alloc([]Header{{Name: "Host", Value: "example.com"}, {Name: "Accept", Value: "*/*"}})
	if err != nil {
		panic(err)
	}
	b, err := p.Alloc([]Header{{Name: "Host", Value: "other.com"}})
	if err != nil {
		panic(err)
	}

	fmt.Printf("request B view before A's plugin runs: %v\n", b)
	withTrace := InjectTraceID(a, "trace-A")
	fmt.Printf("request B view after A's plugin runs:  %v\n", b)
	fmt.Printf("request A's own result: %v\n", withTrace)

	// Output:
	// request B view before A's plugin runs: [{Host other.com}]
	// request B view after A's plugin runs:  [{Host other.com}]
	// request A's own result: [{Host example.com} {Accept */*} {X-Trace-Id trace-A}]
}
```

## Review

The guarded handoff is correct when a plugin's `append` can never make
another request's data change value out from under it, and the two
corruption/protection tests are written to actually observe that -- through
the arena's own storage, not just through the return value of `Alloc` --
because the bug's whole danger is that it is invisible from the view of the
code holding the corrupted slice. The trap this module is built around is
assuming a quick "allocate then append" smoke test would have caught this:
it would not, because the corruption only appears once a second request has
been allocated into the reachable region *before* the first request's
plugin runs, which is exactly what pipelining does and exactly what
`TestUnguardedAllocCorruptsNextRequest` reproduces on purpose using a helper
that is deliberately kept out of the package's exported surface. Any API
that hands a caller a slice view into memory it does not fully own -- a
plugin system, a middleware chain, a scripting sandbox -- should default to
the three-index cap unless there is a specific, documented reason the
consumer needs write-through access to the shared backing array. `Alloc`
demonstrates the stronger version of that default: it does not merely
recommend the guard, it makes the unguarded form structurally unreachable
from outside the package, which is a more durable fix than a code-review
convention that someone eventually forgets to apply. Run
`go test -count=1 -race ./...`.

## Resources

- [Go Specification: Slice expressions (full slice expression)](https://go.dev/ref/spec#Slice_expressions) — the three-index form `arena[start:start+n:start+n]` this module's fix relies on.
- [`slices.Clip`](https://pkg.go.dev/slices#Clip) — the standard-library helper for the same len-equals-cap clip, useful once you already have a two-index slice in hand.
- [Go blog: Arrays, slices (and strings): The mechanics of 'append'](https://go.dev/blog/slices) — why append only reallocates once len == cap, the exact fact this exercise's bug and fix both hinge on.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [17-ring-wraparound-two-subslices-writev.md](17-ring-wraparound-two-subslices-writev.md) | Next: [19-ndjson-splitter-no-alloc-window.md](19-ndjson-splitter-no-alloc-window.md)
