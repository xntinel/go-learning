# Exercise 5: Deterministic Config-Drift Diff (Added / Removed / Changed Keys)

Before a deploy, or inside a reconciler loop, you compare the config you *want* against
the config that is *live* and log exactly what differs. This module builds that drift
detector over two `map[string]string`s, producing sorted lists of added, removed, and
changed keys — with a `maps.Equal` fast path for the common "no drift" case.

This module is fully self-contained: its own `go mod init`, all code inline, its own
demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
drift/                     independent module: example.com/drift
  go.mod                   go 1.26
  drift.go                 Diff, type Drift, type Change; Report
  cmd/
    demo/
      main.go              diffs desired vs live, prints the report
  drift_test.go            additions, removals, changes, no-drift, golden report
```

- Files: `drift.go`, `cmd/demo/main.go`, `drift_test.go`.
- Implement: `Diff(desired, live map[string]string) Drift` with sorted `Added`,
  `Removed`, `Changed` (old/new), plus `Drift.Empty()` and `Drift.Report()`.
- Test: pure additions, pure removals, value changes, mixed; identical maps report no
  drift (`maps.Equal`); ordering stable across runs; nil/empty inputs; golden report.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/drift/cmd/demo
cd ~/go-exercises/drift
go mod init example.com/drift
```

### Computing the diff over two key spaces

Drift is a set operation on two key spaces plus a value comparison on the overlap. A
key in `desired` but not in `live` is **added** (we want it, it is not there yet); a key
in `live` but not in `desired` is **removed** (it should go); a key in both whose values
differ is **changed**. The fast path first: if `maps.Equal(desired, live)` the configs
are identical, so `Diff` returns an empty `Drift` immediately without allocating any
slices — the common reconciler tick where nothing drifted costs one comparison.

For the general case, the code walks `desired` once (classifying each key as added or,
if present in both with a different value, changed) and walks `live` once (classifying
keys absent from `desired` as removed). Every classification appends to an unsorted
slice, and the last step sorts each slice: `slices.Sort` for the plain string lists
(`Added`, `Removed`), and because `Changed` is a `[]Change` (a slice of structs) we sort
it by its `Key` field with `slices.SortFunc(changed, func(a, b Change) int {
return cmp.Compare(a.Key, b.Key) })`. Sorting is mandatory: without it the report lines
would reorder every run (they come from ranging maps) and any golden-file or
log-diffing tooling would flap.

`Report` renders the drift as sorted, prefixed lines (`+ key = value` for added, `- key`
for removed, `~ key: old -> new` for changed), which is exactly what you want in a
pre-deploy log.

Create `drift.go`:

```go
package drift

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Change is one key whose value differs between desired and live.
type Change struct {
	Key string
	Old string // value in live
	New string // value in desired
}

// Drift is the difference between a desired config and the live one. Each slice
// is sorted, so the result is deterministic.
type Drift struct {
	Added   []string // in desired, not in live
	Removed []string // in live, not in desired
	Changed []Change // in both, different value
}

// Empty reports whether there is no drift at all.
func (d Drift) Empty() bool {
	return len(d.Added) == 0 && len(d.Removed) == 0 && len(d.Changed) == 0
}

// Diff computes the drift needed to turn live into desired.
func Diff(desired, live map[string]string) Drift {
	if maps.Equal(desired, live) { // fast path: no drift, no allocation
		return Drift{}
	}

	var d Drift
	for k, want := range desired {
		if have, ok := live[k]; !ok {
			d.Added = append(d.Added, k)
		} else if have != want {
			d.Changed = append(d.Changed, Change{Key: k, Old: have, New: want})
		}
	}
	for k := range live {
		if _, ok := desired[k]; !ok {
			d.Removed = append(d.Removed, k)
		}
	}

	slices.Sort(d.Added)
	slices.Sort(d.Removed)
	slices.SortFunc(d.Changed, func(a, b Change) int { return cmp.Compare(a.Key, b.Key) })
	return d
}

// Report renders the drift as sorted, prefixed lines. Empty drift renders "".
func (d Drift) Report() string {
	var b strings.Builder
	for _, k := range d.Added {
		fmt.Fprintf(&b, "+ %s\n", k)
	}
	for _, k := range d.Removed {
		fmt.Fprintf(&b, "- %s\n", k)
	}
	for _, c := range d.Changed {
		fmt.Fprintf(&b, "~ %s: %q -> %q\n", c.Key, c.Old, c.New)
	}
	return b.String()
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/drift"
)

func main() {
	desired := map[string]string{"replicas": "3", "image": "app:v2", "region": "us-east"}
	live := map[string]string{"replicas": "3", "image": "app:v1", "debug": "true"}

	d := drift.Diff(desired, live)
	if d.Empty() {
		fmt.Println("no drift")
		return
	}
	fmt.Print(d.Report())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
+ region
- debug
~ image: "app:v1" -> "app:v2"
```

### Tests

Each drift category gets a test; `maps.Equal` no-drift is verified via `Empty`;
nil/empty inputs are handled; a golden report pins the exact rendered string; and a
determinism test runs `Diff` twice on the same inputs and compares the reports.

Create `drift_test.go`:

```go
package drift

import (
	"testing"
)

func TestPureAdditions(t *testing.T) {
	t.Parallel()

	d := Diff(map[string]string{"a": "1", "b": "2"}, map[string]string{"a": "1"})
	if got, want := d.Added, []string{"b"}; !equalStrings(got, want) {
		t.Fatalf("Added = %v, want %v", got, want)
	}
	if len(d.Removed) != 0 || len(d.Changed) != 0 {
		t.Fatalf("unexpected removed/changed: %+v", d)
	}
}

func TestPureRemovals(t *testing.T) {
	t.Parallel()

	d := Diff(map[string]string{"a": "1"}, map[string]string{"a": "1", "b": "2"})
	if got, want := d.Removed, []string{"b"}; !equalStrings(got, want) {
		t.Fatalf("Removed = %v, want %v", got, want)
	}
}

func TestValueChanges(t *testing.T) {
	t.Parallel()

	d := Diff(map[string]string{"a": "2"}, map[string]string{"a": "1"})
	if len(d.Changed) != 1 || d.Changed[0] != (Change{Key: "a", Old: "1", New: "2"}) {
		t.Fatalf("Changed = %+v, want one {a 1 2}", d.Changed)
	}
}

func TestNoDrift(t *testing.T) {
	t.Parallel()

	same := map[string]string{"a": "1", "b": "2"}
	d := Diff(same, map[string]string{"a": "1", "b": "2"})
	if !d.Empty() {
		t.Fatalf("expected no drift, got %+v", d)
	}
	if d.Report() != "" {
		t.Fatalf("empty drift Report = %q, want empty", d.Report())
	}
}

func TestNilAndEmptyInputs(t *testing.T) {
	t.Parallel()

	// nil desired, non-empty live -> everything removed.
	d := Diff(nil, map[string]string{"a": "1"})
	if got, want := d.Removed, []string{"a"}; !equalStrings(got, want) {
		t.Fatalf("Removed = %v, want %v", got, want)
	}
	// both nil -> no drift, no panic.
	if !Diff(nil, nil).Empty() {
		t.Fatal("Diff(nil,nil) should be empty")
	}
}

func TestMixedGoldenReport(t *testing.T) {
	t.Parallel()

	desired := map[string]string{"replicas": "3", "image": "app:v2", "region": "us-east"}
	live := map[string]string{"replicas": "3", "image": "app:v1", "debug": "true"}

	want := "+ region\n- debug\n~ image: \"app:v1\" -> \"app:v2\"\n"
	if got := Diff(desired, live).Report(); got != want {
		t.Fatalf("Report mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestReportIsDeterministic(t *testing.T) {
	t.Parallel()

	desired := map[string]string{"a": "1", "b": "9", "c": "3", "d": "4"}
	live := map[string]string{"b": "2", "c": "3", "e": "5", "f": "6"}

	first := Diff(desired, live).Report()
	second := Diff(desired, live).Report()
	if first != second {
		t.Fatalf("report not deterministic:\n%q\n%q", first, second)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
```

## Review

The diff is correct when it partitions the union of the two key spaces into exactly
added, removed, and changed, and the same inputs always produce the same report. The
`maps.Equal` fast path is both an optimization and a correctness anchor: "no drift" is
defined as map equality, not as three empty slices you have to trust. Every output slice
is sorted before it leaves `Diff`, which is why the golden and determinism tests hold —
remove any `slices.Sort` call and the corresponding report lines start reordering per
run. The `Change` struct carries both old and new values so a reconciler log says what
it is about to do, not merely that something differs. Run `go test -race`.

## Resources

- [`maps.Equal`](https://pkg.go.dev/maps#Equal) — the no-drift fast path.
- [`slices.Sort`](https://pkg.go.dev/slices#Sort) and [`slices.SortFunc`](https://pkg.go.dev/slices#SortFunc) — deterministic ordering of the report.
- [`cmp.Compare`](https://pkg.go.dev/cmp#Compare) — the key comparator for `Changed`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-prealloc-index-build.md](06-prealloc-index-build.md)
