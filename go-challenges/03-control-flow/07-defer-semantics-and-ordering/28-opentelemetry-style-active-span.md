# Exercise 28: Distributed Tracing — Deferred Span Close with Parent-Child Nesting

**Nivel: Intermedio** — validacion rapida (un test corto).

Every real APM instrumentation library — OpenTelemetry included — is
built around the same shape: `span := tracer.StartSpan(name)` followed
immediately by `defer span.End()`, with any function that calls a
traced sub-operation opening its own child span the same way. The
resulting tree of parent-child spans is what a trace viewer draws as a
waterfall, and its accuracy depends entirely on `End` reading the clock
and the function's outcome at the *true* end of the traced operation —
not at the moment `defer` was written. This module builds a minimal
version of that tracer: nested spans link to their parent automatically,
and each span's duration and error are captured by a deferred closure
that reads the clock and a named return value at the right instant. The
module is fully self-contained: its own `go mod init`, all code inline,
its own demo and tests.

## What you'll build

```text
tracer/                      independent module: example.com/opentelemetry-style-active-span
  go.mod                      go 1.24
  tracer.go                    Span, Tracer (StartSpan, Finished), Span.End, Span.SetAttr
  cmd/
    demo/
      main.go                 runnable demo: 3 nested spans, one child records an error
  tracer_test.go               duration/error capture table; nested parent-linking case
```

- Files: `tracer.go`, `cmd/demo/main.go`, `tracer_test.go`.
- Implement: `Tracer` (`Now`, `StartSpan`, `Finished`) and `Span` (`SetAttr`, `End`).
- Test: a table over successful/failed spans checking duration and captured error, plus a case proving parent linking and finish-order.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why callers write `defer func() { span.End(err) }()`, never `defer span.End(err)`

`End`'s job is to stamp `Duration` using the clock reading *at the
moment the traced operation actually finishes* and to record whatever
error that operation produced. Both of those values are only known at
the very end of the calling function, which is exactly when a `defer`
fires — but only if the call is wrapped in a closure. `defer
span.End(err)`, written as a plain argument-form defer, evaluates `err`
immediately, at the `defer` statement itself: right after `StartSpan`,
before the function has done any of its actual work, `err` is still its
zero value, `nil`. The deferred call would then always report success,
regardless of what happens next. The closure form, `defer func() {
span.End(err) }()`, defers the *read* of `err` along with the call, so
by the time it executes — at the function's real return, after `err`
has been assigned whatever the function's named return value ends up
holding — it reports the truth. The same reasoning applies to the
tracer's clock: `Span.End` calls `s.tracer.Now()` inside its own body,
not as an argument computed by its caller, so the clock is always read
at the instant the span actually ends.

Create `tracer.go`:

```go
package tracer

import (
	"fmt"
	"time"
)

// Span represents one traced operation. Spans form a tree via parent
// pointers; a child's Start is nested inside its parent's still-open
// duration.
type Span struct {
	Name     string
	Parent   *Span
	Start    time.Time
	Duration time.Duration
	Attrs    map[string]string
	Err      error

	tracer *Tracer
}

// Tracer owns the clock (injectable for deterministic tests/demos) and the
// finished span log.
type Tracer struct {
	Now      func() time.Time
	finished []*Span
}

// StartSpan begins a new span, nested under parent if non-nil.
func (t *Tracer) StartSpan(name string, parent *Span) *Span {
	return &Span{Name: name, Parent: parent, Start: t.Now(), Attrs: map[string]string{}, tracer: t}
}

// SetAttr records a key/value attribute on the span.
func (s *Span) SetAttr(key, value string) {
	s.Attrs[key] = value
}

// End is meant to be deferred by the caller right after StartSpan. It
// stamps the span's duration using the tracer's clock read at return time,
// captures any error via a named-return closure at the call site, and
// appends the finished span to the tracer's log. Because Go evaluates a
// defer's arguments immediately, `defer span.End(err)` would freeze err's
// zero value from the moment the defer statement ran; callers instead
// write `defer func() { span.End(err) }()` so err's final value -- set by
// the time the surrounding function actually returns -- is what gets
// recorded.
func (s *Span) End(err error) {
	s.Duration = s.tracer.Now().Sub(s.Start)
	s.Err = err
	s.tracer.finished = append(s.tracer.finished, s)
}

// Finished returns every span that has been ended so far, in end order.
func (t *Tracer) Finished() []*Span {
	out := make([]*Span, len(t.finished))
	copy(out, t.finished)
	return out
}

func (s *Span) String() string {
	parent := "root"
	if s.Parent != nil {
		parent = s.Parent.Name
	}
	status := "ok"
	if s.Err != nil {
		status = fmt.Sprintf("error(%v)", s.Err)
	}
	return fmt.Sprintf("%s (parent=%s, duration=%s, status=%s, attrs=%v)", s.Name, parent, s.Duration, status, s.Attrs)
}
```

### The runnable demo

A fake clock advances by a fixed 10ms on every call, so every duration
below is exactly reproducible. `handleRequest` opens a root span and
calls `dbQuery`, which opens a child span and calls `rowScan`, which
opens a grandchild span, fails, and reports that failure through its own
named-return `err` into `span.End(err)`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	tracer "example.com/opentelemetry-style-active-span"
)

func newFakeClock() func() time.Time {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	n := 0
	return func() time.Time {
		t := base.Add(time.Duration(n) * 10 * time.Millisecond)
		n++
		return t
	}
}

func handleRequest(tr *tracer.Tracer) {
	root := tr.StartSpan("handle-request", nil)
	root.SetAttr("http.method", "GET")
	defer func() { root.End(nil) }()

	dbQuery(tr, root)
}

func dbQuery(tr *tracer.Tracer, parent *tracer.Span) {
	span := tr.StartSpan("db-query", parent)
	span.SetAttr("db.statement", "SELECT * FROM orders")
	defer func() { span.End(nil) }()

	rowScan(tr, span)
}

func rowScan(tr *tracer.Tracer, parent *tracer.Span) (err error) {
	span := tr.StartSpan("row-scan", parent)
	defer func() { span.End(err) }()

	err = fmt.Errorf("row 12 malformed")
	return err
}

func main() {
	tr := &tracer.Tracer{Now: newFakeClock()}
	handleRequest(tr)

	for _, s := range tr.Finished() {
		fmt.Println(s)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
row-scan (parent=db-query, duration=10ms, status=error(row 12 malformed), attrs=map[])
db-query (parent=handle-request, duration=30ms, status=ok, attrs=map[db.statement:SELECT * FROM orders])
handle-request (parent=root, duration=50ms, status=ok, attrs=map[http.method:GET])
```

Spans finish innermost-first — `row-scan`, then `db-query`, then
`handle-request` — because each one's deferred `End` fires when *its own*
function returns, and the grandchild's function returns first.

### Tests

`TestSpanEndComputesDurationAndCapturesError` drives `End` with a fixed
clock sequence and checks both the computed duration and the captured
error for a successful and a failed span. `TestNestedSpansLinkParentAndAppendInEndOrder`
proves a child's `Parent` pointer is set correctly and that ending a
child before its parent produces exactly that order in `Finished`.

Create `tracer_test.go`:

```go
package tracer

import (
	"errors"
	"testing"
	"time"
)

var errBoom = errors.New("boom")

func fixedClockSeq(times ...time.Time) func() time.Time {
	i := 0
	return func() time.Time {
		t := times[i]
		i++
		return t
	}
}

func TestSpanEndComputesDurationAndCapturesError(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name         string
		startAt      time.Time
		endAt        time.Time
		err          error
		wantDuration time.Duration
	}{
		{
			name:         "successful span",
			startAt:      base,
			endAt:        base.Add(100 * time.Millisecond),
			err:          nil,
			wantDuration: 100 * time.Millisecond,
		},
		{
			name:         "failed span still records duration",
			startAt:      base,
			endAt:        base.Add(5 * time.Second),
			err:          errBoom,
			wantDuration: 5 * time.Second,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := &Tracer{Now: fixedClockSeq(tc.startAt, tc.endAt)}
			span := tr.StartSpan("op", nil)
			span.End(tc.err)

			if span.Duration != tc.wantDuration {
				t.Errorf("duration = %v, want %v", span.Duration, tc.wantDuration)
			}
			if span.Err != tc.err {
				t.Errorf("err = %v, want %v", span.Err, tc.err)
			}
		})
	}
}

func TestNestedSpansLinkParentAndAppendInEndOrder(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	tr := &Tracer{Now: fixedClockSeq(
		base,
		base.Add(1*time.Millisecond),
		base.Add(2*time.Millisecond),
		base.Add(3*time.Millisecond),
	)}

	parent := tr.StartSpan("parent", nil)
	child := tr.StartSpan("child", parent)
	child.End(nil)
	parent.End(nil)

	if child.Parent != parent {
		t.Fatalf("child.Parent = %v, want %v", child.Parent, parent)
	}

	finished := tr.Finished()
	if len(finished) != 2 {
		t.Fatalf("finished = %d spans, want 2", len(finished))
	}
	if finished[0].Name != "child" || finished[1].Name != "parent" {
		t.Fatalf("finished order = [%s %s], want [child parent]", finished[0].Name, finished[1].Name)
	}
}
```

Verify: `go test -count=1 ./...`

## Review

The tracer is correct when every span's `Duration` reflects the clock
read at its true end, and every span's `Err` reflects the function's
actual final outcome, not whatever those values happened to be the
moment `StartSpan` returned. The closure form of the deferred `End` call
is what makes both true. The mistake this design avoids is the plain
argument-form defer, `defer span.End(err)`, written directly after
`StartSpan` — it would compile without complaint and would silently
report every span as successful, because `err` at that point in the
function has not been assigned anything yet.

## Resources

- [Go Specification: Defer statements](https://go.dev/ref/spec#Defer_statements) — arguments to a deferred call are evaluated when the defer statement runs, not when the call executes.
- [OpenTelemetry: Tracing API specification](https://opentelemetry.io/docs/specs/otel/trace/api/) — the `StartSpan`/`End`, parent-child, and attribute model this exercise mirrors.
- [pkg.go.dev: go.opentelemetry.io/otel/trace](https://pkg.go.dev/go.opentelemetry.io/otel/trace) — the production Go tracing API this module is a teaching-sized model of.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [27-streaming-etl-pipeline-stage-cleanup.md](27-streaming-etl-pipeline-stage-cleanup.md) | Next: [29-circuit-breaker-half-open-probe.md](29-circuit-breaker-half-open-probe.md)
