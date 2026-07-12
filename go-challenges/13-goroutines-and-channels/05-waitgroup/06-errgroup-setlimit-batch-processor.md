# Exercise 6: Bounded Bulk Processor With errgroup.SetLimit and Error Aggregation

Bulk jobs — uploading a batch of objects, transcoding a set of files, reindexing
records — need two things at once: bounded concurrency (do not open ten thousand
connections) and a *complete* error report (which items failed, not just the first).
This is collect-all with a cap. `errgroup.Group.SetLimit(n)` bounds the concurrency
without a hand-rolled semaphore, and `errors.Join` aggregates every per-item failure
into one error you can inspect. This module builds that processor.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
bulk/                      independent module: example.com/bulk
  go.mod                   go 1.25; requires golang.org/x/sync
  bulk.go                  Process bounds concurrency, joins all item errors
  cmd/
    demo/
      main.go              runnable demo: 6 items, limit 2, 2 failures reported
  bulk_test.go             peak<=limit; all errors surfaced; TryGo saturation
```

- Files: `bulk.go`, `cmd/demo/main.go`, `bulk_test.go`.
- Implement: `Process(items []Item, limit int, handle Handler) error` — `SetLimit(limit)`, each `g.Go` collects its item error under a mutex and returns nil (no fail-fast), aggregate with `errors.Join`.
- Test: track peak concurrency with an atomic and assert `peak <= limit`; feed mixed pass/fail items and assert `errors.Join` surfaces every failure; a `TryGo` subtest asserting it returns false when the limit is saturated.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/05-waitgroup/06-errgroup-setlimit-batch-processor/cmd/demo
cd go-solutions/13-goroutines-and-channels/05-waitgroup/06-errgroup-setlimit-batch-processor
go mod edit -go=1.25
go get golang.org/x/sync/errgroup
```

### SetLimit bounds concurrency; we collect errors ourselves

`SetLimit(n)` caps the number of goroutines the group runs concurrently. After it is
set, `g.Go` *blocks* when `n` goroutines are already active, so the launch loop feels
backpressure — the same effect as the buffered-channel semaphore in Exercise 2, but
with errgroup managing it. (`SetLimit` must be called before any active goroutines,
and passing a negative value removes the limit.)

The subtlety is the error semantics. errgroup's built-in behavior is fail-fast: if a
`g.Go` function returns a non-nil error, `Wait` returns *that first error* and
discards the rest — and if you also used `WithContext`, it cancels. That is exactly
what we do *not* want for a batch report. So each `g.Go` function here returns `nil`
and instead records its item's error into a shared slice under a `sync.Mutex`. Because
every function returns nil, `Wait` returns nil and never short-circuits, so every item
runs. After `Wait`, we fold all recorded errors into one with `errors.Join`, whose
result satisfies `errors.Is` for each joined error — so a caller can test for any
specific failure.

`TryGo` is the non-blocking sibling of `Go`: it starts the function only if the group
is below its limit, returning `true` if it started and `false` if the limit was
saturated. It is useful when you want to offer work opportunistically without blocking
the producer.

Create `bulk.go`:

```go
package bulk

import (
	"errors"
	"sync"

	"golang.org/x/sync/errgroup"
)

// Item is one unit in the batch.
type Item struct {
	ID int
}

// Handler processes a single item. A non-nil return is recorded, not fatal.
type Handler func(it Item) error

// Process handles every item with at most limit concurrent handlers. It does NOT
// stop on the first failure; instead it collects every item's error and returns
// their errors.Join aggregate (nil if all succeeded).
func Process(items []Item, limit int, handle Handler) error {
	var g errgroup.Group
	if limit > 0 {
		g.SetLimit(limit)
	}

	var (
		mu   sync.Mutex
		errs []error
	)
	for _, it := range items {
		g.Go(func() error {
			if err := handle(it); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
			return nil // return nil so Wait never short-circuits
		})
	}
	_ = g.Wait() // always nil here; item errors live in errs
	return errors.Join(errs...)
}
```

Note `errors.Join` returns nil when its argument slice is empty or all-nil, so a
fully successful batch yields a nil error naturally.

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/bulk"
)

func main() {
	items := make([]bulk.Item, 0, 6)
	for i := range 6 {
		items = append(items, bulk.Item{ID: i})
	}

	handle := func(it bulk.Item) error {
		if it.ID%3 == 0 { // ids 0 and 3 fail
			return fmt.Errorf("item %d: checksum mismatch", it.ID)
		}
		return nil
	}

	err := bulk.Process(items, 2, handle)
	if err != nil {
		fmt.Printf("batch completed with failures: %d\n", len(splitLines(err.Error())))
		return
	}
	fmt.Println("batch OK")
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := range len(s) {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	lines = append(lines, s[start:])
	return lines
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
batch completed with failures: 2
```

### Tests

`TestProcessRespectsLimit` tracks peak concurrency with an atomic and asserts it never
exceeds the limit over many slow items. `TestProcessJoinsAllErrors` feeds a mix of
passing and failing items and asserts `errors.Is` finds every distinct failure in the
joined result — proving fail-fast is *not* happening. `TestTryGoSaturated` drives
`errgroup` directly: it saturates a limit-2 group with two blocking goroutines and
asserts a third `TryGo` returns false.

Create `bulk_test.go`:

```go
package bulk

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"
)

func TestProcessRespectsLimit(t *testing.T) {
	t.Parallel()

	const (
		total = 30
		limit = 3
	)
	var inFlight, peak atomic.Int64

	handle := func(it Item) error {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		inFlight.Add(-1)
		return nil
	}

	items := make([]Item, 0, total)
	for i := range total {
		items = append(items, Item{ID: i})
	}
	if err := Process(items, limit, handle); err != nil {
		t.Fatalf("Process err = %v, want nil", err)
	}
	if got := peak.Load(); got > limit {
		t.Fatalf("peak concurrency = %d, want <= %d", got, limit)
	}
}

func TestProcessJoinsAllErrors(t *testing.T) {
	t.Parallel()

	errEven := errors.New("even id rejected")
	errFive := errors.New("id five rejected")

	handle := func(it Item) error {
		switch {
		case it.ID == 5:
			return errFive
		case it.ID%2 == 0:
			return errEven
		default:
			return nil
		}
	}

	items := []Item{{ID: 1}, {ID: 2}, {ID: 4}, {ID: 5}}
	err := Process(items, 2, handle)
	if err == nil {
		t.Fatal("Process err = nil, want joined errors")
	}
	// errors.Join is fail-fast-free: BOTH distinct errors must be present.
	if !errors.Is(err, errEven) {
		t.Errorf("joined err missing errEven: %v", err)
	}
	if !errors.Is(err, errFive) {
		t.Errorf("joined err missing errFive: %v", err)
	}
}

func TestProcessAllSuccess(t *testing.T) {
	t.Parallel()

	handle := func(it Item) error { return nil }
	if err := Process([]Item{{ID: 1}, {ID: 2}}, 2, handle); err != nil {
		t.Fatalf("Process err = %v, want nil", err)
	}
}

func TestTryGoSaturated(t *testing.T) {
	t.Parallel()

	var g errgroup.Group
	g.SetLimit(2)

	started := make(chan struct{}, 2)
	release := make(chan struct{})

	block := func() error {
		started <- struct{}{}
		<-release
		return nil
	}
	// Fill both slots and wait until both are actually running.
	g.Go(block)
	g.Go(block)
	<-started
	<-started

	if g.TryGo(func() error { return nil }) {
		close(release)
		_ = g.Wait()
		t.Fatal("TryGo returned true while the group was saturated")
	}

	close(release)
	if err := g.Wait(); err != nil {
		t.Fatalf("Wait err = %v, want nil", err)
	}
}

func ExampleProcess() {
	handle := func(it Item) error {
		if it.ID == 0 {
			return fmt.Errorf("bad item %d", it.ID)
		}
		return nil
	}
	err := Process([]Item{{ID: 0}, {ID: 1}}, 2, handle)
	fmt.Println(err)
	// Output: bad item 0
}
```

## Review

The processor is correct when peak concurrency never exceeds the limit, *every*
failing item shows up in the joined error (proved by two `errors.Is` checks, which
would fail under fail-fast semantics), and a clean batch returns nil. The `TryGo`
subtest pins the saturation contract by holding both slots busy until a barrier and
asserting the third attempt is refused.

The design decision worth remembering: errgroup's default is fail-fast, so to get a
batch report you deliberately return nil from each `g.Go` and collect errors yourself,
then `errors.Join` them. Choose this shape when you must attempt every item; choose
Exercise 5's `WithContext` shape when the first failure should abort the rest. Run
`go test -race` to confirm the mutex guards the error slice.

## Resources

- [`errgroup.Group.SetLimit`](https://pkg.go.dev/golang.org/x/sync/errgroup#Group.SetLimit) — bounding concurrency; `TryGo` is documented alongside.
- [`errors.Join`](https://pkg.go.dev/errors#Join) — aggregating multiple errors so `errors.Is` finds each.
- [`errgroup.Group.TryGo`](https://pkg.go.dev/golang.org/x/sync/errgroup#Group.TryGo) — non-blocking launch under a limit.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-errgroup-first-error-cancel.md](05-errgroup-first-error-cancel.md) | Next: [07-graceful-shutdown-inflight-drain.md](07-graceful-shutdown-inflight-drain.md)
