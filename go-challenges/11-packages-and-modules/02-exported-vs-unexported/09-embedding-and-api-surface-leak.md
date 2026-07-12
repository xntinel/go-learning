# Exercise 9: Accidental API Surface: What Embedding Promotes

Embedding is often used just to save typing, and it quietly expands your public API:
embedding an exported type promotes its exported methods into your own method set, so
callers can call the embedded type's methods on your value. This exercise builds two
counters, one that leaks its mutex's `Lock`/`Unlock` through embedding and one that
keeps them closed with a named unexported field, and proves the difference with a test
and with `go doc`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
counter/                   independent module: example.com/counter
  go.mod                   go 1.26
  counter.go               LeakyCounter (embeds sync.Mutex) and Counter (named mu field)
  cmd/
    demo/
      main.go              runs both counters concurrently, prints totals
  counter_test.go          package counter_test: proves Lock is promoted on Leaky, closed on Counter
```

- Files: `counter.go`, `cmd/demo/main.go`, `counter_test.go`.
- Implement: `LeakyCounter` embedding `sync.Mutex` (which promotes `Lock`/`Unlock` into its public API) and `Counter` holding the mutex in a named unexported field `mu` (which does not); both expose `Inc` and `Count`.
- Test: assert the intended methods work; assert `LeakyCounter` really promotes `Lock`/`Unlock` by calling them from the test package; document that `Counter` does not, via `go doc` and a compile-failure snippet.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/11-packages-and-modules/02-exported-vs-unexported/09-embedding-and-api-surface-leak/cmd/demo
cd go-solutions/11-packages-and-modules/02-exported-vs-unexported/09-embedding-and-api-surface-leak
go mod edit -go=1.26
```

### How method promotion leaks surface

When you embed a type anonymously, `struct { sync.Mutex }`, Go promotes the embedded
type's exported methods into the embedding type's method set. So `*LeakyCounter` gains
`Lock()` and `Unlock()` as its own methods, and any package that imports `counter` can
call `lc.Lock()`. Those two methods are now part of `LeakyCounter`'s public contract:
under semantic-import-versioning you cannot remove them without a major version bump, and
worse, a caller can lock your internal mutex out from under you and deadlock the counter.
This is a real, permanent expansion of your API, triggered by a one-line convenience.

The fix is to hold the dependency in a named, unexported field: `mu sync.Mutex`. Named
fields are not promoted, so `Counter` has exactly the method set you defined, `Inc` and
`Count`, and `mu` is unreachable from outside the package. The public surface is closed.
You can confirm this directly with `go doc`, which prints a type's exported method set:
`LeakyCounter` will list `Lock` and `Unlock` alongside `Inc` and `Count`, while `Counter`
lists only `Inc` and `Count`.

Embedding is not always wrong, sometimes you deliberately want to promote an interface's
methods (embedding `io.Reader` in a wrapper that should itself be a reader). The rule is
intent: embed when you mean the promoted methods to be part of your contract; use a named
unexported field when the dependency is an implementation detail. A mutex is almost always
an implementation detail.

Create `counter.go`:

```go
package counter

import "sync"

// LeakyCounter embeds sync.Mutex anonymously, which PROMOTES Lock and Unlock into
// LeakyCounter's own public method set. Callers can now call lc.Lock() and stall
// the counter. This is the accidental surface leak.
type LeakyCounter struct {
	sync.Mutex
	n int
}

func (c *LeakyCounter) Inc() {
	c.Lock()
	defer c.Unlock()
	c.n++
}

func (c *LeakyCounter) Count() int {
	c.Lock()
	defer c.Unlock()
	return c.n
}

// Counter holds the mutex in a NAMED unexported field, so Lock/Unlock are NOT
// promoted and the public surface is exactly Inc and Count.
type Counter struct {
	mu sync.Mutex
	n  int
}

func (c *Counter) Inc() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
}

func (c *Counter) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}
```

### The runnable demo

The demo drives both counters from many goroutines to show they behave identically, the
leak is a surface problem, not a correctness one, so the fix costs nothing at runtime.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/counter"
)

func main() {
	var leaky counter.LeakyCounter
	var safe counter.Counter

	var wg sync.WaitGroup
	for range 1000 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			leaky.Inc()
			safe.Inc()
		}()
	}
	wg.Wait()

	fmt.Printf("leaky: %d\n", leaky.Count())
	fmt.Printf("safe: %d\n", safe.Count())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
leaky: 1000
safe: 1000
```

### Tests

The black-box test asserts both counters count correctly under `-race`, then proves the
surface difference directly: it calls `Lock`/`Unlock` on a `LeakyCounter` from the test
package, which compiles only because those methods were promoted into `LeakyCounter`'s
public API. The commented block records that the same call on `Counter` does not compile,
and points at `go doc` as the tool to audit the leak.

Create `counter_test.go`:

```go
package counter_test

import (
	"sync"
	"testing"

	"example.com/counter"
)

func TestBothCountConcurrently(t *testing.T) {
	t.Parallel()

	var leaky counter.LeakyCounter
	var safe counter.Counter

	var wg sync.WaitGroup
	for range 500 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			leaky.Inc()
			safe.Inc()
		}()
	}
	wg.Wait()

	if got := leaky.Count(); got != 500 {
		t.Fatalf("leaky = %d, want 500", got)
	}
	if got := safe.Count(); got != 500 {
		t.Fatalf("safe = %d, want 500", got)
	}
}

// TestLeakyPromotesLock proves that embedding leaked Lock/Unlock into the public
// API: this code compiles from an EXTERNAL package only because those methods are
// part of LeakyCounter's method set.
func TestLeakyPromotesLock(t *testing.T) {
	t.Parallel()

	var lc counter.LeakyCounter
	lc.Lock()   // promoted from the embedded sync.Mutex; part of the public API
	lc.Unlock() // likewise
	lc.Inc()
	if lc.Count() != 1 {
		t.Fatalf("Count = %d, want 1", lc.Count())
	}
}

// Counter does NOT promote Lock/Unlock, so the following does not compile from an
// external package:
//
//	var c counter.Counter
//	c.Lock()   // c.Lock undefined (type counter.Counter has no field or method Lock)
//
// Audit the difference with `go doc`:
//
//	go doc example.com/counter LeakyCounter  -> lists Count, Inc, Lock, Unlock
//	go doc example.com/counter Counter       -> lists Count, Inc only
```

Confirm the surface difference with `go doc`:

```bash
go doc example.com/counter LeakyCounter
go doc example.com/counter Counter
```

`LeakyCounter`'s printed method set includes `Lock` and `Unlock` (promoted from the
embedded `sync.Mutex`); `Counter`'s includes only `Inc` and `Count`. That is the leak,
made visible.

## Review

Both counters are correct, they count exactly under `-race`, so the difference is entirely
in the public surface: `LeakyCounter` promoted `Lock`/`Unlock` into its API, which the
external test proves by calling them, while `Counter` kept the mutex in a named unexported
field and exposes only `Inc` and `Count`, which `go doc` and the compile-failure snippet
confirm. The lesson is that embedding is an API decision, not a typing shortcut: embed
`sync.Mutex` (or an `*http.Client`, or any exported type) and you sign up to keep every
one of its promoted exported methods in your contract, and you hand callers a way to
manipulate your internals. Reach for a named unexported field whenever the dependency is
an implementation detail, which a lock almost always is, and audit any embedding with
`go doc` before release to catch a surface leak while it is still cheap to fix.

## Resources

- [Go Spec: Struct types and embedded fields](https://go.dev/ref/spec#Struct_types) — how anonymous fields are embedded.
- [Go Spec: Selectors and method promotion](https://go.dev/ref/spec#Selectors) — the promotion rules that expand the method set.
- [Effective Go: Embedding](https://go.dev/doc/effective_go#embedding) — when embedding is the right tool and when it is not.
- [`go doc` command](https://pkg.go.dev/cmd/go#hdr-Show_documentation_for_package_or_symbol) — the tool that reveals the promoted method set.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-constructor-enforced-invariant.md](08-constructor-enforced-invariant.md) | Next: [../03-internal-packages/00-concepts.md](../03-internal-packages/00-concepts.md)
