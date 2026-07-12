# Exercise 13: The Paginator That Assumed Map Range Order

**Nivel: Intermedio** — validacion rapida (un test corto).

A pagination helper builds its key list by ranging directly over a
`map[string]int` and slicing the result by offset. It worked in every manual
check the author ran, then produced pages that skipped and repeated items in
production. Go deliberately randomizes map iteration order per run, so
"page 1" and "page 2" computed from two separate range loops over the same
map are not guaranteed to agree on any ordering at all. You will reproduce
the instability, diagnose the missing sort, and fix it by sorting the keys
before slicing.

## What you'll build

```text
page/                       module example.com/page
  go.mod
  page.go                   Page(items map[string]int, offset, pageSize int) []string
  page_test.go               two-page stability + no-overlap assertion
```

- Files: `page.go`, `page_test.go`.
- Implement: `Page(items map[string]int, offset, pageSize int) []string` that returns a stable, deterministic slice of keys for any offset.
- Test: build a 10-key map, request page 0 and page 1, and assert each equals the expected sorted slice with no key appearing on both pages.
- Verify: `go test -count=1 ./...`.

```bash
mkdir -p go-solutions/03-control-flow/10-control-flow-debugging-challenge/13-range-over-map-assumed-ordered-pagination
cd go-solutions/03-control-flow/10-control-flow-debugging-challenge/13-range-over-map-assumed-ordered-pagination
```

### The artifact and the planted bug

```go
func Page(items map[string]int, offset, pageSize int) []string {
	var keys []string
	for k := range items { // BUG: map range order is randomized per run
		keys = append(keys, k)
	}
	if offset > len(keys) {
		return nil
	}
	end := offset + pageSize
	if end > len(keys) {
		end = len(keys)
	}
	return keys[offset:end]
}
```

`for k := range items` visits a Go map's keys in an order the runtime
deliberately randomizes across process runs — it is not insertion order, not
sorted order, and not stable even between two range loops in the *same*
process for some map internals. Building "page 0" and "page 1" by calling
this function twice against the same map therefore has no guaranteed
relationship between the two results: a key can appear on both pages, or on
neither, depending on what the runtime handed back each time. It passed a
quick manual check because a single ad hoc run of a small map often *looks*
ordered by coincidence; the failure only shows up as flaky, hard-to-reproduce
duplicate or missing rows once real traffic hits it repeatedly.

A failing run reads (the exact keys vary by run, since the bug is
non-determinism itself):

```text
--- FAIL: TestPageIsStableAcrossPages
    page_test.go:20: page0 = [k5 k7 k8], want [k0 k1 k2]
```

The fix collects the keys once, sorts them, and only then slices by offset,
so the "order" is a property of the data, not of the map's internal
randomization:

```go
func Page(items map[string]int, offset, pageSize int) []string {
	keys := make([]string, 0, len(items))
	for k := range items {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	if offset > len(keys) {
		return nil
	}
	end := offset + pageSize
	if end > len(keys) {
		end = len(keys)
	}
	return keys[offset:end]
}
```

Create `page.go`:

```go
package page

import "sort"

// Page returns the pageSize keys of items starting at offset, in a stable,
// sorted order so repeated calls (and different pages) never overlap or skip.
func Page(items map[string]int, offset, pageSize int) []string {
	keys := make([]string, 0, len(items))
	for k := range items {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	if offset > len(keys) {
		return nil
	}
	end := offset + pageSize
	if end > len(keys) {
		end = len(keys)
	}
	return keys[offset:end]
}
```

### Tests

Create `page_test.go`:

```go
package page

import (
	"reflect"
	"testing"
)

func TestPageIsStableAcrossPages(t *testing.T) {
	items := map[string]int{
		"k0": 0, "k1": 1, "k2": 2, "k3": 3, "k4": 4,
		"k5": 5, "k6": 6, "k7": 7, "k8": 8, "k9": 9,
	}

	page0 := Page(items, 0, 3)
	page1 := Page(items, 3, 3)

	wantPage0 := []string{"k0", "k1", "k2"}
	wantPage1 := []string{"k3", "k4", "k5"}
	if !reflect.DeepEqual(page0, wantPage0) {
		t.Fatalf("page0 = %v, want %v", page0, wantPage0)
	}
	if !reflect.DeepEqual(page1, wantPage1) {
		t.Fatalf("page1 = %v, want %v", page1, wantPage1)
	}

	seen := map[string]bool{}
	for _, k := range append(append([]string{}, page0...), page1...) {
		if seen[k] {
			t.Fatalf("key %q returned on more than one page", k)
		}
		seen[k] = true
	}
}
```

Run: `go test -count=1 ./...`. Because the map has 10 keys, the odds of the
buggy version's randomized order accidentally matching the sorted prefix are
negligible, so the test fails reliably against the buggy version.

## Review

A `map` in Go carries no ordering guarantee, by design — the runtime
randomizes iteration order specifically so nobody depends on it accidentally.
Any code that needs a stable order, including pagination, must impose one
explicitly by collecting the keys and sorting them (or otherwise deriving a
total order from the data, such as an insertion timestamp). The test asserts
both pages against fixed expected slices and cross-checks that no key
appears on both, since a version that merely sorted the wrong field could
still pass a looser "is it sorted somehow" check.

## Resources

- [Go Specification: For statements — range](https://go.dev/ref/spec#For_range) — "The iteration order over maps is not specified and is not guaranteed to be the same from one iteration to the next."
- [sort.Strings](https://pkg.go.dev/sort#Strings) — imposing a deterministic order on map keys before use.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-defer-registered-too-late-leak-on-error-path.md](12-defer-registered-too-late-leak-on-error-path.md) | Next: [14-shadowed-err-in-nested-if-swallows-failure.md](14-shadowed-err-in-nested-if-swallows-failure.md)
