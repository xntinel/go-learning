# Exercise 9: Per-Key Memoization: Closures Capturing a Shared Cache

A memoize helper wraps an expensive `func(K) (V, error)` into a cached version,
capturing a map and a mutex, with single-flight-style dedup so concurrent callers
for the same key compute it once. This is intended capture-by-reference — the
cache IS the captured state — and the loop trap returns when you build per-key
loaders in a loop: each loader must bind its own key while sharing one cache
safely. You build it generic, prove single computation per key, and keep it
`-race` clean.

## What you'll build

```text
memoize/                     independent module: example.com/memoize
  go.mod                     go 1.26
  memoize.go                 generic Memoize over a shared map+mutex; PerKeyLoaders
  cmd/
    demo/
      main.go                runnable demo: memoize a counting loader, show hit==1
  memoize_test.go            one-compute-per-key, per-key loader binding, -race concurrent
```

- Files: `memoize.go`, `cmd/demo/main.go`, `memoize_test.go`.
- Implement: generic `Memoize[K comparable, V any](fn) func(K) (V, error)` capturing a map and mutex with single-flight dedup; `PerKeyLoaders(keys, fn)` building one bound loader per key over one shared memoizer.
- Test: repeated calls for one key invoke the underlying func once; distinct keys each compute once; per-key loaders each return their own key's value; concurrent access to the shared cache is `-race` clean and still computes each key once.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Shared cache as captured state, one loader per key

`Memoize` returns a closure over a `map[K]*entry` and a `sync.Mutex`. The map is
the shared state the closure captures on purpose — every call to the returned
function reads and writes the same map, which is what makes it a cache. The
single-flight structure matters under concurrency: a naive memoizer that holds
the lock only to check-then-release, then computes, then re-locks to store, would
let two goroutines racing on the same missing key BOTH compute it. The fix is to
store a per-key `*entry` with its own `sync.Once`-like `ready` channel: the first
caller inserts a placeholder entry under the lock and computes; later callers find
the placeholder, release the lock, and block on the entry's `ready` channel until
the first caller fills it. So the expensive `fn` runs exactly once per key even
under a storm of concurrent callers.

`PerKeyLoaders` is the loop-capture angle. It builds a map of `key -> func() (V,
error)`, one loader per key, all sharing ONE memoizer. Each loader must bind its
own key so that calling `loaders["a"]()` loads `"a"`, not the last key in the
loop. On a `go 1.26` module the per-iteration `range` variable binds each key
correctly; the test pins that each loader returns its own value, so a regression
to a shared key would collapse every loader onto one key and fail.

The generic signature is `Memoize[K comparable, V any]`. `K comparable` is
required because it is a map key; `V any` is the cached value. Errors are cached
too here (the entry stores both value and error), which is a deliberate policy
choice: a failed load will not be retried until evicted. Real memoizers often
choose NOT to cache errors; this one does, and the test asserts it, so the policy
is explicit.

Create `memoize.go`:

```go
package memoize

import "sync"

type entry[V any] struct {
	ready chan struct{}
	value V
	err   error
}

// Memoize wraps fn so that fn runs at most once per key, even under concurrent
// callers. The returned closure captures a shared map guarded by a mutex.
func Memoize[K comparable, V any](fn func(K) (V, error)) func(K) (V, error) {
	var mu sync.Mutex
	cache := make(map[K]*entry[V])

	return func(key K) (V, error) {
		mu.Lock()
		e, ok := cache[key]
		if ok {
			mu.Unlock()
			<-e.ready // another caller is computing (or has); wait for it
			return e.value, e.err
		}
		e = &entry[V]{ready: make(chan struct{})}
		cache[key] = e
		mu.Unlock()

		e.value, e.err = fn(key) // exactly one caller reaches here per key
		close(e.ready)
		return e.value, e.err
	}
}

// PerKeyLoaders builds one loader per key over a single shared memoizer. Each
// loader binds its own key.
func PerKeyLoaders[K comparable, V any](keys []K, fn func(K) (V, error)) map[K]func() (V, error) {
	load := Memoize(fn)
	loaders := make(map[K]func() (V, error), len(keys))
	for _, key := range keys {
		loaders[key] = func() (V, error) {
			return load(key)
		}
	}
	return loaders
}
```

### The runnable demo

The demo memoizes a loader that counts how many times it actually computed, calls
the same key three times, and prints the value plus the compute count to show it
ran once.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"

	"example.com/memoize"
)

func main() {
	computes := 0
	load := memoize.Memoize(func(key string) (string, error) {
		computes++
		return strings.ToUpper(key), nil
	})

	for range 3 {
		v, _ := load("hello")
		fmt.Println("value:", v)
	}
	fmt.Println("computes:", computes)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
value: HELLO
value: HELLO
value: HELLO
computes: 1
```

### Tests

`TestComputesOncePerKey` calls one key repeatedly and distinct keys once each,
asserting the underlying counter matches the number of distinct keys.
`TestPerKeyLoadersBindOwnKey` builds loaders in a loop and asserts each returns
its own key's value. `TestConcurrentSameKeyComputesOnce` fires many goroutines at
one missing key and asserts single-flight dedup held.

Create `memoize_test.go`:

```go
package memoize

import (
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

func TestComputesOncePerKey(t *testing.T) {
	t.Parallel()

	var computes atomic.Int64
	load := Memoize(func(key string) (string, error) {
		computes.Add(1)
		return "v:" + key, nil
	})

	for range 5 {
		if v, _ := load("a"); v != "v:a" {
			t.Fatalf("load(a) = %q, want v:a", v)
		}
	}
	if v, _ := load("b"); v != "v:b" {
		t.Fatalf("load(b) = %q, want v:b", v)
	}
	if got := computes.Load(); got != 2 {
		t.Fatalf("computes = %d, want 2 (once per distinct key)", got)
	}
}

func TestPerKeyLoadersBindOwnKey(t *testing.T) {
	t.Parallel()

	keys := []int{1, 2, 3, 4, 5}
	loaders := PerKeyLoaders(keys, func(k int) (string, error) {
		return "k" + strconv.Itoa(k), nil
	})

	for _, k := range keys {
		v, err := loaders[k]()
		if err != nil {
			t.Fatalf("loader %d: %v", k, err)
		}
		want := "k" + strconv.Itoa(k)
		if v != want {
			t.Fatalf("loader[%d] = %q, want %q (loaders collapsed onto one key)", k, v, want)
		}
	}
}

func TestConcurrentSameKeyComputesOnce(t *testing.T) {
	t.Parallel()

	var computes atomic.Int64
	load := Memoize(func(key string) (int, error) {
		computes.Add(1)
		return len(key), nil
	})

	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if v, _ := load("same"); v != 4 {
				t.Errorf("load(same) = %d, want 4", v)
			}
		}()
	}
	wg.Wait()

	if got := computes.Load(); got != 1 {
		t.Fatalf("computes = %d under concurrency, want 1 (single-flight failed)", got)
	}
}

func ExampleMemoize() {
	n := 0
	load := Memoize(func(key string) (int, error) {
		n++
		return len(key), nil
	})
	load("abc")
	load("abc")
	fmt.Println(load("abc"))
	fmt.Println("computes:", n)
	// Output:
	// 3 <nil>
	// computes: 1
}
```

## Review

The memoizer is correct when `fn` runs exactly once per distinct key — including
under a concurrent storm on one missing key — and each per-key loader returns its
own value. `TestConcurrentSameKeyComputesOnce` is what proves the single-flight
design: the placeholder-entry-plus-`ready`-channel is why 100 racing callers
compute once, where a check-release-compute-store memoizer would compute many
times. `TestPerKeyLoadersBindOwnKey` is the loop-capture guard: each loader binds
its own key over the shared cache. The captured map is the intended shared state,
and the mutex plus the ready channel are what make sharing it across goroutines
safe. Run `go test -race`; the shared map under the mutex must be clean.

## Resources

- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — guarding the shared cache map.
- [The Go Programming Language, memoizing package](https://www.gopl.io/) — the placeholder-plus-ready-channel single-flight pattern (ch. 9).
- [`golang.org/x/sync/singleflight`](https://pkg.go.dev/golang.org/x/sync/singleflight) — the production package for the same dedup guarantee.

---

Back to [00-concepts.md](00-concepts.md) | Next: [10-reused-decode-buffer-pointer-capture.md](10-reused-decode-buffer-pointer-capture.md)
