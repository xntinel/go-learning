# Exercise 8: The &v-in-range trap — taking the address of a loop copy

Building a `[]*Item` index over a `[]Item` batch is a routine repository task, and
it hides one of Go's most durable bugs: taking `&v` of the range variable points
into a per-iteration copy, not the backing array — and Go 1.22 loop scoping did not
fix it. This module builds the index both ways and proves the difference.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
rangeidx/                  independent module: example.com/rangeidx
  go.mod                   module example.com/rangeidx
  index.go                 Item{ID,Qty}; buildIndexBuggy (&v), buildIndexCorrect (&items[i])
  cmd/
    demo/
      main.go              builds both indexes, mutates through each, prints the batch
  index_test.go            correct aliases backing array and mutates it; buggy does not
```

- Files: `index.go`, `cmd/demo/main.go`, `index_test.go`.
- Implement: `buildIndexBuggy(items []Item) []*Item` using `&v`, and `buildIndexCorrect(items []Item) []*Item` using `&items[i]`.
- Test: the correct index's pointers equal `&items[i]` and mutating through them changes the batch; the buggy index's pointers do not alias the batch, so mutations through them leave it unchanged.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/rangeidx/cmd/demo
cd ~/go-exercises/rangeidx
go mod init example.com/rangeidx
```

### A range value is a copy; its address is not the element's address

Imagine loading a batch of rows into a `[]Item` and building a `[]*Item` lookup so
downstream code can mutate rows in place (adjust a quantity, mark a flag). The
obvious loop is the trap:

```go
// Buggy: &v is the address of the per-iteration copy, not items[i].
for _, v := range items {
	idx = append(idx, &v)
}
```

`for _, v := range items` binds `v` to a fresh copy of `items[i]` each iteration.
`&v` is the address of that copy. Under Go 1.22+ per-iteration scoping, each `&v` is
a *distinct* address (a distinct copy), so the index is not "all pointers alias the
last element" (the pre-1.22 bug) — it is worse to diagnose: every pointer aliases
its own disconnected copy. Mutating `*idx[i]` changes a copy that nothing else
references; the batch is untouched. The values you read back through the buggy index
are *correct at build time* but *disconnected* from the slice, which is what makes
the bug survive review — a quick check "does `idx[0].ID` match?" passes.

The fix indexes the backing array directly:

```go
// Correct: &items[i] is the address of the actual element.
for i := range items {
	idx = append(idx, &items[i])
}
```

`&items[i]` is the address of the real element, so `idx[i] == &items[i]` and
mutating `*idx[i]` mutates the batch. The test pins both facts: correct pointers
*equal* `&items[i]` and drive changes into `items`; buggy pointers *do not equal*
`&items[i]` and their mutations never reach `items`.

Create `index.go`:

```go
package rangeidx

// Item is a batch row.
type Item struct {
	ID  string
	Qty int
}

// buildIndexBuggy builds a []*Item using &v of the range variable. Each pointer
// addresses a per-iteration copy, NOT items[i], so mutating through the index
// does not touch the backing slice. This is the trap; it is here to be tested.
func buildIndexBuggy(items []Item) []*Item {
	idx := make([]*Item, 0, len(items))
	for _, v := range items {
		idx = append(idx, &v)
	}
	return idx
}

// buildIndexCorrect builds a []*Item using &items[i], so each pointer addresses
// the real element and mutating through the index mutates the batch.
func buildIndexCorrect(items []Item) []*Item {
	idx := make([]*Item, 0, len(items))
	for i := range items {
		idx = append(idx, &items[i])
	}
	return idx
}
```

### The runnable demo

The demo mutates the batch through both indexes and shows only the correct one lands.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/rangeidx"
)

func main() {
	batch := []rangeidx.Item{{ID: "a", Qty: 1}, {ID: "b", Qty: 2}}

	buggy := rangeidx.BuildIndexBuggy(batch)
	buggy[0].Qty = 100
	fmt.Printf("after buggy mutation:   batch[0].Qty = %d\n", batch[0].Qty)

	correct := rangeidx.BuildIndexCorrect(batch)
	correct[0].Qty = 100
	fmt.Printf("after correct mutation: batch[0].Qty = %d\n", batch[0].Qty)
}
```

Expose both builders to the demo package.

Append to `index.go`:

```go
// BuildIndexBuggy is the exported buggy builder (for the demo).
func BuildIndexBuggy(items []Item) []*Item { return buildIndexBuggy(items) }

// BuildIndexCorrect is the exported correct builder (for the demo).
func BuildIndexCorrect(items []Item) []*Item { return buildIndexCorrect(items) }
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
after buggy mutation:   batch[0].Qty = 1
after correct mutation: batch[0].Qty = 100
```

### Tests

`TestCorrectIndexAliasesBackingArray` asserts each correct pointer equals
`&items[i]` and that mutating through it changes the batch. `TestBuggyIndexDoesNotAlias`
asserts the buggy pointers do *not* equal `&items[i]` and that mutating through them
leaves the batch unchanged — the values were copied out and disconnected.
`TestBuggyValuesAreCorrectButDisconnected` shows the subtle part: the buggy index's
values match at build time (so a naive check passes), yet they do not track later
edits to the batch.

Create `index_test.go`:

```go
package rangeidx

import (
	"fmt"
	"testing"
)

func TestCorrectIndexAliasesBackingArray(t *testing.T) {
	t.Parallel()

	items := []Item{{ID: "a", Qty: 1}, {ID: "b", Qty: 2}, {ID: "c", Qty: 3}}
	idx := buildIndexCorrect(items)

	for i := range items {
		if idx[i] != &items[i] {
			t.Fatalf("idx[%d] = %p, want &items[%d] = %p", i, idx[i], i, &items[i])
		}
	}

	idx[1].Qty = 99
	if items[1].Qty != 99 {
		t.Fatalf("items[1].Qty = %d, want 99 (mutation through correct index must land)", items[1].Qty)
	}
}

func TestBuggyIndexDoesNotAlias(t *testing.T) {
	t.Parallel()

	items := []Item{{ID: "a", Qty: 1}, {ID: "b", Qty: 2}, {ID: "c", Qty: 3}}
	idx := buildIndexBuggy(items)

	for i := range items {
		if idx[i] == &items[i] {
			t.Fatalf("idx[%d] unexpectedly aliases &items[%d]; a range copy's address must differ", i, i)
		}
	}

	idx[1].Qty = 99
	if items[1].Qty != 2 {
		t.Fatalf("items[1].Qty = %d, want 2 (mutation through buggy index must NOT land)", items[1].Qty)
	}
}

func TestBuggyValuesAreCorrectButDisconnected(t *testing.T) {
	t.Parallel()

	items := []Item{{ID: "a", Qty: 1}, {ID: "b", Qty: 2}}
	idx := buildIndexBuggy(items)

	// Values match at build time: a naive check passes and hides the bug.
	if idx[0].ID != "a" || idx[0].Qty != 1 {
		t.Fatalf("idx[0] = %+v, want {a 1}", *idx[0])
	}

	// But editing the batch does not reach the buggy index (it is a copy).
	items[0].Qty = 500
	if idx[0].Qty != 1 {
		t.Fatalf("idx[0].Qty = %d, want 1 (buggy index is disconnected from the batch)", idx[0].Qty)
	}
}

func Example() {
	batch := []Item{{ID: "a", Qty: 1}}
	correct := buildIndexCorrect(batch)
	correct[0].Qty = 42
	buggy := buildIndexBuggy(batch)
	buggy[0].Qty = 999
	fmt.Println(batch[0].Qty)
	// Output: 42
}
```

## Review

The trap is that `for _, v := range items` gives you a copy each iteration, so `&v`
is the address of a copy that nothing else references. The tests make the failure
unmissable: the correct index's pointers *equal* `&items[i]` and mutating through
them lands in the batch, while the buggy index's pointers *differ* and its mutations
evaporate. The most dangerous property is that the buggy index reads correct at
build time — `idx[0].Qty == 1` — so a shallow test passes and the disconnection only
surfaces when the batch is edited later. Do not believe Go 1.22 fixed this: per-
iteration scoping stopped closures from all capturing the same variable, but a range
value is still a copy and its address is still not the element's address. When you
need pointers into a slice, use `&items[i]`.

## Resources

- [Go Wiki: Range loop variable scoping (Go 1.22)](https://go.dev/wiki/LoopvarExperiment) — what per-iteration scoping did and did not change.
- [Go Language Specification: For statements with range](https://go.dev/ref/spec#For_range) — the range value is a copy.
- [Go Language Specification: Address operators](https://go.dev/ref/spec#Address_operators) — `&items[i]` addresses the element.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-in-place-metrics-aggregation.md](07-in-place-metrics-aggregation.md) | Next: [09-nil-pointer-deref-defense.md](09-nil-pointer-deref-defense.md)
