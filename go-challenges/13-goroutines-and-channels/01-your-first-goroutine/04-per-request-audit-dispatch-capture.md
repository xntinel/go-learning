# Exercise 4: Dispatch One Audit Event Per Request Without Corrupting the Payload

An async audit sink is a place where the loop-variable capture question gets
real. You launch one goroutine per event to hand it to a sink, and if the
goroutines observe a mutated shared variable instead of their own event, the
audit trail is corrupted — duplicated IDs, dropped records — in a way that only
shows up under load. This exercise builds a `DispatchAll` that passes each event
by value so no two goroutines ever see the same mutated variable.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
audit/                       independent module: example.com/audit
  go.mod                     go 1.25 (WaitGroup.Go)
  audit.go                   AuditEvent; DispatchAll([]AuditEvent, sink func(AuditEvent))
  cmd/
    demo/
      main.go                dispatch a few audit events to a counting sink
  audit_test.go              received multiset == input multiset, under -race
```

Files: `audit.go`, `cmd/demo/main.go`, `audit_test.go`.
Implement: `DispatchAll(events []AuditEvent, sink func(AuditEvent))` that launches one goroutine per event and passes each event as an explicit closure argument.
Test: a sink appends received events under a `sync.Mutex`; assert the multiset of received IDs exactly matches the input (no aliasing duplicates, none dropped); empty and single-element slices as edge cases.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/audit/cmd/demo
cd ~/go-exercises/audit
go mod init example.com/audit
go mod edit -go=1.25
```

### Loop-var semantics AND argument discipline

Since Go 1.22, `for _, e := range events { go func() { sink(e) }() }` is safe: the
loop variable `e` is per-iteration, so each goroutine captures its own `e`. That
is a genuine improvement — the pre-1.22 version of this exact code was a classic
audit-corruption bug where every goroutine dispatched the *last* event.

But the discipline that survives the language change is passing the value as an
explicit argument. Two reasons. First, it is version-independent: it is correct
whether the reader is on Go 1.21 or Go 1.25, and it does not require the reader to
remember which release changed the semantics. Second, it is robust to refactoring:
the moment someone hoists a variable *out* of the loop — a running accumulator, a
reused buffer — the closure-capture version silently starts sharing it across
goroutines and corrupts the data, whereas the argument-passing version forces a
conscious choice. Here `DispatchAll` uses `wg.Go(func() { sink(e) })` where `e` is
the range copy; to make the by-value guarantee explicit and refactor-proof, the
sink receives an `AuditEvent` value, so even if the caller later mutates its own
slice, the dispatched copy is frozen.

The sink is called concurrently from many goroutines, so it must be safe for
concurrent use. The test's sink guards its slice with a `sync.Mutex`. The
correctness property is a *multiset* equality: the set of received event IDs, with
multiplicity, must exactly equal the input — no duplicates introduced by aliasing,
none dropped. Order is not guaranteed and the test must not assume it.

Create `audit.go`:

```go
package audit

import "sync"

// AuditEvent is a single audit record dispatched to an async sink.
type AuditEvent struct {
	ID     string
	Action string
	Actor  string
}

// DispatchAll launches one goroutine per event and hands each event to sink by
// value, so no two goroutines ever observe the same mutated variable, and
// returns after every dispatch has completed. sink must be safe for concurrent
// use.
func DispatchAll(events []AuditEvent, sink func(AuditEvent)) {
	var wg sync.WaitGroup
	for _, e := range events {
		wg.Go(func() {
			sink(e) // e is the per-iteration copy; passed by value into sink
		})
	}
	wg.Wait()
}
```

### The runnable demo

The demo dispatches three events to a sink that records them under a mutex, then
prints the count and the sorted IDs so the output is deterministic despite the
concurrent dispatch.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sort"
	"sync"

	"example.com/audit"
)

func main() {
	events := []audit.AuditEvent{
		{ID: "e1", Action: "login", Actor: "alice"},
		{ID: "e2", Action: "delete", Actor: "bob"},
		{ID: "e3", Action: "update", Actor: "carol"},
	}

	var mu sync.Mutex
	var got []string
	audit.DispatchAll(events, func(e audit.AuditEvent) {
		mu.Lock()
		got = append(got, e.ID)
		mu.Unlock()
	})

	sort.Strings(got)
	fmt.Printf("dispatched %d events: %v\n", len(got), got)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
dispatched 3 events: [e1 e2 e3]
```

### Tests

`TestDispatchAllDeliversEveryEventExactlyOnce` is the multiset test: it dispatches
N events with distinct IDs, collects the received IDs under a mutex, and asserts a
count map where every input ID maps to exactly one delivery. `TestDispatchAllEmpty`
and `TestDispatchAllSingle` pin the edge cases. Running under `-race` proves the
sink's mutex actually guards the shared slice.

Create `audit_test.go`:

```go
package audit

import (
	"fmt"
	"sync"
	"testing"
)

// countingSink returns a concurrency-safe sink plus an accessor for the
// per-ID delivery counts observed so far.
func countingSink() (func(AuditEvent), func() map[string]int) {
	var mu sync.Mutex
	counts := make(map[string]int)
	sink := func(e AuditEvent) {
		mu.Lock()
		counts[e.ID]++
		mu.Unlock()
	}
	snapshot := func() map[string]int {
		mu.Lock()
		defer mu.Unlock()
		out := make(map[string]int, len(counts))
		for k, v := range counts {
			out[k] = v
		}
		return out
	}
	return sink, snapshot
}

func TestDispatchAllDeliversEveryEventExactlyOnce(t *testing.T) {
	t.Parallel()

	const n = 500
	events := make([]AuditEvent, n)
	for i := range events {
		events[i] = AuditEvent{ID: fmt.Sprintf("e%d", i), Action: "x", Actor: "y"}
	}

	sink, snapshot := countingSink()
	DispatchAll(events, sink)

	counts := snapshot()
	if len(counts) != n {
		t.Fatalf("distinct IDs delivered = %d, want %d", len(counts), n)
	}
	for i := range n {
		id := fmt.Sprintf("e%d", i)
		if counts[id] != 1 {
			t.Fatalf("ID %s delivered %d times, want 1", id, counts[id])
		}
	}
}

func TestDispatchAllEmpty(t *testing.T) {
	t.Parallel()

	sink, snapshot := countingSink()
	DispatchAll(nil, sink)
	if got := len(snapshot()); got != 0 {
		t.Fatalf("delivered %d events for empty input, want 0", got)
	}
}

func TestDispatchAllSingle(t *testing.T) {
	t.Parallel()

	sink, snapshot := countingSink()
	DispatchAll([]AuditEvent{{ID: "only"}}, sink)
	counts := snapshot()
	if counts["only"] != 1 {
		t.Fatalf("single event delivered %d times, want 1", counts["only"])
	}
}
```

## Review

The dispatch is correct when the multiset of delivered IDs equals the input:
every event delivered exactly once, none duplicated by aliasing, none dropped.
The subtle failure this guards against is not visible on Go 1.22+ from the loop
variable alone — that bug is fixed — but from any shared variable a refactor might
introduce; passing the event by value keeps the guarantee explicit. The sink is
called from many goroutines at once, so it must be concurrency-safe; the tests use
a mutex-guarded map and run under `-race` to prove it. Never assert on delivery
order — the goroutines finish in whatever order the scheduler picks.

## Resources

- [Go 1.22 loop variable scoping change](https://go.dev/blog/loopvar-preview)
- [sync.WaitGroup.Go](https://pkg.go.dev/sync#WaitGroup.Go)
- [sync.Mutex](https://pkg.go.dev/sync#Mutex)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-waitgroup-go-modern-idiom.md](03-waitgroup-go-modern-idiom.md) | Next: [05-shutdown-flush-fire-and-forget-trap.md](05-shutdown-flush-fire-and-forget-trap.md)
