# Exercise 12: Tiered Cache Fallback: L1, L2, Then Origin

A read-heavy service often layers caches: a small fast tier, a bigger
slower tier, and the expensive origin behind both. This module builds the
lookup chain as three comma-ok `if`s in sequence — L1, then L2, then
origin — where an L2 hit gets promoted into L1 so the next lookup for that
key is fast.

## What you'll build

```text
tieredcache/                 independent module: example.com/tiered-cache-fallback
  go.mod                    go 1.24
  cache.go                  Cache, New(origin), Get(key) (string, bool)
  cache_test.go             table: l2 hit promotes to l1, cold key hits origin, miss
```

- Files: `cache.go`, `cache_test.go`.
- Implement: `Get(key string) (string, bool)` on `*Cache` using `if v, ok := c.l1[key]; ok { return v, true }`, then the same shape against `l2` (promoting the hit into `l1`), then `origin(key)` (caching the hit into `l2`), falling through to a final miss.
- Test: a table that verifies which tier answers each lookup, that origin is not called on an L1 or L2 hit, and that a hit is promoted into the tier above it.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why three separate `if`s, not one loop over the tiers

Each tier has a different consequence on a hit: an L1 hit does nothing
extra; an L2 hit must be copied up so it stays fast next time; an origin
hit must be copied into L2 so the next lookup skips the origin call
entirely. A loop over `[]map[string]string{l1, l2}` would need a branch
inside it to know which promotion applies anyway — the three `if`s in
sequence are the plainest way to express three different actions attached
to three different sources, and each stays a one-line guard: on a hit,
return; otherwise fall to the next tier.

Create `cache.go`:

```go
// Package tieredcache reads through two cache tiers before falling back to
// an origin lookup, promoting each miss into the tiers below it.
package tieredcache

// Origin looks up a value that was not found in either cache tier. The bool
// follows the comma-ok convention: false means "no such key," not an error.
type Origin func(key string) (string, bool)

// Cache holds two map-backed tiers plus the origin fallback. L1 is checked
// first, then L2, then Origin; a hit in a lower tier is promoted upward so
// the next lookup for the same key is faster.
type Cache struct {
	l1     map[string]string
	l2     map[string]string
	origin Origin
}

// New builds a Cache backed by fresh, empty L1 and L2 tiers.
func New(origin Origin) *Cache {
	return &Cache{
		l1:     make(map[string]string),
		l2:     make(map[string]string),
		origin: origin,
	}
}

// Get resolves key through L1, then L2, then origin, promoting each miss:
// an L2 hit is copied into L1, and an origin hit is copied into L2 (not L1,
// so a cold key still costs one extra lookup before it is L1-hot). The
// second return value reports whether key resolved at all.
func (c *Cache) Get(key string) (string, bool) {
	if v, ok := c.l1[key]; ok {
		return v, true
	}

	if v, ok := c.l2[key]; ok {
		c.l1[key] = v
		return v, true
	}

	if v, ok := c.origin(key); ok {
		c.l2[key] = v
		return v, true
	}

	return "", false
}
```

### Tests

The table drives one shared `Cache` through four ordered lookups, counting
origin calls per key so a hit that should be served from L1 or L2 is caught
if it accidentally falls through to origin, and checking L1 membership
after each call to prove promotion happens exactly where documented.

Create `cache_test.go`:

```go
package tieredcache

import "testing"

func TestCacheGet(t *testing.T) {
	t.Parallel()

	originCalls := make(map[string]int)
	origin := func(key string) (string, bool) {
		originCalls[key]++
		if key == "known" {
			return "from-origin", true
		}
		return "", false
	}

	c := New(origin)
	c.l2["warm"] = "from-l2"

	tests := []struct {
		name        string
		key         string
		wantVal     string
		wantOK      bool
		wantOrigin  int // cumulative origin calls for this key after the Get
		wantL1After bool
	}{
		{
			name:        "l2 hit is served without calling origin",
			key:         "warm",
			wantVal:     "from-l2",
			wantOK:      true,
			wantOrigin:  0,
			wantL1After: true, // promoted to L1
		},
		{
			name:        "repeat lookup now hits l1, still no origin call",
			key:         "warm",
			wantVal:     "from-l2",
			wantOK:      true,
			wantOrigin:  0,
			wantL1After: true,
		},
		{
			name:        "cold key falls through to origin and is cached in l2",
			key:         "known",
			wantVal:     "from-origin",
			wantOK:      true,
			wantOrigin:  1,
			wantL1After: false, // origin hits populate L2, not L1
		},
		{
			name:        "unknown key misses every tier",
			key:         "missing",
			wantVal:     "",
			wantOK:      false,
			wantOrigin:  1,
			wantL1After: false,
		},
	}

	for _, tc := range tests {
		got, ok := c.Get(tc.key)
		if got != tc.wantVal || ok != tc.wantOK {
			t.Errorf("%s: Get(%q) = %q, %v, want %q, %v", tc.name, tc.key, got, ok, tc.wantVal, tc.wantOK)
		}
		if originCalls[tc.key] != tc.wantOrigin {
			t.Errorf("%s: originCalls[%q] = %d, want %d", tc.name, tc.key, originCalls[tc.key], tc.wantOrigin)
		}
		if _, inL1 := c.l1[tc.key]; inL1 != tc.wantL1After {
			t.Errorf("%s: key %q in L1 = %v, want %v", tc.name, tc.key, inL1, tc.wantL1After)
		}
	}
}
```

Verify: `go test -count=1 ./...`

## Review

The origin-call counter is what actually proves the fallback chain is
short-circuiting correctly — a test that only checked the returned value
would still pass even if `Get` called `origin` on every lookup regardless
of the cache tiers. Carry this forward: whenever a chain of `if`s exists to
avoid an expensive fallback, the test needs to prove the fallback was
*not* called on the cheap paths, not just that the right value came back.

## Resources

- [Go Specification: If statements](https://go.dev/ref/spec#If_statements) — the init-statement form and its scoping.
- [AWS: Caching overview](https://aws.amazon.com/caching/) — the production shape of tiered read-through caching.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-feature-flag-rollout-gate.md](11-feature-flag-rollout-gate.md) | Next: [13-monthly-quota-gate.md](13-monthly-quota-gate.md)
