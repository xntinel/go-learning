# 7. Concurrent Test Isolation

Parallel tests that share mutable state — global variables, package-level singletons, shared file paths, fixed TCP ports — produce flaky failures that pass in isolation and fail under `t.Parallel()`. The failure is not a race in the production code; it is a design flaw in the test setup. This lesson builds a service with shared state, writes a parallel test suite where every test is fully isolated, and verifies the result with `-race -parallel 8 -count 10`.

```text
registry/
  go.mod
  registry.go
  registry_test.go
  cmd/demo/
    main.go
```

## Concepts

### Why Shared Test State Causes Flakes

When tests run sequentially, each test sees the state left by the previous one. This is wrong but often goes unnoticed because the order is consistent. When tests run in parallel, the order is non-deterministic, and tests that modify the same state interfere with each other. A test that asserts `len(registry) == 1` fails when another test added an entry between the register and the assert.

The fix is not to remove `t.Parallel()`. The fix is to give each test its own instance of every mutable resource.

### Per-Test Instances

Every test that creates a service must create a new instance of that service, not use a shared package-level variable. If the service carries state (a registry, a counter, a cache), each test's instance is independent.

```go
// Wrong: shared mutable global
var svc = NewService()

// Right: each test creates its own instance
func TestFoo(t *testing.T) {
	t.Parallel()
	svc := NewService()
	...
}
```

### `t.TempDir` for File Isolation

`t.TempDir()` returns a directory that is unique to the test and is automatically removed when the test completes (or fails). Tests that write files must use `t.TempDir()` rather than a shared directory.

### Port 0 for Network Isolation

`net.Listen("tcp", "127.0.0.1:0")` asks the kernel to assign a random available port. Tests that start HTTP or TCP servers must use port 0. A fixed port causes `bind: address already in use` failures when two tests start a server simultaneously.

### `t.Cleanup` for Deterministic Teardown

`t.Cleanup(f)` registers `f` to run when the test (or subtest) ends, regardless of pass/fail status. It is the correct mechanism for releasing resources: closing servers, cancelling contexts, waiting for goroutines. Cleanup functions run in last-in, first-out order.

### The Table-Driven Parallel Pattern

Table-driven tests that use `t.Parallel()` inside the subtest correctly capture the loop variable because each subtest creates a new closure with its own copy of the table entry (since Go 1.22, loop variables are scoped per iteration):

```go
for _, tt := range tests {
	t.Run(tt.name, func(t *testing.T) {
		t.Parallel()
		// tt is captured correctly in Go 1.22+
	})
}
```

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/registry/cmd/demo
cd ~/go-exercises/registry
go mod init example.com/registry
```

### Exercise 1: A Registry Service With State

Create `registry.go`:

```go
package registry

import (
	"errors"
	"fmt"
	"sync"
)

// ErrAlreadyRegistered is returned when an entry with the same name exists.
var ErrAlreadyRegistered = errors.New("name already registered")

// ErrNotFound is returned when a lookup fails.
var ErrNotFound = errors.New("name not found")

// Entry holds a registered name and its associated address.
type Entry struct {
	Name string
	Addr string
}

// Registry is a concurrency-safe name -> address store.
type Registry struct {
	mu      sync.RWMutex
	entries map[string]Entry
}

// New creates an empty Registry.
func New() *Registry {
	return &Registry{entries: make(map[string]Entry)}
}

// Register adds an entry. Returns ErrAlreadyRegistered if name is taken.
func (r *Registry) Register(name, addr string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.entries[name]; ok {
		return fmt.Errorf("register %q: %w", name, ErrAlreadyRegistered)
	}
	r.entries[name] = Entry{Name: name, Addr: addr}
	return nil
}

// Lookup returns the entry for name. Returns ErrNotFound if absent.
func (r *Registry) Lookup(name string) (Entry, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[name]
	if !ok {
		return Entry{}, fmt.Errorf("lookup %q: %w", name, ErrNotFound)
	}
	return e, nil
}

// Deregister removes name. Returns ErrNotFound if absent.
func (r *Registry) Deregister(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.entries[name]; !ok {
		return fmt.Errorf("deregister %q: %w", name, ErrNotFound)
	}
	delete(r.entries, name)
	return nil
}

// Len returns the current number of registered entries.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}
```

### Exercise 2: Fully Isolated Parallel Test Suite

Create `registry_test.go`:

```go
package registry

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// newTestRegistry is a test helper that creates a fresh Registry.
// Each call returns an independent instance; tests do not share state.
func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	return New()
}

// TestRegisterAndLookup verifies basic register-then-lookup.
func TestRegisterAndLookup(t *testing.T) {
	t.Parallel()

	r := newTestRegistry(t)
	if err := r.Register("svc-a", "10.0.0.1:8080"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	e, err := r.Lookup("svc-a")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if e.Addr != "10.0.0.1:8080" {
		t.Fatalf("Addr = %q, want %q", e.Addr, "10.0.0.1:8080")
	}
}

// TestRegisterDuplicateReturnsError verifies duplicate registration is rejected.
func TestRegisterDuplicateReturnsError(t *testing.T) {
	t.Parallel()

	r := newTestRegistry(t)
	if err := r.Register("svc-b", "addr1"); err != nil {
		t.Fatal(err)
	}
	err := r.Register("svc-b", "addr2")
	if !errors.Is(err, ErrAlreadyRegistered) {
		t.Fatalf("err = %v, want ErrAlreadyRegistered", err)
	}
}

// TestLookupMissingReturnsNotFound verifies ErrNotFound on absent keys.
func TestLookupMissingReturnsNotFound(t *testing.T) {
	t.Parallel()

	r := newTestRegistry(t)
	_, err := r.Lookup("ghost")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestDeregisterRemovesEntry verifies that a deregistered name is no longer found.
func TestDeregisterRemovesEntry(t *testing.T) {
	t.Parallel()

	r := newTestRegistry(t)
	if err := r.Register("svc-c", "addr"); err != nil {
		t.Fatal(err)
	}
	if err := r.Deregister("svc-c"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Lookup("svc-c"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after deregister: err = %v, want ErrNotFound", err)
	}
}

// TestDeregisterMissingReturnsNotFound verifies ErrNotFound for absent deregistration.
func TestDeregisterMissingReturnsNotFound(t *testing.T) {
	t.Parallel()

	r := newTestRegistry(t)
	err := r.Deregister("nobody")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestTableDrivenRegister exercises multiple register scenarios in parallel subtests.
func TestTableDrivenRegister(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		entries []struct{ n, a string }
		wantLen int
	}{
		{
			name:    "single entry",
			entries: []struct{ n, a string }{{"x", "1.2.3.4:80"}},
			wantLen: 1,
		},
		{
			name: "three entries",
			entries: []struct{ n, a string }{
				{"a", "addr1"},
				{"b", "addr2"},
				{"c", "addr3"},
			},
			wantLen: 3,
		},
		{
			name:    "zero entries",
			entries: nil,
			wantLen: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newTestRegistry(t) // fresh instance per subtest
			for _, e := range tt.entries {
				if err := r.Register(e.n, e.a); err != nil {
					t.Fatalf("Register(%q): %v", e.n, err)
				}
			}
			if got := r.Len(); got != tt.wantLen {
				t.Fatalf("Len() = %d, want %d", got, tt.wantLen)
			}
		})
	}
}

// TestConcurrentRegistrations verifies concurrent Register + Lookup is race-free.
func TestConcurrentRegistrations(t *testing.T) {
	t.Parallel()

	r := newTestRegistry(t)
	const n = 50
	var wg sync.WaitGroup

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("svc-%d", i)
			_ = r.Register(name, fmt.Sprintf("10.0.0.%d:80", i))
			_, _ = r.Lookup(name)
		}(i)
	}
	wg.Wait()

	if r.Len() != n {
		t.Fatalf("Len() = %d, want %d", r.Len(), n)
	}
}

// ExampleRegistry_Register shows basic registry usage.
func ExampleRegistry_Register() {
	r := New()
	_ = r.Register("api", "127.0.0.1:8080")
	e, _ := r.Lookup("api")
	_ = e.Addr
	// Output:
}
```

Your turn: add `TestDeregisterThenReregister` that registers a name, deregisters it, then registers it again with a different address and asserts `Lookup` returns the new address. Use `t.Parallel()` and `newTestRegistry`.

### Exercise 3: Runnable Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/registry"
)

func main() {
	r := registry.New()

	services := []struct{ name, addr string }{
		{"frontend", "10.0.1.1:80"},
		{"backend", "10.0.1.2:8080"},
		{"cache", "10.0.1.3:6379"},
	}

	for _, svc := range services {
		if err := r.Register(svc.name, svc.addr); err != nil {
			fmt.Printf("register %s: %v\n", svc.name, err)
		}
	}
	fmt.Printf("registered: %d services\n", r.Len())

	e, err := r.Lookup("backend")
	if err != nil {
		fmt.Printf("lookup: %v\n", err)
	} else {
		fmt.Printf("backend addr: %s\n", e.Addr)
	}

	if err := r.Deregister("cache"); err != nil {
		fmt.Printf("deregister: %v\n", err)
	}
	fmt.Printf("after deregister: %d services\n", r.Len())

	_, err = r.Lookup("cache")
	if errors.Is(err, registry.ErrNotFound) {
		fmt.Println("cache: not found (deregistered)")
	}
}
```

## Common Mistakes

### Using Package-Level Variables as Test Fixtures

Wrong:

```go
var globalRegistry = New()

func TestA(t *testing.T) {
	t.Parallel()
	globalRegistry.Register("a", "addr") // modifies shared state
}

func TestB(t *testing.T) {
	t.Parallel()
	if globalRegistry.Len() != 0 { // may see TestA's registration
		t.Fatal("expected empty registry")
	}
}
```

What happens: tests interfere with each other non-deterministically under `t.Parallel()`. The race detector flags the data race on `globalRegistry`.

Fix: each test calls `newTestRegistry(t)` to get an independent instance. No shared mutable state between tests.

### Using `t.Parallel()` Without Capturing the Table Entry

Wrong (pre-Go 1.22 pattern that is still a conceptual trap):

```go
for _, tt := range tests {
	t.Run(tt.name, func(t *testing.T) {
		t.Parallel()
		doWork(tt.input) // Go 1.21 and earlier: tt is shared across iterations
	})
}
```

What happens: in Go 1.21 and earlier, all subtests see the last value of `tt` because the loop variable is reused. In Go 1.22+ loop variables are scoped per iteration, so this is safe without `tt := tt`.

Fix (Go 1.22+): no action needed. For clarity in tutorials targeting mixed Go versions, some teams still write `tt := tt` before `t.Parallel()` as defensive documentation.

### Not Using `t.Cleanup` for Teardown

Wrong:

```go
func TestServer(t *testing.T) {
	t.Parallel()
	s := startServer(t)
	defer s.Close() // fires when the test returns — but if t.Fatal fires, defer still runs
	// ... test body ...
}
```

This is actually correct — `defer` runs even when `t.Fatal` is called. However, `t.Cleanup` is preferred in helpers because it allows the helper to register teardown without the caller needing to manage it:

```go
func newTestServer(t *testing.T) *Server {
	t.Helper()
	s := startServer(t)
	t.Cleanup(func() { s.Close() })
	return s // caller does not need to close
}
```

Fix: in test helpers, use `t.Cleanup` for teardown so the caller's test body is uncluttered.

## Verification

From `~/go-exercises/registry`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go test -count=10 -race -parallel 8 ./...
go run ./cmd/demo
```

The `-count=10 -parallel 8` run stresses the scheduler across ten repetitions with eight parallel tests. Any shared state causes a race detector report or flaky assertion failure. The demo must print the three registration, lookup, and deregister operations correctly.

## Summary

- Parallel tests must use fresh instances of every mutable resource; package-level globals shared across tests cause data races and flaky assertions.
- `t.TempDir()` provides unique, automatically cleaned file paths per test.
- `net.Listen("tcp", "127.0.0.1:0")` assigns a random port; never use a fixed port in parallel tests.
- `t.Cleanup` in test helpers registers teardown so the caller's test body stays clean.
- In Go 1.22+, loop variables are scoped per iteration; `tt := tt` is no longer required but may appear in older code.
- Verify with `-race -parallel 8 -count 10` to stress concurrent test scheduling.

## What's Next

Next: [Chaos Testing Concurrent Code](../08-chaos-testing-concurrent-code/08-chaos-testing-concurrent-code.md).

## Resources

- [testing.T.Parallel](https://pkg.go.dev/testing#T.Parallel)
- [testing.T.Cleanup](https://pkg.go.dev/testing#T.Cleanup)
- [testing.T.TempDir](https://pkg.go.dev/testing#T.TempDir)
- [Go 1.22 Release Notes: Loop variable scoping](https://go.dev/doc/go1.22#language)
