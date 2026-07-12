# Exercise 5: Slice Growth: Preallocating Capacity to Avoid Repeated Escapes

Mapping a known number of DB rows to DTOs is one of the most common allocation
hot spots in a service. Append to a `nil` slice and the runtime reallocates the
backing array a dozen times as it grows; give `make` a capacity hint and those
dozen allocations collapse into one. This module measures the difference on a
row-to-DTO mapper.

This module is fully self-contained.

## What you'll build

```text
rowmapper/                    independent module: example.com/rowmapper
  go.mod                      go 1.26
  rowmap.go                   Row, UserDTO; MapRowsNil (grow from nil),
                              MapRowsPre (make with cap); MapValidated + ErrEmptyName
  cmd/
    demo/
      main.go                 maps rows both ways; shows the alloc gap
  rowmap_test.go              output-equality, AllocsPerRun (pre==1, nil>pre), errors.Is
```

Files: `rowmap.go`, `cmd/demo/main.go`, `rowmap_test.go`.
Implement: `MapRowsNil` (appends to a `nil` slice) and `MapRowsPre` (preallocates
with `make([]UserDTO, 0, len(rows))`), plus `MapValidated` returning a wrapped
`ErrEmptyName`.
Test: an equality test proving both mappers produce identical output; an
`AllocsPerRun` test asserting the preallocated mapper does exactly one allocation
while the nil mapper does more; and an `errors.Is` test for the validator.
Verify: `go test -count=1 -race ./...`, then `go test -bench=. -benchmem ./...`.

Set up the module:

```bash
mkdir -p go-solutions/09-pointers/07-escape-analysis/05-slice-preallocation-and-growth/cmd/demo
cd go-solutions/09-pointers/07-escape-analysis/05-slice-preallocation-and-growth
```

### Why growth costs N allocations and a hint costs one

`append` to a slice that has no spare capacity must allocate a new, larger backing
array, copy the existing elements over, and let the old array become garbage. To
amortize this, the runtime grows capacity geometrically — roughly doubling for
small slices — so appending N elements from `nil` triggers on the order of
`log2(N)` reallocations. Each of those intermediate arrays is a heap allocation
whose size was unknown at compile time, so it escapes; each is also GC garbage the
moment the next growth copies past it. For 1000 rows that is about a dozen
allocations and a dozen throwaway arrays, every time the function runs.

`MapRowsPre` allocates the backing array exactly once: `make([]UserDTO, 0, n)`
reserves capacity for all `n` elements up front, so every subsequent `append`
writes into the existing array with no growth and no reallocation. The result is
one allocation (the single backing array, which escapes because it is returned)
instead of a dozen. When you know the final length — and a row count from a query
almost always tells you — the capacity hint is close to free money.

The two mappers must produce identical slices, or the "optimization" is a bug.
`TestMappersAgree` enforces that. `MapValidated` adds a realistic wrinkle: it
rejects rows with an empty name, returning `ErrEmptyName` wrapped with `%w` so a
caller can distinguish a validation failure from any other error — and it, too,
preallocates.

Create `rowmap.go`:

```go
package rowmap

import (
	"errors"
	"fmt"
)

// ErrEmptyName marks a row whose Name is blank.
var ErrEmptyName = errors.New("rowmap: empty name")

// Row is a record as read from the database.
type Row struct {
	ID    int
	Name  string
	Email string
}

// UserDTO is the shape returned to callers of the service.
type UserDTO struct {
	ID    int
	Label string
}

// MapRowsNil grows the result slice from nil, reallocating as it goes.
func MapRowsNil(rows []Row) []UserDTO {
	var out []UserDTO
	for _, r := range rows {
		out = append(out, UserDTO{ID: r.ID, Label: r.Name})
	}
	return out
}

// MapRowsPre preallocates capacity for len(rows), so the backing array is
// allocated exactly once.
func MapRowsPre(rows []Row) []UserDTO {
	out := make([]UserDTO, 0, len(rows))
	for _, r := range rows {
		out = append(out, UserDTO{ID: r.ID, Label: r.Name})
	}
	return out
}

// MapValidated preallocates and rejects rows with an empty name.
func MapValidated(rows []Row) ([]UserDTO, error) {
	out := make([]UserDTO, 0, len(rows))
	for _, r := range rows {
		if r.Name == "" {
			return nil, fmt.Errorf("row id=%d: %w", r.ID, ErrEmptyName)
		}
		out = append(out, UserDTO{ID: r.ID, Label: r.Name})
	}
	return out, nil
}
```

### The runnable demo

The demo maps a batch both ways, confirms the results are identical, and prints
the allocation counts using `testing.AllocsPerRun`. The preallocated count is a
stable `1`; the grow-from-nil count is larger (the exact number depends on the row
count and the runtime's growth policy, so the demo asserts only that it exceeds
the preallocated count).

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"testing"

	"example.com/rowmapper"
)

func main() {
	rows := make([]rowmap.Row, 1000)
	for i := range rows {
		rows[i] = rowmap.Row{ID: i, Name: fmt.Sprintf("user-%d", i)}
	}

	a := rowmap.MapRowsNil(rows)
	b := rowmap.MapRowsPre(rows)

	equal := len(a) == len(b)
	for i := range a {
		if a[i] != b[i] {
			equal = false
			break
		}
	}

	var sink []rowmap.UserDTO
	nilAllocs := testing.AllocsPerRun(200, func() { sink = rowmap.MapRowsNil(rows) })
	preAllocs := testing.AllocsPerRun(200, func() { sink = rowmap.MapRowsPre(rows) })
	_ = sink

	fmt.Printf("rows: %d\n", len(rows))
	fmt.Printf("equal: %v\n", equal)
	fmt.Printf("preallocated allocs/op: %.0f\n", preAllocs)
	fmt.Printf("grow-from-nil more than prealloc: %v\n", nilAllocs > preAllocs)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
rows: 1000
equal: true
preallocated allocs/op: 1
grow-from-nil more than prealloc: true
```

### Tests

`TestMappersAgree` proves the two mappers are interchangeable.
`TestPreallocOneAlloc` pins the preallocated mapper to exactly one allocation and
asserts the nil mapper does more — the executable form of the growth argument.
`TestMapValidated` covers both the happy path and the wrapped-error path.

Create `rowmap_test.go`:

```go
package rowmap

import (
	"errors"
	"fmt"
	"testing"
)

func makeRows(n int) []Row {
	rows := make([]Row, n)
	for i := range rows {
		rows[i] = Row{ID: i, Name: fmt.Sprintf("user-%d", i)}
	}
	return rows
}

func TestMappersAgree(t *testing.T) {
	t.Parallel()
	rows := makeRows(37)
	a := MapRowsNil(rows)
	b := MapRowsPre(rows)
	if len(a) != len(b) {
		t.Fatalf("len mismatch: nil=%d pre=%d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("element %d: nil=%+v pre=%+v", i, a[i], b[i])
		}
	}
}

var sink []UserDTO

func TestPreallocOneAlloc(t *testing.T) {
	rows := makeRows(1000)
	pre := testing.AllocsPerRun(200, func() { sink = MapRowsPre(rows) })
	nilA := testing.AllocsPerRun(200, func() { sink = MapRowsNil(rows) })
	if pre != 1 {
		t.Errorf("MapRowsPre allocs/op = %.1f, want exactly 1", pre)
	}
	if !(nilA > pre) {
		t.Errorf("grow-from-nil should allocate more: nil=%.1f pre=%.1f", nilA, pre)
	}
}

func TestMapValidated(t *testing.T) {
	t.Parallel()
	ok, err := MapValidated([]Row{{ID: 1, Name: "a"}, {ID: 2, Name: "b"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ok) != 2 {
		t.Errorf("len = %d, want 2", len(ok))
	}

	_, err = MapValidated([]Row{{ID: 1, Name: "a"}, {ID: 2, Name: ""}})
	if !errors.Is(err, ErrEmptyName) {
		t.Fatalf("err = %v, want wrapped ErrEmptyName", err)
	}
}

func BenchmarkMapRowsNil(b *testing.B) {
	rows := makeRows(1000)
	b.ReportAllocs()
	for b.Loop() {
		sink = MapRowsNil(rows)
	}
}

func BenchmarkMapRowsPre(b *testing.B) {
	rows := makeRows(1000)
	b.ReportAllocs()
	for b.Loop() {
		sink = MapRowsPre(rows)
	}
}

func ExampleMapRowsPre() {
	out := MapRowsPre([]Row{{ID: 1, Name: "root"}})
	fmt.Printf("%d %s\n", out[0].ID, out[0].Label)
	// Output: 1 root
}
```

## Review

The mappers are correct only if they agree element-for-element; `TestMappersAgree`
is what lets you swap the nil mapper for the preallocated one without changing
behavior. The allocation guarantee is the lesson: `MapRowsPre` does exactly one
heap allocation (the single backing array, which escapes because it is returned),
while `MapRowsNil` pays a growth allocation each time capacity is exhausted. Prove
it to yourself with `-benchmem`: the `allocs/op` and `B/op` columns collapse when
you add the capacity hint. The mistake to avoid is appending to `nil` in a tight
loop when the final length is known — a query that returns a row count, a batch of
known size, a fixed fan-out. Pass the hint to `make` and the whole growth curve
disappears.

## Resources

- [Go Blog: Arrays, slices (and strings)](https://go.dev/blog/slices-intro) — how append grows the backing array.
- [Go slice tricks: growth and capacity](https://go.dev/wiki/SliceTricks) — preallocation and reuse patterns.
- [testing.AllocsPerRun](https://pkg.go.dev/testing#AllocsPerRun) — measuring allocations per call.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-sync-pool-buffer-reuse.md](04-sync-pool-buffer-reuse.md) | Next: [06-closure-capture-escape.md](06-closure-capture-escape.md)
