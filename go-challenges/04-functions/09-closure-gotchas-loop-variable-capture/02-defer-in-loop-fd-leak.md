# Exercise 2: Batch File/Row Processor: Defer Accumulation and Cleanup Order

A batch processor opens a sequence of resources in a loop, processes each, and
closes it. The obvious `defer r.Close()` inside the loop body is a
file-descriptor leak: the deferred calls do not run until the whole function
returns, and they run in reverse order. You build both the leaking version and
the correct per-iteration-helper version, and instrument a fake resource to prove
the difference in peak concurrent-open count and in close order.

## What you'll build

```text
batchproc/                   independent module: example.com/batchproc
  go.mod                     go 1.26
  batch.go                   Resource, Tracker; ProcessLeaky, ProcessScoped; errors.Join on close
  cmd/
    demo/
      main.go                runnable demo: run both, print peak-open and close order
  batch_test.go              peak-open==1 vs ==N, LIFO vs per-iteration order, join errors
```

- Files: `batch.go`, `cmd/demo/main.go`, `batch_test.go`.
- Implement: `ProcessLeaky` (defer inside the loop, all opens outlive the loop) and `ProcessScoped` (a per-iteration helper that scopes the defer), both aggregating close errors with `errors.Join`.
- Test: a `Tracker` records open/close events; assert scoped keeps peak concurrent-open at 1 and closes each before opening the next, while leaky peaks at N and closes LIFO; assert every close error surfaces.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/batchproc/cmd/demo
cd ~/go-exercises/batchproc
go mod init example.com/batchproc
go mod edit -go=1.26
```

### Why the naive version leaks

`defer` schedules a call to run when the *enclosing function* returns, not when
the loop iteration ends. So in `ProcessLeaky`, every `r.Close()` is queued and
none runs until `ProcessLeaky` itself returns. During the loop, resource 0 is
still open when resource 1 opens, and so on: the peak number of simultaneously
open resources equals N. In production this is the classic file-descriptor or
database-connection leak — a loop over ten thousand rows holds ten thousand
handles open at once and exhausts the process or the connection pool long before
the function returns. When the function finally returns, the deferred closes run
LIFO, so resource N-1 closes first and resource 0 closes last.

`ProcessScoped` fixes it by moving the per-item work into a helper function whose
own `return` scopes the `defer`. Each call to the helper opens a resource, defers
its close, processes it, and returns — at which point the deferred close runs,
before the next iteration opens anything. Peak concurrent-open is 1, and closes
happen in open order (each item closes before the next opens). The helper also
lets each iteration return its own error without abandoning the loop.

The `Tracker` is an in-memory fake that models a pool of resources. It records an
"open" and a "close" event with the resource name and tracks the live count so a
test can read the peak. `errors.Join` collects a close error from every resource
into one error whose `Unwrap() []error` lets `errors.Is` match any of them —
this is the standard way to avoid losing the second close failure when several
resources fail.

Create `batch.go`:

```go
package batchproc

import (
	"errors"
	"sync"
)

// Event is one open or close observed by the Tracker.
type Event struct {
	Name   string
	Closed bool // false = open, true = close
}

// Tracker is an in-memory stand-in for a pool of OS resources. It records the
// order of opens and closes and the peak number simultaneously open.
type Tracker struct {
	mu     sync.Mutex
	events []Event
	live   int
	peak   int
	failOn map[string]error
}

// NewTracker returns a Tracker. failOn maps a resource name to the error its
// Close should return, so a test can inject close failures.
func NewTracker(failOn map[string]error) *Tracker {
	return &Tracker{failOn: failOn}
}

// Resource is a handle the batch processor opens and closes.
type Resource struct {
	name string
	t    *Tracker
}

// Open records an open event and returns a live Resource.
func (t *Tracker) Open(name string) *Resource {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.events = append(t.events, Event{Name: name})
	t.live++
	if t.live > t.peak {
		t.peak = t.live
	}
	return &Resource{name: name, t: t}
}

// Close records a close event and returns any injected failure for this name.
func (r *Resource) Close() error {
	r.t.mu.Lock()
	defer r.t.mu.Unlock()
	r.t.events = append(r.t.events, Event{Name: r.name, Closed: true})
	r.t.live--
	return r.t.failOn[r.name]
}

// Peak reports the maximum number of resources open at the same instant.
func (t *Tracker) Peak() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.peak
}

// Order returns the recorded event sequence.
func (t *Tracker) Order() []Event {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]Event(nil), t.events...)
}

// ProcessLeaky is the anti-pattern: defer r.Close() inside the loop, so every
// resource stays open until ProcessLeaky returns and they close LIFO.
func ProcessLeaky(t *Tracker, names []string) error {
	var errs []error
	for _, name := range names {
		r := t.Open(name)
		defer func() {
			if err := r.Close(); err != nil {
				errs = append(errs, err)
			}
		}()
		_ = r.name // "process" the resource
	}
	return errors.Join(errs...)
}

// ProcessScoped is the fix: each iteration runs a helper whose return scopes the
// defer, so a resource closes before the next opens. Peak concurrent-open is 1.
func ProcessScoped(t *Tracker, names []string) error {
	var errs []error
	for _, name := range names {
		err := func() (err error) {
			r := t.Open(name)
			defer func() {
				if cerr := r.Close(); cerr != nil {
					err = cerr
				}
			}()
			_ = r.name // "process" the resource
			return nil
		}()
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
```

### The runnable demo

The demo runs both forms over three named resources and prints the peak open
count and the close order, so the leak is visible: leaky peaks at 3 and closes
in reverse, scoped peaks at 1 and closes in order.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/batchproc"
)

func closeOrder(t *batchproc.Tracker) []string {
	var out []string
	for _, e := range t.Order() {
		if e.Closed {
			out = append(out, e.Name)
		}
	}
	return out
}

func main() {
	names := []string{"a", "b", "c"}

	leaky := batchproc.NewTracker(nil)
	_ = batchproc.ProcessLeaky(leaky, names)
	fmt.Println("leaky  peak-open:", leaky.Peak(), "close-order:", closeOrder(leaky))

	scoped := batchproc.NewTracker(nil)
	_ = batchproc.ProcessScoped(scoped, names)
	fmt.Println("scoped peak-open:", scoped.Peak(), "close-order:", closeOrder(scoped))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
leaky  peak-open: 3 close-order: [c b a]
scoped peak-open: 1 close-order: [a b c]
```

### Tests

`TestScopedClosesEachBeforeNextOpen` asserts the scoped form never holds more
than one resource open and closes in open order. `TestLeakyHoldsAllOpen` asserts
the leaky form peaks at N and closes LIFO — this is the leak, encoded as a
characterization test so the difference is unmistakable. `TestJoinSurfacesEvery
CloseError` injects two close failures and asserts both are reachable through
`errors.Is` on the joined error.

Create `batch_test.go`:

```go
package batchproc

import (
	"errors"
	"fmt"
	"testing"
)

func closeNames(t *Tracker) []string {
	var out []string
	for _, e := range t.Order() {
		if e.Closed {
			out = append(out, e.Name)
		}
	}
	return out
}

func TestScopedClosesEachBeforeNextOpen(t *testing.T) {
	t.Parallel()

	names := []string{"a", "b", "c", "d"}
	tr := NewTracker(nil)
	if err := ProcessScoped(tr, names); err != nil {
		t.Fatalf("ProcessScoped: %v", err)
	}
	if got := tr.Peak(); got != 1 {
		t.Fatalf("scoped peak-open = %d, want 1", got)
	}
	want := []string{"a", "b", "c", "d"}
	got := closeNames(tr)
	if len(got) != len(want) {
		t.Fatalf("close count = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("close order = %v, want %v", got, want)
		}
	}
}

func TestLeakyHoldsAllOpen(t *testing.T) {
	t.Parallel()

	names := []string{"a", "b", "c", "d"}
	tr := NewTracker(nil)
	if err := ProcessLeaky(tr, names); err != nil {
		t.Fatalf("ProcessLeaky: %v", err)
	}
	if got := tr.Peak(); got != len(names) {
		t.Fatalf("leaky peak-open = %d, want %d", got, len(names))
	}
	want := []string{"d", "c", "b", "a"} // LIFO
	got := closeNames(tr)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("close order = %v, want %v (LIFO)", got, want)
		}
	}
}

func TestJoinSurfacesEveryCloseError(t *testing.T) {
	t.Parallel()

	errB := errors.New("close b failed")
	errD := errors.New("close d failed")
	tr := NewTracker(map[string]error{"b": errB, "d": errD})

	err := ProcessScoped(tr, []string{"a", "b", "c", "d"})
	if err == nil {
		t.Fatal("want a joined error, got nil")
	}
	if !errors.Is(err, errB) {
		t.Errorf("joined error does not wrap errB: %v", err)
	}
	if !errors.Is(err, errD) {
		t.Errorf("joined error does not wrap errD: %v", err)
	}
}

func ExampleProcessScoped() {
	tr := NewTracker(nil)
	_ = ProcessScoped(tr, []string{"x", "y"})
	fmt.Println("peak:", tr.Peak())
	// Output: peak: 1
}
```

## Review

The processor is correct when scoped work keeps peak concurrent-open at 1 and
closes in open order, while leaky peaks at N and closes LIFO — the two
characterization tests encode exactly that contrast, so the leak cannot silently
reappear. The mechanism to keep straight is that `defer` fires at function
return: inside a bare loop that means "at the end of the whole batch," which is
the leak; inside a per-iteration helper it means "at the end of this item,"
which is the fix. `errors.Join` is what stops the second close failure from being
swallowed; `errors.Is` reaching both injected errors proves it. Run `go test
-race` since the `Tracker` is mutated under a mutex.

## Resources

- [Effective Go: defer](https://go.dev/doc/effective_go#defer) — defer timing and LIFO ordering.
- [`errors.Join`](https://pkg.go.dev/errors#Join) — aggregating multiple errors into one that `errors.Is` can match.
- [Go blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — when deferred calls actually run.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-errgroup-bounded-fetch.md](03-errgroup-bounded-fetch.md)
