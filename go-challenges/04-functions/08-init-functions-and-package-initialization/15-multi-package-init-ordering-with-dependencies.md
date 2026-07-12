# Exercise 15: Multi-Package Init Order — A Real Cross-Package Dependency

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

`init()` ordering is usually explained with a diagram and left at that. This
exercise makes the guarantee load-bearing: a `units` package builds a table of
length conversions at `init()`, and a `convrules` package — which imports
`units` — derives a *further* table from it at its own `init()`, assuming
`units` is completely ready by the time it runs. The Go spec promises exactly
that, and this exercise proves it by recording, in order, which package
actually finished initializing first.

## What you'll build

```text
multiinit/                 independent module: example.com/multiinit
  go.mod                    module example.com/multiinit
  orderlog/
    orderlog.go              Order []string; Append(name) — a shared init-order log
  units/
    units.go                 BaseUnits map, built by init()
  convrules/
    convrules.go             checkUnitsReady, buildTable, init() depending on units
    convrules_test.go         ordering proof + derived-table checks + broken-input test
  cmd/
    demo/
      main.go                prints the recorded init order and two conversions
```

Files: `orderlog/orderlog.go`, `units/units.go`, `convrules/convrules.go`, `cmd/demo/main.go`, `convrules_test.go`.
Implement: `units.BaseUnits` populated by `init()`; `convrules.Table` derived from it by `init()`, guarded by an extracted `checkUnitsReady` helper; both packages append their name to `orderlog.Order` at the end of their own `init()`.
Test: `orderlog.Order` is `["units", "convrules"]` after the program starts; `convrules.Table` holds correct derived factors; `checkUnitsReady` rejects a table missing a required unit.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/08-init-functions-and-package-initialization/15-multi-package-init-ordering-with-dependencies/orderlog
mkdir -p go-solutions/04-functions/08-init-functions-and-package-initialization/15-multi-package-init-ordering-with-dependencies/units
mkdir -p go-solutions/04-functions/08-init-functions-and-package-initialization/15-multi-package-init-ordering-with-dependencies/convrules
mkdir -p go-solutions/04-functions/08-init-functions-and-package-initialization/15-multi-package-init-ordering-with-dependencies/cmd/demo
cd go-solutions/04-functions/08-init-functions-and-package-initialization/15-multi-package-init-ordering-with-dependencies
go mod edit -go=1.24
```

### Why this ordering is guaranteed, not hoped for

The Go spec's package initialization rule is precise: "a package with no
imports is initialized by assigning initial values to all its package-level
variables followed by calling all init functions... If a package has multiple
imports, ... each imported package will be fully initialized before
initialization of the importing package begins." Because `convrules` imports
`units`, the runtime *cannot* start running `convrules`'s package-level
variable initializers or `init()` functions until every one of `units`'s
package-level variables is set and every `units` `init()` has returned. This
is not a scheduling convenience — it is part of the language's memory model,
enforced identically by every conforming Go implementation.

That guarantee is what makes it safe for `convrules.buildTable` to read
`units.BaseUnits` unconditionally from its own `init()`. There is no race to
defend against here, unlike goroutines started explicitly — the ordering is
established once, at program startup, before `main` (or a test) ever runs.
`checkUnitsReady` exists anyway, but for a different reason: it documents the
assumption in code and gives a test something concrete to attack with a
deliberately incomplete table, the same way `redact.go`'s `validatePatterns`
did in an earlier exercise. In production, `checkUnitsReady` can never
actually fail — the spec forbids it.

`orderlog` is the part that turns "the spec guarantees this" into "and here
is proof, printed and tested": every package's `init()` appends its own name
right before returning, so the final `orderlog.Order` slice is a direct,
observable trace of what ran when.

Create `orderlog/orderlog.go`:

```go
// orderlog/orderlog.go
// Package orderlog records, in the order they actually ran, which package
// inits executed. Every package in this exercise appends its own name at the
// end of its init(), so the resulting slice is direct, observable proof of
// initialization order.
package orderlog

// Order accumulates one entry per package init, in execution order.
var Order []string

// Append records name as having finished initializing.
func Append(name string) {
	Order = append(Order, name)
}
```

Create `units/units.go`:

```go
// units/units.go
// Package units defines the base set of length units this program knows
// about, expressed as "how many meters is one of these".
package units

import "example.com/multiinit/orderlog"

// BaseUnits maps a unit name to how many meters one unit equals. It is built
// by init() rather than a plain var literal, so this package has real
// initialization work another package can legitimately depend on finishing
// first.
var BaseUnits map[string]float64

func init() {
	BaseUnits = computeBaseUnits()
	orderlog.Append("units")
}

func computeBaseUnits() map[string]float64 {
	return map[string]float64{
		"meter":      1,
		"kilometer":  1000,
		"centimeter": 0.01,
		"mile":       1609.34,
	}
}
```

Create `convrules/convrules.go`:

```go
// convrules/convrules.go
// Package convrules derives a table of pairwise unit conversion factors from
// the units package. Its init() depends on units.BaseUnits already being
// fully populated — a dependency the Go spec guarantees is satisfied, because
// convrules imports units.
package convrules

import (
	"fmt"

	"example.com/multiinit/orderlog"
	"example.com/multiinit/units"
)

// requiredUnits lists every unit convrules needs units to have already
// defined by the time this package's own init runs.
var requiredUnits = []string{"meter", "kilometer", "centimeter", "mile"}

// Table maps "X_to_Y" to the multiplicative factor converting X into Y.
var Table map[string]float64

func init() {
	if err := checkUnitsReady(requiredUnits, units.BaseUnits); err != nil {
		panic(fmt.Errorf("convrules: %w", err))
	}
	Table = buildTable(requiredUnits, units.BaseUnits)
	orderlog.Append("convrules")
}

// checkUnitsReady verifies every unit in required is present in base. It is
// extracted from init purely so a test can drive it directly against a
// deliberately incomplete table: because convrules imports units, the Go
// spec guarantees units' init has already completed by the time this
// function runs for real, so in production this check can never fail — it
// exists to document and prove that guarantee, not to defend against it.
func checkUnitsReady(required []string, base map[string]float64) error {
	for _, u := range required {
		if _, ok := base[u]; !ok {
			return fmt.Errorf("units package not initialized: missing %q", u)
		}
	}
	return nil
}

// buildTable computes every from->to conversion factor for the units in
// required, using base as the source of truth for each unit's size in
// meters.
func buildTable(required []string, base map[string]float64) map[string]float64 {
	t := make(map[string]float64, len(required)*len(required))
	for _, from := range required {
		for _, to := range required {
			if from == to {
				continue
			}
			t[fmt.Sprintf("%s_to_%s", from, to)] = base[from] / base[to]
		}
	}
	return t
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/multiinit/convrules"
	"example.com/multiinit/orderlog"
)

func main() {
	fmt.Println("init order:", orderlog.Order)
	fmt.Printf("1 kilometer = %.2f meters\n", convrules.Table["kilometer_to_meter"])
	fmt.Printf("1 mile = %.5f kilometers\n", convrules.Table["mile_to_kilometer"])
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
init order: [units convrules]
1 kilometer = 1000.00 meters
1 mile = 1.60934 kilometers
```

### Tests

Create `convrules/convrules_test.go`:

```go
// convrules/convrules_test.go
package convrules

import (
	"reflect"
	"testing"

	"example.com/multiinit/orderlog"
	"example.com/multiinit/units"
)

// TestUnitsInitializedBeforeConvrules proves the import-graph ordering
// guarantee in practice: because this package imports units, units' init
// must have completed before this package's init ran, so "units" appears in
// orderlog.Order before "convrules".
func TestUnitsInitializedBeforeConvrules(t *testing.T) {
	want := []string{"units", "convrules"}
	if !reflect.DeepEqual(orderlog.Order, want) {
		t.Fatalf("orderlog.Order = %v, want %v", orderlog.Order, want)
	}
}

func TestTableDerivedCorrectly(t *testing.T) {
	tests := []struct {
		key  string
		want float64
	}{
		{"kilometer_to_meter", 1000},
		{"meter_to_kilometer", 0.001},
		{"centimeter_to_meter", 0.01},
		{"mile_to_kilometer", 1.60934},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			got, ok := Table[tt.key]
			if !ok {
				t.Fatalf("Table[%q] missing", tt.key)
			}
			if got != tt.want {
				t.Errorf("Table[%q] = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}

func TestCheckUnitsReadyRejectsMissingUnit(t *testing.T) {
	err := checkUnitsReady([]string{"meter", "furlong"}, units.BaseUnits)
	if err == nil {
		t.Fatal("expected error for missing unit furlong, got nil")
	}
}
```

## Review

`TestUnitsInitializedBeforeConvrules` is the whole exercise: it does not test
a race that might resolve either way, it tests a fact the Go spec pins down
completely — `orderlog.Order` can only ever be `["units", "convrules"]`,
never the reverse, no matter how many times you run it. That is the
difference between this and goroutine ordering elsewhere in the curriculum:
here there is nothing to synchronize, because the language runtime already
did it during program startup, strictly following the import graph.

The trap to watch for is the opposite dependency: if `units` ever imported
`convrules`, you would have an import cycle and the build would fail outright
— Go rejects cyclic imports precisely because it cannot compute a valid
initialization order for them. `checkUnitsReady` is a teaching aid, not a
runtime safeguard; keep it, because the day someone considers introducing a
manual "initialize both, just in case" step is the day this test's real value
becomes obvious.

## Resources

- [Go spec — Package initialization](https://go.dev/ref/spec#Package_initialization) — the exact ordering guarantee this exercise proves.
- [Go spec — Order of evaluation](https://go.dev/ref/spec#Order_of_evaluation) — how package-level variable initializers are sequenced before `init()`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [14-regexp-table-cross-validation-at-init.md](14-regexp-table-cross-validation-at-init.md) | Next: [16-config-environment-source-cascade-validation.md](16-config-environment-source-cascade-validation.md)
