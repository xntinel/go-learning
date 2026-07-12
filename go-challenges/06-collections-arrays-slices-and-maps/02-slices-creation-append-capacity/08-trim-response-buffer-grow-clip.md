# Exercise 8: Sizing and Trimming a Response Payload With Grow and Clip

Assembling a response body from many pieces has two failure modes at opposite
ends. Append the pieces one at a time from nil and you pay repeated reallocations
mid-burst. Over-reserve a big buffer and hand it to a long-lived owner without
trimming, and its excess capacity pins a large backing array alive. `slices.Grow`
fixes the first (reserve capacity once), `slices.Clip` fixes the second (trim
capacity to length before retention).

This module is self-contained: its own module, demo, and tests.

## What you'll build

```text
respbuf/                   independent module: example.com/respbuf
  go.mod                   go 1.26
  respbuf.go               Response; Reserve (Grow), Write, Finalize (Clip), Len, Cap
  cmd/
    demo/
      main.go              reserve, write a burst, finalize, print cap before/after
  respbuf_test.go          Grow reserves; stable pointer; Clip trims; single-alloc, Example
```

Files: `respbuf.go`, `cmd/demo/main.go`, `respbuf_test.go`.
Implement: `Reserve(n)` via `slices.Grow`, `Write(p)` via `append`, `Finalize()` via `slices.Clip`.
Test: assert `Reserve` makes `cap >= needed` and following appends do not reallocate (stable `&buf[0]`); assert `Finalize` yields `cap == len`; `AllocsPerRun` on the grow-then-append path shows the single growth allocation.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Grow reserves once; Clip releases before you hand it off

`slices.Grow(s, n)` returns a slice with the same length as `s` but capacity
guaranteed for at least `n` more appends without reallocating. It is the
existing-slice analogue of `make([]T, 0, n)`: call it once before a burst of
appends when you know (or can bound) how many bytes are coming — a `Content-Length`,
the sum of the parts you are about to concatenate, a per-response size budget. The
burst then appends entirely in place, so a response assembled from twenty
fragments does one allocation instead of several, with no intermediate copying as
the buffer would otherwise double.

`slices.Clip(s)` returns `s[:len(s):len(s)]` — it caps capacity down to length. Its
job is the opposite: you have a slice with more capacity than length (because you
reserved generously, or because it grew past what you kept) and you are about to
store it somewhere long-lived — a cache, a struct field that outlives the request.
The excess capacity is dead tail of a backing array that the GC cannot reclaim
while the slice is alive, so a 12-byte value can pin a 4-KB array. `Clip` before
retention hands the owner a slice whose backing array is exactly its length, so
nothing extra is pinned. (If the excess is large, `Clip` allocates a right-sized
array and copies; that one-time cost buys back all the pinned memory.)

The order matters: `Grow` up front to make the burst allocation-free, `Clip` at the
end to release whatever slack you over-reserved before a long-lived owner takes the
slice.

Create `respbuf.go`:

```go
package respbuf

import "slices"

// Response accumulates a response body from fragments. Reserve capacity up front
// with Grow, Write the fragments, then Finalize with Clip before handing the
// bytes to a long-lived owner.
type Response struct {
	buf []byte
}

// Reserve ensures capacity for at least n more bytes without reallocating during
// the subsequent Writes.
func (r *Response) Reserve(n int) {
	r.buf = slices.Grow(r.buf, n)
}

// Write appends p to the body.
func (r *Response) Write(p []byte) {
	r.buf = append(r.buf, p...)
}

// Finalize trims excess capacity to length and returns the body. After this the
// returned slice's backing array is exactly its length, so retaining it pins no
// extra memory.
func (r *Response) Finalize() []byte {
	return slices.Clip(r.buf)
}

// Len reports the current body length.
func (r *Response) Len() int { return len(r.buf) }

// Cap reports the current backing-array capacity.
func (r *Response) Cap() int { return cap(r.buf) }
```

### The runnable demo

The demo deliberately over-reserves 64 bytes, writes a short body, and prints the
capacity before and after `Finalize` so the trim is visible.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/respbuf"
)

func main() {
	var r respbuf.Response
	r.Reserve(64) // generous reservation
	for _, part := range [][]byte{[]byte("HTTP/1.1 200 OK\r\n"), []byte("\r\n"), []byte("ok")} {
		r.Write(part)
	}
	fmt.Printf("before finalize: len=%d cap>=64: %v\n", r.Len(), r.Cap() >= 64)

	body := r.Finalize()
	fmt.Printf("after finalize:  len=%d cap=%d\n", len(body), cap(body))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
before finalize: len=21 cap>=64: true
after finalize:  len=21 cap=21
```

### Tests

`TestReserveGivesCapacity` asserts `Reserve(n)` yields `cap >= n`.
`TestAppendsDoNotReallocate` writes one byte, captures `&r.buf[0]`, writes the rest
within the reserved capacity, and asserts the pointer is unchanged — proving no
reallocation. `TestFinalizeClipsToLen` over-reserves, writes a short body, and
asserts `Finalize` returns `cap == len`. `TestSingleGrowthAllocation` measures the
grow-then-append path with `testing.AllocsPerRun` and asserts one allocation.

Create `respbuf_test.go`:

```go
package respbuf

import (
	"fmt"
	"testing"
)

func parts() [][]byte {
	return [][]byte{[]byte("alpha"), []byte("beta"), []byte("gamma"), []byte("delta")}
}

func totalLen(ps [][]byte) int {
	n := 0
	for _, p := range ps {
		n += len(p)
	}
	return n
}

func TestReserveGivesCapacity(t *testing.T) {
	t.Parallel()
	var r Response
	r.Reserve(128)
	if r.Cap() < 128 {
		t.Fatalf("cap = %d, want >= 128", r.Cap())
	}
	if r.Len() != 0 {
		t.Fatalf("len = %d, want 0 (Grow must not change length)", r.Len())
	}
}

func TestAppendsDoNotReallocate(t *testing.T) {
	t.Parallel()
	ps := parts()
	var r Response
	r.Reserve(totalLen(ps))

	r.Write(ps[0])
	first := &r.buf[0] // address of the backing array's first element
	for _, p := range ps[1:] {
		r.Write(p)
	}
	if &r.buf[0] != first {
		t.Fatal("backing array reallocated during the reserved burst")
	}
	if r.Len() != totalLen(ps) {
		t.Fatalf("len = %d, want %d", r.Len(), totalLen(ps))
	}
}

func TestFinalizeClipsToLen(t *testing.T) {
	t.Parallel()
	var r Response
	r.Reserve(256) // over-reserve
	r.Write([]byte("small body"))

	body := r.Finalize()
	if cap(body) != len(body) {
		t.Fatalf("after Clip: cap=%d len=%d, want cap==len", cap(body), len(body))
	}
	if string(body) != "small body" {
		t.Fatalf("body = %q, want %q", body, "small body")
	}
}

func TestSingleGrowthAllocation(t *testing.T) {
	ps := parts()
	total := totalLen(ps)
	var sink []byte
	allocs := testing.AllocsPerRun(50, func() {
		var r Response
		r.Reserve(total)
		for _, p := range ps {
			r.Write(p)
		}
		sink = r.buf // measure grow+append only, not Clip
	})
	if allocs > 1 {
		t.Errorf("grow-then-append did %v allocations, want 1", allocs)
	}
	_ = sink
}

func ExampleResponse() {
	var r Response
	r.Reserve(32)
	r.Write([]byte("hi"))
	body := r.Finalize()
	fmt.Printf("%q len=%d cap=%d\n", body, len(body), cap(body))
	// Output: "hi" len=2 cap=2
}
```

## Review

The buffer is correct when `Reserve` makes the following burst allocation-free and
`Finalize` returns a slice with no excess capacity. `TestAppendsDoNotReallocate`
proves the first half by pointer identity of `&r.buf[0]`; `TestFinalizeClipsToLen`
proves the second by `cap == len`. The mistake this exercise guards against on the
retention side is subtle: a correct-looking response that carries a large reserved
capacity into a cache pins that whole array alive for as long as the cached value
lives — `Clip` is what releases it. Do not confuse `Clip` (trims excess capacity)
with `Compact` (removes adjacent duplicates); they are unrelated. `AllocsPerRun`
cannot run inside a parallel test, so `TestSingleGrowthAllocation` is serial. Run
`-race` to confirm.

## Resources

- [`slices.Grow`](https://pkg.go.dev/slices#Grow)
- [`slices.Clip`](https://pkg.go.dev/slices#Clip)
- [Go blog: Arrays, slices (and strings): The mechanics of 'append'](https://go.dev/blog/slices)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-remove-from-pool-ordered-vs-swap.md](07-remove-from-pool-ordered-vs-swap.md) | Next: [09-defensive-copy-of-pooled-read-buffer.md](09-defensive-copy-of-pooled-read-buffer.md)
