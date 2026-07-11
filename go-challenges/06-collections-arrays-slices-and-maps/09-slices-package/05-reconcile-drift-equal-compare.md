# Exercise 5: Detect Config Drift Between Desired And Actual State (Equal / EqualFunc / Compare)

The core of a reconcile loop is a cheap, correct "does actual already match
desired?" check. When they match, the loop skips the write — an idempotency guard
that avoids churning the backend on every tick. This module builds that
comparison out of `slices.Equal` for comparable elements, `slices.EqualFunc` for
structs that must be compared on identity fields only (ignoring volatile ones like
timestamps), and `slices.Compare` for a deterministic ordering signal, including
the nil-versus-empty subtlety that trips reconcilers up.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
reconcile/                     module example.com/reconcile
  go.mod                       go 1.24
  reconcile.go                 type Rule; Reconcile (Equal/EqualFunc guard), Compare signal
  cmd/
    demo/
      main.go                  runnable demo: skip when equal, write when drifted
  reconcile_test.go            equal skips, drift writes, nil==empty, EqualFunc ignores timestamp, Compare -1/0/+1
```

- Files: `reconcile.go`, `cmd/demo/main.go`, `reconcile_test.go`.
- Implement: `Reconcile(desired, actual []Rule, apply func([]Rule)) bool` that compares on identity fields with `slices.EqualFunc` and calls `apply` (returning true) only when they differ; plus `Order(a, b []string) int` using `slices.Compare`.
- Test: equal slices skip; differing length or element triggers apply; nil vs empty treated equal by `Equal`; `EqualFunc` ignoring a timestamp field; `Compare` returning -1/0/+1 with a lexicographic tie example; assert the apply side-effect fires exactly when expected.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/reconcile/cmd/demo
cd ~/go-exercises/reconcile
go mod init example.com/reconcile
go mod edit -go=1.24
```

### Equal, EqualFunc, and the timestamp trap

`slices.Equal(a, b)` reports true when the two slices have the same length and
every element pair is `==`. It requires a comparable element type. For a slice of
plain strings or ints, that is the whole story — and it treats a nil slice and a
non-nil empty slice as equal, because both have length zero. That nil-versus-empty
detail is exactly the kind of thing a reconcile loop must pin: if desired comes
back nil (no rules) and actual comes back `[]Rule{}` (empty result set), `Equal`
correctly says "no drift" and the loop skips, which is what you want.

The trap is comparing structs that carry volatile fields. A `Rule` here has an
identity (`Name`, `Target`) plus a `SyncedAt` timestamp that the actual state
stamps on every fetch. Comparing full structs with `Equal` would see the
timestamps differ and declare drift on every tick, reconciling forever. The fix is
`slices.EqualFunc(desired, actual, eq)`, where `eq` compares only the identity
fields and ignores `SyncedAt`. `EqualFunc` still short-circuits on length, then
calls `eq` pairwise. Choosing which fields define "the same rule" is the design
decision; the test proves two rules with different timestamps but equal identity
compare equal.

`Reconcile` uses `EqualFunc` as the guard: if desired and actual match on
identity, it does nothing and returns false; otherwise it calls `apply(desired)`
and returns true. The `apply` callback is the write side-effect (in real code, the
call that pushes desired to the backend). The tests assert the callback fires
exactly when drift exists and never when it does not — the idempotency property.

`slices.Compare(a, b)` is the ordering counterpart: it returns -1, 0, or +1 by
comparing elements pairwise in order, and if one slice is a prefix of the other,
the shorter one sorts first. It is the tool for a deterministic diff signal or for
ordering two candidate states. The test pins a lexicographic tie: `["a","b"]`
versus `["a","c"]` returns -1 on the second element.

Create `reconcile.go`:

```go
package reconcile

import "slices"

// Rule is a desired-state entry. Name+Target are its identity; SyncedAt is
// volatile metadata the actual state stamps and must not drive reconciliation.
type Rule struct {
	Name     string
	Target   string
	SyncedAt int64
}

// sameIdentity compares two rules on identity only, ignoring SyncedAt.
func sameIdentity(a, b Rule) bool {
	return a.Name == b.Name && a.Target == b.Target
}

// Reconcile applies desired only when it differs from actual on identity,
// returning whether apply was called. Matching state is a no-op (idempotent).
func Reconcile(desired, actual []Rule, apply func([]Rule)) bool {
	if slices.EqualFunc(desired, actual, sameIdentity) {
		return false
	}
	apply(desired)
	return true
}

// Order returns a deterministic -1/0/+1 ordering of two string states, with a
// shorter slice sorting before a longer one that shares its prefix.
func Order(a, b []string) int {
	return slices.Compare(a, b)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/reconcile"
)

func main() {
	desired := []reconcile.Rule{
		{Name: "web", Target: "10.0.0.1"},
		{Name: "api", Target: "10.0.0.2"},
	}
	// Actual matches on identity but has fresh timestamps.
	actual := []reconcile.Rule{
		{Name: "web", Target: "10.0.0.1", SyncedAt: 1700},
		{Name: "api", Target: "10.0.0.2", SyncedAt: 1700},
	}

	wrote := reconcile.Reconcile(desired, actual, func([]reconcile.Rule) {
		fmt.Println("apply called")
	})
	fmt.Printf("in-sync tick wrote: %v\n", wrote)

	// Now actual drifts: api points elsewhere.
	actual[1].Target = "10.9.9.9"
	wrote = reconcile.Reconcile(desired, actual, func([]reconcile.Rule) {
		fmt.Println("apply called")
	})
	fmt.Printf("drifted tick wrote: %v\n", wrote)

	fmt.Printf("order([a b],[a c]) = %d\n", reconcile.Order([]string{"a", "b"}, []string{"a", "c"}))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
in-sync tick wrote: false
apply called
drifted tick wrote: true
order([a b],[a c]) = -1
```

The first tick sees matching identity despite differing timestamps and skips; the
second sees the drifted target and applies; `Compare` orders the two string
states as -1.

### Tests

`TestReconcile` is table-driven over equal (skip), length-drift (apply),
element-drift (apply), and timestamp-only difference (skip via `EqualFunc`), each
asserting the side-effect fired exactly as expected. `TestNilEqualsEmpty` pins
`slices.Equal(nil, []int{})` as true. `TestCompareSignal` pins -1/0/+1 including
the lexicographic tie.

Create `reconcile_test.go`:

```go
package reconcile

import (
	"slices"
	"testing"
)

func TestReconcile(t *testing.T) {
	t.Parallel()

	base := []Rule{{Name: "web", Target: "1"}, {Name: "api", Target: "2"}}

	cases := []struct {
		name      string
		desired   []Rule
		actual    []Rule
		wantWrote bool
	}{
		{
			name:      "equal identity skips",
			desired:   base,
			actual:    []Rule{{Name: "web", Target: "1", SyncedAt: 99}, {Name: "api", Target: "2", SyncedAt: 99}},
			wantWrote: false,
		},
		{
			name:      "length drift applies",
			desired:   base,
			actual:    []Rule{{Name: "web", Target: "1"}},
			wantWrote: true,
		},
		{
			name:      "element drift applies",
			desired:   base,
			actual:    []Rule{{Name: "web", Target: "1"}, {Name: "api", Target: "CHANGED"}},
			wantWrote: true,
		},
		{
			name:      "timestamp-only difference skips",
			desired:   base,
			actual:    []Rule{{Name: "web", Target: "1", SyncedAt: 1}, {Name: "api", Target: "2", SyncedAt: 2}},
			wantWrote: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			applied := false
			got := Reconcile(tc.desired, tc.actual, func([]Rule) { applied = true })
			if got != tc.wantWrote {
				t.Fatalf("Reconcile returned %v, want %v", got, tc.wantWrote)
			}
			if applied != tc.wantWrote {
				t.Fatalf("apply fired = %v, want %v", applied, tc.wantWrote)
			}
		})
	}
}

func TestNilEqualsEmpty(t *testing.T) {
	t.Parallel()

	if !slices.Equal([]int(nil), []int{}) {
		t.Fatal("Equal(nil, empty) = false, want true")
	}
	// Reconcile treats desired-nil / actual-empty as no drift: apply must not fire.
	wrote := Reconcile(nil, []Rule{}, func([]Rule) {
		t.Fatal("apply fired for nil-vs-empty; should be treated as equal")
	})
	if wrote {
		t.Fatal("Reconcile(nil, empty) reported drift, want no-op")
	}
}

func TestCompareSignal(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		a, b []string
		want int
	}{
		{"equal", []string{"a", "b"}, []string{"a", "b"}, 0},
		{"lexicographic less", []string{"a", "b"}, []string{"a", "c"}, -1},
		{"prefix sorts first", []string{"a"}, []string{"a", "b"}, -1},
		{"greater", []string{"b"}, []string{"a"}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Order(tc.a, tc.b); got != tc.want {
				t.Fatalf("Order(%v,%v) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}
```

## Review

The reconcile guard is correct when `apply` fires on every real drift and never on
a tick where only volatile metadata changed. `slices.EqualFunc` on identity fields
is what makes that distinction; comparing full structs with `Equal` would treat a
new `SyncedAt` as drift and reconcile forever. The nil-versus-empty case is a real
production edge: `Equal` treats them as equal, so a desired-nil / actual-empty tick
correctly skips. `Compare` gives a stable ordering signal with the shorter-prefix
rule. Run `go test -race`; each table case owns its `applied` flag, so the parallel
sub-tests do not share state.

## Resources

- [`slices.Equal`](https://pkg.go.dev/slices#Equal) and [`slices.EqualFunc`](https://pkg.go.dev/slices#EqualFunc) — length short-circuit and the nil==empty rule.
- [`slices.Compare`](https://pkg.go.dev/slices#Compare) — pairwise -1/0/+1 with the shorter-prefix ordering.
- [Go blog: the slices and maps packages](https://go.dev/blog/slices) — equality and comparison semantics.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-stable-multikey-pagination-sort.md](06-stable-multikey-pagination-sort.md)
