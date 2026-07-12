# Exercise 9: Fan-Out Publisher — Loop-Variable Capture vs Shared-State Capture

Go 1.22 fixed the classic loop-variable bug: each iteration now gets a fresh copy
of the loop variable, so a goroutine literal that captures it is safe. Engineers
often over-read that fix as "goroutine closures are safe now" — but capturing a
*shared* mutable accumulator (a slice you append to, a map you write, a counter)
from many goroutines is still a data race the compiler cannot fix. This module
builds an event publisher that gets it right and shows precisely where the 1.22 fix
stops helping.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
publisher/                    module example.com/publisher (go 1.22+ semantics)
  go.mod
  publisher.go                PublishAll (mutex) and PublishAllChan (channel) fan-out
  publisher_test.go           count + set-equality, both variants, under -race
  cmd/demo/main.go            publish five events concurrently
```

- Files: `publisher.go`, `publisher_test.go`, `cmd/demo/main.go`.
- Implement: `PublishAll` and `PublishAllChan`, each launching one goroutine literal per event, passing the event as an argument, and funneling results through a mutex-guarded slice or a channel.
- Test: every event is published exactly once regardless of order (count and set-equality); both variants pass under `-race`.
- Verify: `go test -count=1 -race ./...`

### What Go 1.22 fixed, and what it did not

Here is the anti-pattern, kept illustrative (not compiled) because it is a data
race:

```go
// ANTI-PATTERN: a data race the detector will flag. Do NOT ship this.
func publishBuggy(events []Event) []Event {
	var published []Event
	var wg sync.WaitGroup
	for _, e := range events { // Go 1.22: e is per-iteration, so capturing e is fine
		wg.Add(1)
		go func() {
			defer wg.Done()
			published = append(published, e) // RACE: many goroutines mutate one slice
		}()
	}
	wg.Wait()
	return published
}
```

Before Go 1.22, capturing `e` was the bug — every goroutine saw the final `e`. That
part is now fixed: the module's `go` directive (`go 1.22` or later — the gate uses
`go 1.26`) gives each iteration a fresh `e`, so the bare capture is correct. But the
`append(published, ...)` is a *different* bug that the fix does nothing for: `append`
reads the slice header, may reallocate, and writes it back, and doing that from many
goroutines concurrently races on the shared `published` variable. The race detector
flags it; the 1.22 change never touches it.

The correct version passes the event as an argument (`go func(ev Event){...}(e)`,
the clearest way to hand per-item data to a goroutine) and funnels results through
synchronization. `PublishAll` guards the shared slice with a `sync.Mutex`;
`PublishAllChan` sends each event on a buffered channel and collects them in a
single goroutine, so only one goroutine ever appends. Both publish every event
exactly once; neither races.

Create `publisher.go`:

```go
package publisher

import "sync"

// Event is a message to publish.
type Event struct {
	ID    int
	Topic string
}

// PublishAll fans out one goroutine per event, passing the event as an argument,
// and collects results into a mutex-guarded slice. Every event is published once.
func PublishAll(events []Event) []Event {
	var mu sync.Mutex
	published := make([]Event, 0, len(events))
	var wg sync.WaitGroup
	for _, e := range events {
		wg.Add(1)
		go func(ev Event) {
			defer wg.Done()
			// deliver ev to the broker here...
			mu.Lock()
			published = append(published, ev)
			mu.Unlock()
		}(e)
	}
	wg.Wait()
	return published
}

// PublishAllChan is the channel variant: each goroutine sends its event, and a
// single collector appends, so no shared slice is mutated concurrently.
func PublishAllChan(events []Event) []Event {
	out := make(chan Event, len(events))
	var wg sync.WaitGroup
	for _, e := range events {
		wg.Add(1)
		go func(ev Event) {
			defer wg.Done()
			out <- ev
		}(e)
	}
	go func() {
		wg.Wait()
		close(out)
	}()

	published := make([]Event, 0, len(events))
	for ev := range out {
		published = append(published, ev)
	}
	return published
}
```

### The runnable demo

The demo publishes five events concurrently and prints the count and the sorted
ids, so the output is deterministic even though publish order is not.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sort"

	"example.com/publisher"
)

func main() {
	events := make([]publisher.Event, 5)
	for i := range events {
		events[i] = publisher.Event{ID: i, Topic: fmt.Sprintf("topic-%d", i)}
	}

	got := publisher.PublishAll(events)
	ids := make([]int, 0, len(got))
	for _, e := range got {
		ids = append(ids, e.ID)
	}
	sort.Ints(ids)

	fmt.Println("published count:", len(got))
	fmt.Println("ids:", ids)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
published count: 5
ids: [0 1 2 3 4]
```

### Tests

The tests assert the only two things that are order-independent: the count and the
set of ids. `TestPublishAllExactlyOnce` and `TestPublishAllChanExactlyOnce` run both
variants and confirm every event appears exactly once. Running the suite under
`-race` is what certifies the synchronization — a shared-append version would fail
here.

Create `publisher_test.go`:

```go
package publisher

import (
	"fmt"
	"sort"
	"testing"
)

func ids(events []Event) []int {
	out := make([]int, 0, len(events))
	for _, e := range events {
		out = append(out, e.ID)
	}
	sort.Ints(out)
	return out
}

func sampleEvents(n int) []Event {
	events := make([]Event, n)
	for i := range events {
		events[i] = Event{ID: i, Topic: fmt.Sprintf("t-%d", i)}
	}
	return events
}

func assertExactlyOnce(t *testing.T, in, out []Event) {
	t.Helper()
	if len(out) != len(in) {
		t.Fatalf("published %d events, want %d", len(out), len(in))
	}
	seen := make(map[int]int, len(out))
	for _, e := range out {
		seen[e.ID]++
	}
	for _, e := range in {
		if seen[e.ID] != 1 {
			t.Fatalf("event %d published %d times, want exactly 1", e.ID, seen[e.ID])
		}
	}
}

func TestPublishAllExactlyOnce(t *testing.T) {
	t.Parallel()
	in := sampleEvents(200)
	assertExactlyOnce(t, in, PublishAll(in))
}

func TestPublishAllChanExactlyOnce(t *testing.T) {
	t.Parallel()
	in := sampleEvents(200)
	assertExactlyOnce(t, in, PublishAllChan(in))
}

func TestVariantsAgree(t *testing.T) {
	t.Parallel()
	in := sampleEvents(50)
	if a, b := ids(PublishAll(in)), ids(PublishAllChan(in)); !sliceEqual(a, b) {
		t.Fatalf("variants disagree: %v vs %v", a, b)
	}
}

func sliceEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func ExamplePublishAll() {
	got := PublishAll([]Event{{ID: 1}, {ID: 2}, {ID: 3}})
	fmt.Println(len(got), ids(got))
	// Output: 3 [1 2 3]
}
```

## Review

The publisher is correct when every event is published exactly once and the suite
is clean under `-race`. The teaching point is the boundary the concepts drew: the
Go 1.22 loop-variable fix makes the *capture* of `e` safe, and passing `e` as an
argument makes intent explicit, but neither addresses the *shared accumulator*. The
`append` to a shared slice from many goroutines is a race no language change fixes;
the mutex in `PublishAll` and the single-collector channel in `PublishAllChan` are
the actual fixes. The anti-pattern is kept as an illustrative (uncompiled) snippet
precisely because assembling it would fail the race detector — which is exactly the
lesson.

## Resources

- [Go blog: Fixing for loops in Go 1.22](https://go.dev/blog/loopvar-preview)
- [Go Memory Model](https://go.dev/ref/mem)
- [sync.Mutex](https://pkg.go.dev/sync#Mutex)
- [Data Race Detector](https://go.dev/doc/articles/race_detector)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-context-afterfunc-cleanup-hook.md](08-context-afterfunc-cleanup-hook.md) | Next: [10-sort-ranking-anonymous-comparator.md](10-sort-ranking-anonymous-comparator.md)
