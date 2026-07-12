# Exercise 4: Reconciler Diff — Desired vs Actual State

A control loop reconciles: it holds a desired state, observes the actual state,
and computes what to add, update, and delete to close the gap. Over a service
registry or routing table, both states are maps keyed by name. This module builds
that diff, and shows the two flavors of map equality — `maps.Equal` for a fully
comparable value type, and `maps.EqualFunc` when a volatile field must be ignored.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. It gates alone.

## What you'll build

```text
reconcile/                  independent module: example.com/reconcile
  go.mod                    go 1.26
  reconcile.go              Endpoint, Diff, InSync, InSyncIgnoringLastSeen
  cmd/
    demo/
      main.go               desired vs actual, printed add/update/delete sets
  reconcile_test.go         add/update/delete/no-change cases + EqualFunc volatility
```

Files: `reconcile.go`, `cmd/demo/main.go`, `reconcile_test.go`.
Implement: `Diff(desired, actual) (toAdd, toUpdate, toDelete []string)`, `InSync` via `maps.Equal`, `InSyncIgnoringLastSeen` via `maps.EqualFunc`.
Test: add-only, delete-only, update-only, no-change; `maps.Equal` short-circuits identical maps; `EqualFunc` ignores the volatile `LastSeen` field; output slices are sorted.
Verify: `go test -count=1 -race ./...`

## Why the diff is three sorted slices, and why equality has two flavors

`Diff` walks both maps once. A key present in `desired` but not `actual` goes to
`toAdd`. A key present in both, but with a differing value, goes to `toUpdate`. A
key present in `actual` but not `desired` goes to `toDelete`. Everything else is
already in sync and contributes nothing. Crucially the three result slices are
sorted — `slices.Sorted(maps.Keys(...))` on the working sets — because a reconciler
that emits its actions in a random order produces non-deterministic logs, flaky
tests, and unstable plan diffs. A sorted plan is a reproducible plan.

`InSync` is a fast path: before computing a full diff, ask whether the two maps
are already equal. `maps.Equal(desired, actual)` answers in one call and
short-circuits — if the maps have different lengths it returns immediately without
comparing values. It requires the value type `Endpoint` to be `comparable`, which
it is here because `Endpoint` is a struct of comparable fields. This is a
compile-time property: had `Endpoint` contained a slice, the call would not
compile, and no amount of runtime `recover` would help. That is the point the
concepts file makes about `maps.Equal` — the constraint is enforced by the
compiler, not by a panic.

But real endpoints carry volatile fields. A `LastSeen` timestamp updated by every
health check makes two *semantically identical* endpoints compare unequal under
`maps.Equal`, so a naive reconciler would churn forever, "updating" endpoints that
differ only in when they were last polled. `maps.EqualFunc(desired, actual, eq)`
solves this: you supply an equality function that compares the fields that matter
(address, weight) and ignores the ones that do not (`LastSeen`). Two states that
differ only in the volatile field then report in-sync, and the loop settles.
`InSyncIgnoringLastSeen` wraps exactly that.

Create `reconcile.go`:

```go
package reconcile

import (
	"maps"
	"slices"
	"time"
)

// Endpoint is a routing-table entry. It is fully comparable, so maps of Endpoint
// work with maps.Equal.
type Endpoint struct {
	Address  string
	Weight   int
	LastSeen time.Time
}

// Diff reports the keys to add, update, and delete to bring actual into line with
// desired. All three result slices are sorted for deterministic output.
func Diff(desired, actual map[string]Endpoint) (toAdd, toUpdate, toDelete []string) {
	for k, want := range desired {
		got, ok := actual[k]
		switch {
		case !ok:
			toAdd = append(toAdd, k)
		case got != want:
			toUpdate = append(toUpdate, k)
		}
	}
	for k := range actual {
		if _, ok := desired[k]; !ok {
			toDelete = append(toDelete, k)
		}
	}
	slices.Sort(toAdd)
	slices.Sort(toUpdate)
	slices.Sort(toDelete)
	return toAdd, toUpdate, toDelete
}

// InSync reports whether the two states are exactly equal, comparing every field
// including LastSeen. It uses maps.Equal, which requires Endpoint to be comparable.
func InSync(desired, actual map[string]Endpoint) bool {
	return maps.Equal(desired, actual)
}

// InSyncIgnoringLastSeen reports whether the two states are equal ignoring the
// volatile LastSeen field, so a health-check timestamp does not trigger churn.
func InSyncIgnoringLastSeen(desired, actual map[string]Endpoint) bool {
	return maps.EqualFunc(desired, actual, func(a, b Endpoint) bool {
		return a.Address == b.Address && a.Weight == b.Weight
	})
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/reconcile"
)

func main() {
	desired := map[string]reconcile.Endpoint{
		"api":  {Address: "10.0.0.1", Weight: 10},
		"auth": {Address: "10.0.0.2", Weight: 5},
		"web":  {Address: "10.0.0.3", Weight: 8},
	}
	actual := map[string]reconcile.Endpoint{
		"api":   {Address: "10.0.0.1", Weight: 10},
		"auth":  {Address: "10.0.0.9", Weight: 5}, // address drifted
		"stale": {Address: "10.0.0.8", Weight: 1}, // no longer desired
	}

	add, update, del := reconcile.Diff(desired, actual)
	fmt.Println("add:   ", add)
	fmt.Println("update:", update)
	fmt.Println("delete:", del)

	// Same desired/actual but actual's LastSeen differs: not in sync strictly,
	// in sync when ignoring the volatile field.
	a := map[string]reconcile.Endpoint{"api": {Address: "10.0.0.1", Weight: 10}}
	b := map[string]reconcile.Endpoint{"api": {Address: "10.0.0.1", Weight: 10, LastSeen: time.Unix(1, 0)}}
	fmt.Println("strict in sync: ", reconcile.InSync(a, b))
	fmt.Println("ignoring seen:  ", reconcile.InSyncIgnoringLastSeen(a, b))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
add:    [web]
update: [auth]
delete: [stale]
strict in sync:  false
ignoring seen:   true
```

### Tests

The tests cover each diff category in isolation plus the no-change case, assert the
output slices are sorted, and pin the equality contrast: `InSync` is false when
only `LastSeen` differs, while `InSyncIgnoringLastSeen` is true. A no-change case
also confirms `Diff` returns three empty slices.

Create `reconcile_test.go`:

```go
package reconcile

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

func ep(addr string, w int) Endpoint { return Endpoint{Address: addr, Weight: w} }

func TestDiff(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		desired, actual  map[string]Endpoint
		add, update, del []string
	}{
		"add only": {
			desired: map[string]Endpoint{"a": ep("1", 1), "b": ep("2", 1)},
			actual:  map[string]Endpoint{"a": ep("1", 1)},
			add:     []string{"b"},
		},
		"delete only": {
			desired: map[string]Endpoint{"a": ep("1", 1)},
			actual:  map[string]Endpoint{"a": ep("1", 1), "z": ep("9", 1)},
			del:     []string{"z"},
		},
		"update only": {
			desired: map[string]Endpoint{"a": ep("1", 2)},
			actual:  map[string]Endpoint{"a": ep("1", 1)},
			update:  []string{"a"},
		},
		"no change": {
			desired: map[string]Endpoint{"a": ep("1", 1)},
			actual:  map[string]Endpoint{"a": ep("1", 1)},
		},
		"mixed sorted": {
			desired: map[string]Endpoint{"c": ep("3", 1), "a": ep("1", 9), "b": ep("2", 1)},
			actual:  map[string]Endpoint{"a": ep("1", 1), "d": ep("4", 1)},
			add:     []string{"b", "c"},
			update:  []string{"a"},
			del:     []string{"d"},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			add, update, del := Diff(tc.desired, tc.actual)
			if !slices.Equal(add, tc.add) {
				t.Errorf("add = %v, want %v", add, tc.add)
			}
			if !slices.Equal(update, tc.update) {
				t.Errorf("update = %v, want %v", update, tc.update)
			}
			if !slices.Equal(del, tc.del) {
				t.Errorf("delete = %v, want %v", del, tc.del)
			}
		})
	}
}

func TestInSyncStrictVsIgnoringLastSeen(t *testing.T) {
	t.Parallel()

	a := map[string]Endpoint{"api": {Address: "10.0.0.1", Weight: 10}}
	b := map[string]Endpoint{"api": {Address: "10.0.0.1", Weight: 10, LastSeen: time.Unix(100, 0)}}

	if InSync(a, b) {
		t.Error("InSync should be false when LastSeen differs")
	}
	if !InSyncIgnoringLastSeen(a, b) {
		t.Error("InSyncIgnoringLastSeen should be true when only LastSeen differs")
	}
}

func TestInSyncShortCircuitsOnLength(t *testing.T) {
	t.Parallel()

	a := map[string]Endpoint{"x": ep("1", 1)}
	b := map[string]Endpoint{"x": ep("1", 1), "y": ep("2", 1)}
	if InSync(a, b) {
		t.Error("InSync should be false for maps of different length")
	}
}

func ExampleDiff() {
	desired := map[string]Endpoint{"a": ep("1", 1), "b": ep("2", 1)}
	actual := map[string]Endpoint{"a": ep("1", 1)}
	add, update, del := Diff(desired, actual)
	fmt.Println(add, update, del)
	// Output: [b] [] []
}
```

## Review

The reconciler is correct when its plan is complete and deterministic: every key
lands in exactly one of add/update/delete or in neither (already in sync), and the
three slices come out sorted so the plan is reproducible. The equality contrast is
the senior lesson: `maps.Equal` compares everything and requires a comparable value
type at compile time, while `maps.EqualFunc` lets you define semantic equality that
ignores volatile fields — the difference between a control loop that settles and
one that churns on every health check. Do not add a `recover` around `maps.Equal`
hoping to catch a non-comparable value; if the value type were non-comparable the
code would not compile at all. Run `go test -race`.

## Resources

- [maps package](https://pkg.go.dev/maps) — `Equal`, `EqualFunc`, `Keys`.
- [slices package](https://pkg.go.dev/slices) — `Sort`, `Equal`.
- [Kubernetes: the reconciliation loop](https://kubernetes.io/docs/concepts/architecture/controller/) — desired vs actual state as a control loop.

---

Back to [03-layered-config-merge.md](03-layered-config-merge.md) | Next: [05-immutable-snapshot-registry.md](05-immutable-snapshot-registry.md)
