# Exercise 6: A sync.Pool Byte-Buffer That Resets Cleanly and Drops Oversized Buffers

Pooling response scratch buffers across requests is a standard allocation
optimization. It has a classic leak: a buffer grows to fit the largest payload it
ever served and keeps that capacity when returned, so one giant response
permanently inflates the pool. This module builds a pooled scratch object that
resets its buffer length and clears its reused map on `Put`, and — the key fix —
*drops* any buffer whose capacity has grown past a guard threshold instead of
returning it.

## What you'll build

```text
respool/                     independent module: example.com/respool
  go.mod                     go 1.24
  respool.go                 type Scratch, Pool; NewPool, Get, Put, MaxCap
  cmd/
    demo/
      main.go                reuse a small buffer, then drop an oversized one
  respool_test.go            reset-on-put, clear-on-put, oversized-dropped, concurrent; -race
```

Files: `respool.go`, `cmd/demo/main.go`, `respool_test.go`.
Implement: a `Pool` of `*Scratch{Buf *bytes.Buffer; Tags map[string]string}`; `Put` resets `Buf`, `clear`s `Tags`, and drops the whole scratch if `Buf.Cap()` exceeds `MaxCap`.
Test: assert a small buffer is reset and its map cleared on `Put`, and an oversized buffer is dropped (not reset), under `-race`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/11-slice-memory-leaks/06-pool-buffer-reset-cap-guard/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/11-slice-memory-leaks/06-pool-buffer-reset-cap-guard
go mod edit -go=1.24
```

## Reset the length, clear the map, drop the giant

Three moves make a pool safe to reuse, and each addresses a different leak.

Reset the *length*, not the capacity. On `Put`, `Buf.Reset()` sets the buffer's
length to zero while keeping its backing array, so the next `Get` starts empty but
reuses the allocation. You cannot shrink capacity in place — the array is the
array — so resetting length is all you can and should do for a buffer within
bounds.

`clear` the reused map. The `Scratch` carries a `Tags` map reused across requests.
`clear(s.Tags)` deletes every key while keeping the map allocated, so no stale key
from the previous request survives into the next. Setting the field to a new map
would work too but throws away the allocation; `clear` is the reset that keeps it.
(Recall the map does not shrink its buckets — but a reused-per-request scratch map
has a bounded key count, so that is acceptable here; the unbounded-churn case is
the next module.)

Drop the oversized buffer. This is the fix for the pool-growth leak. If
`Buf.Cap()` has grown past `MaxCap` — because this scratch once served a giant
payload — `Put` returns *without* pooling it, so the giant array becomes garbage
instead of the pool's permanent floor. Without this guard, one 8 MiB response
handled once means every buffer the pool hands out afterward may carry 8 MiB of
capacity, and under load the pool fills with such giants. The guard bounds the
pool's per-buffer footprint to `MaxCap` regardless of outlier payloads.

Note the guard runs *before* the reset: an oversized buffer is discarded whole, so
there is no point resetting it. A test distinguishes the two paths by exactly
this — a within-bounds buffer comes back reset, an oversized one comes back
untouched (dropped).

Create `respool.go`:

```go
// Package respool pools response scratch buffers with a capacity guard so one
// oversized payload cannot permanently inflate the pool.
package respool

import (
	"bytes"
	"sync"
)

// Scratch is a reusable per-response workspace: a growable buffer and a small
// map of tags keyed by this request.
type Scratch struct {
	Buf  *bytes.Buffer
	Tags map[string]string
}

// Pool hands out Scratch values and recycles them, dropping any whose buffer has
// grown past maxCap.
type Pool struct {
	pool   sync.Pool
	maxCap int
}

// NewPool returns a Pool that recycles buffers up to maxCap bytes of capacity.
func NewPool(maxCap int) *Pool {
	if maxCap < 1 {
		maxCap = 1
	}
	return &Pool{
		maxCap: maxCap,
		pool: sync.Pool{
			New: func() any {
				return &Scratch{Buf: new(bytes.Buffer), Tags: make(map[string]string)}
			},
		},
	}
}

// MaxCap reports the capacity guard threshold.
func (p *Pool) MaxCap() int { return p.maxCap }

// Get returns a clean Scratch. Buffers are reset on Put, so a pooled one is
// already empty; a freshly created one is empty by construction.
func (p *Pool) Get() *Scratch {
	return p.pool.Get().(*Scratch)
}

// Put recycles s, unless its buffer has grown past the guard, in which case s is
// dropped so the oversized array does not become the pool's permanent floor.
func (p *Pool) Put(s *Scratch) {
	if s.Buf.Cap() > p.maxCap {
		return // drop the oversized buffer; do not pool it
	}
	// Reset length (keep the within-bounds capacity) and wipe stale map keys.
	s.Buf.Reset()
	clear(s.Tags)
	p.pool.Put(s)
}
```

## The runnable demo

The demo reuses a small scratch, then grows one past the guard and shows it is
dropped on `Put` rather than recycled.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/respool"
)

func main() {
	p := respool.NewPool(1024)

	s := p.Get()
	s.Buf.WriteString("small response body")
	s.Tags["status"] = "200"
	fmt.Printf("body: %s\n", s.Buf.String())
	p.Put(s)

	big := p.Get()
	big.Buf.Write(make([]byte, 5000)) // grow past the 1024 guard
	dropped := big.Buf.Cap() > p.MaxCap()
	fmt.Printf("oversized dropped on Put: %v\n", dropped)
	p.Put(big)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
body: small response body
oversized dropped on Put: true
```

## Tests

The reset and clear tests hold a reference to the scratch across `Put` (a
white-box observation, not something production code should do) to assert the
buffer's length is zero and the map is empty afterward. The oversized test relies
on the guard running before the reset: a dropped buffer is *not* reset, so its
content survives, which cleanly distinguishes "pooled and reset" from "dropped."
The concurrency test hammers `Get`/`Put` with mixed sizes under `-race`.

Create `respool_test.go`:

```go
package respool

import (
	"bytes"
	"sync"
	"testing"
)

func TestPutResetsWithinBounds(t *testing.T) {
	t.Parallel()

	p := NewPool(1024)
	s := p.Get()
	s.Buf.WriteString("payload")
	s.Tags["k"] = "v"

	p.Put(s) // within bounds: reset + clear + pool

	if s.Buf.Len() != 0 {
		t.Fatalf("buffer length after Put = %d, want 0 (should be reset)", s.Buf.Len())
	}
	if len(s.Tags) != 0 {
		t.Fatalf("tags after Put = %d, want 0 (should be cleared)", len(s.Tags))
	}
}

func TestPutDropsOversized(t *testing.T) {
	t.Parallel()

	p := NewPool(1024)
	big := p.Get()
	big.Buf.Write(make([]byte, 5000)) // cap now well past 1024
	big.Tags["k"] = "v"

	if big.Buf.Cap() <= p.MaxCap() {
		t.Fatalf("precondition: cap %d should exceed guard %d", big.Buf.Cap(), p.MaxCap())
	}

	p.Put(big) // oversized: dropped before reset

	if big.Buf.Len() != 5000 {
		t.Fatalf("oversized buffer was reset (len %d); it should be dropped untouched", big.Buf.Len())
	}
}

func TestGetReturnsUsableBuffer(t *testing.T) {
	t.Parallel()

	p := NewPool(1024)
	s := p.Get()
	if s.Buf == nil || s.Tags == nil {
		t.Fatal("Get returned a Scratch with nil fields")
	}
	s.Buf.WriteString("ok")
	if s.Buf.String() != "ok" {
		t.Fatalf("buffer = %q, want ok", s.Buf.String())
	}
}

func TestConcurrentGetPut(t *testing.T) {
	t.Parallel()

	p := NewPool(4096)
	var wg sync.WaitGroup
	for i := range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := p.Get()
			size := 16
			if i%50 == 0 {
				size = 8192 // occasional giant, must be dropped
			}
			s.Buf.Write(bytes.Repeat([]byte("x"), size))
			s.Tags["i"] = "v"
			p.Put(s)
		}()
	}
	wg.Wait()
}
```

## Review

The pool is correct when a within-bounds scratch comes back reset (buffer length
zero, tags cleared) and an oversized scratch is dropped rather than recycled — the
two tests split on exactly the reset-versus-untouched observation. The mistake
this module exists to prevent is returning buffers to a `sync.Pool`
unconditionally: one giant payload then floors every future buffer at that
capacity, a leak that shows up as steadily rising RSS under bursty traffic and
never as a failing test. Reset the length and `clear` the map for buffers within
the guard; drop the ones past it. Run `go test -race` to confirm concurrent
`Get`/`Put` with mixed sizes is safe.

## Resources

- [`sync.Pool`](https://pkg.go.dev/sync#Pool) — `Get`, `Put`, `New`, and its best-effort recycling semantics.
- [`bytes.Buffer.Cap` and `Reset`](https://pkg.go.dev/bytes#Buffer.Cap) — reading capacity and resetting length without shrinking the array.
- [`builtin.clear`](https://pkg.go.dev/builtin#clear) — deleting every key of a reused map while keeping it allocated.

---

Back to [05-scanner-line-copy-on-retain.md](05-scanner-line-copy-on-retain.md) | Next: [07-rate-limiter-map-rebuild-reclaim.md](07-rate-limiter-map-rebuild-reclaim.md)
