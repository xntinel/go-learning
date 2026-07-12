# Exercise 19: Broadcast Decorator That Fans Output to Multiple Subscribers

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

An order-placed event usually needs to reach several independent
consumers at once — a log line, a metrics counter, a webhook — none of
which should know the others exist. `Tee` composes any number of
single-argument `Subscriber` functions into one callable that fans the
same value out to all of them, either on the caller's own goroutine or
concurrently with a join at the end.

## What you'll build

```text
broadcast/                   independent module: example.com/broadcast
  go.mod                     go 1.24
  broadcast.go               type Subscriber[T], Tee[T]; func Sequential, Concurrent
  broadcast_test.go          order, zero-subscriber edge, exactly-once delivery, blocking join
  cmd/demo/
    main.go                  broadcasts one event sequentially and concurrently
```

- Files: `broadcast.go`, `broadcast_test.go`, `cmd/demo/main.go`.
- Implement: `Subscriber[T any] func(T)`, `Tee[T any] func(T)`, `Sequential[T any](subs ...Subscriber[T]) Tee[T]`, and `Concurrent[T any](subs ...Subscriber[T]) Tee[T]`.
- Test: `Sequential` invokes every subscriber in the order given on one goroutine; `Concurrent` invokes every subscriber exactly once and does not return until all of them have finished; a zero-subscriber `Tee` is a safe no-op.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Fan-out, then join — the same shape either way

Both `Sequential` and `Concurrent` have the exact same signature —
`func(subs ...Subscriber[T]) Tee[T]` — because they are two strategies for
the same contract: "every subscriber sees this value." `Sequential` is the
simplest possible correct implementation: a `for` loop calling each
subscriber in turn on whatever goroutine called the `Tee`. It is correct
by construction and useful when subscribers must run in a fixed order (a
validation subscriber before a side-effecting one) or when spawning
goroutines for cheap subscribers would just add overhead.

`Concurrent` exists for the opposite case: independent, possibly slow
subscribers (a webhook call over the network) that should not block each
other. It launches one goroutine per subscriber and uses a `sync.WaitGroup`
to fan-out then join: `Add(len(subs))` before starting any goroutine, one
`defer wg.Done()` per goroutine, and `wg.Wait()` before the `Tee` call
returns. That `Wait()` is what keeps `Concurrent`'s contract identical to
`Sequential`'s from the caller's point of view — both block until every
subscriber has run — even though the internal execution is parallel.

Both constructors take a defensive copy of the `subs` slice. A variadic
call passes its arguments as a freshly-allocated slice when called with
individual arguments, but a caller that builds a `[]Subscriber[T]` and
passes it with `...` hands over the *same* backing array; if that caller
later mutated the slice, a `Tee` built without copying it could silently
start invoking different subscribers than it was built with.

Create `broadcast.go`:

```go
package broadcast

import "sync"

// Subscriber processes one broadcast value — a log line, a metric, a
// webhook call — and has no return value: a subscriber's failure must not
// stop the others, so it must handle its own errors internally.
type Subscriber[T any] func(T)

// Tee fans one value out to every subscriber it was built from.
type Tee[T any] func(T)

// Sequential composes subs into a Tee that invokes every subscriber, in
// the order given, on the caller's own goroutine. It returns only after
// the last subscriber returns, so a slow subscriber delays every one
// after it.
func Sequential[T any](subs ...Subscriber[T]) Tee[T] {
	subs = append([]Subscriber[T](nil), subs...) // defensive copy: callers must not mutate our slice later
	return func(v T) {
		for _, s := range subs {
			s(v)
		}
	}
}

// Concurrent composes subs into a Tee that runs every subscriber in its
// own goroutine and blocks until all of them finish — fan-out, then join —
// so one slow subscriber cannot delay the others, but the Tee call itself
// still does not return until every subscriber has seen the value.
func Concurrent[T any](subs ...Subscriber[T]) Tee[T] {
	subs = append([]Subscriber[T](nil), subs...)
	return func(v T) {
		var wg sync.WaitGroup
		wg.Add(len(subs))
		for _, s := range subs {
			go func() {
				defer wg.Done()
				s(v)
			}()
		}
		wg.Wait()
	}
}
```

### The runnable demo

The sequential broadcast prints in a fixed, guaranteed order. The
concurrent broadcast's three goroutines could finish in any order, so
each subscriber records its line into a mutex-protected slice instead of
printing directly, and the demo sorts before printing so the output is
identical on every run.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sort"
	"sync"

	"example.com/broadcast"
)

func main() {
	logLine := func(v string) { fmt.Println("log:", v) }
	metric := func(v string) { fmt.Println("metric: order_placed", v) }
	webhook := func(v string) { fmt.Println("webhook: notified for", v) }

	seq := broadcast.Sequential(logLine, metric, webhook)
	fmt.Println("--- sequential ---")
	seq("order-42")

	// Concurrent subscribers may finish in any order, so each one records
	// its output instead of printing directly; the demo sorts before
	// printing so the output is deterministic.
	var mu sync.Mutex
	var lines []string
	record := func(name string) broadcast.Subscriber[string] {
		return func(v string) {
			mu.Lock()
			lines = append(lines, fmt.Sprintf("%s: %s", name, v))
			mu.Unlock()
		}
	}

	con := broadcast.Concurrent(record("log"), record("metric"), record("webhook"))
	fmt.Println("--- concurrent (sorted for deterministic output) ---")
	con("order-42")

	sort.Strings(lines)
	for _, l := range lines {
		fmt.Println(l)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
--- sequential ---
log: order-42
metric: order_placed order-42
webhook: notified for order-42
--- concurrent (sorted for deterministic output) ---
log: order-42
metric: order-42
webhook: order-42
```

The sequential section's order is guaranteed by the loop; the concurrent
section's lines are sorted before printing so the demo's output does not
depend on which goroutine the scheduler happened to run first.

### Tests

`TestSequentialInvokesInOrder` proves the ordering guarantee by having
each subscriber append its own tag. `TestSequentialInvokesEverySubscriberEvenAfterEmpty`
covers the zero-subscriber edge — a `Tee` built from no subscribers must
not panic. `TestConcurrentInvokesEverySubscriberExactlyOnce` fires five
subscribers concurrently under `-race` and asserts, after sorting since
order is not guaranteed, that all five ran exactly once.
`TestConcurrentBlocksUntilAllSubscribersFinish` proves the join: it reads
a shared counter immediately after the `Tee` call returns and requires it
to already equal the full subscriber count, which is only true if
`Concurrent` truly waits for every goroutine.

Create `broadcast_test.go`:

```go
package broadcast

import (
	"fmt"
	"sort"
	"sync"
	"testing"
)

func TestSequentialInvokesInOrder(t *testing.T) {
	t.Parallel()

	var order []string
	a := func(v string) { order = append(order, "a:"+v) }
	b := func(v string) { order = append(order, "b:"+v) }
	c := func(v string) { order = append(order, "c:"+v) }

	tee := Sequential(a, b, c)
	tee("x")

	want := []string{"a:x", "b:x", "c:x"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestSequentialInvokesEverySubscriberEvenAfterEmpty(t *testing.T) {
	t.Parallel()

	tee := Sequential[int]() // zero subscribers
	tee(1)                   // must not panic
}

func TestConcurrentInvokesEverySubscriberExactlyOnce(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var got []string

	subs := make([]Subscriber[int], 5)
	for i := range subs {
		subs[i] = func(v int) {
			mu.Lock()
			got = append(got, indexedLabel(i, v))
			mu.Unlock()
		}
	}

	tee := Concurrent(subs...)
	tee(7)

	if len(got) != 5 {
		t.Fatalf("got %d invocations, want 5", len(got))
	}

	sort.Strings(got)
	want := []string{
		indexedLabel(0, 7),
		indexedLabel(1, 7),
		indexedLabel(2, 7),
		indexedLabel(3, 7),
		indexedLabel(4, 7),
	}
	sort.Strings(want)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got = %v, want (any order) %v", got, want)
		}
	}
}

func TestConcurrentBlocksUntilAllSubscribersFinish(t *testing.T) {
	t.Parallel()

	const n = 20
	var mu sync.Mutex
	completed := 0

	subs := make([]Subscriber[int], n)
	for i := range subs {
		subs[i] = func(int) {
			mu.Lock()
			completed++
			mu.Unlock()
		}
	}

	tee := Concurrent(subs...)
	tee(1)

	mu.Lock()
	defer mu.Unlock()
	if completed != n {
		t.Fatalf("completed = %d, want %d: Concurrent returned before every subscriber finished", completed, n)
	}
}

func indexedLabel(i, v int) string {
	return fmt.Sprintf("sub%d:%d", i, v)
}
```

## Review

`Sequential` and `Concurrent` are correct when both honor the same
contract from the caller's side — every subscriber sees the value, and the
`Tee` call does not return until they all have — even though one runs on a
single goroutine and the other fans out across many. The `sync.WaitGroup`
is what makes `Concurrent` uphold that contract: without `wg.Wait()`, the
function would return as soon as the last goroutine was merely *launched*,
not finished, and `TestConcurrentBlocksUntilAllSubscribersFinish` is built
specifically to catch that regression. The defensive copy of `subs` at
construction time protects a `Tee` from a caller who reuses and later
mutates the slice it was built from. Run `go test -race`, since `Concurrent`
has multiple goroutines writing to whatever shared state the subscribers
themselves close over.

## Resources

- [sync package](https://pkg.go.dev/sync) — `WaitGroup`, the fan-out-then-join primitive this exercise is built on.
- [Go Concurrency Patterns: Fan-out, fan-in (Go Blog)](https://go.dev/blog/pipelines) — the broader pattern `Concurrent` is a small instance of.
- [RxJS: multicast / Subject](https://rxjs.dev/api/index/class/Subject) — the same one-value-to-many-subscribers idea from another ecosystem's reactive streams library.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [18-probability-sampler-for-observability.md](18-probability-sampler-for-observability.md) | Next: [20-filter-and-map-with-transform-error.md](20-filter-and-map-with-transform-error.md)
