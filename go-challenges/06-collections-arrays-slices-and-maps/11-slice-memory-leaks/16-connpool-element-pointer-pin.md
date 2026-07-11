# Exercise 16: A Connection Pool Where One Pointer Pins the Whole Array

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

High-throughput connection and worker pools routinely store their pooled
items as a value slice, `[]Connection` rather than `[]*Connection`, because
one contiguous allocation is dramatically friendlier to the CPU cache than
N scattered ones -- this is the layout behind pgx's internal connection
slots and behind most hand-rolled worker pools that care about allocation
count. It creates a hazard this lesson has not yet named directly. Every
other module here about pinning talks about *sub-slicing*: `s[low:high]`
handing out a window that keeps the whole array reachable. But `&s[i]`, the
address of a single element, is exactly as dangerous, and far less obvious.
It looks like you are holding a pointer to one small `Connection`. You are
actually holding a pointer into the same one-allocation array as every
other connection in the pool, and the garbage collector cannot tell your
one-element pointer apart from a pointer that spans the whole thing.

This is the sharpest version of "assuming a small window needs no copy"
this lesson covers: a sub-slice at least *looks* like a window, which
primes a reviewer to ask what it is a window into. `&pool[i]` looks like
ordinary, idiomatic Go -- taking the address of a struct to avoid a copy is
something the language actively encourages -- and that is exactly what
makes it easy to hand out without noticing that every sibling connection's
buffer just became unreleasable for as long as the caller keeps it. The fix
is not "never take an address"; it is knowing which values are safe to let
outlive the pool and which are not, and giving the unsafe ones no public
way out.

This module builds `connpool`, a fixed-size pool whose only way to inspect
a connection from outside is a self-contained value type that never aliases
the pool's storage. The pointer-into-the-array version is not part of that
API; it lives in the test file, isolated as the thing the tests prove
pins the pool.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
connpool/                 module example.com/connpool
  go.mod                  go 1.24
  connpool.go             Connection, ConnInfo, Pool; NewPool, Len, Write, Info, InfoAll
  connpool_test.go         construction/range tables, Info correctness, the raw-pointer-
                           vs-value-copy pinning contrast, ExamplePool_Info
```

- Files: `connpool.go`, `connpool_test.go`.
- Implement: `NewPool(size int) (*Pool, error)` rejecting a non-positive size with `ErrInvalidSize`; `(*Pool).Write(idx int, b []byte) error` and `(*Pool).Info(idx int) (ConnInfo, error)`, both rejecting an out-of-range index with `ErrIndexRange`; `(*Pool).InfoAll() []ConnInfo`; `(*Pool).Len() int`.
- Test: construction and index-range rejection; `Info` reflecting a prior `Write`; `InfoAll`'s length, capacity, and per-entry correctness; the pinning contrast -- a raw `*Connection` into the pool's array keeps an unrelated connection reachable after every other reference to the pool is dropped, proven with a `weak.Pointer`, while `Info` and `InfoAll` never do; `ExamplePool_Info` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/connpool
cd ~/go-exercises/connpool
go mod init example.com/connpool
go mod edit -go=1.24
```

### One element's address pins every element's array

`make([]Connection, 5)` is a single heap allocation sized for five
`Connection` values laid out contiguously. `&conns[4]` is a pointer into
that allocation, offset to where element 4 begins -- and that is *all* the
information the pointer carries. Nothing about it says "and only element
4 matters"; from the allocator's point of view, and therefore from the
garbage collector's point of view, it is a pointer into an object, and the
whole object stays reachable for as long as any pointer into any part of
it survives. Whether that pointer is `&conns[0]`, `&conns[4]`, or a
sub-slice `conns[2:5]` makes no difference to what gets kept alive: the
answer is always "the entire backing array."

```go
// connpool.go -- the bug, if the pool exposed this.
func (p *Pool) connAt(idx int) *Connection {
    return &p.conns[idx] // looks like "just one connection"; is the whole array
}
```

A caller who saves the result of `connAt(4)` -- to log it later, to retry
against it, to pass it to a goroutine that will get around to it eventually
-- has, without any indication in the type `*Connection`, also kept
connections 0 through 3 (and their buffers) reachable. Compare that to
handing out a sub-slice: `pool.conns[3:5]` at least announces itself as a
window with two ends. `&pool.conns[4]` looks like the single most minimal,
idiomatic thing you could return, and it is the same hazard wearing a
disguise. `Info` sidesteps it entirely by never returning a pointer into
`p.conns` at all -- it reads the fields it needs and constructs a fresh,
independent `ConnInfo` value, which shares no memory with the pool
whatsoever.

Create `connpool.go`:

```go
// Package connpool models a fixed-size connection pool stored as a value
// slice ([]Connection, not []*Connection), the layout high-throughput
// pools favor for cache locality. It exists to show a pinning hazard
// distinct from sub-slicing: a single-element pointer into that slice,
// &pool[i], is exactly as capable of keeping the pool's entire backing
// array reachable as a multi-element sub-slice is, because both are
// pointers into the same one allocation. Nothing in a *Connection's type
// signature hints that holding it retains every sibling connection's
// buffer too. Info returns an independent value instead, copied field by
// field, so retaining it never pins anything. See the package tests for
// the whole-array retention a raw pointer would cause, proven and
// disproven with a weak.Pointer.
package connpool

import (
	"errors"
	"fmt"
)

// Sentinel errors returned by NewPool, Write, and Info. Callers should
// test for them with errors.Is rather than by comparing error strings.
var (
	// ErrInvalidSize means the requested pool size was not positive.
	ErrInvalidSize = errors.New("connpool: size must be positive")
	// ErrIndexRange means the requested index was outside the pool.
	ErrIndexRange = errors.New("connpool: index out of range")
)

// Connection is one pooled connection's state. Buf stands in for whatever
// a real connection retains: a read buffer, TLS session state.
type Connection struct {
	ID  int
	Buf []byte
}

// ConnInfo is a small, self-contained summary of one Connection. Unlike a
// *Connection, it never aliases the Pool's backing array, so it is safe to
// retain indefinitely -- in a log, an audit trail, a metric label --
// without pinning anything else in the pool.
type ConnInfo struct {
	ID      int
	BufSize int
}

// Pool is a fixed-size set of pre-allocated Connections.
//
// Pool is not safe for concurrent use.
type Pool struct {
	conns []Connection
}

// NewPool returns a Pool of size pre-allocated connections. It returns
// ErrInvalidSize if size is not positive.
func NewPool(size int) (*Pool, error) {
	if size <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidSize, size)
	}
	conns := make([]Connection, size)
	for i := range conns {
		conns[i] = Connection{ID: i}
	}
	return &Pool{conns: conns}, nil
}

// Len reports the pool's size.
func (p *Pool) Len() int { return len(p.conns) }

// Write appends b to the buffer of the connection at idx. It returns
// ErrIndexRange if idx is out of bounds.
func (p *Pool) Write(idx int, b []byte) error {
	if idx < 0 || idx >= len(p.conns) {
		return fmt.Errorf("%w: %d", ErrIndexRange, idx)
	}
	p.conns[idx].Buf = append(p.conns[idx].Buf, b...)
	return nil
}

// Info returns a self-contained summary of the connection at idx. It
// returns ErrIndexRange if idx is out of bounds.
//
// The returned ConnInfo is copied out of the pool's storage field by
// field and never aliases p's backing array: retaining it, unlike
// retaining a *Connection, cannot keep the pool's array reachable.
func (p *Pool) Info(idx int) (ConnInfo, error) {
	if idx < 0 || idx >= len(p.conns) {
		return ConnInfo{}, fmt.Errorf("%w: %d", ErrIndexRange, idx)
	}
	c := p.conns[idx]
	return ConnInfo{ID: c.ID, BufSize: len(c.Buf)}, nil
}

// InfoAll returns a summary of every connection in the pool, such as a
// health-check endpoint would report. Like Info, the returned slice and
// every ConnInfo in it are freshly allocated: the slice has exact
// capacity for len(p.conns) entries, and none of them alias p's storage,
// so the result can be retained, logged, or handed to another goroutine
// with no effect on the pool.
func (p *Pool) InfoAll() []ConnInfo {
	out := make([]ConnInfo, 0, len(p.conns))
	for _, c := range p.conns {
		out = append(out, ConnInfo{ID: c.ID, BufSize: len(c.Buf)})
	}
	return out
}
```

### Using it

Construct one `Pool` at startup with the number of connections you plan to
hold. `Write` is the operational path -- append bytes to a specific
connection's buffer -- and `Info`/`InfoAll` are the only ways to look at a
connection's state from outside the package. Both always hand back plain
values, never anything that shares memory with `p.conns`, which is exactly
what makes them safe to log, store, or send across a channel without
worrying about what else that retention might be holding open.

`ExamplePool_Info`, in the test file below, is the runnable demonstration
of this module: `go test` runs it and compares its output against the
`// Output:` comment. The aliasing contract worth internalizing before
extending this package: any new accessor must follow `Info`'s shape --
build a fresh value from the fields you need, never return `&p.conns[i]`
or a sub-slice of `p.conns` to a caller, no matter how small the requested
piece looks.

### Tests

`TestNewPoolRejectsNonPositiveSize` and `TestWriteAndInfoRejectOutOfRange`
cover the constructor's and the two accessors' edges: a non-positive size,
and a negative, exactly-out-of-bounds, and far-out-of-bounds index.
`TestInfoReflectsWrites` and `TestInfoAll` pin ordinary correctness,
including that `InfoAll`'s result has exact length and capacity for the
pool's size.

`TestRawPointerPinsWholeArray` is the module's center of gravity. `connAt`
is unexported and unreachable from the package API; it is the `&p.conns[idx]`
antipattern the package doc comment warns against. The test weakly tracks
connection 0 specifically -- the one connection nobody ever takes a direct
reference to -- then takes a raw pointer to connection 4 via `connAt`,
drops every other reference to the pool, forces a full GC (twice, so the
second completes the first's sweep), and asserts connection 0 is *still*
reachable: proof that the raw pointer to connection 4 is keeping the whole
array, connection 0 included, alive. `runtime.KeepAlive` on the retained
raw pointer, placed after the check, guards against a subtler failure: with
no later use of the pointer in the test function, the compiler's liveness
analysis could otherwise treat it as dead before the GC runs, collecting
everything for a reason that has nothing to do with the code under test.
`TestInfoDoesNotPinTheArray` and `TestInfoAllDoesNotPinTheArray` run the
identical setup through `Info` and `InfoAll` instead and assert the
opposite: once every other reference to the pool is dropped, connection 0
is collected, because neither accessor's result shares any memory with
`p.conns`.

Create `connpool_test.go`:

```go
package connpool

import (
	"errors"
	"fmt"
	"runtime"
	"testing"
	"weak"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewPoolRejectsNonPositiveSize(t *testing.T) {
	t.Parallel()
	for _, size := range []int{0, -1} {
		if _, err := NewPool(size); !errors.Is(err, ErrInvalidSize) {
			t.Errorf("NewPool(%d) error = %v, want ErrInvalidSize", size, err)
		}
	}
}

func TestWriteAndInfoRejectOutOfRange(t *testing.T) {
	t.Parallel()
	p, err := NewPool(3)
	must(t, err)
	for _, idx := range []int{-1, 3, 100} {
		if err := p.Write(idx, []byte("x")); !errors.Is(err, ErrIndexRange) {
			t.Errorf("Write(%d) error = %v, want ErrIndexRange", idx, err)
		}
		if _, err := p.Info(idx); !errors.Is(err, ErrIndexRange) {
			t.Errorf("Info(%d) error = %v, want ErrIndexRange", idx, err)
		}
	}
}

func TestInfoReflectsWrites(t *testing.T) {
	t.Parallel()
	p, err := NewPool(2)
	must(t, err)
	must(t, p.Write(1, []byte("hello")))
	info, err := p.Info(1)
	must(t, err)
	if info.ID != 1 || info.BufSize != 5 {
		t.Fatalf("Info(1) = %+v, want {ID:1 BufSize:5}", info)
	}
}

func TestInfoAll(t *testing.T) {
	t.Parallel()
	p, err := NewPool(3)
	must(t, err)
	must(t, p.Write(2, []byte("abc")))

	all := p.InfoAll()
	if len(all) != 3 || cap(all) != 3 {
		t.Fatalf("InfoAll() len=%d cap=%d, want len=cap=3", len(all), cap(all))
	}
	for i, info := range all {
		if info.ID != i {
			t.Fatalf("InfoAll()[%d].ID = %d, want %d", i, info.ID, i)
		}
	}
	if all[2].BufSize != 3 {
		t.Fatalf("InfoAll()[2].BufSize = %d, want 3", all[2].BufSize)
	}
}

// TestInfoAllDoesNotPinTheArray extends the contrast to the bulk path:
// retaining InfoAll's result after every other reference to the pool is
// dropped still lets the whole array be collected.
func TestInfoAllDoesNotPinTheArray(t *testing.T) {
	p, err := NewPool(5)
	must(t, err)
	w := weak.Make(&p.conns[0])

	all := p.InfoAll()
	p = nil

	runtime.GC()
	runtime.GC()

	reachable := w.Value() != nil
	runtime.KeepAlive(all)
	if reachable {
		t.Fatal("expected the pool's array to be collected once InfoAll's independent copy was the only thing retained, but it is still reachable")
	}
}

// connAt is the antipattern this module warns against: a raw pointer into
// the pool's backing array, exported nowhere in the package API. It exists
// so the tests can pin the whole-array retention it causes.
func connAt(p *Pool, idx int) *Connection {
	return &p.conns[idx]
}

// TestRawPointerPinsWholeArray is the heart of the module. w tracks
// connection 0 weakly. Nothing ever takes a direct reference to
// connection 0 -- only to connection 4, via connAt -- yet after every
// other reference to the pool is dropped, connection 0 is still
// reachable, because connAt's raw pointer and connection 0 share one
// backing array.
func TestRawPointerPinsWholeArray(t *testing.T) {
	p, err := NewPool(5)
	must(t, err)
	w := weak.Make(&p.conns[0])

	raw := connAt(p, 4)
	p = nil

	runtime.GC()
	runtime.GC()

	collected := w.Value() == nil
	runtime.KeepAlive(raw)
	if collected {
		t.Fatal("expected connection 0 to still be reachable through the raw pointer's shared array, but it was collected")
	}
}

// TestInfoDoesNotPinTheArray is the contrast: Info copies out a value, so
// retaining it after every other reference to the pool is dropped lets
// the whole array be collected.
func TestInfoDoesNotPinTheArray(t *testing.T) {
	p, err := NewPool(5)
	must(t, err)
	w := weak.Make(&p.conns[0])

	info, err := p.Info(4)
	must(t, err)
	p = nil

	runtime.GC()
	runtime.GC()

	reachable := w.Value() != nil
	runtime.KeepAlive(info)
	if reachable {
		t.Fatal("expected connection 0 to be collected once Info's independent copy was the only thing retained, but it is still reachable")
	}
}

// ExamplePool_Info is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExamplePool_Info() {
	p, err := NewPool(3)
	if err != nil {
		panic(err)
	}
	if err := p.Write(1, []byte("hello")); err != nil {
		panic(err)
	}

	info, err := p.Info(1)
	if err != nil {
		panic(err)
	}
	fmt.Printf("connection %d has %d buffered bytes\n", info.ID, info.BufSize)

	for _, all := range p.InfoAll() {
		fmt.Printf("pool entry %d: %d bytes\n", all.ID, all.BufSize)
	}

	// Output:
	// connection 1 has 5 buffered bytes
	// pool entry 0: 0 bytes
	// pool entry 1: 5 bytes
	// pool entry 2: 0 bytes
}
```

## Review

`Info` and `InfoAll` are correct when their result shares no memory with
`p.conns` at all -- not "shares a small window of it," none. That is what
makes them safe to retain past the moment the pool itself might be resized,
replaced, or dropped. `NewPool` rejects a non-positive size with
`ErrInvalidSize`, and `Write`/`Info` reject an out-of-range index with
`ErrIndexRange`, both checkable with `errors.Is`. The dangerous shape,
`&p.conns[idx]`, is confined to the test file as `connAt`, never offered as
part of the package API -- the only way to reproduce the pin is to write
that one line yourself, which is exactly what the contrast tests do to
prove why the package never does. The proof itself generalizes past this
module: any time you need to show that a *specific* small value keeps a
*much larger* structure reachable, a `weak.Pointer` on the small value plus
two `runtime.GC()` calls is the tool, and `runtime.KeepAlive` on whatever
your test's own local variables retain is what stops the compiler's
liveness analysis from invalidating the measurement for reasons unrelated
to the code under test. Run `go test -count=1 -race ./...`.

## Resources

- [Go Spec: Address operators](https://go.dev/ref/spec#Address_operators) — `&s[i]` yields a pointer into `s`'s backing array, not a copy of the element.
- [`weak.Pointer`](https://pkg.go.dev/weak) — the Go 1.24 API used here to observe whether a value survives a GC without extending its lifetime.
- [`runtime.KeepAlive`](https://pkg.go.dev/runtime#KeepAlive) — why a variable can become unreachable before its lexical scope ends, and how to pin it across a measurement window.
- [Go Wiki: SliceTricks](https://go.dev/wiki/SliceTricks) — a working reference for slice operations, useful for recognizing which idioms alias and which copy.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [15-memtable-compact-clip-guard.md](15-memtable-compact-clip-guard.md) | Next: [17-replay-queue-ack-compaction.md](17-replay-queue-ack-compaction.md)
