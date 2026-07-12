# Exercise 14: Desired-vs-Live Resource Reconciler with maps.EqualFunc

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A controller's reconcile loop -- the pattern every Kubernetes controller
runs, and any operator managing infrastructure as declared state -- ticks
forever: read the desired configuration, read the live one, and if they
differ, act to close the gap. The comparison at the center of that loop is
almost always two `map[string]Spec` values, one resource name to one
configuration struct, and it runs thousands of times a minute across a
resource set that can itself number in the thousands. `maps.Equal` from
Go's standard `maps` package looks like the obvious tool for comparing two
maps -- until `Spec` grows a `[]string` field, such as a list of labels, and
`maps.Equal` refuses to compile: it requires the map's value type to be
comparable, and a struct containing a slice is not.

The reconciler has two ways forward. `maps.EqualFunc` takes a comparator
function instead of relying on `==`, so a field-wise comparator that uses
`slices.Equal` for the label list closes the compile error and stays fast.
Or the loop reaches for `reflect.DeepEqual`, which happily accepts anything
and sidesteps the whole question -- and that is the trap. `reflect.DeepEqual`
compiles, runs, and gives the right bool, at a real cost in a hot per-tick
loop: it walks both values through reflection instead of comparing typed
fields directly. Worse than the cost is what it cannot tell you. Applied to
the whole map at once, `reflect.DeepEqual(desired, live)` answers exactly
one question -- did *anything* change -- and nothing about *what*. A
controller that only knows "something is wrong somewhere" cannot act; it
still has to walk every resource by hand to find the one that drifted.

This module builds `Reconciler`, a package that compares a desired resource
map against a live one and reports exactly which resources were added,
removed, or changed, and which field changed on each -- built on
`maps.EqualFunc` for a fast no-drift path and a field-wise comparator for
the slow path that has to say something specific.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
reconciler/               module example.com/reconciler
  go.mod                  go 1.24
  reconciler.go           Spec, FieldDiff, Report, Reconciler; New, Diff, NoDrift
  reconciler_test.go      no-drift table, added/removed/changed table, label-order
                          edge case, the whole-map-reflect contrast, concurrency,
                          ExampleReconciler_Diff
```

- Files: `reconciler.go`, `reconciler_test.go`.
- Implement: `New(capacityHint int) (*Reconciler, error)` rejecting a negative hint with `ErrNegativeCapacityHint`; `(*Reconciler).Diff(desired, live map[string]Spec) Report` built on `maps.EqualFunc` for a fast no-drift path and per-resource field comparison otherwise; `Report.NoDrift() bool`.
- Test: identical maps and both nil produce `NoDrift`; a combined added/removed/changed scenario pins every field of `Report`; reordered (but set-equal) labels are asserted changed, documenting the order-sensitive comparison; the whole-map `reflect.DeepEqual` contrast proving it can only report a bool while `Diff` names the resource and field; a concurrency test calling a shared `Reconciler` from many goroutines; and `ExampleReconciler_Diff` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/08-map-internals-and-iteration-order/14-resource-state-reconciler-diff
cd go-solutions/06-collections-arrays-slices-and-maps/08-map-internals-and-iteration-order/14-resource-state-reconciler-diff
go mod edit -go=1.24
```

### maps.Equal needs comparable values; maps.EqualFunc needs a comparator

`maps.Equal(m1, m2)` is exactly `m1 == m2` extended to maps: same length,
same keys, and `m1[k] == m2[k]` for every key, using the map's value type's
own `==`. That requires the value type to satisfy `comparable`, which the
compiler checks at the call site. A `Spec` carrying `Labels []string` fails
that check immediately -- `[]string` has no `==`, so `Spec` does not
satisfy `comparable`, so `maps.Equal[map[string]Spec]` does not compile at
all:

```go
maps.Equal(desired, live) // compile error: Spec does not satisfy comparable
```

`maps.EqualFunc` generalizes the same algorithm -- same length, same keys,
every key's pair of values compared -- but takes the comparison as a
parameter instead of assuming `==`:

```go
func EqualFunc[K comparable, V1, V2 any](m1 map[K]V1, m2 map[K]V2, eq func(V1, V2) bool) bool
```

Any `func(Spec, Spec) bool` fits, so the field-wise comparator this module
writes -- comparing `Image` and `Replicas` with `==` and `Labels` with
`slices.Equal` -- is exactly what's needed, and it is what makes the
`maps.Equal` idea usable again for a value type that merely contains one
slice among otherwise-comparable fields. The reflection alternative avoids
writing that comparator at all:

```go
changed := !reflect.DeepEqual(desired, live) // compiles for anything, tells you nothing specific
```

It compiles for any two values of any type, which is exactly why it is
tempting to reach for the moment a struct fails `comparable`. But
`reflect.DeepEqual` over a whole map answers one question, "are these
equal," with one bool for the entire map -- it has no concept of "which
key." A reconcile loop that gets `false` back from that call still has to
iterate every resource by hand to find out what changed, which is the exact
work `Diff` below does once and reports structured. `reflect.DeepEqual` is
also strictly more expensive per comparison: it boxes both arguments into
`reflect.Value`s and walks them field by field through the reflection
machinery, instead of comparing two typed `string`s and calling
`slices.Equal` directly.

Create `reconciler.go`:

```go
// Package reconciler compares a desired resource map against the live one
// on every tick of a controller reconcile loop, the shape a Kubernetes-
// style controller runs thousands of times a minute, and reports exactly
// which resources drifted and which fields drifted on each one.
//
// It exists to get one detail right: Spec carries a []string field, and
// slices are not comparable, so maps.Equal (which requires comparable
// values) cannot be used to compare two map[string]Spec. maps.EqualFunc
// takes a field-wise comparator instead, and that comparator -- not
// reflect.DeepEqual -- is what a hot per-tick loop over thousands of
// resources should use, both for speed and because reflect.DeepEqual over
// a whole map can only say "something differs," never which resource or
// which field. See the package tests for a demonstration.
package reconciler

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"
)

// ErrNegativeCapacityHint is returned by New when the capacity hint is
// negative.
var ErrNegativeCapacityHint = errors.New("reconciler: capacity hint must not be negative")

// Spec is the desired or live configuration of one resource.
type Spec struct {
	Image    string
	Replicas int
	Labels   []string
}

// FieldDiff names the fields that differ for one resource between the
// desired and live Spec.
type FieldDiff struct {
	Resource string
	Fields   []string
}

// Report is the result of comparing a desired resource map against a live
// one. Added holds resource names present only in desired, Removed holds
// names present only in live, Changed holds names present in both whose
// Spec differs, and Diffs holds the field-level detail for each entry in
// Changed. All four are sorted by resource name.
type Report struct {
	Added   []string
	Removed []string
	Changed []string
	Diffs   []FieldDiff
}

// NoDrift reports whether the compared maps had no additions, removals, or
// changes at all.
func (r Report) NoDrift() bool {
	return len(r.Added) == 0 && len(r.Removed) == 0 && len(r.Changed) == 0
}

// Reconciler computes drift reports between a desired and a live resource
// map.
//
// Reconciler is immutable after construction and safe for concurrent use:
// Diff reads its two map arguments and builds a fresh Report, never
// touching any shared state.
type Reconciler struct {
	capacityHint int
}

// New returns a Reconciler. capacityHint presizes the working slices Diff
// builds internally for the number of resources expected to differ on a
// typical tick, avoiding repeated slice growth over a reconcile loop that
// runs thousands of times a minute. It returns ErrNegativeCapacityHint if
// capacityHint is negative. A capacityHint of 0 is valid and simply omits
// the size hint.
func New(capacityHint int) (*Reconciler, error) {
	if capacityHint < 0 {
		return nil, fmt.Errorf("%w: got %d", ErrNegativeCapacityHint, capacityHint)
	}
	return &Reconciler{capacityHint: capacityHint}, nil
}

// Diff compares desired against live and returns a Report describing every
// addition, removal, and field-level change.
//
// The returned Report does not alias desired or live: every string and
// slice it holds is freshly built, so both input maps may be freely
// mutated after Diff returns. Label comparison is order-sensitive (via
// slices.Equal), so two Specs whose Labels hold the same elements in a
// different order are reported as changed; callers whose label source
// does not guarantee a stable order should sort Labels before calling
// Diff.
func (rc *Reconciler) Diff(desired, live map[string]Spec) Report {
	// The fast path: maps.EqualFunc walks both maps once using specEqual
	// and returns as soon as it finds no difference in length or content,
	// without allocating a single diff slice. Most reconcile ticks find no
	// drift at all, so this path is the common case, not the exception.
	if maps.EqualFunc(desired, live, specEqual) {
		return Report{}
	}

	added := make([]string, 0, rc.capacityHint)
	for name := range desired {
		if _, ok := live[name]; !ok {
			added = append(added, name)
		}
	}
	removed := make([]string, 0, rc.capacityHint)
	for name := range live {
		if _, ok := desired[name]; !ok {
			removed = append(removed, name)
		}
	}
	changed := make([]string, 0, rc.capacityHint)
	diffs := make([]FieldDiff, 0, rc.capacityHint)
	for name, d := range desired {
		l, ok := live[name]
		if !ok {
			continue
		}
		if fields := diffFields(d, l); len(fields) > 0 {
			changed = append(changed, name)
			diffs = append(diffs, FieldDiff{Resource: name, Fields: fields})
		}
	}

	slices.Sort(added)
	slices.Sort(removed)
	slices.Sort(changed)
	slices.SortFunc(diffs, func(a, b FieldDiff) int { return cmp.Compare(a.Resource, b.Resource) })

	return Report{Added: added, Removed: removed, Changed: changed, Diffs: diffs}
}

// specEqual is the field-wise comparator maps.EqualFunc needs because Spec
// contains a slice field and is therefore not comparable with ==.
func specEqual(a, b Spec) bool {
	return a.Image == b.Image && a.Replicas == b.Replicas && slices.Equal(a.Labels, b.Labels)
}

// diffFields returns the names of every field that differs between d and l.
func diffFields(d, l Spec) []string {
	var fields []string
	if d.Image != l.Image {
		fields = append(fields, "Image")
	}
	if d.Replicas != l.Replicas {
		fields = append(fields, "Replicas")
	}
	if !slices.Equal(d.Labels, l.Labels) {
		fields = append(fields, "Labels")
	}
	return fields
}
```

### Using it

Construct one `Reconciler` per controller with `New`, sized to roughly how
many resources you expect to drift on a typical tick (0 is a perfectly
valid default), and call `Diff(desired, live)` every time you have a fresh
pair of resource maps -- there is no state to carry between calls, so the
same `*Reconciler` can be shared across every worker goroutine a controller
runs, which is what the type's concurrency contract promises. The fast path
inside `Diff` means a tick with no drift -- the common case in a healthy
system -- costs one pass over both maps and zero allocations for the diff
slices; only a tick that actually differs pays for building `Report`.

`Report.NoDrift()` is the one-line check a reconcile loop uses to decide
whether there is anything to act on at all; `Changed` and `Diffs` are what
it acts on when there is. Every slice `Diff` returns is freshly built, never
a view into `desired` or `live`, so the caller may reuse or mutate either
input map immediately after the call returns.

`ExampleReconciler_Diff` in the `_test.go` is the runnable demonstration of
the whole API; `go test` executes it and compares its output against the
`// Output:` comment:

```go
rc, err := New(4)
if err != nil {
	panic(err)
}
report := rc.Diff(desired, live)

fmt.Println("added:", report.Added)
fmt.Println("removed:", report.Removed)
fmt.Println("changed:", report.Changed)
for _, d := range report.Diffs {
	fmt.Printf("%s: %v\n", d.Resource, d.Fields)
}
fmt.Println("no drift:", report.NoDrift())
```

### Tests

`TestDiffNoDrift` covers the fast path from two directions: an unchanged
map compared against a clone of itself, and two `nil` maps, both expected
to report `NoDrift`. `TestDiffAddedRemovedChanged` is the combined table: a
resource only in `desired`, one only in `live`, one whose `Image` differs,
and one that is identical in both -- checked against every field of
`Report` in one pass. `TestLabelOrderIsSignificant` pins the documented
edge case explicitly: the same label set in a different order is reported
as changed, because `slices.Equal` is order-sensitive by design.

`TestNaiveWholeMapCompareLosesAttribution` is the heart of the module.
`changedNaive` is unexported and unreachable from the package API; the test
confirms it correctly reports `true` for two maps that differ in exactly
one resource's `Image` field, and then shows that is *all* it can report --
there is no second call that narrows which of the two resources changed,
because `reflect.DeepEqual` was already handed the whole map. `Diff` on the
same input is asserted to name the resource and the field exactly. The same
test measures allocations per comparison for `specEqual` against
`reflect.DeepEqual` on one pair of `Spec` values, asserting the field-wise
comparator allocates less -- a property, not an exact count, since the
runtime's own reflection cost is not a documented contract. This test
deliberately skips `t.Parallel`, because `testing.AllocsPerRun` panics if
run from a parallel test. `TestReconcilerIsSafeForConcurrentUse` drives many
goroutines calling `Diff` on a shared `*Reconciler` with the same inputs and
checks every result agrees.

Create `reconciler_test.go`:

```go
package reconciler

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"
)

// changedNaive is the comparison a reconcile loop reaches for once someone
// notices Spec contains a slice and maps.Equal won't compile: reflect
// .DeepEqual applied to the whole map at once. It compiles, it is correct
// about *whether* anything differs, and it is the only thing it can say --
// it cannot name a resource or a field. It is never exported and never
// reachable from Reconciler; it exists so the tests can pin what using it
// costs.
func changedNaive(desired, live map[string]Spec) bool {
	return !reflect.DeepEqual(desired, live)
}

func TestNewRejectsNegativeCapacityHint(t *testing.T) {
	t.Parallel()

	if _, err := New(-1); !errors.Is(err, ErrNegativeCapacityHint) {
		t.Fatalf("New(-1) error = %v, want ErrNegativeCapacityHint", err)
	}
}

func TestDiffNoDrift(t *testing.T) {
	t.Parallel()

	specs := map[string]Spec{
		"web": {Image: "app:v2", Replicas: 3, Labels: []string{"env=prod"}},
	}
	rc, err := New(0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	report := rc.Diff(specs, mapsClone(specs))
	if !report.NoDrift() {
		t.Fatalf("Diff on identical maps = %+v, want NoDrift", report)
	}
	if report := rc.Diff(nil, nil); !report.NoDrift() {
		t.Fatalf("Diff(nil, nil) = %+v, want NoDrift", report)
	}
}

func TestDiffAddedRemovedChanged(t *testing.T) {
	t.Parallel()

	desired := map[string]Spec{
		"web":    {Image: "app:v2", Replicas: 3, Labels: []string{"env=prod", "tier=web"}},
		"cache":  {Image: "redis:7", Replicas: 1, Labels: []string{"env=prod", "tier=cache"}},
		"worker": {Image: "app:v2", Replicas: 2, Labels: []string{"env=prod", "tier=worker"}},
	}
	live := map[string]Spec{
		"web":    {Image: "app:v1", Replicas: 3, Labels: []string{"env=prod", "tier=web"}},
		"cache":  {Image: "redis:7", Replicas: 1, Labels: []string{"env=prod", "tier=cache"}},
		"legacy": {Image: "app:v0", Replicas: 1, Labels: nil},
	}
	rc, err := New(0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	report := rc.Diff(desired, live)

	if !slices.Equal(report.Added, []string{"worker"}) {
		t.Errorf("Added = %v, want [worker]", report.Added)
	}
	if !slices.Equal(report.Removed, []string{"legacy"}) {
		t.Errorf("Removed = %v, want [legacy]", report.Removed)
	}
	if !slices.Equal(report.Changed, []string{"web"}) {
		t.Errorf("Changed = %v, want [web]", report.Changed)
	}
	if len(report.Diffs) != 1 || report.Diffs[0].Resource != "web" || !slices.Equal(report.Diffs[0].Fields, []string{"Image"}) {
		t.Errorf("Diffs = %+v, want one entry for web with Fields=[Image]", report.Diffs)
	}
}

// TestLabelOrderIsSignificant pins the documented, order-sensitive label
// comparison: the same labels in a different order are reported as a
// change, because Diff uses slices.Equal, not a set comparison.
func TestLabelOrderIsSignificant(t *testing.T) {
	t.Parallel()

	desired := map[string]Spec{"web": {Image: "app:v2", Labels: []string{"a", "b"}}}
	live := map[string]Spec{"web": {Image: "app:v2", Labels: []string{"b", "a"}}}

	rc, err := New(0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	report := rc.Diff(desired, live)
	if !slices.Equal(report.Changed, []string{"web"}) {
		t.Fatalf("Changed = %v, want [web] (reordered labels must count as changed)", report.Changed)
	}
	if len(report.Diffs) != 1 || !slices.Equal(report.Diffs[0].Fields, []string{"Labels"}) {
		t.Fatalf("Diffs = %+v, want Fields=[Labels]", report.Diffs)
	}
}

// TestNaiveWholeMapCompareLosesAttribution is the heart of the module.
// changedNaive is correct about whether the two maps differ, but it can
// only return a bool -- it has no way to say which resource, let alone
// which field, drifted. Diff, built on the field-wise comparator
// maps.EqualFunc requires, reports both.
//
// This test does not call t.Parallel: testing.AllocsPerRun panics when run
// from a parallel test, because a concurrent goroutine allocating in the
// background would corrupt its measurement.
func TestNaiveWholeMapCompareLosesAttribution(t *testing.T) {
	desired := map[string]Spec{
		"web":   {Image: "app:v2", Replicas: 3, Labels: []string{"env=prod", "tier=web"}},
		"cache": {Image: "redis:7", Replicas: 1, Labels: []string{"env=prod", "tier=cache"}},
	}
	live := map[string]Spec{
		"web":   {Image: "app:v1", Replicas: 3, Labels: []string{"env=prod", "tier=web"}},
		"cache": {Image: "redis:7", Replicas: 1, Labels: []string{"env=prod", "tier=cache"}},
	}

	if !changedNaive(desired, live) {
		t.Fatal("changedNaive reports no difference; want true")
	}
	// changedNaive's entire vocabulary is that one bool: there is no
	// second call that would tell it which of the two resources changed,
	// because reflect.DeepEqual was already given the whole map.

	rc, err := New(0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	report := rc.Diff(desired, live)
	if !slices.Equal(report.Changed, []string{"web"}) {
		t.Fatalf("Diff Changed = %v, want [web]: the reconciler names exactly the drifted resource", report.Changed)
	}
	if len(report.Diffs) != 1 || !slices.Equal(report.Diffs[0].Fields, []string{"Image"}) {
		t.Fatalf("Diff Diffs = %+v, want one entry naming the Image field", report.Diffs)
	}

	// The field-wise comparator also costs less than boxing both structs
	// through reflection on every comparison, which matters run thousands
	// of times a minute across a large resource set.
	d, l := desired["web"], live["web"]
	fieldwise := testing.AllocsPerRun(200, func() { _ = specEqual(d, l) })
	reflected := testing.AllocsPerRun(200, func() { _ = reflect.DeepEqual(d, l) })
	if !(fieldwise < reflected) {
		t.Fatalf("allocations: fieldwise = %v, reflect.DeepEqual = %v; want fieldwise < reflect.DeepEqual", fieldwise, reflected)
	}
}

func TestReconcilerIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	desired := map[string]Spec{"web": {Image: "app:v2", Replicas: 3, Labels: []string{"a", "b"}}}
	live := map[string]Spec{"web": {Image: "app:v1", Replicas: 3, Labels: []string{"a", "b"}}}

	rc, err := New(4)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			report := rc.Diff(desired, live)
			if !slices.Equal(report.Changed, []string{"web"}) {
				t.Errorf("Changed = %v, want [web]", report.Changed)
			}
		}()
	}
	wg.Wait()
}

// mapsClone returns a shallow copy of m with its Labels slices copied too,
// so mutating the clone's Spec values never touches m.
func mapsClone(m map[string]Spec) map[string]Spec {
	out := make(map[string]Spec, len(m))
	for k, v := range m {
		v.Labels = slices.Clone(v.Labels)
		out[k] = v
	}
	return out
}

// ExampleReconciler_Diff demonstrates a full reconcile tick: one resource
// added, one removed, one with a changed field, and one identical.
func ExampleReconciler_Diff() {
	desired := map[string]Spec{
		"web":    {Image: "app:v2", Replicas: 3, Labels: []string{"env=prod", "tier=web"}},
		"cache":  {Image: "redis:7", Replicas: 1, Labels: []string{"env=prod", "tier=cache"}},
		"worker": {Image: "app:v2", Replicas: 2, Labels: []string{"env=prod", "tier=worker"}},
	}
	live := map[string]Spec{
		"web":    {Image: "app:v1", Replicas: 3, Labels: []string{"env=prod", "tier=web"}},
		"cache":  {Image: "redis:7", Replicas: 1, Labels: []string{"env=prod", "tier=cache"}},
		"legacy": {Image: "app:v0", Replicas: 1, Labels: nil},
	}

	rc, err := New(4)
	if err != nil {
		panic(err)
	}
	report := rc.Diff(desired, live)

	fmt.Println("added:", report.Added)
	fmt.Println("removed:", report.Removed)
	fmt.Println("changed:", report.Changed)
	for _, d := range report.Diffs {
		fmt.Printf("%s: %v\n", d.Resource, d.Fields)
	}
	fmt.Println("no drift:", report.NoDrift())

	// Output:
	// added: [worker]
	// removed: [legacy]
	// changed: [web]
	// web: [Image]
	// no drift: false
}
```

## Review

The reconciler is correct when `Report` names every resource that was
added, removed, or changed, and every changed field on each -- never merely
a bool. `New` rejects a negative capacity hint with `ErrNegativeCapacityHint`,
checkable with `errors.Is`. `Diff` leans on `maps.EqualFunc` for the common
no-drift tick, so a healthy reconcile loop pays for one pass over both maps
and no diff-slice allocations, and falls through to the field-wise pass
only when something actually differs. The trap this module isolates is
`reflect.DeepEqual` applied to the whole map: it is a legal, compiling
answer to "does `Spec` need a comparator," but it collapses the entire
comparison into a single bool and costs more per comparison than the
field-wise alternative, both pinned directly in
`TestNaiveWholeMapCompareLosesAttribution`. Label comparison is
order-sensitive by design, documented on `Diff` and pinned by
`TestLabelOrderIsSignificant`. `Reconciler` is immutable after construction
and therefore safe to share across every goroutine a controller runs, and
`ExampleReconciler_Diff` is the executable documentation `go test`
verifies. Run `go test -count=1 -race ./...`.

## Resources

- [`maps.EqualFunc`](https://pkg.go.dev/maps#EqualFunc) — the comparator-based map equality this module is built on.
- [`maps.Equal`](https://pkg.go.dev/maps#Equal) — why it cannot be used once a map's value type contains a slice.
- [`slices.Equal`](https://pkg.go.dev/slices#Equal) — the order-sensitive element comparison used for `Labels`.
- [`reflect.DeepEqual`](https://pkg.go.dev/reflect#DeepEqual) — what it costs and what it cannot tell you, contrasted directly in the tests.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-idempotency-key-dedup-filter.md](13-idempotency-key-dedup-filter.md) | Next: [15-weighted-round-robin-pool.md](15-weighted-round-robin-pool.md)
