# Exercise 8: Preallocate slice capacity in a serialization hot path

Transforming N domain rows into N DTOs by starting from a nil slice and
append-growing is correct, but it reallocates the backing array roughly log2(N)
times as capacity doubles — pure waste when N is known up front. This exercise
measures that cost with `testing.AllocsPerRun`, then eliminates it with
`make([]DTO, 0, len(rows))` and shows where `slices.Grow` fits instead.

This module is fully self-contained: its own `go mod init`, its own `transform`
package, its own demo and tests.

## What you'll build

```text
transform/                    independent module: example.com/transform
  go.mod
  transform/transform.go      TransformNil, TransformPrealloc, TransformGrow
  transform/race_on.go        raceEnabled=true  (//go:build race)
  transform/race_off.go       raceEnabled=false (//go:build !race)
  transform/transform_test.go agreement table, capacity check, AllocsPerRun, benchmarks
  cmd/demo/main.go            transforms 1000 rows, prints alloc counts
```

Files: `transform/transform.go`, `transform/race_on.go`, `transform/race_off.go`,
`transform/transform_test.go`, `cmd/demo/main.go`.
Implement: `TransformNil` (nil start), `TransformPrealloc`
(`make([]DTO, 0, len(rows))`), and `TransformGrow` (`slices.Grow`), all producing
identical output.
Test: all three strategies agree; `make([]DTO, 0, n)` is non-nil with len 0 and
cap n; `TransformPrealloc` does exactly one backing-array allocation while the
nil-start version does more; benchmarks for both.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/05-nil-slices-vs-empty-slices/08-preallocate-capacity-hot-path/transform go-solutions/06-collections-arrays-slices-and-maps/05-nil-slices-vs-empty-slices/08-preallocate-capacity-hot-path/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/05-nil-slices-vs-empty-slices/08-preallocate-capacity-hot-path
```

### Correct is not the same as cheap

`TransformNil` is the version most code ships first: declare `var out []DTO` and
`append` in a loop. It is completely correct. But a nil slice has capacity zero,
so the first `append` allocates a small backing array, and as the loop fills it
`append` keeps allocating larger arrays and copying the elements over — the growth
is geometric, so for a thousand rows the backing array is reallocated on the order
of ten times. Every one of those intermediate arrays is immediate garbage. In a
serialization hot path that runs on every request, that is a measurable amount of
allocation and GC pressure spent on nothing.

When the final size is known — and transforming a slice of rows, it always is —
`make([]DTO, 0, len(rows))` allocates the backing array exactly once, sized for
the whole result, and the loop's appends just fill it in place with no further
allocation. `TransformPrealloc` does this. The length starts at zero (so `append`
appends rather than overwriting) while the capacity is the full N. This is the
single most common and highest-value slice optimization in backend code.

`slices.Grow` is the variant for a different situation: you already hold a slice
and want to extend it by a known amount without the intermediate reallocations.
`slices.Grow(out, n)` returns a slice with capacity for at least `n` more
elements, reallocating at most once. Use `make([]T, 0, N)` when you are building
from scratch and `slices.Grow` when you are extending something you already have.

The important discipline is to measure rather than assume. `testing.AllocsPerRun`
runs a function repeatedly and reports the average number of heap allocations, so
the test can assert that the preallocated version does exactly one and the
nil-start version does more — turning "this should be faster" into a checked fact.
One caveat the code handles: `AllocsPerRun` counts are perturbed by the race
detector, so the allocation assertion is skipped when the binary is built with
`-race`, detected through a pair of build-tagged constants.

Create `transform/transform.go`:

```go
package transform

import "slices"

// Row is a domain record read from storage.
type Row struct {
	ID   int
	Name string
}

// DTO is the shape sent to the client.
type DTO struct {
	ID    int
	Label string
}

func toDTO(r Row) DTO {
	return DTO{ID: r.ID, Label: r.Name}
}

// TransformNil starts from a nil slice and append-grows. It is correct, but the
// backing array is reallocated roughly log2(N) times as capacity doubles.
func TransformNil(rows []Row) []DTO {
	var out []DTO
	for _, r := range rows {
		out = append(out, toDTO(r))
	}
	return out
}

// TransformPrealloc allocates the backing array once with make([]DTO, 0, N),
// since the final size is known up front. append never has to reallocate.
func TransformPrealloc(rows []Row) []DTO {
	out := make([]DTO, 0, len(rows))
	for _, r := range rows {
		out = append(out, toDTO(r))
	}
	return out
}

// TransformGrow reserves capacity on an existing slice with slices.Grow. It is
// the tool to reach for when you are extending a slice you already hold, rather
// than starting fresh.
func TransformGrow(rows []Row) []DTO {
	var out []DTO
	out = slices.Grow(out, len(rows))
	for _, r := range rows {
		out = append(out, toDTO(r))
	}
	return out
}
```

The two build-tagged files expose whether the race detector is active, so the
allocation test can skip its `AllocsPerRun` assertion under `-race`.

Create `transform/race_on.go`:

```go
//go:build race

package transform

// raceEnabled reports whether the binary was built with -race, under which
// testing.AllocsPerRun counts are unreliable.
const raceEnabled = true
```

Create `transform/race_off.go`:

```go
//go:build !race

package transform

const raceEnabled = false
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"testing"

	"example.com/transform/transform"
)

func main() {
	rows := make([]transform.Row, 1000)
	for i := range rows {
		rows[i] = transform.Row{ID: i, Name: fmt.Sprintf("u%d", i)}
	}

	nilAllocs := testing.AllocsPerRun(100, func() { _ = transform.TransformNil(rows) })
	preAllocs := testing.AllocsPerRun(100, func() { _ = transform.TransformPrealloc(rows) })

	out := transform.TransformPrealloc(rows)
	fmt.Printf("transformed %d rows, first=%+v\n", len(out), out[0])
	fmt.Printf("allocs: nil-start=%.0f preallocated=%.0f\n", nilAllocs, preAllocs)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
transformed 1000 rows, first={ID:0 Label:u0}
allocs: nil-start=10 preallocated=1
```

Ten allocations versus one, for the same thousand-row result. (The demo imports
`testing` only to reuse `AllocsPerRun` as a convenient allocation counter; the
measurement itself is what the test asserts.)

### Tests

`TestAllStrategiesAgree` confirms the optimization changes nothing observable:
all three functions produce identical output for sizes 0, 1, 7, and 100.
`TestPreallocMakeIsNonNilWithCapacity` pins the properties of `make([]DTO, 0, 8)`
— non-nil, length zero, capacity eight. `TestPreallocAllocatesOnce` is the
measurement: it asserts `TransformPrealloc` does exactly one backing-array
allocation and that the nil-start version does strictly more, skipping under
`-race` where the counts are unreliable. It does not call `t.Parallel`, because
`AllocsPerRun` must not run inside a parallel test. The benchmarks let you see the
per-op numbers directly with `go test -bench=. -benchmem`.

Create `transform/transform_test.go`:

```go
package transform

import (
	"slices"
	"testing"
)

func sample(n int) []Row {
	rows := make([]Row, n)
	for i := range rows {
		rows[i] = Row{ID: i, Name: "user"}
	}
	return rows
}

func TestAllStrategiesAgree(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, 1, 7, 100} {
		rows := sample(n)
		a := TransformNil(rows)
		b := TransformPrealloc(rows)
		c := TransformGrow(rows)
		if !slices.Equal(a, b) || !slices.Equal(b, c) {
			t.Fatalf("n=%d: strategies disagree: %v %v %v", n, a, b, c)
		}
	}
}

func TestPreallocMakeIsNonNilWithCapacity(t *testing.T) {
	t.Parallel()
	out := make([]DTO, 0, 8)
	if out == nil {
		t.Fatal("make([]DTO, 0, 8) must be non-nil")
	}
	if len(out) != 0 || cap(out) != 8 {
		t.Fatalf("len=%d cap=%d, want 0 and 8", len(out), cap(out))
	}
}

func TestPreallocAllocatesOnce(t *testing.T) {
	if raceEnabled {
		t.Skip("AllocsPerRun counts are unreliable under -race")
	}
	rows := sample(1000)
	got := testing.AllocsPerRun(100, func() {
		_ = TransformPrealloc(rows)
	})
	if got != 1 {
		t.Fatalf("TransformPrealloc allocations = %.0f, want 1", got)
	}
	nilStart := testing.AllocsPerRun(100, func() {
		_ = TransformNil(rows)
	})
	if nilStart <= got {
		t.Fatalf("nil-start allocations = %.0f, want more than preallocated %.0f", nilStart, got)
	}
}

func BenchmarkTransformNil(b *testing.B) {
	rows := sample(1000)
	b.ReportAllocs()
	for b.Loop() {
		_ = TransformNil(rows)
	}
}

func BenchmarkTransformPrealloc(b *testing.B) {
	rows := sample(1000)
	b.ReportAllocs()
	for b.Loop() {
		_ = TransformPrealloc(rows)
	}
}
```

## Review

The optimization is correct when all three strategies produce identical output —
starting from nil is never *wrong*, only wasteful — and it is proven worthwhile
when `TestPreallocAllocatesOnce` shows one allocation for the preallocated version
against many for the nil-start one. The rule to carry forward: when you know the
final length, `make([]T, 0, N)` allocates the backing array once; when you are
extending a slice you already hold, `slices.Grow` reserves capacity in one step.
Measure with `AllocsPerRun` or a benchmark rather than guessing, and remember
that `AllocsPerRun` cannot run in a parallel test and is distorted under `-race`.

## Resources

- [slices.Grow](https://pkg.go.dev/slices#Grow) — reserve capacity on an existing slice with at most one reallocation.
- [testing.AllocsPerRun](https://pkg.go.dev/testing#AllocsPerRun) — average heap allocations per call; not for parallel tests.
- [Go blog: Arrays, slices (and strings): the mechanics of append](https://go.dev/blog/slices) — how capacity growth reallocates.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-filter-in-place-zero-tail-pointers.md](09-filter-in-place-zero-tail-pointers.md)
