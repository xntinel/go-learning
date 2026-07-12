# Exercise 6: Lazy, Race-Free Singletons with sync.OnceValue/OnceValues

`init()` runs eagerly at import and a nil-check singleton is a data race. The modern
replacement is a higher-order builder: `sync.OnceValue` and `sync.OnceValues` take
an expensive constructor and return a memoized accessor that runs it at most once,
on first use, safely under concurrency.

## What you'll build

```text
lazyinit/                    independent module: example.com/lazyinit
  go.mod                     go 1.25
  loader.go                  Config accessor via OnceValue; Pool accessor via OnceValues
  loader_test.go             built exactly once, identical result, error memoized, panic re-raised
  cmd/demo/
    main.go                  triggers lazy build on first use, then reuses it
```

- Files: `loader.go`, `loader_test.go`, `cmd/demo/main.go`.
- Implement: a `Loader` holding a `func() *Config` from `sync.OnceValue` and a `func() (*Pool, error)` from `sync.OnceValues`, each wrapping a builder that runs at most once.
- Test: the builder runs exactly once across many goroutines; every caller gets the identical pointer; the `OnceValues` error is memoized and returned every call; a panicking `OnceValue`/`OnceFunc` builder re-raises the same value.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/10-higher-order-functions/06-lazy-init-oncevalue/cmd/demo
cd go-solutions/04-functions/10-higher-order-functions/06-lazy-init-oncevalue
go mod edit -go=1.25
```

### What OnceValue and OnceValues actually return

`sync.OnceValue[T](f func() T) func() T` takes a builder and returns an accessor.
The first call to the accessor runs `f`, stores its result, and returns it; every
later call — from any goroutine, concurrent or not — returns the stored value
without running `f` again. `sync.OnceValues[T1,T2](f func() (T1,T2)) func() (T1,T2)`
is the same for a builder that also returns an error: both the value and the error
are memoized, so a failed initialization is returned identically on every call
rather than silently retried. `sync.OnceFunc(f func()) func()` is the no-result form
for a side-effecting one-time action.

This is strictly better than the two patterns it replaces. Eager `init()` runs the
build at import time whether or not the value is ever used, and you cannot return an
error from `init` (only panic). The nil-check singleton — `if c == nil { c =
build() }` on a package global — is a data race under concurrent first access, which
the `-race` detector flags. `OnceValue` defers the work to first use *and* is
race-free, because the synchronization lives inside the returned accessor.

Panic behavior is deliberate and worth knowing: if the builder panics, the panic
value is memoized, and every subsequent call to the accessor re-panics with the same
value. A failed initialization does not get a second chance and does not later look
successful. The test pins this.

Build the accessors once, at construction, and store the returned functions. Do not
call `sync.OnceValue` per access — that would create a fresh once each time and
defeat the memoization.

Create `loader.go`:

```go
package lazyinit

import (
	"errors"
	"sync"
	"sync/atomic"
)

// Config is an expensively-parsed configuration value.
type Config struct {
	Env      string
	MaxConns int
}

// Pool stands in for an expensive resource (a DB connection pool) whose
// construction can fail.
type Pool struct {
	DSN string
}

// Loader exposes lazy, race-free accessors built from sync.OnceValue and
// sync.OnceValues. The builders run at most once, on first use.
type Loader struct {
	config func() *Config
	pool   func() (*Pool, error)
	builds atomic.Int64 // counts how many times a builder actually ran
}

// NewLoader wires the accessors. buildConfig and buildPool run at most once each,
// on first access, no matter how many goroutines call concurrently.
func NewLoader(dsn string) *Loader {
	l := &Loader{}
	l.config = sync.OnceValue(func() *Config {
		l.builds.Add(1)
		return &Config{Env: "prod", MaxConns: 32}
	})
	l.pool = sync.OnceValues(func() (*Pool, error) {
		l.builds.Add(1)
		if dsn == "" {
			return nil, errors.New("empty DSN")
		}
		return &Pool{DSN: dsn}, nil
	})
	return l
}

// Config returns the parsed config, building it on first use.
func (l *Loader) Config() *Config { return l.config() }

// Pool returns the resource or the memoized construction error.
func (l *Loader) Pool() (*Pool, error) { return l.pool() }

// Builds reports how many builder invocations actually happened (for tests).
func (l *Loader) Builds() int64 { return l.builds.Load() }
```

### The runnable demo

The demo shows the value is built on first use and reused thereafter: it reads the
config twice and reports the build count stayed at one.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/lazyinit"
)

func main() {
	l := lazyinit.NewLoader("postgres://localhost/app")

	fmt.Printf("builds before first use: %d\n", l.Builds())

	c1 := l.Config()
	c2 := l.Config()
	fmt.Printf("config env=%s maxConns=%d\n", c1.Env, c1.MaxConns)
	fmt.Printf("same pointer reused: %v\n", c1 == c2)

	p, err := l.Pool()
	fmt.Printf("pool dsn=%s err=%v\n", p.DSN, err)

	fmt.Printf("builder invocations total: %d\n", l.Builds())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
builds before first use: 0
config env=prod maxConns=32
same pointer reused: true
pool dsn=postgres://localhost/app err=<nil>
builder invocations total: 2
```

Before any access the build count is zero — nothing ran at construction. Two reads
of the config share one pointer and one build. The total of 2 is one config build
plus one pool build; each accessor ran its builder exactly once.

### Tests

The concurrency test hammers the config accessor from many goroutines and asserts
the builder ran exactly once and every caller got the identical pointer. The error
test builds a loader with an empty DSN and asserts the pool error is returned on
every call (memoized). The panic test wraps a panicking builder in `sync.OnceValue`
and asserts calling the accessor twice re-raises the same value.

Create `loader_test.go`:

```go
package lazyinit

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestConfigBuiltExactlyOnce(t *testing.T) {
	t.Parallel()

	l := NewLoader("dsn")

	const n = 100
	var wg sync.WaitGroup
	ptrs := make([]*Config, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ptrs[i] = l.Config()
		}()
	}
	wg.Wait()

	if b := l.Builds(); b != 1 {
		t.Fatalf("builder ran %d times, want 1", b)
	}
	first := ptrs[0]
	for i, p := range ptrs {
		if p != first {
			t.Fatalf("caller %d got a different *Config pointer", i)
		}
	}
}

func TestPoolErrorIsMemoized(t *testing.T) {
	t.Parallel()

	l := NewLoader("") // empty DSN -> build fails

	_, err1 := l.Pool()
	_, err2 := l.Pool()
	if err1 == nil || err2 == nil {
		t.Fatalf("want an error both times, got %v and %v", err1, err2)
	}
	if err1.Error() != err2.Error() {
		t.Fatalf("memoized error differs: %v vs %v", err1, err2)
	}
	if b := l.Builds(); b != 1 {
		t.Fatalf("pool builder ran %d times, want 1 (error memoized)", b)
	}
}

func TestOnceValueMemoizesPanic(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	get := sync.OnceValue(func() int {
		calls.Add(1)
		panic("build failed")
	})

	firstPanic := recoverValue(get)
	secondPanic := recoverValue(get)

	if firstPanic != "build failed" || secondPanic != "build failed" {
		t.Fatalf("panics = %v, %v, want both \"build failed\"", firstPanic, secondPanic)
	}
	if c := calls.Load(); c != 1 {
		t.Fatalf("builder ran %d times, want 1 (panic memoized)", c)
	}
}

// recoverValue calls get and returns the recovered panic value, or nil.
func recoverValue(get func() int) (v any) {
	defer func() { v = recover() }()
	get()
	return nil
}
```

## Review

The lazy accessors are correct when nothing is built until first use, the builder
runs exactly once no matter how many goroutines race to the first call, and every
caller receives the identical result. `sync.OnceValue`/`OnceValues` give you all of
that with no lock in your own code — the synchronization is inside the returned
accessor, which is why they replace both eager `init()` and the racy nil-check
singleton. Build the accessor once at construction and store the function; calling
`sync.OnceValue` per access defeats the memoization. Remember the panic contract:
a builder that panics memoizes the panic and re-raises it on every later call, so a
failed init stays failed rather than silently succeeding later. Run `go test -race`
to confirm the concurrent first-access path is clean.

## Resources

- [sync package](https://pkg.go.dev/sync) — `OnceFunc`, `OnceValue`, `OnceValues`, and their panic memoization.
- [Go 1.21 release notes](https://go.dev/doc/go1.21) — the introduction of the `Once*` helpers.
- [sync/atomic](https://pkg.go.dev/sync/atomic) — `atomic.Int64` for the build counter.

---

Back to [05-memoize-singleflight.md](05-memoize-singleflight.md) | Next: [07-generic-collection-ops.md](07-generic-collection-ops.md)
