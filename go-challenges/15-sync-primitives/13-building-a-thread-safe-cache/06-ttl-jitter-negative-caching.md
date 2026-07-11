# Exercise 6: TTL Jitter and Negative Caching

Two hardening patterns for the same cache, both born from real pager duty.
First: a deploy warms 100k keys with `ttl=5m`, and five minutes later they all
expire in the same second — the database takes a synchronized load spike, then
the refill re-synchronizes them and it repeats forever. Second: clients keep
requesting a user id that does not exist, and because "not found" is never
cached, every request goes to the database. TTL jitter fixes the first;
negative caching fixes the second.

## What you'll build

```text
cachejitter/                     independent module: example.com/cachejitter
  go.mod
  cache/
    cache.go                     sharded TTL cache core + ExpiresAt debug accessor
    jitter.go                    SetJittered(key, v, ttl, jitterFrac) using
                                 math/rand/v2 rand.N over time.Duration
    jitter_test.go               expiry lands in [ttl, ttl+maxJitter]; offsets differ
  store/
    store.go                     UserStore: Repo interface, ErrNotFound sentinel,
                                 result envelope, positive + short negative TTLs
    store_test.go                counting fake repo: miss cached once, negative TTL
                                 re-consults, transient errors NOT cached, Example
  cmd/
    demo/
      main.go                    jitter spread check + negative-cache walkthrough
```

- Files: `cache/cache.go`, `cache/jitter.go`, `cache/jitter_test.go`, `store/store.go`, `store/store_test.go`, `cmd/demo/main.go`.
- Implement: `SetJittered` spreading expiry by `rand.N(maxJitter)`, and a repository-backed `UserStore` that caches "not found" as a typed envelope with its own short TTL.
- Test: stored expiry falls inside the jitter window and offsets decorrelate; two lookups of a missing id hit the repo once; after the negative TTL the repo is consulted again; transient errors are never cached.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/cachejitter/cache ~/go-exercises/cachejitter/store ~/go-exercises/cachejitter/cmd/demo
cd ~/go-exercises/cachejitter
go mod init example.com/cachejitter
```

### The cache core (self-contained copy, plus one debug accessor)

The core is the exercise-1 cache with one addition: `ExpiresAt(key)`, an
exported accessor for the stored deadline. It exists for observability and
tests — "when does this key actually expire" is a question you will ask in
every cache incident — and it is how the jitter tests assert the window
without reaching into unexported state from another package.

Create `cache/cache.go`:

```go
package cache

import (
	"hash/fnv"
	"sync"
	"time"
)

type entry[V any] struct {
	value     V
	expiresAt time.Time
}

func (e *entry[V]) expired(now time.Time) bool {
	return !e.expiresAt.IsZero() && now.After(e.expiresAt)
}

type shard[V any] struct {
	mu    sync.RWMutex
	items map[string]*entry[V]
}

// Cache is a lock-striped, TTL-aware map with lazy expiry on Get.
type Cache[V any] struct {
	shards    []*shard[V]
	numShards uint32
}

func New[V any](numShards int) *Cache[V] {
	if numShards < 1 {
		numShards = 1
	}
	shards := make([]*shard[V], numShards)
	for i := range shards {
		shards[i] = &shard[V]{items: make(map[string]*entry[V])}
	}
	return &Cache[V]{shards: shards, numShards: uint32(numShards)}
}

func (c *Cache[V]) shardFor(key string) *shard[V] {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return c.shards[h.Sum32()%c.numShards]
}

// Set stores value under key with the given TTL. A non-positive TTL
// means "no expiration".
func (c *Cache[V]) Set(key string, value V, ttl time.Duration) {
	s := c.shardFor(key)
	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = time.Now().Add(ttl)
	}
	s.mu.Lock()
	s.items[key] = &entry[V]{value: value, expiresAt: expiresAt}
	s.mu.Unlock()
}

// Get returns the value for key; false if missing or expired.
func (c *Cache[V]) Get(key string) (V, bool) {
	s := c.shardFor(key)
	s.mu.RLock()
	e, ok := s.items[key]
	if !ok || e.expired(time.Now()) {
		s.mu.RUnlock()
		var zero V
		return zero, false
	}
	v := e.value
	s.mu.RUnlock()
	return v, true
}

// Delete removes the entry for key. It is a no-op if the key is absent.
func (c *Cache[V]) Delete(key string) {
	s := c.shardFor(key)
	s.mu.Lock()
	delete(s.items, key)
	s.mu.Unlock()
}

// Size returns the number of non-expired entries across all shards.
func (c *Cache[V]) Size() int {
	now := time.Now()
	total := 0
	for _, s := range c.shards {
		s.mu.RLock()
		for _, e := range s.items {
			if !e.expired(now) {
				total++
			}
		}
		s.mu.RUnlock()
	}
	return total
}

// ExpiresAt reports the stored deadline for key (zero time means "never
// expires") and whether the key is present at all. Debug/test accessor.
func (c *Cache[V]) ExpiresAt(key string) (time.Time, bool) {
	s := c.shardFor(key)
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.items[key]
	if !ok {
		return time.Time{}, false
	}
	return e.expiresAt, true
}
```

### Jitter: decorrelating mass expiry

`SetJittered(key, v, ttl, jitterFrac)` stores the entry with
`ttl + rand.N(maxJitter)` where `maxJitter = jitterFrac * ttl`. The effective
TTL is *never shorter* than the requested one — jitter only extends, so no
caller sees data expire early — and a warmed cohort's expirations spread
uniformly across the jitter window instead of detonating together. A fraction
of 0.1-0.2 is typical: at `ttl=5m`, a 20% jitter smears the spike across a
full minute.

`math/rand/v2` is the right tool for two reasons. Its top-level functions are
safe for concurrent use with no seeding ceremony (the v1 `rand.Seed` global
pattern is deprecated and gone in v2's design), and `rand.N` is generic over
integer types *including* `time.Duration`, so `rand.N(maxJitter)` returns a
random duration in `[0, maxJitter)` with no unit-conversion arithmetic to get
wrong. One contract detail: `rand.N` panics if its argument is non-positive,
so guard the degenerate cases (`ttl <= 0`, `jitterFrac <= 0`) explicitly.

Create `cache/jitter.go`:

```go
package cache

import (
	"math/rand/v2"
	"time"
)

// SetJittered stores value under key with a TTL extended by a uniformly
// random jitter in [0, jitterFrac*ttl). Bulk-warmed keys stored with the
// same nominal TTL then expire spread across the jitter window instead
// of all at once. A non-positive ttl or jitterFrac falls back to Set.
func (c *Cache[V]) SetJittered(key string, value V, ttl time.Duration, jitterFrac float64) {
	if ttl <= 0 || jitterFrac <= 0 {
		c.Set(key, value, ttl)
		return
	}
	maxJitter := time.Duration(float64(ttl) * jitterFrac)
	if maxJitter <= 0 {
		c.Set(key, value, ttl)
		return
	}
	c.Set(key, value, ttl+rand.N(maxJitter))
}
```

### Negative caching: storing the miss itself

A cache stores answers, and "that user does not exist" *is an answer*. The
`store` package wraps a repository with a cache of `result` envelopes — a
value plus a `missing` flag. A repo hit caches `{user, false}` with the
positive TTL; a repo `ErrNotFound` caches `{nil, true}` with the *negative*
TTL; a cached `missing` entry answers `ErrNotFound` from memory without
touching the database.

Two rules keep this safe. The negative TTL must be much shorter than the
positive one: a positive entry going stale means slightly old data, but a
negative entry going stale means a *newly created user is invisible* until it
expires — existence changes matter more than attribute changes. And only
`ErrNotFound` is cached; a transient error (timeout, connection refused) is
returned but never stored, because caching a five-second outage as "does not
exist" turns a blip into data corruption from the caller's point of view.

The envelope also quietly solves the nil-pointer ambiguity from the concepts
file: `Cache[result]` never stores a bare nil `*User` as a "value", so
`(nil, true)` hits cannot be misread — presence and existence are separate
bits.

Create `store/store.go`:

```go
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/cachejitter/cache"
)

// ErrNotFound is returned when a user does not exist. Repositories
// return it; Store caches it (negative caching) and returns it wrapped.
var ErrNotFound = errors.New("user not found")

type User struct {
	ID   string
	Name string
}

// Repo is the persistence port Store caches in front of.
type Repo interface {
	FindUser(ctx context.Context, id string) (*User, error)
}

// result is the cache envelope: either a present user or a cached miss.
// Wrapping in an envelope keeps "cached nil" distinct from "not cached".
type result struct {
	user    *User
	missing bool
}

// Store caches Repo lookups, including negative ("not found") results.
type Store struct {
	repo   Repo
	cache  *cache.Cache[result]
	posTTL time.Duration
	negTTL time.Duration
}

// New builds a Store. negTTL should be much shorter than posTTL: a
// stale positive entry is old data, a stale negative entry hides a
// newly created user entirely.
func New(repo Repo, posTTL, negTTL time.Duration) *Store {
	return &Store{
		repo:   repo,
		cache:  cache.New[result](8),
		posTTL: posTTL,
		negTTL: negTTL,
	}
}

// GetUser returns the user with the given id, serving both hits and
// known misses from memory. Transient repo errors are never cached.
func (s *Store) GetUser(ctx context.Context, id string) (*User, error) {
	key := "user:" + id
	if r, ok := s.cache.Get(key); ok {
		if r.missing {
			return nil, fmt.Errorf("get user %q (cached): %w", id, ErrNotFound)
		}
		return r.user, nil
	}

	u, err := s.repo.FindUser(ctx, id)
	switch {
	case errors.Is(err, ErrNotFound):
		s.cache.Set(key, result{missing: true}, s.negTTL)
		return nil, fmt.Errorf("get user %q: %w", id, ErrNotFound)
	case err != nil:
		// Transient failure: surface it, cache nothing.
		return nil, fmt.Errorf("get user %q: %w", id, err)
	}
	s.cache.Set(key, result{user: u}, s.posTTL)
	return u, nil
}

// Invalidate drops the cached entry for id — call it after creating or
// updating a user so a cached miss does not hide the new row.
func (s *Store) Invalidate(id string) {
	s.cache.Delete("user:" + id)
}
```

### The demo

The demo shows both patterns: twenty jittered warm-ups whose expirations all
land inside `(5m, 6m]` yet differ from each other, then a negative-cache
walkthrough with a counting repository.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"example.com/cachejitter/cache"
	"example.com/cachejitter/store"
)

type countingRepo struct {
	calls atomic.Int64
	users map[string]*store.User
}

func (r *countingRepo) FindUser(ctx context.Context, id string) (*store.User, error) {
	r.calls.Add(1)
	if u, ok := r.users[id]; ok {
		return u, nil
	}
	return nil, store.ErrNotFound
}

func main() {
	// Jitter: 20 keys warmed with ttl=5m, 20% jitter.
	c := cache.New[string](8)
	base := 5 * time.Minute
	before := time.Now()
	for i := range 20 {
		c.SetJittered(fmt.Sprintf("warm-%d", i), "v", base, 0.2)
	}
	var offsets []time.Duration
	inWindow := true
	for i := range 20 {
		exp, _ := c.ExpiresAt(fmt.Sprintf("warm-%d", i))
		off := exp.Sub(before)
		offsets = append(offsets, off)
		if off < base || off > base+base/5+time.Second {
			inWindow = false
		}
	}
	distinct := false
	for _, o := range offsets[1:] {
		if o != offsets[0] {
			distinct = true
			break
		}
	}
	fmt.Printf("jitter: all 20 expiries in (5m, 6m]: %v, decorrelated: %v\n", inWindow, distinct)

	// Negative caching: repeated misses cost one repo call per negTTL.
	repo := &countingRepo{users: map[string]*store.User{"alice": {ID: "alice", Name: "Alice"}}}
	st := store.New(repo, time.Minute, 50*time.Millisecond)
	ctx := context.Background()

	st.GetUser(ctx, "alice")
	st.GetUser(ctx, "alice")
	fmt.Printf("two hits for alice: repo calls=%d\n", repo.calls.Load())

	_, err1 := st.GetUser(ctx, "ghost")
	_, err2 := st.GetUser(ctx, "ghost")
	fmt.Printf("two misses for ghost: repo calls=%d, both not-found: %v\n",
		repo.calls.Load(), errors.Is(err1, store.ErrNotFound) && errors.Is(err2, store.ErrNotFound))

	time.Sleep(80 * time.Millisecond) // negative TTL elapses
	st.GetUser(ctx, "ghost")
	fmt.Printf("after negative TTL: repo calls=%d\n", repo.calls.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
jitter: all 20 expiries in (5m, 6m]: true, decorrelated: true
two hits for alice: repo calls=1
two misses for ghost: repo calls=2, both not-found: true
after negative TTL: repo calls=3
```

### Tests

The jitter test brackets the window without a fake clock: capture `before`
and `after` timestamps around the `SetJittered` calls, then assert every
stored deadline is at least `before+ttl` and at most `after+ttl+maxJitter`.
The decorrelation assertion — at least two distinct offsets among 100 draws
over a nanosecond-granular window — cannot realistically collide. The store
tests use a counting fake repo and short real TTLs, asserting call counts
(deterministic) rather than timings.

Create `cache/jitter_test.go`:

```go
package cache

import (
	"fmt"
	"testing"
	"time"
)

func TestSetJitteredExpiryWithinWindow(t *testing.T) {
	t.Parallel()

	c := New[int](4)
	const ttl = time.Hour
	const frac = 0.2
	maxJitter := time.Duration(float64(ttl) * frac)

	before := time.Now()
	for i := range 100 {
		c.SetJittered(fmt.Sprintf("k-%d", i), i, ttl, frac)
	}
	after := time.Now()

	var first time.Time
	distinct := false
	for i := range 100 {
		exp, ok := c.ExpiresAt(fmt.Sprintf("k-%d", i))
		if !ok {
			t.Fatalf("k-%d missing", i)
		}
		if exp.Before(before.Add(ttl)) {
			t.Fatalf("k-%d expires %v early; jitter must only extend", i, before.Add(ttl).Sub(exp))
		}
		if exp.After(after.Add(ttl + maxJitter)) {
			t.Fatalf("k-%d expires beyond ttl+maxJitter", i)
		}
		if i == 0 {
			first = exp
		} else if !exp.Equal(first) {
			distinct = true
		}
	}
	if !distinct {
		t.Fatal("all 100 jittered expiries identical; expiry is still synchronized")
	}
}

func TestSetJitteredDegenerateCasesFallBackToSet(t *testing.T) {
	t.Parallel()

	c := New[int](4)
	c.SetJittered("never", 1, 0, 0.2) // ttl<=0: no expiry
	if exp, ok := c.ExpiresAt("never"); !ok || !exp.IsZero() {
		t.Fatalf("ExpiresAt(never) = %v,%v, want zero,true", exp, ok)
	}

	c.SetJittered("plain", 2, time.Hour, 0) // no jitter fraction
	exp, ok := c.ExpiresAt("plain")
	if !ok || exp.IsZero() {
		t.Fatal("plain entry missing or unexpiring")
	}
	if exp.After(time.Now().Add(time.Hour)) {
		t.Fatal("zero jitterFrac must not extend the TTL")
	}
}

func ExampleCache_SetJittered() {
	c := New[string](1)
	c.SetJittered("k", "v", time.Hour, 0.1)
	v, ok := c.Get("k")
	fmt.Println(v, ok)
	// Output: v true
}
```

Create `store/store_test.go`:

```go
package store

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

var errDown = errors.New("connection refused")

type fakeRepo struct {
	calls atomic.Int64
	users map[string]*User
	err   error
}

func (r *fakeRepo) FindUser(ctx context.Context, id string) (*User, error) {
	r.calls.Add(1)
	if r.err != nil {
		return nil, r.err
	}
	if u, ok := r.users[id]; ok {
		return u, nil
	}
	return nil, ErrNotFound
}

func TestNegativeResultCachedOnce(t *testing.T) {
	t.Parallel()

	repo := &fakeRepo{users: map[string]*User{}}
	st := New(repo, time.Minute, time.Minute)

	for range 2 {
		_, err := st.GetUser(t.Context(), "ghost")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want wrapped ErrNotFound", err)
		}
	}
	if got := repo.calls.Load(); got != 1 {
		t.Fatalf("repo calls = %d, want 1 (second miss served from cache)", got)
	}
}

func TestNegativeTTLExpiryReconsultsRepo(t *testing.T) {
	t.Parallel()

	repo := &fakeRepo{users: map[string]*User{}}
	st := New(repo, time.Minute, 20*time.Millisecond)

	st.GetUser(t.Context(), "ghost")
	time.Sleep(40 * time.Millisecond) // negative TTL elapses
	st.GetUser(t.Context(), "ghost")

	if got := repo.calls.Load(); got != 2 {
		t.Fatalf("repo calls = %d, want 2 (negative entry must expire)", got)
	}
}

func TestPositiveResultCached(t *testing.T) {
	t.Parallel()

	repo := &fakeRepo{users: map[string]*User{"alice": {ID: "alice", Name: "Alice"}}}
	st := New(repo, time.Minute, time.Second)

	for range 3 {
		u, err := st.GetUser(t.Context(), "alice")
		if err != nil || u.Name != "Alice" {
			t.Fatalf("GetUser = %v, %v", u, err)
		}
	}
	if got := repo.calls.Load(); got != 1 {
		t.Fatalf("repo calls = %d, want 1", got)
	}
}

func TestTransientErrorsNotCached(t *testing.T) {
	t.Parallel()

	repo := &fakeRepo{err: errDown}
	st := New(repo, time.Minute, time.Minute)

	for range 2 {
		_, err := st.GetUser(t.Context(), "alice")
		if !errors.Is(err, errDown) {
			t.Fatalf("err = %v, want wrapped errDown", err)
		}
		if errors.Is(err, ErrNotFound) {
			t.Fatal("transient error must not become ErrNotFound")
		}
	}
	if got := repo.calls.Load(); got != 2 {
		t.Fatalf("repo calls = %d, want 2 (outage must not be cached)", got)
	}
}

func TestInvalidateUnhidesCreatedUser(t *testing.T) {
	t.Parallel()

	repo := &fakeRepo{users: map[string]*User{}}
	st := New(repo, time.Minute, time.Minute)

	st.GetUser(t.Context(), "new-user") // caches the miss
	repo.users["new-user"] = &User{ID: "new-user", Name: "New"}

	// Without Invalidate, the cached miss would hide the new row.
	st.Invalidate("new-user")
	u, err := st.GetUser(t.Context(), "new-user")
	if err != nil || u.Name != "New" {
		t.Fatalf("GetUser after Invalidate = %v, %v; want the created user", u, err)
	}
}

func ExampleStore_GetUser() {
	repo := &fakeRepo{users: map[string]*User{}}
	st := New(repo, time.Minute, time.Minute)
	_, err := st.GetUser(context.Background(), "ghost")
	fmt.Println(errors.Is(err, ErrNotFound))
	// Output: true
}
```

Run the gate:

```bash
gofmt -l . && go vet ./... && go test -count=1 -race ./...
```

## Review

Jitter's contract is asymmetric on purpose: it may extend a TTL, never shorten
it, so no consumer sees data die early and the only cost is slightly longer
staleness for some keys. If you ever port this, keep the `rand.N` guard —
it panics on non-positive arguments, and a zero `maxJitter` from a tiny
`ttl*frac` product truncating to zero is exactly the kind of input production
finds for you.

Negative caching earns its keep only with discipline about *which* errors are
answers. `ErrNotFound` is a fact about the data; a timeout is a fact about the
infrastructure — cache the first, never the second, and the
`TestTransientErrorsNotCached` case is the regression trap for that
distinction. The `Invalidate` test encodes the operational pairing: every
create/update path in a service with negative caching must invalidate, or new
entities stay invisible for a negative TTL. And note how the `result` envelope
made presence (`ok`) and existence (`missing`) independent bits — the same
envelope trick that resolves the nil-pointer `(nil, true)` ambiguity.

## Resources

- [`math/rand/v2`](https://pkg.go.dev/math/rand/v2) — `rand.N`, its generic signature over `time.Duration`, and concurrency safety.
- [Evolving the Go Standard Library with math/rand/v2](https://go.dev/blog/randv2) — why v2 exists and what changed from v1.
- [`errors` package](https://pkg.go.dev/errors) — sentinel errors, `%w` wrapping, and `errors.Is`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-lru-bounded-shards.md](05-lru-bounded-shards.md) | Next: [07-stale-while-revalidate.md](07-stale-while-revalidate.md)
