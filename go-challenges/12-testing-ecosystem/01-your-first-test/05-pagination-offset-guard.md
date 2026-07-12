# Exercise 5: Offset: Testing a Boundary Guard

Behind every "page 3 of results" is an arithmetic guard that turns a page number
into a database `OFFSET`. It is a one-liner with an off-by-one and two defensive
clamps, and that is exactly where a first test earns its keep — on the boundaries,
not the happy path.

## What you'll build

```text
pageoffset/                independent module: example.com/pageoffset
  go.mod
  offset.go                DefaultPageSize; func Offset(page, size int) int
  offset_test.go           TestOffset (discrete boundary assertions), ExampleOffset
  cmd/
    demo/
      main.go              prints the offset for several page/size pairs
```

- Files: `offset.go`, `offset_test.go`, `cmd/demo/main.go`.
- Implement: `Offset(page, size int) int` returning `(page-1)*size`, clamping `page < 1` to page 1 and `size <= 0` to `DefaultPageSize`.
- Test: the happy cases and, more importantly, the defensive cases `page=0` and `size=-5`, each as a discrete assertion.
- Verify: `gofmt -l .`, `go vet ./...`, `go test -count=1 -race ./...`.

### The off-by-one and the two clamps

Pages are one-based for humans and offsets are zero-based for the database, so
page 1 must map to offset 0, page 2 to offset `size`, page 3 to `2*size`. The
formula is `(page-1)*size`. Get the `-1` wrong and every list endpoint silently
skips or repeats a page of rows — a bug that ships green because the happy path
"works" and nobody tested page 1.

The two clamps guard against hostile or sloppy input arriving from a query
string. `page` comes off `?page=` as an integer that a client can set to `0` or
`-3`; a negative page would produce a negative offset and a database error or,
worse, wrap-around. So `page` is clamped up to 1 with the `max` builtin. `size`
comes off `?per_page=`; a zero or negative size is meaningless and a caller who
omits it should get a sane default, so `size` is normalized: a negative value is
floored to 0, then `cmp.Or` substitutes `DefaultPageSize` for a zero. `cmp.Or`
returns its first non-zero argument, which is exactly the "use the default when
unset" semantics — once the negative is floored to zero, `cmp.Or(size,
DefaultPageSize)` reads as "size, or the default if size is zero".

The test focuses on boundaries because that is where guards live. `page=1` must
give offset 0 (the off-by-one), `page=3, size=20` must give 40 (the general
case), and the defensive `page=0` and `size=-5` must be clamped, not crash.
Writing each as a discrete `t.Errorf` means a single run reports every boundary
the guard gets wrong.

Create `offset.go`:

```go
package pageoffset

import "cmp"

// DefaultPageSize is used when a caller supplies a non-positive page size.
const DefaultPageSize = 20

// Offset converts a one-based page number and page size into a zero-based
// database offset. A page below 1 is clamped to page 1; a size of zero or less
// falls back to DefaultPageSize.
func Offset(page, size int) int {
	page = max(page, 1)
	if size < 0 {
		size = 0
	}
	size = cmp.Or(size, DefaultPageSize)
	return (page - 1) * size
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/pageoffset"
)

func main() {
	pairs := [][2]int{{1, 50}, {3, 20}, {0, 20}, {2, -5}}
	for _, p := range pairs {
		fmt.Printf("page=%d size=%d -> offset=%d\n",
			p[0], p[1], pageoffset.Offset(p[0], p[1]))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
page=1 size=50 -> offset=0
page=3 size=20 -> offset=40
page=0 size=20 -> offset=0
page=2 size=-5 -> offset=20
```

### The tests

Create `offset_test.go`:

```go
package pageoffset

import (
	"fmt"
	"testing"
)

func TestOffset(t *testing.T) {
	t.Parallel()

	// Happy path: the off-by-one and the general case.
	if got, want := Offset(1, 50), 0; got != want {
		t.Errorf("Offset(1, 50) = %d, want %d", got, want)
	}
	if got, want := Offset(3, 20), 40; got != want {
		t.Errorf("Offset(3, 20) = %d, want %d", got, want)
	}

	// Defensive: page below 1 clamps to page 1 (offset 0).
	if got, want := Offset(0, 20), 0; got != want {
		t.Errorf("Offset(0, 20) = %d, want %d", got, want)
	}

	// Defensive: non-positive size falls back to DefaultPageSize.
	if got, want := Offset(2, -5), DefaultPageSize; got != want {
		t.Errorf("Offset(2, -5) = %d, want %d (DefaultPageSize)", got, want)
	}
}

func ExampleOffset() {
	fmt.Println(Offset(3, 20))
	// Output: 40
}
```

## Review

The guard is correct when page 1 maps to offset 0 (proving the `-1`), the general
case computes `(page-1)*size`, and the two clamps hold: `page=0` becomes page 1
(offset 0) and `size=-5` becomes `DefaultPageSize`. The boundary cases are the
whole point — a test that only checked `Offset(3, 20)` would pass with a broken
`page` clamp. `cmp.Or` reads cleanly here only because the negative size is
floored to zero first; `cmp.Or` treats zero as "unset", not negatives. Gate with
`gofmt -l .`, `go vet ./...`, and `go test -count=1 -race ./...`.

## Resources

- [cmp.Or](https://pkg.go.dev/cmp#Or) — first non-zero argument, the "value or default" idiom.
- [Go builtins: max and min](https://pkg.go.dev/builtin#max) — the 1.21 clamping builtins.
- [testing package](https://pkg.go.dev/testing) — discrete assertions with `Errorf`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-slugify-url-path.md](04-slugify-url-path.md) | Next: [06-deterministic-backoff-duration.md](06-deterministic-backoff-duration.md)
