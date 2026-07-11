# Exercise 7: A *[]byte Scratch-Slice Pool for Streaming Body Copies

Every reverse proxy, tee, and audit hook that streams a request body through
`io.CopyBuffer` needs a scratch slice — and pooling that slice is the single
most common place engineers hit the SA6002 trap, where storing `[]byte` by
value makes the pool allocate on every `Put`. This module builds the copier
the right way (`*[]byte`) and benchmarks the wrong way beside it so the boxing
cost stops being folklore and becomes a number.

This module is fully self-contained: its own `go mod init`, its own demo, its
own tests. Nothing here imports another exercise.

## What you'll build

```text
scratchcopy/                  independent module: example.com/scratchcopy
  go.mod                      go 1.26
  scratch/
    copy.go                   CopyWithScratch(dst, src); pointerPool of *[]byte; Allocated()
    copy_test.go              size table, concurrent copies, reuse range, Example
    copy_bench_test.go        BenchmarkPointerSlicePool vs BenchmarkValueSlicePool (SA6002)
  cmd/
    demo/
      main.go                 copies three payloads through one recycled slice
```

Files: `scratch/copy.go`, `scratch/copy_test.go`, `scratch/copy_bench_test.go`, `cmd/demo/main.go`.
Implement: `CopyWithScratch(dst io.Writer, src io.Reader) (int64, error)` that stages the copy through a pooled 32 KiB scratch slice stored as `*[]byte`, plus an `Allocated()` counter that proves reuse.
Test: table-driven sizes (empty, sub-buffer, exact multiple, several refills), 200 concurrent distinct-payload copies under `-race`, a reuse-range assertion, and the pointer-vs-value benchmark pair that pins the boxing allocation.
Verify: `go test -count=1 -race ./...` then `go test -bench=. -benchmem -run=^$ ./scratch`

Set up the module:

```bash
mkdir -p scratchcopy/scratch scratchcopy/cmd/demo
cd scratchcopy
go mod init example.com/scratchcopy
```

### Why *[]byte and not []byte: the boxing allocation

`sync.Pool.Put` takes `any`, and how Go converts your value into that `any` is
the whole game. A pointer fits directly in the interface's data word — putting
a `*[]byte` into the pool costs nothing. A `[]byte` does not fit: a slice is a
three-word header (pointer, length, capacity), so the runtime must heap-allocate
a copy of that header for the interface to point at. That is one allocation per
`Put`, forever, on the exact path you introduced the pool to de-allocate. The
pool still "works" — the backing 32 KiB array is genuinely reused — but you have
traded one large amortized allocation for an unbounded stream of small ones, and
small frequent allocations are precisely what drive GC frequency. `staticcheck`
flags this as [SA6002](https://staticcheck.dev/docs/checks/#SA6002); `go vet`
stays silent, so the mistake compiles, passes review, and ships.

The fix is mechanical: `New` returns `&b` where `b` is the slice, `Get` type-asserts
to `*[]byte`, the copy uses `*bp`, and `Put` returns the same pointer. The
pointer allocated once inside `New` shuttles between pool and caller for free
from then on.

### io.CopyBuffer's fast-path caveat, and why the tests wrap src and dst

`io.CopyBuffer(dst, src, buf)` has a documented escape hatch: "If either src
implements WriterTo or dst implements ReaderFrom, buf will not be used to
perform the copy." That is a feature in production — when the kernel can splice
or the destination can slurp the source directly, skipping the intermediate
buffer is strictly better — but it is a trap in tests and benchmarks, because
`bytes.Buffer` and `strings.Reader` both implement those fast-path interfaces.
Hand them to `CopyBuffer` naked and your pooled slice is never touched; the test
passes while exercising nothing.

Real proxy endpoints — a `net.Conn`, an `http.Request.Body` — do not implement
`WriterTo`/`ReaderFrom`, so the scratch slice is used on the real path. To make
the in-memory tests model that honestly, `CopyWithScratch` wraps both sides in
single-field structs (`onlyWriter`, `onlyReader`) whose embedded field is the
plain interface type. Method promotion through an embedded `io.Writer` promotes
only `Write`, so the wrappers structurally cannot satisfy `ReaderFrom` or
`WriterTo`, and `CopyBuffer` is forced through the buffer. This is the same
problem `net/http/httputil.ReverseProxy` solves with its `BufferPool` field —
the stdlib's own proxy stages body copies through exactly this kind of pooled
scratch slice.

### The lifecycle: Get, use, Put — and why the slice is safe to share serially

The scratch slice carries no state that outlives one call: `CopyBuffer`
overwrites it chunk by chunk and never reads beyond what it just wrote, so
unlike a `bytes.Buffer` there is nothing to reset. The only contract is
exclusivity in time — one goroutine uses the slice between `Get` and `Put`, and
`defer pointerPool.Put(bp)` guarantees the return on every exit path, including
an error from a broken source mid-copy. The `Allocated` counter (incremented
inside `New`) exists so both the demo and the tests can show reuse as a range —
many copies, few allocations — without ever asserting an exact count, which
per-P sharding makes nondeterministic.

Create `scratch/copy.go`:

```go
// Package scratch stages streaming copies through pooled scratch slices, the
// way a reverse proxy or audit tee copies request bodies without allocating
// a fresh 32 KiB buffer per request.
package scratch

import (
	"io"
	"sync"
	"sync/atomic"
)

// BufSize is the scratch-slice size: 32 KiB, matching io.Copy's own internal
// default, which is large enough to amortize syscall-shaped chunking and small
// enough to keep per-P pinned memory trivial.
const BufSize = 32 * 1024

// allocated counts how many scratch slices New has ever created. It is a
// package metric for observing reuse, never a value to assert exactly.
var allocated atomic.Int64

// pointerPool stores *[]byte, never []byte. The pointer fits directly in the
// pool's interface value, so Get and Put are allocation-free in steady state.
// Storing the slice by value would box its three-word header on every Put
// (staticcheck SA6002), allocating on the exact path the pool exists to save.
var pointerPool = sync.Pool{
	New: func() any {
		allocated.Add(1)
		b := make([]byte, BufSize)
		return &b
	},
}

// Allocated reports how many scratch slices have been created so far.
func Allocated() int64 { return allocated.Load() }

// onlyWriter hides any ReaderFrom the destination might implement: embedding
// the io.Writer interface promotes only Write, so io.CopyBuffer cannot take
// the fast path and must stage through the scratch slice.
type onlyWriter struct{ io.Writer }

// onlyReader hides any WriterTo the source might implement, for the same
// reason.
type onlyReader struct{ io.Reader }

// CopyWithScratch copies src to dst through a pooled BufSize scratch slice.
// The deferred Put returns the slice on every exit path, including a mid-copy
// error from a broken source.
func CopyWithScratch(dst io.Writer, src io.Reader) (int64, error) {
	bp := pointerPool.Get().(*[]byte)
	defer pointerPool.Put(bp)
	return io.CopyBuffer(onlyWriter{dst}, onlyReader{src}, *bp)
}
```

### The runnable demo

The demo copies three payloads — smaller than the buffer, exactly one buffer,
and several refills — and then prints the allocation counter: three copies,
one slice.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"fmt"
	"strings"

	"example.com/scratchcopy/scratch"
)

func main() {
	payloads := []string{
		strings.Repeat("a", 10),                   // far below one buffer
		strings.Repeat("b", scratch.BufSize),      // exactly one buffer
		strings.Repeat("c", 3*scratch.BufSize+17), // several refills
	}

	for i, p := range payloads {
		var dst bytes.Buffer
		n, err := scratch.CopyWithScratch(&dst, strings.NewReader(p))
		if err != nil {
			panic(err)
		}
		fmt.Printf("copy %d: copied=%d match=%t\n", i+1, n, dst.String() == p)
	}
	fmt.Printf("scratch slices allocated: %d\n", scratch.Allocated())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
copy 1: copied=10 match=true
copy 2: copied=32768 match=true
copy 3: copied=98321 match=true
scratch slices allocated: 1
```

### Tests

The size table covers the boundary cases a chunked copy can get wrong (empty
input, an exact buffer multiple, a refill with a remainder); the concurrent
test gives 200 goroutines distinct payloads so any accidental sharing of a
scratch slice between two in-flight copies shows up as corruption under
`-race`; and the reuse test asserts a range, never an exact count.

Create `scratch/copy_test.go`:

```go
package scratch

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestCopyWithScratchSizes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		size int
	}{
		{"empty", 0},
		{"small", 10},
		{"exactly one buffer", BufSize},
		{"one byte over", BufSize + 1},
		{"several refills with remainder", 3*BufSize + 17},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			src := strings.Repeat("x", tt.size)
			var dst bytes.Buffer
			n, err := CopyWithScratch(&dst, strings.NewReader(src))
			if err != nil {
				t.Fatalf("CopyWithScratch: %v", err)
			}
			if n != int64(tt.size) {
				t.Fatalf("copied %d bytes, want %d", n, tt.size)
			}
			if dst.String() != src {
				t.Fatalf("destination differs from source (len %d vs %d)", dst.Len(), len(src))
			}
		})
	}
}

func TestConcurrentCopiesNoCorruption(t *testing.T) {
	t.Parallel()

	const n = 200
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			payload := strings.Repeat(fmt.Sprintf("conn-%d-", i), 4096)
			var dst bytes.Buffer
			if _, err := CopyWithScratch(&dst, strings.NewReader(payload)); err != nil {
				t.Errorf("goroutine %d: %v", i, err)
				return
			}
			if dst.String() != payload {
				t.Errorf("goroutine %d: destination corrupted", i)
			}
		}()
	}
	wg.Wait()
}

func TestScratchSlicesAreReused(t *testing.T) {
	// Not parallel: it reads the package-level counter around a burst of
	// sequential copies. The assertion is a range, never an exact count —
	// per-P sharding makes exact allocation counts nondeterministic.
	const ops = 100
	before := Allocated()
	for range ops {
		var dst bytes.Buffer
		if _, err := CopyWithScratch(&dst, strings.NewReader("streamed body chunk")); err != nil {
			t.Fatalf("CopyWithScratch: %v", err)
		}
	}
	if grown := Allocated() - before; grown >= ops/2 {
		t.Fatalf("allocated %d new slices across %d sequential copies; pool reuse is broken", grown, ops)
	}
}

func ExampleCopyWithScratch() {
	var dst bytes.Buffer
	n, err := CopyWithScratch(&dst, strings.NewReader("audit this body"))
	if err != nil {
		panic(err)
	}
	fmt.Println(n, dst.String())
	// Output: 15 audit this body
}
```

### The benchmark pair: pointer pool vs value pool

The wrong variant lives in the benchmark file, clearly labeled: a pool whose
`New` returns `[]byte` by value, so every `Put` boxes the slice header. The two
benchmarks isolate the pure `Get`/`Put` round trip so the delta is exactly the
boxing cost: the pointer pool runs at 0 allocs/op in steady state, the value
pool pays one 24-byte allocation per operation — one slice header per `Put`,
forever.

Create `scratch/copy_bench_test.go`:

```go
package scratch

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

// valuePool deliberately stores []byte by value: the SA6002 pessimization.
// Every Put must box the three-word slice header into the pool's any, which
// heap-allocates. It exists here only to be measured against pointerPool.
var valuePool = sync.Pool{
	New: func() any { return make([]byte, BufSize) },
}

func BenchmarkPointerSlicePool(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		bp := pointerPool.Get().(*[]byte)
		(*bp)[0] = 1
		pointerPool.Put(bp)
	}
}

func BenchmarkValueSlicePool(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		buf := valuePool.Get().([]byte)
		buf[0] = 1
		valuePool.Put(buf) //lint:ignore SA6002 deliberate: this is the wrong variant under measurement
	}
}

func BenchmarkCopyWithScratch(b *testing.B) {
	payload := strings.Repeat("x", 64*1024)
	r := strings.NewReader(payload)
	var dst bytes.Buffer
	dst.Grow(len(payload))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		r.Reset(payload)
		dst.Reset()
		if _, err := CopyWithScratch(&dst, r); err != nil {
			b.Fatal(err)
		}
	}
}
```

Run the benchmarks:

```bash
go test -bench=. -benchmem -run=^$ ./scratch
```

Representative output (times vary by machine; the allocation columns are the
point):

```
BenchmarkPointerSlicePool-8    ...    0 B/op    0 allocs/op
BenchmarkValueSlicePool-8      ...   24 B/op    1 allocs/op
```

## Review

The copier is correct when every size in the table round-trips byte-for-byte,
including the exact-multiple and remainder cases where a chunked loop most
often miscounts. The two mistakes this module exists to burn in are both
silent: storing the slice by value compiles cleanly and only `staticcheck`
(SA6002) or the `-benchmem` column will ever tell you each `Put` allocates;
and passing a `bytes.Buffer` or `strings.Reader` straight into `io.CopyBuffer`
silently bypasses the scratch slice via the `WriterTo`/`ReaderFrom` fast path,
so a naive test can pass without the pool ever being exercised — the
`onlyWriter`/`onlyReader` wrappers are what make the tests honest. Confirm
correctness with `go test -count=1 -race ./...` and read the allocation delta
from `go test -bench=. -benchmem -run=^$ ./scratch`.

## Resources

- [`sync.Pool`](https://pkg.go.dev/sync#Pool) — the Get/Put/New contract and the pointer-type recommendation.
- [`io.CopyBuffer`](https://pkg.go.dev/io#CopyBuffer) — the buffer contract and the WriterTo/ReaderFrom fast-path escape hatch.
- [staticcheck SA6002](https://staticcheck.dev/docs/checks/#SA6002) — storing non-pointer values in a sync.Pool allocates.
- [`httputil.ReverseProxy`](https://pkg.go.dev/net/http/httputil#ReverseProxy) — the stdlib proxy's BufferPool field, this exact pattern in production.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-gzip-response-compression-middleware.md](06-gzip-response-compression-middleware.md) | Next: [08-capacity-capped-pool.md](08-capacity-capped-pool.md)
