# Exercise 3: A Concurrency-Safe Registry for a Running Server

In a real host the registry is read from many request goroutines while an admin
path occasionally mutates it. This module guards the plugin map with a
`sync.RWMutex` and returns a defensive snapshot from `List()`, then proves both
under `go test -race`.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports any other
exercise.

## What you'll build

```text
saferegistry/             independent module: example.com/saferegistry
  go.mod                  go 1.25
  registry.go             Registry guarded by sync.RWMutex; Register/Deregister/Run/List
  cmd/
    demo/
      main.go             concurrent Run while an admin goroutine registers/deregisters
  registry_test.go        -race stress test + deterministic snapshot test
```

- Files: `registry.go`, `cmd/demo/main.go`, `registry_test.go`.
- Implement: a `Registry` with a `sync.RWMutex` — `RLock` for `Run`/lookup, `Lock` for `Register`/`Deregister` — and a `List()` that returns a sorted snapshot independent of later mutation.
- Test: N goroutines running plugins and M goroutines registering/deregistering under `-race` with no data race and correct final invariants; a deterministic test that `List()` returns a sorted copy that a later mutation does not change.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/saferegistry/cmd/demo
cd ~/go-exercises/saferegistry
go mod init example.com/saferegistry
go mod edit -go=1.25
```

### Reader-writer, and the snapshot rule

A registry has a lopsided access pattern: `Run` and lookup are hot and read-only,
`Register`/`Deregister` are rare and mutating. A `sync.RWMutex` fits exactly —
many readers hold `RLock` concurrently, and a writer takes the exclusive `Lock`.
Wrapping the map accesses in the right lock is the obvious half of the job.

The half that is easy to forget, and that the mutex alone does not solve, is
`List()`. If `List` returns the internal slice of names (or the map), the lock it
held is released the moment it returns, and the caller then iterates a slice the
next `Register` is appending to — a data race at the *caller's* site that no lock
inside `List` can prevent. The fix is to return a *copy* made while the lock is
held: build a fresh slice, sort it, and hand that back. The caller owns the
snapshot; a later mutation of the registry cannot touch it. `slices.Sorted(maps.Keys(...))`
produces exactly this — a new, sorted slice — in one call.

`-race` is the arbiter. A registry that "looks" correct but returns the live map
will pass ordinary tests and fail the race detector under concurrent load; that is
the test that matters.

Create `registry.go`:

```go
package saferegistry

import (
	"errors"
	"maps"
	"slices"
	"sync"
)

// ErrNotFound is returned by Run for an unknown plugin name.
var ErrNotFound = errors.New("plugin not found")

// Plugin is the minimal contract for this module.
type Plugin interface {
	Name() string
	Process(input string) (string, error)
}

// Registry is safe for concurrent use: Run/List take a read lock, and
// Register/Deregister take the write lock.
type Registry struct {
	mu      sync.RWMutex
	plugins map[string]Plugin
}

// NewRegistry returns an empty, ready-to-use registry.
func NewRegistry() *Registry {
	return &Registry{plugins: make(map[string]Plugin)}
}

// Register adds or replaces the plugin under its Name(). Write lock.
func (r *Registry) Register(p Plugin) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.plugins[p.Name()] = p
}

// Deregister removes the named plugin if present. Write lock.
func (r *Registry) Deregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.plugins, name)
}

// Run processes input through the named plugin. Read lock: many Runs proceed
// concurrently. Note the plugin reference is captured under the lock and the
// lock is released before Process runs, so a slow plugin does not block writers.
func (r *Registry) Run(name, input string) (string, error) {
	r.mu.RLock()
	p, ok := r.plugins[name]
	r.mu.RUnlock()
	if !ok {
		return "", ErrNotFound
	}
	return p.Process(input)
}

// List returns a sorted snapshot of the registered names. The returned slice is
// a copy: mutating the registry afterward does not change it, and the caller can
// iterate it without holding a lock.
func (r *Registry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Sorted(maps.Keys(r.plugins))
}
```

Note the `Run` deliberately releases the read lock *before* calling `Process`: it
only needs the lock to look the plugin up, and holding it across a slow `Process`
would block every writer for the duration of plugin work. Capture the reference
under the lock, then run unlocked.

### The runnable demo

The demo models the production shape directly: a pool of worker goroutines calling
`Run` while an admin goroutine registers and deregisters plugins, all concurrent.
It prints the final sorted plugin list. Because registration timing is
nondeterministic, the demo asserts only on the deterministic end state it sets up.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/saferegistry"
)

type echo struct{ name string }

func (e echo) Name() string                      { return e.name }
func (e echo) Process(in string) (string, error) { return e.name + ":" + in, nil }

func main() {
	r := saferegistry.NewRegistry()
	r.Register(echo{name: "alpha"})
	r.Register(echo{name: "beta"})

	var wg sync.WaitGroup
	// Readers: dispatch to plugins that may come and go.
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				_, _ = r.Run("alpha", "x")
			}
		}()
	}
	// Writer: churn a transient plugin.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 100 {
			r.Register(echo{name: "gamma"})
			r.Deregister("gamma")
		}
	}()
	wg.Wait()

	// Ensure the deterministic end state, then print it.
	r.Register(echo{name: "gamma"})
	fmt.Println(r.List())
}
```

Run it:

```bash
go run -race ./cmd/demo
```

Expected output:

```text
[alpha beta gamma]
```

### Tests

`TestConcurrentRunAndMutate` is the stress test: 50 reader goroutines hammering
`Run` while 4 writer goroutines register and deregister, all under `-race`. It
asserts the base plugins survive at the end (they are never removed) so the final
invariant is checkable. `TestListReturnsSortedSnapshot` is deterministic: it takes
a `List()`, mutates the registry, and asserts the earlier snapshot is unchanged
and sorted.

Create `registry_test.go`:

```go
package saferegistry

import (
	"fmt"
	"slices"
	"strconv"
	"sync"
	"testing"
)

type echo struct{ name string }

func (e echo) Name() string                      { return e.name }
func (e echo) Process(in string) (string, error) { return e.name + ":" + in, nil }

func TestConcurrentRunAndMutate(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(echo{name: "base-a"})
	r.Register(echo{name: "base-b"})

	var wg sync.WaitGroup
	// Readers.
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				_, _ = r.Run("base-a", "x")
				_ = r.List()
			}
		}()
	}
	// Writers churning transient plugins.
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := "tmp-" + strconv.Itoa(w)
			for range 200 {
				r.Register(echo{name: name})
				r.Deregister(name)
			}
		}()
	}
	wg.Wait()

	got := r.List()
	if !slices.Contains(got, "base-a") || !slices.Contains(got, "base-b") {
		t.Fatalf("base plugins missing after churn: %v", got)
	}
}

func TestListReturnsSortedSnapshot(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(echo{name: "gamma"})
	r.Register(echo{name: "alpha"})
	r.Register(echo{name: "beta"})

	snap := r.List()
	want := []string{"alpha", "beta", "gamma"}
	if !slices.Equal(snap, want) {
		t.Fatalf("List() = %v, want sorted %v", snap, want)
	}

	// Mutating the registry must not change the earlier snapshot.
	r.Deregister("alpha")
	r.Register(echo{name: "delta"})
	if !slices.Equal(snap, want) {
		t.Fatalf("snapshot mutated by later registry change: %v", snap)
	}
	if got := r.List(); !slices.Equal(got, []string{"beta", "delta", "gamma"}) {
		t.Fatalf("post-mutation List() = %v", got)
	}
}

func ExampleRegistry_List() {
	r := NewRegistry()
	r.Register(echo{name: "b"})
	r.Register(echo{name: "a"})
	fmt.Println(r.List())
	// Output: [a b]
}
```

## Review

The registry is correct when it survives `go test -race` under concurrent
`Run`/`List` and `Register`/`Deregister` with no reported race, and when a
`List()` snapshot is provably immutable in the face of later mutation. The two
bugs this guards against are distinct: forgetting the lock (a race inside the
registry) and leaking the live map from `List` (a race at the caller). The first
is caught by the stress test, the second by the snapshot test — you need both.
Note the deliberate choice to release the read lock in `Run` before calling
`Process`; holding a lock across arbitrary plugin work is how a single slow plugin
stalls every writer, so capture the reference and run unlocked.

## Resources

- [sync.RWMutex](https://pkg.go.dev/sync#RWMutex) — the reader-writer lock and its `RLock`/`Lock` semantics.
- [slices.Sorted](https://pkg.go.dev/slices#Sorted) and [maps.Keys](https://pkg.go.dev/maps#Keys) — building a sorted snapshot from a map in one call.
- [Go: Data Race Detector](https://go.dev/doc/articles/race_detector) — how `-race` finds the leaked-map bug that review misses.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-optional-capability-interfaces.md](02-optional-capability-interfaces.md) | Next: [04-constructor-factory-registration.md](04-constructor-factory-registration.md)
