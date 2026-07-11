# Exercise 9: Fast-Timeout Cache Read With Source-of-Truth Fallback

A cache is an optimization, not a dependency you block on. If a stalled cache node
can hang the request for the full budget, the cache is amplifying latency instead of
reducing it. This exercise builds a config reader that queries the cache under a
deliberately short timeout and, on any cache failure — timeout or miss — falls back to
the authoritative source (and finally a safe default), so a slow cache degrades to the
source instead of stalling the request.

This module is fully self-contained. It has its own `go mod init`, defines every type
it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
config-cache/                        independent module: example.com/configcache
  go.mod                             go 1.26
  config.go                          Store, Value, ConfigService, GetConfig; short cache timeout + fallback
  cmd/
    demo/
      main.go                        runnable demo: fast cache, slow cache -> source, miss -> source
  config_test.go                     fast/slow/miss paths, parent-budget preserved, -race
```

- Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
- Implement: a `Store` interface, a `ConfigService{Cache, Source, Default, CacheTimeout}`, and `GetConfig(ctx, key) Value` that reads the cache under a short derived timeout and falls back to the source (on parent budget) then a default.
- Test: a fast cache serves the value and the source is never queried (spy); a cache that blocks past the short timeout falls back to the source value; a cache miss falls back to the source; the short cache timeout does not cancel the parent, so the source runs on the full parent budget; the slow-cache path returns near the short cache timeout.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/config-cache/cmd/demo
cd ~/go-exercises/config-cache
go mod init example.com/configcache
```

### The short timeout that must not leak

The single most important line is the cache read's own budget:
`cctx, cancel := context.WithTimeout(ctx, s.CacheTimeout)`. `CacheTimeout` is small —
a few milliseconds — because the cache is supposed to be fast, and a cache that is not
fast is not helping. If the cache stalls, `cctx` expires quickly, the cache read
returns `DeadlineExceeded`, and the code moves on to the source. The request pays a
few milliseconds for the missed optimization instead of the whole budget for a hung
dependency.

The subtlety that makes or breaks this pattern is that `cctx` is a *child* of the
request context. Cancelling `cctx` — which `defer cancel()` does — cancels only the
cache read, never the parent. So when the code falls back, it calls the source with the
*parent* `ctx`, which still has its full remaining budget. A common bug is to run the
source under the same short cache timeout (or to accidentally cancel the parent), which
would give the authoritative store only a few milliseconds and turn a cache stall into
a total failure. The source gets the real budget; only the cache gets the short leash.

The fallback treats the cache as best-effort: *any* cache error — a timeout, a miss, a
transient node failure — triggers the fallback. There is no need to distinguish them
for control flow, because the response to all of them is the same: ask the source. If
the source also fails, the reader returns a safe `Default`, because a config read
should resolve to *something* usable rather than hard-failing the request. Each
`Value` carries its `Origin` (`cache`, `source`, or `default`), so metrics can track
the cache hit rate and the degradation rate.

Create `config.go`:

```go
package configcache

import (
	"context"
	"time"
)

// Store is a read-only key/value surface. Both the cache and the source implement
// it; in production the cache is Redis/memcached and the source is the database.
type Store interface {
	Get(ctx context.Context, key string) (string, error)
}

// Value is a resolved config value tagged with where it came from.
type Value struct {
	Data   string
	Origin string // "cache", "source", or "default"
}

// ConfigService reads config with a cache-first, source-fallback strategy.
type ConfigService struct {
	Cache        Store
	Source       Store
	Default      string
	CacheTimeout time.Duration // deliberately short
}

// GetConfig reads key from the cache under a short timeout, falling back to the
// source on the parent budget and finally to a safe default. The short cache
// timeout never cancels the parent, so the source runs on the full request budget.
func (s *ConfigService) GetConfig(ctx context.Context, key string) Value {
	cctx, cancel := context.WithTimeout(ctx, s.CacheTimeout)
	defer cancel()

	if v, err := s.Cache.Get(cctx, key); err == nil {
		return Value{Data: v, Origin: "cache"}
	}

	// Cache timed out or missed: fall back to the source on the parent budget.
	if v, err := s.Source.Get(ctx, key); err == nil {
		return Value{Data: v, Origin: "source"}
	}

	return Value{Data: s.Default, Origin: "default"}
}
```

### The runnable demo

The demo wires a fast cache, then a slow cache (blocks past the 10ms cache timeout),
then a missing key — showing each resolves to the right origin.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/configcache"
)

type funcStore func(ctx context.Context, key string) (string, error)

func (f funcStore) Get(ctx context.Context, key string) (string, error) { return f(ctx, key) }

var errMiss = errors.New("cache miss")

func main() {
	source := funcStore(func(ctx context.Context, key string) (string, error) { return "from-db", nil })

	fastCache := funcStore(func(ctx context.Context, key string) (string, error) { return "from-cache", nil })
	fast := &configcache.ConfigService{Cache: fastCache, Source: source, Default: "def", CacheTimeout: 10 * time.Millisecond}
	fmt.Printf("fast:  %+v\n", fast.GetConfig(context.Background(), "k"))

	slowCache := funcStore(func(ctx context.Context, key string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})
	slow := &configcache.ConfigService{Cache: slowCache, Source: source, Default: "def", CacheTimeout: 10 * time.Millisecond}
	start := time.Now()
	v := slow.GetConfig(context.Background(), "k")
	fmt.Printf("slow:  %+v fast=%v\n", v, time.Since(start) < 200*time.Millisecond)

	missCache := funcStore(func(ctx context.Context, key string) (string, error) { return "", errMiss })
	miss := &configcache.ConfigService{Cache: missCache, Source: source, Default: "def", CacheTimeout: 10 * time.Millisecond}
	fmt.Printf("miss:  %+v\n", miss.GetConfig(context.Background(), "k"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
fast:  {Data:from-cache Origin:cache}
slow:  {Data:from-db Origin:source} fast=true
miss:  {Data:from-db Origin:source}
```

### Tests

The source is a spy that records its call count, so a test can assert the fast path
never touches it. `TestFastCacheServesValue` asserts the value comes from the cache and
the source stayed at zero calls. `TestSlowCacheFallsBackToSource` asserts a blocking
cache degrades to the source value and returns near the short cache timeout, not the
cache's 500ms stall. `TestCacheMissFallsBackToSource` asserts a miss degrades to the
source. `TestParentBudgetPreserved` gives the parent a generous budget and a tiny cache
timeout, and asserts the source is invoked with a context whose deadline is the
parent's — proving the short cache timeout did not leak.

Create `config_test.go`:

```go
package configcache

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type funcStore func(ctx context.Context, key string) (string, error)

func (f funcStore) Get(ctx context.Context, key string) (string, error) { return f(ctx, key) }

var errMiss = errors.New("cache miss")

func TestFastCacheServesValue(t *testing.T) {
	t.Parallel()
	var sourceCalls atomic.Int64
	svc := &ConfigService{
		Cache:        funcStore(func(ctx context.Context, key string) (string, error) { return "cached", nil }),
		Source:       funcStore(func(ctx context.Context, key string) (string, error) { sourceCalls.Add(1); return "db", nil }),
		Default:      "def",
		CacheTimeout: 10 * time.Millisecond,
	}
	v := svc.GetConfig(context.Background(), "k")
	if v.Data != "cached" || v.Origin != "cache" {
		t.Fatalf("value = %+v, want {cached cache}", v)
	}
	if sourceCalls.Load() != 0 {
		t.Fatalf("source calls = %d, want 0 (cache hit must not query source)", sourceCalls.Load())
	}
}

func TestSlowCacheFallsBackToSource(t *testing.T) {
	t.Parallel()
	svc := &ConfigService{
		Cache: funcStore(func(ctx context.Context, key string) (string, error) {
			select {
			case <-time.After(500 * time.Millisecond):
				return "late", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}),
		Source:       funcStore(func(ctx context.Context, key string) (string, error) { return "db", nil }),
		Default:      "def",
		CacheTimeout: 10 * time.Millisecond,
	}
	start := time.Now()
	v := svc.GetConfig(context.Background(), "k")
	elapsed := time.Since(start)

	if v.Data != "db" || v.Origin != "source" {
		t.Fatalf("value = %+v, want {db source}", v)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("call took %v, want near the 10ms cache timeout, not the 500ms stall", elapsed)
	}
}

func TestCacheMissFallsBackToSource(t *testing.T) {
	t.Parallel()
	svc := &ConfigService{
		Cache:        funcStore(func(ctx context.Context, key string) (string, error) { return "", errMiss }),
		Source:       funcStore(func(ctx context.Context, key string) (string, error) { return "db", nil }),
		Default:      "def",
		CacheTimeout: 10 * time.Millisecond,
	}
	v := svc.GetConfig(context.Background(), "k")
	if v.Data != "db" || v.Origin != "source" {
		t.Fatalf("value = %+v, want {db source}", v)
	}
}

func TestParentBudgetPreserved(t *testing.T) {
	t.Parallel()
	parent, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	parentDeadline, _ := parent.Deadline()

	var gotDeadline time.Time
	var gotErr error
	svc := &ConfigService{
		Cache: funcStore(func(ctx context.Context, key string) (string, error) { return "", errMiss }),
		Source: funcStore(func(ctx context.Context, key string) (string, error) {
			gotDeadline, _ = ctx.Deadline()
			gotErr = ctx.Err()
			return "db", nil
		}),
		Default:      "def",
		CacheTimeout: 5 * time.Millisecond,
	}
	svc.GetConfig(parent, "k")

	if gotErr != nil {
		t.Fatalf("source ctx.Err() = %v, want nil (short cache timeout must not cancel parent)", gotErr)
	}
	if !gotDeadline.Equal(parentDeadline) {
		t.Fatalf("source deadline = %v, want parent's %v (not the short cache timeout)", gotDeadline, parentDeadline)
	}
}
```

## Review

The reader is correct when a stalled cache costs a few milliseconds, not the whole
budget, and the source always gets the real budget. The short `CacheTimeout` on a child
context is what caps the cache read; `TestSlowCacheFallsBackToSource` proves a 500ms
cache stall returns in near 10ms with the source value. The parent-budget guarantee is
the other half: `TestParentBudgetPreserved` asserts the source sees a context whose
deadline is the parent's and whose `Err()` is nil, proving `defer cancel()` on the
child never touched the parent. The fast path must not touch the source at all, which
the spy in `TestFastCacheServesValue` enforces.

The mistakes to avoid: reading the cache under the full request budget (a stall then
consumes the whole budget); running the source under the short cache timeout (the
authoritative store gets no time and the fallback fails too); and treating a cache miss
differently from a cache timeout when both should simply fall back. Tagging each value
with its `Origin` keeps the cache hit rate and degradation rate observable. Run
`go test -race`; the spy's atomic counter and the captured-deadline reads are exercised
under the detector.

## Resources

- [context.WithTimeout](https://pkg.go.dev/context#WithTimeout) — the short child timeout for the cache read that does not cancel the parent.
- [context.Context](https://pkg.go.dev/context#Context) — Deadline and Err, used to prove the parent budget survives.
- [context.DeadlineExceeded](https://pkg.go.dev/context#pkg-variables) — the error a stalled cache read returns, triggering fallback.
- [AWS Builders' Library: Caching challenges and strategies](https://aws.amazon.com/builders-library/caching-challenges-and-strategies/) — why a cache must be treated as best-effort, not a hard dependency.

---

Back to [08-fanout-subbudget-allocation.md](08-fanout-subbudget-allocation.md) | Next: [../06-context-withvalue/00-concepts.md](../06-context-withvalue/00-concepts.md)
