# Exercise 3: Pooled bytes.Buffer Response Builder for an HTTP Handler

A JSON API serializes thousands of small responses per second. Allocating a fresh
`bytes.Buffer` per request puts steady pressure on the garbage collector. This
exercise builds a response builder that borrows a `*bytes.Buffer` from a
`sync.Pool`, writes the body, copies it out, then resets and returns the buffer —
with a capacity guard that drops oversized buffers instead of pinning multi-megabyte
arrays in the pool forever.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
resppool/                   independent module: example.com/resppool
  go.mod                    go 1.25
  resppool.go               Pool wrapping sync.Pool; Get, Put(cap-guarded), Build, WriteResponse
  cmd/
    demo/
      main.go               serves a handler and prints a response
  resppool_test.go          concurrent correctness, cap-guard drop, alloc benchmark
```

- Files: `resppool.go`, `cmd/demo/main.go`, `resppool_test.go`.
- Implement: a `Pool` over `sync.Pool` of `*bytes.Buffer`, a `Build` that borrows a buffer, runs a caller's write function, copies the bytes out, and returns the buffer via a cap-guarded `Put`; plus an `http.Handler` that uses it.
- Test: many goroutines building responses concurrently with no cross-contamination; a grown-beyond-limit buffer is NOT returned to the pool (observed via a wrapped counter); a benchmark showing fewer allocs/op than a fresh buffer per call.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/resppool/cmd/demo
cd ~/go-exercises/resppool
go mod init example.com/resppool
go mod edit -go=1.25
```

### Why pool, why Reset, why a cap guard

A `bytes.Buffer` is the mutable, append-friendly type for building a response body;
its cost is the backing array it allocates as it grows. When a handler builds one
buffer per request and throws it away, that array is garbage every request. A
`sync.Pool` amortizes it: borrow a buffer, use it, return it, and the next request
reuses the same array instead of allocating a new one.

Two disciplines make pooling correct rather than a source of bugs. First, `Reset`:
a borrowed buffer must be empty, or one request's leftover bytes leak into another's
response — a cross-contamination bug that is also a data-exposure bug. This builder
`Reset`s on return (and the pool's `New` yields an empty buffer), so every `Get`
hands out a clean buffer. Second, a capacity guard: a `sync.Pool` has no size limit,
so a buffer that grew to hold one 5 MB payload will, if returned, keep that 5 MB
array alive in the pool indefinitely. Under load the pool fills with such giants and
your steady-state memory tracks the worst-case response, not the common one. The
guard is one line — if the buffer's capacity exceeds a threshold, drop it (let it be
collected) instead of `Put`-ing it — so the pool holds only right-sized buffers.

`Build` encapsulates the whole borrow/write/copy/return dance so callers cannot
forget a step. It borrows a buffer, runs the caller's `write` function against it,
copies the accumulated bytes into a freshly-owned slice (because `buf.Bytes()`
aliases the buffer, which is about to be reset and reused — returning that alias
would be a use-after-reset bug), returns the buffer to the pool, and hands the owned
copy back. The copy is the ownership boundary: the caller gets bytes it owns, the
pool gets its buffer back clean.

Create `resppool.go`:

```go
package resppool

import (
	"bytes"
	"net/http"
	"sync"
	"sync/atomic"
)

// maxPooledCap is the largest buffer capacity worth keeping. Buffers that grew
// beyond it are dropped on Put so the pool never pins an outlier's storage.
const maxPooledCap = 64 << 10 // 64 KiB

// Pool is a sync.Pool of *bytes.Buffer with a Reset-on-return and a capacity
// guard. dropped counts buffers discarded for exceeding maxPooledCap (for tests).
type Pool struct {
	pool    sync.Pool
	dropped atomic.Int64
}

// NewPool returns a ready Pool.
func NewPool() *Pool {
	return &Pool{
		pool: sync.Pool{
			New: func() any { return new(bytes.Buffer) },
		},
	}
}

// Get borrows an empty buffer.
func (p *Pool) Get() *bytes.Buffer {
	b := p.pool.Get().(*bytes.Buffer)
	b.Reset()
	return b
}

// Put returns buf to the pool unless it grew past maxPooledCap, in which case it
// is dropped so the pool does not pin the outsized array.
func (p *Pool) Put(buf *bytes.Buffer) {
	if buf.Cap() > maxPooledCap {
		p.dropped.Add(1)
		return
	}
	buf.Reset()
	p.pool.Put(buf)
}

// Dropped reports how many buffers the cap guard has discarded.
func (p *Pool) Dropped() int64 { return p.dropped.Load() }

// Build borrows a buffer, runs write against it, copies the result into an owned
// slice, and returns the buffer to the pool. The returned bytes are safe to keep;
// the buffer's own storage is not (it is reset and reused).
func (p *Pool) Build(write func(buf *bytes.Buffer)) []byte {
	buf := p.Get()
	write(buf)
	out := bytes.Clone(buf.Bytes())
	p.Put(buf)
	return out
}

// Handler builds a response body via the pool and writes it to w.
func (p *Pool) Handler(write func(buf *bytes.Buffer, r *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		buf := p.Get()
		write(buf, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(buf.Bytes())
		p.Put(buf)
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"

	"example.com/resppool"
)

func main() {
	p := resppool.NewPool()

	h := p.Handler(func(buf *bytes.Buffer, r *http.Request) {
		buf.WriteString(`{"path":`)
		buf.Write(strconv.AppendQuote(nil, r.URL.Path))
		buf.WriteString(`,"ok":true}`)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/orders/42", nil)
	h.ServeHTTP(rec, req)

	fmt.Printf("status=%d\n", rec.Code)
	fmt.Printf("body=%s\n", rec.Body.String())

	body := p.Build(func(buf *bytes.Buffer) {
		buf.WriteString("id=")
		buf.Write(strconv.AppendInt(nil, 7, 10))
	})
	fmt.Printf("built=%s\n", body)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
status=200
body={"path":"/orders/42","ok":true}
built=id=7
```

### Tests

`TestConcurrentNoContamination` launches many goroutines, each building a response
with a distinct payload, and asserts every result matches its own input — if `Reset`
were missing or the alias were returned, borrowers would see each other's bytes.
`TestCapGuardDropsLargeBuffer` proves the guard: it puts a buffer grown past the
limit and checks `Dropped` incremented, and puts a small one and checks it did not.
`BenchmarkPooledVsFresh` contrasts the pooled builder with allocating a fresh buffer
per call under `-benchmem`.

Create `resppool_test.go`:

```go
package resppool

import (
	"bytes"
	"fmt"
	"strconv"
	"sync"
	"testing"
)

func TestConcurrentNoContamination(t *testing.T) {
	t.Parallel()
	p := NewPool()
	const workers = 64
	var wg sync.WaitGroup
	errs := make([]error, workers)
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 200 {
				want := fmt.Sprintf("worker=%d seq=%d", i, j)
				got := p.Build(func(buf *bytes.Buffer) {
					buf.WriteString("worker=")
					buf.Write(strconv.AppendInt(nil, int64(i), 10))
					buf.WriteString(" seq=")
					buf.Write(strconv.AppendInt(nil, int64(j), 10))
				})
				if string(got) != want {
					errs[i] = fmt.Errorf("got %q want %q", got, want)
					return
				}
			}
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestCapGuardDropsLargeBuffer(t *testing.T) {
	t.Parallel()
	p := NewPool()

	big := p.Get()
	big.Write(make([]byte, maxPooledCap+1))
	p.Put(big)
	if p.Dropped() != 1 {
		t.Fatalf("oversized buffer not dropped: dropped=%d", p.Dropped())
	}

	small := p.Get()
	small.WriteString("small")
	p.Put(small)
	if p.Dropped() != 1 {
		t.Fatalf("small buffer wrongly dropped: dropped=%d", p.Dropped())
	}
}

func TestBuildReturnsOwnedCopy(t *testing.T) {
	t.Parallel()
	p := NewPool()
	a := p.Build(func(buf *bytes.Buffer) { buf.WriteString("first") })
	// Building again reuses the pooled buffer; the earlier result must be unaffected.
	_ = p.Build(func(buf *bytes.Buffer) { buf.WriteString("second-longer") })
	if string(a) != "first" {
		t.Fatalf("earlier result mutated by buffer reuse: %q", a)
	}
}

func BenchmarkPooledVsFresh(b *testing.B) {
	b.Run("pooled", func(b *testing.B) {
		p := NewPool()
		b.ReportAllocs()
		for range b.N {
			_ = p.Build(func(buf *bytes.Buffer) {
				buf.WriteString("id=")
				buf.Write(strconv.AppendInt(nil, 42, 10))
			})
		}
	})
	b.Run("fresh", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			var buf bytes.Buffer
			buf.WriteString("id=")
			buf.Write(strconv.AppendInt(nil, 42, 10))
			_ = bytes.Clone(buf.Bytes())
		}
	})
}
```

## Review

The builder is correct when concurrent borrowers never see each other's bytes,
which `TestConcurrentNoContamination` asserts under `-race` across thousands of
builds. That property rests on two things: every `Get` returns a `Reset` buffer, and
`Build` returns an owned `bytes.Clone` rather than the buffer's aliased `Bytes()` —
`TestBuildReturnsOwnedCopy` pins the second by mutating the pooled buffer on the next
build and checking the earlier result survived.

The cap guard is the memory-hygiene half. Without it the pool silently pins the
largest response ever built; `TestCapGuardDropsLargeBuffer` proves an oversized
buffer is dropped and a normal one is kept. In production, size `maxPooledCap` to
sit above your typical response but below the pathological one, and remember the
guard trades a rare re-allocation (when an outlier is dropped and the next large
response must grow again) for a bounded steady-state footprint — almost always the
right trade for a server.

## Resources

- [`sync.Pool`](https://pkg.go.dev/sync#Pool) — Get/Put semantics and the "no size limit" caveat.
- [`bytes.Buffer`](https://pkg.go.dev/bytes#Buffer) — `Reset`, `Cap`, `Len`, `Bytes` (aliasing) semantics.
- [`bytes.Clone`](https://pkg.go.dev/bytes#Clone) — the owned-copy helper at the ownership boundary.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-utf8-validation-boundary.md](04-utf8-validation-boundary.md)
