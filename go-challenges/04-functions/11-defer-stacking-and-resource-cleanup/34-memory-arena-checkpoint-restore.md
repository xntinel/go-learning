# Exercise 34: Memory Arena Checkpoint — Restore Allocation Position on Error

**Nivel: Intermedio** — validacion rapida (un test corto).

A bump-allocator arena hands out memory by advancing a position marker
through a fixed buffer, and never frees anything individually — the only
way to reclaim space is to rewind the position back to an earlier point. A
multi-step allocation sequence (build a record's fields one at a time,
say) that fails partway through should give back every byte it took, and a
checkpoint-and-restore defer is exactly the tool: snapshot the position
before the sequence, defer a rewind that only fires if the sequence
returns an error.

## What you'll build

```text
arena/                        independent module: example.com/arena
  go.mod
  arena/arena.go                Arena (bump allocator); Transaction (checkpoint + deferred rewind)
  cmd/demo/main.go               a failed multi-step allocation, then a successful one
  arena/arena_test.go            success keeps allocations; error rewinds; alloc-fails-midway rewinds; capacity boundary
```

- Files: `arena/arena.go`, `cmd/demo/main.go`, `arena/arena_test.go`.
- Implement: an `Arena` over a fixed-size `[]byte` with a `pos int`; `Alloc(n int) ([]byte, error)`, which returns an error (leaving `pos` unchanged) if `n` bytes do not fit in the remaining capacity; and `Transaction(fn func() error) (err error)`, which snapshots `pos` as a checkpoint and defers a closure that resets `pos` back to it if `fn` returns a non-nil error.
- Test: a transaction whose `fn` allocates twice and succeeds keeps both allocations; a transaction whose `fn` fails after allocating rewinds `pos` back to the checkpoint; a transaction that fails because a nested `Alloc` itself runs out of space still rewinds, and the reclaimed space is provably reusable afterward; `Alloc` exactly at the capacity boundary succeeds, and one byte past it fails without moving `pos`.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/11-defer-stacking-and-resource-cleanup/34-memory-arena-checkpoint-restore/arena go-solutions/04-functions/11-defer-stacking-and-resource-cleanup/34-memory-arena-checkpoint-restore/cmd/demo
cd go-solutions/04-functions/11-defer-stacking-and-resource-cleanup/34-memory-arena-checkpoint-restore
go mod edit -go=1.24
```

### A position marker, not individual frees

A bump allocator's whole appeal is that allocation is just "advance an
integer" — no free list, no per-allocation bookkeeping. That simplicity
has a cost: there is no way to give back one allocation out of several
without giving back everything allocated after it too, because nothing
records where any individual allocation's boundaries were except the
position marker's value at the time. `Transaction` embraces that: it does
not try to undo individual `Alloc` calls, it just remembers `pos` from
before `fn` ran and, if `fn` reports failure, sets `pos` straight back to
that single remembered number. Every byte `fn` claimed in between —
whether from one `Alloc` call or ten — is reclaimed in that one assignment,
because "reclaimed" for a bump allocator simply means "the next `Alloc`
is allowed to overwrite it."

The `defer` is what makes the rewind unconditional on `fn`'s *outcome*
rather than requiring `fn` to remember to signal "please rewind me" through
some other channel. `Transaction`'s own named return `err` is set by
whatever `fn` returns; the deferred closure reads that same `err` after
`fn` has already run, and only touches `pos` if it is non-nil. On success,
the closure still runs (defers always do) but finds `err` is `nil` and
leaves `pos` exactly where `fn` left it.

Create `arena/arena.go`:

```go
package arena

import "fmt"

// Arena is a simple bump allocator over a fixed-capacity buffer: Alloc hands
// out successive slices and advances the position; nothing is ever freed
// individually, only rewound back to an earlier position.
type Arena struct {
	buf []byte
	pos int
}

// New returns an Arena with the given fixed capacity.
func New(capacity int) *Arena {
	return &Arena{buf: make([]byte, capacity)}
}

// Pos returns the current allocation position (bytes used so far).
func (a *Arena) Pos() int { return a.pos }

// Cap returns the arena's total capacity.
func (a *Arena) Cap() int { return len(a.buf) }

// Alloc reserves n bytes from the arena, advancing the position, and
// returns an error if the arena does not have n bytes of remaining capacity
// (the position is left unchanged on failure).
func (a *Arena) Alloc(n int) ([]byte, error) {
	if n < 0 || a.pos+n > len(a.buf) {
		return nil, fmt.Errorf("arena: out of space: want %d, have %d", n, len(a.buf)-a.pos)
	}
	b := a.buf[a.pos : a.pos+n]
	a.pos += n
	return b, nil
}

// Transaction snapshots the arena's current position as a checkpoint, runs
// fn, and -- via a deferred closure -- rewinds the arena back to that
// checkpoint if fn returns a non-nil error. Every byte allocated during a
// failed fn is reclaimed this way; on success the allocations made during fn
// are kept.
func (a *Arena) Transaction(fn func() error) (err error) {
	checkpoint := a.pos
	defer func() {
		if err != nil {
			a.pos = checkpoint
		}
	}()

	return fn()
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/arena/arena"
)

func main() {
	a := arena.New(64)

	// Allocate a small header outside any transaction.
	if _, err := a.Alloc(8); err != nil {
		panic(err)
	}
	fmt.Println("pos after header:", a.Pos())

	// A multi-step record build that fails partway: everything it
	// allocated is reclaimed.
	err := a.Transaction(func() error {
		if _, err := a.Alloc(16); err != nil {
			return err
		}
		if _, err := a.Alloc(100); err != nil { // too big: fails
			return err
		}
		return nil
	})
	fmt.Println("failed transaction err:", err)
	fmt.Println("pos after failed transaction (rewound):", a.Pos())

	// A multi-step record build that succeeds: allocations are kept.
	err = a.Transaction(func() error {
		if _, err := a.Alloc(16); err != nil {
			return err
		}
		if _, err := a.Alloc(8); err != nil {
			return err
		}
		return nil
	})
	fmt.Println("succeeded transaction err:", err)
	fmt.Println("pos after succeeded transaction:", a.Pos())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
pos after header: 8
failed transaction err: arena: out of space: want 100, have 40
pos after failed transaction (rewound): 8
succeeded transaction err: <nil>
pos after succeeded transaction: 32
```

### Tests

Create `arena/arena_test.go`:

```go
package arena

import (
	"errors"
	"testing"
)

func TestTransactionKeepsAllocationsOnSuccess(t *testing.T) {
	t.Parallel()

	a := New(32)
	err := a.Transaction(func() error {
		if _, err := a.Alloc(10); err != nil {
			return err
		}
		if _, err := a.Alloc(5); err != nil {
			return err
		}
		return nil
	})

	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got := a.Pos(); got != 15 {
		t.Fatalf("Pos() = %d, want 15", got)
	}
}

func TestTransactionRewindsOnError(t *testing.T) {
	t.Parallel()

	a := New(32)
	if _, err := a.Alloc(4); err != nil {
		t.Fatalf("setup Alloc(4) err = %v", err)
	}
	checkpoint := a.Pos()

	boom := errors.New("record validation failed")
	err := a.Transaction(func() error {
		if _, err := a.Alloc(10); err != nil {
			return err
		}
		return boom
	})

	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want errors.Is %v", err, boom)
	}
	if got := a.Pos(); got != checkpoint {
		t.Fatalf("Pos() = %d, want %d (rewound to checkpoint)", got, checkpoint)
	}
}

func TestTransactionRewindsWhenAllocFailsMidway(t *testing.T) {
	t.Parallel()

	a := New(16)
	checkpoint := a.Pos()

	err := a.Transaction(func() error {
		if _, err := a.Alloc(10); err != nil {
			return err
		}
		if _, err := a.Alloc(10); err != nil { // exceeds remaining capacity
			return err
		}
		return nil
	})

	if err == nil {
		t.Fatal("err = nil, want an out-of-space error")
	}
	if got := a.Pos(); got != checkpoint {
		t.Fatalf("Pos() = %d, want %d: space reclaimed after failed transaction", got, checkpoint)
	}

	// The reclaimed space must actually be reusable.
	if _, err := a.Alloc(16); err != nil {
		t.Fatalf("Alloc(16) after rewind = %v, want nil", err)
	}
}

func TestAllocFailsAtCapacityBoundary(t *testing.T) {
	t.Parallel()

	a := New(4)
	if _, err := a.Alloc(4); err != nil {
		t.Fatalf("Alloc(4) = %v, want nil (exactly fills capacity)", err)
	}
	if _, err := a.Alloc(1); err == nil {
		t.Fatal("Alloc(1) = nil, want error: arena is already full")
	}
	if got := a.Pos(); got != 4 {
		t.Fatalf("Pos() = %d, want 4 (unchanged by the failed Alloc)", got)
	}
}
```

## Review

The arena is correct when a failed transaction's `pos` lands back exactly
where it started — not approximately, not "close enough" — and when the
space that rewind frees up is provably reusable by a subsequent `Alloc`,
not just cosmetically zeroed. The mistake this pattern exists to prevent
is tracking "how much to undo" as a running count of successful `Alloc`
calls added up after the fact, which has to be recomputed correctly at
every possible failure point inside `fn`; a single snapshot taken once,
before `fn` runs at all, sidesteps that arithmetic entirely — there is only
one number to remember, and it does not matter how many allocations
happened or in what order between taking it and restoring it.

## Resources

- [The Go Programming Language Specification: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [Slice internals: len and cap](https://go.dev/blog/slices-intro)
- [Bump allocator (Wikipedia: region-based memory management)](https://en.wikipedia.org/wiki/Region-based_memory_management)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [33-goroutine-cancel-panic-unwinding.md](33-goroutine-cancel-panic-unwinding.md) | Next: [../12-functional-options-pattern/00-concepts.md](../12-functional-options-pattern/00-concepts.md)
