# Exercise 12: Canonicalize HTTP Headers from a Case-Colliding Map

**Nivel: Intermedio** — validacion rapida (un test corto).

A raw header map parsed by a lenient upstream can hold the same logical header
under different casings — `Content-Type` and `content-type` are distinct map
keys because Go map lookups are byte-for-byte, not case-insensitive. This
module folds such a map into a canonical, sorted list, merging colliding names
the way a proxy would, without ever depending on the order the map happened to
range in.

## What you'll build

```text
canon/                     independent module: example.com/header-canon
  go.mod                   go 1.24
  canon.go                 type Header; Canonicalize(raw map[string]string) []Header
  canon_test.go             table test: collision merge + single keys + empty input
```

- Files: `canon.go`, `canon_test.go`.
- Implement: `Canonicalize(raw map[string]string) []Header` grouping by
  lowercased key, joining colliding values in a deterministic order, and
  sorting the result by canonical name.
- Test: one table with a colliding pair, an unrelated single key, and an
  empty-map case.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/05-range-over-collections/12-header-canonicalization
cd go-solutions/03-control-flow/05-range-over-collections/12-header-canonicalization
go mod edit -go=1.24
```

### Two ranges, two different order problems

The first range walks `raw` to group entries by lowercased name — this is the
part where map order is genuinely irrelevant, because every entry lands in its
group regardless of visit order. The second problem is subtler: when two
original keys collide on the same canonical name, their values must join in
some fixed order, or two calls over an identical map could produce
`"application/json, charset=utf-8"` one run and `"charset=utf-8,
application/json"` the next, purely because the range visited the colliding
entries in a different sequence. Sorting each group's entries by their
original key before joining fixes that — a decision made from the data itself,
not from range order. The outer `Canonicalize` result is sorted by canonical
name for the same reason: anything that will be logged, diffed, or compared
across runs needs an order that does not come from `range`.

Create `canon.go`:

```go
package canon

import (
	"sort"
	"strings"
)

// Header is one canonicalized header name and its (possibly merged) value.
type Header struct {
	Name  string
	Value string
}

// Canonicalize folds a raw header map — which may hold the same logical
// header under different casings, since map keys are compared byte-for-byte
// — into a sorted, deduplicated slice. Values for colliding canonical names
// are joined with ", ", in the order of their original (pre-lowercase) keys,
// so the result is identical no matter how the map happened to range.
func Canonicalize(raw map[string]string) []Header {
	type entry struct {
		originalKey string
		value       string
	}
	groups := make(map[string][]entry)

	for k, v := range raw {
		name := strings.ToLower(k)
		groups[name] = append(groups[name], entry{originalKey: k, value: v})
	}

	out := make([]Header, 0, len(groups))
	for name, entries := range groups {
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].originalKey < entries[j].originalKey
		})
		values := make([]string, len(entries))
		for i, e := range entries {
			values[i] = e.value
		}
		out = append(out, Header{Name: name, Value: strings.Join(values, ", ")})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
```

### Test

The table covers a colliding case-varied pair (merged and joined in original-key
order), a single unrelated key (passed through unchanged), and an empty map.

Create `canon_test.go`:

```go
package canon

import (
	"reflect"
	"testing"
)

func TestCanonicalize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  map[string]string
		want []Header
	}{
		{
			name: "empty input",
			raw:  map[string]string{},
			want: []Header{},
		},
		{
			name: "colliding case-varied keys merge, single keys pass through",
			raw: map[string]string{
				"Content-Type": "application/json",
				"content-type": "charset=utf-8",
				"X-Request-Id": "abc123",
			},
			want: []Header{
				{Name: "content-type", Value: "application/json, charset=utf-8"},
				{Name: "x-request-id", Value: "abc123"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Canonicalize(tc.raw)
			if len(got) != len(tc.want) {
				t.Fatalf("Canonicalize() len = %d, want %d (%+v)", len(got), len(tc.want), got)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Canonicalize() = %+v, want %+v", got, tc.want)
			}
		})
	}
}
```

Run it:

```bash
go test -count=1 ./...
```

## Review

The function is correct when every original key contributes to exactly one
canonical entry and the merged value's part order never depends on how the map
ranged this run. Grouping tolerates any range order for free; the sort on
original key inside each group, and the sort on canonical name for the final
slice, are what make the output byte-identical across repeated calls on the
same input. Skipping either sort would still pass a single run of the test —
map randomization is not guaranteed to bite on every execution — which is
exactly why this class of bug survives code review and then flakes in CI.

## Resources

- [Go Specification: For statements (range over map)](https://go.dev/ref/spec#For_range) — map iteration order is unspecified.
- [strings.ToLower](https://pkg.go.dev/strings#ToLower)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-tenant-usage-rollup.md](11-tenant-usage-rollup.md) | Next: [13-leaderboard-top-n.md](13-leaderboard-top-n.md)
