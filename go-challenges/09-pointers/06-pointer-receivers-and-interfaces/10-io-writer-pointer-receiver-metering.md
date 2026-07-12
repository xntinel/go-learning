# Exercise 10: Response Size Metering: An io.Writer That Must Be a *T to Accumulate

Middleware that records how many bytes a response wrote wraps the
`http.ResponseWriter` in a counting `io.Writer`. That counter has to survive across
many `Write` calls, which forces a pointer receiver: a value receiver would discard
the running total every call. This module builds a `countingWriter`, meters output
from `fmt.Fprintf` and `io.Copy`, and proves the accumulation is race-free.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
metering/                   independent module: example.com/metering
  go.mod                    go 1.25
  writer.go                 type countingWriter (atomic.Int64); Write (ptr recv); Count
  cmd/
    demo/
      main.go               meter fmt.Fprintf + io.Copy into a buffer
  writer_test.go            byte count accuracy, io.Writer contract, -race concurrency
```

- Files: `writer.go`, `cmd/demo/main.go`, `writer_test.go`.
- Implement: a `countingWriter` wrapping an inner `io.Writer`, with `Write` on a pointer receiver accumulating total bytes in an `atomic.Int64`, plus `Count`.
- Test: `var _ io.Writer = (*countingWriter)(nil)`; write chunks and `io.Copy` a reader, assert the count equals total bytes and the inner buffer received them; a concurrency subtest sums parallel writes under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### Why the counter forces a pointer receiver

`countingWriter` wraps an inner `io.Writer` and keeps a running byte total. The
`io.Writer` contract is `Write(p []byte) (int, error)`, called repeatedly as data
streams through. For the total to accumulate, each `Write` must add to state that
outlives the call — so `Write` needs a **pointer** receiver. A value-receiver
`Write` would receive a copy of the `countingWriter`, add the chunk length to the
copy's counter, and throw the copy away when it returns; the real counter would
stay at zero no matter how much data flowed. On top of that, the counter is an
`atomic.Int64`, which must not be copied, so the pointer receiver is doubly
required.

Because `Write` has a pointer receiver, only `*countingWriter` satisfies
`io.Writer`. That is exactly what you want: you construct one `*countingWriter`,
hand it to `fmt.Fprintf`, `io.Copy`, or an `http` response path as an `io.Writer`,
and read `Count()` afterward. The compile-time contract
`var _ io.Writer = (*countingWriter)(nil)` documents that it is the pointer, not
the value, that is the writer.

The `atomic.Int64` also makes the meter safe when several goroutines write through
it concurrently (as long as the inner writer is itself safe). The concurrency test
uses `io.Discard` as the inner writer and asserts the summed count under `-race`.

Create `writer.go`:

```go
// writer.go
package metering

import (
	"io"
	"sync/atomic"
)

// countingWriter wraps an inner io.Writer and accumulates the total number of
// bytes written. Write has a pointer receiver so the count persists across calls
// and the atomic is never copied.
type countingWriter struct {
	inner io.Writer
	total atomic.Int64
}

// NewCountingWriter wraps w so writes through the result are metered.
func NewCountingWriter(w io.Writer) *countingWriter {
	return &countingWriter{inner: w}
}

// Write forwards p to the inner writer and adds the number of bytes actually
// written to the running total.
func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.inner.Write(p)
	c.total.Add(int64(n))
	return n, err
}

// Count returns the total bytes written so far.
func (c *countingWriter) Count() int64 {
	return c.total.Load()
}
```

### The runnable demo

The demo meters two common write paths — `fmt.Fprintf` and `io.Copy` from a
`strings.Reader` — into a single buffer, then prints the accumulated byte count and
confirms the buffer received the data.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"example.com/metering"
)

func main() {
	var buf bytes.Buffer
	cw := metering.NewCountingWriter(&buf)

	fmt.Fprintf(cw, "status=%d ", 200)     // 11 bytes
	io.Copy(cw, strings.NewReader("body")) // 4 bytes

	fmt.Printf("bytes written: %d\n", cw.Count())
	fmt.Printf("buffer holds:  %q\n", buf.String())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
bytes written: 15
buffer holds:  "status=200 body"
```

### Tests

`TestCountsBytes` writes several chunks and `io.Copy`s a reader, asserting both the
count and that the inner buffer received the exact bytes. `TestWriterContract`
records the compile-time satisfaction. `TestConcurrentWrites` fans out parallel
writes to a meter over `io.Discard` and asserts the total under `-race`, proving
the atomic accumulates correctly.

Create `writer_test.go`:

```go
// writer_test.go
package metering

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
)

func TestWriterContract(t *testing.T) {
	t.Parallel()
	var _ io.Writer = (*countingWriter)(nil)
}

func TestCountsBytes(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	cw := NewCountingWriter(&buf)

	if _, err := cw.Write([]byte("hello ")); err != nil { // 6
		t.Fatalf("Write: %v", err)
	}
	if _, err := fmt.Fprintf(cw, "world"); err != nil { // 5
		t.Fatalf("Fprintf: %v", err)
	}
	if _, err := io.Copy(cw, strings.NewReader("!")); err != nil { // 1
		t.Fatalf("Copy: %v", err)
	}

	if got := cw.Count(); got != 12 {
		t.Fatalf("Count() = %d, want 12", got)
	}
	if buf.String() != "hello world!" {
		t.Fatalf("inner buffer = %q, want %q", buf.String(), "hello world!")
	}
}

func TestConcurrentWrites(t *testing.T) {
	t.Parallel()

	cw := NewCountingWriter(io.Discard) // Discard is safe for concurrent Write
	const workers, size = 100, 8
	chunk := bytes.Repeat([]byte("x"), size)

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = cw.Write(chunk)
		}()
	}
	wg.Wait()

	if got := cw.Count(); got != workers*size {
		t.Fatalf("Count() = %d, want %d", got, workers*size)
	}
}

func ExampleNewCountingWriter() {
	cw := NewCountingWriter(io.Discard)
	fmt.Fprint(cw, "1234567890")
	fmt.Println(cw.Count())
	// Output: 10
}
```

## Review

The meter is correct when `Count()` equals the total bytes the inner writer
accepted, across every `Write`, `Fprintf`, and `Copy` path — which
`TestCountsBytes` pins by checking both the count and the buffer contents. The
pointer receiver on `Write` is the crux: swap it for a value receiver and
`TestCountsBytes` would report a count of zero, because each call would tally into
a discarded copy. `TestConcurrentWrites` under `-race` shows the `atomic.Int64`
aggregating parallel writes without a lock. This is the exact shape of a real
response-size middleware: wrap the writer once as `*countingWriter`, let the
handler stream through it, and read the byte total afterward for logging or
metrics.

## Resources

- [io.Writer](https://pkg.go.dev/io#Writer) — the one-method streaming interface `Write` satisfies.
- [io.Copy](https://pkg.go.dev/io#Copy) — streaming a reader into the metered writer.
- [sync/atomic: Int64](https://pkg.go.dev/sync/atomic#Int64) — the race-free accumulator behind the count.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-sort-interface-pointer-receiver.md](09-sort-interface-pointer-receiver.md) | Next: [../07-escape-analysis/00-concepts.md](../07-escape-analysis/00-concepts.md)
