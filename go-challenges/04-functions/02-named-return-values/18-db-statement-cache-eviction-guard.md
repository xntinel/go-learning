# Exercise 18: Prepared Statement Cache Eviction on Connection Error

A prepared-statement cache that survives a broken connection is a bug
waiting to bite the next query: the statements it holds belong to a
connection that no longer exists, and reusing them against a replacement
connection is invalid. This exercise builds a cache whose `Execute` clears
itself the moment a connection error is observed, via a deferred closure
keyed on the named `err` result — so eviction cannot be forgotten on any
current or future error path.

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde de error de conexion).

## What you'll build

```text
stmtcache/                    independent module: example.com/stmtcache
  go.mod
  stmtcache.go                 Conn (fake); Cache; Execute (named err, deferred eviction)
  cmd/demo/
    main.go                    runnable demo: cached hit, connection error, recovery
  stmtcache_test.go             cache hit, new query, eviction on error, re-prepare after reconnect
```

- Files: `stmtcache.go`, `cmd/demo/main.go`, `stmtcache_test.go`.
- Implement: `(*Cache) Execute(query string, conn *Conn) (result string, err error)` that prepares a query only if it is not already cached, and a deferred closure that evicts the whole cache whenever `err` ends up non-nil.
- Test: a table covering a cached hit (no re-prepare), a new query (fresh prepare), a connection error (cache evicted), and a successful re-prepare after reconnecting.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### One deferred check protects every current and future error path

`Execute` has two places a connection error can surface — preparing the
statement, or running it — and both must evict the cache. Rather than
duplicating `c.ready = false; c.stmts = ...` at each of those spots, a single
deferred closure keyed on the named `err` handles both, and any error path
added later for free:

```go
defer func() {
    if err != nil {
        c.ready = false
        c.stmts = make(map[string]bool) // evict everything: stale for this connection
    }
}()

if !c.stmts[query] {
    if perr := conn.Prepare(query); perr != nil {
        err = perr
        return
    }
    ...
}
result, err = conn.Run(query)
return
```

Because the eviction logic reads the named `err` after the function body has
run, it does not matter which of the two branches set it — the cache is
cleared either way, and the next call re-prepares from scratch against
whatever connection comes next.

Create `stmtcache.go`:

```go
package stmtcache

import (
	"errors"
	"sync"
)

// Conn is a fake database connection whose Prepare/Run can be told to fail,
// simulating a dropped connection.
type Conn struct {
	Broken bool
}

// Prepare "prepares" a statement, failing if the connection is broken.
func (c *Conn) Prepare(query string) error {
	if c.Broken {
		return errors.New("connection error: prepare failed")
	}
	return nil
}

// Run "executes" a prepared statement, failing if the connection is broken.
func (c *Conn) Run(query string) (string, error) {
	if c.Broken {
		return "", errors.New("connection error: run failed")
	}
	return "result:" + query, nil
}

// Cache is a prepared-statement cache keyed by query text. Ready tracks
// whether the cached statements are still valid for the current connection.
type Cache struct {
	mu     sync.Mutex
	ready  bool
	stmts  map[string]bool
	prepCt int // counts calls to Conn.Prepare, for test assertions
}

// NewCache returns an empty, not-ready statement cache.
func NewCache() *Cache {
	return &Cache{stmts: make(map[string]bool)}
}

// Ready reports whether the cache currently holds valid prepared statements.
func (c *Cache) Ready() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ready
}

// PrepareCount reports how many times a statement was actually prepared
// against the connection (as opposed to served from cache).
func (c *Cache) PrepareCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.prepCt
}

// Execute runs query against conn, preparing it first if it is not already
// cached. If the connection errors at any point, a deferred closure keyed on
// the named err clears the cache (ready=false, stmts=nil) so a later call
// never trusts a statement that belongs to a dead connection — it will be
// re-prepared against whatever connection comes next.
func (c *Cache) Execute(query string, conn *Conn) (result string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	defer func() {
		if err != nil {
			c.ready = false
			c.stmts = make(map[string]bool) // evict everything: stale for this connection
		}
	}()

	if !c.stmts[query] {
		if perr := conn.Prepare(query); perr != nil {
			err = perr
			return
		}
		c.prepCt++
		c.stmts[query] = true
		c.ready = true
	}

	result, err = conn.Run(query)
	return
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/stmtcache"
)

func main() {
	cache := stmtcache.NewCache()
	conn := &stmtcache.Conn{}

	res, err := cache.Execute("SELECT 1", conn)
	fmt.Printf("first run: result=%q err=%v ready=%v prepares=%d\n",
		res, err, cache.Ready(), cache.PrepareCount())

	res, err = cache.Execute("SELECT 1", conn)
	fmt.Printf("second run (cached): result=%q err=%v prepares=%d\n",
		res, err, cache.PrepareCount())

	conn.Broken = true
	_, err = cache.Execute("SELECT 1", conn)
	fmt.Printf("run on broken conn: err=%v ready=%v\n", err, cache.Ready())

	conn.Broken = false
	res, err = cache.Execute("SELECT 1", conn)
	fmt.Printf("run after reconnect: result=%q err=%v prepares=%d\n",
		res, err, cache.PrepareCount())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
first run: result="result:SELECT 1" err=<nil> ready=true prepares=1
second run (cached): result="result:SELECT 1" err=<nil> prepares=1
run on broken conn: err=connection error: run failed ready=false
run after reconnect: result="result:SELECT 1" err=<nil> prepares=2
```

### Tests

Create `stmtcache_test.go`:

```go
package stmtcache

import "testing"

func TestExecute(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		steps func(t *testing.T, cache *Cache, conn *Conn)
	}{
		{
			name: "cached query is not re-prepared",
			steps: func(t *testing.T, cache *Cache, conn *Conn) {
				if _, err := cache.Execute("SELECT 1", conn); err != nil {
					t.Fatalf("first Execute: %v", err)
				}
				if _, err := cache.Execute("SELECT 1", conn); err != nil {
					t.Fatalf("second Execute: %v", err)
				}
				if got := cache.PrepareCount(); got != 1 {
					t.Fatalf("PrepareCount = %d, want 1 (second call should hit cache)", got)
				}
				if !cache.Ready() {
					t.Fatal("Ready() = false, want true after successful prepare")
				}
			},
		},
		{
			name: "new query triggers a fresh prepare",
			steps: func(t *testing.T, cache *Cache, conn *Conn) {
				if _, err := cache.Execute("SELECT 1", conn); err != nil {
					t.Fatalf("Execute SELECT 1: %v", err)
				}
				if _, err := cache.Execute("SELECT 2", conn); err != nil {
					t.Fatalf("Execute SELECT 2: %v", err)
				}
				if got := cache.PrepareCount(); got != 2 {
					t.Fatalf("PrepareCount = %d, want 2 (two distinct queries)", got)
				}
			},
		},
		{
			name: "connection error evicts the cache",
			steps: func(t *testing.T, cache *Cache, conn *Conn) {
				if _, err := cache.Execute("SELECT 1", conn); err != nil {
					t.Fatalf("warmup Execute: %v", err)
				}
				conn.Broken = true
				if _, err := cache.Execute("SELECT 1", conn); err == nil {
					t.Fatal("Execute on broken conn: want error, got nil")
				}
				if cache.Ready() {
					t.Fatal("Ready() = true after connection error, want false (cache evicted)")
				}
			},
		},
		{
			name: "eviction forces re-prepare on the next good connection",
			steps: func(t *testing.T, cache *Cache, conn *Conn) {
				if _, err := cache.Execute("SELECT 1", conn); err != nil {
					t.Fatalf("warmup Execute: %v", err)
				}
				conn.Broken = true
				_, _ = cache.Execute("SELECT 1", conn) // evicts

				conn.Broken = false
				if _, err := cache.Execute("SELECT 1", conn); err != nil {
					t.Fatalf("Execute after reconnect: %v", err)
				}
				if got := cache.PrepareCount(); got != 2 {
					t.Fatalf("PrepareCount = %d, want 2 (re-prepared after eviction)", got)
				}
				if !cache.Ready() {
					t.Fatal("Ready() = false after successful re-prepare, want true")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.steps(t, NewCache(), &Conn{})
		})
	}
}
```

## Review

`Execute` is correct when a cached query is never re-prepared, a new query is
always prepared once, and any connection error — whether during prepare or
run — leaves the cache in a state that forces a full re-prepare on the next
call. The named `err` result is what lets the eviction logic live in exactly
one deferred closure instead of being copy-pasted at both error sites inside
`Execute`. The mistake to avoid is evicting only on a prepare failure and
forgetting the run failure (or vice versa) — a defer keyed on the named
result covers both by construction, and any error path added to `Execute`
later inherits the same guarantee for free.

## Resources

- [Go Spec: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [`database/sql` Stmt](https://pkg.go.dev/database/sql#Stmt)
- [Effective Go: Named result parameters](https://go.dev/doc/effective_go#named-results)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [17-distributed-lock-ttl-extension.md](17-distributed-lock-ttl-extension.md) | Next: [19-value-encoding-json-to-yaml-fallback.md](19-value-encoding-json-to-yaml-fallback.md)
