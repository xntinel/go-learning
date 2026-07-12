# 6. DeepEqual and Custom Comparison

Testing complex nested data structures requires more than `==`. `reflect.DeepEqual` fills the gap but carries sharp edges that produce confusing test failures: nil and empty slices compare as unequal, `time.Time` values that represent the same instant differ when their `Location` pointers differ, and unexported fields can cause panics. Understanding these edges — and knowing when to reach for a custom comparator or `go-cmp` — is the practical skill.

```text
compare/
  go.mod
  compare.go
  compare_test.go
  cmd/demo/main.go
```

## Concepts

### How DeepEqual Recurses

`reflect.DeepEqual(x, y)` compares values structurally:

- Scalars: compared with `==`.
- Pointers: the pointed-to values are compared recursively (not the addresses).
- Structs: each field is compared recursively, including unexported fields.
- Slices: compared element-by-element; a nil slice and an empty slice are **not** equal.
- Maps: compared key-by-key regardless of insertion order.
- Functions: equal only if both are nil.

The recursion handles cycles via a visited-pair table, so circular structures do not loop forever.

### The nil-vs-empty Distinction

```go
var s []int       // nil
e := []int{}     // empty, not nil
reflect.DeepEqual(s, e)  // false
```

This surprises developers who treat "empty" and "absent" as the same thing. The same applies to maps: `var m map[string]int` and `map[string]int{}` are not `DeepEqual`.

In tests, a function that returns `[]string{}` when given no results, and `nil` when it encounters an error, can produce spurious failures if the test uses `DeepEqual` without normalizing nil vs empty.

### time.Time and Location Pointer Equality

`time.Time` contains a `*Location` field. Two `time.Time` values that represent the same instant but were constructed with different `*Location` objects (e.g., `time.UTC` vs `time.FixedZone("UTC", 0)`) compare as unequal under `DeepEqual` because the pointer addresses differ.

The right test for time equality is `t1.Equal(t2)`, which compares the instants regardless of location.

### Unexported Fields and Panics

`DeepEqual` reads unexported fields via `unsafe` internally. For most types this is safe. But if a type stores a pointer to something that changes identity across calls (like `sync.Mutex`), comparing with `DeepEqual` may give wrong results without panicking. For types that export a meaningful `Equal` method, prefer calling that method directly.

### go-cmp for Readable Test Diffs

`github.com/google/go-cmp/cmp` is the idiomatic choice for test assertions:

- `cmp.Diff(want, got)` returns an empty string when equal, or a human-readable diff.
- `cmpopts.IgnoreFields(T{}, "FieldName")` excludes fields (e.g., `CreatedAt`, `ID`).
- `cmpopts.EquateEmpty()` treats nil and empty slices/maps as equal.
- `cmpopts.EquateApprox(fraction, margin)` allows floating-point tolerance.
- `cmpopts.IgnoreUnexported(T{})` tells go-cmp to skip unexported fields rather than panic.

Note: `go-cmp` is an external module, so the lesson's stdlib-only code uses `reflect.DeepEqual` and a custom comparator; the `go-cmp` section is conceptual and not compiled.

## Exercises

### Exercise 1: Custom Field-Ignoring Comparator

Create `compare.go`:

```go
package compare

import (
	"reflect"
	"time"
)

// EqualIgnoring compares two struct values of the same type, skipping
// the fields named in ignoreFields. Non-struct types fall back to
// reflect.DeepEqual.
//
// Both a and b must be the same type; a may be a pointer to a struct.
func EqualIgnoring(a, b any, ignoreFields ...string) bool {
	va := reflect.ValueOf(a)
	vb := reflect.ValueOf(b)
	if va.Type() != vb.Type() {
		return false
	}
	if va.Kind() == reflect.Ptr {
		va = va.Elem()
		vb = vb.Elem()
	}
	if va.Kind() != reflect.Struct {
		return reflect.DeepEqual(a, b)
	}

	skip := make(map[string]bool, len(ignoreFields))
	for _, f := range ignoreFields {
		skip[f] = true
	}

	t := va.Type()
	for i := 0; i < t.NumField(); i++ {
		name := t.Field(i).Name
		if skip[name] {
			continue
		}
		if !reflect.DeepEqual(va.Field(i).Interface(), vb.Field(i).Interface()) {
			return false
		}
	}
	return true
}

// TimeEqual reports whether two time.Time values represent the same instant,
// regardless of their Location.
func TimeEqual(a, b time.Time) bool {
	return a.Equal(b)
}

// NilSliceAsEmpty returns a copy of v with any nil slice field replaced by an
// empty (non-nil) slice of the same element type, so DeepEqual treats them as
// equal to empty slices.
//
// v must be a struct value (not a pointer).
func NilSliceAsEmpty(v any) any {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Struct {
		return v
	}
	// Create a settable copy.
	cp := reflect.New(rv.Type()).Elem()
	cp.Set(rv)
	for i := 0; i < rv.Type().NumField(); i++ {
		f := cp.Field(i)
		if f.Kind() == reflect.Slice && f.IsNil() && f.CanSet() {
			f.Set(reflect.MakeSlice(f.Type(), 0, 0))
		}
	}
	return cp.Interface()
}
```

### Exercise 2: Tests

Create `compare_test.go`:

```go
package compare

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

type Record struct {
	ID        int
	Name      string
	Tags      []string
	UpdatedAt time.Time
}

func TestDeepEqualSlices(t *testing.T) {
	t.Parallel()

	a := []int{1, 2, 3}
	b := []int{1, 2, 3}
	if !reflect.DeepEqual(a, b) {
		t.Error("identical slices should be DeepEqual")
	}
}

func TestDeepEqualMaps(t *testing.T) {
	t.Parallel()

	m1 := map[string]int{"a": 1, "b": 2}
	m2 := map[string]int{"b": 2, "a": 1}
	if !reflect.DeepEqual(m1, m2) {
		t.Error("maps with same entries should be DeepEqual regardless of insertion order")
	}
}

func TestDeepEqualNilVsEmptySlice(t *testing.T) {
	t.Parallel()

	var nilSlice []int
	emptySlice := []int{}
	if reflect.DeepEqual(nilSlice, emptySlice) {
		t.Error("nil and empty slice should NOT be DeepEqual — this is a known edge case")
	}
}

func TestDeepEqualNilVsEmptyMap(t *testing.T) {
	t.Parallel()

	var nilMap map[string]int
	emptyMap := map[string]int{}
	if reflect.DeepEqual(nilMap, emptyMap) {
		t.Error("nil and empty map should NOT be DeepEqual")
	}
}

func TestDeepEqualFollowsPointers(t *testing.T) {
	t.Parallel()

	x, y := 42, 42
	if !reflect.DeepEqual(&x, &y) {
		t.Error("pointers to equal values should be DeepEqual")
	}
	z := 99
	if reflect.DeepEqual(&x, &z) {
		t.Error("pointers to different values should not be DeepEqual")
	}
}

func TestTimeEqualVsDeepEqual(t *testing.T) {
	t.Parallel()

	instant := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	// Same instant, different *Location.
	same := time.Date(2025, 1, 1, 12, 0, 0, 0, time.FixedZone("UTC+0", 0))

	// time.Equal ignores location.
	if !TimeEqual(instant, same) {
		t.Error("TimeEqual: same instant should be equal")
	}
	// DeepEqual compares the Location pointer — may be false.
	// We document the edge case without asserting a specific result because
	// the exact pointer equality is an implementation detail.
	_ = reflect.DeepEqual(instant, same)
}

func TestEqualIgnoringSkipsNamedFields(t *testing.T) {
	t.Parallel()

	now := time.Now()
	r1 := Record{ID: 1, Name: "Alice", UpdatedAt: now}
	r2 := Record{ID: 1, Name: "Alice", UpdatedAt: now.Add(time.Hour)}

	if reflect.DeepEqual(r1, r2) {
		t.Error("records with different UpdatedAt should not be DeepEqual")
	}
	if !EqualIgnoring(r1, r2, "UpdatedAt") {
		t.Error("EqualIgnoring UpdatedAt should treat records as equal")
	}
}

func TestEqualIgnoringTypeMismatch(t *testing.T) {
	t.Parallel()

	type A struct{ X int }
	type B struct{ X int }
	if EqualIgnoring(A{X: 1}, B{X: 1}) {
		t.Error("values of different types should not be equal")
	}
}

func TestNilSliceAsEmptyNormalizes(t *testing.T) {
	t.Parallel()

	type S struct {
		Tags []string
	}
	a := NilSliceAsEmpty(S{Tags: nil})
	b := S{Tags: []string{}}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("after NilSliceAsEmpty, nil-tags record should equal empty-tags record: %+v vs %+v", a, b)
	}
}

func TestEqualIgnoringPointerInput(t *testing.T) {
	t.Parallel()

	r1 := &Record{ID: 2, Name: "Bob", UpdatedAt: time.Now()}
	r2 := &Record{ID: 2, Name: "Bob", UpdatedAt: time.Now().Add(time.Second)}
	if !EqualIgnoring(r1, r2, "UpdatedAt") {
		t.Error("pointer input should also work")
	}
}

func ExampleEqualIgnoring() {
	type Item struct {
		ID   int
		Name string
		Rev  int
	}
	a := Item{ID: 1, Name: "widget", Rev: 3}
	b := Item{ID: 1, Name: "widget", Rev: 99}
	fmt.Println(EqualIgnoring(a, b, "Rev"))
	// Output: true
}
```

Add `"fmt"` to the import block — the Example uses it.

**Your turn:** add `TestEqualIgnoringBothFieldsIgnored` that compares two `Record` values with different `ID` and `UpdatedAt`, ignores both, but asserts the records are equal because `Name` matches.

### Exercise 3: Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"reflect"
	"time"

	"example.com/compare"
)

type Order struct {
	ID       int
	Customer string
	Amount   float64
	Tags     []string
	PlacedAt time.Time
}

func main() {
	now := time.Now()
	o1 := Order{ID: 1, Customer: "Alice", Amount: 99.99, Tags: []string{"vip"}, PlacedAt: now}
	o2 := Order{ID: 1, Customer: "Alice", Amount: 99.99, Tags: []string{"vip"}, PlacedAt: now.Add(time.Second)}

	fmt.Println("DeepEqual (different PlacedAt):", reflect.DeepEqual(o1, o2))
	fmt.Println("EqualIgnoring PlacedAt:        ", compare.EqualIgnoring(o1, o2, "PlacedAt"))

	// nil vs empty slice edge case.
	var a []string
	b := []string{}
	fmt.Println("nil == empty (DeepEqual):", reflect.DeepEqual(a, b))

	// NilSliceAsEmpty for normalized comparison.
	type S struct{ Tags []string }
	s1 := compare.NilSliceAsEmpty(S{Tags: nil})
	s2 := S{Tags: []string{}}
	fmt.Println("after NilSliceAsEmpty:", reflect.DeepEqual(s1, s2))
}
```

Run with `go run ./cmd/demo`.

## Common Mistakes

### Comparing time.Time with DeepEqual

Wrong: `reflect.DeepEqual(t1, t2)` where `t1` and `t2` represent the same instant but come from different timezone constructors.

What happens: the internal `*Location` pointers differ, so `DeepEqual` returns `false` even though the instants are identical.

Fix: use `t1.Equal(t2)` for time comparison. When using `go-cmp`, add `cmpopts.EquateApproxTime` or a custom `cmp.Comparer` for `time.Time`.

### Treating nil and Empty Slices as Equivalent

Wrong: asserting `reflect.DeepEqual(gotTags, []string{})` when `gotTags` is `nil`.

What happens: the test fails even though from a logical standpoint "no tags" is the same either way.

Fix: either normalize both sides with `NilSliceAsEmpty` before comparing, or use `cmpopts.EquateEmpty()` with `go-cmp`.

### Using DeepEqual in Production Code for Business Logic

Wrong: using `reflect.DeepEqual` to compare domain objects in handler code.

What happens: you accidentally depend on unexported field equality, pointer addresses, or nil-vs-empty distinctions that are implementation details, not business rules.

Fix: write an `Equal(other T) bool` method that compares only the fields that matter for your business logic.

## Verification

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

## Summary

- `reflect.DeepEqual` recurses into slices, maps, and pointer targets; nil and empty slices are not equal.
- `time.Time` values that represent the same instant may differ under `DeepEqual` due to `*Location` pointer identity; use `t.Equal(other)`.
- `EqualIgnoring` lets tests skip volatile fields like timestamps and autoincrement IDs.
- `NilSliceAsEmpty` normalizes nil slices before comparison when the distinction is not meaningful.
- For production tests, prefer `go-cmp`'s `cmp.Diff` for readable, diff-based failure messages and options like `cmpopts.EquateEmpty` and `cmpopts.IgnoreFields`.

## What's Next

Next: [Reflection Performance Costs](../07-reflection-performance-costs/07-reflection-performance-costs.md).

## Resources

- [reflect.DeepEqual](https://pkg.go.dev/reflect#DeepEqual)
- [time.Time.Equal](https://pkg.go.dev/time#Time.Equal)
- [google/go-cmp](https://pkg.go.dev/github.com/google/go-cmp/cmp)
- [cmpopts package](https://pkg.go.dev/github.com/google/go-cmp/cmp/cmpopts)
