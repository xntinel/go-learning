# Exercise 3: A Hand-Rolled Provider Container

When several services share the same expensively-constructed dependency — one
logger, one database pool — you want it built once and reused across the whole
graph, not reconstructed at every constructor call. This exercise builds the
idiomatic Go answer: a small container struct that holds the config and exposes
memoized provider methods, each lazily constructing its node on first request,
caching it with a `sync.Once`, and handing the same shared instance to everything
downstream. It is plain Go — a struct, methods, and one `sync.Once` per node — and
it is exactly the shape a code generator like Wire emits.

This module is fully self-contained. It starts with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
container.go         Config; Logger, Store, Service nodes; Container with lazy
                     memoized Logger()/Store()/Service() providers (sync.Once each)
                     and build counters proving each node is constructed once
cmd/
  demo/
    main.go          build the container, resolve the Service, run a little work
container_test.go    singletons are shared, construction is lazy, and concurrent
                     resolution still builds each node exactly once (race-checked)
```

- Files: `container.go`, `cmd/demo/main.go`, `container_test.go`.
- Implement: `New(cfg)` and the memoized providers `Logger()`, `Store()`, `Service()`, each building its node once and caching it.
- Test: the same pointer comes back every time, nothing is built until requested, and concurrent resolution builds each node exactly once.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p provider-container/cmd/demo && cd provider-container
go mod init example.com/provider-container
```

### Why memoized providers, and why `sync.Once`

Manual wiring in `main` is the right default, but it has one awkward case: a shared
dependency. If three services each need the logger and you wire by hand, you must
construct the logger first and remember to thread the *same* value into all three —
and if you slip and call `newLogger()` twice, you now have two loggers and a bug
that only shows up as duplicated or split output. A provider container removes the
chance to slip. Each dependency has exactly one provider method, that method is the
only way to obtain the node, and the method guarantees the node is built once and
the same instance returned forever after. `Service()` asks for `Store()` and
`Logger()`; `Store()` asks for `Logger()`; all of them flow to one `*Logger`,
because there is one provider for it and it memoizes.

Two properties make a provider correct. It must be lazy: a node is constructed only
when something first asks for it, so a container you build but only partly use never
pays to construct the unused half. And it must be a singleton: every call after the
first returns the cached instance rather than rebuilding. `sync.Once` delivers both
in one primitive. `once.Do(f)` runs `f` exactly once across the lifetime of the
container no matter how many goroutines call it; the first caller runs the
constructor while every other caller blocks until it finishes, and all of them then
observe the same cached field. That blocking-until-built behavior is what makes the
container safe for concurrent first-use: two goroutines that call `Service()` at the
same moment cannot race to build two services, because `Do` serializes the first
construction and the race detector confirms it. The per-node build counters in this
exercise exist precisely to prove that guarantee — each must read exactly one no
matter how many goroutines resolved through it.

The construction order falls out of the provider call graph automatically. You never
write "build the logger first"; `Store()` simply calls `Logger()` inside its own
`Once`, so the logger is built the moment the store needs it and reused when the
service needs it again. The graph wires itself in dependency order because each
provider pulls its own inputs.

Create `container.go`:

```go
package app

import (
	"fmt"
	"sync"
	"sync/atomic"
)

// Config carries the values the providers need to construct the graph.
type Config struct {
	Prefix string
	DSN    string
}

// Logger is the shared leaf dependency: many nodes write through one instance.
type Logger struct {
	prefix string
	mu     sync.Mutex
	lines  []string
}

func (l *Logger) Logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, l.prefix+fmt.Sprintf(format, args...))
}

func (l *Logger) Lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.lines))
	copy(out, l.lines)
	return out
}

// Store depends on the Logger. It is a node in the middle of the graph.
type Store struct {
	dsn  string
	log  *Logger
	mu   sync.Mutex
	data map[string]string
}

func newStore(dsn string, log *Logger) *Store {
	return &Store{dsn: dsn, log: log, data: make(map[string]string)}
}

func (s *Store) Put(key, value string) {
	s.mu.Lock()
	s.data[key] = value
	s.mu.Unlock()
	s.log.Logf("store put %s", key)
}

func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.data)
}

// Service is the root of the graph: it needs both the Store and the Logger.
type Service struct {
	store *Store
	log   *Logger
}

func (svc *Service) Register(name string) {
	svc.store.Put(name, "active")
	svc.log.Logf("registered %s", name)
}

// Container holds the config and one memoized provider per node. Each sync.Once
// guarantees its node is constructed exactly once; the atomic counters record
// how many times each constructor actually ran so tests can prove "exactly once".
type Container struct {
	cfg Config

	logOnce sync.Once
	log     *Logger

	storeOnce sync.Once
	store     *Store

	svcOnce sync.Once
	svc     *Service

	logBuilds   atomic.Int32
	storeBuilds atomic.Int32
	svcBuilds   atomic.Int32
}

// New returns an empty container. Nothing is constructed yet: every node is built
// lazily on first request.
func New(cfg Config) *Container {
	return &Container{cfg: cfg}
}

// Logger lazily builds and memoizes the shared logger.
func (c *Container) Logger() *Logger {
	c.logOnce.Do(func() {
		c.logBuilds.Add(1)
		c.log = &Logger{prefix: c.cfg.Prefix}
	})
	return c.log
}

// Store lazily builds the store, pulling its Logger dependency from the same
// container so the logger stays a singleton.
func (c *Container) Store() *Store {
	c.storeOnce.Do(func() {
		c.storeBuilds.Add(1)
		c.store = newStore(c.cfg.DSN, c.Logger())
	})
	return c.store
}

// Service lazily builds the root, wiring in the shared Store and Logger.
func (c *Container) Service() *Service {
	c.svcOnce.Do(func() {
		c.svcBuilds.Add(1)
		c.svc = &Service{store: c.Store(), log: c.Logger()}
	})
	return c.svc
}

// Build counts, exposed so tests can assert each node was constructed once.
func (c *Container) LogBuilds() int   { return int(c.logBuilds.Load()) }
func (c *Container) StoreBuilds() int { return int(c.storeBuilds.Load()) }
func (c *Container) SvcBuilds() int   { return int(c.svcBuilds.Load()) }
```

Each provider is the same three-line shape: `Once.Do` runs the constructor and
bumps the build counter, then the method returns the cached field. `Store()` and
`Service()` obtain their dependencies by calling the sibling providers, so the one
`*Logger` flows to both the store and the service and the graph self-assembles in
dependency order. The counters are `atomic.Int32` so the concurrent test can read
them without racing the goroutines that triggered the builds.

### The runnable demo

The demo is the composition root: it constructs the container with a config, asks
for the `Service` once, and runs a little work through it. The build counts it
prints make the singleton behavior visible — one logger, one store, one service,
no matter how the graph was traversed.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/provider-container"
)

func main() {
	c := app.New(app.Config{Prefix: "[app] ", DSN: "memory://demo"})

	svc := c.Service()
	fmt.Printf("service ready (logger builds=%d store builds=%d)\n", c.LogBuilds(), c.StoreBuilds())

	svc.Register("alice")
	svc.Register("bob")

	for _, line := range c.Logger().Lines() {
		fmt.Println(line)
	}
	fmt.Printf("users registered: %d\n", c.Store().Len())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
service ready (logger builds=1 store builds=1)
[app] store put alice
[app] registered alice
[app] store put bob
[app] registered bob
users registered: 2
```

### Tests

`TestSingletonsAreShared` proves the memoization: calling a provider twice returns
the identical pointer, and the store and service hold the very same `*Logger` the
container hands out. `TestConstructionIsLazy` proves nothing is built until asked
for — a fresh container has all build counts at zero, and resolving only the logger
leaves the store and service unbuilt. `TestConcurrentResolveBuildsOnce` launches
many goroutines that all resolve the `Service` at once and asserts they receive one
identical pointer and that each node was constructed exactly once; under `-race`
this is the proof that `sync.Once` makes concurrent first-use safe.

Create `container_test.go`:

```go
package app

import (
	"sync"
	"testing"
)

func TestSingletonsAreShared(t *testing.T) {
	t.Parallel()

	c := New(Config{Prefix: "[t] ", DSN: "memory://test"})

	if c.Logger() != c.Logger() {
		t.Error("Logger() returned different instances")
	}
	if c.Store() != c.Store() {
		t.Error("Store() returned different instances")
	}
	if c.Service() != c.Service() {
		t.Error("Service() returned different instances")
	}
	if c.Store().log != c.Logger() {
		t.Error("Store does not share the container's Logger singleton")
	}
	if c.Service().store != c.Store() {
		t.Error("Service does not share the container's Store singleton")
	}
	if c.LogBuilds() != 1 || c.StoreBuilds() != 1 || c.SvcBuilds() != 1 {
		t.Errorf("builds = log:%d store:%d svc:%d, want 1 each",
			c.LogBuilds(), c.StoreBuilds(), c.SvcBuilds())
	}
}

func TestConstructionIsLazy(t *testing.T) {
	t.Parallel()

	c := New(Config{Prefix: "[t] "})
	if c.LogBuilds() != 0 || c.StoreBuilds() != 0 || c.SvcBuilds() != 0 {
		t.Fatalf("nothing should be built yet: log:%d store:%d svc:%d",
			c.LogBuilds(), c.StoreBuilds(), c.SvcBuilds())
	}

	_ = c.Logger()
	if c.LogBuilds() != 1 {
		t.Errorf("logger builds = %d, want 1", c.LogBuilds())
	}
	if c.StoreBuilds() != 0 || c.SvcBuilds() != 0 {
		t.Errorf("resolving the logger must not build the store or service: store:%d svc:%d",
			c.StoreBuilds(), c.SvcBuilds())
	}
}

func TestConcurrentResolveBuildsOnce(t *testing.T) {
	t.Parallel()

	c := New(Config{Prefix: "[t] ", DSN: "memory://race"})

	const n = 50
	var wg sync.WaitGroup
	got := make([]*Service, n)
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			got[i] = c.Service()
		}()
	}
	wg.Wait()

	for i := 1; i < n; i++ {
		if got[i] != got[0] {
			t.Fatalf("goroutine %d got a different Service instance", i)
		}
	}
	if c.LogBuilds() != 1 || c.StoreBuilds() != 1 || c.SvcBuilds() != 1 {
		t.Errorf("concurrent resolution built more than once: log:%d store:%d svc:%d",
			c.LogBuilds(), c.StoreBuilds(), c.SvcBuilds())
	}
}
```

## Review

The container is correct when each provider is lazy and idempotent: nothing is
constructed by `New`, the first call to a provider builds its node, and every later
call — and every concurrent call — returns that same instance. The build counters
make the guarantee testable; each must read exactly one after any amount of
resolution, including the 50-goroutine race test, which is the concrete proof that
`sync.Once` serializes the first construction. Confirm the store and service hold
the container's one `*Logger`, not private copies, because the entire reason to use
a container instead of hand-wiring is to make the shared singleton impossible to
duplicate.

The common mistakes are two. The first is reaching for a container at all when the
graph is three nodes wired once in `main` — there manual injection is clearer and a
container is ceremony; the container earns its place only when a shared, expensive
dependency must be a true singleton across several consumers. The second is
hand-rolling the memoization with a plain `if c.log == nil { c.log = ... }` check,
which races under concurrent first-use: two goroutines can both see `nil` and both
construct. `sync.Once` is the primitive that closes that window, and the race test
exists to keep you honest about it.

## Resources

- [`sync.Once`](https://pkg.go.dev/sync#Once) — the once-only execution primitive that makes each lazy provider a thread-safe singleton.
- [`sync/atomic`](https://pkg.go.dev/sync/atomic) — `atomic.Int32`, used for the build counters read concurrently with the goroutines that trigger construction.
- [Google Wire: best practices](https://github.com/google/wire/blob/main/docs/best-practices.md) — how a code generator builds exactly this kind of provider graph at compile time instead of by hand.

---

Back to [02-injecting-seams.md](02-injecting-seams.md) | Next: [04-layered-http-service.md](04-layered-http-service.md)
