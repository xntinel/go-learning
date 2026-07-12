# Exercise 9: Kill Allocations in a Hot Path (strings.Builder, sync.Pool)

`allocs/op` is the most stable regression signal a benchmark produces: it is
deterministic and machine-independent, so a new heap allocation in a hot path shows up
identically on a laptop and in CI. This module builds a large response line three
ways — naive `+=` concatenation, `strings.Builder` with `Grow`, and a `bytes.Buffer`
reused through a `sync.Pool` writing straight to the connection — and uses `-benchmem`
to prove the allocation reduction a senior engineer would enforce in review.

## What you'll build

```text
respbuild/                 independent module: example.com/respbuild
  go.mod                   go 1.24
  respbuild.go             BuildConcat, BuildBuilder (return string);
                           WriteLinePooled(io.Writer, n) (pooled bytes.Buffer, zero-alloc)
  cmd/
    demo/
      main.go              runnable demo: build with each, confirm identical
  respbuild_test.go        TestBuildIdentical (byte-identical across all three);
                           per-variant benchmarks with ReportAllocs; concurrency test for the pool; Example
```

- Files: `respbuild.go`, `cmd/demo/main.go`, `respbuild_test.go`.
- Implement: three producers of one large response line — concat, Builder+Grow, pooled `bytes.Buffer` to an `io.Writer`.
- Test: all three yield byte-identical output, plus allocation-reporting benchmarks and a `-race` pool test.
- Verify: `go test -count=1 -race ./...` then `go test -bench=. -benchmem`.

Set up the module:

```bash
go mod edit -go=1.24
```

### Three ways to emit a line, three allocation profiles

The task is to emit one line — a CSV-ish row of `n` fields. All three producers write
the identical bytes; they differ only in how much garbage they make.

`BuildConcat` uses `s += field` in a loop. Go strings are immutable, so every `+=`
allocates a brand-new backing array and copies the whole prefix into it: building an
`n`-field line allocates O(n) times and copies O(n^2) bytes in total. This is the
canonical hot-path allocation bug. `BuildBuilder` uses a `strings.Builder` with a single
`Grow(estimate)` call up front, so the backing array is allocated once and `WriteString`
appends into it; `Builder.String()` then returns that buffer without a final copy, so the
whole call is a single allocation regardless of field count. Note it formats each integer
with `strconv.AppendInt` into a stack scratch array rather than `strconv.Itoa` — `Itoa`
would allocate a string per number, and while Go elides that for values under 100, relying
on that coincidence is exactly the kind of fragile assumption a benchmark should not bake
in.

`WriteLinePooled` is the zero-allocation variant, and it earns that only by changing the
shape of the problem: instead of *returning a string* (which forces one allocation for the
result, always), it writes the bytes straight to an `io.Writer` — in production, the
`http.ResponseWriter` or the connection. It borrows a `*bytes.Buffer` from a `sync.Pool`,
resets it, builds into it, writes `buf.Bytes()` to the sink, and returns the buffer to the
pool. Two details make it truly zero-alloc in steady state. The pool holds a *pointer*
(`*bytes.Buffer`), not a `[]byte`: putting a bare `[]byte` into a `sync.Pool` boxes the
slice header into an interface and *allocates*, a notorious footgun — a pointer stored in
an interface does not. And the reused buffer's backing array survives across calls, so
after warm-up there is nothing left to allocate. That is the difference between "fewer
allocations" and "no allocations".

`-benchmem` is what turns this into evidence: concat reports a large `allocs/op` and
`B/op`, Builder reports exactly one allocation (the returned string), and
`WriteLinePooled` reports zero. Those numbers are deterministic, which is why a reviewer
can gate on them: "this change must not increase `allocs/op`" is enforceable in a way
"must not increase `ns/op`" is not.

Create `respbuild.go`:

```go
package respbuild

import (
	"bytes"
	"io"
	"strconv"
	"strings"
	"sync"
)

// BuildConcat builds the line with += concatenation: O(n) allocations, O(n^2) copying.
func BuildConcat(n int) string {
	s := ""
	for i := range n {
		if i > 0 {
			s += ","
		}
		s += "field" + strconv.Itoa(i)
	}
	return s
}

// BuildBuilder builds the line with a strings.Builder pre-sized by Grow: a single
// allocation (the returned string) regardless of n. It formats integers with
// AppendInt into a stack scratch to avoid per-number string allocation.
func BuildBuilder(n int) string {
	var b strings.Builder
	b.Grow(n * 8)
	var tmp [20]byte
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("field")
		b.Write(strconv.AppendInt(tmp[:0], int64(i), 10))
	}
	return b.String()
}

// bufPool holds *bytes.Buffer (a pointer, so Put does not box a slice and allocate).
var bufPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

// WriteLinePooled builds the line into a pooled bytes.Buffer and writes it to w,
// returning the buffer for reuse. In steady state it allocates nothing per call,
// because it returns no string and the reused buffer's backing array survives.
func WriteLinePooled(w io.Writer, n int) error {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)

	var tmp [20]byte
	for i := range n {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString("field")
		buf.Write(strconv.AppendInt(tmp[:0], int64(i), 10))
	}
	_, err := w.Write(buf.Bytes())
	return err
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"fmt"

	"example.com/respbuild"
)

func main() {
	const n = 4
	a := respbuild.BuildConcat(n)
	b := respbuild.BuildBuilder(n)

	var sink bytes.Buffer
	if err := respbuild.WriteLinePooled(&sink, n); err != nil {
		panic(err)
	}
	c := sink.String()

	fmt.Println(a)
	fmt.Printf("all identical: %v\n", a == b && b == c)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
field0,field1,field2,field3
all identical: true
```

### Tests

`TestBuildIdentical` builds the line at several sizes with all three producers and
asserts byte-for-byte equality — the correctness gate that lets the benchmarks compare
allocation profiles of interchangeable code. `TestPooledConcurrent` drives the pooled
writer from many goroutines under `-race`, each into its own buffer, proving the shared
pool is safe. The three benchmarks each call `b.ReportAllocs()`; the pooled one writes to
`io.Discard` so only the build-and-write cost is measured.

Create `respbuild_test.go`:

```go
package respbuild

import (
	"bytes"
	"fmt"
	"io"
	"sync"
	"testing"
)

func pooledString(n int) string {
	var buf bytes.Buffer
	if err := WriteLinePooled(&buf, n); err != nil {
		panic(err)
	}
	return buf.String()
}

func TestBuildIdentical(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, 1, 4, 100, 500} {
		concat := BuildConcat(n)
		builder := BuildBuilder(n)
		pooled := pooledString(n)
		if concat != builder {
			t.Errorf("n=%d: concat %q != builder %q", n, concat, builder)
		}
		if builder != pooled {
			t.Errorf("n=%d: builder %q != pooled %q", n, builder, pooled)
		}
	}
}

func TestPooledConcurrent(t *testing.T) {
	t.Parallel()
	want := BuildBuilder(50)
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := pooledString(50); got != want {
				t.Errorf("pooled = %q, want %q", got, want)
			}
		}()
	}
	wg.Wait()
}

func BenchmarkBuildConcat(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_ = BuildConcat(100)
	}
}

func BenchmarkBuildBuilder(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_ = BuildBuilder(100)
	}
}

func BenchmarkWriteLinePooled(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if err := WriteLinePooled(io.Discard, 100); err != nil {
			b.Fatal(err)
		}
	}
}

func ExampleBuildBuilder() {
	fmt.Println(BuildBuilder(3))
	// Output: field0,field1,field2
}
```

Run the benchmarks; read the `allocs/op` and `B/op` columns:

```bash
go test -bench=. -benchmem
```

```text
BenchmarkBuildConcat-8         189114     16481 ns/op    81875 B/op    199 allocs/op
BenchmarkBuildBuilder-8       1204831       741 ns/op      896 B/op      1 allocs/op
BenchmarkWriteLinePooled-8    2811203       470 ns/op        0 B/op      0 allocs/op
PASS
```

## Review

`TestBuildIdentical` makes the three producers interchangeable, so the only axis left
to compare is cost — and `-benchmem` makes it stark: concat reports about two hundred
allocations per call (roughly two per `+=`) and tens of kilobytes of `B/op`, the Builder
reports a single allocation for the returned string, and `WriteLinePooled` reports zero
because it returns no string and reuses its buffer. Those figures are the reviewable
artifact: `allocs/op` going from 199 to 1 to 0 is a real, machine-independent improvement
you can assert in CI, whereas the `ns/op` improvement, though real, would be the noisier
signal. The pooled path is the fastest and lowest-allocation here but carries the most
footguns — pool a *pointer* so `Put` does not box and allocate, reset before reuse, and
prove concurrency safety with `-race` — and it only reaches zero by writing to an
`io.Writer` instead of returning a string. The order of preference in review:
`strings.Builder` with `Grow` when you must return a string; a pooled `bytes.Buffer` to
an `io.Writer` when a benchmark proves the hot path cannot afford the result allocation.

## Resources

- [`strings.Builder`](https://pkg.go.dev/strings#Builder) — `Grow`, `WriteString`, and the no-copy `String`.
- [`sync.Pool`](https://pkg.go.dev/sync#Pool) — reusing buffers to cut per-call allocation; pool pointers, not slices.
- [`strconv.AppendInt`](https://pkg.go.dev/strconv#AppendInt) — appending an integer into a byte slice without an intermediate string.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-table-driven-benchmarks-scaling.md](08-table-driven-benchmarks-scaling.md) | Next: [10-ab-comparison-with-benchstat.md](10-ab-comparison-with-benchstat.md)
