# Exercise 3: Sliding Expiration

A session or token cache usually wants *sliding* expiration: each access pushes
the deadline out, so an actively-used entry never expires while idle ones do. The
test that proves this is a *multi-step* virtual-time walk — set, wait, touch, wait,
check, wait, check — and that is the technique this exercise adds: asserting an
entry survives past its *original* TTL but not past the *extended* one, exact to
the millisecond.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
sliding/                   independent module: example.com/sliding
  go.mod                   go 1.25 (synctest needs it)
  sliding.go               type Cache[K,V]; New, Set, Get, Touch
  cmd/
    demo/
      main.go              runnable demo: Touch keeps an entry alive past its TTL
  sliding_test.go          synctest: multi-step virtual time (set, wait, touch, wait, check)
```

- Files: `sliding.go`, `cmd/demo/main.go`, `sliding_test.go`.
- Implement: a `Cache[K,V]` with `New`, `Set`, `Get`, and `Touch(key, ttl) bool` that resets a live entry's deadline to `ttl` from now and reports whether it acted.
- Test: a single bubble that walks virtual time through four stages and asserts the entry outlives its original TTL after a `Touch` but expires after the extended one; an `Example` pinning `Touch`'s return value.
- Verify: `go test -count=1 -race ./...`

Set up the module (`testing/synctest` requires Go 1.25+):

```bash
mkdir -p go-solutions/48-modern-go-language-and-stdlib/02-testing-synctest-deterministic-concurrency/03-sliding-expiration/cmd/demo
cd go-solutions/48-modern-go-language-and-stdlib/02-testing-synctest-deterministic-concurrency/03-sliding-expiration
go mod edit -go=1.25
```

### Touch, and why the multi-step assertion needs virtual time

Sliding expiration means an entry's deadline is not fixed at `Set` time but moves
forward on each access. `Touch` is that move: it looks the entry up, and *only if
it is still present and unexpired* resets `expires` to `time.Now().Add(ttl)` and
returns `true`. An absent or already-expired key cannot be revived, so `Touch`
returns `false` — you never resurrect a dead session. The guard is the same
`!time.Now().Before(e.expires)` test `Get` uses, so a key that lapsed one instant
ago is treated as gone by both.

What makes this a distinct synctest lesson is the *multi-step* timing assertion.
The test advances virtual time in several stages and checks the entry at each
boundary: it sets a one-second TTL, sleeps to t=0.8 s and touches (extending the
deadline to t=1.8 s), sleeps to t=1.6 s and asserts the entry survived *past its
original* one-second deadline, then sleeps to t=2.0 s and asserts it has now
expired *past the extended* deadline — and that a `Touch` on the now-dead key
returns `false`. With real sleeps that test would take two wall-clock seconds and
still be approximate: scheduler slack could place a `time.Sleep(800ms)` at 803 ms
and blur the t=1.8 s boundary. Inside a bubble the clock advances by *exactly* the
slept amount and nothing else, so `time.Sleep(800ms)` lands at precisely t=0.8 s
and every boundary is unambiguous. That exactness is what makes a sliding-deadline
test practical at all.

Create `sliding.go`:

```go
package sliding

import (
	"sync"
	"time"
)

type entry[V any] struct {
	value   V
	expires time.Time
}

// Cache is a TTL map whose entries can have their lifetime extended on access
// (sliding expiration). It reads the wall clock through time.Now, which a
// synctest bubble virtualizes.
type Cache[K comparable, V any] struct {
	mu    sync.Mutex
	items map[K]entry[V]
}

func New[K comparable, V any]() *Cache[K, V] {
	return &Cache[K, V]{items: make(map[K]entry[V])}
}

// Set stores value under key, expiring it ttl from now.
func (c *Cache[K, V]) Set(key K, value V, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = entry[V]{value: value, expires: time.Now().Add(ttl)}
}

// Get returns the value if present and unexpired.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok || !time.Now().Before(e.expires) {
		var zero V
		return zero, false
	}
	return e.value, true
}

// Touch extends a live entry's lifetime to ttl from now (sliding expiration).
// It returns false if the key is absent or already expired.
func (c *Cache[K, V]) Touch(key K, ttl time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok || !time.Now().Before(e.expires) {
		return false
	}
	e.expires = time.Now().Add(ttl)
	c.items[key] = e
	return true
}
```

### The runnable demo

The demo runs against the real clock so you can watch the slide hold an entry
alive: it stores a 40 ms session, sleeps 30 ms, touches it (resetting to another
40 ms), sleeps 30 ms more — 60 ms total, past the original deadline — and confirms
the entry is still there because the touch slid it forward.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/sliding"
)

func main() {
	c := sliding.New[string, string]()
	c.Set("session", "alice", 40*time.Millisecond)

	time.Sleep(30 * time.Millisecond)
	c.Touch("session", 40*time.Millisecond) // extend before it expires

	time.Sleep(30 * time.Millisecond) // 60ms elapsed, but the slide keeps it alive
	if _, ok := c.Get("session"); ok {
		fmt.Println("still alive after sliding the TTL")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
still alive after sliding the TTL
```

### Tests

`TestSlidingExpiration` is the whole point: a single bubble that walks four virtual
stages and asserts at each boundary. The boundaries are chosen to straddle both
deadlines — t=1.6 s is past the original 1 s but before the extended 1.8 s (entry
must live), t=2.0 s is past 1.8 s (entry must die) — so the test would fail if
`Touch` did nothing, if it failed to extend by the full `ttl`, or if it revived an
expired key. The `Example` documents `Touch`'s contract: `true` on a live key,
`false` on a missing one.

Create `sliding_test.go`:

```go
package sliding

import (
	"fmt"
	"testing"
	"testing/synctest"
	"time"
)

func TestSlidingExpiration(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		c := New[string, int]()
		c.Set("k", 1, time.Second) // expires at t=1s

		time.Sleep(800 * time.Millisecond) // t=0.8s
		if !c.Touch("k", time.Second) {    // now expires at t=1.8s
			t.Fatal("Touch should succeed before expiry")
		}

		time.Sleep(800 * time.Millisecond) // t=1.6s, still < 1.8s
		if _, ok := c.Get("k"); !ok {
			t.Fatal("entry should survive past the original TTL after Touch")
		}

		time.Sleep(400 * time.Millisecond) // t=2.0s, > 1.8s
		if _, ok := c.Get("k"); ok {
			t.Fatal("entry should expire after the extended TTL")
		}
		if c.Touch("k", time.Second) {
			t.Fatal("Touch on an expired key should return false")
		}
	})
}

func Example() {
	c := New[string, string]()
	c.Set("session", "alice", time.Minute)

	fmt.Println(c.Touch("session", time.Minute))
	fmt.Println(c.Touch("missing", time.Minute))
	// Output:
	// true
	// false
}
```

## Review

The cache is correct when `Touch` extends only live entries and the four-stage
walk lands exactly on its boundaries. The common bug is making `Touch` unconditional
— resetting the deadline without first checking the entry is unexpired — which
would let a caller resurrect a dead session and would flip the t=2.0 s assertion.
The other subtlety is value semantics: `entry[V]` is a struct stored by value in
the map, so mutating the local `e` is not enough; `Touch` must write `e` back with
`c.items[key] = e` or the slide is lost. The test's exact boundaries (t=1.6 s
alive, t=2.0 s dead) only mean something because virtual time has no slack, so a
flake here points at code consulting a clock the bubble does not control. Run
`go test -race` to confirm the mutex guards the read-modify-write in `Touch`.

## Resources

- [`testing/synctest`](https://pkg.go.dev/testing/synctest) — the bubble whose exact clock makes the multi-stage boundary assertions possible.
- [Testing concurrent code with testing/synctest](https://go.dev/blog/synctest) — the Go blog walkthrough of virtual-time tests.
- [`time.Time.Before`](https://pkg.go.dev/time#Time.Before) — the comparison both `Get` and `Touch` use to decide liveness.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-deadline-bounded-load.md](04-deadline-bounded-load.md)
