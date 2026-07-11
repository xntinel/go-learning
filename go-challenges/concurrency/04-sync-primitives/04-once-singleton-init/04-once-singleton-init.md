# Exercise 4: Once: Singleton Initialization

In any server application, expensive resources -- database connection pools,
gRPC clients, TLS configurations -- should be created once and shared. Lazy
initialization builds the resource on first use rather than at startup, which
avoids paying the cost if it is never needed and simplifies dependency
ordering. But in a concurrent server, dozens of request-handling goroutines may
call `GetDB()` at the same instant during startup, and without synchronization
that yields multiple connection pools, wasted file descriptors, redundant
migrations, or corrupted half-built state. `sync.Once` makes the initializer run
exactly once no matter how many goroutines race into it.

## What you'll build

```text
04-once-singleton-init/
  main.go        a lazily-initialized DB pool -- the unsafe version, the
                 sync.Once fix, and the OnceValue/OnceFunc variants
```

- Build: a lazily-initialized DB pool that survives 20 concurrent handlers without ever being built twice.
- Implement: the unsafe nil-check factory that double-initializes, the `sync.Once` fix, then `sync.OnceValue` and `sync.OnceFunc` versions, and a tour of Once's edge behaviors (different function ignored, panic counts as done, recursive `Do` deadlocks).
- Verify: `go run main.go`, and `go run -race main.go` to see the unsafe race and confirm the fix clears it.

### Why not a plain mutex

But in a concurrent server, dozens of goroutines handling incoming requests might call `GetDB()` simultaneously during startup. Without synchronization, you get multiple connection pools, wasted file descriptors, redundant migrations, or corrupted state from partially initialized resources.

You might think a mutex solves this:
```go
mu.Lock()
if pool == nil {
    pool = createConnectionPool()
}
mu.Unlock()
```

This works but has a cost: every subsequent call pays the lock overhead even though initialization is already done. The "double-checked locking" optimization is notoriously tricky to get right across memory models.

`sync.Once` solves this elegantly. It guarantees that the function passed to `Do` executes exactly once, regardless of how many goroutines call it concurrently. After the first call completes, all subsequent calls return immediately with zero overhead.

## Step 1 -- The Double Initialization Problem

Multiple HTTP handlers call `GetDB()` on the first request. Without `sync.Once`, the connection pool is created multiple times:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const (
	concurrentHandlers   = 20
	connectionSetupDelay = 50 * time.Millisecond
	defaultDBHost        = "postgres:5432"
	defaultMaxConns      = 25
)

type DBPool struct {
	Host        string
	MaxConns    int
	Initialized bool
}

type UnsafePoolFactory struct {
	pool      *DBPool
	initCount atomic.Int64
}

func (f *UnsafePoolFactory) GetDB() *DBPool {
	if f.pool == nil { // UNSAFE: multiple goroutines can pass this check
		f.initCount.Add(1)
		fmt.Printf("  Initializing DB pool... (goroutine #%d to reach this point)\n", f.initCount.Load())
		time.Sleep(connectionSetupDelay)
		f.pool = &DBPool{Host: defaultDBHost, MaxConns: defaultMaxConns, Initialized: true}
	}
	return f.pool
}

func demonstrateUnsafeInit() {
	factory := &UnsafePoolFactory{}
	var wg sync.WaitGroup

	fmt.Println("=== Unsafe Initialization (race condition) ===")
	for i := 0; i < concurrentHandlers; i++ {
		wg.Add(1)
		go func(handlerID int) {
			defer wg.Done()
			_ = factory.GetDB()
		}(i)
	}

	wg.Wait()
	fmt.Printf("DB pool created %d times (should be 1)\n", factory.initCount.Load())
	fmt.Println("Wasted connections, possible corruption, resource leak.")
}

func main() {
	demonstrateUnsafeInit()
}
```

Expected output:
```
=== Unsafe Initialization (race condition) ===
  Initializing DB pool... (goroutine #1 to reach this point)
  Initializing DB pool... (goroutine #2 to reach this point)
  Initializing DB pool... (goroutine #3 to reach this point)
  ...
DB pool created 15+ times (should be 1)
Wasted connections, possible corruption, resource leak.
```

### Verification
Run several times. The initialization count should vary but consistently be greater than 1. With `-race`, you will also see data race warnings on the `pool` variable.

## Step 2 -- Fix with sync.Once

`sync.Once` guarantees the initialization runs exactly once. All concurrent callers block until the first execution completes, then return immediately:

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const (
	concurrentHandlers = 20
	tcpHandshakeDelay  = 100 * time.Millisecond
	migrationDelay     = 50 * time.Millisecond
)

type DBPool struct {
	Host      string
	MaxConns  int
	Connected bool
}

func createDBPool() *DBPool {
	fmt.Println("  Connecting to database...")
	time.Sleep(tcpHandshakeDelay)
	fmt.Println("  Running migration check...")
	time.Sleep(migrationDelay)
	pool := &DBPool{Host: "postgres:5432", MaxConns: 25, Connected: true}
	fmt.Println("  DB pool ready.")
	return pool
}

func demonstrateSafeInit() {
	var once sync.Once
	var pool *DBPool
	var wg sync.WaitGroup
	start := time.Now()

	for i := 0; i < concurrentHandlers; i++ {
		wg.Add(1)
		go func(handlerID int) {
			defer wg.Done()
			once.Do(func() { pool = createDBPool() }) // only the first caller executes
			fmt.Printf("  Handler %d: using pool (connected=%v) at %v\n",
				handlerID, pool.Connected, time.Since(start).Round(time.Millisecond))
		}(i)
	}

	wg.Wait()
}

func main() {
	fmt.Println("=== Safe Initialization with sync.Once ===")
	demonstrateSafeInit()
	fmt.Println("\nAll 20 handlers share the same pool. Initialization happened exactly once.")
}
```

Expected output:
```
=== Safe Initialization with sync.Once ===
  Connecting to database...
  Running migration check...
  DB pool ready.
  Handler 3: using pool (connected=true) at 153ms
  Handler 0: using pool (connected=true) at 153ms
  ...

All 20 handlers share the same pool. Initialization happened exactly once.
```

### Verification
```bash
go run -race main.go
```
"Connecting to database..." appears exactly once. All 20 handlers report `connected=true`. No race warnings.

## Step 3 -- sync.OnceValue: Lazy DB Connection with Return Value

Go 1.21 introduced `sync.OnceValue` for functions that return a value. This is the cleanest pattern for lazy singletons:

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const (
	poolEstablishDelay = 80 * time.Millisecond
	handlerCount       = 10
)

type DBPool struct {
	Host     string
	MaxConns int
	PingOK   bool
}

func establishConnectionPool() *DBPool {
	fmt.Println("  [init] Establishing connection pool...")
	time.Sleep(poolEstablishDelay)
	pool := &DBPool{
		Host:     "postgres.internal:5432",
		MaxConns: 25,
		PingOK:   true,
	}
	fmt.Println("  [init] Pool ready.")
	return pool
}

func main() {
	getPool := sync.OnceValue(establishConnectionPool)

	var wg sync.WaitGroup
	for i := 0; i < handlerCount; i++ {
		wg.Add(1)
		go func(handlerID int) {
			defer wg.Done()
			pool := getPool() // first call initializes, rest return cached
			fmt.Printf("  Handler %d: host=%s maxConns=%d\n", handlerID, pool.Host, pool.MaxConns)
		}(i)
	}

	wg.Wait()
}
```

Expected output:
```
  [init] Establishing connection pool...
  [init] Pool ready.
  Handler 0: host=postgres.internal:5432 maxConns=25
  Handler 1: host=postgres.internal:5432 maxConns=25
  ...
```

`sync.OnceValue` returns a function that, on first call, executes the initializer and caches the result. All subsequent calls return the cached value without re-executing. No separate variable needed to store the singleton.

### Verification
```bash
go run main.go
```
"Establishing connection pool..." appears exactly once. All handlers print the same host and maxConns.

## Step 4 -- sync.OnceFunc: One-Time Migration Runner

`sync.OnceFunc` wraps a function with no return value, useful for one-time side effects like running migrations or registering metrics:

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const handlerCount = 5

func applyDatabaseMigrations() {
	fmt.Println("  [migrate] Checking schema version...")
	time.Sleep(50 * time.Millisecond)
	fmt.Println("  [migrate] Applying migration 001_create_users...")
	time.Sleep(30 * time.Millisecond)
	fmt.Println("  [migrate] Applying migration 002_add_sessions...")
	time.Sleep(30 * time.Millisecond)
	fmt.Println("  [migrate] All migrations applied.")
}

func main() {
	runMigrations := sync.OnceFunc(applyDatabaseMigrations)

	var wg sync.WaitGroup
	for i := 0; i < handlerCount; i++ {
		wg.Add(1)
		go func(handlerID int) {
			defer wg.Done()
			runMigrations() // only first call runs the migrations
			fmt.Printf("  Handler %d: serving requests (migrations complete)\n", handlerID)
		}(i)
	}

	wg.Wait()
}
```

Expected output:
```
  [migrate] Checking schema version...
  [migrate] Applying migration 001_create_users...
  [migrate] Applying migration 002_add_sessions...
  [migrate] All migrations applied.
  Handler 0: serving requests (migrations complete)
  Handler 1: serving requests (migrations complete)
  ...
```

### Verification
```bash
go run main.go
```
Migration messages appear once. All handlers proceed after migrations complete.

## Step 5 -- Important Behaviors

Three critical properties of `sync.Once` that affect production code:

**1. Once tracks calls, not functions.** Passing a different function to a second `Do` call has no effect:

```go
package main

import (
	"fmt"
	"sync"
)

func main() {
	var once sync.Once
	once.Do(func() { fmt.Println("postgres pool created") })
	once.Do(func() { fmt.Println("redis pool created") }) // NEVER executes
	fmt.Println("Only one pool was created.")
}
```

**2. Panic is considered "done".** If the initializer panics, `Once` still marks it as completed. Subsequent `Do` calls will NOT retry:

```go
package main

import (
	"fmt"
	"sync"
)

func main() {
	var once sync.Once

	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("Recovered from panic: %v\n", r)
		}
		// This will NOT retry the initialization
		once.Do(func() {
			fmt.Println("This retry never runs because Once is already 'done'")
		})
		fmt.Println("Once considers the panicked call as 'done'. No retry.")
	}()

	once.Do(func() {
		panic("connection refused")
	})
}
```

**3. Recursive Do deadlocks.** Calling `once.Do(f)` from within `f` will deadlock because the inner `Do` waits for the outer to complete:

```go
// DO NOT RUN -- this deadlocks
var once sync.Once
once.Do(func() {
    once.Do(func() { // DEADLOCK: inner Do waits for outer
        fmt.Println("inner")
    })
})
```

### Verification
```bash
go run main.go
```
The first two behaviors should demonstrate their properties. Do not run the recursive example -- it hangs.

## Common Mistakes

### Passing Different Functions to Do

```go
package main

import (
	"fmt"
	"sync"
)

func main() {
	var once sync.Once
	once.Do(func() { fmt.Println("init postgres") })
	once.Do(func() { fmt.Println("init redis") }) // never executes!
	// Output: only "init postgres"
}
```

**What happens:** The second function is silently ignored. `sync.Once` guarantees exactly one execution, regardless of which function is passed.

**Fix:** Use separate `sync.Once` instances for independent initializations.

### Deadlock Inside Once

```go
var once sync.Once
once.Do(func() {
    once.Do(func() { // DEADLOCK: recursive Do call
        fmt.Println("inner")
    })
})
```

**What happens:** Deadlock. The inner `Do` waits for the outer `Do` to complete, which waits for the inner `Do`.

**Fix:** Never call `Do` recursively on the same `sync.Once`.

### Ignoring Panics in Once
If the function passed to `Do` panics, `sync.Once` still considers it "done". Subsequent calls to `Do` will not re-execute. If initialization can fail, handle errors inside the function:

```go
package main

import (
	"errors"
	"fmt"
	"sync"
)

func main() {
	var once sync.Once
	var pool *struct{ Host string }
	var initErr error

	initPool := func() {
		pool, initErr = nil, errors.New("connection refused: postgres:5432")
		if initErr != nil {
			fmt.Printf("Init failed: %v (will not retry with sync.Once)\n", initErr)
		}
	}

	once.Do(initPool)
	fmt.Printf("pool=%v, err=%v\n", pool, initErr)

	// Callers must check initErr before using pool
	if initErr != nil {
		fmt.Println("Application should fail fast or implement retry outside of Once.")
	}
}
```

## Review

`sync.Once` guarantees the function passed to `Do` runs exactly once, no matter
how many goroutines call it concurrently: the first caller executes the
initializer while every other caller blocks until it finishes, and from then on
each call returns immediately with no lock overhead. That makes it the right
tool for lazily building expensive singletons -- DB pools, gRPC clients, config
loaders -- and the reason to prefer it over hand-written double-checked locking,
which is easy to get subtly wrong across the memory model. The Go 1.21 helpers
sharpen the pattern: `sync.OnceValue` caches and returns the initializer's
result so you need no separate variable, and `sync.OnceFunc` wraps a
side-effect-only function like a one-time migration. Two behaviors bite if you
forget them -- a panic still marks the Once as done, so there is no retry, and
calling `Do` recursively on the same Once deadlocks because the inner call waits
on the outer.

To confirm the concepts hold together, build a `ServiceRegistry` that lazily
initializes three independent resources -- a DB pool, a Redis client, and a gRPC
connection -- each behind its own `sync.OnceValue`. Every resource must
initialize independently on first access, and a driver that hits all three from
100 concurrent goroutines should prove each one was built exactly once. If you
reach for a shared Once across the three, ask yourself why "Once tracks calls,
not functions" makes that wrong.

## Resources
- [sync.Once documentation](https://pkg.go.dev/sync#Once) -- the exactly-once `Do` contract and its blocking behavior.
- [sync.OnceValue documentation](https://pkg.go.dev/sync#OnceValue) -- the Go 1.21 helper that caches an initializer's return value.
- [sync.OnceFunc documentation](https://pkg.go.dev/sync#OnceFunc) -- the Go 1.21 helper that wraps a side-effect-only initializer.
- [The Go Memory Model](https://go.dev/ref/mem) -- the happens-before guarantee that makes the initialized value visible to every later caller.

---

Back to [Concurrency](../../concurrency.md) | Next: [05-pool-object-reuse](../05-pool-object-reuse/05-pool-object-reuse.md)
