# Exercise 12: Reconciler Diffing: slices.Equal and maps.Equal Against reflect.DeepEqual

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A Kubernetes controller's reconcile loop runs the same comparison millions of
times a day across every cluster that has one: given the resource's desired
spec and the resource as it actually exists, decide whether anything needs
to change. Get that comparison wrong in the direction of "too sensitive" and
the controller never settles -- it issues a no-op update every single pass,
forever, burning API server quota and cluttering the audit log with changes
that changed nothing. That failure mode has a name in the controller-runtime
community, the reconcile hot loop, and it has one extremely common root
cause: comparing structs with `reflect.DeepEqual`.

`reflect.DeepEqual` is not wrong about content -- it walks both values field
by field and compares them, which is more than `==` can even attempt for a
struct holding a slice or a map. What it gets wrong is nil. `DeepEqual`
treats a nil slice and a non-nil empty slice as unequal, and the identical
rule holds for maps. A desired spec built in Go, `Spec{}`, has nil
collections by construction; an observed spec decoded from a real API,
which routinely normalizes an absent list to `[]` and an absent object to
`{}`, has non-nil empty ones. Nothing changed between "you never set a
label" and "the API confirms there are no labels" -- but `DeepEqual` says
they differ, and the reconciler updates a resource that needed no update.

This module builds the comparison as a package: a `Spec` type, a validating
`NewSpec` constructor, and a `NeedsUpdate` function built on `slices.Equal`
and `maps.Equal` -- the two standard-library comparisons that were written
specifically to treat nil and empty as the same thing they are: nothing.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

Nothing about this bug is specific to Kubernetes. Any diff-then-act loop --
a config synchronizer, a DNS record reconciler, a cache invalidator that
compares the last-seen value to the current one -- inherits the same trap
the moment it reaches for `reflect.DeepEqual` on a struct with a slice or
map field.

## What you'll build

```text
reconcile/                module example.com/reconcile
  go.mod                  go 1.24
  reconcile.go            Spec; NewSpec, NeedsUpdate; one sentinel error
  reconcile_test.go       NeedsUpdate table, the reflect.DeepEqual hot-loop contrast,
                          NewSpec validation and cloning, ExampleNeedsUpdate
```

- Files: `reconcile.go`, `reconcile_test.go`.
- Implement: `NewSpec(labels map[string]string, ports []int32) (Spec, error)` validating every port is in `1..65535` and returning `ErrInvalidPort` otherwise, holding independent clones of its inputs; `NeedsUpdate(want, got Spec) bool` built on `slices.Equal` and `maps.Equal`.
- Test: the `NeedsUpdate` table (identical specs, nil-vs-empty in both directions, a real port change, a real label change, a removed label, a reordered port list); the `reflect.DeepEqual`-based hot-loop contrast; `NeedsUpdate` exercised concurrently under `-race`; `NewSpec` clones its inputs, validates ports, and preserves a nil input as nil; an all-nil `Spec` compares equal to the Go zero value; and `ExampleNeedsUpdate` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/reconcile
cd ~/go-exercises/reconcile
go mod init example.com/reconcile
go mod edit -go=1.24
```

### Why DeepEqual sees a difference that isn't there

`reflect.DeepEqual` is a generic, structural comparison: for a slice, it
requires both to be nil or both to be non-nil, and only then compares
lengths and elements. That "both nil or both non-nil" clause is exactly the
trap. It is not a comparison of *content* at that point, it is a comparison
of a pointer's nilness, wrapped inside a function whose name promises
content equality:

```go
func deepEqualNeedsUpdate(want, got Spec) bool {
	return !reflect.DeepEqual(want, got)
}
```

Call this with `want := Spec{}` -- the shape a desired spec takes before any
label or port has been configured, all Go zero values -- and
`got := Spec{Labels: map[string]string{}, Ports: []int32{}}` -- the shape an
observed spec takes once it round-trips through an API that always emits
`[]` and `{}` for "nothing here," never `null`. Both specs describe the same
resource: no labels, no ports. `reflect.DeepEqual` reports them as
different, because one holds nil collections and the other holds non-nil
empty ones, and that single false positive is what puts a reconcile loop
into its hot state -- reissuing the same no-op patch every pass, because the
comparison it trusts can never report "nothing to do" for this pair.

`slices.Equal` and `maps.Equal` were built to answer a narrower, more useful
question: do these two collections have the same elements, regardless of
whether either happens to be nil. Both treat a nil collection and a non-nil
empty one as equal, which is exactly the semantics a reconciler needs --
"unset" and "explicitly empty" are the same fact from the controller's point
of view, because there is nothing on either side to act on:

```go
func NeedsUpdate(want, got Spec) bool {
	if !slices.Equal(want.Ports, got.Ports) {
		return true
	}
	if !maps.Equal(want.Labels, got.Labels) {
		return true
	}
	return false
}
```

Create `reconcile.go`:

```go
// Package reconcile compares the desired and observed state of a
// Kubernetes-controller-style resource by content, not by identity, so a
// desired Spec built from Go zero values and an observed Spec decoded from
// an API that normalizes absent lists to [] compare equal when nothing
// actually changed.
package reconcile

import (
	"errors"
	"fmt"
	"maps"
	"slices"
)

// ErrInvalidPort is returned by NewSpec when a port is outside the valid
// TCP/UDP range 1-65535.
var ErrInvalidPort = errors.New("reconcile: port out of range 1-65535")

// Spec is the subset of a resource's spec this package reconciles: its
// labels and the ports it exposes.
//
// A Spec built through NewSpec owns independent copies of Labels and Ports;
// mutating the slice or map passed to NewSpec afterward does not change the
// Spec, and mutating a Spec's fields does not reach back into the caller's
// original values.
type Spec struct {
	Labels map[string]string
	Ports  []int32
}

// NewSpec validates ports and returns a Spec holding independent copies of
// labels and ports. It returns ErrInvalidPort if any port is outside
// 1-65535.
func NewSpec(labels map[string]string, ports []int32) (Spec, error) {
	for _, p := range ports {
		if p < 1 || p > 65535 {
			return Spec{}, fmt.Errorf("%w: got %d", ErrInvalidPort, p)
		}
	}
	return Spec{Labels: maps.Clone(labels), Ports: slices.Clone(ports)}, nil
}

// NeedsUpdate reports whether got's state differs from want in any field
// this package tracks: its labels or its ports.
//
// The comparison is by content, using slices.Equal and maps.Equal, not by
// identity. A nil collection and a non-nil empty collection compare equal:
// nothing meaningfully changed going from unset to explicitly empty, which
// is exactly the shape a desired Spec built with Go zero values and an
// observed Spec decoded from an API that normalizes an absent field to []
// or {} will otherwise disagree about forever.
//
// NeedsUpdate holds no state and is safe for concurrent use by multiple
// goroutines.
func NeedsUpdate(want, got Spec) bool {
	if !slices.Equal(want.Ports, got.Ports) {
		return true
	}
	if !maps.Equal(want.Labels, got.Labels) {
		return true
	}
	return false
}
```

### Using it

Build the desired `Spec` with `NewSpec`, which validates ports up front so a
malformed desired state never reaches the comparison at all, and build the
observed `Spec` however the cluster's real state arrives -- typically a
plain struct literal populated from a decoded API response, not necessarily
through `NewSpec`, since the observed side is already known-good data. Feed
both to `NeedsUpdate` on every reconcile pass; it holds no state of its own,
so a single reconciler goroutine, or many running concurrently across
different resources, can call it without coordination.

The module has no `main.go`, because a reconcile comparison is a library,
not a tool. Its executable demonstration is `ExampleNeedsUpdate`: `go test`
runs it and compares its standard output against the `// Output:` comment,
so the usage shown below cannot drift away from the code.

```go
func ExampleNeedsUpdate() {
	desired, err := NewSpec(nil, nil) // nothing configured yet: zero-value collections
	if err != nil {
		panic(err)
	}

	// observed, as decoded from an API that normalizes an absent list to []
	observed := Spec{Labels: map[string]string{}, Ports: []int32{}}
	fmt.Println("nil desired vs API-normalized empty observed:", NeedsUpdate(desired, observed))

	observed.Ports = []int32{8080}
	fmt.Println("after a real port change:", NeedsUpdate(desired, observed))

	// Output:
	// nil desired vs API-normalized empty observed: false
	// after a real port change: true
}
```

### Tests

`TestNeedsUpdate` is the table, and its most important rows are the two
nil-vs-empty pairs -- nil `want` against empty `got`, and the reverse --
both asserting `false`, next to real changes in ports and labels that must
report `true`, including a reordered port list, since `slices.Equal` is
order-sensitive and a controller that reorders `containerPort` entries is
making a real change to apply.

`TestDeepEqualHotLoopBug` is the heart of the module. `deepEqualNeedsUpdate`
is unexported and unreachable from the package API; it exists so the test
can state the defect concretely -- for the identical nil-vs-empty pair,
`deepEqualNeedsUpdate` reports a change and `NeedsUpdate` does not. If a
future edit swaps `NeedsUpdate`'s implementation back to `reflect.DeepEqual`,
this test fails here instead of in a controller's reconcile metrics.

`TestNeedsUpdateSafeForConcurrentUse` exercises `NeedsUpdate` from twenty
goroutines at once, matching the concurrency contract its doc comment
declares; since the function holds no state, a failure here would surface
as a data race report rather than a wrong answer.

`TestNewSpecClonesAndValidates` mutates the caller's slice and map after
construction and checks the `Spec` did not move, pinning the aliasing
contract in the doc comment. `TestNewSpecRejectsInvalidPort` sweeps zero,
negative, and above-range ports. `TestNewSpecEmptyInputIsValid` confirms a
`Spec` built from `nil, nil` compares equal to the Go zero value `Spec{}`,
and `TestNewSpecPreservesNilFromNilInput` confirms why: `maps.Clone` and
`slices.Clone` pass a nil input through as nil rather than silently
upgrading it to a non-nil empty value, so `NewSpec(nil, nil)` and a literal
`Spec{}` are the same value, not merely equal under `NeedsUpdate`.

Notice what none of these tests need: a mock API server, a fake clientset,
or any of the machinery a real controller-runtime test suite pulls in. The
whole defect and its fix live in how two structs made of a slice and a map
compare, which is exactly why it is worth isolating here -- the reconcile
hot loop this module is named after is usually diagnosed weeks into a
production incident, from a metrics dashboard showing an update count that
never settles, long after the one-line comparison bug that caused it has
been buried under everything else the controller does.

Create `reconcile_test.go`:

```go
package reconcile

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
)

// deepEqualNeedsUpdate is the reconciler as it is usually written the first
// time: reflect.DeepEqual looks correct because it does compare contents,
// not just identity, and it compiles for any pair of structs without asking
// the author to think about slices.Equal or maps.Equal. It is never
// exported and never reachable from the package API; it exists so the tests
// can pin the defect it ships. reflect.DeepEqual treats a nil slice/map and
// a non-nil empty one as UNEQUAL, so it reports a change whenever the
// desired Spec is built from Go zero values and the observed Spec is
// decoded from an API that normalizes an absent list to [] -- a reconcile
// loop built on this helper reissues a no-op update every single pass.
func deepEqualNeedsUpdate(want, got Spec) bool {
	return !reflect.DeepEqual(want, got)
}

func TestNeedsUpdate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		want       Spec
		got        Spec
		wantChange bool
	}{
		{
			name:       "identical specs",
			want:       Spec{Labels: map[string]string{"env": "prod"}, Ports: []int32{80, 443}},
			got:        Spec{Labels: map[string]string{"env": "prod"}, Ports: []int32{80, 443}},
			wantChange: false,
		},
		{
			name:       "nil want vs API-normalized empty got, nothing changed",
			want:       Spec{},
			got:        Spec{Labels: map[string]string{}, Ports: []int32{}},
			wantChange: false,
		},
		{
			name:       "empty want vs nil got, nothing changed",
			want:       Spec{Labels: map[string]string{}, Ports: []int32{}},
			got:        Spec{},
			wantChange: false,
		},
		{
			name:       "port added",
			want:       Spec{Ports: []int32{80}},
			got:        Spec{Ports: []int32{80, 443}},
			wantChange: true,
		},
		{
			name:       "label value changed",
			want:       Spec{Labels: map[string]string{"env": "prod"}},
			got:        Spec{Labels: map[string]string{"env": "staging"}},
			wantChange: true,
		},
		{
			name:       "label removed",
			want:       Spec{Labels: map[string]string{"env": "prod", "tier": "web"}},
			got:        Spec{Labels: map[string]string{"env": "prod"}},
			wantChange: true,
		},
		{
			name:       "port order changed",
			want:       Spec{Ports: []int32{80, 443}},
			got:        Spec{Ports: []int32{443, 80}},
			wantChange: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := NeedsUpdate(tc.want, tc.got); got != tc.wantChange {
				t.Fatalf("NeedsUpdate(%+v, %+v) = %v, want %v", tc.want, tc.got, got, tc.wantChange)
			}
		})
	}
}

// TestDeepEqualHotLoopBug is the heart of the module: it pins the exact
// spurious mismatch reflect.DeepEqual produces for the nil-vs-empty case,
// contrasted against NeedsUpdate agreeing that nothing changed for the
// identical inputs.
func TestDeepEqualHotLoopBug(t *testing.T) {
	t.Parallel()

	want := Spec{}                                             // desired state built from Go zero values: nil Labels, nil Ports
	got := Spec{Labels: map[string]string{}, Ports: []int32{}} // observed, decoded from an API that normalizes absent to empty

	if !deepEqualNeedsUpdate(want, got) {
		t.Fatal("deepEqualNeedsUpdate reported no change; the defect it demonstrates did not reproduce")
	}
	if NeedsUpdate(want, got) {
		t.Fatal("NeedsUpdate reported a spurious change for nil-vs-empty; the fix did not hold")
	}
}

// TestNeedsUpdateSafeForConcurrentUse calls NeedsUpdate from many goroutines
// at once, matching the concurrency contract in its doc comment. A failure
// here would show up as a data race report under -race, not as a test
// assertion, since NeedsUpdate holds no state of its own to corrupt.
func TestNeedsUpdateSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	want := Spec{Labels: map[string]string{"env": "prod"}, Ports: []int32{80, 443}}
	got := Spec{Labels: map[string]string{"env": "prod"}, Ports: []int32{80, 443}}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if NeedsUpdate(want, got) {
				t.Error("NeedsUpdate reported a change for identical specs")
			}
		}()
	}
	wg.Wait()
}

func TestNewSpecClonesAndValidates(t *testing.T) {
	t.Parallel()

	labels := map[string]string{"env": "prod"}
	ports := []int32{80, 443}
	spec, err := NewSpec(labels, ports)
	if err != nil {
		t.Fatalf("NewSpec: %v", err)
	}

	labels["env"] = "mutated"
	ports[0] = 9999

	if spec.Labels["env"] != "prod" {
		t.Fatalf("Spec.Labels aliases the caller's map: got %q", spec.Labels["env"])
	}
	if spec.Ports[0] != 80 {
		t.Fatalf("Spec.Ports aliases the caller's slice: got %d", spec.Ports[0])
	}
}

func TestNewSpecRejectsInvalidPort(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		ports []int32
	}{
		{name: "zero", ports: []int32{0}},
		{name: "negative", ports: []int32{-1}},
		{name: "above range", ports: []int32{65536}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if _, err := NewSpec(nil, tc.ports); !errors.Is(err, ErrInvalidPort) {
				t.Fatalf("NewSpec(ports=%v) error = %v, want ErrInvalidPort", tc.ports, err)
			}
		})
	}
}

func TestNewSpecEmptyInputIsValid(t *testing.T) {
	t.Parallel()

	spec, err := NewSpec(nil, nil)
	if err != nil {
		t.Fatalf("NewSpec(nil, nil): %v", err)
	}
	if NeedsUpdate(spec, Spec{}) {
		t.Fatal("a Spec built from nil inputs must compare equal to the Go zero value Spec")
	}
}

// TestNewSpecPreservesNilFromNilInput confirms maps.Clone and slices.Clone
// pass nil through as nil rather than upgrading it to a non-nil empty
// value: NewSpec(nil, nil) must produce the same nil-collection Spec a
// literal Spec{} does, so the two remain interchangeable for NeedsUpdate.
func TestNewSpecPreservesNilFromNilInput(t *testing.T) {
	t.Parallel()

	spec, err := NewSpec(nil, nil)
	if err != nil {
		t.Fatalf("NewSpec(nil, nil): %v", err)
	}
	if spec.Labels != nil {
		t.Fatalf("Spec.Labels = %v, want nil", spec.Labels)
	}
	if spec.Ports != nil {
		t.Fatalf("Spec.Ports = %v, want nil", spec.Ports)
	}
}

// ExampleNeedsUpdate is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExampleNeedsUpdate() {
	desired, err := NewSpec(nil, nil) // nothing configured yet: zero-value collections
	if err != nil {
		panic(err)
	}

	// observed, as decoded from an API that normalizes an absent list to []
	observed := Spec{Labels: map[string]string{}, Ports: []int32{}}
	fmt.Println("nil desired vs API-normalized empty observed:", NeedsUpdate(desired, observed))

	observed.Ports = []int32{8080}
	fmt.Println("after a real port change:", NeedsUpdate(desired, observed))

	// Output:
	// nil desired vs API-normalized empty observed: false
	// after a real port change: true
}
```

## Review

`NeedsUpdate` is correct when a nil-versus-empty pair with no real
difference compares equal in both directions -- the table's two middle
cases and `TestDeepEqualHotLoopBug` all pin that. The mechanism is choosing
`slices.Equal` and `maps.Equal` over `reflect.DeepEqual`: both were written
to compare content, and both explicitly define a nil collection and a
non-nil empty collection as equal, which is the one guarantee
`reflect.DeepEqual` refuses to make. The consequence of getting this wrong
is not a crash or a wrong answer on a real change -- it is a controller that
never stops reconciling, reissuing a no-op patch on every pass because its
own equality check can never agree that nothing changed. `NewSpec` keeps
the desired side honest by validating every port against the real TCP/UDP
range and by cloning its inputs so the `Spec` it returns cannot be mutated
out from under the reconciler by whatever built the labels map or the port
slice. Run `go test -count=1 -race ./...`.

## Resources

- [`slices.Equal`](https://pkg.go.dev/slices#Equal) — content comparison that treats nil and non-nil-empty as equal.
- [`maps.Equal`](https://pkg.go.dev/maps#Equal) — the map equivalent, same nil-vs-empty rule.
- [`reflect.DeepEqual`](https://pkg.go.dev/reflect#DeepEqual) — the documented rule that a nil and an empty slice or map are *not* deeply equal.
- [Kubernetes controller-runtime: avoiding reconcile hot loops](https://sdk.operatorframework.io/docs/best-practices/reconcile-best-practices/) — the operational cost of a comparison that never agrees "nothing changed."

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-cluster-registry-map-clone-snapshot.md](11-cluster-registry-map-clone-snapshot.md) | Next: [13-compacted-log-tombstone-vs-empty.md](13-compacted-log-tombstone-vs-empty.md)
