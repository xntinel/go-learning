# Exercise 35: Deduplicate Concurrent Identical Requests and Share the Single Result

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A cache-miss stampede is one of the most common ways a healthy service
tips into an outage: a hot key expires, and a hundred goroutines that were
all serving from cache a moment ago now all miss at once and all fan out
an identical, simultaneous request to the origin — a database, an
upstream API, a slow computation — multiplying the load on exactly the
dependency that's least equipped to absorb it. The "singleflight" pattern
(the same one behind `golang.org/x/sync/singleflight`, and used inside
many database drivers and DNS resolvers) fixes this at the root: when
several callers ask for the same key at the same time, only the first one
actually does the work, and the rest wait for and share its result. This
module implements that pattern from scratch, using only the standard
library. It is self-contained: its own `go mod init`, code, demo, and
test.

## What you'll build

```text
reqdedup/                     independent module: example.com/singleflight-request-deduplicator
  go.mod                        go 1.24
  reqdedup.go                     package reqdedup; Group; (Group) Do(key, fn) (val, err, shared)
  cmd/demo/main.go                runnable demo: four concurrent callers share one result, then a fresh call starts a new flight
  reqdedup_test.go                 concurrent dedup count, non-overlapping calls start separate flights, dedup applies to errors too
```

- Implement: `(g *Group) Do(key string, fn func() (string, error)) (val string, err error, shared bool)` — a tagless switch on whether a flight for `key` is already registered decides between waiting on the shared result and starting a brand-new one.
- Test: many goroutines racing for the same key confirm `fn` runs exactly once, two calls that don't overlap in time confirm each starts its own flight, and a failing `fn`'s error is shared by every waiter exactly like a successful result.
- Verify: `go test -race -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/reqdedup/cmd/demo
cd ~/go-exercises/reqdedup
go mod init example.com/singleflight-request-deduplicator
go mod edit -go=1.24
```

### The switch that decides "join" vs. "lead"

`Do`'s entire job is a single decision, made once per call, under the
group's mutex: is a call for this key already in flight?

```go
c, inFlight := g.calls[key]

switch {
case inFlight:
    g.mu.Unlock()
    c.wg.Wait()
    return c.val, c.err, true
default:
    c = new(call)
    c.wg.Add(1)
    g.calls[key] = c
    g.mu.Unlock()

    c.val, c.err = fn()
    c.wg.Done()

    g.mu.Lock()
    delete(g.calls, key)
    g.mu.Unlock()

    return c.val, c.err, false
}
```

The `inFlight` branch never calls `fn` at all — it releases the group's
lock immediately and blocks on `c.wg.Wait()`, a `sync.WaitGroup` that the
leader will signal when its own call to `fn` finishes. The `default`
branch is the leader's path: register the call *before* releasing the
lock (so the very next goroutine to check `g.calls[key]` sees it), release
the lock, do the actual work *without* holding the lock (so unrelated keys
are never blocked by this key's work), then re-acquire the lock only to
remove the now-finished call from the map. That removal is what makes a
later, non-overlapping call for the same key start an entirely new flight
instead of piggy-backing on a result that's already been delivered.

The three-way distinction the `shared` return value carries — this call
led the flight (`false`), this call joined an in-progress flight (`true`)
— is exactly the kind of outcome a caller building metrics around
(`dedup_rate`, cache-stampede protection effectiveness) needs, and it
falls directly out of which branch of the switch actually ran.

### Testing real concurrency without flakiness

Proving deduplication actually happened requires the followers to
genuinely observe the leader's call still in flight, which means `fn`
needs to be deliberately slow enough that several goroutines can reach
`Do` before it returns. The test below blocks `fn` on a channel and uses a
short `time.Sleep` purely as a scheduling barrier — giving every goroutine
a chance to reach `Do` (and see the flight already registered) before the
test releases `fn`. This is not the same category of sleep the lesson
about fixed clocks warns against: that lesson is about a demo's *business
logic* depending on wall-clock time, producing nondeterministic printed
output. This sleep decides nothing about the result — dedup either happens
correctly or the test's assertion on `fn`'s call count catches it — it only
gives goroutines a scheduling head start. It is, in fact, the identical
technique `golang.org/x/sync/singleflight`'s own test suite uses for
exactly this reason.

Create `reqdedup.go`:

```go
// Package reqdedup deduplicates concurrent identical requests: when many
// goroutines ask for the same key at once, only the first actually does
// the work, and the rest share its result. This is the "singleflight"
// pattern used inside database drivers, HTTP caches, and DNS resolvers to
// prevent a thundering herd -- without it, a cache miss for a hot key under
// load fans out into hundreds of identical, simultaneous origin requests
// instead of one.
package reqdedup

import "sync"

// call tracks one in-flight (or just-completed) invocation of fn for a
// given key. wg lets every follower block until the leader's fn call
// finishes, and val/err are only safe to read after wg.Wait() returns,
// because Done() happens-after they're assigned.
type call struct {
	wg  sync.WaitGroup
	val string
	err error
}

// Group deduplicates calls sharing the same key. The zero value is ready
// to use.
type Group struct {
	mu    sync.Mutex
	calls map[string]*call
}

// Do runs fn for key, unless a call for the same key is already in flight,
// in which case Do waits for that call and returns its result instead of
// invoking fn a second time. The returned shared reports which of the two
// happened: false means this call actually executed fn (it was the
// leader), true means it received another goroutine's in-flight result.
func (g *Group) Do(key string, fn func() (string, error)) (val string, err error, shared bool) {
	g.mu.Lock()
	if g.calls == nil {
		g.calls = make(map[string]*call)
	}
	c, inFlight := g.calls[key]

	switch {
	case inFlight:
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err, true
	default:
		c = new(call)
		c.wg.Add(1)
		g.calls[key] = c
		g.mu.Unlock()

		c.val, c.err = fn()
		c.wg.Done()

		g.mu.Lock()
		// Only the leader ever deletes its own call, and only after fn has
		// returned, so a new Do("key", ...) after this point always starts
		// a fresh flight rather than piggy-backing on a finished one.
		delete(g.calls, key)
		g.mu.Unlock()

		return c.val, c.err, false
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	reqdedup "example.com/singleflight-request-deduplicator"
)

func main() {
	var g reqdedup.Group
	var calls int32

	release := make(chan struct{})
	fn := func() (string, error) {
		atomic.AddInt32(&calls, 1)
		<-release // simulates the expensive work in progress
		return "expensive-result", nil
	}

	const n = 4
	shared := make([]bool, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, s := g.Do("resource-42", fn)
			shared[i] = s
		}(i)
	}

	// Give all four goroutines a chance to reach Do (and either register
	// the flight or find it already registered) before releasing fn. This
	// is a scheduling barrier, not a business-logic delay -- singleflight's
	// own upstream test suite uses the identical technique.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	sharedCount := 0
	for _, s := range shared {
		if s {
			sharedCount++
		}
	}
	fmt.Printf("fn executed %d time(s) for %d concurrent callers\n", atomic.LoadInt32(&calls), n)
	fmt.Printf("%d caller(s) shared the in-flight result, %d started the flight\n", sharedCount, n-sharedCount)

	// After the flight completes, the call entry is gone: the next request
	// for the same key starts a brand-new flight.
	_, _, s := g.Do("resource-42", func() (string, error) { return "fresh-result", nil })
	fmt.Println("new call after completion, shared:", s)
}
```

Run `go run ./cmd/demo`, expected output:

```
fn executed 1 time(s) for 4 concurrent callers
3 caller(s) shared the in-flight result, 1 started the flight
new call after completion, shared: false
```

### Tests

`TestDoDeduplicatesConcurrentCalls` fires eight goroutines at the same key
while `fn` is blocked, then releases it once and confirms `fn` ran exactly
once and every goroutine received the same result.
`TestDoStartsNewFlightAfterCompletion` proves two sequential,
non-overlapping calls each run `fn` independently rather than the second
one incorrectly reusing the first's already-deleted entry.
`TestDoSharesAnErrorToo` confirms an error result is deduplicated exactly
like a successful one — every waiter gets the same error, not a generic
"call failed" placeholder.

Create `reqdedup_test.go`:

```go
package reqdedup

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestDoDeduplicatesConcurrentCalls fires n goroutines at the same key
// while fn is deliberately blocked, then releases fn once and confirms it
// only ran once. The short sleep is a scheduling barrier that lets all n
// goroutines reach Do before fn is released -- the same technique
// golang.org/x/sync/singleflight's own test suite uses, since there is no
// portable, sleep-free way to guarantee n goroutines have all entered a
// mutex-protected section from the outside.
func TestDoDeduplicatesConcurrentCalls(t *testing.T) {
	t.Parallel()

	var g Group
	var calls int32
	release := make(chan struct{})
	fn := func() (string, error) {
		atomic.AddInt32(&calls, 1)
		<-release
		return "result", nil
	}

	const n = 8
	results := make([]string, n)
	sharedFlags := make([]bool, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, err := func() (string, error) {
				v, err, s := g.Do("key", fn)
				sharedFlags[i] = s
				return v, err
			}()
			if err != nil {
				t.Errorf("goroutine %d: unexpected error: %v", i, err)
			}
			results[i] = v
		}(i)
	}

	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("fn executed %d time(s), want exactly 1", got)
	}
	for i, v := range results {
		if v != "result" {
			t.Errorf("results[%d] = %q, want %q", i, v, "result")
		}
	}
	sharedCount := 0
	for _, s := range sharedFlags {
		if s {
			sharedCount++
		}
	}
	if sharedCount != n-1 {
		t.Fatalf("sharedCount = %d, want %d (exactly one caller starts the flight)", sharedCount, n-1)
	}
}

func TestDoStartsNewFlightAfterCompletion(t *testing.T) {
	t.Parallel()

	var g Group
	callCount := 0
	fn := func() (string, error) {
		callCount++
		return "result", nil
	}

	_, _, shared1 := g.Do("key", fn)
	_, _, shared2 := g.Do("key", fn)

	if shared1 {
		t.Error("first call: shared = true, want false")
	}
	if shared2 {
		t.Error("second call after the first completed: shared = true, want false (new flight)")
	}
	if callCount != 2 {
		t.Fatalf("fn executed %d time(s), want 2 (calls did not overlap)", callCount)
	}
}

func TestDoSharesAnErrorToo(t *testing.T) {
	t.Parallel()

	var g Group
	wantErr := errors.New("boom")
	release := make(chan struct{})
	var calls int32
	fn := func() (string, error) {
		atomic.AddInt32(&calls, 1)
		<-release
		return "", wantErr
	}

	const n = 3
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err, _ := g.Do("key", fn)
			errs[i] = err
		}(i)
	}

	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("fn executed %d time(s), want exactly 1", got)
	}
	for i, err := range errs {
		if !errors.Is(err, wantErr) {
			t.Errorf("errs[%d] = %v, want %v", i, err, wantErr)
		}
	}
}
```

Verify with:

```bash
go test -race -count=1 ./...
```

## Review

The deduplicator is correct when `fn` runs exactly once no matter how many
goroutines call `Do` with the same key while it's in flight, when a call
made after the previous flight has fully completed always starts a fresh
one instead of reusing a stale, already-deleted entry, and when an error
result is shared among waiters with the same fidelity as a successful one.
Carry this forward: a tagless switch is the right tool even for a decision
as small as "is there already a flight for this key" — spelling out the
`inFlight` case and its `default` counterpart as two switch cases, rather
than an `if`/`else`, keeps the leader's registration-then-unlock-then-work
sequence and the follower's unlock-then-wait sequence visually parallel
and easy to audit side by side.

## Resources

- [Go Specification: Switch statements](https://go.dev/ref/spec#Switch_statements) — the tagless (expressionless) switch form.
- [golang.org/x/sync/singleflight](https://pkg.go.dev/golang.org/x/sync/singleflight) — the reference implementation this exercise reconstructs from first principles.
- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup) — how followers block on the leader's in-flight call.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [34-dns-service-discovery-with-staleness.md](34-dns-service-discovery-with-staleness.md) | Next: [../04-type-switch/00-concepts.md](../04-type-switch/00-concepts.md)
