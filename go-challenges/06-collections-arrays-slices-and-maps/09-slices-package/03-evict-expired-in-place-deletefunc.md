# Exercise 3: Evict Expired Entries From A Connection Pool In Place (DeleteFunc)

A slice-backed connection pool needs a periodic sweep that removes entries whose
deadline has passed, keeps the survivors in their existing order, and reuses the
same backing array so the sweep does not allocate. `slices.DeleteFunc` is the
single-pass, O(n) tool for exactly this — and the trap it replaces is calling
`slices.Delete` once per expired entry inside a loop, which is O(n^2) and shifts
indices under your own cursor.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
poolsweep/                     module example.com/poolsweep
  go.mod                       go 1.24
  pool.go                      type Conn, Pool; Evict (DeleteFunc), naive filter for contrast
  cmd/
    demo/
      main.go                  runnable demo: sweep expired conns at a reference time
  pool_test.go                 all/none/alternating/boundary, order preserved, tail zeroed
```

- Files: `pool.go`, `cmd/demo/main.go`, `pool_test.go`.
- Implement: `(*Pool).Evict(now time.Time) int` using `slices.DeleteFunc` to drop entries whose `Deadline` is before `now`, returning the count evicted; survivors keep order and the freed tail is zeroed.
- Test: all-expired, none-expired, alternating, boundary equal-to-now; survivor order preserved; correct evicted count; freed `*Conn` slots zeroed to nil.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/poolsweep/cmd/demo
cd ~/go-exercises/poolsweep
go mod init example.com/poolsweep
go mod edit -go=1.24
```

### Why DeleteFunc, and the boundary that bites

`slices.DeleteFunc(s, del)` walks the slice once, keeping every element for which
`del` returns false and compacting them toward the front, then zeroes the freed
tail and returns the shortened slice. It preserves the relative order of the
survivors — important for a pool where entries may be ordered by creation or
last-use — and it touches the backing array once. The naive alternative is a
`for` loop that calls `slices.Delete(s, i, i+1)` each time it finds an expired
entry; that is quadratic (each `Delete` shifts the whole tail) and it corrupts
the iteration because deleting at `i` slides the next element into `i`, which the
loop then skips. `DeleteFunc` exists precisely so you never write that loop.

The predicate decides expiry with `entry.Deadline.Before(now)`. The boundary
matters: `Before` is strict, so an entry whose deadline is *exactly* `now` is
NOT before `now` and is kept. That is the right call for a deadline — the
connection is valid up to and including its deadline instant — but it is a
decision you must make deliberately and test, because `Before` versus a
hypothetical `!After` differs by exactly the boundary case. The test pins an
entry at `now` and asserts it survives.

`Evict` takes `now` as a parameter rather than calling `time.Now()` internally.
That is the testable-sweep pattern: the caller (a ticker goroutine) passes
`time.Now()`, but a test passes a fixed instant so the boundary is exact and the
result is deterministic. The pool stores `[]*Conn`, so `DeleteFunc` zeroing the
tail drops references to evicted connections and lets them be collected (and, in
real code, lets a `Close` on them not be blocked by a lingering array reference).

Create `pool.go`:

```go
package poolsweep

import (
	"slices"
	"time"
)

// Conn is a pooled connection with an expiry Deadline.
type Conn struct {
	ID       string
	Deadline time.Time
}

// Pool is a slice-backed connection pool swept in place.
type Pool struct {
	conns []*Conn
}

// NewPool builds a pool over the given connections (order is significant).
func NewPool(conns []*Conn) *Pool {
	return &Pool{conns: conns}
}

// Len reports the number of live connections.
func (p *Pool) Len() int { return len(p.conns) }

// IDs returns the live connection ids in order (for inspection and tests).
func (p *Pool) IDs() []string {
	out := make([]string, len(p.conns))
	for i, c := range p.conns {
		out[i] = c.ID
	}
	return out
}

// Evict removes connections whose Deadline is strictly before now, preserving
// the order of survivors and reusing the backing array. A connection deadlined
// exactly at now is kept. It returns the number evicted.
func (p *Pool) Evict(now time.Time) int {
	before := len(p.conns)
	p.conns = slices.DeleteFunc(p.conns, func(c *Conn) bool {
		return c.Deadline.Before(now)
	})
	return before - len(p.conns)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/poolsweep"
)

func main() {
	base := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	p := poolsweep.NewPool([]*poolsweep.Conn{
		{ID: "c1", Deadline: base.Add(-time.Minute)}, // expired
		{ID: "c2", Deadline: base.Add(time.Minute)},  // live
		{ID: "c3", Deadline: base},                   // exactly now: kept
		{ID: "c4", Deadline: base.Add(-time.Second)}, // expired
	})
	fmt.Printf("before: %v\n", p.IDs())

	n := p.Evict(base)
	fmt.Printf("evicted: %d\n", n)
	fmt.Printf("after:  %v\n", p.IDs())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
before: [c1 c2 c3 c4]
evicted: 2
after:  [c2 c3]
```

`c1` and `c4` are strictly before `base` and go; `c3` is exactly at `base` and
stays (deadline is inclusive); the survivors `c2`, `c3` keep their relative order.

### Tests

`TestEvictCases` is table-driven over all-expired, none-expired, alternating, and
the boundary-equal-to-now case, asserting both the survivor ids (order preserved)
and the evicted count. `TestEvictZeroesTail` proves the freed `*Conn` slots are
nil after the sweep so evicted connections are not retained.

Create `pool_test.go`:

```go
package poolsweep

import (
	"slices"
	"testing"
	"time"
)

func TestEvictCases(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	mk := func(id string, off time.Duration) *Conn {
		return &Conn{ID: id, Deadline: now.Add(off)}
	}

	cases := []struct {
		name      string
		conns     []*Conn
		wantIDs   []string
		wantEvict int
	}{
		{
			name:      "all expired",
			conns:     []*Conn{mk("a", -time.Hour), mk("b", -time.Minute)},
			wantIDs:   []string{},
			wantEvict: 2,
		},
		{
			name:      "none expired",
			conns:     []*Conn{mk("a", time.Minute), mk("b", time.Hour)},
			wantIDs:   []string{"a", "b"},
			wantEvict: 0,
		},
		{
			name:      "alternating keeps order",
			conns:     []*Conn{mk("a", -time.Minute), mk("b", time.Minute), mk("c", -time.Minute), mk("d", time.Minute)},
			wantIDs:   []string{"b", "d"},
			wantEvict: 2,
		},
		{
			name:      "deadline exactly now is kept",
			conns:     []*Conn{mk("a", 0), mk("b", -time.Nanosecond)},
			wantIDs:   []string{"a"},
			wantEvict: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := NewPool(tc.conns)
			got := p.Evict(now)
			if got != tc.wantEvict {
				t.Fatalf("Evict returned %d, want %d", got, tc.wantEvict)
			}
			if !slices.Equal(p.IDs(), tc.wantIDs) {
				t.Fatalf("survivors = %v, want %v", p.IDs(), tc.wantIDs)
			}
		})
	}
}

func TestEvictZeroesTail(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	conns := []*Conn{
		{ID: "a", Deadline: now.Add(time.Minute)},
		{ID: "b", Deadline: now.Add(-time.Minute)},
		{ID: "c", Deadline: now.Add(-time.Minute)},
	}
	oldCap := cap(conns)
	p := NewPool(conns)
	p.Evict(now)

	full := p.conns[:oldCap]
	for i := len(p.conns); i < oldCap; i++ {
		if full[i] != nil {
			t.Fatalf("freed slot [%d] = %v, want nil (evicted conn retained)", i, full[i])
		}
	}
}
```

## Review

The sweep is correct when survivors keep their input order, the evicted count
equals the number of strictly-expired entries, and an entry deadlined exactly at
`now` survives. The single `DeleteFunc` pass is what makes it O(n); if you ever
see a `for` loop calling `slices.Delete` inside it, that is the O(n^2) trap and
also skips elements because deletion shifts the tail into the current index.
Passing `now` in rather than calling `time.Now()` is what makes the boundary case
testable to the nanosecond. The tail-zeroing test guards the GC contract for the
`*Conn` pointers. Run `go test -race`; the parallel table cases each own their
pool, so there is no shared state to race on.

## Resources

- [`slices.DeleteFunc`](https://pkg.go.dev/slices#DeleteFunc) — single-pass in-place filter that zeroes the freed tail.
- [`slices.Delete`](https://pkg.go.dev/slices#Delete) — the index-range delete the loop trap misuses.
- [`time.Time.Before`](https://pkg.go.dev/time#Time.Before) — the strict comparison behind the inclusive-deadline boundary.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-sorted-index-insert-binarysearchfunc.md](04-sorted-index-insert-binarysearchfunc.md)
