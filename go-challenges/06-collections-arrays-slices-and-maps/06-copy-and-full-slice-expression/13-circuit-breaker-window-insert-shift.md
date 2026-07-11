# Exercise 13: Insert-At-Front on a Circuit Breaker's Rolling Outcome Window

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A rate-based circuit breaker -- the kind Hystrix and resilience4j popularized,
now baked into most service meshes -- decides whether to trip open by looking
at a rolling window of the most recent call outcomes: the last N successes
and failures, newest first, oldest evicted once the window is full. The
window itself is a small fixed-size slice, and the operation that keeps it
ordered is a right-shift: every existing entry moves one slot toward the
back, the newest outcome lands at index 0, and whatever fell off the end is
gone. That shift is one line, `copy(w.data[1:], w.data[:len(w.data)-1])`,
and it is correct for a reason that is easy to take for granted: `copy`
behaves like C's `memmove`, not `memcpy`. It reads every source byte before
it writes any destination byte, so it does not matter that the source and
destination regions overlap -- and here they overlap completely, shifted by
exactly one slot.

The version of this shift a developer writes without knowing that fact is a
plain `for` loop copying element `i` into slot `i+1`. It looks like exactly
the same operation, one assignment at a time instead of one `copy` call, and
it passes a test that only pushes a single outcome and checks the front of
the window. It fails catastrophically under the load that actually exercises
it: every slot after the first collapses to the *previous* newest value, not
the one that was really there, because the loop's own earlier iteration
already overwrote the very slot it is about to read from. This module builds
the window with the correct shift and pins the forward-loop version's
corruption directly, element by element.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
breaker/                   module example.com/breaker
  go.mod                   go 1.24
  breaker.go               Outcome, Window, NewWindow, Push, Outcomes, Len, Cap
  breaker_test.go           push table, aliasing, the pushLoopBuggy contrast,
                            ExampleWindow_Push
```

- Files: `breaker.go`, `breaker_test.go`.
- Implement: `NewWindow(capacity int) (*Window, error)` rejecting a non-positive capacity with `ErrInvalidCapacity`; `(*Window).Push(outcome Outcome)` shifting every entry one slot back with `copy(w.data[1:], w.data[:len(w.data)-1])` and writing `outcome` at index 0; `(*Window).Outcomes() []Outcome` returning a cloned, newest-first view; `Len` and `Cap`.
- Test: the push table (empty window, single push, exactly full, past capacity, capacity one); `NewWindow` rejecting a non-positive capacity; `Outcomes` never aliasing the window's internal storage; a `pushLoopBuggy` contrast pinning the exact duplicated-value corruption a forward loop produces; and `ExampleWindow_Push` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/breaker
cd ~/go-exercises/breaker
go mod init example.com/breaker
go mod edit -go=1.24
```

### `copy` is memmove-correct; a hand-rolled forward loop is not

`copy(dst, src)` is specified to behave correctly even when `dst` and `src`
overlap, exactly like `memmove`. Shifting a slice right by one slot in place
is the textbook case of total overlap: `w.data[1:]` and `w.data[:len(w.data)-1]`
share every element except the very last and very first. `copy` handles that
by reading the whole source before committing any write to the destination
(conceptually -- the real implementation is smarter, but the observable
guarantee is the same): every value that needs to move ends up in the right
place, once, undisturbed by writes the same call is making elsewhere.

A `for` loop that writes the assignments out one at a time does not get this
for free. Shifting right with an *ascending* index is the wrong direction:

```go
for i := 0; i < len(data)-1; i++ {
    data[i+1] = data[i]   // wrong direction for a rightward shift
}
```

At `i=0`, `data[1]` is set to `data[0]`'s original value -- fine so far. But at
`i=1`, the loop reads `data[1]` to write into `data[2]` -- and `data[1]` was
just overwritten in the previous iteration. It no longer holds what was
originally there; it holds a copy of `data[0]`. That value then propagates
forward again at `i=2`, and again at every step after. The whole tail of the
window collapses into repeated copies of whatever was at index 0 before the
loop started, and the *actual* history -- what was really at index 1, 2, 3 --
is gone, overwritten before it was ever read. Reversing the loop to walk
backward (`for i := len(data)-1; i > 0; i--`) would fix it, and is exactly
what `copy` already does internally; there is no reason to hand-roll either
version once `copy` exists.

Create `breaker.go`:

```go
// Package breaker implements the rolling outcome window behind a
// rate-based circuit breaker, the mechanism Hystrix and resilience4j use to
// decide whether recent calls have failed often enough to trip.
//
// The detail this package exists to get right is the in-place shift Push
// performs to keep the newest outcome at index 0: it uses copy, which
// behaves like memmove and is correct for overlapping regions regardless
// of direction. See Push's doc comment for the naive alternative that gets
// this wrong.
package breaker

import (
	"errors"
	"fmt"
	"slices"
)

// Outcome is the result of one call the breaker is tracking.
type Outcome int

const (
	// Success marks a call that completed without error.
	Success Outcome = iota
	// Failure marks a call that errored or timed out.
	Failure
)

// String implements fmt.Stringer.
func (o Outcome) String() string {
	switch o {
	case Success:
		return "success"
	case Failure:
		return "failure"
	default:
		return fmt.Sprintf("Outcome(%d)", int(o))
	}
}

// ErrInvalidCapacity means NewWindow was given a non-positive capacity.
var ErrInvalidCapacity = errors.New("breaker: capacity must be positive")

// Window is a fixed-capacity rolling window of the most recent call
// outcomes, newest first. Once full, each Push evicts the oldest entry.
//
// Not safe for concurrent use by multiple goroutines; the caller must
// synchronize calls to Push and Outcomes.
type Window struct {
	data   []Outcome
	filled int
}

// NewWindow returns a Window that retains the capacity most recent
// outcomes. It returns ErrInvalidCapacity if capacity is not positive.
func NewWindow(capacity int) (*Window, error) {
	if capacity <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidCapacity, capacity)
	}
	return &Window{data: make([]Outcome, capacity)}, nil
}

// Push records outcome as the newest entry, shifting every existing entry
// one slot toward the back and evicting the oldest if the window is full.
//
// The shift is copy(w.data[1:], w.data[:len(w.data)-1]): copy behaves like
// memmove, reading each source byte before the destination overwrites it,
// regardless of how the two regions overlap. That is what makes shifting an
// entire slice right by one slot, in place, correct. A naive forward
// for-loop assignment does not have this property; see pushLoopBuggy in the
// test file for what it produces instead.
func (w *Window) Push(outcome Outcome) {
	copy(w.data[1:], w.data[:len(w.data)-1])
	w.data[0] = outcome
	if w.filled < len(w.data) {
		w.filled++
	}
}

// Outcomes returns the window's entries, newest first. It returns a freshly
// cloned slice that never aliases the Window's internal storage: the caller
// may retain, sort, or mutate the result freely.
func (w *Window) Outcomes() []Outcome {
	return slices.Clone(w.data[:w.filled])
}

// Len reports how many real outcomes the window currently holds, at most
// its configured capacity.
func (w *Window) Len() int { return w.filled }

// Cap reports the window's configured capacity.
func (w *Window) Cap() int { return len(w.data) }
```

### Using it

Build one `Window` per breaker with `NewWindow(capacity)`, and call `Push`
once per call outcome as it completes. `Outcomes()` gives you the current
history, newest first, as an independent slice -- the breaker's rate
calculation can sort it, filter it, or hold onto it across a tick without
any risk of the window's own next `Push` mutating memory the caller is still
reading. `Window` is not safe for concurrent use: a real breaker typically
serializes outcome recording through a single goroutine or its own mutex,
so this type deliberately does not add one of its own.

`ExampleWindow_Push` in the test file is the executable demonstration of
this module: `go test` runs it and compares its stdout against the
`// Output:` comment, so the usage shown below cannot drift from the code.
It pushes four outcomes into a capacity-3 window and prints the window
after each one, showing the oldest entry drop off once the window fills.

### Tests

`TestPush` is the table: an empty window, a single push, exactly filling
the window, pushing past capacity to see the oldest entry evicted, and the
capacity-one boundary where every push replaces the only slot.
`TestNewWindowRejectsNonPositiveCapacity` and
`TestWindowCapReportsConfiguredCapacity` pin the constructor's validation
and its unchanging capacity. `TestOutcomesDoesNotAliasInternalState`
mutates a returned slice and confirms the window's own storage, and a
subsequent `Outcomes()` call, are unaffected.

`TestPushLoopBuggyDuplicatesFirstElement` is the heart of the module. It
runs the same three-outcome sequence through `Window.Push` and through
`pushLoopBuggy`, an unexported helper operating directly on a raw slice --
never reachable from `Window` -- that performs the shift with a forward
`for` loop. The correct sequence reads back as `[Success Failure Success]`;
the buggy one collapses to `[Success Failure Failure]`, because by the time
the loop reaches index 2 it is reading a slot that an earlier iteration in
the *same call* already overwrote. The test asserts the buggy result
explicitly rather than merely asserting it differs from the correct one, so
a future change to `pushLoopBuggy` that accidentally fixes it is caught too.

Create `breaker_test.go`:

```go
package breaker

import (
	"errors"
	"fmt"
	"slices"
	"testing"
)

// pushLoopBuggy is Push's shift as it is usually written the first time: a
// forward for-loop copying element i into slot i+1. It is never exported
// and never reachable from Window; it exists only so the tests can pin the
// corruption it produces.
func pushLoopBuggy(data []Outcome, outcome Outcome) {
	for i := 0; i < len(data)-1; i++ {
		data[i+1] = data[i]
	}
	data[0] = outcome
}

func TestNewWindowRejectsNonPositiveCapacity(t *testing.T) {
	t.Parallel()

	for _, capacity := range []int{0, -1, -10} {
		if _, err := NewWindow(capacity); !errors.Is(err, ErrInvalidCapacity) {
			t.Errorf("NewWindow(%d) error = %v, want ErrInvalidCapacity", capacity, err)
		}
	}
}

func TestWindowCapReportsConfiguredCapacity(t *testing.T) {
	t.Parallel()

	w, err := NewWindow(5)
	if err != nil {
		t.Fatalf("NewWindow: %v", err)
	}
	if w.Cap() != 5 {
		t.Fatalf("Cap() = %d, want 5", w.Cap())
	}
	w.Push(Success)
	if w.Cap() != 5 {
		t.Fatalf("Cap() after a Push = %d, want unchanged 5", w.Cap())
	}
}

func TestPush(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		capacity int
		pushes   []Outcome
		want     []Outcome
	}{
		{
			name:     "no pushes yields an empty window",
			capacity: 3,
			pushes:   nil,
			want:     []Outcome{},
		},
		{
			name:     "single push into empty window",
			capacity: 3,
			pushes:   []Outcome{Success},
			want:     []Outcome{Success},
		},
		{
			name:     "fills exactly to capacity",
			capacity: 3,
			pushes:   []Outcome{Success, Failure, Success},
			want:     []Outcome{Success, Failure, Success},
		},
		{
			name:     "push past capacity evicts the oldest",
			capacity: 3,
			pushes:   []Outcome{Success, Failure, Success, Failure},
			want:     []Outcome{Failure, Success, Failure},
		},
		{
			name:     "capacity one keeps only the newest",
			capacity: 1,
			pushes:   []Outcome{Success, Success, Failure},
			want:     []Outcome{Failure},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			w, err := NewWindow(tc.capacity)
			if err != nil {
				t.Fatalf("NewWindow: %v", err)
			}
			for _, o := range tc.pushes {
				w.Push(o)
			}
			got := w.Outcomes()
			if !slices.Equal(got, tc.want) {
				t.Fatalf("Outcomes() = %v, want %v", got, tc.want)
			}
			wantLen := min(len(tc.pushes), tc.capacity)
			if w.Len() != wantLen {
				t.Fatalf("Len() = %d, want %d", w.Len(), wantLen)
			}
		})
	}
}

func TestOutcomesDoesNotAliasInternalState(t *testing.T) {
	t.Parallel()

	w, err := NewWindow(3)
	if err != nil {
		t.Fatalf("NewWindow: %v", err)
	}
	w.Push(Success)
	w.Push(Failure)

	got := w.Outcomes()
	got[0] = Failure // mutate the returned slice

	again := w.Outcomes()
	if again[0] != Failure {
		// again[0] is the second Push (Failure); the mutation above should
		// not have changed it either way, but it must not have touched
		// Window's own storage.
		t.Fatalf("Outcomes() after mutating a prior result = %v, want unaffected", again)
	}
	w.Push(Success)
	third := w.Outcomes()
	if third[0] != Success || third[1] != Failure || third[2] != Success {
		t.Fatalf("Outcomes() after a further Push = %v, want [success failure success]", third)
	}
}

// TestPushLoopBuggyDuplicatesFirstElement is the heart of the module: it
// pins the exact corruption a forward for-loop shift produces, so a
// regression in Push's own copy-based shift would need to reintroduce this
// exact pattern to reappear, and this test would catch it if it did.
func TestPushLoopBuggyDuplicatesFirstElement(t *testing.T) {
	t.Parallel()

	// Correct shift, via Window.Push: [Success Failure Success] pushed in
	// order should read back newest-first as [Success Failure Success].
	w, err := NewWindow(3)
	if err != nil {
		t.Fatalf("NewWindow: %v", err)
	}
	w.Push(Success)
	w.Push(Failure)
	w.Push(Success)
	want := []Outcome{Success, Failure, Success}
	if got := w.Outcomes(); !slices.Equal(got, want) {
		t.Fatalf("Window.Push sequence = %v, want %v", got, want)
	}

	// The same three pushes through the buggy forward loop: every slot
	// after the first collapses to the *previous* newest value instead of
	// truly shifting, because data[i] has already been overwritten by the
	// time it is read to fill data[i+1].
	data := make([]Outcome, 3)
	pushLoopBuggy(data, Success)
	pushLoopBuggy(data, Failure)
	pushLoopBuggy(data, Success)

	wantBuggy := []Outcome{Success, Failure, Failure}
	if !slices.Equal(data, wantBuggy) {
		t.Fatalf("pushLoopBuggy sequence = %v, want %v (duplicated, not shifted)", data, wantBuggy)
	}
	if slices.Equal(data, want) {
		t.Fatal("pushLoopBuggy accidentally matched the correct shift; the test no longer demonstrates the bug")
	}
}

// ExampleWindow_Push is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExampleWindow_Push() {
	w, err := NewWindow(3)
	if err != nil {
		panic(err)
	}

	for _, o := range []Outcome{Success, Success, Failure, Failure} {
		w.Push(o)
		fmt.Println(w.Outcomes())
	}

	// Output:
	// [success]
	// [success success]
	// [failure success success]
	// [failure failure success]
}
```

## Review

`Push` is correct when the window's newest-first history exactly matches
the order outcomes were pushed in, at every step -- `TestPush` pins that
across the empty, filling, full, and eviction cases, and
`TestPushLoopBuggyDuplicatesFirstElement` shows precisely what a forward
loop gets wrong instead: the tail of the window collapses to repeated
copies of an earlier value because the loop reads a slot after its own
previous iteration already overwrote it. `copy` avoids this because it is
specified to behave like `memmove`, correct for overlapping source and
destination regardless of direction, which a hand-written loop only gets by
choosing the right iteration order -- and there is no reason to make that
choice by hand once `copy` exists. Around that core, `NewWindow` rejects a
non-positive capacity with `ErrInvalidCapacity`, `Outcomes` never aliases
the window's internal storage so a caller can hold and mutate its own copy
freely, and `Window` is explicitly not safe for concurrent use. Run
`go test -count=1 -race ./...` to confirm all of it, including
`ExampleWindow_Push`, the runnable demonstration `go test` checks against
its `// Output:` comment.

## Resources

- [`copy`](https://go.dev/ref/spec#Appending_and_copying_slices) — the spec paragraph guaranteeing correct behavior for overlapping source and destination.
- [`slices.Clone`](https://pkg.go.dev/slices#Clone) — the copy used to keep `Outcomes` independent of the window's internal storage.
- [Hystrix wiki: How it Works](https://github.com/Netflix/Hystrix/wiki/How-it-Works) — the rolling-window, rate-based tripping model this exercise builds the data structure for.
- [Go Wiki: SliceTricks](https://go.dev/wiki/SliceTricks) — more `copy`-based in-place slice manipulations, including deletion.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-arena-allocator-reslice-not-isolation.md](12-arena-allocator-reslice-not-isolation.md) | Next: [14-rate-limiter-registry-pointer-map-clone.md](14-rate-limiter-registry-pointer-map-clone.md)
