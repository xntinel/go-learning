# 1. GMP Model

The Go runtime scheduler multiplexes goroutines onto OS threads using three
abstractions: G (goroutine), M (machine / OS thread), and P (processor). The
tricky part is that none of these map 1:1: thousands of G's can share a handful
of M's, each P owns a local run queue, and M's detach from P's during blocking
syscalls. This lesson builds a user-space tracker that makes those lifecycle
transitions observable and testable.

## Concepts

### The three-layer model

**G (goroutine)** starts with a 2-8 KB stack that grows on demand. It is not
pinned to any OS thread; the scheduler moves it around freely.

**M (machine)** is an OS thread. The runtime creates M's as needed (when
goroutines block in syscalls) and caches idle M's rather than destroying them.
The number of active M's can exceed GOMAXPROCS when goroutines are in blocking
calls.

**P (processor)** is a logical CPU slot. The count equals GOMAXPROCS (default
`runtime.NumCPU()`). Each P holds a local run queue of runnable G's. An M must
hold a P to execute Go code.

### Scheduling steps

1. A new goroutine is placed on the local run queue of the current P.
2. When an M finishes a G, it pops the next G from its P's local queue.
3. If the local queue is empty the M steals half the G's from another P's queue
   (work stealing, covered in lesson 03).
4. When a goroutine calls a blocking syscall, the runtime detaches the P from
   the M so a different M can take the P and keep running goroutines.

### Why not one goroutine per OS thread?

OS thread creation costs ~1 MB of stack plus kernel bookkeeping. Goroutine
creation costs ~2-8 KB. A server handling 100 000 concurrent requests needs
goroutines, not threads. The Go scheduler handles multiplexing entirely in
user space, avoiding kernel context switches for most scheduling decisions.

### Observable states (G lifecycle)

A goroutine is always in one of four conceptual states:

| State   | Meaning                                           |
|---------|---------------------------------------------------|
| created | allocated, not yet runnable                       |
| running | assigned to an M/P, executing instructions        |
| parked  | waiting (channel, mutex, sleep, syscall)          |
| done    | exited; resources will be reclaimed               |

The standard library exposes counts (`runtime.NumGoroutine()`) but not
per-goroutine states. This lesson implements a user-space analog so you can
observe the state machine in tests.

### Failure modes

- Goroutine leak: G's stuck in `parked` forever because no one closes their
  channel. `runtime.NumGoroutine()` stays elevated.
- Thread explosion: many goroutines blocked in CGO or raw syscalls cause
  unbounded M creation. Mitigate with worker pools.
- P starvation: one goroutine monopolises a P with a tight loop, starving
  others. Go 1.14 added async preemption (SIGURG) to break this.

## Exercises

### Exercise 1: implement the GMP state tracker

Create a package `gmpstate` that tracks goroutine lifecycle events. The tracker
is a user-space model -- it does not interact with the real scheduler -- but it
mirrors the G state machine and gives you testable, deterministic behavior.

Valid transitions:

```
created  -> running
running  -> parked
running  -> done
parked   -> running
```

Any other transition must return an error.

Module path is `example.com/gmpstate`. Set up the module:

```go
// go.mod
module example.com/gmpstate

go 1.26
```

Create `gmpstate.go`:

```go
// gmpstate.go
package gmpstate

import (
	"errors"
	"fmt"
	"sync"
)

// State represents a goroutine lifecycle state.
type State int

const (
	Created State = iota
	Running
	Parked
	Done
)

func (s State) String() string {
	switch s {
	case Created:
		return "created"
	case Running:
		return "running"
	case Parked:
		return "parked"
	case Done:
		return "done"
	default:
		return "unknown"
	}
}

// GoroutineID is an opaque identifier for a tracked goroutine.
type GoroutineID int64

// ErrInvalidTransition is returned when a state transition is not permitted.
var ErrInvalidTransition = errors.New("invalid state transition")

// ErrUnknownID is returned when the GoroutineID is not registered.
var ErrUnknownID = errors.New("unknown goroutine id")

// Tracker records goroutine lifecycle events.
type Tracker struct {
	mu     sync.Mutex
	next   GoroutineID
	states map[GoroutineID]State
	counts [4]int // indexed by State constant
}

// NewTracker returns an initialised Tracker.
func NewTracker() *Tracker {
	return &Tracker{
		states: make(map[GoroutineID]State),
	}
}

// Create registers a new goroutine in state Created and returns its ID.
func (t *Tracker) Create() GoroutineID {
	t.mu.Lock()
	defer t.mu.Unlock()
	id := t.next
	t.next++
	t.states[id] = Created
	t.counts[Created]++
	return id
}

// Transition moves goroutine id to state s.
// Returns ErrInvalidTransition for illegal moves, ErrUnknownID if id is not registered.
func (t *Tracker) Transition(id GoroutineID, s State) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	cur, ok := t.states[id]
	if !ok {
		return fmt.Errorf("goroutine %d: %w", id, ErrUnknownID)
	}
	if !validTransition(cur, s) {
		return fmt.Errorf("goroutine %d: %s -> %s: %w", id, cur, s, ErrInvalidTransition)
	}
	t.counts[cur]--
	t.counts[s]++
	t.states[id] = s
	return nil
}

// Count returns the number of goroutines currently in state s.
func (t *Tracker) Count(s State) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.counts[s]
}

// All returns a snapshot of all tracked GoroutineIDs.
func (t *Tracker) All() []GoroutineID {
	t.mu.Lock()
	defer t.mu.Unlock()
	ids := make([]GoroutineID, 0, len(t.states))
	for id := range t.states {
		ids = append(ids, id)
	}
	return ids
}

// validTransition reports whether moving from cur to next is allowed.
func validTransition(cur, next State) bool {
	switch cur {
	case Created:
		return next == Running
	case Running:
		return next == Parked || next == Done
	case Parked:
		return next == Running
	default: // Done
		return false
	}
}
```

Create `gmpstate_test.go`:

```go
// gmpstate_test.go
package gmpstate

import (
	"errors"
	"testing"
)

func TestCreate(t *testing.T) {
	t.Parallel()
	tr := NewTracker()
	id := tr.Create()
	if tr.Count(Created) != 1 {
		t.Fatalf("Count(Created) = %d, want 1", tr.Count(Created))
	}
	if len(tr.All()) != 1 {
		t.Fatalf("All() len = %d, want 1", len(tr.All()))
	}
	_ = id
}

func TestTransitionValid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		transitions []State
	}{
		{"created-running-done", []State{Running, Done}},
		{"created-running-parked-running-done", []State{Running, Parked, Running, Done}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := NewTracker()
			id := tr.Create()
			for _, s := range tc.transitions {
				if err := tr.Transition(id, s); err != nil {
					t.Fatalf("Transition(%v) unexpected error: %v", s, err)
				}
			}
		})
	}
}

func TestTransitionInvalid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		from State
		to   State
	}{
		{"created-parked", Created, Parked},
		{"created-done", Created, Done},
		{"running-created", Running, Created},
		{"parked-done", Parked, Done},
		{"done-running", Done, Running},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := NewTracker()
			id := tr.Create()
			// Advance to the 'from' state via valid path first.
			switch tc.from {
			case Running:
				_ = tr.Transition(id, Running)
			case Parked:
				_ = tr.Transition(id, Running)
				_ = tr.Transition(id, Parked)
			case Done:
				_ = tr.Transition(id, Running)
				_ = tr.Transition(id, Done)
			}
			err := tr.Transition(id, tc.to)
			if !errors.Is(err, ErrInvalidTransition) {
				t.Fatalf("Transition(%v->%v) = %v, want ErrInvalidTransition", tc.from, tc.to, err)
			}
		})
	}
}

func TestUnknownID(t *testing.T) {
	t.Parallel()
	tr := NewTracker()
	err := tr.Transition(GoroutineID(999), Running)
	if !errors.Is(err, ErrUnknownID) {
		t.Fatalf("expected ErrUnknownID, got %v", err)
	}
}

func TestCount(t *testing.T) {
	t.Parallel()
	tr := NewTracker()
	a := tr.Create()
	b := tr.Create()
	c := tr.Create()
	if tr.Count(Created) != 3 {
		t.Fatalf("Count(Created) = %d, want 3", tr.Count(Created))
	}
	_ = tr.Transition(a, Running)
	_ = tr.Transition(b, Running)
	if tr.Count(Running) != 2 {
		t.Fatalf("Count(Running) = %d, want 2", tr.Count(Running))
	}
	_ = tr.Transition(a, Parked)
	_ = tr.Transition(b, Done)
	_ = tr.Transition(c, Running)
	if tr.Count(Parked) != 1 || tr.Count(Done) != 1 || tr.Count(Running) != 1 {
		t.Fatalf("unexpected counts: parked=%d done=%d running=%d",
			tr.Count(Parked), tr.Count(Done), tr.Count(Running))
	}
}

func TestAllLen(t *testing.T) {
	t.Parallel()
	tr := NewTracker()
	for i := 0; i < 5; i++ {
		tr.Create()
	}
	if len(tr.All()) != 5 {
		t.Fatalf("All() len = %d, want 5", len(tr.All()))
	}
}
```

Create `example_test.go`:

```go
// example_test.go
package gmpstate_test

import (
	"fmt"

	"example.com/gmpstate"
)

func ExampleNewTracker() {
	tr := gmpstate.NewTracker()
	id := tr.Create()
	fmt.Println(tr.Count(gmpstate.Created))
	_ = tr.Transition(id, gmpstate.Running)
	fmt.Println(tr.Count(gmpstate.Running))
	_ = tr.Transition(id, gmpstate.Done)
	fmt.Println(tr.Count(gmpstate.Done))
	// Output:
	// 1
	// 1
	// 1
}
```

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"sync"

	"example.com/gmpstate"
)

func main() {
	tr := gmpstate.NewTracker()
	const workers = 5

	ids := make([]gmpstate.GoroutineID, workers)
	for i := 0; i < workers; i++ {
		ids[i] = tr.Create()
	}
	fmt.Printf("created: %d\n", tr.Count(gmpstate.Created))

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		id := ids[i]
		go func() {
			defer wg.Done()
			_ = tr.Transition(id, gmpstate.Running)
			sum := 0
			for j := 0; j < 1_000_000; j++ {
				sum += j
			}
			_ = sum
			_ = tr.Transition(id, gmpstate.Done)
		}()
	}

	wg.Wait()
	fmt.Printf("done: %d\n", tr.Count(gmpstate.Done))
}
```

## Common Mistakes

Wrong: assuming goroutines can jump from `parked` directly to `done` without
going back through `running`. In the real scheduler a goroutine must be
scheduled (running) before it can exit.

What happens: the tracker returns `ErrInvalidTransition` with a message showing
the illegal pair. The test that calls `errors.Is(err, ErrInvalidTransition)`
fails loudly.

Fix: always go `parked -> running -> done`.

---

Wrong: forgetting to call `Create()` before `Transition()`. If you call
`Transition` with an ID you fabricated, the tracker returns `ErrUnknownID`.

What happens: `errors.Is(err, ErrUnknownID)` is true; the caller gets a clear
error rather than a silent no-op.

Fix: obtain IDs exclusively from `Create()`.

---

Wrong: sharing a tracker across parallel tests without creating a new instance
per subtest. Because `Tracker` is mutex-protected it is safe for concurrent
use, but test state leaks between subtests if each subtest reuses the same tracker.

Fix: create a fresh `NewTracker()` per subtest as shown in the test file.

## Verification

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

## Summary

- G (goroutine): lightweight (~2-8 KB stack), user-space, moves between P's freely.
- M (machine): OS thread; count can exceed GOMAXPROCS during blocking syscalls.
- P (processor): logical CPU slot; count = GOMAXPROCS; owns a local run queue.
- G lifecycle: created -> running <-> parked, running -> done.
- Goroutine leaks show up as a permanently elevated `runtime.NumGoroutine()`.
- Go 1.14 async preemption (SIGURG) prevents CPU-bound goroutines from starving others.

## What's Next

[GOMAXPROCS and Processor Binding](../02-gomaxprocs-processor-binding/02-gomaxprocs-processor-binding.md)

## Resources

- [Go scheduler design doc (Dmitry Vyukov)](https://docs.google.com/document/d/1TTj4T2JO42uD5ID9e89oa0sLKhJYD0Y_kqxDv3I3XMw)
- [runtime package](https://pkg.go.dev/runtime)
- [Scheduling In Go (Ardan Labs)](https://www.ardanlabs.com/blog/2018/08/scheduling-in-go-part1.html)
- [Go runtime source: proc.go](https://github.com/golang/go/blob/master/src/runtime/proc.go)
