# Exercise 7: Evicting a Pooled Connection Without Leaking the Tail Pointer

A connection or session pool holds a slice of pointers and evicts entries by
index. If you remove one with a naive shift, the freed tail slot still holds the
evicted pointer, and the object it points to can never be garbage-collected — a
slow leak in a long-lived pool. This exercise uses `slices.Delete`, which zeroes
the freed tail slot (Go 1.22+), so the evicted connection becomes GC-eligible.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
connpool/                   independent module: example.com/connpool
  go.mod
  connpool.go               type Pool of *Conn; Evict (slices.Delete); EvictNaive (leaks)
  cmd/
    demo/
      main.go               runnable demo: evict, show order preserved and tail zeroed
  connpool_test.go          order-preserving removal; tail slot nil'd; naive leaks
```

Files: `connpool.go`, `cmd/demo/main.go`, `connpool_test.go`.
Implement: a `Pool` holding `[]*Conn`, `Evict(i)` using `slices.Delete`, `Insert` using `slices.Insert`, and a contrasting `EvictNaive(i)` that shifts manually and leaks.
Test: after `Evict`, order is preserved and `len` drops by 1; the freed tail slot is `nil` (GC-eligible); the naive version leaves a stale non-nil pointer in the tail.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/connpool/cmd/demo
cd ~/go-exercises/connpool
go mod init example.com/connpool
```

### The tail-pointer leak and how slices.Delete fixes it

`slices.Delete(s, i, j)` removes elements `[i:j)` by copying the tail
(`s[j:]`) left over the gap and returning `s[:len(s)-(j-i)]`. For a slice of
pointers this shift leaves the *old* last slots holding copies of pointers that
are now logically gone. If those slots kept their old values, the pointed-at
objects would remain reachable through the backing array even though they are no
longer part of the logical slice — a leak the GC cannot see through. Since Go
1.22, `slices.Delete` **zeroes** those freed tail slots (writes `nil` into them)
precisely so the evicted pointers become collectable.

You can observe the zeroing directly. After `pool = slices.Delete(pool, i, i+1)`,
reslice back into the freed region with `pool[:len(pool)+1]` and read the last
slot: it is `nil`. The `EvictNaive` version does the shift by hand
(`copy(s[i:], s[i+1:]); s = s[:len(s)-1]`) and *skips* the zeroing, so the same
reslice reveals a stale, still-live pointer — the leak. This is not a
micro-optimization: in a pool that churns connections for the lifetime of a
process, a leaked pointer per eviction accumulates into unbounded retained
memory.

`Insert` is the mirror operation: `slices.Insert(s, i, v...)` opens a gap at `i`
by shifting the tail right and writes the new values, growing the slice. Both
`Delete` and `Insert` operate in place on the backing array and return the
resliced result, so you must assign the return value back.

Create `connpool.go`:

```go
package connpool

import "slices"

// Conn is a stand-in for a pooled connection or session. It is referenced by
// pointer, so a leaked pointer in an evicted slot keeps the whole Conn alive.
type Conn struct {
	ID     int
	closed bool
}

// Close marks the connection closed; a pool must not retain a pointer to a
// closed Conn, or the memory leaks.
func (c *Conn) Close() { c.closed = true }

// Pool holds active connections in insertion order.
type Pool struct {
	conns []*Conn
}

// Add appends a connection to the pool.
func (p *Pool) Add(c *Conn) { p.conns = append(p.conns, c) }

// Len reports the number of pooled connections.
func (p *Pool) Len() int { return len(p.conns) }

// At returns the connection at index i.
func (p *Pool) At(i int) *Conn { return p.conns[i] }

// Evict removes the connection at index i, preserving order. It uses
// slices.Delete, which zeroes the freed tail slot so the evicted *Conn becomes
// garbage-collectable.
func (p *Pool) Evict(i int) {
	p.conns[i].Close()
	p.conns = slices.Delete(p.conns, i, i+1)
}

// InsertAt inserts c at index i, preserving order (slices.Insert).
func (p *Pool) InsertAt(i int, c *Conn) {
	p.conns = slices.Insert(p.conns, i, c)
}

// tailAfterEvict reslices back into the freed region to inspect the slot that
// Evict vacated. It is used by tests to prove the slot was zeroed.
func (p *Pool) tailAfterEvict() *Conn {
	return p.conns[:len(p.conns)+1][len(p.conns)]
}

// EvictNaive removes index i with a manual shift and does NOT zero the freed
// tail slot, leaking the evicted pointer. Kept as a contrast; do not use it.
func EvictNaive(s []*Conn, i int) []*Conn {
	copy(s[i:], s[i+1:])
	return s[:len(s)-1] // WRONG: old last slot still holds the evicted pointer
}
```

### The runnable demo

The demo builds a pool of four connections, evicts the second, and shows order is
preserved and the freed tail slot is nil, then shows the naive version leaking a
pointer.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/connpool"
)

func main() {
	var p connpool.Pool
	for _, id := range []int{1, 2, 3, 4} {
		p.Add(&connpool.Conn{ID: id})
	}

	p.Evict(1) // remove ID 2
	fmt.Printf("after evict: len=%d ids=[%d %d %d]\n", p.Len(), p.At(0).ID, p.At(1).ID, p.At(2).ID)

	// Naive shift leaves a stale pointer in the tail.
	conns := []*connpool.Conn{{ID: 1}, {ID: 2}, {ID: 3}}
	conns = connpool.EvictNaive(conns, 1)
	leaked := conns[:len(conns)+1][len(conns)]
	fmt.Printf("naive tail leaked pointer: %v\n", leaked != nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
after evict: len=3 ids=[1 3 4]
naive tail leaked pointer: true
```

### Tests

`TestEvictPreservesOrder` checks order-preserving removal and `len - 1`.
`TestEvictZeroesTail` proves the freed slot is `nil` (GC-eligible).
`TestNaiveEvictLeaksTail` proves the manual shift leaves a stale pointer.

Create `connpool_test.go`:

```go
package connpool

import (
	"fmt"
	"testing"
)

func buildPool(ids ...int) *Pool {
	var p Pool
	for _, id := range ids {
		p.Add(&Conn{ID: id})
	}
	return &p
}

func TestEvictPreservesOrder(t *testing.T) {
	t.Parallel()
	p := buildPool(1, 2, 3, 4)
	p.Evict(1) // remove ID 2
	if p.Len() != 3 {
		t.Fatalf("len = %d, want 3", p.Len())
	}
	want := []int{1, 3, 4}
	for i, id := range want {
		if p.At(i).ID != id {
			t.Fatalf("conns[%d].ID = %d, want %d", i, p.At(i).ID, id)
		}
	}
}

func TestEvictZeroesTail(t *testing.T) {
	t.Parallel()
	p := buildPool(1, 2, 3, 4)
	p.Evict(1)
	if slot := p.tailAfterEvict(); slot != nil {
		t.Fatalf("freed tail slot = %v, want nil (slices.Delete must zero it)", slot)
	}
}

func TestEvictClosesConnection(t *testing.T) {
	t.Parallel()
	p := buildPool(1, 2, 3)
	evicted := p.At(1)
	p.Evict(1)
	if !evicted.closed {
		t.Fatal("Evict must Close the removed connection")
	}
}

func TestNaiveEvictLeaksTail(t *testing.T) {
	t.Parallel()
	conns := []*Conn{{ID: 1}, {ID: 2}, {ID: 3}}
	conns = EvictNaive(conns, 1)
	leaked := conns[:len(conns)+1][len(conns)]
	if leaked == nil {
		t.Fatal("expected the naive shift to leave a stale pointer in the tail")
	}
	if leaked.ID != 3 {
		t.Fatalf("leaked tail pointer ID = %d, want 3", leaked.ID)
	}
}

func TestInsertAtPreservesOrder(t *testing.T) {
	t.Parallel()
	p := buildPool(1, 2, 4)
	p.InsertAt(2, &Conn{ID: 3})
	want := []int{1, 2, 3, 4}
	if p.Len() != len(want) {
		t.Fatalf("len = %d, want %d", p.Len(), len(want))
	}
	for i, id := range want {
		if p.At(i).ID != id {
			t.Fatalf("conns[%d].ID = %d, want %d", i, p.At(i).ID, id)
		}
	}
}

func ExamplePool_Evict() {
	p := buildPool(1, 2, 3, 4)
	p.Evict(1)
	fmt.Println(p.Len(), p.At(0).ID, p.At(1).ID, p.At(2).ID)
	// Output: 3 1 3 4
}
```

## Review

Eviction is correct when it preserves order, drops `len` by one, and — the part
that matters for a long-lived pool — leaves the freed tail slot `nil`.
`slices.Delete` does all three, zeroing the vacated slot (Go 1.22+) so the
evicted `*Conn` is collectable; `TestEvictZeroesTail` reads that slot through a
reslice-into-capacity and asserts `nil`. `EvictNaive` shows the leak: a manual
`copy` + reslice skips the zeroing, so `TestNaiveEvictLeaksTail` finds a live
pointer stranded in the tail. Always assign the result of `slices.Delete`/
`slices.Insert` back; they reslice in place and return the new header. Run
`go test -race`.

## Resources

- [pkg.go.dev: slices.Delete](https://pkg.go.dev/slices#Delete) — removes a range and zeroes the freed tail (Go 1.22+).
- [pkg.go.dev: slices.Insert](https://pkg.go.dev/slices#Insert) — the mirror operation.
- [Go 1.22 release notes: slices](https://go.dev/doc/go1.22) — the tail-zeroing change that prevents the pointer leak.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-config-snapshot-clone.md](08-config-snapshot-clone.md)
