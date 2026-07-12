# Exercise 13: A Scoped Cleanup Closure for Temporary Flag Overrides

**Nivel: Intermedio** — validacion rapida (un test corto).

Tests and short-lived request paths often need to flip a feature flag for one
block of code and be certain it snaps back afterward, even if several
overrides on the same key are nested. This module builds that as a single
method that returns an anonymous restore function — a closure over a
snapshot of the prior state, not a live reference — meant to be used as
`defer store.Override("k", true)()`.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
flagscope/                    module example.com/flagscope
  go.mod
  flagscope.go                 Store, NewStore, Enabled, Override (returns a restore closure)
  flagscope_test.go            restore to prior value, restore of a previously-unset key, nested overrides
```

- Files: `flagscope.go`, `flagscope_test.go`.
- Implement: `Store` holding `map[string]bool`; `NewStore(initial)` that copies its input; `Enabled(key) bool`; `Override(key, val) func()` that sets the flag and returns a closure snapshotting the prior value (or its absence) to restore later.
- Test: overriding then restoring returns to the original value; overriding a key that was never set removes it entirely on restore; two nested overrides of the same key unwind correctly in reverse order.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/05-anonymous-functions/13-scoped-flag-override-cleanup
cd go-solutions/04-functions/05-anonymous-functions/13-scoped-flag-override-cleanup
go mod edit -go=1.24
```

### A snapshot closure, not a reference closure

Most closures in this lesson capture a variable *by reference* and read
whatever it holds when they finally run. `Override` deliberately does the
opposite: it reads the prior value (and whether the key existed at all)
*immediately*, before returning, and bakes that snapshot into the returned
literal. That is what makes nested overrides on the same key correct —
`restoreInner` must restore to the value the outer override set, not to
whatever the map happens to hold when `restoreInner` is finally called, which
by then could be a third override's value. If `Override` captured `s.flags`
by reference and re-read it at call time instead of snapshotting up front,
nested restores would unwind to the wrong values.

Create `flagscope.go`:

```go
package flagscope

// Store holds boolean feature flags by key.
type Store struct {
	flags map[string]bool
}

// NewStore builds a Store from an initial set of flags. The map is copied so
// later mutation of the caller's map does not reach into the Store.
func NewStore(initial map[string]bool) *Store {
	flags := make(map[string]bool, len(initial))
	for k, v := range initial {
		flags[k] = v
	}
	return &Store{flags: flags}
}

// Enabled reports whether key is set and true.
func (s *Store) Enabled(key string) bool {
	return s.flags[key]
}

// Override sets key to val and returns a restore function that undoes the
// change. The restore closure captures a snapshot of the prior state — the
// old value and whether the key existed at all — taken at the moment
// Override is called, not a live reference to s.flags; that snapshot is what
// lets nested overrides on the same key unwind correctly in reverse order.
// The idiomatic call site is `defer store.Override("k", true)()`, scoping the
// change to the enclosing block.
func (s *Store) Override(key string, val bool) func() {
	prevVal, existed := s.flags[key]
	s.flags[key] = val
	return func() {
		if existed {
			s.flags[key] = prevVal
		} else {
			delete(s.flags, key)
		}
	}
}
```

### Tests

`TestOverrideRestoresPreviousValue` checks the basic round trip.
`TestOverrideOnUnsetKeyIsRemovedByRestore` checks that overriding a key with
no prior entry leaves no trace after restore. `TestNestedOverridesUnwindInReverseOrder`
overrides the same key twice and restores in LIFO order, checking the
intermediate and final states. `TestNewStoreCopiesInitialMap` guards the
defensive copy.

Create `flagscope_test.go`:

```go
package flagscope

import "testing"

func TestOverrideRestoresPreviousValue(t *testing.T) {
	t.Parallel()
	s := NewStore(map[string]bool{"beta": false})

	restore := s.Override("beta", true)
	if !s.Enabled("beta") {
		t.Fatal("Enabled(beta) = false during override, want true")
	}
	restore()
	if s.Enabled("beta") {
		t.Fatal("Enabled(beta) = true after restore, want false")
	}
}

func TestOverrideOnUnsetKeyIsRemovedByRestore(t *testing.T) {
	t.Parallel()
	s := NewStore(nil)

	restore := s.Override("new-flag", true)
	if !s.Enabled("new-flag") {
		t.Fatal("Enabled(new-flag) = false during override, want true")
	}
	restore()
	if _, present := s.flags["new-flag"]; present {
		t.Fatal("restore() left new-flag present in the map, want removed")
	}
}

func TestNestedOverridesUnwindInReverseOrder(t *testing.T) {
	t.Parallel()
	s := NewStore(map[string]bool{"tier": false})

	restoreOuter := s.Override("tier", true)
	restoreInner := s.Override("tier", false)
	if s.Enabled("tier") {
		t.Fatal("Enabled(tier) = true after inner override, want false")
	}

	restoreInner()
	if !s.Enabled("tier") {
		t.Fatal("Enabled(tier) = false after inner restore, want true (outer value)")
	}

	restoreOuter()
	if s.Enabled("tier") {
		t.Fatal("Enabled(tier) = true after outer restore, want false (original value)")
	}
}

func TestNewStoreCopiesInitialMap(t *testing.T) {
	t.Parallel()
	initial := map[string]bool{"x": true}
	s := NewStore(initial)
	initial["x"] = false
	if !s.Enabled("x") {
		t.Fatal("Enabled(x) changed after mutating the caller's source map, want independent copy")
	}
}
```

## Review

The correctness hinges on the snapshot being taken at `Override`'s call time,
not at the returned closure's call time — that is the opposite of the
"capture by reference, read at run time" rule that governs most closures in
this lesson, and mixing the two up is the classic bug in scoped-override
helpers. The nested-override test is the one that would fail first if the
snapshot were taken lazily instead.

## Resources

- [Closures](https://go.dev/tour/moretypes/25)
- [defer statements](https://go.dev/ref/spec#Defer_statements)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-iife-lookup-table-order-transitions.md](12-iife-lookup-table-order-transitions.md) | Next: [14-strategy-selection-pricing.md](14-strategy-selection-pricing.md)
