# Exercise 4: Zero-Reallocation Bulk-Insert Argument Builder

Building the flat `[]any` argument list for a multi-row SQL `INSERT` is a hot
path in any write-heavy service. Done wrong it allocates repeatedly and, worse,
prepends a block of zero values. This exercise builds the args list with exactly
one allocation using a capacity hint, and uses `slices.Grow` to pre-extend an
existing buffer before a known-size append burst.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
bulkinsert/                 independent module: example.com/bulkinsert
  go.mod
  bulkinsert.go             BuildArgs (make cap hint); AppendRows (slices.Grow); BadBuild
  cmd/
    demo/
      main.go               runnable demo: build args, print count and shape
  bulkinsert_test.go        one-alloc path, length-vs-capacity bug, Grow keeps len
```

Files: `bulkinsert.go`, `cmd/demo/main.go`, `bulkinsert_test.go`.
Implement: `BuildArgs(rows [][]any) []any` (one allocation), `AppendRows(dst []any, rows [][]any) []any` using `slices.Grow`, and `BadBuild` demonstrating the length-preallocation mistake.
Test: `testing.AllocsPerRun(_, build) == 1`; the `make([]T, n)` bug yields leading zeros; `slices.Grow` keeps `len` but raises `cap`, and the follow-up append burst does not reallocate.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/07-slice-internals/04-preallocated-bulk-insert-args/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/07-slice-internals/04-preallocated-bulk-insert-args
```

### One allocation with a capacity hint

The row count and the columns-per-row are known before you build the args, so the
total length is known: `sum(len(row))`. `BuildArgs` computes that total, calls
`make([]any, 0, total)` — length zero, capacity total — and appends each row's
already-boxed `any` values. Because the values arrive as `[]any` (already
interface-boxed by the caller), spreading `row...` into the args copies interface
words without re-boxing, so the *only* allocation is the single `make`. That is
what `testing.AllocsPerRun(_, build) == 1` proves.

The wrong version, `BadBuild`, makes the classic mistake: `make([]any, total)`
allocates `total` **zero** (`nil`) interface values and sets length to `total`,
so the subsequent `append` adds the real data *after* the zeros and reallocates
once it overflows. The result is twice the length, a leading block of `nil`, and
an extra allocation. Preallocate **capacity** (`make([]T, 0, n)`), never length,
for an append burst.

`AppendRows` shows the other tool. When you already hold a partially-filled
buffer and want to append a known number of elements through existing append
code, `slices.Grow(dst, n)` guarantees `cap(dst) >= len(dst)+n` — reallocating at
most once, up front — without changing `len(dst)`. After the grow, the append
burst writes into the reserved capacity and never reallocates, so the backing
array pointer stays stable across the whole burst.

Create `bulkinsert.go`:

```go
package bulkinsert

import "slices"

// BuildArgs flattens the per-row argument slices into one []any suitable for a
// multi-row INSERT. It preallocates the exact capacity, so the whole build is a
// single allocation (the values are already interface-boxed, so spreading them
// does not allocate).
func BuildArgs(rows [][]any) []any {
	total := 0
	for _, row := range rows {
		total += len(row)
	}
	args := make([]any, 0, total)
	for _, row := range rows {
		args = append(args, row...)
	}
	return args
}

// BadBuild is the length-preallocation mistake: make([]any, total) fills the
// slice with total nil values first, so appending the real data yields leading
// nils and an extra reallocation. Kept as a contrast; do not use it.
func BadBuild(rows [][]any) []any {
	total := 0
	for _, row := range rows {
		total += len(row)
	}
	args := make([]any, total) // WRONG: length, not capacity
	for _, row := range rows {
		args = append(args, row...)
	}
	return args
}

// AppendRows appends the rows' arguments onto dst, pre-extending dst with
// slices.Grow so the append burst reallocates at most once (inside Grow) and
// never again. len(dst) is unchanged by Grow; only cap grows.
func AppendRows(dst []any, rows [][]any) []any {
	total := 0
	for _, row := range rows {
		total += len(row)
	}
	dst = slices.Grow(dst, total)
	for _, row := range rows {
		dst = append(dst, row...)
	}
	return dst
}
```

### The runnable demo

The demo builds the args for three rows of three columns and shows the correct
build has 9 values while the length-preallocation bug produces 18 with a leading
block of `<nil>`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/bulkinsert"
)

func main() {
	rows := [][]any{
		{1, "alice", "a@example.com"},
		{2, "bob", "b@example.com"},
		{3, "carol", "c@example.com"},
	}

	good := bulkinsert.BuildArgs(rows)
	fmt.Printf("BuildArgs: len=%d first=%v\n", len(good), good[0])

	bad := bulkinsert.BadBuild(rows)
	fmt.Printf("BadBuild:  len=%d first=%v\n", len(bad), bad[0])
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
BuildArgs: len=9 first=1
BadBuild:  len=18 first=<nil>
```

### Tests

`TestBuildArgsOneAllocation` is the headline: it asserts the preallocated build
is exactly one allocation. (It does not call `t.Parallel()`, because
`testing.AllocsPerRun` must not run in a parallel test.) `TestBadBuildLeadingZeros`
proves the length-preallocation bug produces leading `nil`s and double length.
`TestAppendRowsGrowKeepsLen` proves `slices.Grow` leaves `len` unchanged while
raising `cap`, and that the follow-up append burst does not reallocate.

Create `bulkinsert_test.go`:

```go
package bulkinsert

import (
	"fmt"
	"slices"
	"testing"
	"unsafe"
)

func sampleRows(n int) [][]any {
	rows := make([][]any, n)
	for i := range rows {
		rows[i] = []any{i, "name", "email"}
	}
	return rows
}

func TestBuildArgsOneAllocation(t *testing.T) {
	rows := sampleRows(50)
	allocs := testing.AllocsPerRun(100, func() {
		sink := BuildArgs(rows)
		_ = sink
	})
	if allocs != 1 {
		t.Fatalf("BuildArgs made %v allocations, want exactly 1", allocs)
	}
}

func TestBuildArgsFlattensInOrder(t *testing.T) {
	t.Parallel()
	rows := [][]any{{1, 2}, {3, 4}, {5, 6}}
	got := BuildArgs(rows)
	if len(got) != 6 {
		t.Fatalf("len = %d, want 6", len(got))
	}
	for i := range got {
		if got[i] != i+1 {
			t.Fatalf("args[%d] = %v, want %d", i, got[i], i+1)
		}
	}
}

func TestBadBuildLeadingZeros(t *testing.T) {
	t.Parallel()
	rows := [][]any{{1, 2}, {3, 4}}
	bad := BadBuild(rows)
	if len(bad) != 8 {
		t.Fatalf("BadBuild len = %d, want 8 (4 nils + 4 values)", len(bad))
	}
	for i := range 4 {
		if bad[i] != nil {
			t.Fatalf("bad[%d] = %v, want nil (the leading-zero bug)", i, bad[i])
		}
	}
}

func TestGrowRaisesCapNotLen(t *testing.T) {
	t.Parallel()
	s := make([]any, 3, 4)
	grown := slices.Grow(s, 10)
	if len(grown) != 3 {
		t.Fatalf("Grow changed len to %d, want 3", len(grown))
	}
	if cap(grown) < 13 {
		t.Fatalf("Grow cap = %d, want >= 13 (len+n)", cap(grown))
	}
}

func TestGrowThenBurstDoesNotRealloc(t *testing.T) {
	t.Parallel()
	dst := make([]any, 0, 2)
	dst = append(dst, "existing")
	dst = slices.Grow(dst, 30)
	before := unsafe.SliceData(dst)
	for i := range 30 {
		dst = append(dst, i)
	}
	if after := unsafe.SliceData(dst); before != after {
		t.Fatal("append burst after Grow reallocated; the backing array must be stable")
	}
}

func TestAppendRowsFlattensOntoDst(t *testing.T) {
	t.Parallel()
	dst := []any{"existing"}
	out := AppendRows(dst, sampleRows(10)) // 30 more values
	if len(out) != 31 {
		t.Fatalf("AppendRows len = %d, want 31", len(out))
	}
	if out[0] != "existing" {
		t.Fatalf("AppendRows[0] = %v, want existing", out[0])
	}
}

func ExampleBuildArgs() {
	args := BuildArgs([][]any{{1, "a"}, {2, "b"}})
	fmt.Println(len(args), args[0], args[1])
	// Output: 4 1 a
}
```

## Review

The build is correct when `BuildArgs` allocates exactly once —
`testing.AllocsPerRun == 1` — which requires two things: preallocate **capacity**
(`make([]any, 0, total)`, not `make([]any, total)`), and let the already-boxed
`any` values flow through `append(args, row...)` without re-boxing.
`TestBadBuildLeadingZeros` shows the failure mode of getting the length/capacity
distinction wrong: a block of leading `nil`s and a wasted reallocation.
`slices.Grow` is the same idea for an existing buffer: it raises `cap` without
touching `len`, so a following known-size append burst reallocates at most once.
Never call `AllocsPerRun` from a parallel test. Run `go test -race`.

## Resources

- [pkg.go.dev: slices.Grow](https://pkg.go.dev/slices#Grow) — pre-extend without changing length.
- [pkg.go.dev: testing.AllocsPerRun](https://pkg.go.dev/testing#AllocsPerRun) — asserting a one-allocation hot path.
- [Go blog: Go Slices: usage and internals](https://go.dev/blog/slices-intro) — `make` length versus capacity.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-append-aliasing-corruption.md](05-append-aliasing-corruption.md)
