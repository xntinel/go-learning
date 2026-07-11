# Exercise 8: Per-tenant schema migrator: one Once per key, errors cached per key

A multi-tenant service must run each tenant's schema migration exactly once per
process — not once total, and not once per call. That is a *per-key*
exactly-once contract, and a single global `sync.Once` gets it catastrophically
wrong: the first tenant migrates and every other tenant silently never does.
This exercise builds the correct shape: a mutex-guarded map of per-key `Once`
entries, with the map lock released before `Do` so distinct tenants migrate
concurrently.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
per-key-migration-once/       module: example.com/per-key-migration-once
  go.mod
  migrate.go                  ErrMigration; type entry, Migrator; New,
                              EnsureMigrated, Tenants
  cmd/
    demo/
      main.go                 runnable demo: dedup per tenant, per-key failure
  migrate_test.go             exactly-once per key, real concurrency between
                              keys, per-key error caching, Example
```

- Files: `migrate.go`, `cmd/demo/main.go`, `migrate_test.go`.
- Implement: `EnsureMigrated(tenantID)` backed by `map[string]*entry` where each entry holds its own `sync.Once` and cached error; the map mutex covers only the entry lookup/insert, never the migration itself; failures wrap `ErrMigration` and are cached per key without poisoning other tenants.
- Test: per-tenant atomic run counters all equal 1 under N goroutines x K tenants; two slow tenants provably overlap (channel handshake, no sleeps); a failing tenant returns the identical cached error on every later call while a healthy tenant still succeeds.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p per-key-migration-once/cmd/demo
cd per-key-migration-once
go mod init example.com/per-key-migration-once
```

### Three contracts, one keyword: granularity is the design decision

"Run the migration once" hides a question: once per *what*? There are three
different answers, and picking the wrong one is an outage, not a style issue.

One global `Once` means once per *process*. Correct for one shared resource —
one pool, one TLS config. For a per-tenant migration it means tenant `acme`
migrates because it happened to arrive first, and tenants `globex` and `initech`
never do; their requests then fail on missing tables with no error anywhere
near the migration code.

`golang.org/x/sync/singleflight` means once per *in-flight call*. Concurrent
callers of the same key coalesce, but the result is not cached: after the
flight completes, the next call runs the migration again. That is exactly right
for dedup of idempotent-but-expensive reads (cache fill, DNS lookup) and wrong
for a migration, which must not re-run every time the key goes quiet.
Conversely, `singleflight`'s not-caching is what makes failures naturally
retryable — with `Once`, a transient failure is cached forever unless you build
generational reset on top.

A map of per-key `Once` entries means once per *key per process*: each tenant
gets its own exactly-once guard and its own cached error. That is the migration
contract. The implementation has one load-bearing detail: the map mutex is held
only long enough to find-or-insert the entry, and is *released before* calling
`Do`. Hold it across `Do` and every tenant's migration serializes behind the
slowest one — a global lock wearing a per-key costume. Releasing first is safe
because the entry pointer, once in the map, is never replaced: concurrent
callers of the same tenant all reach the same `*entry` and its `Once` does the
per-key election; callers of different tenants hold different entries and
proceed in parallel.

The error policy is also per key. A failed migration wraps `ErrMigration` and
the cause, and that error is cached on that tenant's entry: every later call
for the broken tenant gets the same controlled failure (no retry storm against
a broken schema), while every other tenant is untouched. If you want bounded
retry for a failed tenant, you install a fresh `Once` per generation exactly as
in Exercise 5 — per key.

Create `migrate.go`:

```go
// Package migrate runs one schema migration per tenant per process: a
// mutex-guarded map of per-key sync.Once entries, with errors cached per key.
package migrate

import (
	"errors"
	"fmt"
	"sync"
)

// ErrMigration wraps any per-tenant migration failure.
var ErrMigration = errors.New("migrate: migration failed")

// entry is one tenant's exactly-once guard and cached outcome.
type entry struct {
	once sync.Once
	err  error
}

// Migrator runs a migration function exactly once per tenant ID. Distinct
// tenants migrate concurrently; concurrent callers of one tenant coalesce.
type Migrator struct {
	run func(tenantID string) error

	mu      sync.Mutex
	tenants map[string]*entry
}

// New returns a Migrator that calls run once per tenant.
func New(run func(tenantID string) error) *Migrator {
	return &Migrator{run: run, tenants: make(map[string]*entry)}
}

// EnsureMigrated runs the migration for tenantID if this process has not run
// it yet, and returns that tenant's cached outcome. The map lock covers only
// the entry lookup; the migration itself runs outside it, so different tenants
// do not serialize behind each other.
func (m *Migrator) EnsureMigrated(tenantID string) error {
	m.mu.Lock()
	e, ok := m.tenants[tenantID]
	if !ok {
		e = &entry{}
		m.tenants[tenantID] = e
	}
	m.mu.Unlock()

	e.once.Do(func() {
		if err := m.run(tenantID); err != nil {
			e.err = fmt.Errorf("%w: tenant %q: %w", ErrMigration, tenantID, err)
		}
	})
	return e.err
}

// Tenants reports how many tenant entries exist (migrated or failed).
func (m *Migrator) Tenants() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.tenants)
}
```

The read of `e.err` after `Do` is race-free for every caller — winner and
coalesced waiters alike — because the return of the closure synchronizes before
the return of every `Do` call on that entry's `Once`. That is the same
happens-before edge as the single-`Once` patterns, applied per key.

### The runnable demo

The demo migrates two healthy tenants (repeat calls are no-ops), then shows a
broken tenant caching its own failure without affecting the others.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	migrate "example.com/per-key-migration-once"
)

func main() {
	m := migrate.New(func(tenantID string) error {
		if tenantID == "corrupt-co" {
			return errors.New("relation tenants_corrupt_co.orders already exists")
		}
		fmt.Println("applying schema for", tenantID)
		return nil
	})

	for _, tenant := range []string{"acme", "acme", "globex", "acme"} {
		if err := m.EnsureMigrated(tenant); err != nil {
			fmt.Println("error:", err)
		}
	}

	err1 := m.EnsureMigrated("corrupt-co")
	err2 := m.EnsureMigrated("corrupt-co")
	fmt.Println("is ErrMigration:", errors.Is(err1, migrate.ErrMigration))
	fmt.Println("same cached error:", errors.Is(err2, migrate.ErrMigration) && err1.Error() == err2.Error())
	fmt.Println("healthy tenant unaffected:", m.EnsureMigrated("globex") == nil)
	fmt.Println("tenant entries:", m.Tenants())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
applying schema for acme
applying schema for globex
is ErrMigration: true
same cached error: true
healthy tenant unaffected: true
tenant entries: 3
```

### Tests

`TestExactlyOncePerTenant` is the contract test: many goroutines hammer a small
set of tenants and each tenant's atomic run counter ends at exactly 1.
`TestDistinctTenantsOverlap` proves the map lock is not held across `Do`: two
migrations block inside `run` at the same time, confirmed by a channel
handshake (each signals "entered", the test receives both before releasing
either — impossible if the migrations serialized). `TestFailureCachedPerKey`
asserts the failing tenant's error is stable and sentinel-wrapped while a
healthy tenant still succeeds.

Create `migrate_test.go`:

```go
package migrate

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func TestExactlyOncePerTenant(t *testing.T) {
	t.Parallel()

	tenants := []string{"acme", "globex", "initech", "umbrella", "hooli"}
	runs := make(map[string]*atomic.Int64, len(tenants))
	for _, id := range tenants {
		runs[id] = &atomic.Int64{}
	}

	m := New(func(tenantID string) error {
		runs[tenantID].Add(1)
		return nil
	})

	const goroutinesPerTenant = 20
	var wg sync.WaitGroup
	for _, id := range tenants {
		for range goroutinesPerTenant {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := m.EnsureMigrated(id); err != nil {
					t.Errorf("EnsureMigrated(%q): %v", id, err)
				}
			}()
		}
	}
	wg.Wait()

	for _, id := range tenants {
		if got := runs[id].Load(); got != 1 {
			t.Errorf("tenant %q migrated %d times, want exactly 1", id, got)
		}
	}
	if got := m.Tenants(); got != len(tenants) {
		t.Errorf("Tenants() = %d, want %d", got, len(tenants))
	}
}

func TestDistinctTenantsOverlap(t *testing.T) {
	t.Parallel()

	entered := make(chan string, 2)
	release := make(chan struct{})

	m := New(func(tenantID string) error {
		entered <- tenantID
		<-release
		return nil
	})

	var wg sync.WaitGroup
	for _, id := range []string{"slow-a", "slow-b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.EnsureMigrated(id); err != nil {
				t.Errorf("EnsureMigrated(%q): %v", id, err)
			}
		}()
	}

	// Both migrations are inside run at once: if the map lock were held
	// across Do, the second receive would deadlock and the test would fail
	// on timeout.
	first := <-entered
	second := <-entered
	if first == second {
		t.Fatalf("both entries came from %q; expected two distinct tenants in flight", first)
	}
	close(release)
	wg.Wait()
}

func TestFailureCachedPerKey(t *testing.T) {
	t.Parallel()

	cause := errors.New("column tenants_bad.plan does not exist")
	var badRuns atomic.Int64
	m := New(func(tenantID string) error {
		if tenantID == "bad" {
			badRuns.Add(1)
			return cause
		}
		return nil
	})

	err1 := m.EnsureMigrated("bad")
	err2 := m.EnsureMigrated("bad")

	tests := []struct {
		name string
		err  error
	}{
		{name: "first call", err: err1},
		{name: "second call cached", err: err2},
	}
	for _, tt := range tests {
		if !errors.Is(tt.err, ErrMigration) {
			t.Errorf("%s: err = %v, want ErrMigration", tt.name, tt.err)
		}
		if !errors.Is(tt.err, cause) {
			t.Errorf("%s: err = %v, want it to wrap the cause", tt.name, tt.err)
		}
	}
	if err1.Error() != err2.Error() {
		t.Errorf("cached error changed between calls: %v vs %v", err1, err2)
	}
	if got := badRuns.Load(); got != 1 {
		t.Errorf("failing tenant ran %d times, want 1 (failure cached per key)", got)
	}
	if err := m.EnsureMigrated("good"); err != nil {
		t.Errorf("healthy tenant poisoned by another tenant's failure: %v", err)
	}
}

func Example() {
	m := New(func(tenantID string) error {
		fmt.Println("migrating", tenantID)
		return nil
	})
	m.EnsureMigrated("acme")
	m.EnsureMigrated("acme")
	m.EnsureMigrated("globex")
	// Output:
	// migrating acme
	// migrating globex
}
```

## Review

The migrator is correct when three properties hold at once: exactly-once per
key (every run counter is 1), concurrency between keys (the handshake test
observes two migrations in flight simultaneously), and failure isolation (a
broken tenant's cached error never touches a healthy tenant). The overlap test
is the one worth internalizing — it fails by deadlock, not by flaking, if you
reintroduce the classic bug of holding the map mutex across `Do`. When
reviewing similar code in the wild, ask the granularity question explicitly:
once per process wants one `Once`; once per in-flight call wants
`singleflight`; once per key per process wants this map-of-entries shape.
Mixing them up either starves keys (global `Once`) or re-runs completed work
(`singleflight` for a migration). Run `go test -count=1 -race`.

## Resources

- [sync.Once — pkg.go.dev](https://pkg.go.dev/sync#Once)
- [x/sync/singleflight — pkg.go.dev](https://pkg.go.dev/golang.org/x/sync/singleflight)
- [The Go Memory Model: Once — go.dev](https://go.dev/ref/mem#once)

---

Prev: [07-once-registration-guard.md](07-once-registration-guard.md) | Back to [00-concepts.md](00-concepts.md) | Next: [09-lazy-tls-config.md](09-lazy-tls-config.md)
