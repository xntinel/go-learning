# Exercise 33: Resource Factory with Construction/Destruction Lifecycle Callbacks

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye concurrencia).

A connection pool, a temp-file manager, and a lease broker all share one
shape: something is built, tracked while live, and eventually torn down —
possibly many of them at once during shutdown. This module factors that
shape into a `Factory` parameterized entirely by a `Constructor` and a
`Destructor` callback, so the factory itself never knows what a
`Resource` actually is, only that every one it hands out gets exactly one
matching teardown call.

## What you'll build

```text
factory/                      independent module: example.com/resource-factory-lifecycle-callback
  go.mod                       go 1.24
  factory.go                   type Constructor, type Destructor, type Factory: Acquire, Release, LiveCount, CloseAll
  cmd/
    demo/
      main.go                    runnable demo: acquire two, release one, double-release, CloseAll the rest
  factory_test.go                acquire tracks, release untracks+destructs, untracked/double release errors, CloseAll joins errors, concurrency (-race)
```

Files: `factory.go`, `cmd/demo/main.go`, `factory_test.go`.
Implement: `type Constructor func(ctx, id string) (*Resource, error)`, `type Destructor func(ctx, *Resource) error`, `Factory` with `New(construct, destruct)`, `Acquire(ctx) (*Resource, error)`, `Release(ctx, *Resource) error`, `LiveCount() int`, and `CloseAll(ctx) error`; `Release` on an untracked or already-released resource returns `ErrNotTracked` without calling the destructor, and `CloseAll` destructs every remaining resource in a deterministic (ID-sorted) order, joining every destructor error with `errors.Join` instead of stopping at the first.
Test: `Acquire` calls the constructor and tracks the result; `Release` calls the destructor and untracks; releasing an untracked resource errors without invoking the destructor; releasing the same resource twice errors on the second call and destructs only once; `CloseAll` destructs every remaining resource and joins a failing destructor's error with the others' success, still leaving `LiveCount` at zero; concurrent `Acquire`/`Release` from many goroutines keep `LiveCount` consistent under `-race`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/resource-factory-lifecycle-callback/cmd/demo
cd ~/go-exercises/resource-factory-lifecycle-callback
go mod init example.com/resource-factory-lifecycle-callback
go mod edit -go=1.24
```

### Why `CloseAll` sorts before it destructs

`Factory` tracks live resources in a `map[string]*Resource`, and Go
deliberately randomizes map iteration order between runs — a fact this
package cannot fight and should not try to. That randomization is
invisible for `Acquire`/`Release`, which only ever look up one key at a
time, but it becomes visible the moment something ranges over the whole
map, which is exactly what `CloseAll` has to do to destruct everything
still live. Two runs of the same shutdown sequence must produce the same
observable order (this is what makes the demo's expected output
reproducible, and it also matters for real destructors — closing
resources in a stable order is often part of the contract, e.g. reverse
dependency order). So `CloseAll` copies the map's values into a slice
under the lock, releases the lock immediately (an unbounded number of
destructors is not something you want to run while holding the mutex
other goroutines' `Acquire`/`Release` need), then sorts that slice by
`ID` before calling every `Destructor` in that fixed order. Every
resource is untracked and every destructor is attempted — `errors.Join`
collects failures without a failing destructor preventing the others from
running, which matters most exactly during shutdown, when you want to
release as much as possible even if one release goes wrong.

Create `factory.go`:

```go
// Package factory builds a resource pool whose construction and
// destruction are entirely delegated to callbacks, so the pool itself
// only tracks which resources are currently live and coordinates cleanup,
// never knowing what a Resource actually is.
package factory

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Resource is an opaque handle identified by ID. In a real system this
// might wrap a DB connection, a file handle, or a leased lock.
type Resource struct {
	ID string
}

// Constructor builds a new Resource, e.g. opening a connection.
type Constructor func(ctx context.Context, id string) (*Resource, error)

// Destructor releases a Resource previously built by a Constructor.
type Destructor func(ctx context.Context, r *Resource) error

// ErrNotTracked is returned by Release for a Resource the Factory never
// acquired (or already released).
var ErrNotTracked = errors.New("resource not tracked by this factory")

// Factory hands out and reclaims Resources, delegating the actual work to
// a Constructor and Destructor supplied at construction time.
type Factory struct {
	mu        sync.Mutex
	construct Constructor
	destruct  Destructor
	live      map[string]*Resource
	nextID    int
}

// New returns a Factory that uses construct/destruct for every resource's
// lifecycle.
func New(construct Constructor, destruct Destructor) *Factory {
	return &Factory{
		construct: construct,
		destruct:  destruct,
		live:      make(map[string]*Resource),
	}
}

// Acquire builds a new Resource via the Constructor and tracks it as live.
func (f *Factory) Acquire(ctx context.Context) (*Resource, error) {
	f.mu.Lock()
	f.nextID++
	id := fmt.Sprintf("res-%d", f.nextID)
	f.mu.Unlock()

	r, err := f.construct(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("construct %s: %w", id, err)
	}

	f.mu.Lock()
	f.live[r.ID] = r
	f.mu.Unlock()
	return r, nil
}

// Release untracks r and runs the Destructor on it. Releasing a Resource
// the Factory does not currently track returns ErrNotTracked without
// calling the Destructor.
func (f *Factory) Release(ctx context.Context, r *Resource) error {
	f.mu.Lock()
	_, ok := f.live[r.ID]
	if ok {
		delete(f.live, r.ID)
	}
	f.mu.Unlock()

	if !ok {
		return fmt.Errorf("release %s: %w", r.ID, ErrNotTracked)
	}
	return f.destruct(ctx, r)
}

// LiveCount reports how many resources are currently tracked as live.
func (f *Factory) LiveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.live)
}

// CloseAll destructs every remaining live resource in a deterministic
// order (sorted by ID, since map iteration order is randomized),
// joining every Destructor error instead of stopping at the first one,
// and untracks each resource regardless of whether its Destructor
// succeeded.
func (f *Factory) CloseAll(ctx context.Context) error {
	f.mu.Lock()
	remaining := make([]*Resource, 0, len(f.live))
	for _, r := range f.live {
		remaining = append(remaining, r)
	}
	f.live = make(map[string]*Resource)
	f.mu.Unlock()

	sort.Slice(remaining, func(i, j int) bool { return remaining[i].ID < remaining[j].ID })

	var errs []error
	for _, r := range remaining {
		if err := f.destruct(ctx, r); err != nil {
			errs = append(errs, fmt.Errorf("destruct %s: %w", r.ID, err))
		}
	}
	return errors.Join(errs...)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/resource-factory-lifecycle-callback"
)

func main() {
	construct := func(ctx context.Context, id string) (*factory.Resource, error) {
		fmt.Printf("construct: opening %s\n", id)
		return &factory.Resource{ID: id}, nil
	}
	destruct := func(ctx context.Context, r *factory.Resource) error {
		fmt.Printf("destruct: closing %s\n", r.ID)
		return nil
	}

	f := factory.New(construct, destruct)
	ctx := context.Background()

	r1, _ := f.Acquire(ctx)
	_, _ = f.Acquire(ctx)
	fmt.Println("live count:", f.LiveCount())

	_ = f.Release(ctx, r1)
	fmt.Println("live count after release:", f.LiveCount())

	err := f.Release(ctx, r1)
	fmt.Println("second release error:", err != nil)

	_, _ = f.Acquire(ctx)
	fmt.Println("live count before CloseAll:", f.LiveCount())
	_ = f.CloseAll(ctx)
	fmt.Println("live count after CloseAll:", f.LiveCount())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
construct: opening res-1
construct: opening res-2
live count: 2
destruct: closing res-1
live count after release: 1
second release error: true
construct: opening res-3
live count before CloseAll: 2
destruct: closing res-2
destruct: closing res-3
live count after CloseAll: 0
```

### Tests

Create `factory_test.go`:

```go
package factory

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestAcquireCallsConstructorAndTracksResource(t *testing.T) {
	t.Parallel()
	constructed := 0
	construct := func(ctx context.Context, id string) (*Resource, error) {
		constructed++
		return &Resource{ID: id}, nil
	}
	f := New(construct, func(ctx context.Context, r *Resource) error { return nil })

	r, err := f.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if constructed != 1 {
		t.Fatalf("constructed = %d, want 1", constructed)
	}
	if f.LiveCount() != 1 {
		t.Fatalf("LiveCount() = %d, want 1", f.LiveCount())
	}
	if r.ID == "" {
		t.Fatal("resource has empty ID")
	}
}

func TestReleaseCallsDestructorAndUntracks(t *testing.T) {
	t.Parallel()
	destructed := 0
	f := New(
		func(ctx context.Context, id string) (*Resource, error) { return &Resource{ID: id}, nil },
		func(ctx context.Context, r *Resource) error { destructed++; return nil },
	)

	r, _ := f.Acquire(context.Background())
	if err := f.Release(context.Background(), r); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if destructed != 1 {
		t.Fatalf("destructed = %d, want 1", destructed)
	}
	if f.LiveCount() != 0 {
		t.Fatalf("LiveCount() = %d, want 0", f.LiveCount())
	}
}

func TestReleasingUntrackedResourceErrors(t *testing.T) {
	t.Parallel()
	destructed := 0
	f := New(
		func(ctx context.Context, id string) (*Resource, error) { return &Resource{ID: id}, nil },
		func(ctx context.Context, r *Resource) error { destructed++; return nil },
	)

	err := f.Release(context.Background(), &Resource{ID: "never-acquired"})
	if !errors.Is(err, ErrNotTracked) {
		t.Fatalf("err = %v, want ErrNotTracked", err)
	}
	if destructed != 0 {
		t.Fatal("destructor ran for an untracked resource")
	}
}

func TestReleasingTwiceErrorsTheSecondTime(t *testing.T) {
	t.Parallel()
	destructed := 0
	f := New(
		func(ctx context.Context, id string) (*Resource, error) { return &Resource{ID: id}, nil },
		func(ctx context.Context, r *Resource) error { destructed++; return nil },
	)

	r, _ := f.Acquire(context.Background())
	if err := f.Release(context.Background(), r); err != nil {
		t.Fatalf("first Release: %v", err)
	}
	err := f.Release(context.Background(), r)
	if !errors.Is(err, ErrNotTracked) {
		t.Fatalf("second Release err = %v, want ErrNotTracked", err)
	}
	if destructed != 1 {
		t.Fatalf("destructed = %d, want 1 (only the first Release)", destructed)
	}
}

func TestCloseAllDestructsAllAndJoinsFailingDestructorErrors(t *testing.T) {
	t.Parallel()
	failOn := "res-2"
	f := New(
		func(ctx context.Context, id string) (*Resource, error) { return &Resource{ID: id}, nil },
		func(ctx context.Context, r *Resource) error {
			if r.ID == failOn {
				return fmt.Errorf("cannot close %s", r.ID)
			}
			return nil
		},
	)

	// The factory assigns IDs "res-1", "res-2", "res-3" in acquisition
	// order, so the second Acquire deterministically produces failOn.
	for range 3 {
		if _, err := f.Acquire(context.Background()); err != nil {
			t.Fatalf("Acquire: %v", err)
		}
	}

	err := f.CloseAll(context.Background())
	if err == nil {
		t.Fatal("expected CloseAll to report the failing destructor's error")
	}
	if f.LiveCount() != 0 {
		t.Fatalf("LiveCount() = %d, want 0 (CloseAll untracks regardless of destructor outcome)", f.LiveCount())
	}
}

func TestConcurrentAcquireReleaseIsRaceFree(t *testing.T) {
	t.Parallel()
	f := New(
		func(ctx context.Context, id string) (*Resource, error) { return &Resource{ID: id}, nil },
		func(ctx context.Context, r *Resource) error { return nil },
	)

	var wg sync.WaitGroup
	results := make(chan *Resource, 50)
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := f.Acquire(context.Background())
			if err != nil {
				return
			}
			results <- r
		}()
	}
	wg.Wait()
	close(results)

	if f.LiveCount() != 50 {
		t.Fatalf("LiveCount() = %d, want 50", f.LiveCount())
	}

	var releaseWG sync.WaitGroup
	for r := range results {
		releaseWG.Add(1)
		go func(r *Resource) {
			defer releaseWG.Done()
			_ = f.Release(context.Background(), r)
		}(r)
	}
	releaseWG.Wait()

	if f.LiveCount() != 0 {
		t.Fatalf("LiveCount() = %d, want 0", f.LiveCount())
	}
}
```

## Review

`Factory` is correct when every `Acquire` has exactly one matching
`Destructor` call, whether that call comes from an explicit `Release` or
from `CloseAll` during shutdown. `TestReleasingTwiceErrorsTheSecondTime`
is the test that pins down the "exactly one" half — a naive
implementation that checks "is this resource live" without atomically
removing it under the same lock could let two concurrent `Release` calls
both see the resource as tracked and both destruct it.
`TestCloseAllDestructsAllAndJoinsFailingDestructorErrors` pins down the
other important property for a shutdown path specifically: one
destructor failing must never stop the rest from running, and the
factory must still consider everything released afterward — a resource
whose destructor errored is not a resource you get to leak forever, it is
one whose failure you get to observe and act on. The concurrency test
does not assert anything about *which* IDs end up live at any instant
(that would be racy by construction); it only asserts the invariants that
must hold at the quiescent points — every one of 50 concurrent `Acquire`
calls succeeds and is tracked, and every one of 50 concurrent `Release`
calls afterward leaves the factory empty, both under `-race`.

## Resources

- [Go Specification: Function types](https://go.dev/ref/spec#Function_types)
- [errors.Join](https://pkg.go.dev/errors#Join)
- [sync.Pool (a standard-library resource pool for comparison)](https://pkg.go.dev/sync#Pool)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [32-permission-evaluator-callback-chain.md](32-permission-evaluator-callback-chain.md) | Next: [34-schema-migration-before-after-hook.md](34-schema-migration-before-after-hook.md)
