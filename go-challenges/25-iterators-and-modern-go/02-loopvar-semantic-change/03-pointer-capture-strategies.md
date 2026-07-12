# Exercise 3: Pointer Capture Strategies

Per-iteration scope made `&v` inside a range loop safe to store, but it did not change what `&v` points at. This exercise builds two functions that return pointers from a range loop — one taking the address of the iteration variable, one taking the address of the slice element — and proves they differ in exactly one observable way: whether writing through the pointer mutates the caller's slice.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
pointers.go          Item, IterationPointers, ElementPointers, Names, ErrEmptyInput
cmd/
  demo/
    main.go          build both pointer slices, write through each, show the difference
pointers_test.go     element pointers alias the slice; iteration pointers do not
example_test.go      ExampleElementPointers with a verified // Output block
```

- Files: `pointers.go`, `cmd/demo/main.go`, `pointers_test.go`, `example_test.go`.
- Implement: `IterationPointers([]Item) ([]*Item, error)` using `&item`, `ElementPointers([]Item) ([]*Item, error)` using `&items[i]`, and `Names([]*Item) []string`, plus the `Item` type and the sentinel `ErrEmptyInput`.
- Test: assert both strategies read back the original names, that writing through an element pointer mutates the slice, and that writing through an iteration pointer does not.
- Verify: `go test -count=1 -race ./...`

### The difference per-iteration scope did not erase

The old per-loop bug for pointers was aliasing: every `&item` in `for _, item := range items` was the address of the single shared `item`, so a slice of those pointers held the same address repeated, and after the loop they all pointed at storage holding the last element. The pointers were equal and useless. Per-iteration scope fixes this completely. Each iteration's `item` is a distinct variable with its own address, so `IterationPointers` now returns distinct, individually valid pointers, and reading `Names` back gives the original sequence rather than the last element repeated.

But "distinct and valid" is not the same as "points into the slice." `&item` is the address of the iteration's *copy* of the element. The range loop copied `items[i]` into `item`, and `&item` addresses that copy, which lives on independently of the backing array. Writing through it mutates the copy and leaves `items` untouched. This is the subtlety that survives Go 1.22: per-iteration scope made the copy-pointers safe to store, but they are still pointers to copies.

`ElementPointers` is the form to use when the caller wants pointers that alias the original elements. The index loop `for i := range items` plus `&items[i]` takes the address of the element inside the backing array, so writing through the returned pointer updates `items[i]` directly. The two functions read back identical names — both expose the same values — and differ only when you write through the pointer: the element pointer reaches the caller's slice, the iteration pointer reaches a copy. Choosing between them is a real design decision about whether the returned handles should be live references or independent snapshots, and the loop-variable change neither makes nor obscures that choice.

Create `pointers.go`:

```go
package loopvar

import (
	"errors"
	"fmt"
)

// ErrEmptyInput is returned when there is nothing to take pointers into.
var ErrEmptyInput = errors.New("input must not be empty")

// Item is a small value type used to make pointer identity observable.
type Item struct {
	Name string
}

// IterationPointers returns &item for each iteration's copy of the element.
// Under go 1.22 the pointers are distinct and safe to store, but each addresses
// the iteration's copy, not the caller's slice element, so writing through one
// does not mutate the original slice.
func IterationPointers(items []Item) ([]*Item, error) {
	if len(items) == 0 {
		return nil, fmt.Errorf("iteration pointers: %w", ErrEmptyInput)
	}

	ptrs := make([]*Item, 0, len(items))
	for _, item := range items {
		ptrs = append(ptrs, &item)
	}
	return ptrs, nil
}

// ElementPointers returns &items[i] for each element, so each pointer aliases
// the caller's original slice element and writing through it mutates the slice.
func ElementPointers(items []Item) ([]*Item, error) {
	if len(items) == 0 {
		return nil, fmt.Errorf("element pointers: %w", ErrEmptyInput)
	}

	ptrs := make([]*Item, 0, len(items))
	for i := range items {
		ptrs = append(ptrs, &items[i])
	}
	return ptrs, nil
}

// Names dereferences each pointer and collects the names in order.
func Names(ptrs []*Item) []string {
	names := make([]string, 0, len(ptrs))
	for _, p := range ptrs {
		names = append(names, p.Name)
	}
	return names
}
```

### The runnable demo

The demo builds both pointer slices from the same input, shows they read back identical names, then writes through the first pointer of each and prints `items[0]` afterward. The iteration pointer leaves the slice alone; the element pointer changes it.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/loop-pointers"
)

func main() {
	items := []loopvar.Item{{Name: "alpha"}, {Name: "beta"}, {Name: "gamma"}}

	iter, err := loopvar.IterationPointers(items)
	if err != nil {
		fmt.Println("iteration pointers error:", err)
		return
	}
	fmt.Println("iteration names:", loopvar.Names(iter))
	iter[0].Name = "CHANGED"
	fmt.Println("slice after iteration write:", items[0].Name)

	elem, err := loopvar.ElementPointers(items)
	if err != nil {
		fmt.Println("element pointers error:", err)
		return
	}
	fmt.Println("element names:", loopvar.Names(elem))
	elem[0].Name = "CHANGED"
	fmt.Println("slice after element write:", items[0].Name)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
iteration names: [alpha beta gamma]
slice after iteration write: alpha
element names: [alpha beta gamma]
slice after element write: CHANGED
```

The two `Names` lines are identical; the two "slice after" lines are where the strategies diverge.

### Tests

The tests assert both strategies read back the original names, then pin the one behavior that distinguishes them: writing through an element pointer mutates `items[0]`, while writing through an iteration pointer leaves it alone. The parallel subtests in `TestBothStrategiesReturnDistinctNames` capture `tc` directly, with no `tc := tc` copy, relying on the per-iteration scope this chapter is about.

Create `pointers_test.go`:

```go
package loopvar

import (
	"errors"
	"reflect"
	"testing"
)

func TestBothStrategiesReturnDistinctNames(t *testing.T) {
	t.Parallel()

	items := []Item{{Name: "alpha"}, {Name: "beta"}, {Name: "gamma"}}
	cases := []struct {
		name string
		make func([]Item) ([]*Item, error)
	}{
		{name: "iteration", make: IterationPointers},
		{name: "element", make: ElementPointers},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ptrs, err := tc.make(items)
			if err != nil {
				t.Fatal(err)
			}
			got := Names(ptrs)
			want := []string{"alpha", "beta", "gamma"}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Names = %v, want %v", got, want)
			}
		})
	}
}

func TestElementPointersAliasOriginal(t *testing.T) {
	t.Parallel()

	items := []Item{{Name: "alpha"}, {Name: "beta"}}
	ptrs, err := ElementPointers(items)
	if err != nil {
		t.Fatal(err)
	}
	ptrs[0].Name = "changed"
	if items[0].Name != "changed" {
		t.Fatalf("items[0].Name = %q, want changed", items[0].Name)
	}
}

func TestIterationPointersDoNotAliasOriginal(t *testing.T) {
	t.Parallel()

	items := []Item{{Name: "alpha"}, {Name: "beta"}}
	ptrs, err := IterationPointers(items)
	if err != nil {
		t.Fatal(err)
	}
	ptrs[0].Name = "changed"
	if items[0].Name != "alpha" {
		t.Fatalf("items[0].Name = %q, want alpha", items[0].Name)
	}
}

func TestEmptyInputs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		call func() error
	}{
		{name: "iteration", call: func() error { _, err := IterationPointers(nil); return err }},
		{name: "element", call: func() error { _, err := ElementPointers(nil); return err }},
	}

	for _, tc := range cases {
		if err := tc.call(); !errors.Is(err, ErrEmptyInput) {
			t.Fatalf("%s error = %v, want ErrEmptyInput", tc.name, err)
		}
	}
}
```

Create `example_test.go`:

```go
package loopvar

import "fmt"

func ExampleElementPointers() {
	items := []Item{{Name: "alpha"}, {Name: "beta"}}
	ptrs, _ := ElementPointers(items)
	ptrs[0].Name = "changed"
	fmt.Println(items[0].Name)
	// Output: changed
}
```

## Review

Both functions are correct when they read back `[alpha beta gamma]` — per-iteration scope guarantees the pointers are distinct, so neither returns the last element repeated. The behavior that matters is divergence under a write: `ElementPointers` must mutate `items[0]` and `IterationPointers` must not, and the two dedicated tests pin exactly that. If you reach for `&item` expecting it to alias the slice, the `TestIterationPointersDoNotAliasOriginal` case is the one that will fail you in practice; the fix is `&items[i]` with the index form.

The common mistake is concluding that because Go 1.22 made `&item` pointers distinct and safe, they now point into the slice. They do not; they point at per-iteration copies. Use the index form when the returned pointer must be a live reference to the caller's element, and the value form when an independent snapshot is what you want. The loop-variable change improved the safety of the copy-pointers without erasing the difference between a pointer to a copy and a pointer to an element.

## Resources

- [Fixing For Loops in Go 1.22](https://go.dev/blog/loopvar-preview) — the change that made per-iteration `&v` pointers distinct, and its limits.
- [Go Spec: For statements](https://go.dev/ref/spec#For_statements) — the rules for range clauses and how the iteration variable receives a copy of each element.
- [Go Spec: Address operators](https://go.dev/ref/spec#Address_operators) — what `&x` yields for a variable versus an indexed slice element.

---

Back to [02-goroutines-in-loops.md](02-goroutines-in-loops.md) | Next: [04-fan-out-to-a-downstream-service.md](04-fan-out-to-a-downstream-service.md)
