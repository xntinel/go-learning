# Exercise 9: Degraded-Mode Feature-Flag Bitmask with atomic Or/And

When a service is drowning during an incident, the runbook says: shed optional
features one by one — kill the recommendations rail, pause async enrichment, force
cache-only reads — and log each transition exactly once. One `atomic.Uint32` holds
all the degraded-feature bits, and the Go 1.23 `Or`/`And` methods flip them
race-free while their OLD-value return tells the winning caller it caused the
transition.

This module is fully self-contained.

## What you'll build

```text
featuremask/               independent module: example.com/featuremask
  go.mod
  mask.go                  type Flag; type Mask over atomic.Uint32; Set (Or), Clear (And), Enabled (Load), SnapshotNames
  middleware.go            Degradable: http middleware that 503s a route while its feature bit is degraded
  cmd/
    demo/
      main.go              flips degraded modes and shows the route behavior change
  mask_test.go             disjoint-bit storm, exactly-one-winner, middleware round-trip, Example
```

- Files: `mask.go`, `middleware.go`, `cmd/demo/main.go`, `mask_test.go`.
- Implement: a `Mask` over `atomic.Uint32` with named `Flag` bits; `Set` via `Or` reporting whether this call flipped the bit, `Clear` via `And(^flag)`, `Enabled` via `Load`, `SnapshotNames` for the health endpoint; a `Degradable` middleware that short-circuits a route while its bit is set.
- Test: a concurrent Set/Clear storm on disjoint bits loses no updates; exactly one of 100 racing `Set` callers gets `true`; httptest proves 200 normally and 503 while degraded; an Example pins `SnapshotNames`.
- Verify: `go test -count=1 -race ./...`

### Why a bitmask, and why Or/And instead of Load-then-Store

The state here is genuinely one value: a set of independent boolean bits packed
into a `uint32`. Packing them buys two things a `map[string]bool` under a mutex
cannot give you. First, every request-path check (`Enabled`) is a single wait-free
`Load` — this code runs on every request to every degradable route, so it must
cost nothing. Second, the health endpoint reads the *entire* degraded set as one
coherent point-in-time value with one `Load`; a map read under a mutex gives you
the same coherence but makes every hot-path check pay for the lock.

The update side is where the classic bug lives. The tempting
`m.Store(m.Load() | bit)` is a load-then-store race: operator A degrading
recommendations and operator B degrading enrichment can interleave so that one of
the two bits is silently lost — B loads before A stores, then B's store overwrites
A's bit. Before Go 1.23 the fix was a CAS retry loop; since Go 1.23 the integer
wrappers have `Or` and `And`, which perform the read-modify-write as one atomic
step. `Set` is `Or(bit)`; `Clear` is `And(^bit)` — AND with the complement zeroes
exactly that bit and preserves every other.

Both methods return the OLD value, and that return is the senior payoff of this
exercise. After `old := bits.Or(flag)`, the expression `old&flag == 0` is true for
exactly one of any number of concurrent callers — the one whose `Or` actually
flipped the bit from 0 to 1. That is the bitmask analogue of
`atomic.Bool.CompareAndSwap(false, true)`: a free transition detector. During an
incident, five automated triggers and two humans may all try to degrade
recommendations in the same second; every caller gets the degraded state it asked
for, but only the winner logs "recommendations degraded", fires the alert, and
stamps the incident timeline. Without the old value you either log seven times or
you bolt a mutex onto a lock-free design just to deduplicate the log line.

The middleware is deliberately boring: check the bit, and if degraded answer
`503 Service Unavailable` with an `X-Degraded-Feature` header so callers and
dashboards can tell load shedding from a crash. Failing closed with a 503 is the
right default for a feature whose absence changes semantics; a team that prefers
failing open (empty recommendations, stale cache) swaps the short-circuit body —
the bit machinery is identical.

One boundary to respect: the mask is one value, so flipping *several* flags
"together" with two `Or` calls is two transitions — a reader can observe the gap.
Here that is fine (each feature degrades independently); if you ever need an
all-or-nothing multi-flag switch, set all the bits in a single `Or` with the
combined mask — still one atomic step.

Create `mask.go`:

```go
package featuremask

import "sync/atomic"

// Flag identifies one degradable feature as a single bit in the mask.
type Flag uint32

const (
	// FlagRecommendations degrades the personalized recommendations rail.
	FlagRecommendations Flag = 1 << iota
	// FlagAsyncEnrichment pauses background enrichment of new records.
	FlagAsyncEnrichment
	// FlagCacheOnlyReads forces reads to be served from cache, skipping the
	// primary store.
	FlagCacheOnlyReads
)

// flagNames orders the known flags for SnapshotNames. Keep in sync with the
// constants above.
var flagNames = []struct {
	flag Flag
	name string
}{
	{FlagRecommendations, "recommendations"},
	{FlagAsyncEnrichment, "async-enrichment"},
	{FlagCacheOnlyReads, "cache-only-reads"},
}

// Mask tracks which features are currently degraded. The zero value is ready
// to use: nothing degraded. Do not copy a Mask after first use.
type Mask struct {
	bits atomic.Uint32
}

// Set marks flag as degraded. It reports whether THIS call flipped the bit:
// exactly one of any number of concurrent Set calls for the same flag gets
// true, so the winner can log the transition exactly once.
func (m *Mask) Set(flag Flag) bool {
	old := m.bits.Or(uint32(flag))
	return old&uint32(flag) == 0
}

// Clear restores flag to normal operation. It reports whether this call
// flipped the bit back, with the same exactly-one-winner guarantee as Set.
func (m *Mask) Clear(flag Flag) bool {
	old := m.bits.And(^uint32(flag))
	return old&uint32(flag) != 0
}

// Enabled reports whether the feature is running normally (bit not set).
// It is a single wait-free Load, cheap enough for every request.
func (m *Mask) Enabled(flag Flag) bool {
	return m.bits.Load()&uint32(flag) == 0
}

// SnapshotNames returns the names of the currently degraded features in
// declaration order, for the health endpoint. It loads the mask once, so the
// result is one coherent point-in-time view of the whole set.
func (m *Mask) SnapshotNames() []string {
	bits := m.bits.Load()
	names := []string{}
	for _, fn := range flagNames {
		if bits&uint32(fn.flag) != 0 {
			names = append(names, fn.name)
		}
	}
	return names
}
```

Create `middleware.go`:

```go
package featuremask

import "net/http"

// Degradable wraps next and short-circuits it with 503 Service Unavailable
// while flag is degraded in m. The X-Degraded-Feature header lets clients and
// dashboards distinguish deliberate load shedding from a failure.
func Degradable(m *Mask, flag Flag, name string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !m.Enabled(flag) {
			w.Header().Set("X-Degraded-Feature", name)
			http.Error(w, "feature temporarily degraded: "+name, http.StatusServiceUnavailable)
			return
		}
		next.ServeHTTP(w, r)
	})
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/featuremask"
)

func main() {
	var mask featuremask.Mask

	recs := featuremask.Degradable(&mask, featuremask.FlagRecommendations, "recommendations",
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintln(w, `["item-42","item-7"]`)
		}))

	hit := func() {
		rec := httptest.NewRecorder()
		recs.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/recommendations", nil))
		fmt.Printf("GET /recommendations -> %d %s", rec.Code, rec.Body.String())
	}

	fmt.Println("degraded:", mask.SnapshotNames())
	hit()

	if mask.Set(featuremask.FlagRecommendations) {
		fmt.Println("incident: recommendations degraded (winner logs exactly once)")
	}
	mask.Set(featuremask.FlagCacheOnlyReads)
	fmt.Println("degraded:", mask.SnapshotNames())
	hit()

	mask.Clear(featuremask.FlagRecommendations)
	fmt.Println("degraded:", mask.SnapshotNames())
	hit()
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
degraded: []
GET /recommendations -> 200 ["item-42","item-7"]
incident: recommendations degraded (winner logs exactly once)
degraded: [recommendations cache-only-reads]
GET /recommendations -> 503 feature temporarily degraded: recommendations
degraded: [cache-only-reads]
GET /recommendations -> 200 ["item-42","item-7"]
```

### Tests

`TestNoLostBits` is the regression test for the load-then-store race: one
goroutine per flag hammers Set/Clear on its own bit and finishes with a final
`Set`. Because every bit ends set, any cross-bit lost update — the exact failure
mode of `Store(Load()|bit)` — makes the final assertion fail. `TestExactlyOneSetWinner`
races 100 goroutines on one flag and counts `true` returns. The middleware test
round-trips 200, 503, 200 through `httptest`.

Create `mask_test.go`:

```go
package featuremask

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

func TestSetClearEnabled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		run         func(m *Mask) bool
		wantRet     bool
		wantEnabled bool
	}{
		{"first set flips", func(m *Mask) bool { return m.Set(FlagRecommendations) }, true, false},
		{"second set is a no-op", func(m *Mask) bool { m.Set(FlagRecommendations); return m.Set(FlagRecommendations) }, false, false},
		{"clear after set flips back", func(m *Mask) bool { m.Set(FlagRecommendations); return m.Clear(FlagRecommendations) }, true, true},
		{"clear when unset is a no-op", func(m *Mask) bool { return m.Clear(FlagRecommendations) }, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var m Mask
			if got := tc.run(&m); got != tc.wantRet {
				t.Fatalf("returned %v, want %v", got, tc.wantRet)
			}
			if got := m.Enabled(FlagRecommendations); got != tc.wantEnabled {
				t.Fatalf("Enabled = %v, want %v", got, tc.wantEnabled)
			}
		})
	}
}

func TestNoLostBits(t *testing.T) {
	t.Parallel()

	flags := []Flag{FlagRecommendations, FlagAsyncEnrichment, FlagCacheOnlyReads}
	var m Mask
	var wg sync.WaitGroup
	for _, f := range flags {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 2000 {
				m.Set(f)
				m.Clear(f)
			}
			m.Set(f)
		}()
	}
	wg.Wait()

	for _, f := range flags {
		if m.Enabled(f) {
			t.Errorf("flag %b lost its final Set under concurrency", f)
		}
	}
	if got := len(m.SnapshotNames()); got != len(flags) {
		t.Fatalf("SnapshotNames reports %d degraded flags, want %d", got, len(flags))
	}
}

func TestExactlyOneSetWinner(t *testing.T) {
	t.Parallel()

	var m Mask
	var winners atomic.Int32
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if m.Set(FlagAsyncEnrichment) {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := winners.Load(); got != 1 {
		t.Fatalf("%d Set callers reported flipping the bit, want exactly 1", got)
	}
	if m.Enabled(FlagAsyncEnrichment) {
		t.Fatal("flag not degraded after 100 Set calls")
	}
}

func TestDegradableMiddleware(t *testing.T) {
	t.Parallel()

	var m Mask
	h := Degradable(&m, FlagRecommendations, "recommendations",
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

	get := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/recommendations", nil))
		return rec
	}

	if rec := get(); rec.Code != http.StatusOK {
		t.Fatalf("healthy route returned %d, want 200", rec.Code)
	}

	m.Set(FlagRecommendations)
	rec := get()
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("degraded route returned %d, want 503", rec.Code)
	}
	if got := rec.Header().Get("X-Degraded-Feature"); got != "recommendations" {
		t.Fatalf("X-Degraded-Feature = %q, want %q", got, "recommendations")
	}

	m.Clear(FlagRecommendations)
	if rec := get(); rec.Code != http.StatusOK {
		t.Fatalf("restored route returned %d, want 200", rec.Code)
	}
}

func ExampleMask_SnapshotNames() {
	var m Mask
	m.Set(FlagAsyncEnrichment)
	m.Set(FlagCacheOnlyReads)
	fmt.Println(m.SnapshotNames())
	// Output: [async-enrichment cache-only-reads]
}
```

## Review

The mask is correct when no concurrent bit update is ever lost and exactly one
racing `Set` caller learns it caused the transition — `TestNoLostBits` and
`TestExactlyOneSetWinner` under `-race` prove both, and they are exactly the tests
a `Store(Load()|bit)` implementation fails. The mistakes to avoid: reconstructing
the mask with load-then-store instead of `Or`/`And`; discarding the OLD return
value and losing the free transition detector (then logging the degradation once
per caller); clearing with `And(flag)` instead of `And(^flag)`, which wipes every
*other* bit; and treating two `Or` calls as one transaction when a joint switch is
needed — combine the bits into a single `Or` mask instead. Remember `Or` and `And`
require Go 1.23+.

## Resources

- [`sync/atomic` package documentation](https://pkg.go.dev/sync/atomic) — `Uint32.Or`, `Uint32.And`, `Uint32.Load` signatures and the OLD-value return.
- [Go 1.23 release notes: sync/atomic And/Or](https://go.dev/doc/go1.23#minor_library_changes) — where the bitwise atomics were added.
- [The Go Memory Model](https://go.dev/ref/mem) — why the single atomic read-modify-write step closes the load-then-store race.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-snapshot-publisher-atomic-pointer.md](08-snapshot-publisher-atomic-pointer.md) | Next: [10-sharded-counter-false-sharing.md](10-sharded-counter-false-sharing.md)
