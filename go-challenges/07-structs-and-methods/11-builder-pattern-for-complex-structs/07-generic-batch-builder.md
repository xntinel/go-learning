# Exercise 7: Generic Batch Accumulator Builder with Capacity Hints

Bulk operations — bulk insert, bulk publish — accumulate typed items up to a flush
threshold and then hand the whole batch off at once. That accumulator is the same
shape for every element type, so it is naturally generic. This module builds a
`Batch[T]` with a size guard, capacity hints, and copy-on-build ownership.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
batchbuild/                 independent module: example.com/batchbuild
  go.mod                    go 1.26
  batch.go                  package batch: Batch[T], New, Add, AddAll, Grow, Len, Build
  cmd/
    demo/
      main.go               runnable demo: accumulate users, flush a batch
  batch_test.go             two element types, ownership, over-limit, empty
```

- Files: `batch.go`, `cmd/demo/main.go`, `batch_test.go`.
- Implement: a generic `Batch[T any]` with `Add(T) error`, `AddAll(...T) error`, `Grow(int)`, `Len() int`, and `Build() []T` returning a `slices.Clone` so the caller owns the result. A `MaxSize` guard returns `ErrBatchFull` when an add would exceed the configured limit; the default limit comes from `cmp.Or`.
- Test: instantiate for `int` and for a struct; prove `Build` returns an independent copy; over-limit `Add`/`AddAll` returns `ErrBatchFull`; an empty batch builds to a non-nil empty slice.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/batchbuild/cmd/demo
cd ~/go-exercises/batchbuild
go mod init example.com/batchbuild
```

### Generics, ownership, and capacity in one accumulator

The type parameter `[T any]` lets one implementation serve `Batch[int]`,
`Batch[User]`, `Batch[[]byte]`, and anything else — the accumulation logic never
depends on what `T` is. `New[T](maxSize)` uses `cmp.Or(maxSize, defaultMax)` so a
zero argument falls back to a sane default instead of a zero-capacity batch that can
never accept an item.

Two operational details make this production-shaped. First, capacity hints: `Grow`
and the internal `slices.Grow` in `AddAll` pre-extend the backing array so a large
bulk add does not trigger a cascade of reallocations as it appends. Second,
ownership: `Build` returns `slices.Clone(b.items)`, not `b.items` itself. If it
returned the internal slice, the caller could append to it or mutate an element and
corrupt a subsequent `Build`, or a later `Add` could grow the shared backing array
out from under the caller. The clone severs that link — the returned batch is fully
owned. A subtle but documented consequence: `slices.Clone` of a nil slice is nil,
but of a non-nil empty slice is a non-nil empty slice; because `New` initializes the
buffer, an empty `Build` returns a non-nil empty slice, which is the friendlier
contract for a caller ranging over it.

The size guard is a boundary check: `Add` refuses once the batch is at `max`,
returning `ErrBatchFull`, and `AddAll` refuses the *whole* group if it would
overflow rather than partially filling — an all-or-nothing add is easier to reason
about at a flush threshold. This builder is not safe for concurrent use; a caller
that fills batches from multiple goroutines must guard it, or better, give each
goroutine its own `Batch`.

Create `batch.go`:

```go
package batch

import (
	"cmp"
	"errors"
	"slices"
)

// ErrBatchFull is returned when an add would exceed the configured max size.
var ErrBatchFull = errors.New("batch is full")

const defaultMax = 1000

// Batch accumulates items of any type up to a max size, then hands off an owned
// copy at Build. It is not safe for concurrent use.
type Batch[T any] struct {
	items []T
	max   int
}

// New returns a batch with the given max size; a zero maxSize uses the default.
func New[T any](maxSize int) *Batch[T] {
	m := cmp.Or(maxSize, defaultMax)
	return &Batch[T]{max: m, items: make([]T, 0, min(m, 64))}
}

// Add appends one item, or returns ErrBatchFull if the batch is at capacity.
func (b *Batch[T]) Add(item T) error {
	if len(b.items) >= b.max {
		return ErrBatchFull
	}
	b.items = append(b.items, item)
	return nil
}

// AddAll appends all items or none: if the group would overflow the max, it
// returns ErrBatchFull and adds nothing.
func (b *Batch[T]) AddAll(items ...T) error {
	if len(b.items)+len(items) > b.max {
		return ErrBatchFull
	}
	b.items = slices.Grow(b.items, len(items))
	b.items = append(b.items, items...)
	return nil
}

// Grow pre-extends the backing array to hold n more items without reallocating.
func (b *Batch[T]) Grow(n int) {
	b.items = slices.Grow(b.items, n)
}

// Len reports how many items are accumulated.
func (b *Batch[T]) Len() int {
	return len(b.items)
}

// Build returns an owned copy of the accumulated items. The caller may mutate
// the result without affecting the batch or a later Build.
func (b *Batch[T]) Build() []T {
	return slices.Clone(b.items)
}
```

### The runnable demo

The demo accumulates a batch of users for a bulk insert, then flushes it.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/batchbuild"
)

type user struct {
	ID    int
	Email string
}

func main() {
	b := batch.New[user](500)
	b.Grow(3)
	_ = b.AddAll(
		user{1, "a@example.com"},
		user{2, "b@example.com"},
	)
	_ = b.Add(user{3, "c@example.com"})

	rows := b.Build()
	fmt.Printf("flushing %d rows\n", len(rows))
	for _, u := range rows {
		fmt.Printf("  user %d %s\n", u.ID, u.Email)
	}

	small := batch.New[int](2)
	_ = small.Add(1)
	_ = small.Add(2)
	if err := small.Add(3); err != nil {
		fmt.Println("third add:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
flushing 3 rows
  user 1 a@example.com
  user 2 b@example.com
  user 3 c@example.com
third add: batch is full
```

### Tests

The tests instantiate the batch for two element types, prove the ownership contract
(mutating a built result never touches a later build), and pin the over-limit and
empty-batch behavior.

Create `batch_test.go`:

```go
package batch

import (
	"errors"
	"fmt"
	"testing"
)

type row struct {
	ID   int
	Name string
}

func TestBatchIntType(t *testing.T) {
	t.Parallel()

	b := New[int](10)
	for i := range 5 {
		if err := b.Add(i); err != nil {
			t.Fatal(err)
		}
	}
	got := b.Build()
	if len(got) != 5 {
		t.Fatalf("len = %d, want 5", len(got))
	}
	for i, v := range got {
		if v != i {
			t.Fatalf("got[%d] = %d, want %d", i, v, i)
		}
	}
}

func TestBatchStructType(t *testing.T) {
	t.Parallel()

	b := New[row](10)
	if err := b.AddAll(row{1, "a"}, row{2, "b"}); err != nil {
		t.Fatal(err)
	}
	got := b.Build()
	if len(got) != 2 || got[1].Name != "b" {
		t.Fatalf("got = %+v", got)
	}
}

func TestBuildIsIndependentCopy(t *testing.T) {
	t.Parallel()

	b := New[int](10)
	_ = b.AddAll(1, 2, 3)

	first := b.Build()
	first[0] = 99 // mutate the returned slice

	second := b.Build()
	if second[0] != 1 {
		t.Fatalf("second Build saw mutation: got %d, want 1", second[0])
	}
}

func TestAddOverLimit(t *testing.T) {
	t.Parallel()

	b := New[int](2)
	if err := b.Add(1); err != nil {
		t.Fatal(err)
	}
	if err := b.Add(2); err != nil {
		t.Fatal(err)
	}
	if err := b.Add(3); !errors.Is(err, ErrBatchFull) {
		t.Fatalf("err = %v, want ErrBatchFull", err)
	}
}

func TestAddAllOverLimitAddsNothing(t *testing.T) {
	t.Parallel()

	b := New[int](3)
	_ = b.Add(1)
	if err := b.AddAll(2, 3, 4); !errors.Is(err, ErrBatchFull) {
		t.Fatalf("err = %v, want ErrBatchFull", err)
	}
	if b.Len() != 1 {
		t.Fatalf("failed AddAll should add nothing; len = %d, want 1", b.Len())
	}
}

func TestEmptyBuildIsNonNil(t *testing.T) {
	t.Parallel()

	b := New[int](10)
	got := b.Build()
	if got == nil {
		t.Fatal("empty Build should return a non-nil empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("len = %d, want 0", len(got))
	}
}

func ExampleBatch_Build() {
	b := New[string](4)
	_ = b.AddAll("x", "y")
	fmt.Println(b.Build())
	// Output: [x y]
}
```

## Review

The batch is correct when it accumulates any element type, guards its size, and
hands back an owned copy. `TestBatchIntType` and `TestBatchStructType` prove the
same code serves two instantiations. `TestBuildIsIndependentCopy` mutates a built
result and proves a later build is unaffected — the guarantee `slices.Clone`
provides; returning `b.items` directly would fail this. `TestAddAllOverLimitAddsNothing`
proves the all-or-nothing guard, and `TestEmptyBuildIsNonNil` pins the friendlier
non-nil contract that `New`'s initialized buffer produces. The traps to avoid:
leaking the internal slice from `Build` (an aliasing bug), and defaulting `maxSize`
to a literal zero instead of `cmp.Or`-ing to a real limit. Run `go test -race`;
the batch is single-goroutine by contract, so the flag confirms the tests
themselves are clean.

## Resources

- [slices.Clone](https://pkg.go.dev/slices#Clone) — the owned-copy primitive for Build.
- [slices.Grow](https://pkg.go.dev/slices#Grow) — pre-extending the backing array for bulk adds.
- [cmp.Or](https://pkg.go.dev/cmp#Or) — defaulting a zero max size to a real limit.
- [Go: Type parameters](https://go.dev/blog/intro-generics) — the generics that let one builder serve every element type.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-http-client-builder-with-defaults.md](08-http-client-builder-with-defaults.md)
