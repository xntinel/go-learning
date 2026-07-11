# Exercise 6: Pool Buffers with sync.Pool for a Per-Request Encoder Without Leaking Capacity

A high-throughput handler that builds a response buffer per request allocates one
buffer per request, and under load that allocation churn becomes GC pressure. This
exercise reuses `*bytes.Buffer` instances through a `sync.Pool` with the two
disciplines that make pooling correct: `Reset` on `Get`, and a capacity guard before
`Put` so a single huge payload cannot pin megabytes in the pool forever.

This module is self-contained.

## What you'll build

```text
bufpool/                     independent module: example.com/bufpool
  go.mod
  bufpool.go                 Encode (pooled), EncodeUnpooled, retain policy, cap guard
  cmd/
    demo/
      main.go                encodes a few field sets, prints results
  bufpool_test.go            concurrent -race correctness, no-bleed-through, retain policy, benchmarks
```

Files: `bufpool.go`, `cmd/demo/main.go`, `bufpool_test.go`.
Implement: `Encode(fields []string) string` backed by a `sync.Pool` of `*bytes.Buffer`, with `Reset` on get and a `Cap()` guard on put; `EncodeUnpooled` for comparison.
Test: concurrent encode under `-race` gives uncorrupted output; no bleed-through between reuses; the cap policy drops oversized buffers; pooled vs unpooled benchmarks.
Verify: `go test -count=1 -race ./...`

```bash
mkdir -p ~/go-exercises/bufpool/cmd/demo
cd ~/go-exercises/bufpool
go mod init example.com/bufpool
```

### The two disciplines of a correct pool

`sync.Pool` hands you objects that other goroutines used before you and may reuse
after you. That single fact dictates both disciplines. When you `Get`, the buffer
contains whatever the last user wrote — the pool does not clear it — so you must
`Reset()` it before writing, or the previous request's bytes bleed into yours. When
you `Put`, you are handing your buffer to the next user, and if this request grew it
to 8 MB to hold a giant payload, the pool now holds an 8 MB buffer that will never
shrink; the next thousand small requests reuse and re-retain that capacity, and you
have a memory leak that looks like a cache. The guard is to check `Cap()` before
`Put` and simply *not* return oversized buffers — let them be collected — so the pool
stabilizes around the common small size.

The lifecycle in `Encode` is: `Get`, `Reset`, write the fields, capture the result
with `String()` (which copies the bytes out into a new string, so the buffer is free
to be reused immediately), then in a deferred step decide whether to `Put` based on
`Cap()`. Because `String()` copies, returning the buffer to the pool in the same call
is safe — the returned string does not alias the buffer's backing array. `EncodeUnpooled`
does the same work with a fresh `strings.Builder` each call and exists only so the
benchmark can show the allocation difference.

Correctness under concurrency is not automatic and is the thing `-race` verifies.
`sync.Pool` itself is safe for concurrent `Get`/`Put`, and because each call gets its
*own* buffer for the duration of the call, there is no sharing of a buffer across
goroutines — the race detector confirms no two goroutines touch the same buffer at
once and no output is corrupted.

Create `bufpool.go`:

```go
package bufpool

import (
	"bytes"
	"strings"
	"sync"
)

// maxRetainedCap bounds the capacity of buffers returned to the pool. A buffer
// grown past this by one large request is dropped so it cannot pin memory.
const maxRetainedCap = 64 << 10 // 64 KiB

var bufPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

// retain reports whether a buffer with the given capacity is small enough to
// return to the pool.
func retain(capacity int) bool {
	return capacity <= maxRetainedCap
}

// Encode joins fields with single spaces using a pooled *bytes.Buffer. The
// buffer is Reset on get (its contents are arbitrary) and only returned to the
// pool if its capacity is within the guard.
func Encode(fields []string) string {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer func() {
		if retain(buf.Cap()) {
			bufPool.Put(buf)
		}
	}()

	for i, f := range fields {
		if i > 0 {
			buf.WriteByte(' ')
		}
		buf.WriteString(f)
	}
	return buf.String()
}

// EncodeUnpooled does the same work with a fresh strings.Builder each call, for
// benchmark comparison against the pooled path.
func EncodeUnpooled(fields []string) string {
	var b strings.Builder
	for i, f := range fields {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(f)
	}
	return b.String()
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/bufpool"
)

func main() {
	fmt.Println(bufpool.Encode([]string{"GET", "/api/users", "200"}))
	fmt.Println(bufpool.Encode([]string{"POST", "/login", "401"}))
	fmt.Println(bufpool.EncodeUnpooled([]string{"one", "two"}))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
GET /api/users 200
POST /login 401
one two
```

### Tests

The concurrency test spins many goroutines encoding distinct inputs and checks each
gets its own correct output; run under `-race` it proves no buffer is shared. The
bleed-through test encodes a long input then a short one and asserts the short output
is not contaminated. The policy test pins the cap guard directly.

Create `bufpool_test.go`:

```go
package bufpool

import (
	"fmt"
	"strconv"
	"sync"
	"testing"
)

func TestConcurrentEncode(t *testing.T) {
	t.Parallel()

	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			in := []string{"req", strconv.Itoa(i)}
			want := "req " + strconv.Itoa(i)
			if got := Encode(in); got != want {
				t.Errorf("Encode(%v) = %q, want %q", in, got, want)
			}
		}()
	}
	wg.Wait()
}

func TestNoBleedThrough(t *testing.T) {
	t.Parallel()

	_ = Encode([]string{"aaaaaaaaaaaa", "bbbbbbbbbbbb", "cccccccccccc"})
	if got := Encode([]string{"x"}); got != "x" {
		t.Fatalf("bleed-through after reuse: got %q, want %q", got, "x")
	}
}

func TestRetainPolicy(t *testing.T) {
	t.Parallel()

	if !retain(1024) {
		t.Fatal("a small buffer should be retained")
	}
	if retain(maxRetainedCap + 1) {
		t.Fatal("an oversized buffer should be dropped, not retained")
	}
}

func BenchmarkEncodePooled(b *testing.B) {
	fields := []string{"GET", "/api/users", "200", "127.0.0.1"}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = Encode(fields)
		}
	})
}

func BenchmarkEncodeUnpooled(b *testing.B) {
	fields := []string{"GET", "/api/users", "200", "127.0.0.1"}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = EncodeUnpooled(fields)
		}
	})
}

func ExampleEncode() {
	fmt.Println(Encode([]string{"GET", "/health", "200"}))
	// Output: GET /health 200
}
```

Run the pooled-vs-unpooled comparison yourself:

```bash
go test -bench=. -benchmem -run=^$ ./...
```

## Review

The pool is correct when three things hold: `Reset` runs right after `Get` so no
prior content bleeds through, the buffer is only returned when `retain(buf.Cap())` is
true so one large payload cannot pin memory, and concurrent encoding produces
uncorrupted output under `-race` because each call owns its buffer for its whole
lifetime. `String()` copies the bytes out, which is why it is safe to `Put` the
buffer back in the same call. Run `go test -race` to confirm the concurrency claim
and the benchmarks to see the pooled path do fewer allocations than the unpooled one.
The two mistakes this guards against are the classic pool footguns: assuming a
`Get` buffer is empty, and blindly `Put`-ing an unbounded buffer.

## Resources

- [sync.Pool](https://pkg.go.dev/sync#Pool) — `Get`, `Put`, and the "contents are arbitrary" contract.
- [bytes.Buffer](https://pkg.go.dev/bytes#Buffer) — `Reset`, `Cap`, `WriteString`, `String`.
- [testing.B.RunParallel](https://pkg.go.dev/testing#B.RunParallel) — parallel benchmark harness.

---

Prev: [05-prometheus-text-exposition-builder.md](05-prometheus-text-exposition-builder.md) | Back to [00-concepts.md](00-concepts.md) | Next: [07-append-based-zero-intermediate-serializer.md](07-append-based-zero-intermediate-serializer.md)
