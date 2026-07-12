# Exercise 3: Annotating the Workload so the Window Is Diagnosable

A captured trace window shows the scheduler, GC, and syscalls, but not what your
service was doing. This exercise instruments a multi-stage request pipeline —
parse, authorize, query, render — with one `trace.Task` per request, a
`trace.Region` per stage, and structured `trace.Log`/`Logf` breadcrumbs at
decision points, so the 3am snapshot is actually readable. The annotations are
guarded so they cost nothing when no trace is being collected.

This module is fully self-contained: its own `go mod init`, its own demo, and its
own tests. Nothing here imports another exercise.

## What you'll build

```text
pipeline/                  independent module: example.com/pipeline
  go.mod                   go 1.25
  pipeline.go              Handle: Task + four Regions + guarded Logf breadcrumbs
  cmd/
    demo/
      main.go              runnable demo: run three requests, print rendered bodies
  pipeline_test.go         behavior identical on/off, zero-alloc guarded log, sentinels
  pipeline_tracecheck_test.go  //go:build tracecheck: assert task/regions/logs in the window
```

- Files: `pipeline.go`, `cmd/demo/main.go`, `pipeline_test.go`, `pipeline_tracecheck_test.go`.
- Implement: `Handle(ctx, Request) (Result, error)` wrapping the request in a `trace.NewTask("request")`, each stage in `trace.WithRegion`, with `if trace.IsEnabled() { trace.Logf(...) }` breadcrumbs at auth and cache decisions.
- Test: the result is identical whether tracing is on (recorder armed) or off; the guarded `Logf` adds zero allocations when tracing is off; validation failures return wrapped sentinels asserted with `errors.Is`.
- Verify: `go test -count=1 -race ./...` (and, with the external reader, `go get golang.org/x/exp/trace && go test -tags tracecheck ./...`).

Set up the module:

```bash
go mod edit -go=1.25
```

### Task, regions, and why the context must be propagated

`trace.NewTask(ctx, "request")` returns a new context that *carries* the task, and
a `*Task` whose `End()` closes it. The returned context is the load-bearing part:
every region and log must be created from it (or a child of it) so the trace tool
can group the whole request — including work on goroutines it spawns — under one
task. Passing the original `ctx` instead of the returned one is the classic bug:
the regions still appear, but detached from the task, and the grouping that makes
the window readable is gone.

Each stage is wrapped in `trace.WithRegion(ctx, name, fn)`, which starts a region,
runs `fn`, and ends the region — the safe form, since it cannot leak an unbalanced
region the way a bare `StartRegion` without a matching `End` can. Regions nest
within a goroutine, so the four sequential stages produce four sibling regions
under the task: `parse`, `authorize`, `query`, `render`.

### The IsEnabled guard, precisely

The breadcrumbs use `trace.Logf`, whose signature is
`Logf(ctx, category, format string, args ...any)`. The variadic `args` are boxed
into an `[]any` *at the call site*, before `Logf` runs — and that boxing allocates
even if the log is ultimately discarded. Wrapping the call in
`if trace.IsEnabled() { trace.Logf(...) }` skips the boxing entirely when nothing
is recording, which is what makes the hot path allocation-free in the common case.
The guard must be at the call site, not inside a helper: a helper
`log(ctx, format, args...)` would still box the arguments before you could check
`IsEnabled` inside it.

Two honest caveats. First, `trace.NewTask` and `trace.WithRegion` themselves
allocate a small amount even when tracing is off (the task struct and the derived
context), so the *whole pipeline* is not zero-alloc; only the guarded `Logf`
breadcrumb is. Second, an armed `FlightRecorder` makes `trace.IsEnabled()` return
true, so in a black-box-always-armed service the guard's win is mainly in tests
and tooling-off builds. It stays because it is correct, documents intent, and
protects the genuinely-off case — which is exactly what the zero-alloc test below
pins down.

Create `pipeline.go`:

```go
// pipeline.go
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"runtime/trace"
	"strings"
)

// Sentinel errors, wrapped with %w at the return site so callers use errors.Is.
var (
	ErrEmptyQuery   = errors.New("pipeline: empty query")
	ErrUnauthorized = errors.New("pipeline: unauthorized")
)

// Request is one unit of work through the pipeline.
type Request struct {
	Token string
	Query string
}

// Result is the rendered outcome of a request.
type Result struct {
	User string
	Rows int
	Body string
}

// rows is a tiny read-only dataset. Read-only access is race-safe under -race.
var rows = map[string]int{
	"users":  3,
	"orders": 7,
}

// Handle runs a request through parse -> authorize -> query -> render, annotated
// for the execution trace. The annotations never change the returned result.
func Handle(ctx context.Context, req Request) (Result, error) {
	ctx, task := trace.NewTask(ctx, "request")
	defer task.End()

	var res Result

	var q string
	trace.WithRegion(ctx, "parse", func() {
		q = strings.TrimSpace(strings.ToLower(req.Query))
	})
	if q == "" {
		return Result{}, fmt.Errorf("parse: %w", ErrEmptyQuery)
	}

	var user string
	var authErr error
	trace.WithRegion(ctx, "authorize", func() {
		user, authErr = authorize(ctx, req.Token)
	})
	if authErr != nil {
		return Result{}, authErr
	}
	res.User = user

	trace.WithRegion(ctx, "query", func() {
		res.Rows = runQuery(ctx, q)
	})

	trace.WithRegion(ctx, "render", func() {
		res.Body = fmt.Sprintf("%s: %d rows for %q", user, res.Rows, q)
	})
	return res, nil
}

// authorize accepts tokens of the form "user:<name>" and logs the resolved user.
func authorize(ctx context.Context, token string) (string, error) {
	user, ok := strings.CutPrefix(token, "user:")
	if !ok || user == "" {
		return "", fmt.Errorf("authorize: %w", ErrUnauthorized)
	}
	if trace.IsEnabled() {
		trace.Logf(ctx, "auth", "user=%s", user)
	}
	return user, nil
}

// runQuery looks q up in the dataset, logging a cache hit/miss breadcrumb. A miss
// simulates a bounded retry and returns zero rows.
func runQuery(ctx context.Context, q string) int {
	n, hit := rows[q]
	if trace.IsEnabled() {
		trace.Logf(ctx, "cache", "query=%s hit=%v rows=%d", q, hit, n)
	}
	if !hit {
		const retries = 2
		if trace.IsEnabled() {
			trace.Logf(ctx, "cache", "miss retries=%d", retries)
		}
		return 0
	}
	return n
}
```

### The runnable demo

The demo runs three requests through the pipeline with tracing off (no recorder
armed) and prints the rendered bodies, showing a cache hit (`users`, `orders`)
and a miss (`widgets`). The annotations are present in the code but silent,
because nothing is recording.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"context"
	"fmt"

	"example.com/pipeline"
)

func main() {
	ctx := context.Background()
	reqs := []pipeline.Request{
		{Token: "user:alice", Query: "users"},
		{Token: "user:bob", Query: "orders"},
		{Token: "user:carol", Query: "widgets"},
	}
	for _, req := range reqs {
		res, err := pipeline.Handle(ctx, req)
		if err != nil {
			fmt.Println("error:", err)
			continue
		}
		fmt.Println(res.Body)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
alice: 3 rows for "users"
bob: 7 rows for "orders"
carol: 0 rows for "widgets"
```

### Tests

The tests prove three things. `TestHandle` is table-driven and checks results and
wrapped sentinel errors via `errors.Is`. `TestBehaviorIndependentOfTracing` runs
the same request with tracing off and then with a `FlightRecorder` armed
(`trace.IsEnabled()` true), asserting the result is byte-for-byte identical —
annotations must never change behavior. `TestGuardedLogZeroAllocWhenOff` uses
`testing.AllocsPerRun` to assert the guarded `Logf` pattern allocates zero times
when tracing is off, which is the whole justification for the guard.

`TestHandle` uses `t.Parallel()`; the arming test does not, because only one
flight recorder may be active process-wide. Parallel subtests pause until the
non-parallel tests finish, so the armed recorder never overlaps them.

Create `pipeline_test.go`:

```go
// pipeline_test.go
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"runtime/trace"
	"testing"
	"time"
)

func TestHandle(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		req     Request
		want    Result
		wantErr error
	}{
		{
			name: "cache hit",
			req:  Request{Token: "user:alice", Query: "users"},
			want: Result{User: "alice", Rows: 3, Body: `alice: 3 rows for "users"`},
		},
		{
			name: "cache miss",
			req:  Request{Token: "user:bob", Query: "widgets"},
			want: Result{User: "bob", Rows: 0, Body: `bob: 0 rows for "widgets"`},
		},
		{
			name:    "empty query",
			req:     Request{Token: "user:alice", Query: "   "},
			wantErr: ErrEmptyQuery,
		},
		{
			name:    "missing token",
			req:     Request{Token: "", Query: "users"},
			wantErr: ErrUnauthorized,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Handle(t.Context(), tc.req)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Handle err = %v; want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Handle = %+v; want %+v", got, tc.want)
			}
		})
	}
}

func TestBehaviorIndependentOfTracing(t *testing.T) {
	req := Request{Token: "user:alice", Query: "users"}

	off, err := Handle(context.Background(), req)
	if err != nil {
		t.Fatalf("tracing off: %v", err)
	}

	fr := trace.NewFlightRecorder(trace.FlightRecorderConfig{MinAge: time.Second})
	if err := fr.Start(); err != nil {
		t.Fatalf("start recorder: %v", err)
	}
	defer fr.Stop()
	if !trace.IsEnabled() {
		t.Fatal("trace.IsEnabled() false while a flight recorder is armed")
	}

	on, err := Handle(context.Background(), req)
	if err != nil {
		t.Fatalf("tracing on: %v", err)
	}
	if off != on {
		t.Fatalf("result changed with tracing on: off=%+v on=%+v", off, on)
	}
}

func TestGuardedLogZeroAllocWhenOff(t *testing.T) {
	if trace.IsEnabled() {
		t.Skip("a trace is active; the off-path allocation claim does not apply")
	}
	ctx := context.Background()
	got := testing.AllocsPerRun(1000, func() {
		if trace.IsEnabled() {
			trace.Logf(ctx, "cache", "query=%s hit=%v rows=%d", "users", true, 3)
		}
	})
	if got != 0 {
		t.Fatalf("guarded Logf allocated %v/op with tracing off; want 0", got)
	}
}

func ExampleHandle() {
	res, err := Handle(context.Background(), Request{Token: "user:alice", Query: "users"})
	fmt.Println(res.Body, err)
	// Output: alice: 3 rows for "users" <nil>
}
```

### Reading the annotations back out of the window

The payoff test lives behind the `tracecheck` build tag: arm a recorder, run the
pipeline, snapshot the window, and parse it with `golang.org/x/exp/trace` to
confirm the annotations are actually present. It counts the task type, the four
region types, and the log categories, asserting each expected value appears. This
is the automated version of opening the trace in `go tool trace` and checking
that the window tells the request's story.

Create `pipeline_tracecheck_test.go`:

```go
// pipeline_tracecheck_test.go
//go:build tracecheck

package pipeline

import (
	"bytes"
	"context"
	"io"
	"runtime/trace"
	"testing"
	"time"

	exptrace "golang.org/x/exp/trace"
)

func TestWindowIsAnnotated(t *testing.T) {
	fr := trace.NewFlightRecorder(trace.FlightRecorderConfig{MinAge: time.Second})
	if err := fr.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer fr.Stop()

	// A miss so both the "auth" and "cache" (hit + miss) breadcrumbs fire.
	if _, err := Handle(context.Background(), Request{Token: "user:alice", Query: "widgets"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	var buf bytes.Buffer
	if _, err := fr.WriteTo(&buf); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}

	r, err := exptrace.NewReader(&buf)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	tasks := map[string]bool{}
	regions := map[string]bool{}
	logs := map[string]bool{}
	for {
		ev, err := r.ReadEvent()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("ReadEvent: %v", err)
		}
		switch ev.Kind() {
		case exptrace.EventTaskBegin:
			tasks[ev.Task().Type] = true
		case exptrace.EventRegionBegin:
			regions[ev.Region().Type] = true
		case exptrace.EventLog:
			logs[ev.Log().Category] = true
		}
	}

	if !tasks["request"] {
		t.Errorf("task type %q not in window; got %v", "request", tasks)
	}
	for _, want := range []string{"parse", "authorize", "query", "render"} {
		if !regions[want] {
			t.Errorf("region %q not in window; got %v", want, regions)
		}
	}
	for _, want := range []string{"auth", "cache"} {
		if !logs[want] {
			t.Errorf("log category %q not in window; got %v", want, logs)
		}
	}
}
```

Run it:

```bash
go get golang.org/x/exp/trace
go test -tags tracecheck ./...
```

## Review

The instrumentation is correct when it is *invisible to behavior and visible in
the trace*. Invisible to behavior: `TestHandle` and `TestBehaviorIndependentOfTracing`
prove the result is identical whether or not a recorder is armed, so no annotation
smuggles logic into the hot path. Visible in the trace: the `tracecheck` test
proves the task, four regions, and log categories land in the captured window.
The mistakes to avoid are the annotation-specific ones. Propagate the context
`NewTask` returns — pass the *derived* `ctx` into every region and log, or the
regions detach from the task and the grouping is lost. Prefer `WithRegion` (or
`defer StartRegion(...).End()`) so a region cannot be left unbalanced. Put the
`IsEnabled` guard at the `Logf` call site, not inside a helper, so the variadic
arguments are never boxed when tracing is off — that is what
`TestGuardedLogZeroAllocWhenOff` verifies. Do not bother guarding a constant
`trace.Log(ctx, "cache", "hit")`: it has no arguments to box, so the guard buys
nothing. Confirm with `go test -count=1 -race ./...`; the read-only `rows` map
keeps the concurrent table test race-clean.

## Resources

- [`runtime/trace` tasks and regions](https://pkg.go.dev/runtime/trace#NewTask) — `NewTask`, `WithRegion`, `Log`, `Logf`, and `IsEnabled`.
- [`testing.AllocsPerRun`](https://pkg.go.dev/testing#AllocsPerRun) — measuring the zero-allocation guarded-log path.
- [`golang.org/x/exp/trace`](https://pkg.go.dev/golang.org/x/exp/trace) — `NewReader`, `ReadEvent`, and the `Event`/`Task`/`Region`/`Log` accessors used to read the window.
- [The Go execution tracer](https://go.dev/blog/execution-traces-2024) — how tasks, regions, and logs surface in `go tool trace`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-event-triggered-capture.md](02-event-triggered-capture.md) | Next: [../14-errors-astype-generic-matching/00-concepts.md](../14-errors-astype-generic-matching/00-concepts.md)
