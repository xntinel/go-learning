# Exercise 3: A Runnable Harness That Prints Pool Savings

A pool's win is invisible unless you surface it. This module builds the smallest
operator-facing harness: run a batch of buffer operations, then print the
`allocated / gets / puts` counters so a human (or a smoke test) can confirm reuse
actually happened. It is the minimal observable proof that the counters and the
allocation metric behave as designed.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
pooldemo/                   independent module: example.com/pooldemo
  go.mod                    go 1.26
  harness/
    pool.go                 the typed *bytes.Buffer pool (bundled)
    harness.go              Report(w, msgs) — runs the batch, writes results+stats
  cmd/
    demo/
      main.go               calls Report(os.Stdout, ...)
  harness/harness_test.go   asserts the batch output and the stats line
```

Files: `harness/pool.go`, `harness/harness.go`, `cmd/demo/main.go`, `harness/harness_test.go`.
Implement: `Report(w io.Writer, msgs []string)` that formats each message through the pool, writes each result line, then writes an `allocated=.. gets=.. puts=..` summary.
Test: capture `Report` output into a buffer and assert both the per-message lines and the stats line; the demo must build and run.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p pooldemo/harness pooldemo/cmd/demo
cd pooldemo
go mod init example.com/pooldemo
```

### Why write the harness against an io.Writer

The instinct is to make the harness `fmt.Println` straight to stdout. Writing to
an injected `io.Writer` instead costs nothing and makes the whole thing testable:
the demo passes `os.Stdout`, the test passes a `*bytes.Buffer` and asserts on the
captured bytes. This is the same discipline that makes any CLI testable — keep
the logic in a function that writes to an `io.Writer`, and let `main` be a
three-line adapter that supplies `os.Stdout`.

`Report` drives the pool the way a real hot path would: for each message it
`Get`s a buffer, formats into it, records the result, and `Put`s it back. Because
the batch runs on a single goroutine with no GC, the pool's per-P private slot
returns the same buffer every time, so `allocated` settles at exactly `1` while
`gets` and `puts` equal the message count. That is the "savings" the harness
makes visible: one allocation served an arbitrary number of operations.

Create `harness/pool.go`:

```go
package harness

import (
	"bytes"
	"sync"
	"sync/atomic"
)

// Pool is a type-safe wrapper over sync.Pool for *bytes.Buffer.
type Pool struct {
	p         sync.Pool
	allocated atomic.Int64
	gets      atomic.Int64
	puts      atomic.Int64
}

// New returns a Pool whose New allocates and counts a fresh *bytes.Buffer.
func New() *Pool {
	p := &Pool{}
	p.p.New = func() any {
		p.allocated.Add(1)
		return new(bytes.Buffer)
	}
	return p
}

// Get returns a reset buffer, allocating only if the pool is empty.
func (p *Pool) Get() *bytes.Buffer {
	p.gets.Add(1)
	buf := p.p.Get().(*bytes.Buffer)
	buf.Reset()
	return buf
}

// Put returns a buffer to the pool for reuse.
func (p *Pool) Put(buf *bytes.Buffer) {
	p.puts.Add(1)
	p.p.Put(buf)
}

// Stats reports the cumulative counters: allocated, gets, puts.
func (p *Pool) Stats() (allocated, gets, puts int64) {
	return p.allocated.Load(), p.gets.Load(), p.puts.Load()
}
```

Create `harness/harness.go`:

```go
package harness

import (
	"fmt"
	"io"
)

// Report runs a batch of formatting operations through a pool and writes an
// operator-facing view of the result to w: one line per message, then a summary
// of the pool counters. A single allocation serving many messages is the reuse
// the summary makes visible.
func Report(w io.Writer, msgs []string) {
	p := New()
	for _, m := range msgs {
		buf := p.Get()
		fmt.Fprintf(buf, "record:%s", m)
		fmt.Fprintln(w, buf.String())
		p.Put(buf)
	}
	allocated, gets, puts := p.Stats()
	fmt.Fprintf(w, "allocated=%d gets=%d puts=%d\n", allocated, gets, puts)
}
```

### The runnable demo

`main` is the thin adapter: it supplies `os.Stdout` and a fixed batch.

Create `cmd/demo/main.go`:

```go
package main

import (
	"os"

	"example.com/pooldemo/harness"
)

func main() {
	harness.Report(os.Stdout, []string{"alpha", "beta", "gamma"})
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
record:alpha
record:beta
record:gamma
allocated=1 gets=3 puts=3
```

### Tests

The test captures `Report` output into a `bytes.Buffer` and asserts both the
per-message lines and the stats summary, so the harness is verified rather than
merely run.

Create `harness/harness_test.go`:

```go
package harness

import (
	"bytes"
	"strings"
	"testing"
)

func TestReportOutput(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	Report(&out, []string{"alpha", "beta", "gamma"})
	got := out.String()

	wantLines := []string{
		"record:alpha",
		"record:beta",
		"record:gamma",
		"allocated=1 gets=3 puts=3",
	}
	for _, line := range wantLines {
		if !strings.Contains(got, line) {
			t.Errorf("Report output missing %q\ngot:\n%s", line, got)
		}
	}
}

func TestReportStatsLinePresent(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	Report(&out, []string{"one", "two"})
	got := out.String()

	if !strings.Contains(got, "gets=2 puts=2") {
		t.Fatalf("stats line wrong; got:\n%s", got)
	}
}

func TestReportEmptyBatch(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	Report(&out, nil)
	got := out.String()

	if !strings.Contains(got, "allocated=0 gets=0 puts=0") {
		t.Fatalf("empty batch should allocate nothing; got:\n%s", got)
	}
}
```

## Review

The harness is correct when the captured output contains one `record:` line per
input plus a stats line whose counters match the batch size. Writing to an
injected `io.Writer` rather than `os.Stdout` is the choice that makes this
testable at all — `TestReportOutput` reads the exact bytes the demo would print.
`TestReportEmptyBatch` pins a real edge: an empty batch does zero work and must
report `allocated=0`, which also proves `New`'s counter only fires when a buffer
is actually created. Run `go vet ./...` and `go test -race`; both must be clean.

## Resources

- [`io.Writer`](https://pkg.go.dev/io#Writer) — the single-method interface that makes CLI output testable.
- [`fmt.Fprintf`](https://pkg.go.dev/fmt#Fprintf) — formatted writes to any `io.Writer`.
- [`testing`](https://pkg.go.dev/testing) — capturing output into a `bytes.Buffer` for assertions.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-pooled-append-helper.md](02-pooled-append-helper.md) | Next: [04-contract-tests-and-benchmark.md](04-contract-tests-and-benchmark.md)
