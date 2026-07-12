# Exercise 15: Deduplicate Request Tags Preserving First-Seen Order

**Nivel: Intermedio** — validacion rapida (un test corto).

A request can arrive with the same trace tag attached twice — a retry, a proxy
that double-forwards a header, a client bug — and a downstream system that
fans a tag out to N side effects needs each tag exactly once, in the order the
caller first sent it. This module ranges a slice once, uses a map purely as a
"have I seen this?" set, and appends to a separate output slice, so dedup
never disturbs arrival order.

## What you'll build

```text
tagdedup/                   independent module: example.com/tag-dedup
  go.mod                    go 1.24
  dedup.go                  DedupTags(tags []string) []string
  dedup_test.go              table test: repeats + blanks + empty input
```

- Files: `dedup.go`, `dedup_test.go`.
- Implement: `DedupTags(tags []string) []string` ranging the input slice once,
  tracking seen tags in a `map[string]bool`, and appending each unseen,
  non-blank tag to an output slice in arrival order.
- Test: one table covering repeated tags interleaved with a blank entry, plus
  an empty-input case.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/05-range-over-collections/15-tag-dedup-first-seen
cd go-solutions/03-control-flow/05-range-over-collections/15-tag-dedup-first-seen
go mod edit -go=1.24
```

### The map is scratch space, not the answer

It is tempting to build a `map[string]bool` of tags and think the job is done
— a map already refuses to hold a duplicate key. But a map has no reliable
order, and "the tags, deduplicated" as a production requirement almost always
means "in the order the caller sent them," which a map cannot give you no
matter how you range it afterward. The fix is to keep the map purely as
membership scratch space — `seen[tag]` — and let a second, ordinary slice
carry the real output, appended to exactly once per new tag as the single
range over the input progresses. The output's order is then just "the order
`append` ran," which is the input's order by construction, not something
reconstructed after the fact.

Create `dedup.go`:

```go
package tagdedup

// DedupTags ranges tags once, keeping a map[string]bool only as a "have we
// seen this?" set — never as the output itself — so the result preserves the
// first-seen order of the input instead of whatever order a map would give.
// Blank tags are dropped; nil input returns a non-nil empty slice.
func DedupTags(tags []string) []string {
	seen := make(map[string]bool, len(tags))
	out := make([]string, 0, len(tags))

	for _, tag := range tags {
		if tag == "" || seen[tag] {
			continue
		}
		seen[tag] = true
		out = append(out, tag)
	}

	return out
}
```

### Test

The table covers a run of tags with a repeat pattern and a blank entry mixed
in — proving both dedup and blank-dropping hold arrival order — plus an
empty-input case.

Create `dedup_test.go`:

```go
package tagdedup

import (
	"reflect"
	"testing"
)

func TestDedupTags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		tags []string
		want []string
	}{
		{
			name: "empty input",
			tags: []string{},
			want: []string{},
		},
		{
			name: "repeats collapse, first occurrence order kept, blanks dropped",
			tags: []string{"beta", "alpha", "beta", "", "gamma", "alpha", "beta"},
			want: []string{"beta", "alpha", "gamma"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DedupTags(tc.tags)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("DedupTags(%v) = %v, want %v", tc.tags, got, tc.want)
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

`DedupTags` is correct when the output holds each distinct, non-blank tag
exactly once, in the order it first appeared — not the order a map would
range in, and not sorted. The `seen` map's only job is an O(1) membership
check; it is never ranged and never becomes the return value, which is the
detail that keeps this function's contract ("first-seen order") true instead
of accidentally true only when a map happens to range in insertion order (it
does not, by design).

## Resources

- [Go Specification: For statements (range over slice)](https://go.dev/ref/spec#For_range)
- [Effective Go: Maps](https://go.dev/doc/effective_go#maps)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [14-csv-row-validation.md](14-csv-row-validation.md) | Next: [16-idempotency-key-gate-windowed.md](16-idempotency-key-gate-windowed.md)
