# Exercise 17: Lazy Connection Pools Per Dialect via sync.OnceValue

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A service that supports several SQL dialects should not pay the cost of
opening a connection pool for a dialect it never actually uses. This exercise
builds a `Manager` that lazily creates one pool per dialect the first time
that dialect is requested, using `sync.OnceValue` so the (simulated)
expensive `Open` call runs exactly once per dialect no matter how many
goroutines ask for it concurrently — and never at all for a dialect nobody
asks for.

## What you'll build

```text
pool/                      independent module: example.com/pool
  go.mod                    module example.com/pool
  pool.go                    Pool, Opener, Manager with lazy per-dialect sync.OnceValue caching
  cmd/
    demo/
      main.go                proves reuse and laziness with a counting Opener
  pool_test.go                lazy creation, error caching, concurrent-access table
```

Files: `pool.go`, `cmd/demo/main.go`, `pool_test.go`.
Implement: `Manager` with `Get(dialect string) (*Pool, error)` that lazily builds and caches one `sync.OnceValue`-backed result per dialect; `Dialects() int` reporting how many dialects have been touched.
Test: a dialect's `Opener` runs exactly once even under many concurrent `Get` calls; the same `*Pool` pointer is returned on every call for a dialect; an `Opener` error is cached too, not retried.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why sync.OnceValue instead of a mutex-guarded if-nil check

The naive lazy-init pattern — `if m.pools[dialect] == nil { m.pools[dialect]
= open(dialect) }` under a lock — has a subtle race even with the lock held
correctly: if the lock is released between the check and the open (say,
because opening genuinely takes time and you don't want to hold a global
lock across it), two goroutines can both see `nil` and both call `open`.
`sync.OnceValue(f func() T) func() T` sidesteps the whole problem: it wraps
`f` so that no matter how many goroutines call the returned function
concurrently, `f` runs exactly once, and every caller — the one that ran it
and every one that arrived later — gets the same cached result, blocking
until it is ready if necessary.

This exercise uses one `sync.OnceValue` *per dialect*, stored in a map
guarded by a plain `sync.Mutex`. The mutex's job is much smaller than in the
naive version: it only ever protects "does a `OnceValue` function already
exist for this dialect", never the (possibly slow) work of actually opening
the pool. Once the per-dialect `OnceValue` function exists, `Get` releases
the map lock and calls it — so two different dialects can be opened fully in
parallel, and only genuinely concurrent first-requests for the *same*
dialect ever wait on each other, and even then only until that one `Open`
call finishes.

Because `Opener` returns `(*Pool, error)` and `sync.OnceValue` caches a
single value, `result` bundles both together — the error path is cached
exactly as durably as the success path, which matters: a failed connection
attempt should not be silently retried on every subsequent call as if
nothing happened.

Create `pool.go`:

```go
// pool.go
// Package pool lazily builds one connection pool per SQL dialect, using
// sync.OnceValue so an expensive Open only ever runs once per dialect — and
// never at all for a dialect nobody asks for.
package pool

import "sync"

// Pool is a fake opened connection pool for one dialect.
type Pool struct {
	Dialect string
	Opened  int // how many opens had happened when this pool was created
}

// Opener creates the underlying pool for a dialect. It is injected so tests
// can count calls and simulate failures instead of dialing a real database.
type Opener func(dialect string) (*Pool, error)

// result bundles what a dialect's Opener produced, so a single
// sync.OnceValue can cache both the pool and any error together.
type result struct {
	pool *Pool
	err  error
}

// Manager lazily builds and caches one Pool per dialect. Get is safe to call
// concurrently for the same or different dialects.
type Manager struct {
	mu    sync.Mutex
	open  Opener
	onces map[string]func() result
}

// NewManager returns a Manager that uses open to build a pool the first time
// each dialect is requested.
func NewManager(open Opener) *Manager {
	return &Manager{open: open, onces: make(map[string]func() result)}
}

// Get returns the pool for dialect, creating it lazily via open on the first
// call for that dialect and reusing the same *Pool on every later call, even
// under concurrent access from multiple goroutines.
func (m *Manager) Get(dialect string) (*Pool, error) {
	m.mu.Lock()
	once, ok := m.onces[dialect]
	if !ok {
		once = sync.OnceValue(func() result {
			p, err := m.open(dialect)
			return result{pool: p, err: err}
		})
		m.onces[dialect] = once
	}
	m.mu.Unlock()

	res := once()
	return res.pool, res.err
}

// Dialects reports how many distinct dialects have had Get called for them
// at least once, i.e. how many onces have been created (not necessarily
// resolved successfully).
func (m *Manager) Dialects() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.onces)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"sync"

	"example.com/pool"
)

func main() {
	var mu sync.Mutex
	opens := map[string]int{}

	opener := func(dialect string) (*pool.Pool, error) {
		mu.Lock()
		opens[dialect]++
		n := opens[dialect]
		mu.Unlock()
		return &pool.Pool{Dialect: dialect, Opened: n}, nil
	}

	mgr := pool.NewManager(opener)

	p1, _ := mgr.Get("postgres")
	p2, _ := mgr.Get("postgres")
	fmt.Println("same pool instance:", p1 == p2)
	fmt.Println("postgres opened count:", p1.Opened)

	mu.Lock()
	mysqlOpens := opens["mysql"]
	mu.Unlock()
	fmt.Println("mysql opens before first Get:", mysqlOpens)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
same pool instance: true
postgres opened count: 1
mysql opens before first Get: 0
```

The last line is the laziness proof: `mysql` was never requested, so its
`Opener` never ran — there is nothing in the `opens` map for it at all,
which is why the read defaults to `0` rather than some stale count.

### Tests

Create `pool_test.go`:

```go
// pool_test.go
package pool

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestGetCreatesPoolLazily(t *testing.T) {
	var opens int32
	mgr := NewManager(func(dialect string) (*Pool, error) {
		atomic.AddInt32(&opens, 1)
		return &Pool{Dialect: dialect}, nil
	})

	if got := mgr.Dialects(); got != 0 {
		t.Fatalf("Dialects() before any Get = %d, want 0", got)
	}
	if _, err := mgr.Get("postgres"); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&opens); got != 1 {
		t.Fatalf("opens after one Get = %d, want 1", got)
	}
	if got := mgr.Dialects(); got != 1 {
		t.Fatalf("Dialects() after one Get = %d, want 1", got)
	}
}

func TestGetReusesSamePoolAndDoesNotReopen(t *testing.T) {
	var opens int32
	mgr := NewManager(func(dialect string) (*Pool, error) {
		atomic.AddInt32(&opens, 1)
		return &Pool{Dialect: dialect}, nil
	})

	p1, err := mgr.Get("postgres")
	if err != nil {
		t.Fatal(err)
	}
	p2, err := mgr.Get("postgres")
	if err != nil {
		t.Fatal(err)
	}
	if p1 != p2 {
		t.Fatalf("Get returned different pools for the same dialect: %p != %p", p1, p2)
	}
	if got := atomic.LoadInt32(&opens); got != 1 {
		t.Fatalf("opens after two Get calls for the same dialect = %d, want 1", got)
	}
}

func TestOpenErrorIsCached(t *testing.T) {
	var opens int32
	wantErr := errors.New("connect refused")
	mgr := NewManager(func(dialect string) (*Pool, error) {
		atomic.AddInt32(&opens, 1)
		return nil, wantErr
	})

	_, err1 := mgr.Get("sqlite")
	_, err2 := mgr.Get("sqlite")
	if !errors.Is(err1, wantErr) || !errors.Is(err2, wantErr) {
		t.Fatalf("errors = %v, %v, want both to be wantErr", err1, err2)
	}
	if got := atomic.LoadInt32(&opens); got != 1 {
		t.Fatalf("opens after two failing Get calls = %d, want 1 (error must be cached too)", got)
	}
}

func TestConcurrentGetOpensExactlyOncePerDialect(t *testing.T) {
	var mu sync.Mutex
	opens := map[string]int{}
	mgr := NewManager(func(dialect string) (*Pool, error) {
		mu.Lock()
		opens[dialect]++
		mu.Unlock()
		return &Pool{Dialect: dialect}, nil
	})

	dialects := []string{"postgres", "mysql", "sqlite"}
	const callersPerDialect = 50

	var wg sync.WaitGroup
	results := make([]*Pool, len(dialects)*callersPerDialect)
	for i, d := range dialects {
		for j := 0; j < callersPerDialect; j++ {
			wg.Add(1)
			idx := i*callersPerDialect + j
			dialect := d
			go func() {
				defer wg.Done()
				p, err := mgr.Get(dialect)
				if err != nil {
					t.Errorf("Get(%s) error = %v", dialect, err)
					return
				}
				results[idx] = p
			}()
		}
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	for _, d := range dialects {
		if opens[d] != 1 {
			t.Errorf("opens[%s] = %d, want exactly 1", d, opens[d])
		}
	}

	// Every goroutine for the same dialect must have received the identical
	// *Pool pointer.
	first := make(map[string]*Pool, len(dialects))
	for i, d := range dialects {
		for j := 0; j < callersPerDialect; j++ {
			p := results[i*callersPerDialect+j]
			if p == nil {
				continue
			}
			if existing, ok := first[d]; !ok {
				first[d] = p
			} else if existing != p {
				t.Errorf("dialect %s: goroutines received different *Pool instances", d)
			}
		}
	}
}
```

## Review

`TestConcurrentGetOpensExactlyOncePerDialect` is the test that would fail
first against the naive if-nil-under-lock version described above: it drives
50 goroutines at each of three dialects simultaneously and demands that each
dialect's `Opener` ran exactly once, and that every goroutine for that
dialect got the identical `*Pool` pointer back. Run it with `-race` — the
whole point of `sync.OnceValue` is that this passes cleanly, with no data
race on the map or on the cached result, even though dozens of goroutines are
touching the same dialect at once.

`TestOpenErrorIsCached` is the detail easy to miss: `sync.OnceValue` caches
whatever `f` returned the *first* time, success or failure, and never calls
`f` again. If a pool's `Open` should instead be retried on a later call after
a transient failure, `sync.OnceValue` is the wrong tool — that would call for
an explicit retry policy layered on top, not lazy-once caching.

## Resources

- [sync.OnceValue](https://pkg.go.dev/sync#OnceValue) — caches the result of the first call, blocking concurrent callers until it's ready.
- [sync.Mutex](https://pkg.go.dev/sync#Mutex) — guards only the "does a OnceValue exist for this key" map lookup here, not the (possibly slow) work behind it.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [16-config-environment-source-cascade-validation.md](16-config-environment-source-cascade-validation.md) | Next: [18-feature-flag-table-consistency-check.md](18-feature-flag-table-consistency-check.md)
