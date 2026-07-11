# Exercise 13: Sharded Ring Buffers and the Cost of a Bad Hash

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A single mutex-guarded ring, the shape built in Exercise 4, works until one
lock becomes the bottleneck a high-throughput ingestion path is waiting on.
The standard answer -- used by the Linux kernel's per-CPU `perf_event` and
`ftrace` ring buffers, and by the striped-lock design underneath `sync.Map`
-- is to stop sharing one ring and instead keep N independent rings, each
with its own lock, and route every write to exactly one of them by hashing a
key. Two writers touching different keys never contend; two writers touching
the same key still serialize, exactly as a single mutex-guarded ring would,
but only against each other, not against every other key in the system.

The part of this design that is easy to get right by accident and easy to
get catastrophically wrong under real traffic is the function that picks the
shard. It has to look like a fair coin across the whole key space, not just
vary when the input happens to vary in whatever one detail the naive version
inspects. Production keys are rarely uniformly random strings -- they are
tenant IDs, event-type names, route prefixes -- and they routinely share a
long common prefix. A shard selector built around the wrong slice of the key
can stare at a thousand different tenant IDs and route every single one of
them to the same shard, silently rebuilding the one-lock bottleneck that
sharding was supposed to remove, with N times the memory and none of the
benefit.

This module builds `Sharded[T]`, a fixed number of independent, per-shard
mutex-guarded rings selected by hashing the full key with FNV-1a, plus the
accessors a caller needs to inspect one shard at a time.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
shardring/                module example.com/shardring
  go.mod                   go 1.24
  shardring.go             Sharded[T]; New, ShardFor, Push, ShardLen, Snapshot;
                           three sentinel errors
  shardring_test.go         config validation, FIFO-within-a-shard, key stability,
                           out-of-range access, aliasing, the naive-hash contrast,
                           concurrency, ExampleSharded_Push
```

- Files: `shardring.go`, `shardring_test.go`.
- Implement: `New[T any](numShards, capacityPerShard int) (*Sharded[T], error)` rejecting a non-positive shard count with `ErrInvalidShardCount` and a non-positive capacity with `ErrInvalidCapacity`; `(*Sharded[T]).ShardFor(key string) int` hashing the full key with FNV-1a; `(*Sharded[T]).Push(key string, v T)` writing into that shard's ring, overwrite-oldest when full; `(*Sharded[T]).ShardLen(idx int) (int, error)` and `(*Sharded[T]).Snapshot(idx int) ([]T, error)` both returning `ErrShardIndexOutOfRange` for an invalid index.
- Test: config validation for both sentinel errors; FIFO overwrite-oldest behavior inside one shard; the same key always resolving to the same shard; out-of-range indices rejected by both accessors; `Snapshot` never aliasing shard storage; the naive first-byte selector collapsing a batch of prefixed keys onto one shard while `ShardFor` spreads them; concurrent `Push` across many keys under `-race`; and `ExampleSharded_Push` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/shardring
cd ~/go-exercises/shardring
go mod init example.com/shardring
go mod edit -go=1.24
```

### Sharding trades one lock for N, but only if the hash actually spreads keys

Sharding is a mechanical transformation: instead of one `Ring[T]` behind one
`sync.Mutex`, keep a slice of `N` of them, and send `Push(key, v)` to
`shards[hash(key) % N]`. Every property this lesson already established about
a single mutex-guarded ring -- overwrite-oldest eviction, a fresh `Snapshot`
that never aliases internal storage, a `size` counter that disambiguates
`head == tail` -- holds unchanged *inside* each shard. What sharding adds is
entirely in the routing function, and that function is where the naive
version fails silently:

```go
func naiveShardFor(key string, numShards int) int {
    return int(key[0]) % numShards   // "hash" by the first byte
}
```

It compiles. It genuinely does route different keys to different shards --
as long as those keys happen to start with different bytes. The moment a
caller feeds it a realistic batch of keys, that assumption collapses:
`"tenant-001"`, `"tenant-002"`, ..., `"tenant-999"` all begin with the byte
`'t'`, so `int('t') % numShards` is the *same* number for every one of them,
regardless of how many shards exist. Eight shards, one used. The other seven
sit empty while every producer serializes on the one lock the sharding was
built to avoid -- and nothing about this fails a build, a lint pass, or a
casual smoke test with two or three hand-picked keys that happen to differ
in their first byte.

`ShardFor` in this package hashes the entire key with `hash/fnv`'s FNV-1a
implementation instead. FNV-1a folds every byte of the input into the
running hash, so two keys that differ anywhere -- not just in one
pre-chosen position -- are overwhelmingly likely to land in different
buckets. It is deliberately not `hash/maphash`: `maphash.Seed` is randomized
per process by design (it exists to make Go's own map iteration order
unpredictable and resist hash-flooding attacks), which would make `ShardFor`
return a different answer on every run and turn any test asserting a
specific shard index into a coin flip. FNV-1a is a pure, seedless,
deterministic function of its input, which is exactly what a reproducible
test -- and a reproducible on-call debugging session, where "which shard is
key X in" needs one stable answer -- requires.

Create `shardring.go`:

```go
// Package shardring implements a fixed number of independent ring buffers,
// selected by hashing a string key, so that concurrent writers touching
// different keys never contend on the same mutex.
//
// It exists to get one detail right that a hand-rolled sharding scheme
// routinely gets wrong: the function that picks a shard must spread real
// keys across the whole shard space, not just vary when the input happens
// to vary in the one byte the naive version looks at. Shard selection built
// from a single byte of the key collapses every key that shares that byte
// into one shard, silently turning N independent locks back into one --
// exactly the bottleneck sharding was meant to remove. See the package
// tests for a side-by-side demonstration.
package shardring

import (
	"errors"
	"fmt"
	"hash/fnv"
	"sync"
)

// Sentinel errors returned by New and by the per-shard accessors. Callers
// should test for them with errors.Is rather than by comparing error
// strings.
var (
	// ErrInvalidShardCount means the requested number of shards was not positive.
	ErrInvalidShardCount = errors.New("shardring: shard count must be positive")
	// ErrInvalidCapacity means the requested per-shard capacity was not positive.
	ErrInvalidCapacity = errors.New("shardring: capacity must be positive")
	// ErrShardIndexOutOfRange means a shard index was outside [0, NumShards).
	ErrShardIndexOutOfRange = errors.New("shardring: shard index out of range")
)

// shard is one fixed-capacity, overwrite-oldest ring guarded by its own
// mutex. Multiple shards never share a lock.
type shard[T any] struct {
	mu   sync.Mutex
	data []T
	head int
	tail int
	size int
}

// Sharded is NumShards independent ring buffers of equal capacity, each
// guarded by its own mutex, with the shard for a given key chosen by
// hashing the key with FNV-1a.
//
// Sharded is safe for concurrent use by multiple goroutines. Two goroutines
// pushing to keys that hash to different shards proceed without blocking
// each other; two goroutines pushing to the same shard serialize on that
// shard's mutex, exactly as a single mutex-guarded ring would.
type Sharded[T any] struct {
	shards []*shard[T]
}

// New returns a Sharded ring with numShards independent shards, each holding
// up to capacityPerShard elements. It returns ErrInvalidShardCount or
// ErrInvalidCapacity if either argument is not positive.
func New[T any](numShards, capacityPerShard int) (*Sharded[T], error) {
	if numShards <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidShardCount, numShards)
	}
	if capacityPerShard <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidCapacity, capacityPerShard)
	}
	shards := make([]*shard[T], numShards)
	for i := range shards {
		shards[i] = &shard[T]{data: make([]T, capacityPerShard)}
	}
	return &Sharded[T]{shards: shards}, nil
}

// NumShards reports how many independent shards this Sharded ring has.
func (s *Sharded[T]) NumShards() int { return len(s.shards) }

// ShardFor reports which shard Push(key, ...) would write to. It hashes the
// full key with FNV-1a rather than looking at a single byte, so keys that
// differ anywhere -- not just in one chosen position -- land in different
// shards with reasonably even spread.
func (s *Sharded[T]) ShardFor(key string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return int(h.Sum32() % uint32(len(s.shards)))
}

// Push appends v to the shard selected by key, overwriting that shard's
// oldest entry if it is full. It is safe to call concurrently for any
// combination of keys.
func (s *Sharded[T]) Push(key string, v T) {
	sh := s.shards[s.ShardFor(key)]
	sh.mu.Lock()
	defer sh.mu.Unlock()

	if sh.size == len(sh.data) {
		var zero T
		sh.data[sh.tail] = zero // drop the reference before overwriting elsewhere
		sh.tail = (sh.tail + 1) % len(sh.data)
		sh.size--
	}
	sh.data[sh.head] = v
	sh.head = (sh.head + 1) % len(sh.data)
	sh.size++
}

// ShardLen reports how many elements are currently stored in shard idx. It
// returns ErrShardIndexOutOfRange if idx is not a valid shard index.
func (s *Sharded[T]) ShardLen(idx int) (int, error) {
	if idx < 0 || idx >= len(s.shards) {
		return 0, fmt.Errorf("%w: got %d, have %d shards", ErrShardIndexOutOfRange, idx, len(s.shards))
	}
	sh := s.shards[idx]
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return sh.size, nil
}

// Snapshot returns a fresh copy of shard idx's contents, oldest first. The
// returned slice is newly allocated and never aliases the shard's internal
// storage; the caller may retain and mutate it freely. It returns
// ErrShardIndexOutOfRange if idx is not a valid shard index.
func (s *Sharded[T]) Snapshot(idx int) ([]T, error) {
	if idx < 0 || idx >= len(s.shards) {
		return nil, fmt.Errorf("%w: got %d, have %d shards", ErrShardIndexOutOfRange, idx, len(s.shards))
	}
	sh := s.shards[idx]
	sh.mu.Lock()
	defer sh.mu.Unlock()

	out := make([]T, 0, sh.size)
	for i, pos := 0, sh.tail; i < sh.size; i, pos = i+1, (pos+1)%len(sh.data) {
		out = append(out, sh.data[pos])
	}
	return out, nil
}
```

### Using it

Construct one `Sharded[T]` at startup with a shard count sized to your
expected concurrency (a small multiple of `GOMAXPROCS` is a common starting
point) and a per-shard capacity sized the same way a single ring's capacity
would be. Every producer calls `Push(key, v)`; the package routes the write
without the caller ever needing to know which shard it landed in, though
`ShardFor` is exported precisely so a caller *can* ask -- for a debug
endpoint, or for a test that wants to assert something about one shard in
isolation, as this module's own tests do.

Two contracts matter to a caller reading data back out. `ShardFor` is a pure
function of the key: the same key always maps to the same shard for the
lifetime of the `Sharded` value, which `TestSameKeyAlwaysMapsToTheSameShard`
pins. And `Snapshot` returns an independent copy, never a view into a
shard's live storage, so a caller can sort, marshal, or mutate the result
freely -- `TestSnapshotDoesNotAliasShardStorage` holds that promise to
account.

`ExampleSharded_Push` in the test file is the runnable demonstration of this
module: `go test` executes it and compares its stdout against the
`// Output:` comment, so the usage shown there cannot drift from the code.

### Tests

`TestNewRejectsInvalidConfig` covers both sentinel errors.
`TestPushOverwritesOldestWithinItsShard` confirms the per-shard ring behaves
like any other ring in this lesson once you know which shard you are
looking at. `TestSameKeyAlwaysMapsToTheSameShard` and
`TestShardLenAndSnapshotRejectOutOfRangeIndex` pin the two accessor
contracts described above. `TestSnapshotDoesNotAliasShardStorage` mutates a
returned snapshot and confirms a second `Snapshot` call is unaffected.

`TestNaiveFirstByteHashCollapsesSharedPrefixKeys` is the heart of the
module. `naiveShardFor` is unexported and unreachable from the package API;
the test feeds both it and `ShardFor` the same batch of twenty
`"tenant-NNN"` keys -- realistic, prefix-sharing production keys, not random
strings -- and asserts a property rather than an exact distribution: the
naive selector must land all twenty keys on exactly one shard (the bug,
stated numerically), while `ShardFor` must spread them across more than one.
`TestConcurrentPushAcrossKeysIsSafe` drives thirty-two goroutines, each
hammering its own key, and confirms the total element count across every
shard is sane under `-race`.

Create `shardring_test.go`:

```go
package shardring

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// naiveShardFor is the shard-selection function as it is usually written
// the first time: "hash" by taking the key's first byte modulo the shard
// count. It is never exported and never reachable from the package API; it
// exists only so the tests can pin what it gets wrong. Any set of keys that
// share a first byte -- a common tenant-ID or event-type prefix, which is
// the normal case in production -- collapses onto a single shard no matter
// how many shards exist, defeating the entire point of sharding.
func naiveShardFor(key string, numShards int) int {
	if key == "" {
		return 0
	}
	return int(key[0]) % numShards
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	t.Parallel()

	if _, err := New[int](0, 4); !errors.Is(err, ErrInvalidShardCount) {
		t.Errorf("New(shards=0) error = %v, want ErrInvalidShardCount", err)
	}
	if _, err := New[int](-1, 4); !errors.Is(err, ErrInvalidShardCount) {
		t.Errorf("New(shards=-1) error = %v, want ErrInvalidShardCount", err)
	}
	if _, err := New[int](4, 0); !errors.Is(err, ErrInvalidCapacity) {
		t.Errorf("New(cap=0) error = %v, want ErrInvalidCapacity", err)
	}
	if _, err := New[int](4, -1); !errors.Is(err, ErrInvalidCapacity) {
		t.Errorf("New(cap=-1) error = %v, want ErrInvalidCapacity", err)
	}
}

func TestPushOverwritesOldestWithinItsShard(t *testing.T) {
	t.Parallel()

	s, err := New[int](8, 3)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	key := "sensor-1"
	idx := s.ShardFor(key)
	for _, v := range []int{1, 2, 3, 4, 5} {
		s.Push(key, v)
	}
	got, err := s.Snapshot(idx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	want := []int{3, 4, 5}
	if len(got) != len(want) {
		t.Fatalf("Snapshot = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Snapshot = %v, want %v", got, want)
		}
	}
}

func TestSameKeyAlwaysMapsToTheSameShard(t *testing.T) {
	t.Parallel()

	s, err := New[int](16, 4)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, key := range []string{"", "a", "tenant-42", "日本語-key"} {
		first := s.ShardFor(key)
		for i := 0; i < 5; i++ {
			if got := s.ShardFor(key); got != first {
				t.Fatalf("ShardFor(%q) = %d on call %d, want stable %d", key, got, i, first)
			}
		}
	}
}

func TestShardLenAndSnapshotRejectOutOfRangeIndex(t *testing.T) {
	t.Parallel()

	s, err := New[int](4, 2)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, idx := range []int{-1, 4, 99} {
		if _, err := s.ShardLen(idx); !errors.Is(err, ErrShardIndexOutOfRange) {
			t.Errorf("ShardLen(%d) error = %v, want ErrShardIndexOutOfRange", idx, err)
		}
		if _, err := s.Snapshot(idx); !errors.Is(err, ErrShardIndexOutOfRange) {
			t.Errorf("Snapshot(%d) error = %v, want ErrShardIndexOutOfRange", idx, err)
		}
	}
}

func TestSnapshotDoesNotAliasShardStorage(t *testing.T) {
	t.Parallel()

	s, err := New[string](4, 4)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	key := "k"
	s.Push(key, "original")
	idx := s.ShardFor(key)

	got, err := s.Snapshot(idx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	got[0] = "mutated"

	again, err := s.Snapshot(idx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if again[0] != "original" {
		t.Fatalf("mutating a returned snapshot corrupted shard storage: %q", again[0])
	}
}

// TestNaiveFirstByteHashCollapsesSharedPrefixKeys is the whole point of the
// module: a realistic batch of keys that share a common prefix -- exactly
// what tenant IDs, event types, or route names look like in production --
// all land on the same shard under the naive first-byte selector, while the
// FNV-1a selector spreads them out. The test asserts the property (all one
// shard versus more than one shard), never an exact distribution, because
// the precise spread of a hash function is not a contract worth pinning.
func TestNaiveFirstByteHashCollapsesSharedPrefixKeys(t *testing.T) {
	t.Parallel()

	const numShards = 8
	keys := make([]string, 20)
	for i := range keys {
		keys[i] = fmt.Sprintf("tenant-%03d", i) // shared 't' prefix
	}

	naiveShards := map[int]bool{}
	for _, k := range keys {
		naiveShards[naiveShardFor(k, numShards)] = true
	}
	if len(naiveShards) != 1 {
		t.Fatalf("naiveShardFor spread %d prefixed keys across %d shards, want exactly 1 (that is the bug)", len(keys), len(naiveShards))
	}

	s, err := New[int](numShards, 4)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	correctShards := map[int]bool{}
	for _, k := range keys {
		correctShards[s.ShardFor(k)] = true
	}
	if len(correctShards) <= 1 {
		t.Fatalf("ShardFor spread %d prefixed keys across only %d shard(s), want more than 1", len(keys), len(correctShards))
	}
}

func TestConcurrentPushAcrossKeysIsSafe(t *testing.T) {
	t.Parallel()

	s, err := New[int](16, 64)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var wg sync.WaitGroup
	for k := 0; k < 32; k++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			key := fmt.Sprintf("worker-%d", k)
			for v := 0; v < 50; v++ {
				s.Push(key, v)
			}
		}(k)
	}
	wg.Wait()

	var total int
	for idx := 0; idx < s.NumShards(); idx++ {
		n, err := s.ShardLen(idx)
		if err != nil {
			t.Fatalf("ShardLen(%d): %v", idx, err)
		}
		total += n
	}
	if total == 0 {
		t.Fatal("total elements across all shards is 0 after concurrent pushes")
	}
}

// ExampleSharded_Push is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExampleSharded_Push() {
	s, err := New[string](4, 2)
	if err != nil {
		panic(err)
	}

	s.Push("order-1", "created")
	s.Push("order-1", "paid")
	s.Push("order-2", "created")

	idx := s.ShardFor("order-1")
	page, err := s.Snapshot(idx)
	if err != nil {
		panic(err)
	}
	fmt.Println(page)

	n, err := s.ShardLen(idx)
	if err != nil {
		panic(err)
	}
	fmt.Println("shard size:", n)

	// Output:
	// [created paid]
	// shard size: 2
}
```

## Review

`Sharded[T]` is correct when two guarantees both hold: within any one shard,
the ring behaves exactly like the single-goroutine ring this lesson started
with, FIFO overwrite-oldest and all; and across the whole key space, real
keys actually spread across the available shards instead of collapsing onto
one. The second guarantee is the one a naive implementation loses first,
because a selector built from a single byte of the key looks correct on any
handful of test keys that happen to differ in that byte and only reveals its
flaw against a realistic batch that shares a prefix -- exactly the shape
production keys usually have. FNV-1a over the full key avoids that failure
mode without needing a random seed, which keeps shard assignment
deterministic and testable. Around that core, `New` rejects a non-positive
shard count or capacity with its own sentinel error, `ShardLen` and
`Snapshot` reject an out-of-range index with `ErrShardIndexOutOfRange`, and
`Snapshot` never aliases a shard's internal storage. Every shard has its own
mutex, so `Sharded[T]` is safe for concurrent use and two goroutines
touching different keys never block each other. `ExampleSharded_Push` is the
executable documentation: `go test` verifies its output. Run
`go test -count=1 -race ./...`.

## Resources

- [`hash/fnv`](https://pkg.go.dev/hash/fnv) — the FNV-1a implementation used to hash the full key.
- [`hash/maphash`](https://pkg.go.dev/hash/maphash) — why a randomized-seed hash is the wrong tool when you need a reproducible shard assignment.
- [Linux kernel perf ring buffer design](https://www.kernel.org/doc/html/latest/trace/ring-buffer-design.html) — the per-CPU sharded ring buffer this module's shape is modeled on.
- [`sync.Map`](https://pkg.go.dev/sync#Map) — the standard library's own answer to lock contention under many independent keys.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-sliding-window-rate-limiter-count-vs-time.md](12-sliding-window-rate-limiter-count-vs-time.md) | Next: [14-clock-second-chance-page-cache.md](14-clock-second-chance-page-cache.md)
