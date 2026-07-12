# Exercise 30: Distributed Tracing: Child-Span Panic Isolation in Root Span

**Nivel: Intermedio** — validacion rapida (un test corto).

A traced checkout request typically fans out into several child spans run
concurrently — fetch inventory, charge the card, send a receipt — each
instrumented independently and each capable of failing in its own way. A
tracing SDK that lets one child span's panic take down the goroutine
running a completely unrelated child span would be actively harmful: the
one thing a tracing layer must never do is make an application-level bug
in one traced operation worse by turning it into a crash that also destroys
the trace data for every sibling operation. This module builds
`RootSpan.RunChildren`, which runs every named child concurrently, isolates
each one's panic to its own goroutine, and reports a stable, ordered set of
per-span outcomes back to the root. It is fully self-contained: its own
module, demo, and tests.

## What you'll build

```text
tracing/                    independent module: example.com/tracing
  go.mod                     go 1.24
  tracing.go                   SpanResult, RootSpan, NewRootSpan, RunChildren, AnyFailed
  cmd/
    demo/
      main.go                runnable demo: 3 children for a checkout trace, one panics
  tracing_test.go              panic isolated to one span, all succeed, results order-stable
```

Files: `tracing.go`, `cmd/demo/main.go`, `tracing_test.go`.
Implement: `RootSpan.RunChildren(children map[string]func() error) []SpanResult` that launches one goroutine per child span, isolates each child's panic in `runChild`, and returns results in lexical name order regardless of goroutine completion order.
Test: three child spans - one clean, one panicking, one returning an ordinary error - asserting the panic is isolated to its own span and distinguishable from the plain error; all children succeeding; the returned slice's order matching sorted names rather than completion order.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why results are pre-indexed by sorted name, not appended as goroutines finish

Every child span in `RunChildren` runs concurrently in its own goroutine,
so which one finishes first is not something the root span controls or
should depend on. If the goroutines each appended their own `SpanResult` to
a shared slice as they finished, the resulting order would be
scheduler-dependent - correct on every individual run, but different from
run to run, which makes both a demo's expected output and a test's
assertions nondeterministic in the worst way: they pass locally and fail
intermittently in CI. `RunChildren` instead sorts the child names up front,
assigns each child a fixed index into a pre-sized `results` slice, and has
each goroutine write only to its own index - a pattern that requires no
mutex at all, because two goroutines never touch the same slice element,
while still producing a completely reproducible, sorted-by-name result
regardless of real concurrency underneath.

The recover boundary itself lives in `runChild`, one per goroutine, exactly
like the per-worker boundary in a worker pool: a child span panicking - a
nil pointer on an unexpected downstream response shape - can only ever
unwind that child's own goroutine stack, never a sibling's and never the
root's. The root's job is purely to wait for every child (`wg.Wait()`) and
then look at the accumulated `SpanResult`s to decide whether the whole
trace should be reported as failed; it never needs its own recover, because
nothing it does directly can panic on a child's behalf.

Create `tracing.go`:

```go
package tracing

import (
	"fmt"
	"sort"
	"sync"
)

// SpanResult is one child span's outcome.
type SpanResult struct {
	Name     string
	Err      error
	Panicked bool
}

// RootSpan is the parent span coordinating a set of concurrently-run child
// spans that share its TraceID for correlation.
type RootSpan struct {
	TraceID string
	Name    string
}

// NewRootSpan starts a root span for one trace.
func NewRootSpan(traceID, name string) *RootSpan {
	return &RootSpan{TraceID: traceID, Name: name}
}

// RunChildren executes every named child span function concurrently, each
// in its own goroutine. Each child span runs under its own recover
// boundary, so one child's panic - a nil response from a downstream call, a
// bad assumption about a payload shape - can never unwind into a sibling
// child's goroutine or into the root span's own goroutine: it only ever
// produces that one child's SpanResult. The root collects every outcome
// before deciding whether the whole trace should be reported as failed,
// rather than the first panicking child taking the rest of the trace's
// observability down with it.
//
// Results are returned in a stable order - the child names sorted
// lexically - rather than in whatever order the goroutines happened to
// finish, so a caller (or a test) can rely on a reproducible report instead
// of a scheduler-dependent one.
func (r *RootSpan) RunChildren(children map[string]func() error) []SpanResult {
	names := make([]string, 0, len(children))
	for name := range children {
		names = append(names, name)
	}
	sort.Strings(names)

	var wg sync.WaitGroup
	results := make([]SpanResult, len(names))
	for i, name := range names {
		i, name, fn := i, name, children[name]
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = runChild(r.TraceID, name, fn)
		}()
	}
	wg.Wait()
	return results
}

// runChild is the recover boundary: exactly one child span's untrusted fn,
// running in its own goroutine.
func runChild(traceID, name string, fn func() error) (result SpanResult) {
	result = SpanResult{Name: name}
	defer func() {
		if r := recover(); r != nil {
			result.Panicked = true
			if e, ok := r.(error); ok {
				result.Err = fmt.Errorf("trace %s span %q panicked: %w", traceID, name, e)
				return
			}
			result.Err = fmt.Errorf("trace %s span %q panicked: %v", traceID, name, r)
		}
	}()
	if err := fn(); err != nil {
		result.Err = fmt.Errorf("trace %s span %q failed: %w", traceID, name, err)
	}
	return result
}

// AnyFailed reports whether at least one child span failed or panicked.
func AnyFailed(results []SpanResult) bool {
	for _, r := range results {
		if r.Err != nil {
			return true
		}
	}
	return false
}
```

### The runnable demo

Three children of a `checkout` trace: `fetch-inventory` and `send-receipt`
succeed; `charge-card` panics on a nil pointer dereference. Sorted
lexically, `charge-card` prints first.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/tracing"
)

func main() {
	root := tracing.NewRootSpan("trace-9f3a", "checkout")

	children := map[string]func() error{
		"fetch-inventory": func() error {
			return nil
		},
		"charge-card": func() error {
			var resp *struct{ Approved bool }
			return fmt.Errorf("approved=%v", resp.Approved) // nil pointer dereference
		},
		"send-receipt": func() error {
			return nil
		},
	}

	results := root.RunChildren(children)
	for _, r := range results {
		status := "ok"
		if r.Err != nil {
			status = r.Err.Error()
		}
		fmt.Printf("span %s: %s\n", r.Name, status)
	}

	if tracing.AnyFailed(results) {
		fmt.Printf("trace %s (%s): reporting failure, %d span(s) affected\n", root.TraceID, root.Name, countFailed(results))
	}
}

func countFailed(results []tracing.SpanResult) int {
	n := 0
	for _, r := range results {
		if r.Err != nil {
			n++
		}
	}
	return n
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
span charge-card: trace trace-9f3a span "charge-card" panicked: runtime error: invalid memory address or nil pointer dereference
span fetch-inventory: ok
span send-receipt: ok
trace trace-9f3a (checkout): reporting failure, 1 span(s) affected
```

### Tests

`TestRunChildrenIsolatesPanicToOneSpan` runs three children - clean,
panicking, and plain-error - and asserts each is distinguishable by name
via a lookup map rather than by index, since goroutine completion order is
not otherwise guaranteed. `TestRunChildrenResultsAreOrderStable` confirms
the returned slice always matches sorted name order.

Create `tracing_test.go`:

```go
package tracing

import (
	"errors"
	"strings"
	"testing"
)

func TestRunChildrenIsolatesPanicToOneSpan(t *testing.T) {
	root := NewRootSpan("t1", "root")
	children := map[string]func() error{
		"a": func() error { return nil },
		"b": func() error { panic(errors.New("boom")) },
		"c": func() error { return errors.New("plain failure") },
	}

	results := root.RunChildren(children)
	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3", len(results))
	}

	byName := make(map[string]SpanResult, len(results))
	for _, r := range results {
		byName[r.Name] = r
	}

	if byName["a"].Err != nil {
		t.Fatalf("span a Err = %v, want nil", byName["a"].Err)
	}
	if !byName["b"].Panicked || !strings.Contains(byName["b"].Err.Error(), "boom") {
		t.Fatalf("span b = %+v, want a panic containing boom", byName["b"])
	}
	if byName["c"].Panicked || !strings.Contains(byName["c"].Err.Error(), "plain failure") {
		t.Fatalf("span c = %+v, want a plain failure, not a panic", byName["c"])
	}

	if !AnyFailed(results) {
		t.Fatal("AnyFailed = false, want true")
	}
}

func TestRunChildrenAllSucceed(t *testing.T) {
	root := NewRootSpan("t2", "root")
	children := map[string]func() error{
		"a": func() error { return nil },
		"b": func() error { return nil },
	}
	results := root.RunChildren(children)
	if AnyFailed(results) {
		t.Fatalf("AnyFailed = true, want false: %+v", results)
	}
}

func TestRunChildrenResultsAreOrderStable(t *testing.T) {
	root := NewRootSpan("t3", "root")
	children := map[string]func() error{
		"zeta":  func() error { return nil },
		"alpha": func() error { return nil },
		"mid":   func() error { return nil },
	}
	results := root.RunChildren(children)
	want := []string{"alpha", "mid", "zeta"}
	for i, name := range want {
		if results[i].Name != name {
			t.Fatalf("results[%d].Name = %q, want %q", i, results[i].Name, name)
		}
	}
}
```

## Review

`RunChildren` is correct when a panic in any one child span can only ever
affect that child's own `SpanResult`, never the root span's ability to
collect every other outcome, and when the returned order is fully
reproducible rather than dependent on goroutine scheduling. Pre-indexing
each goroutine's write by a sorted position is the detail that makes both
of those true at once without adding a mutex: it turns "which goroutine
finished first" from something the caller has to worry about into
something that provably cannot matter.

## Resources

- [OpenTelemetry: Traces](https://opentelemetry.io/docs/concepts/signals/traces/) — the production tracing model (root span, child spans) this module's shape is drawn from.
- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — the per-goroutine recover boundary `runChild` relies on.
- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup) — coordinating the root span's wait for every concurrently-running child.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [29-broadcast-observer-registry.md](29-broadcast-observer-registry.md) | Next: [31-fan-in-aggregator-partial-failure.md](31-fan-in-aggregator-partial-failure.md)
