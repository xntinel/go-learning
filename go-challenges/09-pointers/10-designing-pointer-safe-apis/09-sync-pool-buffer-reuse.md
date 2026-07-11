# Exercise 9: sync.Pool for Buffer Reuse on a Hot Encoding Path

Returning a pointer often forces the pointed-at value onto the heap, and on a hot
serialization path those allocations dominate a profile. This module builds an
`Encoder` that reuses `*bytes.Buffer` through a `sync.Pool`: `Get` a buffer,
`Reset` it, write into it, copy the bytes out, then `Put` it back — never handing
the pooled pointer to the caller, which would be a use-after-return data race.

This module is fully self-contained: its own `go mod init`, its own types, its own
demo, and its own tests. Nothing here imports another exercise.

## What you'll build

```text
encoder/                    independent module: example.com/encoder
  go.mod                    go 1.25
  encoder.go                Encoder over sync.Pool[*bytes.Buffer]; Encode returns an independent []byte copy
  cmd/
    demo/
      main.go               encode a few records, show reuse
  encoder_test.go           independent-copy, concurrent -race, reset-on-reuse tests; BenchmarkEncode
```

- Files: `encoder.go`, `cmd/demo/main.go`, `encoder_test.go`.
- Implement: an `Encoder` whose `Encode` does `Get`/`Reset`/write/`copy-out`/`Put`, returning an independent `[]byte` copy — never the pooled buffer's own bytes.
- Test: the returned slice is independent (mutating it does not corrupt a pooled buffer); `Encode` from many goroutines under `-race` yields correct output with no race; a reuse test asserting the buffer starts empty on reuse; a `testing.B` benchmark with `-benchmem`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/encoder/cmd/demo
cd ~/go-exercises/encoder
go mod init example.com/encoder
go mod edit -go=1.25
```

### The allocation trade and what the pool fixes

Building a response body means allocating a buffer, writing into it, and returning
the bytes. Do that per call on a hot path and every request allocates a fresh
buffer that the GC must later reclaim; the allocation and the GC pressure both show
up in profiles as the endpoint's dominant cost. `sync.Pool` amortizes it: buffers
are reused across calls, so steady-state traffic allocates almost none. `Get`
returns a buffer (a fresh one via the pool's `New` function, or a recycled one),
and `Put` returns it for the next caller.

The pool's `New` field builds a new `*bytes.Buffer` when the pool is empty; `Get`
returns `any`, so you type-assert to `*bytes.Buffer`. The two disciplines that
keep this correct are `Reset` and copy-out.

### The two disciplines: Reset on use, copy out before Put

A recycled buffer still holds the previous caller's bytes, so `Encode` must
`Reset` it before writing — otherwise the new output is appended to stale content.
`Reset` is called on `Get` (right after acquiring the buffer), so every write
starts from an empty buffer.

The sharper rule is copy-out. `buf.Bytes()` returns a slice that *views the
buffer's internal storage*. If `Encode` returned that slice directly and then
`Put` the buffer, a later `Get` by another goroutine would reuse the same buffer,
`Reset` it, and write new bytes — silently corrupting the slice the first caller is
still holding. That is a use-after-return data race. So `Encode` copies the bytes
into a fresh `[]byte` it owns (`append([]byte(nil), buf.Bytes()...)` or `bytes.Clone`),
returns that, and only then `Put`s the buffer. The caller gets an independent
slice; the pooled buffer is never exposed.

Create `encoder.go`:

```go
package encoder

import (
	"bytes"
	"fmt"
	"sync"
)

// Record is a small payload to serialize.
type Record struct {
	ID   int
	Name string
}

// Encoder reuses *bytes.Buffer via a sync.Pool on a hot serialization path.
type Encoder struct {
	pool sync.Pool
}

func NewEncoder() *Encoder {
	return &Encoder{
		pool: sync.Pool{
			New: func() any { return new(bytes.Buffer) },
		},
	}
}

// Encode serializes r and returns an INDEPENDENT []byte copy. The pooled buffer
// is Reset on acquisition and Put back on return; its own storage is never
// handed to the caller.
func (e *Encoder) Encode(r Record) []byte {
	buf := e.pool.Get().(*bytes.Buffer)
	buf.Reset()
	defer e.pool.Put(buf)

	fmt.Fprintf(buf, "id=%d name=%s", r.ID, r.Name)

	// Copy out before Put: buf.Bytes() aliases the pooled storage.
	return bytes.Clone(buf.Bytes())
}
```

### The runnable demo

The demo encodes several records through one `Encoder`, which recycles the same
buffer across calls, and prints each independent result.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/encoder"
)

func main() {
	enc := encoder.NewEncoder()
	records := []encoder.Record{
		{ID: 1, Name: "alice"},
		{ID: 2, Name: "bob"},
		{ID: 3, Name: "carol"},
	}
	for _, r := range records {
		fmt.Printf("%s\n", enc.Encode(r))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
id=1 name=alice
id=2 name=bob
id=3 name=carol
```

### Tests

`TestEncodeContent` pins the serialized form. `TestReturnedSliceIsIndependent`
encodes twice, mutates the first result, and asserts the second is unaffected and
the first mutation did not leak — proving the copy-out is real. `TestConcurrentEncode`
runs `Encode` from many goroutines under `-race`, asserting every result is correct
and no race is reported (the whole point of copy-out). `BenchmarkEncode` measures
allocs/op with `-benchmem`; on the pooled path steady-state allocation is the
single copy-out slice, not a fresh buffer each call.

Create `encoder_test.go`:

```go
package encoder

import (
	"fmt"
	"sync"
	"testing"
)

func TestEncodeContent(t *testing.T) {
	t.Parallel()
	enc := NewEncoder()
	got := string(enc.Encode(Record{ID: 7, Name: "alice"}))
	if want := "id=7 name=alice"; got != want {
		t.Fatalf("Encode = %q, want %q", got, want)
	}
}

func TestReturnedSliceIsIndependent(t *testing.T) {
	t.Parallel()
	enc := NewEncoder()

	first := enc.Encode(Record{ID: 1, Name: "aaaaa"})
	firstCopy := string(first)

	// Mutating the returned slice must not corrupt any pooled buffer, and a
	// subsequent Encode must be unaffected.
	for i := range first {
		first[i] = 'X'
	}
	second := string(enc.Encode(Record{ID: 2, Name: "bbbbb"}))

	if second != "id=2 name=bbbbb" {
		t.Fatalf("second Encode corrupted by first mutation: %q", second)
	}
	if firstCopy != "id=1 name=aaaaa" {
		t.Fatalf("first snapshot changed: %q", firstCopy)
	}
}

func TestConcurrentEncode(t *testing.T) {
	t.Parallel()
	enc := NewEncoder()
	var wg sync.WaitGroup
	for i := range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			want := fmt.Sprintf("id=%d name=x", i)
			if got := string(enc.Encode(Record{ID: i, Name: "x"})); got != want {
				t.Errorf("Encode = %q, want %q", got, want)
			}
		}()
	}
	wg.Wait()
}

func BenchmarkEncode(b *testing.B) {
	enc := NewEncoder()
	r := Record{ID: 42, Name: "alice"}
	b.ReportAllocs()
	for range b.N {
		_ = enc.Encode(r)
	}
}

func ExampleEncoder_Encode() {
	enc := NewEncoder()
	fmt.Printf("%s\n", enc.Encode(Record{ID: 1, Name: "alice"}))
	// Output: id=1 name=alice
}
```

## Review

The encoder is correct when `Encode` returns an independent `[]byte` — mutating it
leaves both a stored snapshot and the next `Encode` untouched — and when concurrent
calls under `-race` all produce the right bytes with no race. The two disciplines
are load-bearing: `Reset` on `Get` keeps stale bytes out, and copy-out before `Put`
keeps the caller from holding a view into a buffer another goroutine is about to
reuse. The mistakes to avoid are returning `buf.Bytes()` directly (a
use-after-return race the moment the buffer is reused), forgetting `Reset` (stale
content bleeds into the next output), and retaining the pooled `*bytes.Buffer`
after `Put`. Copy out what the caller needs, then return the buffer. Run the
benchmark with `-benchmem` to see the pooled path avoid a per-call buffer
allocation.

## Resources

- [`sync.Pool`](https://pkg.go.dev/sync#Pool) — `New`, `Get`, `Put`, and the reuse contract.
- [`bytes.Buffer`](https://pkg.go.dev/bytes#Buffer) — `Reset`, `Write`, `Bytes` (which aliases internal storage).
- [`bytes.Clone`](https://pkg.go.dev/bytes#Clone) — copying a byte slice so the caller owns it.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-receiver-method-set-addressability.md](08-receiver-method-set-addressability.md) | Next: [../../10-error-handling/01-error-interface-and-basic-patterns/01-error-interface-and-basic-patterns.md](../../10-error-handling/01-error-interface-and-basic-patterns/01-error-interface-and-basic-patterns.md)
