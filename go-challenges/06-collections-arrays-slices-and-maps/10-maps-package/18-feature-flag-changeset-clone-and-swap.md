# Exercise 18: Feature-Flag Changesets: Validate a Clone Before You Swap It In

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A feature-flag service exposes one dangerous endpoint: batch update, the one
an admin panel or a CI pipeline calls to flip several flags at once --
"enable new-checkout and payments-v2 together" is a single deploy step, not
two. The natural way to implement it is a loop that walks the live flag map
and sets each key as it goes, then checks whether the result still makes
sense. That order is the bug. If the ninth change in a batch of ten fails an
invariant -- "new-checkout requires payments-v2", the same shape of rule
etcd's `Txn` API and Consul's transactional KV endpoint exist to enforce
atomically across keys -- the first eight are already live. The service is
now running a configuration nobody approved and no check ever validated as a
whole, and it stays that way until someone notices and issues a second,
corrective batch.

The fix is an ordering change, not a smarter check: build the candidate
state on a private clone, run the whole invariant against the clone, and only
if it passes do you swap the clone in as the new live map. If it fails, the
live map was never touched -- not even for the changes that would, on their
own, have been fine. `maps.Clone` is what makes this cheap enough to do on
every write: it is a shallow, O(n) copy of a small map, not a database
transaction, and it turns "validate then commit" from an aspiration into two
lines of code.

This module builds `flagstore`, a package with exactly that store: a
`FlagStore` whose `ApplyChangeset` clones, mutates the clone, validates, and
swaps, guarded by a `sync.RWMutex` so it is safe to call from every request
handler concurrently. The mutate-the-live-map version is not part of that
API. It lives in the test file, where a test proves the exact state it
leaves behind.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
flagstore/               module example.com/flagstore
  go.mod                 go 1.24
  flagstore.go           Change, InvariantFunc, FlagStore; New, ApplyChangeset, Enabled, Snapshot
  flagstore_test.go      New table, ApplyChangeset table, partial-state contrast,
                          aliasing, concurrency, ExampleFlagStore_ApplyChangeset
```

- Files: `flagstore.go`, `flagstore_test.go`.
- Implement: `New(initial map[string]bool, invariant InvariantFunc) (*FlagStore, error)` cloning `initial`, rejecting a nil invariant with `ErrNilInvariant` and a seed that already fails `invariant` with `ErrInvalidInitial`; `(*FlagStore).ApplyChangeset(changes []Change) error` applying every change to a private clone of the live flags, validating the clone as a whole, and swapping it in only on success, rejecting an empty `Key` with `ErrEmptyKey` and a failing clone with `ErrInvariantViolated`; `(*FlagStore).Enabled(key string) bool`; `(*FlagStore).Snapshot() map[string]bool`.
- Test: the `New` table (valid, nil initial, nil invariant, invariant-failing initial); the `ApplyChangeset` table (a passing multi-key batch, an empty no-op batch, a violating batch, an empty key); a rejected changeset leaving the store byte-for-byte unchanged; a `applyChangesetLive` contrast proving the naive mutate-then-validate order leaks the partial state `FlagStore` never does; the clone-not-retained and `Snapshot`-does-not-alias contracts; fifty goroutines applying changesets and reading concurrently under `-race`; and `ExampleFlagStore_ApplyChangeset` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/10-maps-package/18-feature-flag-changeset-clone-and-swap
cd go-solutions/06-collections-arrays-slices-and-maps/10-maps-package/18-feature-flag-changeset-clone-and-swap
go mod edit -go=1.24
```

### Clone-then-validate is what makes "all or nothing" cheap

The bug this module is about does not look like a bug while you are writing
it. A batch-update handler that walks its list of changes and does
`flags[change.Key] = change.Enabled` in the loop body, then calls
`invariant(flags)` once at the end, reads as perfectly reasonable code --
right up until the invariant fails and you notice the loop already ran:

```go
func applyLive(flags map[string]bool, changes []Change, invariant InvariantFunc) error {
    for _, c := range changes {
        flags[c.Key] = c.Enabled   // mutates the live map immediately
    }
    return invariant(flags)        // validated only after the damage is done
}
```

`invariant` runs against `flags` after every change has already landed in
it. A non-nil error tells the caller the batch was rejected, but it lies
about the consequences: `flags` itself was never rolled back, so every
change in the batch is still there. The caller retries, or logs the error
and moves on, either way believing the flags are still whatever they were
before the call. They are not.

The fix does not need a database or a lock held across the whole request; it
needs the mutation to happen somewhere the invariant can veto before anyone
else can observe it. `maps.Clone` supplies exactly that somewhere: clone the
live map, apply every change to the clone, run `invariant` against the
clone, and only on success replace the live map with it. A rejected
changeset touches nothing a reader can see -- the clone that held the bad
state is simply discarded. Because `maps.Clone` is a flat O(n) copy over a
small map (a service's flag set is dozens of keys, not millions), the extra
allocation this costs per write is a rounding error next to the correctness
it buys, and it is the same trick `Snapshot` below reuses to hand out a copy
readers can never see mutate underneath them.

Create `flagstore.go`:

```go
// Package flagstore implements a feature-flag store that applies a batch of
// changes atomically: every change in a changeset takes effect, or none
// does.
//
// The pattern it exists to prevent is the naive PATCH-style handler that
// mutates the live flag map key by key while it validates the result. If
// the tenth change in a batch of ten fails an invariant, the first nine are
// already live -- the service now runs a configuration nobody approved and
// no invariant ever checked as a whole. FlagStore instead applies every
// change to a private clone, validates the clone in full, and only then
// swaps it in. A rejected changeset leaves the live flags byte-for-byte
// identical to what they were before the call. See the package tests for a
// side-by-side demonstration of the mutate-then-validate bug this avoids.
package flagstore

import (
	"errors"
	"fmt"
	"maps"
	"sync"
)

// Sentinel errors returned by New and ApplyChangeset. Callers should test
// for them with errors.Is rather than by comparing error strings.
var (
	// ErrNilInvariant means New was called without a validation function.
	ErrNilInvariant = errors.New("flagstore: invariant must not be nil")
	// ErrInvalidInitial means the seed flags failed the invariant at
	// construction time.
	ErrInvalidInitial = errors.New("flagstore: initial flags violate invariant")
	// ErrEmptyKey means a Change in a changeset had an empty Key.
	ErrEmptyKey = errors.New("flagstore: change key must not be empty")
	// ErrInvariantViolated means the fully-applied changeset failed the
	// store's invariant. It wraps the invariant's own error via %w.
	ErrInvariantViolated = errors.New("flagstore: changeset violates invariant")
)

// Change is one proposed mutation to a single flag.
type Change struct {
	Key     string
	Enabled bool
}

// InvariantFunc reports whether a fully-applied flag set is acceptable. It
// receives the complete proposed state, not just the keys that changed, so
// it can check relationships between flags (for example: if "new-checkout"
// is enabled, "payments-v2" must be enabled too). It must not retain or
// mutate the map it is given.
type InvariantFunc func(flags map[string]bool) error

// FlagStore holds a set of named boolean feature flags and applies
// changesets atomically against a caller-supplied invariant.
//
// A FlagStore is safe for concurrent use by multiple goroutines. Reads
// (Enabled, Snapshot) and writes (ApplyChangeset) may be called concurrently
// from any number of goroutines; a sync.RWMutex serializes writers against
// each other and against readers, and every write swaps in a wholly new,
// wholly validated map rather than mutating the one readers might be
// observing.
type FlagStore struct {
	mu        sync.RWMutex
	flags     map[string]bool
	invariant InvariantFunc
}

// New returns a FlagStore seeded with a clone of initial (the caller's map
// is never retained or mutated) and validated against invariant on every
// subsequent ApplyChangeset. New returns ErrNilInvariant if invariant is
// nil, and ErrInvalidInitial, wrapping invariant's error, if the seed flags
// themselves fail the check. A nil initial is treated as an empty flag set.
func New(initial map[string]bool, invariant InvariantFunc) (*FlagStore, error) {
	if invariant == nil {
		return nil, ErrNilInvariant
	}
	seed := maps.Clone(initial)
	if seed == nil {
		seed = map[string]bool{}
	}
	if err := invariant(seed); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInitial, err)
	}
	return &FlagStore{flags: seed, invariant: invariant}, nil
}

// ApplyChangeset applies every change in changes to a private clone of the
// current flags, validates the clone as a whole against the store's
// invariant, and only then swaps it in as the live map.
//
// If changes contains an empty Key, or the resulting clone fails the
// invariant, ApplyChangeset returns an error and the live flags are left
// exactly as they were: application is all-or-nothing, never partial. An
// empty changeset is a no-op that still re-validates the current state.
func (s *FlagStore) ApplyChangeset(changes []Change) error {
	for _, c := range changes {
		if c.Key == "" {
			return ErrEmptyKey
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Clone-then-mutate: every change lands on scratch, never on s.flags.
	// If validation below fails, s.flags is untouched -- readers never see
	// a state that was only half-applied.
	scratch := maps.Clone(s.flags)
	for _, c := range changes {
		scratch[c.Key] = c.Enabled
	}
	if err := s.invariant(scratch); err != nil {
		return fmt.Errorf("%w: %v", ErrInvariantViolated, err)
	}
	s.flags = scratch
	return nil
}

// Enabled reports whether key is currently enabled. An unknown key reports
// false, matching Go's map zero-value convention.
func (s *FlagStore) Enabled(key string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.flags[key]
}

// Snapshot returns an independent copy of the current flags. The caller may
// read or mutate it freely; it never aliases the store's internal map and
// is never affected by a later ApplyChangeset.
func (s *FlagStore) Snapshot() map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return maps.Clone(s.flags)
}
```

### Using it

Construct one `FlagStore` at startup with the service's dependency rules
folded into a single `InvariantFunc`, and share it across every request
handler -- the type's doc comment promises that is safe, and
`TestConcurrentApplyChangesetAndReads` holds it to that promise. Every write
goes through `ApplyChangeset`; every read goes through `Enabled` or
`Snapshot`. There is no way to reach the live map directly, which is what
makes the atomicity guarantee actually load-bearing rather than a comment
someone can route around.

The module has no `main.go`, because a flag store is a library a service
embeds, not a program you run on its own. Its executable demonstration is
`ExampleFlagStore_ApplyChangeset`: `go test` runs it and compares its
standard output against the `// Output:` comment, so the usage shown below
cannot drift away from the code.

### Tests

`TestNew` is the constructor table: a valid seed, a nil initial treated as
empty, a nil invariant, and a seed that already fails the invariant.
`TestApplyChangeset` is the changeset table: a multi-key batch that satisfies
the dependency together, an empty no-op batch, a batch that violates the
dependency, and an empty key rejected up front.

`TestFailedChangesetLeavesStoreUntouched` and
`TestLiveMutationLeaksPartialState` are the two tests the module exists for.
The first takes a `Snapshot` before a changeset that fails validation and
another after, and asserts they are equal with `maps.Equal` -- a rejected
changeset must not move the live flags at all. The second calls the
unexported `applyChangesetLive` helper -- never reachable from `FlagStore`'s
API -- with the identical rejected changeset and shows it left one of the two
changes applied; then it runs the same changeset through `FlagStore` and
shows the store is untouched. `TestAliasingContracts` pins the two aliasing
rules from the doc comments: `New` clones its input, and `Snapshot` returns
an independent copy. `TestConcurrentApplyChangesetAndReads` runs fifty
goroutines each applying one changeset and reading concurrently under
`-race`, then checks every applied key stuck.

Create `flagstore_test.go`:

```go
package flagstore

import (
	"errors"
	"fmt"
	"maps"
	"sync"
	"testing"
)

// checkoutInvariant models a real dependency between two flags: the new
// checkout flow may only be enabled once the payments backend that serves
// it is also enabled.
func checkoutInvariant(flags map[string]bool) error {
	if flags["new-checkout"] && !flags["payments-v2"] {
		return errors.New("new-checkout requires payments-v2")
	}
	return nil
}

func alwaysValid(map[string]bool) error { return nil }

// applyChangesetLive is the mutate-then-validate handler most PATCH /flags
// endpoints start out as: it writes every change straight into the caller's
// live map, then validates the result. It is never exported and never
// reachable from FlagStore; it exists so the tests can pin exactly what it
// gets wrong.
func applyChangesetLive(flags map[string]bool, changes []Change, invariant InvariantFunc) error {
	for _, c := range changes {
		flags[c.Key] = c.Enabled // mutates the live map immediately
	}
	return invariant(flags) // validated only after the damage is already done
}

func TestNew(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		initial map[string]bool
		inv     InvariantFunc
		wantErr error
	}{
		{name: "valid initial", initial: map[string]bool{"payments-v2": true}, inv: checkoutInvariant},
		{name: "nil initial treated as empty", initial: nil, inv: checkoutInvariant},
		{name: "nil invariant rejected", initial: nil, inv: nil, wantErr: ErrNilInvariant},
		{
			name:    "invalid initial rejected",
			initial: map[string]bool{"new-checkout": true, "payments-v2": false},
			inv:     checkoutInvariant,
			wantErr: ErrInvalidInitial,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(tc.initial, tc.inv)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("New err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestApplyChangeset(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		changes []Change
		wantErr error
	}{
		{
			name: "enabling both together satisfies the dependency",
			changes: []Change{
				{Key: "payments-v2", Enabled: true},
				{Key: "new-checkout", Enabled: true},
			},
		},
		{name: "empty changeset is a no-op"},
		{
			name:    "enabling new-checkout alone violates the dependency",
			changes: []Change{{Key: "new-checkout", Enabled: true}},
			wantErr: ErrInvariantViolated,
		},
		{
			name:    "empty key is rejected before anything is touched",
			changes: []Change{{Key: "", Enabled: true}},
			wantErr: ErrEmptyKey,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, err := New(map[string]bool{}, checkoutInvariant)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if err := s.ApplyChangeset(tc.changes); !errors.Is(err, tc.wantErr) {
				t.Fatalf("ApplyChangeset err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestFailedChangesetLeavesStoreUntouched is the heart of the module: a
// changeset that fails validation must not move the live flags at all, not
// even for the changes that would individually have been fine.
func TestFailedChangesetLeavesStoreUntouched(t *testing.T) {
	t.Parallel()

	s, err := New(map[string]bool{"payments-v2": true}, checkoutInvariant)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	before := s.Snapshot()

	// Disabling payments-v2 while enabling new-checkout in the same batch
	// is invalid as a whole, even though the store's prior state already
	// satisfied the invariant.
	changes := []Change{
		{Key: "payments-v2", Enabled: false},
		{Key: "new-checkout", Enabled: true},
	}
	if err := s.ApplyChangeset(changes); !errors.Is(err, ErrInvariantViolated) {
		t.Fatalf("ApplyChangeset err = %v, want ErrInvariantViolated", err)
	}
	if after := s.Snapshot(); !maps.Equal(before, after) {
		t.Fatalf("store changed after a rejected changeset: before=%v after=%v", before, after)
	}
}

// TestLiveMutationLeaksPartialState contrasts applyChangesetLive against
// FlagStore.ApplyChangeset on the identical rejected changeset: the naive
// version leaves its changes applied; the store does not.
func TestLiveMutationLeaksPartialState(t *testing.T) {
	t.Parallel()

	changes := []Change{
		{Key: "payments-v2", Enabled: false},
		{Key: "new-checkout", Enabled: true},
	}

	live := map[string]bool{"payments-v2": true}
	if err := applyChangesetLive(live, changes, checkoutInvariant); err == nil {
		t.Fatal("applyChangesetLive: want error, got nil")
	}
	if !live["new-checkout"] || live["payments-v2"] {
		t.Fatalf("applyChangesetLive did not leak partial state: live=%v", live)
	}

	s, err := New(map[string]bool{"payments-v2": true}, checkoutInvariant)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.ApplyChangeset(changes); !errors.Is(err, ErrInvariantViolated) {
		t.Fatalf("ApplyChangeset err = %v, want ErrInvariantViolated", err)
	}
	if s.Enabled("new-checkout") || !s.Enabled("payments-v2") {
		t.Fatalf("FlagStore leaked partial state: new-checkout=%v payments-v2=%v",
			s.Enabled("new-checkout"), s.Enabled("payments-v2"))
	}
}

// TestAliasingContracts pins both documented aliasing rules: New clones its
// input instead of retaining it, and Snapshot returns an independent copy.
func TestAliasingContracts(t *testing.T) {
	t.Parallel()

	initial := map[string]bool{"a": true}
	s, err := New(initial, alwaysValid)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	initial["a"] = false
	if !s.Enabled("a") {
		t.Fatal("mutating the caller's initial map changed the store")
	}

	snap := s.Snapshot()
	snap["a"] = false
	if !s.Enabled("a") {
		t.Fatal("mutating the snapshot changed the store")
	}
}

func TestConcurrentApplyChangesetAndReads(t *testing.T) {
	t.Parallel()

	s, err := New(map[string]bool{}, alwaysValid)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const n = 50
	var wg sync.WaitGroup
	for i := range n {
		key := fmt.Sprintf("flag-%d", i)
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := s.ApplyChangeset([]Change{{Key: key, Enabled: true}}); err != nil {
				t.Errorf("ApplyChangeset(%s): %v", key, err)
			}
		}()
		go func() { defer wg.Done(); _ = s.Snapshot(); _ = s.Enabled(key) }()
	}
	wg.Wait()

	snap := s.Snapshot()
	for i := range n {
		if key := fmt.Sprintf("flag-%d", i); !snap[key] {
			t.Errorf("%s not enabled after concurrent apply", key)
		}
	}
}

// ExampleFlagStore_ApplyChangeset is the runnable demonstration of this
// module: go test executes it and compares its stdout against the Output
// comment below.
func ExampleFlagStore_ApplyChangeset() {
	s, err := New(map[string]bool{}, checkoutInvariant)
	if err != nil {
		panic(err)
	}

	err = s.ApplyChangeset([]Change{
		{Key: "payments-v2", Enabled: true},
		{Key: "new-checkout", Enabled: true},
	})
	fmt.Println("apply both:", err)
	fmt.Println("new-checkout:", s.Enabled("new-checkout"))

	err = s.ApplyChangeset([]Change{{Key: "payments-v2", Enabled: false}})
	fmt.Println("disable dependency alone:", err)
	fmt.Println("payments-v2 still:", s.Enabled("payments-v2"))

	// Output:
	// apply both: <nil>
	// new-checkout: true
	// disable dependency alone: flagstore: changeset violates invariant: new-checkout requires payments-v2
	// payments-v2 still: true
}
```

## Review

`FlagStore` is correct when a rejected changeset changes nothing: not the
key that failed the invariant, and not the other keys in the same batch that
would have been fine on their own. `ApplyChangeset` gets there by never
writing to `s.flags` directly -- every change lands on a `maps.Clone` of it,
`invariant` runs against that clone, and only a clone that passes ever
becomes the new `s.flags`. The mistake this design avoids is validating
after mutating the live map, which turns a rejected batch into a silent
partial commit -- `TestLiveMutationLeaksPartialState` pins exactly that
difference against `FlagStore`. Around that core, `New` rejects a nil
invariant with `ErrNilInvariant` and a seed that already fails it with
`ErrInvalidInitial`, `ApplyChangeset` rejects an empty key with
`ErrEmptyKey` before touching anything, both clone their input rather than
retain it, and a `sync.RWMutex` makes every method safe to call from any
number of goroutines at once. `ExampleFlagStore_ApplyChangeset` is the
executable documentation: `go test` verifies its output. Run
`go test -count=1 -race ./...`.

## Resources

- [`maps.Clone`](https://pkg.go.dev/maps#Clone) — the shallow copy this module builds its atomicity on.
- [`maps.Equal`](https://pkg.go.dev/maps#Equal) — used by the test that proves a rejected changeset leaves the store unchanged.
- [etcd: Transactions](https://etcd.io/docs/latest/learning/api/#transaction) — a real distributed store offering the same all-or-nothing contract across keys.
- [Consul: Transactions API](https://developer.hashicorp.com/consul/api-docs/txn) — the same pattern in a service-mesh control plane.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [17-fingerprint-dedup-stream-filter.md](17-fingerprint-dedup-stream-filter.md) | Next: [19-alert-threshold-scan-early-stop-iterator.md](19-alert-threshold-scan-early-stop-iterator.md)
