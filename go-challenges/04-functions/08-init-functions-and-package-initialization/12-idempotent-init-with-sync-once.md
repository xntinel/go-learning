# Exercise 12: Idempotent Explicit Init() Guarded by sync.Once

**Nivel: Intermedio** — validacion rapida (un test corto).

Multiple subsystems in a service may each want to make sure a shared metrics
registry is seeded before they use it, without knowing whether some other
subsystem already did it. This exercise builds an exported `Init()` — not a
package `init()` — that any number of callers can invoke safely because
`sync.Once` collapses the real seeding work into a single run.

## What you'll build

```text
metrics/                   independent module: example.com/metrics
  go.mod                    module example.com/metrics
  metrics.go                 counters map, sync.Once-guarded Init, Inc, Value
  metrics_test.go            not-seeded-before-Init + concurrent idempotency + unknown counter
```

Files: `metrics.go`, `metrics_test.go`.
Implement: an exported `Init()` guarded by `sync.Once`, plus `Inc` and `Value` that call `Init()` defensively so callers never need to sequence setup.
Test: the registry is unseeded until the first call; many concurrent callers invoking `Init`/`Inc` still result in the seeding logic running exactly once.

Set up the module:

```bash
mkdir -p go-solutions/04-functions/08-init-functions-and-package-initialization/12-idempotent-init-with-sync-once
cd go-solutions/04-functions/08-init-functions-and-package-initialization/12-idempotent-init-with-sync-once
go mod edit -go=1.24
```

### Why an explicit Init(), not a package init()

A package `init()` runs automatically, exactly once, the moment the package
is imported — no caller controls when, and no caller can call it again to
"make sure". An exported `Init()` guarded by `sync.Once` flips that: any
subsystem can call it as many times as it wants, in whatever order its own
setup happens to run, and only the first call does real work. This is the
right shape when several independent parts of a program each depend on some
shared state being ready but none of them owns initialization order.

Create `metrics.go`:

```go
// Package metrics is a tiny in-memory counter registry seeded by an
// idempotent, explicitly-called Init, instead of a package init() function.
package metrics

import (
	"sync"
	"sync/atomic"
)

var (
	mu       sync.Mutex
	counters map[string]int64
	seedOnce sync.Once
	seedRuns atomic.Int64
)

// defaultCounters are the counters every caller expects to exist once the
// registry has been seeded.
var defaultCounters = []string{"requests_total", "errors_total", "retries_total"}

// Init seeds the counter set. Multiple independent subsystems can each call
// Init defensively during their own setup, in any order, without knowing
// whether another subsystem already did it: sync.Once guarantees the actual
// seeding work runs exactly once no matter how many times Init is called.
func Init() {
	seedOnce.Do(func() {
		seedRuns.Add(1)
		mu.Lock()
		defer mu.Unlock()
		counters = make(map[string]int64, len(defaultCounters))
		for _, name := range defaultCounters {
			counters[name] = 0
		}
	})
}

// SeedRuns reports how many times the seeding logic actually executed. Test
// observable only; production code has no need for it.
func SeedRuns() int64 { return seedRuns.Load() }

// Inc increments a counter by 1. It calls Init defensively so a caller never
// needs to sequence its own setup relative to another subsystem's.
func Inc(name string) {
	Init()
	mu.Lock()
	defer mu.Unlock()
	counters[name]++
}

// Value returns a counter's current value and whether the name is known.
func Value(name string) (int64, bool) {
	Init()
	mu.Lock()
	defer mu.Unlock()
	v, ok := counters[name]
	return v, ok
}
```

Create `metrics_test.go`:

```go
package metrics

import (
	"sync"
	"testing"
)

// TestNotSeededBeforeInit proves the registry is not built merely by
// importing the package. It runs first (source order, no t.Parallel) so no
// other test has called Init, Inc, or Value yet.
func TestNotSeededBeforeInit(t *testing.T) {
	if got := SeedRuns(); got != 0 {
		t.Fatalf("SeedRuns() = %d before any Init call; want 0", got)
	}
}

// TestInitIsIdempotentUnderConcurrency drives many concurrent callers into
// Init and Inc and asserts the seeding logic ran exactly once.
func TestInitIsIdempotentUnderConcurrency(t *testing.T) {
	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			Init()
			Inc("requests_total")
		}()
	}
	wg.Wait()

	if got := SeedRuns(); got != 1 {
		t.Fatalf("SeedRuns() = %d after %d concurrent Init calls; want 1", got, n)
	}
	got, ok := Value("requests_total")
	if !ok {
		t.Fatal("Value(\"requests_total\") reported unknown counter")
	}
	if got != int64(n) {
		t.Fatalf("requests_total = %d, want %d", got, n)
	}
}

func TestValueUnknownCounter(t *testing.T) {
	Init()
	if _, ok := Value("does_not_exist"); ok {
		t.Fatal("Value reported ok=true for an unseeded counter name")
	}
}
```

Verify: `go test -count=1 ./...`

## Review

`TestNotSeededBeforeInit` must run before any other test touches `Init`,
`Inc`, or `Value` — it relies on Go running tests within a file in source
order when none call `t.Parallel()`, the same technique the lazy-singleton
exercise earlier in this lesson uses. Once any test calls `Init`, `SeedRuns`
stays at 1 for the rest of the process, which is exactly the guarantee this
package exists to provide: any number of defensive `Init()` calls from any
number of subsystems still only seed the registry once.

## Resources

- [sync.Once](https://pkg.go.dev/sync#Once) — run exactly once, safe under concurrent callers.
- [sync/atomic](https://pkg.go.dev/sync/atomic) — the counter used here to observe how many times the guarded body ran.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-fail-fast-status-transition-table.md](11-fail-fast-status-transition-table.md) | Next: [13-constructor-refactor-for-parallel-tests.md](13-constructor-refactor-for-parallel-tests.md)
