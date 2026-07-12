# Exercise 3: Debugging 'method has pointer receiver': Method Sets and Satisfaction

`X does not implement Y (method M has pointer receiver)` is the most common
interface compile error in Go, and behind it is a semantic trap worse than the
build failure: storing a value copy of a stateful type silently breaks shared
state. This module builds a `RateLimiter` whose `Allow` method has a pointer
receiver, registers it in a middleware chain through a `Limiter` interface, and
proves both the compile-time rule and the runtime consequence of getting the
receiver wrong.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests.

## What you'll build

```text
limiter/                      independent module: example.com/limiter
  go.mod                      go 1.26
  limiter.go                  Limiter interface (Allow); *RateLimiter (sync.Mutex + counter); guard var _ Limiter
  cmd/
    demo/
      main.go                 runnable demo: drive Allow through the interface, watch shared counter
  limiter_test.go             pointer identity test; shared-state mutation; -race concurrent Allow
```

- Files: `limiter.go`, `cmd/demo/main.go`, `limiter_test.go`.
- Implement: a `Limiter` interface with `Allow() bool`, and a `*RateLimiter` (a fixed-window counter with a `sync.Mutex`) whose `Allow` has a pointer receiver.
- Test: a compile-time guard `var _ Limiter = (*RateLimiter)(nil)`; a test that drives many `Allow` calls through the interface value and asserts the shared counter advanced (proving pointer identity, not a copy); a `-race` concurrent test.
- Verify: `go test -count=1 -race ./...`

### Why the value form does not satisfy the interface

`RateLimiter` holds a `sync.Mutex` and a mutable counter. Its `Allow` method takes
a pointer receiver `func (r *RateLimiter) Allow() bool` for two independent
reasons. First, the method set rule: a pointer-receiver method is in the method
set of `*RateLimiter` only, not `RateLimiter`. So:

```go
var _ Limiter = RateLimiter{}   // does NOT compile: Allow has pointer receiver
var _ Limiter = &RateLimiter{}  // compiles
```

The commented `Limiter = RateLimiter{}` guard in the file below documents exactly
this — uncomment it and the module stops building with `RateLimiter does not
implement Limiter (method Allow has pointer receiver)`.

Second, and more dangerous because it compiles, is the copy problem. If `Allow`
had a value receiver, calling it through an interface would operate on a *copy* of
the `RateLimiter` — including a copy of the counter and the mutex. Each call would
see its own copy, the shared counter would never advance across calls, and
copying the mutex after use is itself a bug `go vet` reports. The rate limiter
would silently never limit anything. Registering the limiter in a middleware chain
means many requests must share one counter; that requires pointer identity, which
requires a pointer receiver and storing `*RateLimiter` in the interface.

The limiter itself is a fixed-window counter: it admits up to `limit` calls within
each `window` and rejects the rest, resetting the count when the window rolls over.
`time.Since(r.windowStart)` measures elapsed time; when it exceeds `window`, the
window resets. This is a real (if simple) rate-limiting strategy used in front of
APIs.

Create `limiter.go`:

```go
package limiter

import (
	"sync"
	"time"
)

// Limiter is the consumer-defined interface a middleware chain depends on.
type Limiter interface {
	Allow() bool
}

// RateLimiter is a fixed-window counter: it admits up to limit calls per window.
// It holds a mutex and a mutable counter, so its methods take a pointer receiver
// and it is used through a *RateLimiter — a value copy would not share state.
type RateLimiter struct {
	mu          sync.Mutex
	limit       int
	window      time.Duration
	count       int
	windowStart time.Time
}

// NewRateLimiter returns a *RateLimiter admitting limit calls per window.
func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	return &RateLimiter{
		limit:       limit,
		window:      window,
		windowStart: time.Now(),
	}
}

// Allow reports whether a call is admitted, advancing the shared counter. The
// pointer receiver is mandatory: every caller through the interface must mutate
// the same counter and lock the same mutex.
func (r *RateLimiter) Allow() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if time.Since(r.windowStart) >= r.window {
		r.count = 0
		r.windowStart = time.Now()
	}
	if r.count >= r.limit {
		return false
	}
	r.count++
	return true
}

// Count reports the calls admitted in the current window (for tests/demo).
func (r *RateLimiter) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count
}

// Compile-time guard: *RateLimiter satisfies Limiter.
var _ Limiter = (*RateLimiter)(nil)

// The value form does NOT satisfy Limiter because Allow has a pointer receiver.
// Uncommenting the next line fails the build with:
//   RateLimiter does not implement Limiter (method Allow has pointer receiver)
// var _ Limiter = RateLimiter{}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/limiter"
)

func main() {
	// Registered once, shared by every request in the chain: the interface must
	// hold a pointer so all calls advance the same counter.
	var lim limiter.Limiter = limiter.NewRateLimiter(3, time.Minute)

	for i := range 5 {
		fmt.Printf("request %d: allowed=%v\n", i+1, lim.Allow())
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
request 1: allowed=true
request 2: allowed=true
request 3: allowed=true
request 4: allowed=false
request 5: allowed=false
```

### Tests

`TestSharedStateThroughInterface` is the core proof: it stores one `*RateLimiter`
in a `Limiter` interface variable, calls `Allow` several times through the
interface, and asserts the concrete counter advanced. This only holds because the
interface carries a pointer — the calls share one `RateLimiter`. If `Allow` were a
value method, the assertion would fail because each call would have mutated a copy.
`TestGuardCompiles` documents the satisfaction. `TestConcurrentAllow` runs `Allow`
from many goroutines under `-race` to prove the mutex guards the counter.

Create `limiter_test.go`:

```go
package limiter

import (
	"sync"
	"testing"
	"time"
)

func TestSharedStateThroughInterface(t *testing.T) {
	t.Parallel()

	concrete := NewRateLimiter(10, time.Minute)
	var lim Limiter = concrete // one value, shared through the interface

	for range 4 {
		lim.Allow()
	}

	// The interface calls mutated the SAME RateLimiter, not copies.
	if got := concrete.Count(); got != 4 {
		t.Fatalf("count = %d after 4 Allow calls; want 4 (value copy would give 0)", got)
	}
}

func TestAllowRejectsOverLimit(t *testing.T) {
	t.Parallel()

	lim := NewRateLimiter(2, time.Minute)

	got := []bool{lim.Allow(), lim.Allow(), lim.Allow()}
	want := []bool{true, true, false}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Allow #%d = %v, want %v", i+1, got[i], want[i])
		}
	}
}

func TestConcurrentAllow(t *testing.T) {
	t.Parallel()

	lim := NewRateLimiter(1_000_000, time.Minute)
	var wg sync.WaitGroup
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lim.Allow()
		}()
	}
	wg.Wait()

	if got := lim.Count(); got != 200 {
		t.Fatalf("count = %d, want 200", got)
	}
}
```

## Review

The rule to carry away: a pointer-receiver method is in the method set of `*T`
only, so a stateful type is satisfied by `*T` and used through a pointer. The
compile error `method Allow has pointer receiver` is the friendly failure; the
dangerous one is the version that compiles because you used a value receiver on a
stateful type, then silently operates on copies so the shared counter never moves.
`TestSharedStateThroughInterface` is written specifically to catch that: it would
fail (`count = 0`) if the interface held a copy instead of a pointer. The other
frequent mistake is passing the limiter by value into the middleware constructor —
a `RateLimiter` argument instead of `*RateLimiter` — which copies the mutex and is
flagged by `go vet`. Run `go test -race` to confirm the counter is guarded.

## Resources

- [Go Specification: Method sets](https://go.dev/ref/spec#Method_sets) — which methods `T` vs `*T` carry.
- [Go FAQ: pointer vs value method receivers](https://go.dev/doc/faq#methods_on_values_or_pointers) — when to use each.
- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — the "must not be copied after first use" contract.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-compile-time-interface-guards.md](04-compile-time-interface-guards.md)
