# Exercise 15: A Notification Fanout Recipient List Builder

**Nivel: Intermedio** — validacion rapida (un test corto).

A notification fanout — email, SMS, or push — often assembles its final
recipient list from several independent sources: a team distribution list,
an on-call escalation list, a few manually cc'd addresses. Each source is
already a `[]string`, so the natural signature is a variadic *of slices*:
`Recipients(sources ...[]string) []string`, merged with case-insensitive
deduplication so `Alice@Example.com` and `alice@example.com` don't page the
same person twice.

## What you'll build

```text
fanout/                     independent module: example.com/notify-fanout
  go.mod                    go 1.24
  fanout.go                 package fanout; func Recipients(sources ...[]string) []string
  fanout_test.go            table test: merge order, case-insensitive dedup, empty entries, zero sources
```

- Files: `fanout.go`, `fanout_test.go`.
- Implement: `Recipients(sources ...[]string) []string` merging any number of address slices, deduplicated case-insensitively, empty addresses dropped.
- Test: two sources with an overlapping address merge in first-seen order; a duplicate differing only in case keeps the first casing; empty strings are dropped; zero sources returns an empty slice.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/03-variadic-functions/15-notification-fanout-recipients
cd go-solutions/04-functions/03-variadic-functions/15-notification-fanout-recipients
go mod edit -go=1.24
```

### A variadic of slices, and first-seen wins

`sources ...[]string` means the outer variadic collects a `[][]string` —
each caller-supplied source stays its own slice; nothing is flattened
before `Recipients` sees it. The function walks the sources in argument
order and, within each, its addresses in order, so the overall iteration
order is deterministic and matches how a caller would list its sources
(team list first, on-call second, manual cc's last, say). Deduplication
uses `strings.ToLower` as the comparison key but *stores the original
string* the first time a key is seen — so the output preserves whatever
casing the recipient's address had on its first appearance, which matters
because the dedup key is case-insensitive but the address you actually
send to should look like something a human typed, not a lowercased
normal form. An empty string is never a valid address, so it is dropped
regardless of which source it came from.

Create `fanout.go`:

```go
// fanout.go
package fanout

import "strings"

// Recipients merges any number of recipient-address sources into one
// ordered, deduplicated list. Comparison is case-insensitive (so
// "Alice@Example.com" and "alice@example.com" are the same recipient), the
// first-seen casing and position win, and empty addresses are dropped.
func Recipients(sources ...[]string) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0)

	for _, src := range sources {
		for _, addr := range src {
			if addr == "" {
				continue
			}
			key := strings.ToLower(addr)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, addr)
		}
	}
	return out
}
```

### Test

Create `fanout_test.go`:

```go
// fanout_test.go
package fanout

import (
	"reflect"
	"testing"
)

func TestRecipients(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		sources [][]string
		want    []string
	}{
		{
			name: "merges distinct sources in order",
			sources: [][]string{
				{"oncall@example.com"},
				{"team@example.com", "oncall@example.com"},
			},
			want: []string{"oncall@example.com", "team@example.com"},
		},
		{
			name: "case-insensitive dedup keeps first casing",
			sources: [][]string{
				{"Alice@Example.com"},
				{"alice@example.com"},
			},
			want: []string{"Alice@Example.com"},
		},
		{
			name: "empty addresses are dropped",
			sources: [][]string{
				{"", "bob@example.com", ""},
			},
			want: []string{"bob@example.com"},
		},
		{
			name:    "zero sources returns empty slice",
			sources: nil,
			want:    []string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Recipients(tc.sources...)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Recipients(%v) = %v, want %v", tc.sources, got, tc.want)
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

`Recipients` is correct when addresses merge in the order their sources
were given and appear within each source, a case-insensitive duplicate
collapses to its first-seen spelling, empty addresses never reach the
output, and zero sources returns an empty (never nil) slice. The senior
point: a variadic parameter's element type does not have to be a scalar —
`...[]string` is a perfectly ordinary shape for "a variable number of
already-grouped lists to merge", and the dedup/order contract you choose
here (first-seen wins, case-insensitive) is exactly the kind of behavior
that must be pinned down by a test rather than left as an implicit
assumption about map iteration order.

## Resources

- [`strings.ToLower`](https://pkg.go.dev/strings#ToLower) — the case-insensitive comparison key used for dedup.
- [Go Spec: Passing arguments to `...` parameters](https://go.dev/ref/spec#Passing_arguments_to_..._parameters) — variadic parameters whose element type is itself a composite type.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [14-csv-row-builder.md](14-csv-row-builder.md) | Next: [16-hash-ring-endpoint-aggregator.md](16-hash-ring-endpoint-aggregator.md)
