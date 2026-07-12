# Exercise 8: Catching Inter-Test Coupling with -shuffle, -count, and -race

The nastiest test failures are the ones that pass in declaration order and fail
when the order changes, because one test quietly left state in a package-level
variable that another test read. This module builds a route registry, shows the
coupled test suite that passes only by luck of ordering, and the refactor plus CI
gate — `-race -count -shuffle=on` — that catches the coupling and keeps it caught.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
routereg/                   independent module: example.com/routereg
  go.mod
  registry.go               Registry: Register, Count, Handler, Routes
  cmd/
    demo/
      main.go               runnable demo: register routes, list them
  registry_test.go          order-independent parallel tests (the fixed suite)
```

Files: `registry.go`, `cmd/demo/main.go`, `registry_test.go`.
Implement: a `Registry` mapping method+path to a handler name, with `Register`,
`Count`, `Handler`, and a sorted `Routes`.
Test: order-independent parallel tests, each owning its own `Registry`, that pass
under `-race -count -shuffle=on`.
Verify: `go test -race -count=20 -shuffle=on ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/14-parallel-tests/08-flaky-detection-shuffle-count-race/cmd/demo
cd go-solutions/12-testing-ecosystem/14-parallel-tests/08-flaky-detection-shuffle-count-race
```

### The coupling bug, and how the gate exposes it

Here is the trap, using a package-level default registry (illustrative — do not
assemble it):

```go
// COUPLED AND WRONG — do not assemble.
var defaultReg = NewRegistry()

func TestRegisterHealth(t *testing.T) {
	t.Parallel()
	defaultReg.Register("GET", "/health", "health") // leaks into shared state
}

func TestCountIsOne(t *testing.T) {
	t.Parallel()
	if defaultReg.Count() != 1 { // depends on which tests ran first
		t.Fatalf("Count = %d, want 1", defaultReg.Count())
	}
}
```

In *declaration order* with the runner's default scheduling this might pass. But
these two tests are parallel and share `defaultReg`: `TestCountIsOne` races
`TestRegisterHealth`'s write (a data race `-race` will flag), and its assertion
depends on whether other tests already registered routes (an ordering dependency
`-shuffle=on` will flag by permuting the order). The suite is green today and red
on a busier machine or a different seed — the definition of flaky.

The three flags form the gate that catches this class of bug:

- `-race` instruments the shared-variable access and reports the data race
  directly.
- `-shuffle=on` randomizes test execution order and prints the seed it chose; the
  coupled suite fails under some seeds. Reproduce a specific failure with
  `-shuffle=<seed>`.
- `-count=N` re-runs the suite N times so a race or ordering bug that only
  manifests occasionally gets many chances to appear.

None of them is a proof on its own — `-race` only observes races that occur, and a
single `-shuffle` seed only tries one order — but stacked and repeated they turn a
rare flake into a reliable failure you can fix.

### The fix: no shared state

The refactor is mechanical and it is the whole lesson: remove the package-level
`defaultReg`. Each test constructs its own `NewRegistry()`, so there is nothing to
leak and nothing to depend on. Every test is now a pure function of its own setup;
order and repetition change nothing. The assembled suite below is the fixed one,
and it is designed to pass under `-race -count=20 -shuffle=on`.

Create `registry.go`:

```go
package routereg

import (
	"fmt"
	"slices"
)

// Registry maps "METHOD path" to a handler name. It is a plain value type with
// no package-level singleton, so tests construct their own and never couple.
type Registry struct {
	routes map[string]string
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{routes: make(map[string]string)}
}

func key(method, path string) string { return method + " " + path }

// Register adds a route, returning an error if the method+path is already taken.
func (r *Registry) Register(method, path, handler string) error {
	k := key(method, path)
	if _, ok := r.routes[k]; ok {
		return fmt.Errorf("route %q already registered", k)
	}
	r.routes[k] = handler
	return nil
}

// Handler returns the handler name for a route, if registered.
func (r *Registry) Handler(method, path string) (string, bool) {
	h, ok := r.routes[key(method, path)]
	return h, ok
}

// Count returns the number of registered routes.
func (r *Registry) Count() int { return len(r.routes) }

// Routes returns the route keys in sorted order (stable regardless of insertion
// order, so tests asserting on it are deterministic).
func (r *Registry) Routes() []string {
	keys := make([]string, 0, len(r.routes))
	for k := range r.routes {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/routereg"
)

func main() {
	reg := routereg.NewRegistry()
	_ = reg.Register("GET", "/health", "health")
	_ = reg.Register("POST", "/users", "createUser")
	_ = reg.Register("GET", "/users/{id}", "getUser")

	fmt.Printf("routes: %d\n", reg.Count())
	for _, r := range reg.Routes() {
		fmt.Println(r)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
routes: 3
GET /health
GET /users/{id}
POST /users
```

### Tests

Every test builds its own `NewRegistry()`. There is no package-level state, so the
tests are order-independent and safe to run in parallel, repeated, and shuffled.
`Routes()` returns a sorted slice, so the ordering assertion is deterministic
regardless of map iteration order.

Create `registry_test.go`:

```go
package routereg

import (
	"slices"
	"testing"
)

func TestRegisterAndLookup(t *testing.T) {
	t.Parallel()

	reg := NewRegistry() // own state: no coupling with any other test
	if err := reg.Register("GET", "/health", "health"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if h, ok := reg.Handler("GET", "/health"); !ok || h != "health" {
		t.Fatalf("Handler = %q,%v; want health,true", h, ok)
	}
	if reg.Count() != 1 {
		t.Fatalf("Count = %d, want 1", reg.Count())
	}
}

func TestDuplicateRejected(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	if err := reg.Register("POST", "/users", "a"); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if err := reg.Register("POST", "/users", "b"); err == nil {
		t.Fatal("duplicate Register: want error, got nil")
	}
	if reg.Count() != 1 {
		t.Fatalf("Count after duplicate = %d, want 1", reg.Count())
	}
}

func TestRoutesSorted(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	_ = reg.Register("POST", "/users", "createUser")
	_ = reg.Register("GET", "/health", "health")
	_ = reg.Register("GET", "/users/{id}", "getUser")

	want := []string{"GET /health", "GET /users/{id}", "POST /users"}
	if got := reg.Routes(); !slices.Equal(got, want) {
		t.Fatalf("Routes = %v, want %v", got, want)
	}
}
```

## Review

The suite is order-independent when no test reads or writes state another test
touches — here, because each test owns its `NewRegistry()`. The proof is that it
passes under `go test -race -count=20 -shuffle=on`: `-shuffle` permutes the order
across runs, `-count=20` repeats it, and `-race` watches for shared-memory access;
a suite with a package-level default registry fails at least one of the three. When
a shuffled run does fail, the printed seed reproduces it exactly with
`-shuffle=<seed>`, which turns a heisenbug into a deterministic one you can debug.

The discipline: treat `-race -count -shuffle=on` as the real CI gate, not plain
`go test`. A green plain run proves nothing about coupling or ordering; the stacked
flags are what make "the suite is stable" a claim you can stand behind.

## Resources

- [go command Testing flags](https://pkg.go.dev/cmd/go#hdr-Testing_flags) — `-shuffle`, `-count`, and `-race` semantics, including the printed shuffle seed.
- [Data Race Detector](https://go.dev/doc/articles/race_detector) — what `-race` observes and its limits.
- [`slices.Sort`](https://pkg.go.dev/slices#Sort) — deterministic ordering for assertions over map-derived slices.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-parallel-cleanup-ordering-and-context.md](07-parallel-cleanup-ordering-and-context.md) | Next: [09-deterministic-concurrency-synctest.md](09-deterministic-concurrency-synctest.md)
