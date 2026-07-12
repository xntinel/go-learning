# Exercise 12: Backend Outlier Detector -- Map-Element Addressability and the Forgotten Store-Back

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A load balancer's passive health check -- the mechanism Envoy calls outlier
detection -- watches every request it proxies and ejects a backend from
rotation once it has failed too many times in a row. The state it needs per
backend is small: a running failure count and the length of the current
losing streak, both mutated on the hot path of every proxied request. The
natural home for that state is `map[string]BackendStats`, one entry per
backend address, and the natural way to update it is the pattern every Go
programmer reaches for on a struct field: read it, change it, done. Except a
map does not let you do that.

`m[addr].Failures++` does not compile. Indexing a map yields a value that is
not addressable -- the runtime is free to move an entry during a rehash, so
the language refuses to hand out a pointer into the table. The only legal
mutation is copy-out, mutate, store-back: `s := m[addr]; s.Failures++;
m[addr] = s`. The trap is that the first two steps are enough to satisfy the
compiler and enough to look, on a fast read, like correct code. Drop the
third line and nothing complains: `s` is a perfectly ordinary, perfectly
addressable local variable, `s.Failures++` compiles and runs, and the
incremented copy is thrown away the moment the function returns. The map
entry -- if one even exists yet -- never changes. A backend can fail every
single request forever and the outlier detector will report it healthy,
because the counter it is reading was never actually written.

This module builds that detector as a package: `Registry` tracks
`BackendStats` per address, correctly, with the store-back as a permanent
part of every mutating method. The forgotten-store-back version is not part
of that API -- it lives in the test file, isolated, as the thing the tests
prove wrong.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
outlierdetector/          module example.com/outlierdetector
  go.mod                  go 1.24
  detector.go             BackendStats, Registry; New, RecordFailure, RecordSuccess,
                          Stats, IsEjected
  detector_test.go        failure/success table, ejection-threshold table, the
                          forgotten-store-back contrast, concurrency, ExampleRegistry
```

- Files: `detector.go`, `detector_test.go`.
- Implement: `New(capacityHint int) (*Registry, error)` rejecting a negative hint with `ErrNegativeCapacityHint`; `(*Registry).RecordFailure(addr string)`; `RecordSuccess(addr string)`; `Stats(addr string) (BackendStats, bool)`; `IsEjected(addr string, threshold int) bool`.
- Test: a failure/recovery/failure sequence pinning both counters and the streak reset; the ejection table (below threshold, exactly at, above, and an address never recorded); the forgotten-store-back contrast showing the buggy map gains no entry at all after five calls while `Registry` tracks all five; concurrent `RecordFailure` from many goroutines under `-race`; and `ExampleRegistry` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Map elements are not addressable, and the fix has three mandatory steps

A `map[string]BackendStats` stores `BackendStats` by value. Reading `m[addr]`
copies the struct out; there is no way to get `&m[addr]`, because the spec
says so directly: the result of a map index expression is not addressable.
The reason is representational, not arbitrary -- Go's map implementation
(a Swiss table since Go 1.24, a bucket array before it) may relocate an
entry's storage on a rehash, so a pointer into the table would be a pointer
the runtime could invalidate out from under you. The compiler closes that
door entirely: it does not let you take the address in the first place, so
`m[addr].Failures++` is rejected before it can ever run:

```go
m[addr].Failures++ // does not compile: cannot take address of m[addr]
```

The correct pattern is three separate statements, and all three are
required every time: copy the current value out, mutate the local copy,
write the local copy back.

```go
s := stats[addr]         // 1. copy out (zero value if addr is new)
s.Failures++              // 2. mutate the copy
stats[addr] = s           // 3. store back -- without this, nothing changed
```

The compiler enforces steps 1 and 2 by construction -- there is no other way
to write "increment a field of a map's value type". It enforces nothing
about step 3. Delete that line and the function still compiles, because `s`
was always a legal, addressable local variable; the assignment back into the
map is a completely separate statement the compiler has no way to know you
meant to keep. The failure mode is not a wrong count, it is *no* count: if
`addr` was never recorded before, the map gains no entry for it at all,
because the only place an entry would be created -- the store-back -- never
executes. A backend can fail its health check forever and `Stats` will
report it as never having been seen.

`Registry` wraps this pattern behind a mutex, because a passive health check
runs on the hot path of every proxied request from many goroutines at once,
and pairs it with a capacity hint at construction so the map backing tens of
thousands of backend addresses does not pay for repeated grow-and-rehash as
they are first observed.

Create `detector.go`:

```go
// Package outlierdetector tracks passive health-check counters per backend
// address, the shape Envoy calls outlier detection: every proxied request's
// success or failure updates a running failure count and a consecutive-
// failure streak for the backend it hit, and a backend whose streak crosses
// a threshold is reported as an outlier to eject from rotation.
//
// The package exists to get one detail right: BackendStats is stored by
// value in a map, and a map element is not addressable in Go, so mutating
// it always requires copying it out, changing the copy, and writing the
// copy back. Forgetting the write-back compiles cleanly and silently drops
// every update. See the package tests for a demonstration.
package outlierdetector

import (
	"errors"
	"fmt"
	"sync"
)

// ErrNegativeCapacityHint is returned by New when the initial capacity hint
// is negative.
var ErrNegativeCapacityHint = errors.New("outlierdetector: capacity hint must not be negative")

// BackendStats holds the passive health-check counters for one backend
// address.
type BackendStats struct {
	// Failures is the total number of recorded failures, ever.
	Failures int
	// Successes is the total number of recorded successes, ever.
	Successes int
	// ConsecutiveFailures is the current run of failures since the last
	// recorded success. RecordSuccess resets it to zero.
	ConsecutiveFailures int
}

// Registry tracks BackendStats per address.
//
// Registry is safe for concurrent use by multiple goroutines: every method
// takes an internal mutex covering the whole map, so concurrent request
// handlers may record outcomes for the same or different backends without
// external synchronization.
type Registry struct {
	mu    sync.Mutex
	stats map[string]BackendStats
}

// New returns an empty Registry. capacityHint presizes the internal map for
// the expected number of distinct backends, avoiding repeated grow-and-
// rehash as addresses are first observed; it returns
// ErrNegativeCapacityHint if capacityHint is negative. A capacityHint of 0
// is valid and simply omits the size hint.
func New(capacityHint int) (*Registry, error) {
	if capacityHint < 0 {
		return nil, fmt.Errorf("%w: got %d", ErrNegativeCapacityHint, capacityHint)
	}
	return &Registry{stats: make(map[string]BackendStats, capacityHint)}, nil
}

// RecordFailure records one failed request against addr, incrementing both
// the total failure count and the consecutive-failure streak. The first
// call for an address creates its entry starting from the zero value.
func (r *Registry) RecordFailure(addr string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// A map element is not addressable: r.stats[addr].Failures++ does not
	// compile. Read the current value out, mutate the copy, then write the
	// copy back -- all three steps, in that order, every time.
	s := r.stats[addr]
	s.Failures++
	s.ConsecutiveFailures++
	r.stats[addr] = s
}

// RecordSuccess records one successful request against addr, incrementing
// the total success count and resetting the consecutive-failure streak to
// zero. The first call for an address creates its entry starting from the
// zero value.
func (r *Registry) RecordSuccess(addr string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	s := r.stats[addr]
	s.Successes++
	s.ConsecutiveFailures = 0
	r.stats[addr] = s
}

// Stats returns the current BackendStats for addr and whether addr has ever
// been recorded. The returned value is an independent copy; mutating it
// does not affect the Registry.
func (r *Registry) Stats(addr string) (BackendStats, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	s, ok := r.stats[addr]
	return s, ok
}

// IsEjected reports whether addr's current consecutive-failure streak has
// reached threshold. An address that has never been recorded is never
// ejected, regardless of threshold. A non-positive threshold is satisfied
// by any recorded address, including one whose streak is currently zero,
// since 0 >= a non-positive threshold; callers should pass a threshold of
// at least 1.
func (r *Registry) IsEjected(addr string, threshold int) bool {
	s, ok := r.Stats(addr)
	if !ok {
		return false
	}
	return s.ConsecutiveFailures >= threshold
}
```

### Using it

Construct one `Registry` per proxy process with `New`, sized to roughly the
number of backends the service discovery layer reports, and call
`RecordFailure` or `RecordSuccess` from wherever the proxy observes a
proxied request's outcome -- typically a response interceptor or a transport
round-tripper. `Stats` and `IsEjected` are the read side a load-balancing
loop polls before picking a backend. Every method locks internally, so a
single shared `*Registry` is safe to hand to every request-handling
goroutine; nothing about its use requires the caller to add their own
synchronization.

`Stats` returns a copy, never a reference into the internal map -- there
would be nothing to alias even if it wanted to, since map elements are not
addressable in the first place. A caller that wants a point-in-time read
before deciding whether to route around a backend gets exactly that: a
snapshot that will not change under them even if another goroutine calls
`RecordFailure` for the same address a moment later.

`ExampleRegistry` in the `_test.go` is the runnable demonstration of the
whole API; `go test` executes it and compares its output against the
`// Output:` comment:

```go
r, err := New(4)
if err != nil {
	panic(err)
}
const addr = "10.0.0.7:8080"

for range 2 {
	r.RecordFailure(addr)
}
fmt.Println("ejected after 2 failures (threshold 3):", r.IsEjected(addr, 3))

r.RecordFailure(addr)
fmt.Println("ejected after 3 failures (threshold 3):", r.IsEjected(addr, 3))

r.RecordSuccess(addr)
s, _ := r.Stats(addr)
fmt.Printf("after recovery: failures=%d consecutive=%d\n", s.Failures, s.ConsecutiveFailures)
```

### Tests

`TestRecordFailureAndSuccess` walks a failure/recovery/failure sequence and
checks both counters and the streak reset at each step, including that a
failure after a recovery restarts the streak at 1 rather than continuing
from wherever it left off. `TestIsEjected` is the ejection table: below
threshold, exactly at threshold, above threshold, and an address that was
never recorded at all, which must never be ejected regardless of the
threshold passed.

`TestBuggyRecordFailureDropsEveryUpdate` is the heart of the module.
`recordFailureBuggy` is unexported and unreachable from the package API; the
test calls it five times against a fresh map and asserts the map ends up
with *zero* entries -- not a wrong count for `addr`, no entry for it at all,
because the store-back that would have created one never runs. The same
five calls through `Registry.RecordFailure` are asserted to produce exactly
`Failures=5, ConsecutiveFailures=5`. `TestRegistryIsSafeForConcurrentUse`
drives many goroutines incrementing the same address concurrently and checks
the final count is exact, catching either a missing lock or a broken
store-back under `-race`.

Create `detector_test.go`:

```go
package outlierdetector

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// recordFailureBuggy is the mutation a first draft of RecordFailure tends to
// ship with: it copies the entry out and mutates the copy, exactly like the
// real version, and then simply never writes the copy back. It compiles
// without error -- s is a valid, addressable local variable -- and every
// call silently discards its own update. It is never exported and never
// reachable from Registry; it exists so the tests can pin the defect.
func recordFailureBuggy(stats map[string]BackendStats, addr string) {
	s := stats[addr]
	s.Failures++
	s.ConsecutiveFailures++
	// BUG: no "stats[addr] = s" -- the mutated copy is discarded here.
}

func TestNewRejectsNegativeCapacityHint(t *testing.T) {
	t.Parallel()

	if _, err := New(-1); !errors.Is(err, ErrNegativeCapacityHint) {
		t.Fatalf("New(-1) error = %v, want ErrNegativeCapacityHint", err)
	}
}

func TestRecordFailureAndSuccess(t *testing.T) {
	t.Parallel()

	r, err := New(0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const addr = "10.0.0.9:9000"

	if _, ok := r.Stats(addr); ok {
		t.Fatalf("Stats(%q) before any record: ok = true, want false", addr)
	}

	r.RecordFailure(addr)
	r.RecordFailure(addr)
	r.RecordFailure(addr)
	s, ok := r.Stats(addr)
	if !ok {
		t.Fatalf("Stats(%q) after failures: ok = false, want true", addr)
	}
	if s.Failures != 3 || s.ConsecutiveFailures != 3 {
		t.Fatalf("Stats(%q) = %+v, want Failures=3 ConsecutiveFailures=3", addr, s)
	}

	r.RecordSuccess(addr)
	s, _ = r.Stats(addr)
	if s.Failures != 3 || s.Successes != 1 || s.ConsecutiveFailures != 0 {
		t.Fatalf("Stats(%q) after success = %+v, want Failures=3 Successes=1 ConsecutiveFailures=0", addr, s)
	}

	r.RecordFailure(addr)
	s, _ = r.Stats(addr)
	if s.ConsecutiveFailures != 1 {
		t.Fatalf("ConsecutiveFailures after post-success failure = %d, want 1 (streak restarted)", s.ConsecutiveFailures)
	}
}

func TestIsEjected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		failures  int
		threshold int
		record    bool // whether the address is recorded at all
		want      bool
	}{
		{name: "below threshold", failures: 2, threshold: 3, record: true, want: false},
		{name: "exactly at threshold", failures: 3, threshold: 3, record: true, want: true},
		{name: "above threshold", failures: 5, threshold: 3, record: true, want: true},
		{name: "unknown address never ejected", failures: 0, threshold: 1, record: false, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r, err := New(0)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			const addr = "backend"
			if tc.record {
				for range tc.failures {
					r.RecordFailure(addr)
				}
			}
			if got := r.IsEjected(addr, tc.threshold); got != tc.want {
				t.Fatalf("IsEjected(%q, %d) = %v, want %v", addr, tc.threshold, got, tc.want)
			}
		})
	}
}

// TestBuggyRecordFailureDropsEveryUpdate is the heart of the module.
// recordFailureBuggy mutates a copy of the map entry and never writes it
// back, so after five calls the map has gained no entry for addr at all --
// not a wrong count, no entry whatsoever. The real Registry, given the same
// five calls through RecordFailure, tracks the count exactly.
func TestBuggyRecordFailureDropsEveryUpdate(t *testing.T) {
	t.Parallel()

	const addr = "10.0.0.5:9000"
	stats := make(map[string]BackendStats)
	for range 5 {
		recordFailureBuggy(stats, addr)
	}
	if len(stats) != 0 {
		t.Fatalf("buggy map has %d entries, want 0: every update was discarded, none ever created an entry: %+v", len(stats), stats)
	}

	r, err := New(0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for range 5 {
		r.RecordFailure(addr)
	}
	got, ok := r.Stats(addr)
	if !ok {
		t.Fatalf("Registry.Stats(%q): ok = false, want true", addr)
	}
	if got.Failures != 5 || got.ConsecutiveFailures != 5 {
		t.Fatalf("Registry.Stats(%q) = %+v, want Failures=5 ConsecutiveFailures=5", addr, got)
	}
}

func TestRegistryIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	r, err := New(0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const addr = "shared-backend"
	const goroutines, perGoroutine = 20, 50

	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perGoroutine {
				r.RecordFailure(addr)
			}
		}()
	}
	wg.Wait()

	got, ok := r.Stats(addr)
	if !ok {
		t.Fatalf("Stats(%q): ok = false, want true", addr)
	}
	want := goroutines * perGoroutine
	if got.Failures != want {
		t.Fatalf("Failures = %d, want %d (a lost update indicates a missing write-back or a race)", got.Failures, want)
	}
}

// ExampleRegistry demonstrates the health-check counters end to end: a
// backend that fails, recovers, then fails again, checked against an
// ejection threshold after each step.
func ExampleRegistry() {
	r, err := New(4)
	if err != nil {
		panic(err)
	}
	const addr = "10.0.0.7:8080"

	for range 2 {
		r.RecordFailure(addr)
	}
	fmt.Println("ejected after 2 failures (threshold 3):", r.IsEjected(addr, 3))

	r.RecordFailure(addr)
	fmt.Println("ejected after 3 failures (threshold 3):", r.IsEjected(addr, 3))

	r.RecordSuccess(addr)
	s, _ := r.Stats(addr)
	fmt.Printf("after recovery: failures=%d consecutive=%d\n", s.Failures, s.ConsecutiveFailures)

	// Output:
	// ejected after 2 failures (threshold 3): false
	// ejected after 3 failures (threshold 3): true
	// after recovery: failures=3 consecutive=0
}
```

## Review

The detector is correct when every recorded outcome is reflected in
`Stats`, and that depends entirely on the three-step copy-out/mutate/store-
back pattern being complete in `RecordFailure` and `RecordSuccess`. `New`
rejects a negative capacity hint with `ErrNegativeCapacityHint`, checkable
with `errors.Is`. The trap this module isolates is the version that compiles
and runs and does nothing: `recordFailureBuggy` mutates a copy the compiler
is perfectly happy with and then never assigns it back, so the map gains no
entry at all -- the tests pin that as zero entries, not a wrong count, which
is the sharper and more surprising failure. `IsEjected` treats an unrecorded
address as healthy by definition, never ejecting a backend the detector has
not actually observed. `Registry` is safe for concurrent use, guarded by a
single mutex covering the whole map, and `ExampleRegistry` is the executable
documentation `go test` verifies. Run `go test -count=1 -race ./...`.

## Resources

- [Go Specification: Address operators](https://go.dev/ref/spec#Address_operators) — the rule that a map index expression is not addressable.
- [Go Specification: Index expressions](https://go.dev/ref/spec#Index_expressions) — why `m[k]` yields a value, not a location.
- [Envoy: Outlier detection](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/upstream/outlier) — the passive health check this module's shape is drawn from.
- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — the lock guarding the shared map.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-sharded-connection-pool-registry.md](11-sharded-connection-pool-registry.md) | Next: [13-idempotency-key-dedup-filter.md](13-idempotency-key-dedup-filter.md)
