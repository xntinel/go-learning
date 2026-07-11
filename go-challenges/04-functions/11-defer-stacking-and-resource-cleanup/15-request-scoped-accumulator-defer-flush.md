# Exercise 15: Request-Scoped Accumulator — Flush Metrics With Deferred Closure

**Nivel: Intermedio** — validacion rapida (un test corto).

A request handler often wants to record several timed steps — parse,
validate, query — and ship them somewhere (a tracer, a metrics backend) as
one batch when the request finishes, rather than one call per step. `defer`
makes that trivial: accumulate into a request-scoped value, and defer a
closure that reads the *final* accumulated state and flushes it once,
whatever the handler's exit path.

## What you'll build

```text
reqaccum/                    independent module: example.com/reqaccum
  go.mod
  reqaccum/reqaccum.go        Span, Accumulator, Sink; HandleRequest (defer flush)
  reqaccum/reqaccum_test.go   flush on success; flush of partial state on error
  cmd/demo/main.go            runnable demo: record three spans, watch the flush
```

- Files: `reqaccum/reqaccum.go`, `reqaccum/reqaccum_test.go`, `cmd/demo/main.go`.
- Implement: an `Accumulator` with `Record(name string, d time.Duration)` and `Spans() []Span`; a `Sink func([]Span)`; and `HandleRequest(sink Sink, work func(*Accumulator) error) (err error)` that builds a fresh `Accumulator`, defers a closure that calls `sink(acc.Spans())`, and then runs `work`.
- Test: a handler that records two spans and returns `nil` — assert the sink received both, in order; a handler that records one span and then returns an error — assert the sink still received that one span (the flush runs on every exit path, not just success).
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/reqaccum/reqaccum ~/go-exercises/reqaccum/cmd/demo
cd ~/go-exercises/reqaccum
go mod init example.com/reqaccum
go mod edit -go=1.24
```

### The flush has to read state that does not exist yet at defer time

The defining trick here is not the accumulation — appending to a slice is
ordinary — it is *when* the flush closure reads that slice. `defer sink(acc.Spans())`,
written literally, would evaluate `acc.Spans()` immediately, at the point
the `defer` statement runs, and flush whatever had been recorded so far
(nothing, if it runs before `work`). Wrapping the read in a closure —
`defer func() { sink(acc.Spans()) }()` — defers the *call* to `acc.Spans()`
itself, so it only happens when the deferred function actually executes,
after `work` has returned (or panicked). That is the same argument-evaluation
rule from `06-latency-timing-defer-and-arg-eval.md` applied to a accumulator
instead of a timestamp: arguments to a deferred call are evaluated at `defer`
time, but a deferred closure's *body* runs at unwind time and sees whatever
is true then.

Because the flush is unconditional — it runs whether `work` returns `nil` or
an error — a caller does not have to remember to flush on every branch of
its own logic. That is the same benefit `defer conn.Close()` gives a
connection pool borrower: one line, written once, correct on every exit path.

Create `reqaccum/reqaccum.go`:

```go
package reqaccum

import "time"

// Span is one recorded unit of work inside a request: a trace span, a timed
// step, or a metric sample. Millis is stored rather than time.Duration so
// flushed output does not depend on wall-clock formatting.
type Span struct {
	Name   string
	Millis int64
}

// Accumulator collects spans for the lifetime of a single request. It is not
// safe for concurrent use by multiple goroutines: it is request-scoped and
// meant to be built and read within one handler's call tree.
type Accumulator struct {
	spans []Span
}

// Record appends one span. Called any number of times, from any depth of the
// request's call tree, up until the handler returns.
func (a *Accumulator) Record(name string, d time.Duration) {
	a.spans = append(a.spans, Span{Name: name, Millis: d.Milliseconds()})
}

// Spans returns a defensive copy of everything recorded so far.
func (a *Accumulator) Spans() []Span {
	out := make([]Span, len(a.spans))
	copy(out, a.spans)
	return out
}

// Sink receives the final accumulated spans exactly once per request.
type Sink func(spans []Span)

// HandleRequest runs work against a fresh, request-scoped Accumulator, then
// flushes whatever was accumulated in a deferred closure. Because the flush
// reads acc.Spans() only when the deferred closure actually runs -- after
// work has returned or panicked -- it always sees the FINAL accumulated
// state, including spans recorded on work's very last line, regardless of
// whether work returned an error.
func HandleRequest(sink Sink, work func(*Accumulator) error) (err error) {
	acc := &Accumulator{}
	defer func() {
		sink(acc.Spans())
	}()
	return work(acc)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/reqaccum/reqaccum"
)

func main() {
	sink := func(spans []reqaccum.Span) {
		fmt.Println("flushed spans:")
		for _, s := range spans {
			fmt.Printf("  %s: %dms\n", s.Name, s.Millis)
		}
	}

	err := reqaccum.HandleRequest(sink, func(acc *reqaccum.Accumulator) error {
		acc.Record("parse", 2*time.Millisecond)
		acc.Record("validate", 1*time.Millisecond)
		acc.Record("db-query", 7*time.Millisecond)
		return nil
	})
	fmt.Println("handler error:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
flushed spans:
  parse: 2ms
  validate: 1ms
  db-query: 7ms
handler error: <nil>
```

### Tests

Create `reqaccum/reqaccum_test.go`:

```go
package reqaccum

import (
	"errors"
	"testing"
	"time"
)

func TestHandleRequestFlushesFinalStateOnSuccess(t *testing.T) {
	t.Parallel()

	var flushed []Span
	sink := func(spans []Span) { flushed = spans }

	err := HandleRequest(sink, func(acc *Accumulator) error {
		acc.Record("parse", 2*time.Millisecond)
		acc.Record("db-query", 5*time.Millisecond)
		return nil
	})
	if err != nil {
		t.Fatalf("HandleRequest err = %v, want nil", err)
	}

	want := []Span{{Name: "parse", Millis: 2}, {Name: "db-query", Millis: 5}}
	if len(flushed) != len(want) {
		t.Fatalf("flushed = %v, want %v", flushed, want)
	}
	for i := range want {
		if flushed[i] != want[i] {
			t.Fatalf("flushed[%d] = %v, want %v", i, flushed[i], want[i])
		}
	}
}

func TestHandleRequestFlushesWhateverWasRecordedOnError(t *testing.T) {
	t.Parallel()

	var flushed []Span
	sink := func(spans []Span) { flushed = spans }

	wantErr := errors.New("db unavailable")
	err := HandleRequest(sink, func(acc *Accumulator) error {
		acc.Record("parse", 1*time.Millisecond)
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}

	// The flush must still have happened, with the one span recorded before
	// the error was returned -- the deferred flush runs on every exit path.
	if len(flushed) != 1 || flushed[0].Name != "parse" {
		t.Fatalf("flushed = %v, want one span named parse", flushed)
	}
}
```

Verify:

```bash
go test -count=1 ./...
```

## Review

The pattern only works because the deferred closure calls `acc.Spans()` at
unwind time, not at `defer` time — the second test exists specifically to
catch a regression where someone "optimizes" the defer into
`defer sink(acc.Spans())` and silently starts flushing an empty (or stale)
slice instead of the real final state. Flushing unconditionally, on both the
success and error paths, is the other half of the guarantee: a caller who
only remembered to flush after a `nil` return would lose every partial trace
from a failed request, which is often the trace you most want to see.

## Resources

- [Go Language Specification: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover)
- [OpenTelemetry Go: Span](https://pkg.go.dev/go.opentelemetry.io/otel/trace#Span) — the real-world analog of the accumulated spans here.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [14-staged-write-discard-unless-committed.md](14-staged-write-discard-unless-committed.md) | Next: [16-savepoint-depth-nested-rollback.md](16-savepoint-depth-nested-rollback.md)
