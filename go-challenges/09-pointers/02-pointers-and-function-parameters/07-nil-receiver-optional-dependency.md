# Exercise 7: Optional Observability — A nil *Metrics Whose Methods No-Op Safely

Instrumentation is usually optional: in tests or lightweight deployments you want to
disable metrics without sprinkling `if m.metrics != nil` around every call site.
This exercise builds the idiomatic answer — a `*Metrics` whose pointer-receiver
methods check for a nil receiver and return early — so callers can inject nil to turn
instrumentation off, and it pins the exact boundary where a nil receiver panics.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
metrics/                    independent module: example.com/metrics
  go.mod
  metrics.go                Metrics (nil-safe Inc/Count); Server embedding *Metrics; unsafeCount (panics on nil)
  cmd/
    demo/
      main.go               a real sink and a nil sink used through the same calls
  metrics_test.go           nil is a no-op; real instance records; unguarded deref panics; embedding constructs
```

- Files: `metrics.go`, `cmd/demo/main.go`, `metrics_test.go`.
- Implement: `Metrics` with `Inc`/`Count` that begin with `if m == nil { return ... }`, a `Server` embedding `*Metrics`, and one method that dereferences without a guard to show the panic boundary.
- Test: every method on a nil `*Metrics` is a safe no-op, a non-nil instance records, the unguarded method panics on nil, and a `Server` with a nil `*Metrics` still constructs and runs.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/09-pointers/02-pointers-and-function-parameters/07-nil-receiver-optional-dependency/cmd/demo
cd go-solutions/09-pointers/02-pointers-and-function-parameters/07-nil-receiver-optional-dependency
```

### Why a nil receiver is callable

A method with a pointer receiver is really a function whose first argument is the
pointer. Calling `m.Inc("x")` where `m` is a nil `*Metrics` passes nil as that
argument — perfectly legal. The method runs; it only panics if it *dereferences* the
nil receiver (reads `m.counters`). So a method that begins `if m == nil { return }`
is a safe no-op on nil, and `Count` returning `0` on nil is a safe default. This is
the "optional dependency" pattern: a caller that wants instrumentation passes a real
`*Metrics`; a caller that does not passes nil, and every call site works unchanged
with zero nil-guards in the business logic.

The boundary matters, so this module makes it explicit. `unsafeCount` reads
`m.counters` with no guard; on a nil receiver that dereference panics with an
invalid-memory error. The rule that falls out: a pointer-receiver method may accept a
nil receiver *only if it does not touch a field before checking*. Embedding raises the
same point one level up — a `Server` that embeds `*Metrics` promotes `Inc`/`Count`, so
`Server{}` (with a nil embedded `*Metrics`) still calls the nil-safe methods and runs.

Create `metrics.go`:

```go
package metrics

import "sync/atomic"

// Metrics is an optional counter sink. A nil *Metrics is a valid receiver: its
// methods no-op, so callers can disable instrumentation by injecting nil.
type Metrics struct {
	counters map[string]*atomic.Int64
}

// New creates a sink with the named counters registered.
func New(names ...string) *Metrics {
	m := &Metrics{counters: make(map[string]*atomic.Int64)}
	for _, n := range names {
		m.counters[n] = new(atomic.Int64)
	}
	return m
}

// Inc increments a counter. On a nil receiver it is a safe no-op.
func (m *Metrics) Inc(name string) {
	if m == nil {
		return
	}
	if c, ok := m.counters[name]; ok {
		c.Add(1)
	}
}

// Count returns a counter's value, or 0 on a nil receiver or unknown name.
func (m *Metrics) Count(name string) int64 {
	if m == nil {
		return 0
	}
	if c, ok := m.counters[name]; ok {
		return c.Load()
	}
	return 0
}

// unsafeCount dereferences the receiver WITHOUT a nil guard, to document the
// boundary: calling it on a nil *Metrics panics.
func (m *Metrics) unsafeCount(name string) int64 {
	return m.counters[name].Load()
}

// Server embeds *Metrics so Inc/Count are promoted. A zero Server has a nil
// embedded *Metrics, which still works because the methods are nil-safe.
type Server struct {
	*Metrics
}
```

### The runnable demo

The demo runs the same calls through a real sink and a nil sink; the nil sink turns
every call into a no-op with no special-casing at the call site.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/metrics"
)

func handle(m *metrics.Metrics) {
	m.Inc("requests") // works whether m is real or nil
}

func main() {
	real := metrics.New("requests")
	handle(real)
	handle(real)
	fmt.Printf("real sink requests=%d\n", real.Count("requests"))

	var disabled *metrics.Metrics // nil: instrumentation off
	handle(disabled)              // safe no-op
	fmt.Printf("nil sink  requests=%d\n", disabled.Count("requests"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
real sink requests=2
nil sink  requests=0
```

### Tests

Create `metrics_test.go`:

```go
package metrics

import "testing"

func TestNilReceiverIsNoOp(t *testing.T) {
	t.Parallel()
	var m *Metrics // nil
	// None of these may panic.
	m.Inc("requests")
	m.Inc("requests")
	if got := m.Count("requests"); got != 0 {
		t.Fatalf("nil Count = %d, want 0", got)
	}
}

func TestNonNilRecords(t *testing.T) {
	t.Parallel()
	m := New("requests")
	m.Inc("requests")
	m.Inc("requests")
	m.Inc("requests")
	if got := m.Count("requests"); got != 3 {
		t.Fatalf("Count = %d, want 3", got)
	}
	// Unknown counter is a safe zero, not a panic.
	if got := m.Count("unknown"); got != 0 {
		t.Fatalf("unknown Count = %d, want 0", got)
	}
}

func TestUnsafeCountPanicsOnNil(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("unsafeCount on nil *Metrics should panic")
		}
	}()
	var m *Metrics
	_ = m.unsafeCount("requests") // dereferences nil -> panic
}

func TestEmbeddedNilMetricsConstructsAndRuns(t *testing.T) {
	t.Parallel()
	var s Server // embedded *Metrics is nil
	s.Inc("requests")
	if got := s.Count("requests"); got != 0 {
		t.Fatalf("embedded nil Count = %d, want 0", got)
	}

	s2 := Server{Metrics: New("requests")}
	s2.Inc("requests")
	if got := s2.Count("requests"); got != 1 {
		t.Fatalf("embedded real Count = %d, want 1", got)
	}
}
```

## Review

The pattern is correct when a nil `*Metrics` is genuinely interchangeable with a
real one at every call site — which is why `handle` in the demo takes a `*Metrics`
and never checks it for nil. The two failure boundaries are pinned by tests:
`TestNilReceiverIsNoOp` proves the guarded methods survive nil, and
`TestUnsafeCountPanicsOnNil` proves an *unguarded* dereference does not — so the rule
"guard nil before touching a field" is executable, not just advice. Embedding
extends the pattern: a `Server` with a nil embedded `*Metrics` still constructs and
runs because the promoted methods are nil-safe. The trap to avoid is adding a new
method later that reads `m.counters` without the guard; it will pass every test until
someone injects nil in production. Run `go test -race`; the counters are
`atomic.Int64`, so concurrent `Inc` is safe.

## Resources

- [Go spec: Method values](https://go.dev/ref/spec#Method_values) — the receiver `x` in a method value `x.M` is evaluated and saved as the receiver used in later calls; a nil pointer is a valid saved receiver, so the method runs and panics only if it dereferences it.
- [Go Spec: Method sets and calls](https://go.dev/ref/spec#Method_sets) — how a receiver is passed as the first argument.
- [`sync/atomic` Int64](https://pkg.go.dev/sync/atomic#Int64) — the atomic counter used so `Inc` is concurrency-safe.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-atomic-pointer-config-hot-reload.md](06-atomic-pointer-config-hot-reload.md) | Next: [08-sync-pool-buffer-pointers.md](08-sync-pool-buffer-pointers.md)
