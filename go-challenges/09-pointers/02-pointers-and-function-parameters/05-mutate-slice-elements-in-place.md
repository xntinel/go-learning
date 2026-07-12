# Exercise 5: Normalization Pipeline — Why range Copies Drop Your Writes

A batch normalizer that trims and lowercases fields across a `[]Record` before
persistence is a place the range-copies-elements bug hides in plain sight:
`for _, r := range recs { r.Email = ... }` silently discards every write because
`r` is a copy. This exercise builds the buggy version and the two correct fixes
side by side, plus the `[]*Record` variant where mutation through the loop pointer
does persist.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
normalize/                  independent module: example.com/normalize
  go.mod
  normalize.go              Record; NormalizeBuggy; NormalizeIndex; NormalizePointer; NormalizePtrSlice
  cmd/
    demo/
      main.go               runs all four over the same data and prints results
  normalize_test.go         buggy leaves slice unchanged; index/&elem/[]*T persist
```

- Files: `normalize.go`, `cmd/demo/main.go`, `normalize_test.go`.
- Implement: a value-range normalizer that (buggily) drops writes, an index version, an `&recs[i]` version, and a `[]*Record` version.
- Test: prove the value-range version leaves the slice unchanged, the index and pointer versions persist, and the pointer-slice loop mutates the underlying records.
- Verify: `go test -count=1 -race ./...`

### Why the value-range write is lost

`for _, r := range recs` copies element `recs[i]` into a fresh `r` each iteration.
`r.Email = norm(r.Email)` writes to that copy; when the iteration ends, the copy is
discarded and `recs[i]` was never touched. The slice comes out exactly as it went
in. This is not a Go 1.22 regression and 1.22 did not fix it: 1.22 made `r` a
*per-iteration* variable (which fixed the old closure-capture aliasing bug), but the
element is still *copied* into `r`. Per-iteration scoping and element-copying are
orthogonal; the write is lost regardless.

There are three correct shapes. Index — `for i := range recs { recs[i].Email = ... }`
— assigns directly into the backing array. Address-of-element —
`p := &recs[i]; p.Email = ...` — takes a pointer into the backing array and mutates
through it (identical effect, sometimes cleaner when you touch several fields). And a
slice of pointers — `[]*Record`, where `for _, r := range recs` gives a copy of the
*pointer*, so `r.Email = ...` follows the pointer to the shared underlying record and
the write persists. The pointer-slice case is the one that "just works" with the
natural range syntax, because there the loop variable is already a reference.

Create `normalize.go`:

```go
package normalize

import "strings"

// Record is one row to be normalized before persistence.
type Record struct {
	Email string
	Name  string
}

func norm(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// NormalizeBuggy ranges by value: each write lands on the loop-variable copy and
// is discarded. The slice is returned UNCHANGED. Documented trap, not a fix.
func NormalizeBuggy(recs []Record) {
	for _, r := range recs {
		r.Email = norm(r.Email)
		r.Name = norm(r.Name)
	}
}

// NormalizeIndex mutates each element in place via its index.
func NormalizeIndex(recs []Record) {
	for i := range recs {
		recs[i].Email = norm(recs[i].Email)
		recs[i].Name = norm(recs[i].Name)
	}
}

// NormalizePointer takes the address of each element and mutates through it.
func NormalizePointer(recs []Record) {
	for i := range recs {
		p := &recs[i]
		p.Email = norm(p.Email)
		p.Name = norm(p.Name)
	}
}

// NormalizePtrSlice ranges over []*Record; the loop variable is a pointer, so
// mutating through it reaches the shared underlying record.
func NormalizePtrSlice(recs []*Record) {
	for _, r := range recs {
		r.Email = norm(r.Email)
		r.Name = norm(r.Name)
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/normalize"
)

func main() {
	raw := func() []normalize.Record {
		return []normalize.Record{
			{Email: "  ADA@X.com ", Name: " Ada "},
			{Email: "BOB@X.COM", Name: "BOB"},
		}
	}

	buggy := raw()
	normalize.NormalizeBuggy(buggy)
	fmt.Printf("buggy[0]:   %q %q\n", buggy[0].Email, buggy[0].Name)

	idx := raw()
	normalize.NormalizeIndex(idx)
	fmt.Printf("index[0]:   %q %q\n", idx[0].Email, idx[0].Name)

	ptrs := []*normalize.Record{{Email: "  ADA@X.com ", Name: " Ada "}}
	normalize.NormalizePtrSlice(ptrs)
	fmt.Printf("ptrslice:   %q %q\n", ptrs[0].Email, ptrs[0].Name)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
buggy[0]:   "  ADA@X.com " " Ada "
index[0]:   "ada@x.com" "ada"
ptrslice:   "ada@x.com" "ada"
```

### Tests

Create `normalize_test.go`:

```go
package normalize

import "testing"

func fixture() []Record {
	return []Record{
		{Email: "  ADA@X.com ", Name: " Ada "},
		{Email: "BOB@X.COM", Name: "BOB"},
	}
}

// TestBuggyLeavesSliceUnchanged documents the trap: value-range writes are lost.
func TestBuggyLeavesSliceUnchanged(t *testing.T) {
	t.Parallel()
	recs := fixture()
	NormalizeBuggy(recs)
	if recs[0].Email != "  ADA@X.com " || recs[0].Name != " Ada " {
		t.Fatalf("value-range unexpectedly mutated the slice: %+v", recs[0])
	}
}

func TestIndexPersists(t *testing.T) {
	t.Parallel()
	recs := fixture()
	NormalizeIndex(recs)
	if recs[0].Email != "ada@x.com" || recs[0].Name != "ada" {
		t.Fatalf("index normalize did not persist: %+v", recs[0])
	}
	if recs[1].Email != "bob@x.com" || recs[1].Name != "bob" {
		t.Fatalf("index normalize row 1: %+v", recs[1])
	}
}

func TestPointerElemPersists(t *testing.T) {
	t.Parallel()
	recs := fixture()
	NormalizePointer(recs)
	if recs[0].Email != "ada@x.com" || recs[0].Name != "ada" {
		t.Fatalf("&recs[i] normalize did not persist: %+v", recs[0])
	}
}

func TestPtrSlicePersists(t *testing.T) {
	t.Parallel()
	recs := []*Record{
		{Email: "  ADA@X.com ", Name: " Ada "},
		{Email: "BOB@X.COM", Name: "BOB"},
	}
	NormalizePtrSlice(recs)
	if recs[0].Email != "ada@x.com" || recs[1].Name != "bob" {
		t.Fatalf("[]*Record normalize did not persist: %+v %+v", recs[0], recs[1])
	}
}

// TestIndexAndPointerAgree pins that the two in-place fixes are equivalent.
func TestIndexAndPointerAgree(t *testing.T) {
	t.Parallel()
	a, b := fixture(), fixture()
	NormalizeIndex(a)
	NormalizePointer(b)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("row %d differs: index=%+v pointer=%+v", i, a[i], b[i])
		}
	}
}
```

## Review

The pipeline is correct when normalization actually reaches the persisted slice —
which the value-range version provably does not. `TestBuggyLeavesSliceUnchanged` is
kept deliberately: it documents the trap as executable truth, so a future reader who
"cleans up" `NormalizeIndex` back into a value range will see it fail. The three
working shapes are equivalent (`TestIndexAndPointerAgree` pins index and `&recs[i]`),
and the `[]*Record` variant is the one that works with the natural range syntax
because the element is already a pointer. The single sentence to remember: ranging a
`[]T` hands you a copy of each element; index or take `&s[i]` to mutate in place.
Run `go test -race`; the functions are single-goroutine, so this confirms
correctness and formatting.

## Resources

- [Go Spec: For statements with range clause](https://go.dev/ref/spec#For_range) — the rule that the range variable is assigned a copy of each element.
- [Go 1.22 release notes: loop variable scoping](https://go.dev/doc/go1.22#language) — what per-iteration scoping did and did not change.
- [`strings` package](https://pkg.go.dev/strings#ToLower) — `ToLower`/`TrimSpace` used by the normalizer.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-repository-scan-into-pointers.md](04-repository-scan-into-pointers.md) | Next: [06-atomic-pointer-config-hot-reload.md](06-atomic-pointer-config-hot-reload.md)
