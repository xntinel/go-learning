# Exercise 7: Lazy One-Time Initialization Of A Shared Resource

Some dependencies are expensive to build and should be built once, lazily, only
if used — a parsed template set, a compiled regex table, a connection-pool
handle. `sync.Once` (and the modern `sync.OnceValue`/`sync.OnceValues`) give you
exactly-once initialization that is safe when many goroutines race to be the
first caller, with the primitive's zero value doing the coordination.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
lazyinit/                  independent module: example.com/lazyinit
  go.mod
  lazyinit.go              generic Lazy[T]; BuildResource (OnceValue); Config (OnceValues)
  cmd/
    demo/
      main.go              triggers init, shows one build and shared instance
  lazyinit_test.go         concurrent exactly-once test + OnceValue panic test
```

Files: `lazyinit.go`, `cmd/demo/main.go`, `lazyinit_test.go`.
Implement: a generic `Lazy[T]` whose `Get` runs the builder exactly once under `sync.Once`; a package-level `BuildResource` via `sync.OnceValue`; a `Config` via `sync.OnceValues` for the error-returning case.
Test: launch many goroutines calling the accessor simultaneously and assert the builder ran exactly once (atomic counter) and every caller observes the same instance, under `-race`; verify a panicking `OnceValue` builder re-panics with the same value.
Verify: `go test -count=1 -race ./...`

## Why sync.Once, and what OnceValue adds

`sync.Once` is usable at its zero value: a `Once` field needs no constructor, and
`once.Do(f)` runs `f` at most once across all goroutines and all calls, blocking
concurrent callers until the first `Do` returns. That is the whole contract you
need for lazy init: the first goroutine to reach `Get` runs the builder while any
others block, and after that every `Get` returns the already-built value with no
locking on the fast path. `Lazy[T]` wraps that pattern generically — store a
`build func() T`, run it once inside `Do`, cache the result in `val`.

The modern forms remove the boilerplate when the builder is fixed at package
scope. `sync.OnceValue(f)` returns a function that runs `f` once and returns its
(cached) value — `BuildResource` here is a one-liner that yields the same
`*Resource` on every call. `sync.OnceValues(f)` is the two-return version for a
builder that can fail: `Config` returns `(value, error)`, both cached, so a
successful one-time load is memoized and callers all see the same result. Reach
for these instead of hand-rolling a `Once` + field when there is no per-instance
builder to inject.

One behavior to know for robustness: if the `OnceValue`/`OnceValues` builder
*panics*, the returned function re-panics with the same value on every subsequent
call rather than retrying. That makes a failed one-time init fail *consistently*
— you never get a half-initialized resource that some callers see and others
don't. The test pins this down.

The contrast with a package `init()` is deliberate: `init()` runs at program
start whether or not the feature is ever used, so expensive unconditional
initialization there is wasted work in a process that never touches it. Lazy
one-time init pays the cost only on first use.

Create `lazyinit.go`:

```go
package lazyinit

import "sync"

// Resource is a stand-in for an expensive-to-build shared dependency.
type Resource struct {
	ID    int
	Ready bool
}

// Lazy builds a value of type T exactly once, on first Get, safe under
// concurrent first-callers. Construct it with NewLazy; do not copy after use
// (it embeds a sync.Once).
type Lazy[T any] struct {
	once  sync.Once
	build func() T
	val   T
}

// NewLazy returns a Lazy that will call build the first time Get is invoked.
func NewLazy[T any](build func() T) *Lazy[T] {
	return &Lazy[T]{build: build}
}

// Get returns the built value, running build exactly once across all callers.
func (l *Lazy[T]) Get() T {
	l.once.Do(func() { l.val = l.build() })
	return l.val
}

// BuildResource lazily builds a shared *Resource exactly once using the modern
// sync.OnceValue form. Every call returns the same instance.
var BuildResource = sync.OnceValue(func() *Resource {
	return &Resource{ID: 42, Ready: true}
})

// loadConfig is a one-time loader that can fail; sync.OnceValues caches both the
// value and the error.
var loadConfig = sync.OnceValues(func() (int, error) {
	return 7, nil
})

// Config returns the one-time-loaded config value (and any load error).
func Config() (int, error) {
	return loadConfig()
}
```

## The runnable demo

The demo builds a `Lazy[*Resource]` whose builder increments a local counter,
calls `Get` twice to show the builder ran once and both calls return the same
pointer, then exercises the `OnceValue` and `OnceValues` forms.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/lazyinit"
)

func main() {
	calls := 0
	lazy := lazyinit.NewLazy(func() *lazyinit.Resource {
		calls++
		return &lazyinit.Resource{ID: 1, Ready: true}
	})

	a := lazy.Get()
	b := lazy.Get()
	fmt.Printf("build calls=%d same=%v ready=%v\n", calls, a == b, a.Ready)

	r1 := lazyinit.BuildResource()
	r2 := lazyinit.BuildResource()
	fmt.Printf("oncevalue same=%v id=%d\n", r1 == r2, r1.ID)

	cfg, err := lazyinit.Config()
	fmt.Printf("config=%d err=%v\n", cfg, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
build calls=1 same=true ready=true
oncevalue same=true id=42
config=7 err=<nil>
```

## Tests

`TestBuildsExactlyOnce` launches 100 goroutines that all call `Get` at once and
asserts the builder ran exactly once (via an `atomic.Int64`) and every caller got
the identical `*Resource`. Under `-race` this proves `sync.Once` actually
serializes the first call. `TestOnceValuePanicPropagates` builds a `sync.OnceValue`
whose builder panics and asserts the second call re-panics with the same value,
not a retry.

Create `lazyinit_test.go`:

```go
package lazyinit

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func TestBuildsExactlyOnce(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	lazy := NewLazy(func() *Resource {
		calls.Add(1)
		return &Resource{ID: 1, Ready: true}
	})

	const n = 100
	got := make([]*Resource, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i] = lazy.Get()
		}()
	}
	wg.Wait()

	if calls.Load() != 1 {
		t.Fatalf("build calls = %d, want 1", calls.Load())
	}
	first := got[0]
	for i, r := range got {
		if r != first {
			t.Fatalf("caller %d observed a different instance", i)
		}
	}
}

func recoverValue(fn func()) (v any) {
	defer func() { v = recover() }()
	fn()
	return nil
}

func TestOnceValuePanicPropagates(t *testing.T) {
	t.Parallel()

	get := sync.OnceValue(func() int {
		panic("build failed")
	})

	first := recoverValue(func() { _ = get() })
	second := recoverValue(func() { _ = get() })

	if first != "build failed" || second != "build failed" {
		t.Fatalf("panic values = %v, %v; want both \"build failed\"", first, second)
	}
}

func ExampleLazy_Get() {
	lazy := NewLazy(func() int { return 42 })
	fmt.Println(lazy.Get(), lazy.Get())
	// Output: 42 42
}
```

## Review

The initialization is correct when the builder runs exactly once regardless of
how many goroutines race to the first `Get`, and every caller ends up with the
same instance. `TestBuildsExactlyOnce` under `-race` is the proof: a plain `if
!done { build(); done = true }` guard (without `Once`) would let two goroutines
both build, and either the race detector or the `calls == 1` assertion would
fire. Prefer `sync.OnceValue`/`sync.OnceValues` when the builder is fixed at
package scope — less code and the same guarantees, including consistent
re-panicking on a failed build. Do not copy a `Lazy` after use; it embeds a
`sync.Once`, which `go vet` `copylocks` protects. And resist doing this work in
`init()` when the resource may never be needed.

## Resources

- [`sync.Once`](https://pkg.go.dev/sync#Once) — `Do` runs its function at most once.
- [`sync.OnceValue`](https://pkg.go.dev/sync#OnceValue) — one-time lazy value, re-panics consistently on a failed build.
- [`sync.OnceValues`](https://pkg.go.dev/sync#OnceValues) — the two-return `(value, error)` form.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-nil-vs-empty-slice-json.md](06-nil-vs-empty-slice-json.md) | Next: [08-zero-value-default-options.md](08-zero-value-default-options.md)
