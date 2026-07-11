# Exercise 35: Distributed Tracing Context Propagation via Decorator Callbacks

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye concurrencia).

Distributed tracing needs every span to know its trace and its parent
without any call site manually threading IDs through function arguments —
which is exactly what `context.Context` plus a decorator is for. This
module builds a `Decorate` function that wraps any `Operation` so it
transparently starts a child span of whatever span is already active in
the incoming context (or a brand-new trace if none is), installs that
child span back into the context the operation actually runs with, and
reports the outcome to an injected `Recorder` — the same shape
OpenTelemetry's span parenting uses, built from nothing but function
types and `context.WithValue`.

## What you'll build

```text
tracing/                      independent module: example.com/tracing-context-propagator-decorator
  go.mod                       go 1.24
  tracing.go                   type SpanContext, func WithSpan/SpanFromContext, type Operation, type IDGenerator, type Recorder, func Decorate, type RecordingSink
  cmd/
    demo/
      main.go                    runnable demo: an order-service operation calling an inventory-service operation, trace propagated across both
  tracing_test.go                root span generates a TraceID, nested Decorate parent/child linkage, recorder observes error, concurrency (-race)
```

Files: `tracing.go`, `cmd/demo/main.go`, `tracing_test.go`.
Implement: `type SpanContext struct { TraceID, SpanID, ParentSpanID string }`, `func WithSpan(ctx, SpanContext) context.Context`, `func SpanFromContext(ctx) (SpanContext, bool)`, `type Operation func(ctx) error`, `type IDGenerator func() string`, `type Recorder func(SpanContext, error)`, `func Decorate(op Operation, gen IDGenerator, rec Recorder) Operation`, and a mutex-guarded `RecordingSink`; `Decorate` must create a fresh child span linked to any span already in `ctx` (or start a new trace if none), run `op` with the child span installed, and always call `rec` with the finished span and outcome.
Test: a root call (no existing span in context) generates a new `TraceID` and an empty `ParentSpanID`; a nested `Decorate` call inside another produces a child whose `TraceID` matches the parent's and whose `ParentSpanID` equals the parent's `SpanID`, with a distinct `SpanID` of its own; the recorder observes an operation's error; many concurrent decorated operations each get a distinct `SpanID` with no data race, using an injected, lock-protected `IDGenerator` instead of a random/UUID one.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/tracing-context-propagator-decorator/cmd/demo
cd ~/go-exercises/tracing-context-propagator-decorator
go mod init example.com/tracing-context-propagator-decorator
go mod edit -go=1.24
```

### Why the child span goes into a new context, not the caller's

`Decorate` never mutates the `ctx` it receives — `context.Context` values
are immutable by convention, and `WithSpan` returns a *new* context
wrapping the old one via `context.WithValue`. That is what makes the
parent/child relationship fall out of ordinary function nesting instead
of needing any global registry: when `outer` (decorated) calls `inner`
(also decorated) with the `ctx` it was itself given — not
`context.Background()`, the actual `ctx` parameter passed down from its
own `Decorate` wrapper — `inner`'s call to `SpanFromContext` finds
`outer`'s span already installed there, and builds its own child
`SpanContext` with `TraceID` copied across and `ParentSpanID` set to
`outer`'s `SpanID`. No span ever needs to know about any other span
directly; the chain is threaded entirely through the immutable context
values each call passes to the next, which is the same mechanism
`context.WithCancel`/`WithTimeout` use to thread deadlines through a call
tree. The `IDGenerator` is injected for exactly the reason a clock gets
injected elsewhere in this chapter: a real tracer would use random UUIDs,
but a test needs `TestNestedDecorateProducesCorrectParentChildLinkage` to
assert *which* ID went where, and that only works with a deterministic
counter standing in for `gen`.

Create `tracing.go`:

```go
// Package tracing decorates operations with distributed trace context
// propagation, in the spirit of OpenTelemetry's span parenting: every
// decorated call creates a child span linked to whatever span (if any)
// was already active in ctx, and reports its own outcome to a Recorder.
package tracing

import (
	"context"
	"sync"
)

// SpanContext identifies one span within a trace.
type SpanContext struct {
	TraceID      string
	SpanID       string
	ParentSpanID string
}

type contextKey struct{}

// WithSpan returns a copy of ctx carrying sc as the active span.
func WithSpan(ctx context.Context, sc SpanContext) context.Context {
	return context.WithValue(ctx, contextKey{}, sc)
}

// SpanFromContext returns the active span in ctx, if any.
func SpanFromContext(ctx context.Context) (SpanContext, bool) {
	sc, ok := ctx.Value(contextKey{}).(SpanContext)
	return sc, ok
}

// Operation is any function whose execution should be traced.
type Operation func(ctx context.Context) error

// IDGenerator produces a new span ID on every call. Inject a counter-based
// one in tests instead of a random/UUID generator, for deterministic IDs.
type IDGenerator func() string

// Recorder observes a decorated Operation's completed span and outcome.
type Recorder func(sc SpanContext, err error)

// Decorate wraps op so that, on every call, it creates a child span of
// whatever span is active in the incoming ctx (or starts a new trace if
// none is active), runs op with that child span installed in ctx, and
// reports the finished span and outcome to rec.
func Decorate(op Operation, gen IDGenerator, rec Recorder) Operation {
	return func(ctx context.Context) error {
		parent, hasParent := SpanFromContext(ctx)

		child := SpanContext{SpanID: gen()}
		if hasParent {
			child.TraceID = parent.TraceID
			child.ParentSpanID = parent.SpanID
		} else {
			child.TraceID = gen()
		}

		err := op(WithSpan(ctx, child))
		if rec != nil {
			rec(child, err)
		}
		return err
	}
}

// RecordingSink collects spans reported by a Recorder, guarded by a
// mutex since decorated Operations may run concurrently.
type RecordingSink struct {
	mu    sync.Mutex
	spans []SpanContext
	errs  []error
}

// Record is a Recorder that appends to the sink.
func (s *RecordingSink) Record(sc SpanContext, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.spans = append(s.spans, sc)
	s.errs = append(s.errs, err)
}

// Snapshot returns a copy of every span and error recorded so far.
func (s *RecordingSink) Snapshot() ([]SpanContext, []error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	spans := append([]SpanContext(nil), s.spans...)
	errs := append([]error(nil), s.errs...)
	return spans, errs
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/tracing-context-propagator-decorator"
)

func main() {
	var counter int
	gen := func() string {
		counter++
		return fmt.Sprintf("id-%d", counter)
	}

	sink := &tracing.RecordingSink{}

	// callInventoryService is the innermost "downstream" call.
	callInventoryService := tracing.Decorate(func(ctx context.Context) error {
		fmt.Println("inventory service: checking stock")
		return nil
	}, gen, sink.Record)

	// handleOrder is the "root" operation. It calls callInventoryService
	// with the context it was given, so the inventory span becomes a
	// child of the order span.
	handleOrder := tracing.Decorate(func(ctx context.Context) error {
		fmt.Println("order service: handling order")
		return callInventoryService(ctx)
	}, gen, sink.Record)

	if err := handleOrder(context.Background()); err != nil {
		fmt.Println("error:", err)
	}

	spans, _ := sink.Snapshot()
	for _, sp := range spans {
		fmt.Printf("span=%s trace=%s parent=%q\n", sp.SpanID, sp.TraceID, sp.ParentSpanID)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
order service: handling order
inventory service: checking stock
span=id-3 trace=id-2 parent="id-1"
span=id-1 trace=id-2 parent=""
```

### Tests

Create `tracing_test.go`:

```go
package tracing

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

func counterGen() IDGenerator {
	var n int
	return func() string {
		n++
		return fmt.Sprintf("id-%d", n)
	}
}

func TestDecorateWithNoParentGeneratesNewTraceID(t *testing.T) {
	t.Parallel()
	gen := counterGen()
	var recorded SpanContext
	op := Decorate(func(ctx context.Context) error {
		sc, ok := SpanFromContext(ctx)
		if !ok {
			t.Fatal("op should see a span installed by Decorate")
		}
		recorded = sc
		return nil
	}, gen, func(sc SpanContext, err error) {})

	if err := op(context.Background()); err != nil {
		t.Fatalf("op: %v", err)
	}
	if recorded.TraceID == "" {
		t.Fatal("expected a generated TraceID for a root span")
	}
	if recorded.ParentSpanID != "" {
		t.Fatalf("ParentSpanID = %q, want empty for a root span", recorded.ParentSpanID)
	}
}

func TestNestedDecorateProducesCorrectParentChildLinkage(t *testing.T) {
	t.Parallel()
	gen := counterGen()
	var innerSeen SpanContext
	var outerSeen SpanContext

	inner := Decorate(func(ctx context.Context) error {
		sc, _ := SpanFromContext(ctx)
		innerSeen = sc
		return nil
	}, gen, func(sc SpanContext, err error) {})

	outer := Decorate(func(ctx context.Context) error {
		sc, _ := SpanFromContext(ctx)
		outerSeen = sc
		return inner(ctx)
	}, gen, func(sc SpanContext, err error) {})

	if err := outer(context.Background()); err != nil {
		t.Fatalf("outer: %v", err)
	}

	if innerSeen.TraceID != outerSeen.TraceID {
		t.Fatalf("innerSeen.TraceID = %q, outerSeen.TraceID = %q, want equal (same trace)", innerSeen.TraceID, outerSeen.TraceID)
	}
	if innerSeen.ParentSpanID != outerSeen.SpanID {
		t.Fatalf("innerSeen.ParentSpanID = %q, want outerSeen.SpanID = %q", innerSeen.ParentSpanID, outerSeen.SpanID)
	}
	if innerSeen.SpanID == outerSeen.SpanID {
		t.Fatal("inner and outer spans must have distinct SpanIDs")
	}
}

func TestRecorderObservesOperationError(t *testing.T) {
	t.Parallel()
	gen := counterGen()
	opErr := errors.New("downstream unavailable")
	var gotErr error
	op := Decorate(func(ctx context.Context) error {
		return opErr
	}, gen, func(sc SpanContext, err error) {
		gotErr = err
	})

	err := op(context.Background())
	if !errors.Is(err, opErr) {
		t.Fatalf("op err = %v, want %v", err, opErr)
	}
	if !errors.Is(gotErr, opErr) {
		t.Fatalf("recorder saw err = %v, want %v", gotErr, opErr)
	}
}

func TestConcurrentDecoratedOperationsGetIndependentSpans(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var genN int
	gen := func() string {
		mu.Lock()
		defer mu.Unlock()
		genN++
		return fmt.Sprintf("id-%d", genN)
	}

	sink := &RecordingSink{}
	op := Decorate(func(ctx context.Context) error {
		return nil
	}, gen, sink.Record)

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = op(context.Background())
		}()
	}
	wg.Wait()

	spans, errs := sink.Snapshot()
	if len(spans) != 50 || len(errs) != 50 {
		t.Fatalf("recorded %d spans / %d errs, want 50/50", len(spans), len(errs))
	}
	seen := make(map[string]bool, 50)
	for _, sp := range spans {
		if seen[sp.SpanID] {
			t.Fatalf("duplicate SpanID recorded: %s", sp.SpanID)
		}
		seen[sp.SpanID] = true
	}
}
```

## Review

`Decorate` is correct when the span it builds always reflects the true
call structure: a root call (nothing in `ctx` yet) starts a new trace with
no parent, and a nested call inside another decorated call always links
to that caller's span, never to some stale or global "current span".
`TestDecorateWithNoParentGeneratesNewTraceID` pins down the root case, and
`TestNestedDecorateProducesCorrectParentChildLinkage` pins down the
propagation itself — same `TraceID`, `ParentSpanID` equal to the
caller's `SpanID`, and a `SpanID` of its own distinct from the caller's,
which is the entire contract distributed tracing depends on to
reconstruct a call tree from independently reported spans afterward.
`TestRecorderObservesOperationError` confirms the observability half:
whatever `op` returns reaches both the caller and the recorder
identically. The concurrency test does not (and cannot) assert anything
about trace/parent relationships across goroutines — each goroutine calls
the *same* root operation independently, so their spans are unrelated by
design — it only asserts the property that must hold regardless: every
concurrent call gets its own non-colliding `SpanID`, which requires the
injected `IDGenerator` itself to be safe for concurrent use, exactly like
the fake clock injected elsewhere in this chapter needs to be when shared
across goroutines.

## Resources

- [context package](https://pkg.go.dev/context)
- [OpenTelemetry: Traces (spans, trace/span/parent IDs)](https://opentelemetry.io/docs/concepts/signals/traces/)
- [Go blog: Context](https://go.dev/blog/context)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [34-schema-migration-before-after-hook.md](34-schema-migration-before-after-hook.md) | Next: [../07-recursive-functions-and-stack-depth/00-concepts.md](../07-recursive-functions-and-stack-depth/00-concepts.md)
