# Exercise 17: Tenant Key Sharding Builder with Variadic IDs

**Nivel: Intermedio** — validacion rapida (un test corto).

A multi-tenant cache in front of a database needs two things from every
lookup: a deterministic key string for logging and debugging, and a shard
index so the lookup lands on the right backend node. Both are derived from
the same ordered pieces — a region, then any number of tenant-scoping IDs —
so one variadic method builds both in a single pass over a pre-sized buffer.

## What you'll build

```text
tenantshard/                independent module: example.com/tenantshard
  go.mod                    go 1.24
  tenantshard.go            package tenantshard; type Sharder; New(shardCount int), Key(region string, parts ...string) (shard int, key string)
  cmd/
    demo/
      main.go               runnable demo: shard a few tenant lookups
  tenantshard_test.go        table tests: determinism, key format, shard bounds, zero shard count, alloc budget
```

- Files: `tenantshard.go`, `cmd/demo/main.go`, `tenantshard_test.go`.
- Implement: `New(shardCount int) *Sharder` and `(*Sharder).Key(region string, parts ...string) (shard int, key string)`, building the key with a pre-sized `strings.Builder` and hashing it with `hash/fnv` to pick a shard.
- Test: the same region and parts always yield the same shard and key; `Key(region)` with zero parts returns the bare region; different regions with the same tenant parts produce different keys; the shard index always falls in `[0, shardCount)`; `New(0)` and `New(-3)` both behave as one shard.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/03-variadic-functions/17-tenant-key-sharding-variadic/cmd/demo
cd go-solutions/04-functions/03-variadic-functions/17-tenant-key-sharding-variadic
go mod edit -go=1.24
```

### One variadic pass, pre-sized, no wasted allocation

`Key(region string, parts ...string)` takes the fixed piece (`region`) as a
named parameter and the variable-length tenant path as `parts ...string`,
which is the idiomatic split: a required piece stays required and typed on
its own, while the open-ended tail becomes the variadic slice. The method
computes the exact final string length up front — `len(region)` plus one
separator byte and `len(p)` for every part — and calls `strings.Builder.Grow`
with that total before writing a single byte. That single `Grow` call is what
keeps this "without allocating": the builder's internal buffer is sized once
and every `WriteString`/`WriteByte` after that just appends into already-
reserved capacity, instead of the buffer doubling and copying itself two or
three times as the key grows.

The shard index is derived from the same key string via FNV-1a and a modulo
by `shardCount`, so two calls with identical `region` and `parts` always
agree on both the key and the shard — that determinism is the whole point of
a sharding scheme; a cache miss must always probe the same backend node a
cache write went to. `New` guards against a misconfigured `shardCount <= 0`
by falling back to a single shard rather than dividing by zero or returning a
negative index.

Create `tenantshard.go`:

```go
// tenantshard.go
package tenantshard

import (
	"hash/fnv"
	"strings"
)

// Sharder assigns deterministic multi-tenant cache keys to one of a fixed
// number of shards.
type Sharder struct {
	shardCount int
}

// New returns a Sharder that distributes keys across shardCount shards.
// shardCount <= 0 is treated as 1 (a single shard).
func New(shardCount int) *Sharder {
	if shardCount <= 0 {
		shardCount = 1
	}
	return &Sharder{shardCount: shardCount}
}

// Key builds the deterministic cache key for region plus any number of
// tenant-scoped parts, and reports which shard owns that key. The builder is
// pre-sized from region and parts so building the key never reallocates its
// backing buffer.
func (s *Sharder) Key(region string, parts ...string) (shard int, key string) {
	size := len(region)
	for _, p := range parts {
		size += 1 + len(p)
	}

	var b strings.Builder
	b.Grow(size)
	b.WriteString(region)
	for _, p := range parts {
		b.WriteByte(':')
		b.WriteString(p)
	}
	key = b.String()

	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	shard = int(h.Sum32() % uint32(s.shardCount))
	return shard, key
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/tenantshard"
)

func main() {
	s := tenantshard.New(8)

	shard, key := s.Key("eu-west", "acme-corp", "42")
	fmt.Printf("key=%q shard=%d\n", key, shard)

	shard, key = s.Key("us-east", "acme-corp", "42")
	fmt.Printf("key=%q shard=%d\n", key, shard)

	shard, key = s.Key("eu-west")
	fmt.Printf("key=%q shard=%d\n", key, shard)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
key="eu-west:acme-corp:42" shard=2
key="us-east:acme-corp:42" shard=6
key="eu-west" shard=5
```

### Tests

`TestKeyAllocationsAreBounded` is the one that pins down the "without
allocating" claim in the exercise title: it uses `testing.AllocsPerRun` to
assert `Key` stays within a small, fixed allocation budget instead of
allocating once per part.

Create `tenantshard_test.go`:

```go
// tenantshard_test.go
package tenantshard

import "testing"

func TestKeyIsDeterministic(t *testing.T) {
	t.Parallel()

	s := New(8)
	wantShard, wantKey := s.Key("eu-west", "acme-corp", "42")
	for i := 0; i < 5; i++ {
		gotShard, gotKey := s.Key("eu-west", "acme-corp", "42")
		if gotShard != wantShard || gotKey != wantKey {
			t.Fatalf("call %d: Key = (%d, %q), want (%d, %q)", i, gotShard, gotKey, wantShard, wantKey)
		}
	}
}

func TestKeyFormatsRegionAndParts(t *testing.T) {
	t.Parallel()

	s := New(8)

	tests := []struct {
		name    string
		region  string
		parts   []string
		wantKey string
	}{
		{"region only", "eu-west", nil, "eu-west"},
		{"region and tenant", "eu-west", []string{"acme-corp"}, "eu-west:acme-corp"},
		{"region tenant and id", "eu-west", []string{"acme-corp", "42"}, "eu-west:acme-corp:42"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, gotKey := s.Key(tc.region, tc.parts...)
			if gotKey != tc.wantKey {
				t.Fatalf("Key(%q, %v) key = %q, want %q", tc.region, tc.parts, gotKey, tc.wantKey)
			}
		})
	}
}

func TestDifferentRegionsCanShareTenantButDifferKey(t *testing.T) {
	t.Parallel()

	s := New(8)
	_, keyEU := s.Key("eu-west", "acme-corp", "42")
	_, keyUS := s.Key("us-east", "acme-corp", "42")

	if keyEU == keyUS {
		t.Fatalf("expected distinct keys per region, both were %q", keyEU)
	}
}

func TestShardIsWithinBounds(t *testing.T) {
	t.Parallel()

	s := New(8)
	for i := 0; i < 100; i++ {
		shard, _ := s.Key("eu-west", "tenant", string(rune('A'+i%26)))
		if shard < 0 || shard >= 8 {
			t.Fatalf("shard %d out of bounds [0,8)", shard)
		}
	}
}

func TestZeroOrNegativeShardCountTreatedAsOne(t *testing.T) {
	t.Parallel()

	for _, n := range []int{0, -3} {
		s := New(n)
		shard, _ := s.Key("eu-west", "acme-corp")
		if shard != 0 {
			t.Fatalf("New(%d).Key(...) shard = %d, want 0", n, shard)
		}
	}
}

func TestKeyAllocationsAreBounded(t *testing.T) {
	s := New(8)
	allocs := testing.AllocsPerRun(100, func() {
		_, _ = s.Key("eu-west", "acme-corp", "42")
	})
	if allocs > 3 {
		t.Fatalf("Key allocated %.1f times per call, want <= 3 (builder buffer, hash write, result string)", allocs)
	}
}
```

## Review

`Key` is correct when it is a pure function of `region` and `parts` — same
inputs always give the same shard and the same key string — and when the
shard index always lands inside `[0, shardCount)` no matter how
`shardCount` was configured. The senior point is the pre-sizing discipline:
computing the total length before touching the builder turns an operation
that could reallocate its buffer two or three times (once the string roughly
doubles past the initial small-string optimization) into one that allocates
its backing array exactly once. The mistake to avoid is building the key with
naive `+=` string concatenation across the loop over `parts` — each `+=`
allocates a brand-new string, so a five-part key would cost five allocations
instead of one.

## Resources

- [`strings.Builder`](https://pkg.go.dev/strings#Builder) — `Grow` reserves capacity so subsequent writes never reallocate.
- [`hash/fnv`](https://pkg.go.dev/hash/fnv) — the fast, non-cryptographic hash used to pick a shard.
- [`testing.AllocsPerRun`](https://pkg.go.dev/testing#AllocsPerRun) — measuring the allocation cost of a hot-path function.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [16-hash-ring-endpoint-aggregator.md](16-hash-ring-endpoint-aggregator.md) | Next: [18-bulk-insert-args-placeholder-builder.md](18-bulk-insert-args-placeholder-builder.md)
