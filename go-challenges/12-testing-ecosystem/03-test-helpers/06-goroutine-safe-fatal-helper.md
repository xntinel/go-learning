# Exercise 6: Concurrency-Safe Assertion Helpers for Worker Fanout

`t.Fatal` and `t.FailNow` must be called from the goroutine running the test — not
from a worker you spawned. A helper used inside an `errgroup` or `go func()` fanout
that calls `t.Fatal` is a documented bug that can hang the run or silently pass a
broken test. This module builds the correct pattern: workers record failures into
a thread-safe collector, and a `drain` helper on the test goroutine turns them into
a single attributed failure.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
safefanout/                  independent module: example.com/safefanout
  go.mod                     go 1.26
  process.go                 a pure per-item validator under concurrent test
  cmd/
    demo/
      main.go                fans out work, collects failures, reports a count
  fanout_test.go             collector (mutex-guarded) + drain(t); fanout validation test
```

- Files: `process.go`, `cmd/demo/main.go`, `fanout_test.go`.
- Implement: a pure `Classify(int) (string, error)` used as the work under test.
- Test: a mutex-guarded `collector` whose `errorf` is safe from any goroutine; `drain(t)` on the test goroutine converts accumulated failures into one `t.Error`/`t.FailNow`; N workers validate results through the collector.
- Verify: `go test -count=1 -race ./...`

### Why not `t.Fatal` in a worker

`t.Fatal` calls `runtime.Goexit`, which unwinds *the goroutine that called it*.
When that goroutine is the test's own, `Goexit` correctly ends the test. When it
is a worker you spawned, `Goexit` ends only the worker: the test goroutine keeps
running on state a "fatal" condition should have stopped, the `WaitGroup` the
worker was supposed to signal may never complete, and the run can hang or report a
misleading pass. The `testing` docs state the rule flatly: `FailNow` "must be
called from the goroutine running the test or benchmark function, not from other
goroutines created during the test."

Two safe options exist. `t.Error` (which only records, never `Goexit`s) *is* safe
from any goroutine, so a worker may call it directly. But if you want the richer
control of a fatal-style abort, or you want to aggregate many worker failures into
one readable report, the worker must instead push its failure into a synchronized
collector, and the *test goroutine* makes the terminal `t.Error`/`t.FailNow` call.
That is the pattern here: `collector.errorf` is mutex-guarded and callable from any
goroutine; `drain(t)`, called on the test goroutine after the fanout joins, replays
the collected messages through `t.Error` and calls `t.FailNow` if any exist.

Create `process.go`:

```go
package safefanout

import (
	"errors"
	"fmt"
)

// ErrUnclassified is returned for codes outside the known HTTP ranges.
var ErrUnclassified = errors.New("unclassified status code")

// Classify maps an HTTP status code to a coarse class used for metrics.
func Classify(code int) (string, error) {
	switch {
	case code >= 100 && code < 200:
		return "informational", nil
	case code >= 200 && code < 300:
		return "success", nil
	case code >= 300 && code < 400:
		return "redirect", nil
	case code >= 400 && code < 500:
		return "client_error", nil
	case code >= 500 && code < 600:
		return "server_error", nil
	default:
		return "", fmt.Errorf("classify %d: %w", code, ErrUnclassified)
	}
}
```

### The runnable demo

The demo fans out classification over a slice of codes on multiple goroutines,
records mismatches against an expected-class table into a mutex-guarded collector,
and prints the failure count — mirroring the test's structure without a
`*testing.T`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/safefanout"
)

func main() {
	type want struct {
		code int
		want string
	}
	cases := []want{
		{200, "success"},
		{404, "client_error"},
		{503, "server_error"},
		{200, "redirect"}, // deliberately wrong to show detection
	}

	var mu sync.Mutex
	var failures []string
	var wg sync.WaitGroup
	for _, c := range cases {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := safefanout.Classify(c.code)
			if err != nil || got != c.want {
				mu.Lock()
				failures = append(failures, fmt.Sprintf("Classify(%d)=%q want %q", c.code, got, c.want))
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	fmt.Printf("failures: %d\n", len(failures))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
failures: 1
```

### The tests

`collector` wraps a mutex around a slice of failure messages; `errorf` appends
under the lock and is safe to call from any goroutine. `drain(t)` runs on the test
goroutine, replays each message through `t.Error`, and calls `t.FailNow` if any
exist — the terminal fatal decision made where it is legal. `TestFanoutAllValid`
fans out over correct expectations, joins the workers, then drains: no failures, so
the test passes. `TestCollectorDetectsFailure` feeds a deliberately wrong
expectation to a *local* collector and asserts it recorded the mismatch — proving
the collector catches failures without failing the real test. A comment documents
why calling `t.Fatal` inside the worker would be a bug.

Create `fanout_test.go`:

```go
package safefanout

import (
	"fmt"
	"sync"
	"testing"
)

// collector accumulates failure messages from worker goroutines. errorf is safe
// to call from any goroutine; drain must run on the test goroutine.
type collector struct {
	mu   sync.Mutex
	msgs []string
}

func (c *collector) errorf(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, fmt.Sprintf(format, args...))
}

// drain replays collected failures through t on the test goroutine. Calling
// t.FailNow here is legal because drain runs on the test goroutine — doing the
// same from a worker would be undefined behavior.
func (c *collector) drain(t *testing.T) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.msgs {
		t.Error(m)
	}
	if len(c.msgs) > 0 {
		t.FailNow()
	}
}

func (c *collector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.msgs)
}

func TestFanoutAllValid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		code int
		want string
	}{
		{100, "informational"},
		{204, "success"},
		{301, "redirect"},
		{404, "client_error"},
		{500, "server_error"},
	}

	var col collector
	var wg sync.WaitGroup
	for _, c := range cases {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A worker must NOT call t.Fatal here: Goexit would unwind this
			// goroutine, not the test, and could hang the run. Record instead.
			got, err := Classify(c.code)
			if err != nil {
				col.errorf("Classify(%d) errored: %v", c.code, err)
				return
			}
			if got != c.want {
				col.errorf("Classify(%d) = %q, want %q", c.code, got, c.want)
			}
		}()
	}
	wg.Wait()
	col.drain(t) // terminal decision on the test goroutine
}

func TestCollectorDetectsFailure(t *testing.T) {
	t.Parallel()
	var col collector
	var wg sync.WaitGroup
	// One deliberately wrong expectation, checked concurrently.
	wg.Add(1)
	go func() {
		defer wg.Done()
		got, _ := Classify(200)
		if got != "redirect" {
			col.errorf("Classify(200) = %q, want redirect", got)
		}
	}()
	wg.Wait()

	if col.count() != 1 {
		t.Fatalf("collector recorded %d failures, want 1", col.count())
	}
}

func ExampleClassify() {
	class, _ := Classify(503)
	fmt.Println(class)
	// Output: server_error
}
```

## Review

The pattern is correct when no worker goroutine ever calls `t.Fatal`/`t.FailNow`,
failures flow through the mutex-guarded `collector`, and the single terminal
decision happens in `drain` on the test goroutine. `TestFanoutAllValid` proves the
happy path drains clean; `TestCollectorDetectsFailure` proves the collector
actually catches a mismatch — asserting the *mechanism* works without failing the
real suite, which is how you test a failure path without failing. Run
`go test -race`: the race detector is the whole point here — it confirms
`collector.errorf` is properly synchronized across the fanout, and it would flag an
unguarded slice append immediately. The mistake to avoid is the tempting
`if got != want { t.Fatalf(...) }` inside the `go func()`; it compiles, sometimes
even passes, and is a latent hang.

## Resources

- [testing.T.FailNow](https://pkg.go.dev/testing#T.FailNow) — the goroutine rule, quoted verbatim in the docs.
- [testing.T.Error](https://pkg.go.dev/testing#T.Error) — records without `Goexit`, safe from any goroutine.
- [Go Memory Model](https://go.dev/ref/mem) — why the mutex is required for the shared collector.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-golden-file-assert-helper.md](07-golden-file-assert-helper.md)
