# Exercise 10: Derived Lookup Tables and Package-Level Initialization Order

**Nivel: Intermedio** — validacion rapida (un test corto).

A byte-size formatter needs a name-to-exponent lookup derived from its base
unit list. This exercise declares the derived map *before* the list it depends
on, in source, to prove Go still initializes the list first because
initialization follows dependency order, not source order.

## What you'll build

```text
humansize/                 independent module: example.com/humansize
  go.mod                    module example.com/humansize
  humansize.go               units list, derived unitIndex, ExponentOf, Format
  humansize_test.go          dependency-order proof + Format table test
```

Files: `humansize.go`, `humansize_test.go`.
Implement: a package-level `units` slice and a derived `unitIndex` map declared *before* `units` in source, plus `Format` and `ExponentOf`.
Test: `unitIndex` was built from the fully populated `units` slice despite the declaration order; `Format` renders known byte counts correctly.

Set up the module:

```bash
mkdir -p ~/go-exercises/humansize
cd ~/go-exercises/humansize
go mod init example.com/humansize
go mod edit -go=1.24
```

### Why the declaration order does not matter here

Go initializes package-level variables in dependency order: if `unitIndex`
reads `units` to build itself, `units` is guaranteed to be fully initialized
first, regardless of which `var` line appears earlier in the file. Relying on
this rule — instead of shuffling declarations to "look correct" — is what
lets a derived lookup table sit anywhere convenient in a file.

Create `humansize.go`:

```go
// Package humansize formats byte counts using the largest 1024-based unit
// that keeps the value readable.
package humansize

import "fmt"

// unitIndex maps a unit name to its power-of-1024 exponent, derived from
// units. It is declared BEFORE units in source, but Go initializes
// package-level variables in dependency order, not source order: since
// unitIndex depends on units, units is guaranteed to be initialized first.
var unitIndex = buildIndex(units)

// units is the base ordered list of size units, smallest first.
var units = []string{"B", "KB", "MB", "GB", "TB"}

// buildIndex derives a name-to-exponent map from an ordered unit list.
func buildIndex(u []string) map[string]int {
	idx := make(map[string]int, len(u))
	for i, name := range u {
		idx[name] = i
	}
	return idx
}

// ExponentOf returns the power-of-1024 exponent for a unit name (0 for "B",
// 1 for "KB", ...) and whether the unit is known.
func ExponentOf(unit string) (int, bool) {
	e, ok := unitIndex[unit]
	return e, ok
}

// Format renders n bytes using the largest unit that keeps the scaled value
// under 1024, with one decimal place.
func Format(n int64) string {
	f := float64(n)
	idx := 0
	for f >= 1024 && idx < len(units)-1 {
		f /= 1024
		idx++
	}
	return fmt.Sprintf("%.1f%s", f, units[idx])
}
```

Create `humansize_test.go`:

```go
package humansize

import "testing"

// TestUnitIndexMatchesUnits proves unitIndex was built from the fully
// initialized units slice, even though unitIndex is declared first in
// source. If dependency order were not honored, unitIndex would have been
// built from a nil or empty slice and this would fail.
func TestUnitIndexMatchesUnits(t *testing.T) {
	if got, want := len(unitIndex), len(units); got != want {
		t.Fatalf("len(unitIndex) = %d, want %d (built from units before units was populated?)", got, want)
	}
	for i, name := range units {
		e, ok := ExponentOf(name)
		if !ok {
			t.Fatalf("ExponentOf(%q) missing from unitIndex", name)
		}
		if e != i {
			t.Fatalf("ExponentOf(%q) = %d, want %d", name, e, i)
		}
	}
}

func TestExponentOfUnknownUnit(t *testing.T) {
	if _, ok := ExponentOf("PB"); ok {
		t.Fatalf("ExponentOf(%q) reported ok=true for an undeclared unit", "PB")
	}
}

func TestFormat(t *testing.T) {
	tests := []struct {
		name string
		n    int64
		want string
	}{
		{"zero", 0, "0.0B"},
		{"under_1kb", 500, "500.0B"},
		{"exactly_1kb", 1024, "1.0KB"},
		{"one_and_half_kb", 1536, "1.5KB"},
		{"exactly_1mb", 1024 * 1024, "1.0MB"},
		{"exactly_1gb", 1024 * 1024 * 1024, "1.0GB"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Format(tt.n); got != tt.want {
				t.Errorf("Format(%d) = %q, want %q", tt.n, got, tt.want)
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

`TestUnitIndexMatchesUnits` is the whole point of the exercise: it would only
fail if Go initialized `unitIndex` before `units` had its elements, which
never happens because Go tracks the dependency and orders around it. The
lesson generalizes past this one file — any package-level variable built from
another (a reverse index, a total computed from a rate table, a set derived
from a slice) can be declared in whatever order reads best, because the
compiler reorders initialization to satisfy dependencies, never the reverse.

## Resources

- [The Go Programming Language Specification — Package initialization](https://go.dev/ref/spec#Package_initialization) — the exact dependency-order algorithm.
- [Effective Go — init function](https://go.dev/doc/effective_go#init) — how `init()` relates to variable initialization.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-refactor-init-env-to-explicit-constructor.md](09-refactor-init-env-to-explicit-constructor.md) | Next: [11-fail-fast-status-transition-table.md](11-fail-fast-status-transition-table.md)
