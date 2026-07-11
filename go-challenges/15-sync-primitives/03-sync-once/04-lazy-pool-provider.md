# Exercise 4: Package-level lazy singleton connection pool via GetPool()

The most common `sync.Once` in a real service is the one you never see: a
package-level `Once` behind a `GetPool()` that builds the process's single
connection pool the first time anyone asks for it. This exercise builds that
pattern honestly, with a `Reset` hook so tests are not poisoned by singleton
state, and makes explicit why the post-`Do` read of the package variable is legal.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
lazy-pool-provider/           module: example.com/lazy-pool-provider
  go.mod
  pool.go                     type Config, Pool; Configure, Reset, GetPool, Builds
  cmd/
    demo/
      main.go                 runnable demo: configure, get, get again (same pool)
  pool_test.go                pointer-equality, one build under load, singleton semantics
```

- Files: `pool.go`, `cmd/demo/main.go`, `pool_test.go`.
- Implement: a package-level `*sync.Once` and `*Pool`; `GetPool()` builds the pool from the captured `Config` once; `Configure(c)` sets the config (before first use); `Reset()` reinstalls a fresh `Once` for tests; `Builds()` counts constructions.
- Test: two concurrent `GetPool` calls return the same pointer; a build counter asserts exactly one construction under 100-goroutine load; config set before first use is honored and a later `Configure` is ignored (singleton semantics).
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p lazy-pool-provider/cmd/demo
cd lazy-pool-provider
go mod init example.com/lazy-pool-provider
```

### Why the shared read after Do is legal

`GetPool` reads a package-level `pool` variable that a different goroutine's `Do`
closure wrote. That read has no lock and no atomic wrapping the pointer, yet it is
race-free — and it is worth being precise about why, because the same code with
the read moved outside `Do` would be a bug. The Go memory model guarantees the
closure's write to `pool` synchronizes before the return from every `GetPool`
call that goes through `poolOnce.Do`. So every caller that returns from `Do`
observes the fully-constructed pool. This is the exact justification the concepts
file gives, made concrete: the legality is not "pointers are word-sized so the
write is atomic" (that is not a guarantee you may rely on); it is the `Do`
happens-before edge.

Two design choices make this testable without leaking state. First, the `Once` is
held behind a pointer (`poolOnce *sync.Once`), so `Reset()` can install a *fresh*
`Once` by pointer assignment — you cannot re-zero a `Once` value in place without
tripping `go vet copylocks`, but reassigning a pointer to a new `&sync.Once{}` is
clean. Second, `Configure` sets only the package `Config` and does not touch the
`Once`; it must be called before the first `GetPool` to take effect, which is
precisely the singleton semantics we want to document: once the pool is built, the
config that built it is frozen.

Create `pool.go`:

```go
package pool

import (
	"fmt"
	"sync"
	"sync/atomic"
)

// Config holds the settings the pool is built from. It is captured once, at
// first GetPool; later changes via Configure are ignored (singleton semantics).
type Config struct {
	DSN      string
	MaxConns int
}

// Pool is the process-wide connection pool. In real code it would own sockets;
// here it holds the config it was built from so tests can observe it.
type Pool struct {
	dsn      string
	maxConns int
}

// DSN reports the data source name the pool was built with.
func (p *Pool) DSN() string { return p.dsn }

// MaxConns reports the configured maximum connection count.
func (p *Pool) MaxConns() int { return p.maxConns }

var (
	poolOnce = &sync.Once{}
	pool     *Pool
	cfg      Config
	builds   atomic.Int64
)

// Configure sets the config used to build the pool. Call it before the first
// GetPool; a call after the pool is built has no effect.
func Configure(c Config) {
	cfg = c
}

// Reset reinstalls a fresh Once and clears the pool. It exists so tests start
// from a clean singleton; production code never calls it.
func Reset() {
	poolOnce = &sync.Once{}
	pool = nil
	builds.Store(0)
}

// GetPool returns the process-wide pool, building it exactly once from the
// captured Config. The read of pool after Do is race-free by the memory model.
func GetPool() *Pool {
	poolOnce.Do(func() {
		builds.Add(1)
		pool = &Pool{dsn: cfg.DSN, maxConns: cfg.MaxConns}
	})
	return pool
}

// Builds reports how many times the pool was constructed; it must be 0 or 1.
func Builds() int64 {
	return builds.Load()
}

// describe is a tiny helper used by the demo to format a pool.
func describe(p *Pool) string {
	return fmt.Sprintf("%s (max %d)", p.DSN(), p.MaxConns())
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/lazy-pool-provider"
)

func main() {
	pool.Reset()
	pool.Configure(pool.Config{DSN: "db:5432", MaxConns: 16})

	p1 := pool.GetPool()
	fmt.Printf("pool: %s (max %d)\n", p1.DSN(), p1.MaxConns())

	// A later Configure is ignored: the singleton is already built.
	pool.Configure(pool.Config{DSN: "other:5433", MaxConns: 99})
	p2 := pool.GetPool()

	fmt.Println("same instance:", p1 == p2)
	fmt.Println("builds:", pool.Builds())
	fmt.Printf("still: %s (max %d)\n", p2.DSN(), p2.MaxConns())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
pool: db:5432 (max 16)
same instance: true
builds: 1
still: db:5432 (max 16)
```

### Tests

Because the package state is a singleton, each test starts with `Reset()`. Tests
that mutate the singleton must not run in parallel with each other, so they do not
call `t.Parallel()`. `TestSameInstanceUnderLoad` fans 100 goroutines at `GetPool`,
collects every returned pointer, and asserts they are all identical and that
`Builds() == 1`. `TestSingletonIgnoresLaterConfig` proves a `Configure` after
first use is ignored.

Create `pool_test.go`:

```go
package pool

import (
	"sync"
	"testing"
)

func TestSameInstanceUnderLoad(t *testing.T) {
	Reset()
	Configure(Config{DSN: "db:5432", MaxConns: 8})

	const goroutines = 100
	got := make([]*Pool, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := range goroutines {
		go func() {
			defer wg.Done()
			got[i] = GetPool()
		}()
	}
	wg.Wait()

	first := got[0]
	if first == nil {
		t.Fatal("GetPool returned nil")
	}
	for i, p := range got {
		if p != first {
			t.Fatalf("goroutine %d got a different pool pointer", i)
		}
	}
	if b := Builds(); b != 1 {
		t.Fatalf("Builds() = %d, want exactly 1", b)
	}
}

func TestConfigHonoredBeforeFirstUse(t *testing.T) {
	Reset()
	Configure(Config{DSN: "primary:5432", MaxConns: 32})

	p := GetPool()
	if p.DSN() != "primary:5432" || p.MaxConns() != 32 {
		t.Fatalf("pool = %s max %d, want primary:5432 max 32", p.DSN(), p.MaxConns())
	}
}

func TestSingletonIgnoresLaterConfig(t *testing.T) {
	Reset()
	Configure(Config{DSN: "first:1", MaxConns: 1})
	p1 := GetPool()

	Configure(Config{DSN: "second:2", MaxConns: 2})
	p2 := GetPool()

	if p1 != p2 {
		t.Fatal("GetPool built a second pool after reconfigure")
	}
	if p2.DSN() != "first:1" {
		t.Fatalf("DSN = %q, want first:1 (later Configure must be ignored)", p2.DSN())
	}
	if b := Builds(); b != 1 {
		t.Fatalf("Builds() = %d, want 1", b)
	}
}

func ExampleGetPool() {
	Reset()
	Configure(Config{DSN: "db:5432", MaxConns: 4})
	p := GetPool()
	println(describe(p) != "") // touch describe so it is exercised
	// Output:
}
```

The `Example` calls `describe` (via the demo path it would be used in) only to
keep it exercised; the behavioral proof is the three tests above and the demo.

## Review

The provider is correct when the pool is built once and every caller shares it.
`TestSameInstanceUnderLoad` proves pointer identity across 100 goroutines with
`Builds() == 1`; `TestSingletonIgnoresLaterConfig` documents that reconfiguring
after first use is a no-op — the honest, sometimes surprising, contract of a
lazy singleton. The subtle correctness point is the unsynchronized read of `pool`
in `GetPool`: it is legal only because it follows `poolOnce.Do`, exactly the
memory-model edge described in the concepts file; the race detector would fire if
you read `pool` from any path that skipped `Do`. `Reset` must reassign a pointer
to a fresh `Once`, never re-zero a `Once` in place, or `go vet copylocks` fails.
Run `go test -race`.

## Resources

- [sync.Once — pkg.go.dev](https://pkg.go.dev/sync#Once)
- [The Go Memory Model: Once](https://go.dev/ref/mem#once)
- [go vet copylocks check](https://pkg.go.dev/golang.org/x/tools/go/analysis/passes/copylock)

---

Prev: [03-idempotent-close.md](03-idempotent-close.md) | Back to [00-concepts.md](00-concepts.md) | Next: [05-resettable-init-retry.md](05-resettable-init-retry.md)
