# Exercise 11: Comma-Ok vs Zero Value: telling "off" from "never configured"

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A feature-flag service backs every rollout decision with a `map[string]bool`:
one entry per flag, `true` to enable, `false` to kill it. The trap is that
`false` is also exactly what a single-value map read returns for a flag that
was *never configured at all* — `m["unknown"]` and `m["explicitly-disabled"]`
both read as `false`. A kill switch that an operator explicitly flipped off
and a flag nobody has ever touched are two very different operational facts,
and conflating them either silently re-enables a disabled feature (falling
back to a default) or silently disables a feature that was supposed to
default on. Comma-ok is the only construct in the language that separates
them.

Production flag stores add a second failure mode on top: a typo in the flag
name. `LaunchDarkly`, `Unleash`, and every in-house flag service anyone has
built all face the same question when a caller asks for a key that was never
declared — is that "off" too? Silently treating an unregistered key as just
another unset flag hides the typo behind the same `false` a real kill switch
would produce, and the bug ships until someone notices the feature never
turned on. This module's `Store` closes both gaps: `Lookup` separates
explicit-false from never-set with comma-ok, and a fixed key schema rejects
any key outside it before comma-ok even gets a chance to run.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
flagstore/                    module example.com/flagstore
  go.mod                      go 1.24
  flags.go                    Store; New, Set, Unset, Lookup, Enabled; two sentinel errors
  flags_test.go                comma-ok table, unknown-key rejection, fallback, naive-read
                               contrast, concurrent access, ExampleStore_Enabled
```

- Files: `flags.go`, `flags_test.go`.
- Implement: `New(knownKeys []string, fallback bool) (*Store, error)` rejecting an empty schema with `ErrNoFlags`; `(*Store).Set(key string, value bool) error`, `(*Store).Unset(key string) error`, `(*Store).Lookup(key string) (value, ok bool, err error)`, and `(*Store).Enabled(key string) (bool, error)`, each rejecting a key outside the schema with `ErrUnknownFlag`.
- Test: the four states a flag can be in (explicit true, explicit false, registered-but-unset, unregistered) via `Lookup`; every accessor's unknown-key rejection; `Enabled`'s fallback behavior; `Unset` reverting to the fallback; a naive single-value read contrasted against `Lookup`; `Store` driven from many goroutines under `-race`; and `ExampleStore_Enabled` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/flagstore
cd ~/go-exercises/flagstore
go mod init example.com/flagstore
go mod edit -go=1.24
```

### Why a single-value read cannot answer "is this flag configured"

`v := m[k]` is a single-value form: it reads the stored value if `k` is
present, or the value type's zero if `k` is absent, and there is no way to
tell which case happened from `v` alone. For an `int` counter this is often
fine, because `0` is a natural "nothing recorded yet" starting point. For a
`bool` feature flag it is not fine, because `false` is a real, meaningful,
*intentionally stored* value — "this is explicitly disabled" — and it is
bit-for-bit identical to the zero value returned for "nobody has an opinion
about this key." A rollout system that reads `m[k]` and treats `false` as
"use the default" will happily re-enable a flag an operator just killed, the
moment that operator's disable request is read back through the wrong path:

```go
func naiveEnabled(flags map[string]bool, key string) bool {
	return flags[key]   // false for "explicitly off" AND for "never heard of it"
}
```

`v, ok := m[k]` is the two-value form, and `ok` is exactly the missing bit of
information: `true` when `k` is a key in the map (regardless of what value it
maps to), `false` when it is not. `Store.Lookup` exposes this directly so a
caller who needs the distinction — an audit log, a "why is this flag in this
state" debug endpoint, an admin UI that needs to show "explicitly off" versus
"inherited default" — gets it. `Store.Enabled` is a second, narrower method
built on top of `Lookup`: it collapses the three-way fact (on / explicitly
off / unset) down to the two-way question most call sites actually have
("should I run this code path"), using the store's configured fallback for
the unset case.

`Store` adds one more guard that a bare `map[string]bool` cannot offer: a
fixed schema of known keys, supplied once at construction. Every accessor
checks the key against that schema before it ever touches comma-ok, so a
typo'd flag name (`"lagacy-billing"` for `"legacy-billing"`) fails loudly
with `ErrUnknownFlag` instead of silently behaving like an unset — but
otherwise real — flag. Without the schema check, a typo and a legitimate
unset flag would be indistinguishable, which defeats the entire point of
separating "off" from "never configured" one layer up.

Create `flags.go`:

```go
// Package flagstore is a feature-flag store that keeps "explicitly disabled"
// distinct from "never configured" -- a distinction a plain map[string]bool
// read cannot make, because both states read back as the zero value false.
package flagstore

import (
	"errors"
	"fmt"
	"sync"
)

// Sentinel errors returned by Store's constructor and its accessors.
// Callers should test for them with errors.Is.
var (
	// ErrNoFlags means New was called with an empty key schema.
	ErrNoFlags = errors.New("flagstore: at least one known flag key is required")
	// ErrUnknownFlag means the key is not part of the Store's registered
	// schema -- most often an operator's typo rather than a real flag.
	ErrUnknownFlag = errors.New("flagstore: flag key is not registered")
)

// Store holds boolean feature flags for a fixed schema of known keys. The
// zero value of bool is false, which is also a legitimate flag state, so
// Store must distinguish "explicitly disabled" from "never configured" or
// every kill-switch check collapses into the same answer. It also rejects
// any key outside its registered schema, catching a mistyped flag name at
// the call site instead of silently tracking a flag nobody declared.
//
// Store is safe for concurrent use by multiple goroutines.
type Store struct {
	mu       sync.RWMutex
	flags    map[string]bool
	known    map[string]struct{} // immutable after New; read without mu
	fallback bool
}

// New returns a Store whose schema is knownKeys. fallback is what Enabled
// reports for a key that is registered but was never explicitly Set. New
// returns ErrNoFlags if knownKeys is empty.
func New(knownKeys []string, fallback bool) (*Store, error) {
	if len(knownKeys) == 0 {
		return nil, ErrNoFlags
	}
	known := make(map[string]struct{}, len(knownKeys))
	for _, k := range knownKeys {
		known[k] = struct{}{}
	}
	return &Store{flags: make(map[string]bool), known: known, fallback: fallback}, nil
}

// Set explicitly records key's state, including false. A flag Set to false
// is present in the store and reads back with ok true from Lookup. Set
// returns ErrUnknownFlag if key is outside the registered schema.
func (s *Store) Set(key string, value bool) error {
	if _, ok := s.known[key]; !ok {
		return fmt.Errorf("%w: %q", ErrUnknownFlag, key)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flags[key] = value
	return nil
}

// Unset removes any explicit configuration for key, reverting it to
// never-configured. It returns ErrUnknownFlag if key is outside the schema.
func (s *Store) Unset(key string) error {
	if _, ok := s.known[key]; !ok {
		return fmt.Errorf("%w: %q", ErrUnknownFlag, key)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.flags, key)
	return nil
}

// Lookup reports key's explicit value and whether it was ever Set. This is
// the only way to tell "explicitly false" (false, true, nil) apart from
// "never configured" (false, false, nil) -- both read as false the way a
// plain map index would. It returns ErrUnknownFlag for a key outside the
// schema, distinguishing a typo from a flag nobody has touched yet.
func (s *Store) Lookup(key string) (value, ok bool, err error) {
	if _, known := s.known[key]; !known {
		return false, false, fmt.Errorf("%w: %q", ErrUnknownFlag, key)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok = s.flags[key]
	return value, ok, nil
}

// Enabled reports whether key should be treated as on: its explicit value
// if Set, or the Store's configured default if never configured. It returns
// ErrUnknownFlag for a key outside the schema.
func (s *Store) Enabled(key string) (bool, error) {
	value, ok, err := s.Lookup(key)
	if err != nil {
		return false, err
	}
	if ok {
		return value, nil
	}
	return s.fallback, nil
}
```

### Using it

Construct one `Store` per process with the complete set of flag keys the
service knows about and the default those flags should have when nobody has
set them — `New` refuses to build a schema-less store. From then on, `Set`
and `Unset` are the only writes, `Lookup` is the audit path when a caller
needs to know whether a flag was ever touched, and `Enabled` is what most
call sites actually want: a plain `bool` answer with the fallback already
applied. `Store` carries a `sync.RWMutex` internally and its doc comment
promises safety for concurrent use, so a single value can be shared across
every request-handling goroutine.

The module has no `main.go`, because a flag store is a library, not a tool.
Its executable demonstration is `ExampleStore_Enabled`: `go test` runs it and
compares its standard output against the `// Output:` comment, so the usage
shown below cannot drift away from the code.

### Tests

`TestLookupDistinguishesAllCombinations` is the table that matters most: it
pins all four states a flag can be in — explicit true, explicit false,
registered-but-unset, and unregistered — each with its own `(value, ok, err)`
triple. `TestAccessorsRejectUnknownKey` confirms every one of the four
methods, not just `Lookup`, refuses a key outside the schema.
`TestEnabledFallsBackToDefaultWhenUnset` is the sharpest correctness test: it
configures a store whose default is "on," explicitly disables one flag, and
proves `Enabled` reports that flag as `false` while an untouched flag reports
the fallback `true`. `TestUnsetRevertsToFallback` confirms `Unset` is not the
same as `Set(key, false)`.

`TestNaiveReadCollapsesExplicitFalseAndUnset` is the antipattern contrast:
`naiveEnabled` is unexported and lives only in the test file, doing exactly
what a plain `flags[key]` read does — returning `false` for both an
explicitly disabled flag and one nobody has touched — and the test shows
`Store.Enabled` getting the second case right where the naive read cannot.
`TestStoreConcurrentAccess` drives `Set` and `Lookup` on shared keys from
many goroutines at once; run under `-race`, it is what holds `Store` to the
concurrency contract in its doc comment.

Create `flags_test.go`:

```go
package flagstore

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// naiveEnabled is the mapper bug for a bool flag: it reads the map with the
// single-value form and ignores the missing-key case entirely, exactly the
// mistake Lookup exists to prevent. It is never exported and never reachable
// from the package API.
func naiveEnabled(flags map[string]bool, key string) bool {
	return flags[key]
}

func TestNewRejectsEmptyKnownKeys(t *testing.T) {
	t.Parallel()

	if _, err := New(nil, false); !errors.Is(err, ErrNoFlags) {
		t.Fatalf("New(nil) err = %v, want ErrNoFlags", err)
	}
}

func TestAccessorsRejectUnknownKey(t *testing.T) {
	t.Parallel()

	s, err := New([]string{"beta-ui"}, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := s.Set("typo-flag", true); !errors.Is(err, ErrUnknownFlag) {
		t.Fatalf("Set err = %v, want ErrUnknownFlag", err)
	}
	if err := s.Unset("typo-flag"); !errors.Is(err, ErrUnknownFlag) {
		t.Fatalf("Unset err = %v, want ErrUnknownFlag", err)
	}
	if _, _, err := s.Lookup("typo-flag"); !errors.Is(err, ErrUnknownFlag) {
		t.Fatalf("Lookup err = %v, want ErrUnknownFlag", err)
	}
	if _, err := s.Enabled("typo-flag"); !errors.Is(err, ErrUnknownFlag) {
		t.Fatalf("Enabled err = %v, want ErrUnknownFlag", err)
	}
}

// TestLookupDistinguishesAllCombinations covers the four states a boolean
// flag can be in: explicitly true, explicitly false, registered but never
// set, and not part of the schema at all.
func TestLookupDistinguishesAllCombinations(t *testing.T) {
	t.Parallel()

	s, err := New([]string{"beta-ui", "legacy-billing", "dark-mode"}, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Set("beta-ui", true); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Set("legacy-billing", false); err != nil {
		t.Fatalf("Set: %v", err)
	}

	tests := []struct {
		name      string
		key       string
		wantValue bool
		wantOK    bool
		wantErr   bool
	}{
		{"explicit true", "beta-ui", true, true, false},
		{"explicit false", "legacy-billing", false, true, false},
		{"registered, never set", "dark-mode", false, false, false},
		{"not in schema", "ghost-flag", false, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			value, ok, err := s.Lookup(tc.key)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Lookup(%q) err = %v, wantErr %v", tc.key, err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if value != tc.wantValue || ok != tc.wantOK {
				t.Fatalf("Lookup(%q) = (%v, %v), want (%v, %v)", tc.key, value, ok, tc.wantValue, tc.wantOK)
			}
		})
	}
}

func TestEnabledFallsBackToDefaultWhenUnset(t *testing.T) {
	t.Parallel()

	s, err := New([]string{"legacy-billing", "dark-mode"}, true) // fallback: on
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Set("legacy-billing", false); err != nil {
		t.Fatalf("Set: %v", err)
	}

	if got, err := s.Enabled("legacy-billing"); err != nil || got != false {
		t.Fatalf("Enabled(legacy-billing) = (%v, %v), want (false, nil)", got, err)
	}
	if got, err := s.Enabled("dark-mode"); err != nil || got != true {
		t.Fatalf("Enabled(dark-mode) = (%v, %v), want (true, nil)", got, err)
	}
}

func TestUnsetRevertsToFallback(t *testing.T) {
	t.Parallel()

	s, err := New([]string{"beta-ui"}, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Set("beta-ui", true); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, ok, _ := s.Lookup("beta-ui"); !ok {
		t.Fatal("beta-ui should be set")
	}

	if err := s.Unset("beta-ui"); err != nil {
		t.Fatalf("Unset: %v", err)
	}
	if _, ok, _ := s.Lookup("beta-ui"); ok {
		t.Fatal("beta-ui should be unset")
	}
	if got, err := s.Enabled("beta-ui"); err != nil || got != false {
		t.Fatalf("Enabled(beta-ui) after Unset = (%v, %v), want fallback (false, nil)", got, err)
	}
}

// TestNaiveReadCollapsesExplicitFalseAndUnset pins the exact defect a plain
// map read produces: it cannot tell "explicitly disabled" from "nobody has
// an opinion", so it always answers false for both, even when the store's
// configured fallback for an unset key is true.
func TestNaiveReadCollapsesExplicitFalseAndUnset(t *testing.T) {
	t.Parallel()

	raw := map[string]bool{"legacy-billing": false}

	if got := naiveEnabled(raw, "legacy-billing"); got != false {
		t.Fatalf("naiveEnabled(legacy-billing) = %v, want false", got)
	}
	if got := naiveEnabled(raw, "dark-mode"); got != false {
		t.Fatalf("naiveEnabled(dark-mode) = %v, want false (indistinguishable from explicit)", got)
	}

	s, err := New([]string{"legacy-billing", "dark-mode"}, true)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Set("legacy-billing", false); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.Enabled("dark-mode")
	if err != nil {
		t.Fatalf("Enabled: %v", err)
	}
	if got != true {
		t.Fatalf("Store.Enabled(dark-mode) = %v, want true (fallback) -- the naive read got this wrong above", got)
	}
}

// TestStoreConcurrentAccess drives Set and Lookup on the same keys from many
// goroutines at once. It exists to be run under -race: Store's doc comment
// promises safety for concurrent use, and this is what holds it to that.
func TestStoreConcurrentAccess(t *testing.T) {
	t.Parallel()

	keys := []string{"beta-ui", "legacy-billing", "dark-mode"}
	s, err := New(keys, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := keys[i%len(keys)]
			if err := s.Set(key, i%2 == 0); err != nil {
				t.Errorf("Set: %v", err)
			}
			if _, _, err := s.Lookup(key); err != nil {
				t.Errorf("Lookup: %v", err)
			}
		}(i)
	}
	wg.Wait()
}

// ExampleStore_Enabled is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExampleStore_Enabled() {
	s, err := New([]string{"beta-ui", "legacy-billing", "dark-mode"}, true)
	if err != nil {
		panic(err)
	}
	_ = s.Set("beta-ui", true)
	_ = s.Set("legacy-billing", false)

	for _, key := range []string{"beta-ui", "legacy-billing", "dark-mode"} {
		value, ok, _ := s.Lookup(key)
		enabled, _ := s.Enabled(key)
		fmt.Printf("%s: lookup=(%v,%v) enabled=%v\n", key, value, ok, enabled)
	}

	if _, _, err := s.Lookup("ghost-flag"); errors.Is(err, ErrUnknownFlag) {
		fmt.Println("ghost-flag rejected:", err)
	}

	// Output:
	// beta-ui: lookup=(true,true) enabled=true
	// legacy-billing: lookup=(false,true) enabled=false
	// dark-mode: lookup=(false,false) enabled=true
	// ghost-flag rejected: flagstore: flag key is not registered: "ghost-flag"
}
```

## Review

The store is correct exactly when `Lookup`'s `ok` — not the value it carries
— is what any caller asks about presence, and `Enabled` is the only place a
default is allowed to leak in. The trap this exercise pins is treating
`flags[key]` (the antipattern isolated in `naiveEnabled`) as "the flag's
state": that collapses "explicitly off" and "never configured" into the same
`false`, which is precisely backwards for a kill switch, where "explicitly
off" must always win over any default. Layered on top, the fixed key schema
turns a third failure mode — a typo'd flag name — into a loud
`ErrUnknownFlag` instead of a silently-always-off flag indistinguishable from
a real one. `Store` guards its internal map with a `sync.RWMutex`, so it can
be shared across every request-handling goroutine, which
`TestStoreConcurrentAccess` exercises under `-race`. Run
`go test -count=1 -race ./...`.

## Resources

- [Go Specification: Index expressions](https://go.dev/ref/spec#Index_expressions) — the two-result form of a map index expression.
- [Effective Go: Comma, ok](https://go.dev/doc/effective_go#maps) — the idiom this exercise is built around.
- [sync.RWMutex](https://pkg.go.dev/sync#RWMutex) — the lock `Store` uses to stay safe for concurrent use.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-frequency-top-n-aggregation.md](10-frequency-top-n-aggregation.md) | Next: [12-delete-during-range-safe-sweep.md](12-delete-during-range-safe-sweep.md)
