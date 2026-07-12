# Exercise 6: Validation Gate and CompareAndSwap: Rejecting Stale Config Pushes

When config arrives by push from a distributed control plane, the replicas
pushing it lag each other — and a slow replica delivering yesterday's config
*after* today's must not roll your fleet backward. This exercise hardens the
update path: validate every candidate, then install it with a
`CompareAndSwap` loop that only admits strictly newer versions, counting
every rejection for the metrics pipeline.

This module is fully self-contained: its own `go mod init`, every type it
needs, its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
cfgcas/                    independent module: example.com/cfgcas
  go.mod
  store.go                 Store: Push (validate + CAS version gate), Get, counters; sentinels
  cmd/
    demo/
      main.go              runnable demo: accept v3, reject a late v2, reject an invalid push
  store_test.go            table tests for both rejection kinds; N-goroutine shuffled-push race test
```

- Files: `store.go`, `cmd/demo/main.go`, `store_test.go`.
- Implement: `Push(candidate)` returning `ErrInvalidConfig` on bounds violations and `ErrStaleVersion` when `candidate.Version` is not strictly greater than the installed snapshot's, installing via a `CompareAndSwap` retry loop; `Accepted`/`RejectedStale`/`RejectedInvalid` counters on `atomic.Int64`.
- Test: `errors.Is` assertions on both sentinels; a concurrent test firing N goroutines with shuffled versions 1..N asserting the final snapshot is exactly version N and accepted+stale-rejected counters sum to N, under `-race`.
- Verify: `go test -count=1 -race ./...`

### Why Load-then-Store cannot enforce "never roll back"

The naive versioned update reads the current snapshot, compares versions, and
stores if newer:

```
cur := ptr.Load()
if candidate.Version > cur.Version {
	ptr.Store(candidate)      // WRONG under concurrency
}
```

The check and the store are two separate operations, so they interleave: a
goroutine pushing v5 and one pushing v9 can both pass the check against v3,
and if the v9 store lands first, the v5 store then *overwrites it* — the
fleet just rolled back four versions with every individual line looking
correct. Load-then-Store is not read-modify-write.

`CompareAndSwap(old, new)` closes the gap: it installs `new` only if the
pointer still equals `old`, atomically. The loop shape matters:

1. `Load` the current snapshot.
2. Decide against *that* snapshot: if `candidate.Version <= cur.Version`,
   reject as stale — a terminal answer, not a retry.
3. `CompareAndSwap(cur, candidate)`. Success means the decision and the
   install happened against the same snapshot — the push linearizes there.
   Failure means someone else installed between steps 1 and 3; loop and
   re-decide, because the newly installed config may make our candidate
   stale.

The loop is lock-free: a CAS failure implies another push *succeeded*, so
the system as a whole always makes progress, and a push retries at most once
per concurrent winner. Note also that CAS on `atomic.Pointer[T]` compares
pointer identity, not struct contents — exactly what we want, since every
push carries a distinct allocation, and Go's GC means the ABA problem
(a recycled address making a stale CAS succeed) cannot occur while any
reference is live.

Validation runs *before* the loop — an invalid config must never win a race
into the snapshot no matter what version it claims — and each rejection kind
gets its own counter, because on a dashboard "stale pushes" (normal noise
from lagging replicas, alarming only in bulk) and "invalid pushes" (someone
is shipping garbage) mean completely different pages.

Create `store.go`:

```go
// Package cfgcas installs pushed config snapshots through a validation
// gate and a CompareAndSwap version fence, so an out-of-order push from a
// lagging control-plane replica can never roll the config backward.
package cfgcas

import (
	"errors"
	"fmt"
	"sync/atomic"
)

var (
	// ErrInvalidConfig reports a candidate that failed bounds validation.
	ErrInvalidConfig = errors.New("invalid config")
	// ErrStaleVersion reports a candidate not strictly newer than the
	// installed snapshot.
	ErrStaleVersion = errors.New("stale config version")
)

// Config is one immutable configuration snapshot.
type Config struct {
	MaxConnections int
	TimeoutMillis  int
	Version        int
}

func validate(c *Config) error {
	if c.MaxConnections <= 0 {
		return fmt.Errorf("%w: max_connections %d, want > 0", ErrInvalidConfig, c.MaxConnections)
	}
	if c.TimeoutMillis <= 0 {
		return fmt.Errorf("%w: timeout_millis %d, want > 0", ErrInvalidConfig, c.TimeoutMillis)
	}
	return nil
}

// Store holds the installed snapshot plus rejection counters. Share by
// pointer; do not copy after first use.
type Store struct {
	ptr             atomic.Pointer[Config]
	accepted        atomic.Int64
	rejectedStale   atomic.Int64
	rejectedInvalid atomic.Int64
}

// New returns a Store serving initial. The initial config is validated;
// its version becomes the fence later pushes must exceed.
func New(initial *Config) (*Store, error) {
	if err := validate(initial); err != nil {
		return nil, err
	}
	s := &Store{}
	s.ptr.Store(initial)
	return s, nil
}

// Get returns the installed snapshot (read-only, non-nil after New).
func (s *Store) Get() *Config {
	return s.ptr.Load()
}

// Accepted, RejectedStale and RejectedInvalid expose push outcomes for
// metrics. Every Push increments exactly one of them.
func (s *Store) Accepted() int64        { return s.accepted.Load() }
func (s *Store) RejectedStale() int64   { return s.rejectedStale.Load() }
func (s *Store) RejectedInvalid() int64 { return s.rejectedInvalid.Load() }

// Push validates candidate and installs it only if its version is
// strictly greater than the installed snapshot's, using a CompareAndSwap
// loop so concurrent pushes cannot interleave a rollback. It returns nil
// on install, ErrInvalidConfig or ErrStaleVersion (wrapped) on rejection.
func (s *Store) Push(candidate *Config) error {
	if err := validate(candidate); err != nil {
		s.rejectedInvalid.Add(1)
		return err
	}
	for {
		cur := s.ptr.Load()
		if candidate.Version <= cur.Version {
			s.rejectedStale.Add(1)
			return fmt.Errorf("push v%d rejected: current v%d: %w",
				candidate.Version, cur.Version, ErrStaleVersion)
		}
		if s.ptr.CompareAndSwap(cur, candidate) {
			s.accepted.Add(1)
			return nil
		}
		// Lost the race to another push; re-read and re-decide, because
		// the winner may have made this candidate stale.
	}
}
```

### The runnable demo

The demo replays the control-plane failure this store exists for: v3 arrives
and is installed, then a lagging replica delivers v2 — rejected, fleet stays
on v3 — and a corrupted push is rejected by validation regardless of its
version claim.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/cfgcas"
)

func main() {
	s, err := cfgcas.New(&cfgcas.Config{MaxConnections: 100, TimeoutMillis: 5000, Version: 1})
	if err != nil {
		panic(err)
	}

	if err := s.Push(&cfgcas.Config{MaxConnections: 300, TimeoutMillis: 3000, Version: 3}); err == nil {
		fmt.Printf("accepted v3: max=%d\n", s.Get().MaxConnections)
	}

	// A lagging replica delivers v2 after v3 landed.
	if err := s.Push(&cfgcas.Config{MaxConnections: 200, TimeoutMillis: 4000, Version: 2}); err != nil {
		fmt.Println("rejected:", err)
	}

	// A corrupted push fails validation regardless of version.
	if err := s.Push(&cfgcas.Config{MaxConnections: 0, TimeoutMillis: 1000, Version: 9}); err != nil {
		fmt.Println("rejected:", err)
	}

	fmt.Printf("final: v=%d accepted=%d stale=%d invalid=%d\n",
		s.Get().Version, s.Accepted(), s.RejectedStale(), s.RejectedInvalid())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
accepted v3: max=300
rejected: push v2 rejected: current v3: stale config version
rejected: invalid config: max_connections 0, want > 0
final: v=3 accepted=1 stale=1 invalid=1
```

### Tests

The deterministic tests pin each rejection kind with `errors.Is` against the
wrapped sentinels — including the boundary case of pushing the *same*
version, which must be stale (the fence is strictly-greater). The concurrent
test is the point of the module: 64 goroutines push versions 1..64 in
shuffled order, all racing; whatever the interleaving, the final snapshot
must be exactly v64 (the CAS fence never lets the version move backward) and
the accepted+stale counters must sum to 64 (every push got exactly one
terminal outcome — the retry loop never double-counts or loses a push).

Create `store_test.go`:

```go
package cfgcas

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(&Config{MaxConnections: 1, TimeoutMillis: 1, Version: 0})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPushNewerVersionAccepted(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	if err := s.Push(&Config{MaxConnections: 10, TimeoutMillis: 10, Version: 1}); err != nil {
		t.Fatalf("Push v1: %v", err)
	}
	if got := s.Get(); got.Version != 1 || got.MaxConnections != 10 {
		t.Fatalf("installed = %+v", got)
	}
	if s.Accepted() != 1 {
		t.Fatalf("Accepted = %d", s.Accepted())
	}
}

func TestPushStaleVersionRejected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		version int
	}{
		{"older version", 2},
		{"equal version", 5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := newStore(t)
			if err := s.Push(&Config{MaxConnections: 10, TimeoutMillis: 10, Version: 5}); err != nil {
				t.Fatal(err)
			}

			err := s.Push(&Config{MaxConnections: 20, TimeoutMillis: 20, Version: tc.version})
			if !errors.Is(err, ErrStaleVersion) {
				t.Fatalf("err = %v, want errors.Is(ErrStaleVersion)", err)
			}
			if got := s.Get().Version; got != 5 {
				t.Fatalf("Version = %d after stale push, want 5", got)
			}
			if s.RejectedStale() != 1 {
				t.Fatalf("RejectedStale = %d", s.RejectedStale())
			}
		})
	}
}

func TestPushInvalidConfigRejected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		candidate *Config
	}{
		{"zero max connections", &Config{MaxConnections: 0, TimeoutMillis: 1, Version: 9}},
		{"negative timeout", &Config{MaxConnections: 1, TimeoutMillis: -5, Version: 9}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := newStore(t)
			err := s.Push(tc.candidate)
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("err = %v, want errors.Is(ErrInvalidConfig)", err)
			}
			if got := s.Get().Version; got != 0 {
				t.Fatalf("Version = %d after invalid push, want 0", got)
			}
			if s.RejectedInvalid() != 1 {
				t.Fatalf("RejectedInvalid = %d", s.RejectedInvalid())
			}
		})
	}
}

func TestNewRejectsInvalidInitial(t *testing.T) {
	t.Parallel()

	if _, err := New(&Config{MaxConnections: 0, TimeoutMillis: 1, Version: 1}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New err = %v, want errors.Is(ErrInvalidConfig)", err)
	}
}

func TestConcurrentShuffledPushes(t *testing.T) {
	t.Parallel()

	const n = 64
	s := newStore(t)

	versions := rand.Perm(n) // 0..n-1 shuffled
	var wg sync.WaitGroup
	for _, v := range versions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Push versions 1..n; outcome (accepted or stale) is
			// interleaving-dependent, but the invariants below are not.
			_ = s.Push(&Config{MaxConnections: v + 1, TimeoutMillis: 1, Version: v + 1})
		}()
	}
	wg.Wait()

	if got := s.Get().Version; got != n {
		t.Fatalf("final Version = %d, want %d: the CAS fence let a stale push win", got, n)
	}
	acc, stale := s.Accepted(), s.RejectedStale()
	if acc+stale != n {
		t.Fatalf("accepted %d + stale %d = %d, want %d: a push was lost or double-counted",
			acc, stale, acc+stale, n)
	}
	if acc < 1 {
		t.Fatalf("accepted = %d, want at least the v%d push", acc, n)
	}
	if s.RejectedInvalid() != 0 {
		t.Fatalf("RejectedInvalid = %d, want 0", s.RejectedInvalid())
	}
}

func ExampleStore_Push() {
	s, _ := New(&Config{MaxConnections: 100, TimeoutMillis: 5000, Version: 1})
	fmt.Println(s.Push(&Config{MaxConnections: 200, TimeoutMillis: 3000, Version: 2}))
	fmt.Println(s.Get().Version)
	err := s.Push(&Config{MaxConnections: 150, TimeoutMillis: 4000, Version: 2})
	fmt.Println(errors.Is(err, ErrStaleVersion))
	// Output:
	// <nil>
	// 2
	// true
}
```

## Review

The invariant pair to internalize: the installed version never decreases
(safety), and every push terminates with exactly one outcome (liveness plus
accounting). `TestConcurrentShuffledPushes` checks both without asserting
anything about scheduling — which pushes get accepted genuinely varies run to
run (v17 can land before v9 arrives, making v9 stale), but "final version is
the maximum" and "outcomes sum to N" hold under every interleaving. That is
what makes a concurrency test trustworthy: assert the invariants, never the
interleaving.

The subtle line in the implementation is the re-decide after a failed CAS.
Retrying the CAS *without* re-checking staleness would be wrong: the push
that beat you may carry a higher version, and blindly retrying would install
your now-stale candidate on the next spin. Equally load-bearing:
validation sits outside the loop (an invalid config must not consume CAS
attempts or ever be installable) and the fence is strictly-greater, so a
duplicate delivery of the current version — routine at-least-once behavior
from any control plane — lands as a quiet stale rejection rather than a
spurious "change". Verify with `go test -count=1 -race ./...`.

## Resources

- [sync/atomic: Pointer.CompareAndSwap](https://pkg.go.dev/sync/atomic#Pointer.CompareAndSwap) — the compare-and-swap this fence is built on.
- [The Go Memory Model](https://go.dev/ref/mem) — why the CAS winner's writes are visible to every later Load.
- [math/rand/v2: Perm](https://pkg.go.dev/math/rand/v2#Perm) — the shuffled version order in the race test.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-sighup-reload-worker.md](05-sighup-reload-worker.md) | Next: [07-reload-subscriber-fanout.md](07-reload-subscriber-fanout.md)
