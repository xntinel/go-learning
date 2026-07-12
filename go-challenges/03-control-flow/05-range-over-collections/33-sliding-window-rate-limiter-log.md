# Exercise 33: Distributed Rate Limiter with Sliding Window Log

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A token-bucket rate limiter is simple and cheap, but it is also static: it
refills at a fixed rate regardless of the actual shape of recent traffic, so
a client that sends nothing for a minute and then bursts can either be
punished for a burst a bucket would have permitted, or permitted a burst a
stricter policy should have blocked, depending on which side of the refill
math the burst lands on. A sliding-window log fixes this by tracking every
individual request's timestamp per key and counting exactly how many fall
within the trailing window at decision time — exact enforcement against the
real traffic shape, at the cost of a per-key log that keeps growing unless
something prunes it. This module ranges each key's request log to evict
expired entries on every `Allow` call, enforces the limit atomically so
concurrent callers can never jointly exceed it, and ranges every key under
a periodic `Prune` to reclaim memory from keys that have gone quiet. The
module is fully self-contained: its own `go mod init`, no external
dependencies.

## What you'll build

```text
ratelimit/                  independent module: example.com/sliding-window-rate-limiter-log
  go.mod                    go 1.24
  ratelimit.go              type Limiter; Allow, Prune, Count
  cmd/
    demo/
      main.go               runnable demo: 5 requests against a 3-per-10s limit, then a full prune
  ratelimit_test.go          table test: admit/deny/age-out + Prune eviction; concurrent Allow under -race
```

- Files: `ratelimit.go`, `cmd/demo/main.go`, `ratelimit_test.go`.
- Implement: `Limiter.Allow`, `Limiter.Prune`, and `Limiter.Count`, all
  synchronized under one `sync.Mutex`.
- Test: a three-case `Allow` table (under limit, at capacity, aging out of
  the window), a `Prune` eviction case, and a concurrent-`Allow` test under
  `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Pruning in place, and pruning as part of every decision, not a separate pass

`Allow`'s range over `log` does not just count old entries to discard, it
rebuilds the log in place using the classic filter-into-yourself idiom:
`kept := log[:0]` reuses the same backing array, and the loop only ever
writes to an index at or behind the one it is currently reading, so the
in-place compaction is safe even though `kept` and `log` share memory. This
matters for more than tidiness — it means every single `Allow` call, not
just a periodic sweep, keeps a key's log bounded to roughly its actual
recent traffic instead of growing forever between prunes. `Prune` exists for
a different problem entirely: a key that stops sending requests altogether
still has a `[]time.Time` entry sitting in `l.logs` that `Allow` will never
touch again, because `Allow` only prunes the key it is currently asked
about. `Prune` ranges *every* key in `l.logs`, evicts each one's expired
entries the same way, and deletes the map entry outright once a key's log is
left empty — the garbage collection an idle key needs that no amount of
`Allow` traffic on other keys would ever trigger.

The reason `Allow`'s prune-then-check-then-append sequence has to happen
under one lock acquisition, not three, is the same reason every other
exercise in this lesson makes the same point: without it, two concurrent
callers for the same key could each read the count *after* eviction but
*before* either one appended, both see `count < limit`, and both admit a
request that together push the key over its cap. Holding `l.mu` for the
entire body of `Allow` is what makes "count after pruning, then admit or
deny" atomic with respect to every other goroutine calling `Allow` for the
same key.

Create `ratelimit.go`:

```go
package ratelimit

import (
	"sync"
	"time"
)

// Limiter enforces a per-key sliding-window request cap: at most limit
// requests may fall within any window-long trailing interval. Unlike a
// token bucket, which smooths bursts by design, a sliding-window log tracks
// every individual request's timestamp, so it can enforce the cap exactly
// against real burst patterns at the cost of the log itself needing pruning
// as entries age out.
type Limiter struct {
	mu     sync.Mutex
	logs   map[string][]time.Time
	limit  int
	window time.Duration
}

// New builds a Limiter allowing at most limit requests per key within any
// trailing window.
func New(limit int, window time.Duration) *Limiter {
	return &Limiter{
		logs:   make(map[string][]time.Time),
		limit:  limit,
		window: window,
	}
}

// Allow reports whether a request for key at time now is within the limit.
// It first ranges key's log to evict every entry older than the trailing
// window, then admits the request only if the surviving count is still
// under the limit — the prune and the admission decision happen inside the
// same critical section, so two concurrent callers can never both read the
// pre-prune count and both admit a request that pushes the key over limit.
func (l *Limiter) Allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := now.Add(-l.window)
	log := l.logs[key]

	kept := log[:0]
	for _, t := range log {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}

	if len(kept) >= l.limit {
		l.logs[key] = kept
		return false
	}

	l.logs[key] = append(kept, now)
	return true
}

// Prune ranges every key's log under lock, evicting entries older than the
// trailing window relative to now, and removes any key whose log is left
// empty entirely — reclaiming memory for keys that have gone quiet instead
// of letting every key that was ever seen hold an entry in l.logs forever.
// It returns how many stale entries were evicted and how many keys were
// removed outright.
func (l *Limiter) Prune(now time.Time) (evictedEntries, removedKeys int) {
	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := now.Add(-l.window)
	for key, log := range l.logs {
		kept := log[:0]
		for _, t := range log {
			if t.After(cutoff) {
				kept = append(kept, t)
			}
		}
		evictedEntries += len(log) - len(kept)
		if len(kept) == 0 {
			delete(l.logs, key)
			removedKeys++
			continue
		}
		l.logs[key] = kept
	}
	return evictedEntries, removedKeys
}

// Count reports how many entries are currently logged for key, without
// pruning them first.
func (l *Limiter) Count(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.logs[key])
}
```

### The runnable demo

The demo sends 5 requests from the same client at 2-second intervals against
a 3-per-10-second limit — the fourth is denied at capacity, the fifth is
admitted once the first request has aged out of the window — then runs a
full `Prune` an hour later.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/sliding-window-rate-limiter-log"
)

func main() {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := ratelimit.New(3, 10*time.Second)

	offsets := []time.Duration{0, 2 * time.Second, 4 * time.Second, 6 * time.Second, 12 * time.Second}
	for _, off := range offsets {
		now := base.Add(off)
		allowed := l.Allow("ip:203.0.113.9", now)
		fmt.Printf("t=%ds allowed=%v\n", int(off.Seconds()), allowed)
	}

	evicted, removed := l.Prune(base.Add(1 * time.Hour))
	fmt.Printf("prune: evicted=%d removed_keys=%d\n", evicted, removed)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
t=0s allowed=true
t=2s allowed=true
t=4s allowed=true
t=6s allowed=false
t=12s allowed=true
prune: evicted=2 removed_keys=1
```

### Tests

The `Allow` table drives the under-limit, at-capacity, and aging-out-of-
window cases through the same offsets as the demo. A dedicated `Prune` test
checks that stale entries are evicted and an emptied key is removed
entirely while an active key survives. The concurrency test fires 50
simultaneous `Allow` calls for one key against a limit of 10 and must admit
exactly 10, no matter how the goroutines interleave, under `-race`.

Create `ratelimit_test.go`:

```go
package ratelimit

import (
	"sync"
	"testing"
	"time"
)

func TestAllow(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		limit   int
		window  time.Duration
		offsets []time.Duration // request times, relative to base
		want    []bool
	}{
		{
			name:    "under limit admits every request",
			limit:   5,
			window:  10 * time.Second,
			offsets: []time.Duration{0, time.Second, 2 * time.Second},
			want:    []bool{true, true, true},
		},
		{
			name:    "denies once the trailing window is at capacity",
			limit:   3,
			window:  10 * time.Second,
			offsets: []time.Duration{0, 2 * time.Second, 4 * time.Second, 6 * time.Second},
			want:    []bool{true, true, true, false},
		},
		{
			name:    "an entry aging out of the window frees capacity",
			limit:   3,
			window:  10 * time.Second,
			offsets: []time.Duration{0, 2 * time.Second, 4 * time.Second, 6 * time.Second, 12 * time.Second},
			want:    []bool{true, true, true, false, true},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := New(tc.limit, tc.window)
			got := make([]bool, len(tc.offsets))
			for i, off := range tc.offsets {
				got[i] = l.Allow("k", base.Add(off))
			}
			for i, want := range tc.want {
				if got[i] != want {
					t.Errorf("Allow() call %d = %v, want %v", i, got[i], want)
				}
			}
		})
	}
}

func TestPruneEvictsExpiredAndRemovesEmptyKeys(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := New(5, 10*time.Second)

	l.Allow("stale", base)
	l.Allow("stale", base.Add(1*time.Second))
	l.Allow("fresh", base.Add(9*time.Minute+55*time.Second))

	evicted, removed := l.Prune(base.Add(10 * time.Minute))
	if evicted != 2 {
		t.Fatalf("Prune() evicted = %d, want 2", evicted)
	}
	if removed != 1 {
		t.Fatalf("Prune() removedKeys = %d, want 1", removed)
	}
	if l.Count("stale") != 0 {
		t.Fatalf("Count(stale) = %d, want 0", l.Count("stale"))
	}
	if l.Count("fresh") != 1 {
		t.Fatalf("Count(fresh) = %d, want 1", l.Count("fresh"))
	}
}

func TestConcurrentAllowNeverExceedsLimit(t *testing.T) {
	t.Parallel()

	const limit = 10
	l := New(limit, time.Minute)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	var mu sync.Mutex
	admitted := 0

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.Allow("shared-key", now) {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if admitted != limit {
		t.Fatalf("admitted = %d, want exactly %d", admitted, limit)
	}
}
```

Run it:

```bash
go test -count=1 -race ./...
```

## Review

The limiter is correct when `Allow` never admits more than `limit` requests
within any trailing `window` for a given key, regardless of how many
goroutines call it concurrently, and when `Prune` reclaims memory for keys
that have stopped sending traffic without disturbing keys that are still
active. The bug this design specifically avoids is splitting the prune and
the admission check across two lock acquisitions — reading the pruned count
under one `Lock`/`Unlock`, then deciding to admit and appending under a
second one. That gap, however small, is exactly where two concurrent
callers could both observe `count < limit` and both admit, jointly pushing
the key over its cap in a way `TestConcurrentAllowNeverExceedsLimit` is
built to catch: the prune, the capacity check, and the append all happen
inside one `l.mu.Lock()`/`defer l.mu.Unlock()`, with no window for another
goroutine to interleave.

## Resources

- [sync.Mutex](https://pkg.go.dev/sync#Mutex)
- [Cloudflare: How we built rate limiting capable of scaling to millions of domains](https://blog.cloudflare.com/counting-things-a-lot-of-different-things/) — a production sliding-window rate limiter design at scale.
- [Go Specification: For statements (range over slice, in-place filtering)](https://go.dev/ref/spec#For_statements)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [32-gossip-protocol-vector-clocks.md](32-gossip-protocol-vector-clocks.md) | Next: [34-dns-service-discovery-ttl-cache.md](34-dns-service-discovery-ttl-cache.md)
