# Exercise 15: Reconciliation Differ Driven by Key and Equality Callbacks

**Nivel: Intermedio** — validacion rapida (un test corto).

A reconciler — the core loop behind Terraform, Kubernetes controllers, and
any "sync desired state to actual state" system — needs two independent
callbacks: one to match records across two lists, and one to decide whether a
matched pair has actually drifted. This module builds that generic differ.

## What you'll build

```text
reconcile/                  independent module: example.com/reconcile-differ
  go.mod                     go 1.24
  reconcile.go                type KeyFunc, type EqualFunc, type Diff, func Reconcile
  reconcile_test.go           table test: no-op diff vs add/remove/update
```

Files: `reconcile.go`, `reconcile_test.go`.
Implement: `type KeyFunc[T any] func(record T) string`,
`type EqualFunc[T any] func(desired, actual T) bool`, a `Diff[T]` struct with
`Added`, `Removed`, `Updated` slices, and
`func Reconcile[T any](desired, actual []T, key KeyFunc[T], equal EqualFunc[T]) Diff[T]`.
Test: a table with an identical-states case (empty diff) and a mixed case
exercising all three outcomes in one pass.
Verify: `go test -count=1 ./...`

```bash
mkdir -p go-solutions/04-functions/06-function-types-and-callbacks/15-reconciliation-differ-callback
cd go-solutions/04-functions/06-function-types-and-callbacks/15-reconciliation-differ-callback
go mod edit -go=1.24
```

### Why key and equality are two separate callbacks

`KeyFunc` answers "which desired record does this actual record correspond
to" — usually a resource name or primary key. `EqualFunc` answers a different
question entirely: "given a matched pair, has anything worth acting on
changed." Collapsing them into one callback would force a caller who wants to
ignore a generated timestamp or an internal ID when comparing to also
reinvent identity matching from scratch. Kept separate, a caller can match on
`ID` while comparing only the fields that matter operationally — exactly how
a real desired-state reconciler decides "same resource, but drifted" versus
"same resource, no action needed."

Create `reconcile.go`:

```go
package reconcile

// KeyFunc extracts the identity of a record — the field a reconciler matches
// desired against actual on (a resource name, a primary key, and so on).
type KeyFunc[T any] func(record T) string

// EqualFunc reports whether two records with the same key are equivalent. It
// is deliberately separate from KeyFunc: identity and equality are different
// questions, and a caller may want to ignore some fields (timestamps,
// generated IDs) when deciding "is this actually changed."
type EqualFunc[T any] func(desired, actual T) bool

// Diff is the result of reconciling a desired state against an actual state.
type Diff[T any] struct {
	Added   []T // present in desired, missing from actual
	Removed []T // present in actual, missing from desired
	Updated []T // present in both, but not Equal (the desired version)
}

// Reconcile compares desired against actual using key to match records and
// equal to decide whether a matched pair has drifted. It never mutates
// either input slice.
func Reconcile[T any](desired, actual []T, key KeyFunc[T], equal EqualFunc[T]) Diff[T] {
	actualByKey := make(map[string]T, len(actual))
	for _, a := range actual {
		actualByKey[key(a)] = a
	}

	var diff Diff[T]
	seen := make(map[string]bool, len(desired))

	for _, d := range desired {
		k := key(d)
		seen[k] = true
		if a, ok := actualByKey[k]; ok {
			if !equal(d, a) {
				diff.Updated = append(diff.Updated, d)
			}
		} else {
			diff.Added = append(diff.Added, d)
		}
	}

	for _, a := range actual {
		if !seen[key(a)] {
			diff.Removed = append(diff.Removed, a)
		}
	}

	return diff
}
```

### Tests

`Record{ID, Value}` stands in for any reconciled resource; `byID` is the
`KeyFunc`, `sameValue` is the `EqualFunc`. The mixed case has `c` only in
desired (Added), `d` only in actual (Removed), and `b` in both with a
different `Value` (Updated) — all three outcomes from a single `Reconcile`
call.

Create `reconcile_test.go`:

```go
package reconcile

import (
	"slices"
	"testing"
)

type Record struct {
	ID    string
	Value int
}

func byID(r Record) string       { return r.ID }
func sameValue(d, a Record) bool { return d.Value == a.Value }

func TestReconcile(t *testing.T) {
	tests := []struct {
		name        string
		desired     []Record
		actual      []Record
		wantAdded   []string
		wantRemoved []string
		wantUpdated []string
	}{
		{
			name:        "identical states produce an empty diff",
			desired:     []Record{{"a", 1}, {"b", 2}},
			actual:      []Record{{"a", 1}, {"b", 2}},
			wantAdded:   nil,
			wantRemoved: nil,
			wantUpdated: nil,
		},
		{
			name:        "add, remove, and update detected in one pass",
			desired:     []Record{{"a", 1}, {"b", 2}, {"c", 3}},
			actual:      []Record{{"a", 1}, {"b", 99}, {"d", 4}},
			wantAdded:   []string{"c"},
			wantRemoved: []string{"d"},
			wantUpdated: []string{"b"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			diff := Reconcile(tc.desired, tc.actual, byID, sameValue)

			if got := ids(diff.Added); !slices.Equal(got, tc.wantAdded) {
				t.Errorf("Added = %v, want %v", got, tc.wantAdded)
			}
			if got := ids(diff.Removed); !slices.Equal(got, tc.wantRemoved) {
				t.Errorf("Removed = %v, want %v", got, tc.wantRemoved)
			}
			if got := ids(diff.Updated); !slices.Equal(got, tc.wantUpdated) {
				t.Errorf("Updated = %v, want %v", got, tc.wantUpdated)
			}
		})
	}
}

func ids(records []Record) []string {
	out := make([]string, len(records))
	for i, r := range records {
		out[i] = r.ID
	}
	return out
}
```

Run it: `go test -count=1 ./...`

## Review

`Reconcile` builds one lookup map from `actual` and then makes a single pass
over `desired`, checking off each key it sees; a second short pass over
`actual` catches whatever was never checked off as `Removed`. That two-pass,
one-map shape is the same one every desired-vs-actual reconciler in the wild
uses, whether the records are Kubernetes objects, DNS entries, or IAM
policies. Left out on purpose: nested-field diffing inside `Updated` (which
field changed, not just that something did) — that needs a
structure-specific comparator layered on top of this generic pass.

## Resources

- [Go Specification: Type parameter declarations](https://go.dev/ref/spec#Type_parameter_declarations)
- [slices.Equal](https://pkg.go.dev/slices#Equal)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [14-batch-migrator-progress-callback.md](14-batch-migrator-progress-callback.md) | Next: [16-batch-process-error-handler-callback.md](16-batch-process-error-handler-callback.md)
