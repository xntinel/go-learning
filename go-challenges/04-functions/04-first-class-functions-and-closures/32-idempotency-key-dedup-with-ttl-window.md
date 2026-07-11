# Exercise 32: Idempotency Key Deduplication with TTL Expiration Window

**Nivel: Intermedio** — validacion rapida (un test corto).

A client that retries a payment request after a timeout must not risk a
double charge if the original request actually succeeded server-side.
The standard fix is an idempotency key: the caller sends the same key on
every retry, and the server deduplicates within a bounded window instead of
remembering every key forever. `NewDeduper` closes over a map from key to
cached result plus an expiry, and an injected clock so the TTL boundary is
tested in microseconds instead of real minutes.

## What you'll build

```text
idempotency-dedup/           independent module: example.com/idempotency-dedup
  go.mod                      go 1.24
  idem.go                     NewDeduper returns func(key string, fn func() (string, error)) (string, error)
  cmd/
    demo/
      main.go                  same key twice (cached), then after TTL (re-runs)
  idem_test.go                 table test: cached within TTL, expires, key isolation, concurrent reads
```

- Files: `idem.go`, `cmd/demo/main.go`, `idem_test.go`.
- Implement: `NewDeduper(ttl time.Duration, now func() time.Time) func(key string, fn func() (string, error)) (string, error)`, closing over a mutex-guarded `map[string]entry`.
- Test: repeat calls with the same key inside the TTL return the cached result and call `fn` only once; a call after the TTL has elapsed re-runs `fn`; two different keys never share a cache entry; many goroutines reading an already-cached key never call `fn` again.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/idempotency-dedup/cmd/demo
cd ~/go-exercises/idempotency-dedup
go mod init example.com/idempotency-dedup
go mod edit -go=1.24
```

### A cache with a self-expiring entry, not a separate cleanup job

`NewDeduper` captures `entries`, a `map[string]entry` where each `entry`
holds a cached `result`, `err`, and `expiresAt`. The returned closure first
checks, under the lock, whether `key` has an entry whose `expiresAt` is
still in the future; if so it returns the cached result immediately. If not
— brand new key, or a stale entry past its TTL — it calls `fn` *outside* the
lock (so one slow operation never blocks unrelated keys), then re-locks to
store the fresh result with a new `expiresAt` of `now() + ttl`. On the way
out, that same locked section opportunistically deletes any other entries
whose TTL has already elapsed, so the map does not grow forever even though
nothing calls a separate cleanup goroutine.

Because `fn` runs outside the lock, this deduper's guarantee is scoped: it
caches a *completed* result for `ttl`, but it does not collapse two
goroutines that race on the very same brand-new key into a single call to
`fn` — both can observe "not cached yet" and both run `fn` once each. That
stronger single-flight guarantee is a different, narrower tool (see the
`sync.Once`-based guard elsewhere in this lesson); this module's job is TTL-
scoped deduplication of an already-known key, which is what an idempotency-
key header on a retried request actually needs.

Create `idem.go`:

```go
// Package idem deduplicates calls by an idempotency key within a TTL window.
package idem

import (
	"sync"
	"time"
)

type entry struct {
	result    string
	err       error
	expiresAt time.Time
}

// NewDeduper returns a closure over a mutex-guarded map of idempotency keys
// to cached results with an expiry, plus an injected clock. The first call
// seen for a key runs fn and caches its result until now()+ttl; any call
// with the same key before that instant returns the cached result without
// calling fn again. A call after the TTL has elapsed is treated as new: it
// re-runs fn, replacing the stale entry. now is injected so tests advance a
// fake clock instead of sleeping.
//
// The mutex guards the map only; fn itself runs outside the lock so one
// slow request never blocks unrelated keys. Two goroutines racing on the
// very same brand-new key can both observe "not cached yet" and both call
// fn once each -- this deduper's guarantee is TTL-scoped caching of
// completed results, not single-flight collapsing of concurrent identical
// first calls (see the sync.Once-based guard elsewhere in this lesson for
// that stronger guarantee).
func NewDeduper(ttl time.Duration, now func() time.Time) func(key string, fn func() (string, error)) (string, error) {
	var mu sync.Mutex
	entries := make(map[string]entry)

	return func(key string, fn func() (string, error)) (string, error) {
		mu.Lock()
		if e, ok := entries[key]; ok && now().Before(e.expiresAt) {
			mu.Unlock()
			return e.result, e.err
		}
		mu.Unlock()

		result, err := fn()

		mu.Lock()
		entries[key] = entry{result: result, err: err, expiresAt: now().Add(ttl)}
		for k, e := range entries {
			if !now().Before(e.expiresAt) {
				delete(entries, k)
			}
		}
		mu.Unlock()

		return result, err
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/idempotency-dedup"
)

func main() {
	clockNow := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return clockNow }

	charges := 0
	charge := func() (string, error) {
		charges++
		return fmt.Sprintf("charge-%d", charges), nil
	}

	dedupe := idem.NewDeduper(5*time.Minute, clock)

	r1, _ := dedupe("idem-key-1", charge)
	fmt.Println("t+0m call 1:", r1)

	r2, _ := dedupe("idem-key-1", charge)
	fmt.Println("t+0m call 2 (same key, cached):", r2)

	clockNow = clockNow.Add(10 * time.Minute)
	r3, _ := dedupe("idem-key-1", charge)
	fmt.Println("t+10m call 3 (TTL expired, re-runs):", r3)

	fmt.Println("total charges executed:", charges)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
t+0m call 1: charge-1
t+0m call 2 (same key, cached): charge-1
t+10m call 3 (TTL expired, re-runs): charge-2
total charges executed: 2
```

### Tests

Create `idem_test.go`:

```go
package idem

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func fakeClock(start time.Time) (now func() time.Time, advance func(time.Duration)) {
	var mu sync.Mutex
	cur := start
	now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return cur
	}
	advance = func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		cur = cur.Add(d)
	}
	return now, advance
}

func TestDedupWithinTTLReturnsCachedResult(t *testing.T) {
	now, _ := fakeClock(time.Unix(0, 0))
	dedupe := NewDeduper(5*time.Minute, now)

	calls := 0
	fn := func() (string, error) {
		calls++
		return "result-1", nil
	}

	tests := []struct {
		name string
	}{
		{"first call runs fn"},
		{"second call within TTL is cached"},
		{"third call within TTL is still cached"},
	}
	for _, tc := range tests {
		got, err := dedupe("key-a", fn)
		if err != nil {
			t.Fatalf("%s: err = %v, want nil", tc.name, err)
		}
		if got != "result-1" {
			t.Fatalf("%s: result = %q, want %q", tc.name, got, "result-1")
		}
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (fn only runs once within TTL)", calls)
	}
}

func TestDedupExpiresAfterTTL(t *testing.T) {
	now, advance := fakeClock(time.Unix(0, 0))
	dedupe := NewDeduper(5*time.Minute, now)

	calls := 0
	fn := func() (string, error) {
		calls++
		return "result", nil
	}

	dedupe("key-a", fn)
	if calls != 1 {
		t.Fatalf("calls = %d after first call, want 1", calls)
	}

	advance(4 * time.Minute)
	dedupe("key-a", fn)
	if calls != 1 {
		t.Fatalf("calls = %d at t+4m, want 1 (still within TTL)", calls)
	}

	advance(2 * time.Minute) // t+6m, past the 5-minute TTL
	dedupe("key-a", fn)
	if calls != 2 {
		t.Fatalf("calls = %d at t+6m, want 2 (TTL expired, fn re-runs)", calls)
	}
}

func TestDedupKeysAreIndependent(t *testing.T) {
	now, _ := fakeClock(time.Unix(0, 0))
	dedupe := NewDeduper(time.Minute, now)

	callsA, callsB := 0, 0
	fnA := func() (string, error) { callsA++; return "a", nil }
	fnB := func() (string, error) { callsB++; return "b", nil }

	dedupe("key-a", fnA)
	dedupe("key-b", fnB)
	dedupe("key-a", fnA)
	dedupe("key-b", fnB)

	if callsA != 1 || callsB != 1 {
		t.Fatalf("callsA=%d callsB=%d, want 1, 1 (independent keys)", callsA, callsB)
	}
}

func TestDedupConcurrentReadsOfCachedKey(t *testing.T) {
	now, _ := fakeClock(time.Unix(0, 0)) // frozen: cached entry never expires mid-test
	dedupe := NewDeduper(time.Hour, now)

	var calls atomic.Int64
	fn := func() (string, error) {
		calls.Add(1)
		return "cached-result", nil
	}

	// Seed the cache sequentially first, so the concurrent phase below only
	// ever exercises the read (cache-hit) path -- no race on who runs fn.
	dedupe("hot-key", fn)

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := dedupe("hot-key", fn)
			if err != nil || got != "cached-result" {
				t.Errorf("dedupe() = (%q, %v), want (cached-result, nil)", got, err)
			}
		}()
	}
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want 1 (all concurrent reads hit the cache)", got)
	}
}
```

Verify: `go test -count=1 -race ./...`

## Review

`TestDedupWithinTTLReturnsCachedResult` and `TestDedupExpiresAfterTTL` pin
the exact TTL boundary using a fake clock: cached inside the window,
re-executed exactly once the window has passed. `TestDedupKeysAreIndependent`
confirms the map is keyed correctly rather than collapsing every call into
one slot. The concurrency test seeds the cache sequentially first and then
hammers it with fifty goroutines, proving the mutex-guarded read path is
race-free and that a hot, already-cached key never triggers `fn` again no
matter how many goroutines hit it simultaneously.

## Resources

- [Stripe API docs: Idempotent requests](https://docs.stripe.com/api/idempotent_requests) — the idempotency-key pattern this deduper implements, including the TTL window.
- [pkg.go.dev: time.Time.Before](https://pkg.go.dev/time#Time.Before) — the comparison that decides whether a cached entry is still within its TTL.
- [pkg.go.dev: sync.Mutex](https://pkg.go.dev/sync#Mutex) — guards the shared entries map while `fn` itself runs unlocked.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [31-connection-pool-fast-fail-when-exhausted.md](31-connection-pool-fast-fail-when-exhausted.md) | Next: [33-schema-migration-barrier-reader-blocker.md](33-schema-migration-barrier-reader-blocker.md)
