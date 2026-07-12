# Exercise 22: Distributed Cache Invalidator with Partial Failure Isolation

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Invalidating a key across every cache backend a service uses — Redis for the
shared tier, memcached for a legacy layer, an in-process map for hot local
reads — has to broadcast to all of them, concurrently, and one backend's
broken client must not stop the others from being cleared. A Redis
connection panicking on a malformed protocol reply must not leave stale data
sitting in memcached and the local cache just because they never got their
turn. This module builds `InvalidateAll`, which fans the invalidation out to
every backend in its own goroutine, isolates each one's panic, and reports
per-backend outcomes so the caller knows exactly which backends still hold
stale data. It is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
cacheinvalidate/              independent module: example.com/cacheinvalidate
  go.mod                      go 1.24
  cacheinvalidate.go           Backend, BackendResult, InvalidateAll, invalidateOne
  cmd/
    demo/
      main.go                 runnable demo: redis, memcached (panics), local
  cacheinvalidate_test.go       table of failure modes + concurrent race-free fan-out
```

Files: `cacheinvalidate.go`, `cmd/demo/main.go`, `cacheinvalidate_test.go`.
Implement: `InvalidateAll(key string, backends []Backend) []BackendResult` that spawns one goroutine per backend, isolating each in `invalidateOne` so a panic in one backend never stops another's invalidation from completing.
Test: a table covering one backend panicking while the others still succeed, one backend returning an ordinary error, and all backends succeeding; an empty backend list; a 20-backend concurrent run (a third of them panicking) asserting a race-free mix of both outcomes under `-race`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/08-panic-and-recover/22-cache-invalidation-multi-backend/cmd/demo
cd go-solutions/03-control-flow/08-panic-and-recover/22-cache-invalidation-multi-backend
go mod edit -go=1.24
```

### Why each goroutine owns its own slice index, and what the recover has to preserve

`InvalidateAll` gives every spawned goroutine its own index `i` into a
pre-sized `results` slice, so each goroutine only ever writes to
`results[i]` — a memory location no other goroutine touches. That is what
makes the fan-out race-free without any extra locking: `sync.WaitGroup`
provides the happens-before edge between every goroutine's write and the
main goroutine's read of the completed slice after `wg.Wait()`, and because
the writes themselves never overlap in memory, there is nothing for `-race`
to flag. This is the same "index-per-goroutine, no shared mutable state
beyond that" shape used for the connection pool and the worker pool
elsewhere in this chapter, applied here to a broadcast rather than a queue.

The recover in `invalidateOne` has exactly one job beyond isolating the
panic: preserve which backend failed and why, without letting one backend's
crash look like every backend failed. A recovered panic value is wrapped
with the backend's name and the key it was invalidating (`%w` so
`errors.Is` still reaches whatever sentinel the backend's own client
panicked with), and the `BackendResult.Backend` field means a caller can
build an accurate picture — "memcached is stale, redis and local are
clean" — directly from the returned slice, which is the information an
operator actually needs to decide whether to retry just the failed backend
or escalate.

Create `cacheinvalidate.go`:

```go
package cacheinvalidate

import (
	"fmt"
	"sync"
)

// Backend is one place a cached key might live — Redis, memcached, a
// process-local map — each with its own Invalidate implementation that may
// panic (a nil client, a malformed protocol reply).
type Backend struct {
	Name       string
	Invalidate func(key string) error
}

// BackendResult is the per-backend outcome of one invalidation broadcast.
type BackendResult struct {
	Backend string
	OK      bool
	Err     error
}

// InvalidateAll broadcasts key to every backend concurrently, one goroutine
// per backend. A panic inside one backend's Invalidate is isolated to that
// backend's own goroutine: recover fires in invalidateOne, records the
// backend name and failure, and every other backend's invalidation still
// runs to completion — a broken Redis client must not stop memcached and
// the local cache from being cleared. Each goroutine writes to its own
// slice index, so no synchronization is needed around the results
// themselves beyond the WaitGroup that waits for every goroutine to finish.
func InvalidateAll(key string, backends []Backend) []BackendResult {
	results := make([]BackendResult, len(backends))
	var wg sync.WaitGroup
	for i := range backends {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = invalidateOne(key, backends[i])
		}(i)
	}
	wg.Wait()
	return results
}

// invalidateOne is the recover boundary: exactly one backend's worth of
// untrusted invalidation logic.
func invalidateOne(key string, b Backend) (result BackendResult) {
	result = BackendResult{Backend: b.Name}
	defer func() {
		if r := recover(); r != nil {
			result.OK = false
			if e, ok := r.(error); ok {
				result.Err = fmt.Errorf("backend %q panicked invalidating %q: %w", b.Name, key, e)
				return
			}
			result.Err = fmt.Errorf("backend %q panicked invalidating %q: %v", b.Name, key, r)
		}
	}()

	if err := b.Invalidate(key); err != nil {
		result.Err = fmt.Errorf("backend %q failed invalidating %q: %w", b.Name, key, err)
		return result
	}
	result.OK = true
	return result
}
```

### The runnable demo

`memcached` panics with a protocol error; `redis` and `local` still get
invalidated.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"sort"

	"example.com/cacheinvalidate"
)

func main() {
	backends := []cacheinvalidate.Backend{
		{Name: "redis", Invalidate: func(string) error { return nil }},
		{Name: "memcached", Invalidate: func(string) error {
			panic(errors.New("protocol error: unexpected reply type"))
		}},
		{Name: "local", Invalidate: func(string) error { return nil }},
	}

	results := cacheinvalidate.InvalidateAll("user:42", backends)

	sort.Slice(results, func(i, j int) bool { return results[i].Backend < results[j].Backend })
	for _, r := range results {
		status := "ok"
		if !r.OK {
			status = "failed"
		}
		fmt.Printf("%s: %s\n", r.Backend, status)
	}
}
```

Run it (results are sorted by backend name before printing, since goroutine
completion order is not deterministic):

```bash
go run ./cmd/demo
```

Expected output:

```
local: ok
memcached: failed
redis: ok
```

### Tests

`TestInvalidateAllTable` drives three scenarios through one table: one
backend panicking while the others still succeed, one backend returning an
ordinary error, and a fully clean broadcast. `TestInvalidateAllEmptyBackends`
covers the empty-list edge case. `TestInvalidateAllConcurrentAndRaceFree`
fans out to 20 backends (a third panicking) and asserts a genuine mix of
both outcomes with no data race.

Create `cacheinvalidate_test.go`:

```go
package cacheinvalidate

import (
	"errors"
	"testing"
)

func TestInvalidateAllTable(t *testing.T) {
	sentinel := errors.New("connection reset")

	cases := []struct {
		name       string
		backends   []Backend
		wantOK     map[string]bool
		wantErrFor string
	}{
		{
			name: "one backend panics, others still invalidated",
			backends: []Backend{
				{Name: "redis", Invalidate: func(string) error { return nil }},
				{Name: "memcached", Invalidate: func(string) error { panic(sentinel) }},
				{Name: "local", Invalidate: func(string) error { return nil }},
			},
			wantOK:     map[string]bool{"redis": true, "memcached": false, "local": true},
			wantErrFor: "memcached",
		},
		{
			name: "one backend returns an ordinary error",
			backends: []Backend{
				{Name: "redis", Invalidate: func(string) error { return errors.New("timeout") }},
				{Name: "local", Invalidate: func(string) error { return nil }},
			},
			wantOK: map[string]bool{"redis": false, "local": true},
		},
		{
			name: "all backends succeed",
			backends: []Backend{
				{Name: "redis", Invalidate: func(string) error { return nil }},
				{Name: "memcached", Invalidate: func(string) error { return nil }},
			},
			wantOK: map[string]bool{"redis": true, "memcached": true},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			results := InvalidateAll("user:42", tc.backends)
			if len(results) != len(tc.backends) {
				t.Fatalf("len(results) = %d, want %d", len(results), len(tc.backends))
			}
			for _, r := range results {
				if r.OK != tc.wantOK[r.Backend] {
					t.Errorf("backend %q OK = %v, want %v", r.Backend, r.OK, tc.wantOK[r.Backend])
				}
				if r.Backend == tc.wantErrFor && !errors.Is(r.Err, sentinel) {
					t.Errorf("backend %q error %v does not wrap the sentinel", r.Backend, r.Err)
				}
			}
		})
	}
}

func TestInvalidateAllEmptyBackends(t *testing.T) {
	results := InvalidateAll("k", nil)
	if len(results) != 0 {
		t.Fatalf("len(results) = %d, want 0", len(results))
	}
}

func TestInvalidateAllConcurrentAndRaceFree(t *testing.T) {
	backends := make([]Backend, 20)
	for i := range backends {
		i := i
		backends[i] = Backend{
			Name: "backend-" + string(rune('a'+i)),
			Invalidate: func(string) error {
				if i%3 == 0 {
					panic(errors.New("simulated fault"))
				}
				return nil
			},
		}
	}

	results := InvalidateAll("hot-key", backends)
	if len(results) != 20 {
		t.Fatalf("len(results) = %d, want 20", len(results))
	}
	failed := 0
	for _, r := range results {
		if !r.OK {
			failed++
		}
	}
	if failed == 0 || failed == 20 {
		t.Fatalf("failed = %d, want a mix of both outcomes", failed)
	}
}
```

## Review

`InvalidateAll` is correct when every backend gets its invalidation attempt
regardless of how many other backends panicked, and when the returned
`[]BackendResult` accurately identifies which backends still hold stale
data. The race-free fan-out depends on two things working together: each
goroutine owning a distinct slice index (so there is nothing to synchronize
around the results themselves) and the recover living in `invalidateOne`,
one backend wide, so a panic in goroutine 2 has no way to reach goroutine
3's stack. Run this with `-race` specifically because it is a genuine
concurrent fan-out, not a sequential loop dressed up as one — the pattern
only proves race-free by actually being exercised with the race detector,
not by inspection.

## Resources

- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup) — the happens-before edge between each goroutine's write and the caller's read after Wait.
- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — the per-goroutine recover boundary this fan-out relies on.
- [Race Detector](https://go.dev/doc/articles/race_detector) — verifying a concurrent fan-out has no shared-memory bugs.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [21-circuit-breaker-state-machine.md](21-circuit-breaker-state-machine.md) | Next: [23-worker-pool-supervisor.md](23-worker-pool-supervisor.md)
