# Exercise 32: Dead Letter Queue Handler via Goroutine Literal with Per-Item Argument

**Nivel: Intermedio** — validacion rapida (un test corto, incluye concurrencia).

Reprocessing dead-letter items concurrently, one goroutine literal per
item, is only safe if every literal's inputs are frozen to exactly the item
it was launched for. This module builds `ProcessAll`, which passes each
item's index and value into its goroutine literal as explicit arguments —
`i`, `item` — rather than leaving the literal to read the enclosing loop's
variables, and has every literal write only its own slot in the results
slice.

This module is fully self-contained. Nothing here imports another
exercise.

## What you'll build

```text
dlq/                           module example.com/dlq
  go.mod
  dlq.go                        Item, Result, ProcessAll (per-item argument literal)
  dlq_test.go                    order preserved, per-item failure isolation, race-free
  cmd/demo/main.go               three items, one poison payload
```

- Files: `dlq.go`, `dlq_test.go`, `cmd/demo/main.go`.
- Implement: `ProcessAll(items, handle)` launching `go func(i int, item Item) { ... }(i, item)` per item, each writing only `results[i]`.
- Test: results come back in item order; one failing item does not affect the others' results; 50 concurrent items pass under `-race` with call counts exactly matching item count.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/05-anonymous-functions/32-dlq-handler-literal/cmd/demo
cd go-solutions/04-functions/05-anonymous-functions/32-dlq-handler-literal
go mod edit -go=1.24
```

### Passing i and item as arguments freezes what each literal sees

`ProcessAll` launches `go func(i int, item Item) { ... }(i, item)` inside
its `range` loop. Since Go 1.22 every loop iteration already gets its own
fresh `i` and `item`, so a bare closure capture of the range variables
would in fact be safe too — but writing them as explicit parameters,
evaluated and copied at the `go` statement itself, makes each goroutine's
inputs visibly, unambiguously frozen to exactly that iteration's values,
independent of whichever Go version compiles it or how the loop is later
refactored. Inside, the literal calls `handle(item)` and writes the
result to `results[i]` — its own index, and only its own index — so
however many of these run concurrently, no two goroutines ever read or
write the same slice element. `wg.Wait()` blocks until every literal has
finished before `ProcessAll` returns the completed slice.

Create `dlq.go`:

```go
package dlq

import "sync"

// Item is one message pulled off a dead-letter queue for reprocessing.
type Item struct {
	ID      string
	Payload string
}

// Result is the outcome of reprocessing one Item.
type Result struct {
	ID  string
	Err error
}

// ProcessAll reprocesses every item concurrently, one goroutine literal
// per item, and returns once all of them finish. Each literal receives the
// item's index and its own Item copy as explicit arguments -- i, item --
// rather than reaching out to the loop variables from the enclosing
// scope. That keeps every goroutine's inputs frozen to exactly the item it
// was launched for, and every goroutine writes only results[i], its own
// slot in the shared slice, so no two goroutines ever touch the same
// memory and the slice needs no lock.
func ProcessAll(items []Item, handle func(Item) error) []Result {
	results := make([]Result, len(items))
	var wg sync.WaitGroup
	for i, item := range items {
		wg.Add(1)
		go func(i int, item Item) {
			defer wg.Done()
			results[i] = Result{ID: item.ID, Err: handle(item)}
		}(i, item)
	}
	wg.Wait()
	return results
}
```

### The runnable demo

The demo reprocesses three items where the middle one is poison.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/dlq"
)

func main() {
	items := []dlq.Item{
		{ID: "msg-1", Payload: "ok"},
		{ID: "msg-2", Payload: "boom"},
		{ID: "msg-3", Payload: "ok"},
	}

	results := dlq.ProcessAll(items, func(item dlq.Item) error {
		if item.Payload == "boom" {
			return errors.New("poison payload")
		}
		return nil
	})

	// Results are index-partitioned, so they already come back in the same
	// order as items -- no sort needed for deterministic output.
	for _, r := range results {
		fmt.Printf("%s: err=%v\n", r.ID, r.Err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
msg-1: err=<nil>
msg-2: err=poison payload
msg-3: err=<nil>
```

### Tests

`TestProcessAllReturnsResultsInItemOrder` checks a batch of successes
comes back in exactly the input order. `TestProcessAllIsolatesFailuresPerItem`
mixes a failing item between two successes and checks only the failing
one's result carries the error. `TestProcessAllRunsConcurrentlyWithoutDataRaces`
runs 50 items under `-race`, counting calls with an atomic counter and
checking every result still lines up with its item.

Create `dlq_test.go`:

```go
package dlq

import (
	"errors"
	"sync/atomic"
	"testing"
)

func TestProcessAllReturnsResultsInItemOrder(t *testing.T) {
	t.Parallel()
	items := []Item{
		{ID: "a", Payload: "1"},
		{ID: "b", Payload: "2"},
		{ID: "c", Payload: "3"},
	}
	results := ProcessAll(items, func(Item) error { return nil })

	if len(results) != len(items) {
		t.Fatalf("len(results) = %d, want %d", len(results), len(items))
	}
	for i, r := range results {
		if r.ID != items[i].ID {
			t.Errorf("results[%d].ID = %q, want %q (index partitioning must preserve order)", i, r.ID, items[i].ID)
		}
		if r.Err != nil {
			t.Errorf("results[%d].Err = %v, want nil", i, r.Err)
		}
	}
}

func TestProcessAllIsolatesFailuresPerItem(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("poison payload")
	items := []Item{
		{ID: "ok-1", Payload: "fine"},
		{ID: "bad", Payload: "boom"},
		{ID: "ok-2", Payload: "fine"},
	}

	results := ProcessAll(items, func(item Item) error {
		if item.Payload == "boom" {
			return sentinel
		}
		return nil
	})

	want := map[string]error{"ok-1": nil, "bad": sentinel, "ok-2": nil}
	for _, r := range results {
		if !errors.Is(r.Err, want[r.ID]) {
			t.Errorf("results[%s].Err = %v, want %v", r.ID, r.Err, want[r.ID])
		}
	}
}

func TestProcessAllRunsConcurrentlyWithoutDataRaces(t *testing.T) {
	items := make([]Item, 50)
	for i := range items {
		items[i] = Item{ID: string(rune('a' + i%26)), Payload: "x"}
	}
	var calls atomic.Int64

	results := ProcessAll(items, func(item Item) error {
		calls.Add(1)
		return nil
	})

	if got := calls.Load(); got != int64(len(items)) {
		t.Fatalf("handle was called %d times, want %d", got, len(items))
	}
	for i, r := range results {
		if r.ID != items[i].ID {
			t.Fatalf("results[%d].ID = %q, want %q", i, r.ID, items[i].ID)
		}
	}
}
```

## Review

`ProcessAll` is correct when every result lines up with the item that
produced it and one item's failure never contaminates another's, verified
under `-race` across 50 concurrent goroutines. The explicit-argument style
here is a defensive habit worth keeping even on modern Go: it makes the
freeze-per-iteration guarantee visible in the goroutine literal's own
signature instead of resting entirely on which Go version compiled the
loop. The actual correctness property, though, is the index partition on
`results[i]` — that is what a shared, unpartitioned write (say, appending
to a single results slice instead of indexing into it) would break, `-race`
or no `-race`, the moment two literals happened to append at the same
instant.

## Resources

- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup)
- [Go Language Specification: Go statements](https://go.dev/ref/spec#Go_statements)
- [Go wiki: Common Mistakes (loop variables)](https://go.dev/wiki/CommonMistakes)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [31-permission-closure-factory.md](31-permission-closure-factory.md) | Next: [33-connection-state-deferred.md](33-connection-state-deferred.md)
