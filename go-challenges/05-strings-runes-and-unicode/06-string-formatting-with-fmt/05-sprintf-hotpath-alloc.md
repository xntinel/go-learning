# Exercise 5: Killing Allocations in a Hot Logging Path

Every request through an HTTP middleware builds one access-log line. Built with
`fmt.Sprintf`, that line boxes five values into `interface{}` and allocates on the
heap on every request. This exercise builds the same line with `strconv.Append*`
into a reused buffer, proves it is byte-identical, and measures the allocation
difference.

This module is fully self-contained: its own `go mod init`, code, demo, and tests.

## What you'll build

```text
hotlog/                    independent module: example.com/hotlog
  go.mod                   go 1.24
  hotlog.go                type Record; FormatSprintf (baseline); AppendLine (optimized)
  cmd/
    demo/
      main.go              runnable demo: both builders produce the same line
  hotlog_test.go           byte-identical + AllocsPerRun budget + benchmark tests
```

- Files: `hotlog.go`, `cmd/demo/main.go`, `hotlog_test.go`.
- Implement: `Record` (method, path, status, bytes, duration ms); `FormatSprintf(Record) string` (the naive `fmt.Sprintf` baseline); `AppendLine(buf []byte, Record) []byte` writing into a caller-supplied buffer with `strconv.AppendInt`/`AppendQuote`.
- Test: byte-identical output between the two; `testing.AllocsPerRun` asserting the optimized path stays at or below a fixed budget while the baseline exceeds it; a benchmark; a table of inputs including a path that needs quoting.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/hotlog/cmd/demo
cd ~/go-exercises/hotlog
go mod init example.com/hotlog
go mod edit -go=1.24
```

### Why fmt.Sprintf allocates, and how the buffer avoids it

`fmt.Sprintf("... %s ... %d ...", r.Method, r.Status, ...)` has two costs. First,
each argument is passed through the `...any` parameter, which boxes it into an
`interface{}`; for a value that is not already behind a pointer (an `int`, a
`string` header copied into an interface), that boxing escapes to the heap.
Second, `fmt` then walks the format string and uses reflection to dispatch each
verb, which is slower than a direct conversion. For a cold path this is free money
you never miss. For a middleware on the request path at thousands of requests per
second, it is measurable CPU and a stream of garbage for the collector.

The optimized builder sidesteps both. `strconv.AppendInt(buf, n, 10)` converts an
integer and appends its digits directly onto `buf`, with no interface boxing and no
reflection. `strconv.AppendQuote(buf, s)` does the `%q` job — quoting and escaping
— straight onto the buffer. The static key fragments (`method=`, ` status=`) are
appended as string constants. The crucial move is the *buffer ownership*:
`AppendLine` takes a `[]byte` and returns the grown slice, so the caller can pass
`buf[:0]` of a buffer it keeps across calls (or one from a `sync.Pool`). Reusing
the buffer means the steady-state allocation count is zero: the backing array is
allocated once and truncated-and-refilled forever after.

Two honest caveats. First, the win only materializes if the buffer is actually
reused; `AppendLine(nil, r)` allocates a fresh buffer every call just like
`Sprintf`. The optimization is the *reuse*, not `Append*` by itself. Second, this
is worth doing only where a measurement shows it matters — the point of the test is
to *measure* with `testing.AllocsPerRun` and a benchmark, not to assume. Optimizing
a cold path this way trades readability for nothing.

Create `hotlog.go`:

```go
package hotlog

import (
	"fmt"
	"strconv"
)

// Record is one request's access-log data.
type Record struct {
	Method string
	Path   string
	Status int
	Bytes  int
	DurMs  int
}

// FormatSprintf is the naive baseline: one fmt.Sprintf, boxing every argument
// into an interface and using the reflection-based formatting path.
func FormatSprintf(r Record) string {
	return fmt.Sprintf("method=%s path=%q status=%d bytes=%d dur_ms=%d",
		r.Method, r.Path, r.Status, r.Bytes, r.DurMs)
}

// AppendLine writes the same line onto buf and returns the extended slice, with
// no interface boxing and no reflection. Pass buf[:0] of a reused buffer for a
// zero-allocation steady state.
func AppendLine(buf []byte, r Record) []byte {
	buf = append(buf, "method="...)
	buf = append(buf, r.Method...)
	buf = append(buf, " path="...)
	buf = strconv.AppendQuote(buf, r.Path)
	buf = append(buf, " status="...)
	buf = strconv.AppendInt(buf, int64(r.Status), 10)
	buf = append(buf, " bytes="...)
	buf = strconv.AppendInt(buf, int64(r.Bytes), 10)
	buf = append(buf, " dur_ms="...)
	buf = strconv.AppendInt(buf, int64(r.DurMs), 10)
	return buf
}
```

### The runnable demo

The demo builds one line each way and shows they are identical, then reuses a
single buffer across three records to model the steady state.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/hotlog"
)

func main() {
	r := hotlog.Record{Method: "GET", Path: "/api/users", Status: 200, Bytes: 1532, DurMs: 42}

	fmt.Println(hotlog.FormatSprintf(r))
	fmt.Println(string(hotlog.AppendLine(nil, r)))

	// Reused buffer across requests: allocated once, refilled each time.
	buf := make([]byte, 0, 256)
	for _, rec := range []hotlog.Record{
		{Method: "GET", Path: "/health", Status: 200, Bytes: 2, DurMs: 1},
		{Method: "POST", Path: "/api/orders", Status: 201, Bytes: 128, DurMs: 17},
	} {
		buf = hotlog.AppendLine(buf[:0], rec)
		fmt.Println(string(buf))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
method=GET path="/api/users" status=200 bytes=1532 dur_ms=42
method=GET path="/api/users" status=200 bytes=1532 dur_ms=42
method=GET path="/health" status=200 bytes=2 dur_ms=1
method=POST path="/api/orders" status=201 bytes=128 dur_ms=17
```

### Tests

`TestByteIdentical` is the correctness gate: the optimized builder must produce
exactly what the baseline produces, over a table that includes a path with a space
and a quote (so `AppendQuote` and `%q` are compared on a non-trivial input).
`TestAllocationBudget` uses `testing.AllocsPerRun`: the optimized path with a
reused buffer must stay at or below a small budget, while the baseline must exceed
it — this is the measurement that justifies the optimization. `BenchmarkFormat`
lets you see ns/op with `go test -bench`.

Create `hotlog_test.go`:

```go
package hotlog

import (
	"fmt"
	"testing"
)

func TestByteIdentical(t *testing.T) {
	t.Parallel()

	records := []Record{
		{Method: "GET", Path: "/api/users", Status: 200, Bytes: 1532, DurMs: 42},
		{Method: "POST", Path: "/api/orders?q=a b", Status: 201, Bytes: 0, DurMs: 3},
		{Method: "GET", Path: `/weird/"quoted"`, Status: 404, Bytes: 9, DurMs: 1},
		{Method: "DELETE", Path: "/", Status: 500, Bytes: -1, DurMs: 12345},
	}
	for _, r := range records {
		want := FormatSprintf(r)
		got := string(AppendLine(nil, r))
		if got != want {
			t.Fatalf("AppendLine = %q, want %q", got, want)
		}
	}
}

func TestAllocationBudget(t *testing.T) {
	// Not parallel: testing.AllocsPerRun must not run concurrently with other
	// tests, as it measures process-wide allocation counts.
	r := Record{Method: "GET", Path: "/api/users", Status: 200, Bytes: 1532, DurMs: 42}

	buf := make([]byte, 0, 256)
	optimized := testing.AllocsPerRun(1000, func() {
		buf = AppendLine(buf[:0], r)
	})
	if optimized > 1 {
		t.Fatalf("optimized path allocated %.1f/op, want <= 1", optimized)
	}

	baseline := testing.AllocsPerRun(1000, func() {
		_ = FormatSprintf(r)
	})
	if baseline < 2 {
		t.Fatalf("baseline allocated %.1f/op, expected it to exceed the budget", baseline)
	}
	if baseline <= optimized {
		t.Fatalf("baseline (%.1f) should allocate more than optimized (%.1f)", baseline, optimized)
	}
}

func BenchmarkFormatSprintf(b *testing.B) {
	r := Record{Method: "GET", Path: "/api/users", Status: 200, Bytes: 1532, DurMs: 42}
	b.ReportAllocs()
	for range b.N {
		_ = FormatSprintf(r)
	}
}

func BenchmarkAppendLine(b *testing.B) {
	r := Record{Method: "GET", Path: "/api/users", Status: 200, Bytes: 1532, DurMs: 42}
	buf := make([]byte, 0, 256)
	b.ReportAllocs()
	for range b.N {
		buf = AppendLine(buf[:0], r)
	}
}

func Example() {
	r := Record{Method: "GET", Path: "/health", Status: 200, Bytes: 2, DurMs: 1}
	fmt.Println(string(AppendLine(nil, r)))
	// Output: method=GET path="/health" status=200 bytes=2 dur_ms=1
}
```

Run the benchmark to see the difference:

```bash
go test -bench=. -benchmem -run=^$
```

Expected shape (numbers vary by machine): `BenchmarkFormatSprintf` reports several
allocs/op, `BenchmarkAppendLine` reports `0 allocs/op`.

## Review

The optimized builder is correct only if it is byte-identical to the baseline —
`TestByteIdentical` enforces that over quoted and edge inputs, so the optimization
never changes the log format. The value is proven, not assumed: `TestAllocationBudget`
shows the reused-buffer path holds at or below one allocation per call while the
`Sprintf` baseline exceeds it, and the benchmark shows the ns/op gap. The two traps
are (1) forgetting the reuse — `AppendLine(nil, r)` allocates a fresh buffer every
call and wins nothing, so the caller must keep and pass `buf[:0]`; and (2)
optimizing a path that is not hot, trading readable `Sprintf` for `Append*` where
no measurement justifies it. `strconv.AppendQuote` is doing the `%q` escaping, so
do not hand-roll quoting here either. Run `go test -race` to confirm; a per-request
buffer or a pooled one keeps this safe under concurrency.

## Resources

- [`strconv` Append functions](https://pkg.go.dev/strconv#AppendInt) — `AppendInt`, `AppendQuote` for zero-reflection formatting.
- [`fmt.Appendf`](https://pkg.go.dev/fmt#Appendf) — `fmt`-style formatting straight into a `[]byte`.
- [`testing.AllocsPerRun`](https://pkg.go.dev/testing#AllocsPerRun) — measuring allocations per call.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-tabwriter-ops-report.md](06-tabwriter-ops-report.md)
