# Exercise 9: Config hot-reload fan-out (wait for version to advance)

Long-running services reload configuration without restarting: one goroutine
publishes a new snapshot, and every subsystem's watcher goroutine — the rate
limiter, the log pipeline, the feature-flag evaluator — must wake and pick it
up. This module builds that watch primitive with a monotonically increasing
version number, and shows the chapter's final discipline: when ordering
matters, encode it in the shared state and the predicate, never in who wakes
first.

## What you'll build

```text
confwatch/                  independent module: example.com/confwatch
  go.mod                    module path example.com/confwatch
  store.go                  type Config; type Store: New, Current, Publish, AwaitChange
  cmd/
    demo/
      main.go               3 watchers all woken by one Publish, same snapshot
  store_test.go             M-watcher fan-out, stale-version fast path, monotonicity, -race
```

- Files: `store.go`, `cmd/demo/main.go`, `store_test.go`.
- Implement: a `Store` where `Publish(cfg)` installs a snapshot under the lock, bumps a `uint64` version, and Broadcasts; `AwaitChange(lastSeen)` blocks while `version <= lastSeen` and returns the new snapshot with its version; `Current()` reads without blocking.
- Test: M parked watchers are ALL released by one `Publish` with a consistent snapshot+version pair; a watcher arriving with a stale `lastSeen` returns immediately; rapid double `Publish` never delivers a version `<= lastSeen`; `-race` stress with a publisher racing looping watchers.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/15-sync-primitives/06-sync-cond/09-config-version-fanout/cmd/demo
cd go-solutions/15-sync-primitives/06-sync-cond/09-config-version-fanout
```

### The version IS the protocol

The naive watch API — "block until the config changes" — is unimplementable
without races: a change that lands between two waits is silently missed, and a
`Broadcast` that arrives before the watcher parks is lost. The version number
dissolves both problems. Every publish increments `version` under the lock;
`AwaitChange(lastSeen)` waits on the predicate `version <= lastSeen`. A change
that happened before the watcher even called `AwaitChange` makes the predicate
already false, so the call returns immediately — nothing is missed and no park
happens (the stale-`lastSeen` fast path). Two publishes in quick succession
may coalesce from a watcher's point of view — it wakes once and sees the
version advanced by two — which is exactly what a config consumer wants: the
LATEST snapshot, not a replay of every intermediate one. Contrast the job
waiter from the previous exercise, where each status is awaited explicitly;
here the contract is level-triggered ("newer than what I have"), not
edge-triggered.

The snapshot and its version are returned as one pair read under the lock, so
they can never be torn — a watcher never observes version 3 with version 2's
payload. `Config` is a plain value struct: copying it out under the lock hands
the watcher an immutable snapshot it can read forever without holding any
lock, which is the whole point of snapshot-style config (an alternative
implementation stores `atomic.Pointer[Config]` for readers and keeps the Cond
only for watchers; the version protocol is identical).

`Publish` Broadcasts because a new version satisfies EVERY parked watcher at
once — the many-satisfiable-waiters case from the concepts file. `Signal`
would update one subsystem's config and leave the rest running with the old
one until the next publish: a split-brain where half your service enforces the
new rate limit and half the old, which is worse than either consistent state.
And because `Cond` guarantees nothing about wakeup order, the monotonicity
test asserts only what the state machine guarantees — every return value is
`> lastSeen` and consistent — never which watcher woke first.

Create `store.go`:

```go
package confwatch

import "sync"

// Config is an immutable configuration snapshot. It is copied out by value:
// watchers can read their copy without any lock.
type Config struct {
	LogLevel     string
	RateLimitRPS int
}

// Store holds the current Config and a monotonically increasing version, and
// lets watcher goroutines block until the version advances.
type Store struct {
	mu      sync.Mutex
	cond    *sync.Cond
	version uint64
	current Config
}

// New returns a Store holding initial as version 1.
func New(initial Config) *Store {
	s := &Store{version: 1, current: initial}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// Current returns the current snapshot and its version without blocking.
func (s *Store) Current() (Config, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current, s.version
}

// Publish installs cfg as a new version, wakes every watcher, and returns the
// new version. Broadcast is required: one publish satisfies ALL watchers.
func (s *Store) Publish(cfg Config) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.version++
	s.current = cfg
	s.cond.Broadcast()
	return s.version
}

// AwaitChange blocks until the version is greater than lastSeen and returns
// the snapshot with its version. If newer config already exists it returns
// immediately.
func (s *Store) AwaitChange(lastSeen uint64) (Config, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for s.version <= lastSeen {
		s.cond.Wait()
	}
	return s.current, s.version
}
```

### The runnable demo

Three subsystem watchers park on version 1; one `Publish` releases all three
with the same snapshot.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/confwatch"
)

func main() {
	s := confwatch.New(confwatch.Config{LogLevel: "info", RateLimitRPS: 100})

	results := make(chan string, 3)
	for range 3 {
		go func() {
			cfg, v := s.AwaitChange(1)
			results <- fmt.Sprintf("woke on v%d: log=%s rps=%d", v, cfg.LogLevel, cfg.RateLimitRPS)
		}()
	}

	v := s.Publish(confwatch.Config{LogLevel: "debug", RateLimitRPS: 250})
	fmt.Println("published version", v)
	for range 3 {
		fmt.Println("watcher", <-results)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
published version 2
watcher woke on v2: log=debug rps=250
watcher woke on v2: log=debug rps=250
watcher woke on v2: log=debug rps=250
```

The three watcher lines are identical by construction — all three received
the same snapshot — so the output is deterministic even though the wakeup
order is not. If a watcher parks after the publish instead of before, the
stale-version fast path returns the same pair immediately; the demo cannot
race itself into different output.

### Tests

`TestPublishReleasesAllWatchers` pins the fan-out: five watchers park
(`synctest.Wait` proves it), one publish releases every one with the same
consistent pair. `TestMonotonicity` publishes twice into a parked watcher and
asserts snapshot/version consistency for whichever version it observed.
`TestStress` runs looping watchers against a rapid publisher under `-race`,
asserting versions strictly increase per watcher.

Create `store_test.go`:

```go
package confwatch

import (
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
)

func TestPublishReleasesAllWatchers(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := New(Config{LogLevel: "info", RateLimitRPS: 100})

		type seen struct {
			cfg Config
			v   uint64
		}
		const watchers = 5
		results := make(chan seen, watchers)
		for range watchers {
			go func() {
				cfg, v := s.AwaitChange(1)
				results <- seen{cfg, v}
			}()
		}

		synctest.Wait() // all watchers durably parked
		select {
		case r := <-results:
			t.Fatalf("watcher returned %+v before any publish", r)
		default:
		}

		want := Config{LogLevel: "debug", RateLimitRPS: 250}
		if v := s.Publish(want); v != 2 {
			t.Fatalf("Publish returned version %d, want 2", v)
		}
		synctest.Wait()

		for range watchers {
			select {
			case r := <-results:
				if r.v != 2 || r.cfg != want {
					t.Fatalf("watcher saw %+v at v%d, want %+v at v2", r.cfg, r.v, want)
				}
			default:
				t.Fatal("a watcher was not released by Publish; Signal instead of Broadcast?")
			}
		}
	})
}

func TestStaleVersionReturnsImmediately(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := New(Config{LogLevel: "info", RateLimitRPS: 100})
		// lastSeen 0 < version 1: must return without parking. Running on the
		// test goroutine, a park here would deadlock the bubble and fail.
		cfg, v := s.AwaitChange(0)
		if v != 1 || cfg.LogLevel != "info" {
			t.Fatalf("AwaitChange(0) = %+v at v%d, want initial config at v1", cfg, v)
		}
	})
}

func TestMonotonicity(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := New(Config{LogLevel: "info", RateLimitRPS: 100})

		type seen struct {
			cfg Config
			v   uint64
		}
		got := make(chan seen, 1)
		go func() {
			cfg, v := s.AwaitChange(1)
			got <- seen{cfg, v}
		}()
		synctest.Wait()

		cfgA := Config{LogLevel: "warn", RateLimitRPS: 200}
		cfgB := Config{LogLevel: "error", RateLimitRPS: 300}
		s.Publish(cfgA)
		s.Publish(cfgB)
		synctest.Wait()

		r := <-got
		if r.v <= 1 {
			t.Fatalf("watcher saw version %d, want > lastSeen 1", r.v)
		}
		// The pair must be consistent whichever publish the watcher observed.
		if !(r.v == 2 && r.cfg == cfgA) && !(r.v == 3 && r.cfg == cfgB) {
			t.Fatalf("torn read: version %d with config %+v", r.v, r.cfg)
		}
	})
}

func TestStress(t *testing.T) {
	t.Parallel()

	s := New(Config{LogLevel: "info", RateLimitRPS: 100})
	const publishes = 100
	const finalVersion = 1 + publishes

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			last := uint64(1)
			for last < finalVersion {
				_, v := s.AwaitChange(last)
				if v <= last {
					t.Errorf("AwaitChange(%d) returned version %d, not greater", last, v)
					return
				}
				last = v
			}
		})
	}
	for i := range publishes {
		s.Publish(Config{LogLevel: "info", RateLimitRPS: 100 + i})
	}
	wg.Wait()

	if _, v := s.Current(); v != finalVersion {
		t.Fatalf("final version = %d, want %d", v, finalVersion)
	}
}

func Example() {
	s := New(Config{LogLevel: "info", RateLimitRPS: 100})
	go s.Publish(Config{LogLevel: "debug", RateLimitRPS: 100})
	cfg, v := s.AwaitChange(1)
	fmt.Println(v, cfg.LogLevel)
	// Output: 2 debug
}
```

## Review

The store is correct when every `AwaitChange(lastSeen)` return satisfies three
properties: the version is strictly greater than `lastSeen`, the snapshot is
the one installed at that version (no torn pair), and no publish strands a
parked watcher. Each has a dedicated test, and each maps to one line of the
implementation: the `for` predicate, the paired return under the lock, and the
`Broadcast`. The stress test's per-watcher `last = v` loop is the same loop a
real config-watcher goroutine runs in production — `AwaitChange`, apply, wait
again — so passing it means the API composes into the real usage shape.

The mistake this design defends against most is trusting the wakeup instead
of the version. A watcher that treats "I woke up" as "there is a new config"
double-applies on spurious or coalesced wakeups; a watcher that stores
`lastSeen` outside the returned pair drifts. Everything flows from the
version comparison under the lock. If you later need cancellable watches
(shutdown while parked), combine this module with the ctx-watcher pattern
from the job-status exercise — the two compose without modification.

## Resources

- [`sync.Cond`](https://pkg.go.dev/sync#Cond) — Broadcast fan-out and the no-ordering guarantee.
- [The Go Memory Model](https://go.dev/ref/mem) — why the snapshot written before Broadcast is visible to woken waiters.
- [`testing/synctest`](https://pkg.go.dev/testing/synctest) — proving all watchers park and all wake.

---

Back to [08-weighted-semaphore.md](08-weighted-semaphore.md) | Next: [../07-atomic-package/00-concepts.md](../07-atomic-package/00-concepts.md)
