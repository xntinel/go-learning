# Exercise 8: Feature-Flag Middleware with Percentage Rollout

Feature flags earn their keep at the HTTP boundary: gate the new code path,
roll it out to 10 percent of users, watch the dashboards, then widen. This
exercise builds the middleware that does it — loading one config snapshot per
request so the whole request sees one consistent flag view, and deciding
percentage rollouts with a deterministic hash so a given user is in or out
consistently across every pod in the fleet.

This module is fully self-contained: its own `go mod init`, every type it
needs, its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
flagmw/                    independent module: example.com/flagmw
  go.mod
  flags.go                 Config, Flag, Manager; Bucket (fnv-1a), EnabledFor
  middleware.go            RequireFlag middleware: one snapshot per request, in context
  cmd/
    demo/
      main.go              runnable demo: four users against a 40 percent rollout, then 100
  flagmw_test.go           httptest flip tests, determinism test, distribution test
```

- Files: `flags.go`, `middleware.go`, `cmd/demo/main.go`, `flagmw_test.go`.
- Implement: `Bucket(flagName, userID)` hashing with `hash/fnv` (fnv-1a) modulo 100; `EnabledFor(cfg, flag, userID)`; `RequireFlag` middleware that Loads the snapshot once, stores it in the request context, and gates on the flag (404 when off, 400 when the user header is missing); `ConfigFrom(ctx)` for downstream handlers.
- Test: httptest-driven 404/200 switching when a flag flips via `Update` between requests; a determinism test (same user, same decision, 1000 calls); a distribution test (10000 distinct IDs at 30 percent land inside a fixed tolerance band — deterministic, so never flaky); a snapshot-consistency test proving a mid-request `Update` cannot change the request's view.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/15-sync-primitives/08-atomic-value-config-hot-reload/08-feature-flag-http-middleware/cmd/demo
cd go-solutions/15-sync-primitives/08-atomic-value-config-hot-reload/08-feature-flag-http-middleware
```

### Deterministic bucketing: why a hash and not a coin flip

A percentage rollout must answer one question the same way everywhere: *is
this user in the rollout?* A random draw per request gives a user the new
behavior on one page load and the old on the next, and gives support two
irreproducible bug reports. The standard answer is hash-based bucketing:
hash a stable user ID into one of 100 buckets, admit the user if their
bucket is below the rollout percentage. The hash is pure, so the decision is
identical on every request, every pod, every restart — raising the
percentage only ever *adds* users (buckets 0-9 are a subset of 0-29), so a
widening rollout never kicks anyone back out.

FNV-1a is the conventional choice: `hash/fnv` is in the stdlib, it is fast,
non-cryptographic (nothing here needs to resist an adversary), and it mixes
short keys like user IDs well enough that bucket counts land close to
uniform. One refinement that separates production flag systems from naive
ones: salt the hash with the *flag name*. Hash only the user ID and the same
10 percent of users become the guinea pigs for every experiment you ever
run — their experience degrades, and your experiments contaminate each
other. Hashing `flag + ":" + user` decorrelates the cohorts.

The middleware side carries the lesson's other discipline: `RequireFlag`
calls `m.Get()` exactly once per request and threads that snapshot into the
request context. Downstream code reads `ConfigFrom(ctx)` instead of touching
the manager, so even if a reload lands mid-request, the request's flag view
cannot mix two config versions — the same reason you never read a global
twice. Requests without the user-ID header get `400`: a percentage rollout
without a stable identity is unanswerable, and failing loudly beats silently
bucketing everyone as the empty string.

Create `flags.go`:

```go
// Package flagmw gates HTTP handlers on hot-reloadable feature flags with
// deterministic percentage rollouts: one config snapshot per request, one
// fnv-1a bucket per (flag, user) pair, fleet-wide consistency for free.
package flagmw

import (
	"hash/fnv"
	"sync/atomic"
)

// Flag configures one feature: a kill switch plus a rollout percentage.
type Flag struct {
	Enabled        bool
	RolloutPercent int // 0..100: admit users whose bucket is below this
}

// Config is one immutable configuration snapshot.
type Config struct {
	Flags   map[string]Flag
	Version int
}

// Manager publishes the current snapshot. Share by pointer; do not copy.
type Manager struct {
	ptr atomic.Pointer[Config]
}

// NewManager returns a Manager serving initial.
func NewManager(initial *Config) *Manager {
	m := &Manager{}
	m.ptr.Store(initial)
	return m
}

// Get returns the current snapshot (read-only, non-nil, lock-free).
func (m *Manager) Get() *Config {
	return m.ptr.Load()
}

// Update atomically replaces the snapshot; ownership of next transfers.
func (m *Manager) Update(next *Config) {
	m.ptr.Store(next)
}

// Bucket maps a (flag, user) pair to a stable bucket in [0, 100) using
// fnv-1a. Salting with the flag name decorrelates cohorts across flags:
// the users testing one experiment are not the users testing them all.
func Bucket(flagName, userID string) int {
	h := fnv.New32a()
	h.Write([]byte(flagName))
	h.Write([]byte{':'})
	h.Write([]byte(userID))
	return int(h.Sum32() % 100)
}

// EnabledFor reports whether the flag admits this user under cfg: the
// flag must be enabled and the user's bucket below the rollout
// percentage. The decision is a pure function of its inputs, so it is
// identical on every pod serving the same config version.
func EnabledFor(cfg *Config, flagName, userID string) bool {
	f, ok := cfg.Flags[flagName]
	if !ok || !f.Enabled {
		return false
	}
	if f.RolloutPercent >= 100 {
		return true
	}
	return Bucket(flagName, userID) < f.RolloutPercent
}
```

Create `middleware.go`:

```go
package flagmw

import (
	"context"
	"net/http"
)

// UserIDHeader carries the stable user identity rollout decisions hash.
const UserIDHeader = "X-User-ID"

type ctxKey struct{}

// ConfigFrom returns the snapshot RequireFlag attached to this request,
// or nil if the request did not pass through the middleware. Downstream
// handlers must use this — never the Manager — so a reload landing
// mid-request cannot split the request across two config versions.
func ConfigFrom(ctx context.Context) *Config {
	cfg, _ := ctx.Value(ctxKey{}).(*Config)
	return cfg
}

// RequireFlag gates next behind flagName. It loads one snapshot per
// request, decides admission from it, and threads the same snapshot into
// the request context for downstream flag checks. Missing identity is a
// 400; a disabled flag or an out-of-cohort user is a 404, indistinguishable
// from the route not existing.
func RequireFlag(m *Manager, flagName string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID := r.Header.Get(UserIDHeader)
		if userID == "" {
			http.Error(w, "missing "+UserIDHeader, http.StatusBadRequest)
			return
		}
		cfg := m.Get() // exactly one Load per request
		if !EnabledFor(cfg, flagName, userID) {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, cfg)))
	})
}
```

### The runnable demo

The demo puts four users against a 40 percent rollout and prints each user's
bucket next to the HTTP status the middleware returned — you can see the
threshold doing the deciding. Then it hot-reloads the flag to 100 percent and
the previously excluded users get in, without touching the handler.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/flagmw"
)

func main() {
	m := flagmw.NewManager(&flagmw.Config{
		Flags:   map[string]flagmw.Flag{"beta_search": {Enabled: true, RolloutPercent: 40}},
		Version: 1,
	})
	handler := flagmw.RequireFlag(m, "beta_search", http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintln(w, "new search")
		}))

	users := []string{"alice", "erin", "grace", "heidi"}
	probe := func(user string) int {
		req := httptest.NewRequest(http.MethodGet, "/search", nil)
		req.Header.Set(flagmw.UserIDHeader, user)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	fmt.Println("rollout at 40 percent:")
	for _, u := range users {
		fmt.Printf("  %-5s bucket=%2d status=%d\n", u, flagmw.Bucket("beta_search", u), probe(u))
	}

	m.Update(&flagmw.Config{
		Flags:   map[string]flagmw.Flag{"beta_search": {Enabled: true, RolloutPercent: 100}},
		Version: 2,
	})
	fmt.Println("rollout at 100 percent:")
	for _, u := range users {
		fmt.Printf("  %-5s status=%d\n", u, probe(u))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
rollout at 40 percent:
  alice bucket=22 status=200
  erin  bucket=62 status=404
  grace bucket=30 status=200
  heidi bucket=75 status=404
rollout at 100 percent:
  alice status=200
  erin  status=200
  grace status=200
  heidi status=200
```

### Tests

`TestFlagFlipSwitchesResponses` is the hot-reload path through `httptest`:
the same request goes from 404 to 200 to 404 as `Update` lands between
requests. `TestRolloutDeterministic` calls the decision 1000 times for one
user and requires a single answer. `TestRolloutDistribution` checks 10000
distinct IDs at 30 percent land inside a fixed band — this looks statistical
but is fully deterministic (fixed IDs, fixed hash), so once green it is
green forever. `TestSnapshotConsistentWithinRequest` is the discipline test:
an `Update` fired *inside* the handler must not change what `ConfigFrom`
returns for the in-flight request.

Create `flagmw_test.go`:

```go
package flagmw

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func flagConfig(percent int, version int) *Config {
	return &Config{
		Flags:   map[string]Flag{"beta_search": {Enabled: true, RolloutPercent: percent}},
		Version: version,
	}
}

func probe(t *testing.T, h http.Handler, userID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/search", nil)
	if userID != "" {
		req.Header.Set(UserIDHeader, userID)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func TestFlagFlipSwitchesResponses(t *testing.T) {
	t.Parallel()

	m := NewManager(&Config{
		Flags:   map[string]Flag{"beta_search": {Enabled: false}},
		Version: 1,
	})
	h := RequireFlag(m, "beta_search", okHandler())

	if got := probe(t, h, "alice").Code; got != http.StatusNotFound {
		t.Fatalf("disabled flag: status = %d, want 404", got)
	}

	m.Update(flagConfig(100, 2))
	if got := probe(t, h, "alice").Code; got != http.StatusOK {
		t.Fatalf("enabled flag: status = %d, want 200", got)
	}

	m.Update(&Config{
		Flags:   map[string]Flag{"beta_search": {Enabled: false}},
		Version: 3,
	})
	if got := probe(t, h, "alice").Code; got != http.StatusNotFound {
		t.Fatalf("re-disabled flag: status = %d, want 404", got)
	}
}

func TestMissingUserIDRejected(t *testing.T) {
	t.Parallel()

	m := NewManager(flagConfig(50, 1))
	h := RequireFlag(m, "beta_search", okHandler())

	if got := probe(t, h, "").Code; got != http.StatusBadRequest {
		t.Fatalf("missing user header: status = %d, want 400", got)
	}
}

func TestUnknownFlagIsOff(t *testing.T) {
	t.Parallel()

	m := NewManager(flagConfig(100, 1))
	h := RequireFlag(m, "no_such_flag", okHandler())

	if got := probe(t, h, "alice").Code; got != http.StatusNotFound {
		t.Fatalf("unknown flag: status = %d, want 404", got)
	}
}

func TestRolloutDeterministic(t *testing.T) {
	t.Parallel()

	cfg := flagConfig(50, 1)
	first := EnabledFor(cfg, "beta_search", "user-42")
	for range 1000 {
		if got := EnabledFor(cfg, "beta_search", "user-42"); got != first {
			t.Fatalf("decision flapped: was %v, now %v", first, got)
		}
	}
}

func TestWideningRolloutNeverEvicts(t *testing.T) {
	t.Parallel()

	// Every user admitted at 30 percent must still be admitted at 60:
	// bucket < 30 implies bucket < 60. This is the property that lets a
	// rollout be widened without churning users' experience.
	at30, at60 := flagConfig(30, 1), flagConfig(60, 2)
	for i := range 1000 {
		user := fmt.Sprintf("user-%d", i)
		if EnabledFor(at30, "beta_search", user) && !EnabledFor(at60, "beta_search", user) {
			t.Fatalf("user %s admitted at 30 percent but evicted at 60", user)
		}
	}
}

func TestRolloutDistribution(t *testing.T) {
	t.Parallel()

	// Deterministic, not statistical: fixed IDs and a fixed hash give the
	// same count on every run. The band checks fnv-1a spreads these IDs
	// roughly uniformly: 10000 users at 30 percent, expect about 3000.
	cfg := flagConfig(30, 1)
	admitted := 0
	for i := range 10000 {
		if EnabledFor(cfg, "beta_search", fmt.Sprintf("user-%d", i)) {
			admitted++
		}
	}
	if admitted < 2700 || admitted > 3300 {
		t.Fatalf("admitted %d of 10000 at 30 percent; want within [2700, 3300]", admitted)
	}
}

func TestSnapshotConsistentWithinRequest(t *testing.T) {
	t.Parallel()

	m := NewManager(flagConfig(100, 1))

	var before, after int
	h := RequireFlag(m, "beta_search", http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			before = ConfigFrom(r.Context()).Version
			m.Update(flagConfig(100, 2)) // reload lands mid-request
			after = ConfigFrom(r.Context()).Version
		}))

	if got := probe(t, h, "alice").Code; got != http.StatusOK {
		t.Fatalf("status = %d", got)
	}
	if before != 1 || after != 1 {
		t.Fatalf("request saw versions %d then %d; want 1 then 1 (one snapshot per request)", before, after)
	}
	if m.Get().Version != 2 {
		t.Fatalf("manager Version = %d; the update itself must land", m.Get().Version)
	}
}

func ExampleEnabledFor() {
	cfg := &Config{Flags: map[string]Flag{"beta_search": {Enabled: true, RolloutPercent: 100}}}
	fmt.Println(EnabledFor(cfg, "beta_search", "alice"))
	fmt.Println(EnabledFor(cfg, "other_flag", "alice"))
	// Output:
	// true
	// false
}
```

## Review

Three properties make this middleware production-grade, and each has a test
whose failure diagnoses a specific regression. Determinism
(`TestRolloutDeterministic`): if it fails, someone introduced randomness or
per-request state into the decision. Monotone widening
(`TestWideningRolloutNeverEvicts`): if it fails, the bucketing changed shape
— for example someone "improved" the hash or re-salted it, which silently
reshuffles every live cohort; treat the hash function and salt as a frozen
wire format once a rollout is in flight. Snapshot-per-request
(`TestSnapshotConsistentWithinRequest`): if it fails, some code path is
reading the manager instead of the context, reintroducing the
two-versions-in-one-request bug.

The distribution test deserves a word of honesty: it validates that fnv-1a
spreads *these synthetic IDs* acceptably, with a generous band. Real user-ID
shapes (UUIDs, emails, numeric IDs sharing long prefixes) can distribute
differently, and a real flag system measures its live bucket skew rather
than assuming it. Verify with `go test -count=1 -race ./...`.

## Resources

- [hash/fnv](https://pkg.go.dev/hash/fnv) — FNV-1a, the stdlib's fast non-cryptographic hash.
- [net/http/httptest](https://pkg.go.dev/net/http/httptest) — NewRequest and NewRecorder driving every test here.
- [net/http: Handler and HandlerFunc](https://pkg.go.dev/net/http#Handler) — the middleware contract.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-reload-subscriber-fanout.md](07-reload-subscriber-fanout.md) | Next: [09-config-observability-endpoint.md](09-config-observability-endpoint.md)
