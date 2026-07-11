# Exercise 26: Singleflight Request Deduplication Races in Map Lookup

**Nivel: Intermedio** — validacion rapida (un test corto).

A cache-miss stampede is one of the most common ways a healthy backend
takes itself down: a thousand requests for the same hot key all miss
the cache at once and all hammer the same downstream at once. The
standard defense is request coalescing — often called "singleflight"
after the pattern's most widely deployed Go implementation — where
only the first caller for a given key actually performs the expensive
work, and every concurrent caller for that same key waits for and
shares its result. The entire correctness of that guarantee lives in
one detail: checking whether a call is already in flight and
registering a new one must happen as a single atomic step, or two
callers arriving close enough together will both check, both see
nothing, and both become "the first caller." This module is fully
self-contained: its own `go mod init`, all code inline, its own demo
and tests.

## What you'll build

```text
dedup/                      independent module: example.com/singleflight-request-dedup-map-race
  go.mod                     go 1.22
  dedup.go                    Group, NewGroup, Do
  cmd/
    demo/
      main.go                 runnable demo: 5 concurrent callers, one slow backend lookup
  dedup_test.go                50-goroutine coalescing race, plus a sequential re-execution case
```

- Files: `dedup.go`, `cmd/demo/main.go`, `dedup_test.go`.
- Implement: `Group.Do(key string, fn func() (string, error)) (string, error)` that runs `fn` at most once per key among concurrent callers, sharing the result with everyone else waiting on that key.
- Test: a 50-goroutine burst against the same key asserting `fn` executes exactly once; a sequential test asserting a later call for the same key executes `fn` again once the first has completed.
- Verify: `go test -count=1 -race ./...`.

```bash
mkdir -p ~/go-exercises/singleflight-request-dedup-map-race/cmd/demo
cd ~/go-exercises/singleflight-request-dedup-map-race
go mod init example.com/singleflight-request-dedup-map-race
go mod edit -go=1.22
```

### Why check-then-write has to be one critical section, not two

The version that looks reasonable on a first pass locks only long
enough to read the map, releases the lock, and locks again to write —
on the theory that shrinking each critical section reduces contention:

```go
// BUG: the read and the write are two separate critical sections, with a
// window between them where another goroutine can run the same sequence.
func (g *Group) Do(key string, fn func() (string, error)) (string, error) {
	g.mu.Lock()
	c, ok := g.calls[key]
	g.mu.Unlock() // released before the map is ever written

	if ok {
		c.wg.Wait()
		return c.val, c.err
	}

	c = new(call)
	c.wg.Add(1)

	g.mu.Lock()
	g.calls[key] = c // another goroutine may already have stored its own *call here
	g.mu.Unlock()

	c.val, c.err = fn()
	c.wg.Done()
	return c.val, c.err
}
```

Two goroutines calling `Do("product-42", fn)` at nearly the same
instant can both execute the read, both see `ok == false`, and both
proceed to build their own `*call`, both calling `fn` — the exact
stampede the whole abstraction exists to prevent. Worse, whichever
goroutine's `g.calls[key] = c` write happens *second* silently
overwrites the first goroutine's entry in the map, so any *third*
caller arriving during this window waits on the second goroutine's
`call` while the first goroutine's `fn` result is never delivered to
anyone but the first goroutine itself. None of this shows up in a
sequential test, and it does not show up in *every* concurrent run
either — it needs two calls to race into that specific window, which
is precisely why `-race` and a tight, artificially widened race window
(a `fn` slow enough that many goroutines are still inside `Do` at
once) are the right tools to make it reproducible.

The fix holds one lock across the entire check-and-register sequence,
so "does a call already exist" and "register this call as the one that
exists" happen atomically:

```go
g.mu.Lock()
if c, ok := g.calls[key]; ok {
	g.mu.Unlock()
	c.wg.Wait()
	return c.val, c.err
}
c := new(call)
c.wg.Add(1)
g.calls[key] = c
g.mu.Unlock()
```

Now there is no instant at which two goroutines can both observe an
empty slot for the same key: whichever goroutine acquires the mutex
first either finds nothing and claims the key before releasing the
lock, or finds the call the first goroutine already claimed.

Create `dedup.go`:

```go
package dedup

import "sync"

// call represents one in-flight or completed execution of Do for a given
// key. wg is released once the underlying fn returns, unblocking every
// caller that arrived while the call was in flight.
type call struct {
	wg  sync.WaitGroup
	val string
	err error
}

// Group deduplicates concurrent calls that share the same key: only the
// first caller for a key actually executes fn; every other concurrent
// caller for that same key blocks and receives the first caller's result
// instead of triggering a redundant, possibly expensive, downstream call.
type Group struct {
	mu    sync.Mutex
	calls map[string]*call
}

// NewGroup returns an empty Group.
func NewGroup() *Group {
	return &Group{calls: make(map[string]*call)}
}

// Do executes fn for key and returns its result. If another goroutine is
// already executing fn for the same key, Do waits for that call to finish
// and returns its result instead of calling fn again. The check for an
// existing in-flight call and the registration of a new one happen inside
// the same critical section, so two concurrent callers for the same key
// can never both conclude they are the first.
func (g *Group) Do(key string, fn func() (string, error)) (string, error) {
	g.mu.Lock()
	if c, ok := g.calls[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err
	}

	c := new(call)
	c.wg.Add(1)
	g.calls[key] = c
	g.mu.Unlock()

	c.val, c.err = fn()
	c.wg.Done()

	g.mu.Lock()
	delete(g.calls, key)
	g.mu.Unlock()

	return c.val, c.err
}
```

### The runnable demo

Five goroutines all request the same product's price at once against a
backend lookup that takes 20ms; only one of them should ever reach the
backend.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"example.com/singleflight-request-dedup-map-race"
)

func main() {
	g := dedup.NewGroup()

	var backendCalls int64
	fn := func() (string, error) {
		atomic.AddInt64(&backendCalls, 1)
		time.Sleep(20 * time.Millisecond) // simulates a slow downstream price lookup
		return "product-42-price-19.99", nil
	}

	const callers = 5
	var wg sync.WaitGroup
	results := make([]string, callers)
	wg.Add(callers)
	for i := range callers {
		go func() {
			defer wg.Done()
			val, _ := g.Do("product-42", fn)
			results[i] = val
		}()
	}
	wg.Wait()

	fmt.Println("backend calls made:", backendCalls)
	for i, r := range results {
		fmt.Printf("caller %d got: %s\n", i, r)
	}
}
```

```bash
go run ./cmd/demo
```

Expected output:

```text
backend calls made: 1
caller 0 got: product-42-price-19.99
caller 1 got: product-42-price-19.99
caller 2 got: product-42-price-19.99
caller 3 got: product-42-price-19.99
caller 4 got: product-42-price-19.99
```

### Tests

`TestDoDeduplicatesConcurrentCallers` is the core concurrency case: 50
goroutines launched from a closed start barrier all call `Do` with the
same key at once, against a `fn` slow enough (5ms) to keep most of them
inside `Do` simultaneously; the assertion is that `fn` ran exactly
once, not "usually once." `TestDoRunsAgainAfterCompletion` pins the
simpler non-racy half: after a call for a key finishes, a later call
for that same key executes `fn` again instead of replaying a stale
result forever.

Create `dedup_test.go`:

```go
package dedup

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestDoDeduplicatesConcurrentCallers is the concurrency case: many
// goroutines call Do with the same key at the same time, and fn is slow
// enough (5ms) that a correct implementation is guaranteed to coalesce
// them regardless of scheduling -- the guarantee comes from the mutex
// covering both the map check and the map write in one critical section,
// not from timing luck.
func TestDoDeduplicatesConcurrentCallers(t *testing.T) {
	g := NewGroup()

	var calls int64
	fn := func() (string, error) {
		atomic.AddInt64(&calls, 1)
		time.Sleep(5 * time.Millisecond)
		return "result", nil
	}

	const callers = 50
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]string, callers)
	wg.Add(callers)
	for i := range callers {
		go func() {
			defer wg.Done()
			<-start
			val, err := g.Do("shared-key", fn)
			if err != nil {
				t.Errorf("Do() error = %v, want nil", err)
			}
			results[i] = val
		}()
	}
	close(start)
	wg.Wait()

	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("fn executed %d times, want exactly 1 (concurrent callers were not deduplicated)", got)
	}
	for i, r := range results {
		if r != "result" {
			t.Fatalf("results[%d] = %q, want %q", i, r, "result")
		}
	}
}

// TestDoRunsAgainAfterCompletion pins the non-racy half: once a call for a
// key has finished, a later call for the same key executes fn again rather
// than reusing a stale cached result forever.
func TestDoRunsAgainAfterCompletion(t *testing.T) {
	g := NewGroup()
	var calls int64
	fn := func() (string, error) {
		n := atomic.AddInt64(&calls, 1)
		return fmt.Sprintf("call-%d", n), nil
	}

	first, _ := g.Do("k", fn)
	second, _ := g.Do("k", fn)

	if first == second {
		t.Fatalf("second call reused %q, want a fresh execution once the first completed", first)
	}
	if got := atomic.LoadInt64(&calls); got != 2 {
		t.Fatalf("fn executed %d times across two sequential calls, want 2", got)
	}
}
```

Run: `go test -count=1 -race ./...`.

## Review

`Do` is correct when, for any number of concurrent callers sharing a
key, `fn` executes exactly once and every caller receives that single
execution's result — proven under `-race` with a burst large enough
that scheduling variance would expose a check-then-write race if one
existed, not merely observed to "usually" work. The mistake this
design avoids is assuming that shrinking a critical section always
reduces risk: splitting "check for an existing call" and "register a
new call" into two separate `Lock`/`Unlock` pairs looks like it lowers
contention, but it reopens exactly the window the whole abstraction
was built to close, letting two callers both conclude they are the
first. The rule this exercise pins down generalizes past this one
`Group`: whenever a decision ("is there already one of these?") and an
action based on that decision ("if not, make one") must be indivisible
from a concurrent point of view, they belong inside the same lock
acquisition, never two.

## Resources

- [golang.org/x/sync/singleflight](https://pkg.go.dev/golang.org/x/sync/singleflight) — the reference implementation this pattern is named after.
- [sync.Mutex](https://pkg.go.dev/sync#Mutex) — critical section boundaries and why splitting one logical operation across two lock acquisitions reintroduces the race it was meant to prevent.
- [Go Race Detector](https://go.dev/doc/articles/race_detector) — running a coalescing test under `-race`, and why a wide race window (a deliberately slow `fn`) makes the bug reproducible instead of merely possible.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [25-sliding-window-rate-limiter.md](25-sliding-window-rate-limiter.md) | Next: [27-token-bucket-refill-concurrent-race.md](27-token-bucket-refill-concurrent-race.md)
