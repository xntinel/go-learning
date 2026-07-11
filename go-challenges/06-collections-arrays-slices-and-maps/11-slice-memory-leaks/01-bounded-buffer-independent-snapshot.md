# Exercise 1: Bounded Buffer That Hands Out Non-Pinning Snapshots

A ring-style capture buffer that retains the last N bytes of a stream — a request
tail, a log window, a protocol trace — is a real fixture in backend diagnostics.
The design hazard is the accessor: if `Snapshot` hands back a view into the
buffer's own storage, every consumer that keeps the result pins the whole buffer
and can mutate it out from under the producer. This module builds the buffer so
that `Snapshot` and `Last` return independent copies that neither alias nor pin
the producer's backing array.

## What you'll build

```text
leakbuf/                     independent module: example.com/leakbuf
  go.mod                     go 1.24
  leakbuf.go                 type Buffer; New, Add, Get, Snapshot, Last, Len, Cap
  cmd/
    demo/
      main.go                fills a buffer, mutates a snapshot, shows the buffer is untouched
  leakbuf_test.go            table tests: independent-copy, ErrFull, Last clamping; -race
```

Files: `leakbuf.go`, `cmd/demo/main.go`, `leakbuf_test.go`.
Implement: a `Buffer` over a fixed-capacity `[]byte` with `Add`, `Get`, `Snapshot`, `Last`, `Len`, `Cap`, where `Snapshot` and `Last` return fresh copies.
Test: mutate a returned slice and assert the buffer is unchanged; assert `Add` returns `ErrFull` at capacity; assert `Last(n)` clamps to `Len`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/leakbuf/cmd/demo
cd ~/go-exercises/leakbuf
go mod init example.com/leakbuf
go mod edit -go=1.24
```

## The design

The buffer holds a `[]byte` created with a fixed capacity and an explicit `size`
counter. `Add` appends until `size == cap(data)`, then returns `ErrFull` — a full
buffer is a caller-visible condition, not a silent drop, so it is a sentinel error
wrapped-checkable with `errors.Is`. `Get(i)` bounds-checks against `size` and
returns `ErrIndex` out of range.

The two accessors are the point of the lesson. `Snapshot` returns *all* stored
bytes; `Last(n)` returns the trailing `n`. Each must return a slice that (a) does
not alias the buffer's `data`, so a consumer's write cannot corrupt the producer,
and (b) does not pin `data`, so a consumer that keeps the result for a long time
does not keep the whole buffer alive. Both properties come from the same move: a
fresh allocation. `Snapshot` uses `copy` into a `make`'d slice; `Last` uses
`append([]byte(nil), src...)`. These are two spellings of the identical idiom — a
new backing array whose only contents are the visible window.

`Last(n)` clamps `n` to `size` rather than panicking, because "give me the last
100 bytes" of a buffer holding 12 is a reasonable request with an obvious answer:
all 12. Clamping keeps the accessor total.

The wrong version — `return b.data[b.size-n : b.size]` — would compile and pass a
naive equality test, because the bytes are correct. It fails the two tests that
matter: mutating the result mutates the buffer (aliasing), and holding the result
pins the buffer (the pin, proven in Exercise 2). The copy is what makes the
accessor safe to hand across an ownership boundary.

Create `leakbuf.go`:

```go
package leakbuf

import "errors"

// Sentinel errors, checkable with errors.Is.
var (
	ErrEmpty = errors.New("leakbuf: buffer is empty")
	ErrFull  = errors.New("leakbuf: buffer is full")
	ErrIndex = errors.New("leakbuf: index out of range")
)

// Buffer retains up to a fixed number of bytes and hands out independent copies.
type Buffer struct {
	data []byte
	size int
}

// New returns a Buffer that stores at most capacity bytes (minimum 1).
func New(capacity int) *Buffer {
	if capacity <= 0 {
		capacity = 1
	}
	return &Buffer{data: make([]byte, 0, capacity)}
}

// Add appends v, or returns ErrFull if the buffer is at capacity.
func (b *Buffer) Add(v byte) error {
	if b.size == cap(b.data) {
		return ErrFull
	}
	b.data = append(b.data, v)
	b.size++
	return nil
}

// Get returns the byte at index i, or ErrIndex if i is out of range.
func (b *Buffer) Get(i int) (byte, error) {
	if i < 0 || i >= b.size {
		return 0, ErrIndex
	}
	return b.data[i], nil
}

// Snapshot returns an independent copy of every stored byte. The result neither
// aliases nor pins the buffer's backing array.
func (b *Buffer) Snapshot() []byte {
	out := make([]byte, b.size)
	copy(out, b.data)
	return out
}

// Last returns an independent copy of the trailing n bytes, clamped to Len.
func (b *Buffer) Last(n int) []byte {
	if n > b.size {
		n = b.size
	}
	if n < 0 {
		n = 0
	}
	return append([]byte(nil), b.data[b.size-n:b.size]...)
}

// Len reports the number of stored bytes.
func (b *Buffer) Len() int { return b.size }

// Cap reports the fixed capacity.
func (b *Buffer) Cap() int { return cap(b.data) }
```

## The runnable demo

The demo fills a small buffer, takes a snapshot, mutates the snapshot, and shows
the buffer is untouched — the observable proof that the copy is independent.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/leakbuf"
)

func main() {
	b := leakbuf.New(4)
	for _, v := range []byte{10, 20, 30, 40} {
		if err := b.Add(v); err != nil {
			fmt.Println("add:", err)
		}
	}

	if err := b.Add(50); errors.Is(err, leakbuf.ErrFull) {
		fmt.Println("fifth Add rejected: buffer full")
	}

	snap := b.Snapshot()
	snap[0] = 99 // mutate the copy

	first, _ := b.Get(0)
	fmt.Printf("snapshot after mutate: %v\n", snap)
	fmt.Printf("buffer[0] still:       %d\n", first)
	fmt.Printf("last 2:                %v\n", b.Last(2))
	fmt.Printf("last 10 (clamped):     %v\n", b.Last(10))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
fifth Add rejected: buffer full
snapshot after mutate: [99 20 30 40]
buffer[0] still:       10
last 2:                [30 40]
last 10 (clamped):     [10 20 30 40]
```

## Tests

The tests are table-driven and prove the three contracts: `Snapshot` and `Last`
return independent copies (mutating the result does not touch the buffer), `Add`
returns `ErrFull` at capacity (asserted with `errors.Is`), and `Last(n)` clamps to
`Len`. All are safe to run in parallel and under `-race`.

Create `leakbuf_test.go`:

```go
package leakbuf

import (
	"errors"
	"reflect"
	"testing"
)

func fill(t *testing.T, b *Buffer, vs ...byte) {
	t.Helper()
	for _, v := range vs {
		if err := b.Add(v); err != nil {
			t.Fatalf("Add(%d): %v", v, err)
		}
	}
}

func TestSnapshotIsIndependentCopy(t *testing.T) {
	t.Parallel()

	b := New(10)
	fill(t, b, 1, 2, 3)

	got := b.Snapshot()
	got[0] = 99

	if v, _ := b.Get(0); v != 1 {
		t.Fatalf("mutating snapshot changed buffer[0] to %d, want 1", v)
	}
}

func TestLastIsIndependentCopy(t *testing.T) {
	t.Parallel()

	b := New(10)
	fill(t, b, 1, 2, 3, 4, 5)

	got := b.Last(3)
	got[0] = 99

	if v, _ := b.Get(2); v != 3 {
		t.Fatalf("mutating Last changed buffer[2] to %d, want 3", v)
	}
}

func TestAddRejectsWhenFull(t *testing.T) {
	t.Parallel()

	b := New(2)
	fill(t, b, 1, 2)

	err := b.Add(3)
	if !errors.Is(err, ErrFull) {
		t.Fatalf("Add on full buffer = %v, want ErrFull", err)
	}
}

func TestGetOutOfRange(t *testing.T) {
	t.Parallel()

	b := New(4)
	fill(t, b, 7)

	if _, err := b.Get(5); !errors.Is(err, ErrIndex) {
		t.Fatalf("Get(5) err = %v, want ErrIndex", err)
	}
}

func TestLastClamping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		n    int
		want []byte
	}{
		{"exact", 2, []byte{1, 2}},
		{"clamped over len", 10, []byte{1, 2}},
		{"zero", 0, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := New(10)
			fill(t, b, 1, 2)
			if got := b.Last(tt.n); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Last(%d) = %v, want %v", tt.n, got, tt.want)
			}
		})
	}
}
```

## Review

The buffer is correct when both accessors are total and independent. `Snapshot`
and `Last` return slices whose backing array is freshly allocated, so a consumer
can neither mutate the producer through them nor keep the producer's storage alive
by holding them; the independent-copy tests prove the first property directly, and
Exercise 2 proves the second with a `runtime.ReadMemStats` harness. `Add` returns
the `ErrFull` sentinel at capacity rather than silently dropping, and `Last` clamps
to `Len` rather than panicking. The mistake this module exists to prevent is
returning `b.data[low:high]` from an accessor — it compiles, it returns the right
bytes, and it hands every caller a pin on and a write path into the producer's
storage. Run `go test -race` to confirm the accessors are safe under concurrent
reads.

## Resources

- [`slices.Clone`](https://pkg.go.dev/slices#Clone) — the standard-library spelling of the defensive copy this module makes by hand.
- [`builtin.copy` and `append`](https://pkg.go.dev/builtin#copy) — the two primitives that allocate a fresh backing array.
- [Go blog: Arrays, slices (and strings): the mechanics of 'append'](https://go.dev/blog/slices) — how a slice header shares and pins a backing array.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-memstats-pinning-leak-test.md](02-memstats-pinning-leak-test.md)
