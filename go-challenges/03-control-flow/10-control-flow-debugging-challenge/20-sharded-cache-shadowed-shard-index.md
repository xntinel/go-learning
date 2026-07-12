# Exercise 20: Sharded Cache Writes to Wrong Partition Due to Shadowed Shard Index Variable

**Nivel: Intermedio** — validacion rapida (un test corto).

A sharded cache that hashes most keys to a shard but pins a special class
of keys (an account tier, a tenant ID) to shard 0 for operational
locality — so a maintenance job can scan one shard instead of the whole
cache — has to actually *overwrite* the computed index for that special
case, not merely compute a new one that goes nowhere. `idx := 0` inside
the `if` block declares a brand-new, block-scoped `idx` that shadows the
outer one; the write then proceeds with the *original* hash-based index,
silently defeating the pinning rule while every other read/write path
stays internally consistent enough that nothing looks wrong from the
cache's own `Get`. This module is fully self-contained: its own
`go mod init`, all code inline, its own demo and tests.

## What you'll build

```text
shardcache/                 independent module: example.com/sharded-cache-shadowed-shard-index
  go.mod
  shardcache.go              Cache, PinnedPrefix, Set, Get, ShardKeys
  cmd/
    demo/
      main.go                runnable demo: a pinned key and a plain key land on different shards
  shardcache_test.go          pinned key found on shard 0, not its hash-based shard
```

- Files: `shardcache.go`, `cmd/demo/main.go`, `shardcache_test.go`.
- Implement: `Cache` (`Set`, `Get`, `ShardKeys`) where keys with `PinnedPrefix` are routed to shard 0 regardless of their hash.
- Test: set a pinned key whose hash lands on a different shard and assert `ShardKeys(0)` contains it directly — not merely that `Get` can still find it.
- Verify: `go test -count=1 ./...`.

```bash
mkdir -p go-solutions/03-control-flow/10-control-flow-debugging-challenge/20-sharded-cache-shadowed-shard-index/cmd/demo
cd go-solutions/03-control-flow/10-control-flow-debugging-challenge/20-sharded-cache-shadowed-shard-index
```

### Why Get alone cannot catch this bug

`Set` computes a hash-based `idx`, then is supposed to override it for
pinned keys:

```go
idx := c.shardIndex(key)
if strings.HasPrefix(key, PinnedPrefix) {
	idx := 0 // BUG: := declares a new, if-block-scoped idx
}

s := c.shards[idx] // always the original hash-based idx; the override never happened
```

`idx := 0` inside the `if` block is a fresh declaration, valid only for
the lifetime of that block; it shadows the outer `idx` the same way a
nested `if item, err := ...; err != nil` shadows an outer `err`. Once the
block ends, that inner `idx` is gone and `c.shards[idx]` on the next line
reads the *outer* `idx` — the hash-based one, completely untouched by the
`if` body. The insidious part is that `Get`, if it applies the exact same
(broken) rule, will *also* recompute the plain hash-based index and find
the value exactly where `Set` actually put it — so a round-trip
`Set`/`Get` through the cache's own API looks perfectly correct. The bug
only becomes visible to code that assumes shard locality directly: a
maintenance job that scans shard 0 for every pinned key without going
through `Get` finds nothing there, because every pinned key silently
landed on its ordinary hash-based shard instead. The fix is one character:
`idx = 0`, a plain assignment to the already-declared outer variable, with
no `:=` and therefore no new scope to lose it in.

Create `shardcache.go`:

```go
package shardcache

import (
	"hash/fnv"
	"strings"
	"sync"
)

// PinnedPrefix routes any key with this prefix to shard 0 explicitly, so a
// maintenance job that needs every pinned key can scan shard 0 alone
// without locking the whole cache.
const PinnedPrefix = "pinned:"

type shard struct {
	mu   sync.Mutex
	data map[string]string
}

// Cache is a fixed-size sharded key/value store.
type Cache struct {
	shards []*shard
}

// New creates a Cache with n shards.
func New(n int) *Cache {
	c := &Cache{shards: make([]*shard, n)}
	for i := range c.shards {
		c.shards[i] = &shard{data: make(map[string]string)}
	}
	return c
}

// shardIndex hashes key to a shard number in [0, len(shards)).
func (c *Cache) shardIndex(key string) int {
	h := fnv.New32a()
	h.Write([]byte(key))
	return int(h.Sum32()) % len(c.shards)
}

// Set stores value under key. Pinned keys are routed to shard 0 regardless
// of their hash, so operational tooling can find every pinned key by
// scanning shard 0 alone.
func (c *Cache) Set(key, value string) {
	idx := c.shardIndex(key)
	if strings.HasPrefix(key, PinnedPrefix) {
		idx = 0
	}

	s := c.shards[idx]
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = value
}

// Get looks up key using the same routing rule Set uses.
func (c *Cache) Get(key string) (string, bool) {
	idx := c.shardIndex(key)
	if strings.HasPrefix(key, PinnedPrefix) {
		idx = 0
	}

	s := c.shards[idx]
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[key]
	return v, ok
}

// ShardKeys returns every key currently stored in shard idx, for tooling
// that inspects one shard at a time instead of the whole cache.
func (c *Cache) ShardKeys(idx int) []string {
	s := c.shards[idx]
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.data))
	for k := range s.data {
		keys = append(keys, k)
	}
	return keys
}
```

### The runnable demo

The demo sets a pinned key and a plain key and prints which shard each
physically landed on.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/sharded-cache-shadowed-shard-index"
)

func main() {
	c := shardcache.New(4)

	c.Set("pinned:acct-1", "gold-tier")
	c.Set("normal-key-1", "some-value")

	fmt.Println("shard 0 keys:", c.ShardKeys(0))
	fmt.Println("shard 2 keys:", c.ShardKeys(2))

	v, ok := c.Get("pinned:acct-1")
	fmt.Println("get pinned:acct-1:", v, ok)
}
```

```bash
go run ./cmd/demo
```

Expected output:

```text
shard 0 keys: [pinned:acct-1]
shard 2 keys: [normal-key-1]
get pinned:acct-1: gold-tier true
```

### Tests

`TestPinnedKeyLandsOnShardZero` uses a pinned key whose plain hash lands
on shard 2, so the only way it can appear on shard 0 is if the pinning
rule actually took effect — a test built around `Get` alone would pass on
the buggy version too, since `Get` recomputes the same broken route.

Create `shardcache_test.go`:

```go
package shardcache

import "testing"

func TestPinnedKeyLandsOnShardZero(t *testing.T) {
	c := New(4)

	// "pinned:acct-1" hashes to shard 2 on a plain hash-based route; the
	// PinnedPrefix rule must override that and place it on shard 0 instead.
	c.Set("pinned:acct-1", "gold-tier")

	keys := c.ShardKeys(0)
	found := false
	for _, k := range keys {
		if k == "pinned:acct-1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("shard 0 keys = %v, want to contain %q", keys, "pinned:acct-1")
	}

	otherKeys := c.ShardKeys(2)
	for _, k := range otherKeys {
		if k == "pinned:acct-1" {
			t.Fatalf("pinned key landed on shard 2 (its hash-based shard) instead of the pinned shard 0")
		}
	}
}

func TestGetReturnsWhatSetStored(t *testing.T) {
	c := New(4)
	c.Set("pinned:acct-1", "gold-tier")
	c.Set("normal-key-1", "some-value")

	if v, ok := c.Get("pinned:acct-1"); !ok || v != "gold-tier" {
		t.Fatalf("Get(pinned:acct-1) = %q, %v, want %q, true", v, ok, "gold-tier")
	}
	if v, ok := c.Get("normal-key-1"); !ok || v != "some-value" {
		t.Fatalf("Get(normal-key-1) = %q, %v, want %q, true", v, ok, "some-value")
	}
}
```

Run: `go test -count=1 ./...`.

## Review

`Set` is correct when a pinned key is found on shard 0 by inspecting shard
contents directly, not merely when `Get` can still retrieve it — because
`Get` recomputing the same override rule makes the API self-consistent
even when the rule silently does nothing. The tell for this class of bug
is a variable that is declared once outside a conditional and then never
visibly reassigned by name inside it — `idx := 0` reads as an assignment
but is a declaration the moment it appears in a scope where `idx` was not
already declared. The fix is always the same one-character change: drop
the `:=` to `=` so the statement writes through to the variable the rest
of the function actually reads.

## Resources

- [Go Specification: Declarations and scope](https://go.dev/ref/spec#Declarations_and_scope) — `:=` inside a nested block declares new bindings scoped to that block.
- [go vet -shadow](https://pkg.go.dev/golang.org/x/tools/go/analysis/passes/shadow) — an analyzer built specifically to flag variables shadowed by an inner declaration of the same name.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [19-log-pipeline-drain-skipped-by-continue.md](19-log-pipeline-drain-skipped-by-continue.md) | Next: [21-circuit-breaker-half-open-guard-missing.md](21-circuit-breaker-half-open-guard-missing.md)
