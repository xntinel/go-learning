# Exercise 31: Distributed tracing span context propagation across services

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Every distributed trace is a tree of spans connected by a parent-span-ID
pointer: a request enters at a gateway, fans out to several downstream
services concurrently, and each of those may fan out further — and a
tracing backend has to reconstruct that causal tree from spans that arrive
in whatever order the goroutines that emitted them happened to finish. Get
the propagation wrong and the trace UI shows disconnected fragments instead
of one coherent waterfall, which is exactly the tool an on-call engineer
reaches for during an incident. This module is fully self-contained: its
own `go mod init`, all code inline, its own demo and tests.

## What you'll build

```text
tracing/                    independent module: example.com/tracing
  go.mod                     go 1.24
  tracing.go                   Span, Node, Propagate, Collect, Run, ValidateChain
  cmd/
    demo/
      main.go                runnable demo: a 3-level concurrent fanout, validated end to end
  tracing_test.go              table test: full fanout, single-node trace, early cancel, chain validation cases, 20 concurrent independent traces (-race)
```

- Files: `tracing.go`, `cmd/demo/main.go`, `tracing_test.go`.
- Implement: `Propagate(traceID, parentSpanID, node, spanCh)` emitting one span per node and recursing into every child concurrently; `Collect(spanCh, cancel) []Span` dequeuing until the channel closes or cancellation fires; `ValidateChain(spans) error` checking the causal tree is well-formed.
- Test: every span in a 3-level fanout collected with correct parent links, a single-node trace, `Collect` returning immediately on an already-cancelled channel, five `ValidateChain` cases (valid, missing parent, two roots, mismatched trace ID), and 20 concurrent independent `Run` calls proving no shared state leaks between them.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/06-labels-break-continue-goto/31-distributed-trace-span-propagation/cmd/demo
cd go-solutions/03-control-flow/06-labels-break-continue-goto/31-distributed-trace-span-propagation
go mod edit -go=1.24
```

### Why Collect's dequeue loop needs labeled breaks in both directions

`Collect` is a `for`-`select` — the shape this entire chapter centers on —
racing two ways to stop: the span channel closing (every producing
goroutine has returned, `Propagate` closed it) and an explicit `cancel`
signal (a caller-imposed bound, such as a maximum hop count a real tracing
backend enforces to avoid an unbounded collection window). A bare `break`
in either `select` case would leave only the `select`; the enclosing `for`
would immediately re-enter it, and since a closed channel is always
immediately ready to receive (`ok == false`, forever), that spin would burn
a core rather than return. `break collect`, named on the `for`, is what
actually ends the loop from either branch.

`Propagate` is the producer side, and its concurrency shape is the point of
the exercise: a real service does not call its downstream dependencies one
at a time and wait for each in turn — it fans out concurrently and joins on
all of them, exactly like `sync.WaitGroup` here spawning one goroutine per
child before calling `Wait`. Two properties make the whole thing testable
despite the concurrency: every node's span ID is assigned by the caller up
front (`Node.ID`), not generated at runtime, so the *content* of a trace is
fully deterministic regardless of which goroutine happens to run first —
only the *order* spans arrive on the channel is nondeterministic, and
`Collect`'s output is compared as a set (by parent-child links, or sorted by
ID), never by arrival order.

Create `tracing.go`:

```go
package tracing

import (
	"errors"
	"fmt"
	"sync"
)

// Span is one hop of a distributed trace: a service handling (a portion of)
// a request, with a stable link back to whichever span invoked it.
type Span struct {
	TraceID      string
	SpanID       string
	ParentSpanID string
	Service      string
}

// Node describes one service in a call tree used to drive Propagate. ID is
// a stable, caller-assigned span ID (unique within one trace) rather than
// something generated at runtime -- assigning IDs ahead of time keeps a
// trace's shape fully deterministic regardless of goroutine scheduling,
// which only affects the ORDER spans arrive on the channel, never their
// content.
type Node struct {
	ID       string
	Service  string
	Children []*Node
}

// Propagate walks node's call tree, emitting one Span per node onto spanCh
// with TraceID and a ParentSpanID linking it back to whichever node invoked
// it. Each child is walked in its own goroutine, mirroring a service that
// fans out to several downstream dependencies concurrently rather than
// calling them one at a time; Propagate returns only once every child (and
// every span in its own subtree) has been emitted.
func Propagate(traceID, parentSpanID string, node *Node, spanCh chan<- Span) {
	spanCh <- Span{TraceID: traceID, SpanID: node.ID, ParentSpanID: parentSpanID, Service: node.Service}

	var wg sync.WaitGroup
	for _, child := range node.Children {
		wg.Add(1)
		go func(child *Node) {
			defer wg.Done()
			Propagate(traceID, node.ID, child, spanCh)
		}(child)
	}
	wg.Wait()
}

// Collect reads spans from spanCh until it is closed (the trace is
// complete: every service in the fanout has returned and Propagate closed
// the channel) or cancel fires (the caller decided to stop collecting
// early -- e.g. a bound on how many hops a trace may have). Either way, the
// labeled break is what actually leaves the dequeue loop: a bare break
// inside either select case would leave only the select, and the for would
// immediately re-enter it and spin forever instead of returning.
func Collect(spanCh <-chan Span, cancel <-chan struct{}) []Span {
	var spans []Span
collect:
	for {
		select {
		case span, ok := <-spanCh:
			if !ok {
				break collect
			}
			spans = append(spans, span)
		case <-cancel:
			break collect
		}
	}
	return spans
}

// Run drives a full trace: it walks root concurrently, collecting every
// span emitted, and returns once the whole fanout has completed.
func Run(root *Node, traceID string) []Span {
	spanCh := make(chan Span)
	cancel := make(chan struct{}) // never closed: Run always waits for completion

	go func() {
		Propagate(traceID, "", root, spanCh)
		close(spanCh)
	}()

	return Collect(spanCh, cancel)
}

// ValidateChain checks that spans form a single well-formed causal chain:
// exactly one span is a root (empty ParentSpanID), every other span's
// ParentSpanID refers to another span actually present in the set, and
// every span shares the same TraceID -- the invariants a trace-collection
// backend checks before trusting a trace enough to render it.
func ValidateChain(spans []Span) error {
	if len(spans) == 0 {
		return errors.New("no spans")
	}
	byID := make(map[string]Span, len(spans))
	for _, s := range spans {
		byID[s.SpanID] = s
	}

	roots := 0
	for _, s := range spans {
		if s.TraceID != spans[0].TraceID {
			return fmt.Errorf("span %s: trace ID %q does not match %q", s.SpanID, s.TraceID, spans[0].TraceID)
		}
		if s.ParentSpanID == "" {
			roots++
			continue
		}
		if _, ok := byID[s.ParentSpanID]; !ok {
			return fmt.Errorf("span %s: parent %q not found in trace", s.SpanID, s.ParentSpanID)
		}
	}
	if roots != 1 {
		return fmt.Errorf("trace has %d root spans, want exactly 1", roots)
	}
	return nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sort"

	"example.com/tracing"
)

func main() {
	root := &tracing.Node{
		ID:      "gateway",
		Service: "gateway",
		Children: []*tracing.Node{
			{ID: "auth", Service: "auth"},
			{
				ID:      "billing",
				Service: "billing",
				Children: []*tracing.Node{
					{ID: "billing-db", Service: "billing-db"},
				},
			},
		},
	}

	spans := tracing.Run(root, "trace-abc123")

	sort.Slice(spans, func(i, j int) bool { return spans[i].SpanID < spans[j].SpanID })
	for _, s := range spans {
		fmt.Printf("span=%-12s parent=%-12q service=%s\n", s.SpanID, s.ParentSpanID, s.Service)
	}

	if err := tracing.ValidateChain(spans); err != nil {
		fmt.Println("invalid trace:", err)
	} else {
		fmt.Println("trace is valid:", len(spans), "spans")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
span=auth         parent="gateway"    service=auth
span=billing      parent="gateway"    service=billing
span=billing-db   parent="billing"    service=billing-db
span=gateway      parent=""           service=gateway
trace is valid: 4 spans
```

The four spans arrive on the channel in whatever order the underlying
goroutines happened to finish, but sorting by `SpanID` before printing makes
the demo's output identical on every run — the concurrency affects arrival
order, never the trace's actual shape.

### Tests

`TestRunCollectsEverySpanInTheFanout` is the core case, asserting every
parent link in a 3-level tree. `TestRunSingleNodeTraceHasOnlyARoot` and
`TestCollectStopsEarlyOnCancel` cover the boundaries. `TestValidateChain` is
a table of five causal-chain shapes. `TestRunManyConcurrentTracesDoNotInterfere`
launches 20 independent traces from separate goroutines and checks each one
comes back internally consistent, proving no state leaks between
unrelated `Run` calls — run this one under `-race`.

Create `tracing_test.go`:

```go
package tracing

import (
	"slices"
	"sort"
	"sync"
	"testing"
)

func sortedSpanIDs(spans []Span) []string {
	ids := make([]string, len(spans))
	for i, s := range spans {
		ids[i] = s.SpanID
	}
	sort.Strings(ids)
	return ids
}

func TestRunCollectsEverySpanInTheFanout(t *testing.T) {
	t.Parallel()

	root := &Node{
		ID:      "gateway",
		Service: "gateway",
		Children: []*Node{
			{ID: "auth", Service: "auth"},
			{ID: "billing", Service: "billing", Children: []*Node{
				{ID: "billing-db", Service: "billing-db"},
			}},
		},
	}

	spans := Run(root, "trace-1")

	wantIDs := []string{"auth", "billing", "billing-db", "gateway"}
	if got := sortedSpanIDs(spans); !slices.Equal(got, wantIDs) {
		t.Fatalf("span IDs = %v, want %v", got, wantIDs)
	}

	byID := make(map[string]Span, len(spans))
	for _, s := range spans {
		byID[s.SpanID] = s
	}
	if byID["gateway"].ParentSpanID != "" {
		t.Fatalf("root span parent = %q, want empty", byID["gateway"].ParentSpanID)
	}
	if byID["auth"].ParentSpanID != "gateway" {
		t.Fatalf("auth's parent = %q, want %q", byID["auth"].ParentSpanID, "gateway")
	}
	if byID["billing"].ParentSpanID != "gateway" {
		t.Fatalf("billing's parent = %q, want %q", byID["billing"].ParentSpanID, "gateway")
	}
	if byID["billing-db"].ParentSpanID != "billing" {
		t.Fatalf("billing-db's parent = %q, want %q", byID["billing-db"].ParentSpanID, "billing")
	}
	for _, s := range spans {
		if s.TraceID != "trace-1" {
			t.Fatalf("span %s trace ID = %q, want %q", s.SpanID, s.TraceID, "trace-1")
		}
	}
}

func TestRunSingleNodeTraceHasOnlyARoot(t *testing.T) {
	t.Parallel()

	spans := Run(&Node{ID: "solo", Service: "solo"}, "trace-solo")
	if len(spans) != 1 {
		t.Fatalf("len(spans) = %d, want 1", len(spans))
	}
	if spans[0].ParentSpanID != "" {
		t.Fatalf("ParentSpanID = %q, want empty", spans[0].ParentSpanID)
	}
}

func TestCollectStopsEarlyOnCancel(t *testing.T) {
	t.Parallel()

	spanCh := make(chan Span)
	cancel := make(chan struct{})
	close(cancel) // already cancelled: Collect must return immediately

	spans := Collect(spanCh, cancel)
	if spans != nil {
		t.Fatalf("spans = %v, want nil when cancelled before any span arrives", spans)
	}
}

func TestValidateChain(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		spans   []Span
		wantErr bool
	}{
		"empty trace is invalid": {
			spans:   nil,
			wantErr: true,
		},
		"a single root span is valid": {
			spans: []Span{
				{TraceID: "t1", SpanID: "root", ParentSpanID: ""},
			},
		},
		"a child whose parent is present is valid": {
			spans: []Span{
				{TraceID: "t1", SpanID: "root", ParentSpanID: ""},
				{TraceID: "t1", SpanID: "child", ParentSpanID: "root"},
			},
		},
		"a child whose parent is missing is invalid": {
			spans: []Span{
				{TraceID: "t1", SpanID: "root", ParentSpanID: ""},
				{TraceID: "t1", SpanID: "orphan", ParentSpanID: "ghost"},
			},
			wantErr: true,
		},
		"two roots is invalid": {
			spans: []Span{
				{TraceID: "t1", SpanID: "root-a", ParentSpanID: ""},
				{TraceID: "t1", SpanID: "root-b", ParentSpanID: ""},
			},
			wantErr: true,
		},
		"a mismatched trace ID is invalid": {
			spans: []Span{
				{TraceID: "t1", SpanID: "root", ParentSpanID: ""},
				{TraceID: "t2", SpanID: "child", ParentSpanID: "root"},
			},
			wantErr: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := ValidateChain(tc.spans)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateChain() err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// TestRunManyConcurrentTracesDoNotInterfere launches several independent
// traces concurrently from separate goroutines and confirms each one comes
// back internally consistent -- proving Run's state (the channel, the
// WaitGroup) is never shared across independent calls.
func TestRunManyConcurrentTracesDoNotInterfere(t *testing.T) {
	t.Parallel()

	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures []string

	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			root := &Node{
				ID:      "root",
				Service: "svc",
				Children: []*Node{
					{ID: "child-a", Service: "svc"},
					{ID: "child-b", Service: "svc"},
				},
			}
			spans := Run(root, "trace")
			if err := ValidateChain(spans); err != nil {
				mu.Lock()
				failures = append(failures, err.Error())
				mu.Unlock()
			}
			if len(spans) != 3 {
				mu.Lock()
				failures = append(failures, "wrong span count")
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(failures) != 0 {
		sort.Strings(failures)
		t.Fatalf("%d concurrent traces failed: %v", len(failures), failures)
	}
}
```

Verify:

```bash
go test -count=1 -race ./...
```

## Review

The propagation is correct when every span in a fanout arrives with a
`ParentSpanID` pointing to a span that is *also* in the collected set, no
matter what order the goroutines finished in — `TestRunCollectsEverySpanInTheFanout`
pins the parent links explicitly rather than trusting arrival order. The bug
this exercise guards against is a bare `break` in `Collect`'s `select`: it
would leave only the `select`, and the `for` would spin against an
already-closed, always-ready channel instead of ever returning. Assigning
`Node.ID` ahead of time rather than generating span IDs at runtime is the
design choice that makes the whole thing testable without pinning goroutine
scheduling order — only `Collect`'s *arrival* order is nondeterministic, and
every assertion in this exercise is written against the set of spans, their
parent links, or a sorted view, never against arrival order.

## Resources

- [W3C Trace Context](https://www.w3.org/TR/trace-context/) — the `traceparent` header format this exercise's TraceID/ParentSpanID model.
- [OpenTelemetry: Traces](https://opentelemetry.io/docs/concepts/signals/traces/) — spans, trace context, and how a real backend reconstructs a trace tree.
- [Go Specification: Select statements](https://go.dev/ref/spec#Select_statements) — a closed channel is always ready to receive.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [30-request-coalescing-singleflight.md](30-request-coalescing-singleflight.md) | Next: [32-mvcc-snapshot-isolation-reads.md](32-mvcc-snapshot-isolation-reads.md)
