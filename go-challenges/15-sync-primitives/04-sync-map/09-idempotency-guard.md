# Exercise 9: Idempotency guard ensuring once-per-key execution

A payment or webhook handler must process each idempotency key at most once, even
when the client retries and two identical requests arrive concurrently. The guard
that enforces this is singleflight in miniature: the first caller for a key runs
the work, concurrent duplicates block until it finishes and receive the *same*
result, and no key is ever executed twice. This module builds that guard on
`sync.Map` plus a `done` channel, and proves under `-race` that concurrent
duplicates trigger exactly one execution.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
idemguard/                    independent module: example.com/idemguard
  go.mod                      go 1.26
  guard.go                    type Guard[T]; Do, Forget
  cmd/
    demo/
      main.go                 runnable demo: duplicate calls share one execution
  guard_test.go               once-per-key-under-concurrency, distinct-keys, Forget re-executes, Example
```

- Files: `guard.go`, `cmd/demo/main.go`, `guard_test.go`.
- Implement: `Guard[T]` with `Do(key string, fn func() (T, error)) (T, error)` and `Forget(key string)`.
- Test: K goroutines call `Do(sameKey, fn)` where `fn` increments an atomic counter; the counter is exactly 1 and every caller gets the identical result; distinct keys execute independently; `Forget` lets a later `Do` re-execute.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir idemguard && cd idemguard
go mod init example.com/idemguard
```

### Why a *call with a done channel

The heart of the pattern is a per-key `*call` holding a `done` channel and the
result fields. `Do` does `LoadOrStore(key, &call[T]{done: make(chan struct{})})`.
Exactly one caller sees `loaded == false` — it is the owner. The owner runs `fn`,
writes the result into the `call`, and `close(done)`. Every other caller sees
`loaded == true`, blocks on `<-c.done`, and once the channel is closed reads the
result the owner already wrote. Closing a channel is a broadcast: it unblocks all
waiters at once and establishes a happens-before edge from the owner's writes to
each waiter's reads, so reading `c.val`/`c.err` after `<-c.done` is race-free
without any additional lock.

Two `sync.Map` guarantees make this correct. `LoadOrStore` atomically elects a
single owner — no two callers can both think they are the owner, because the
check-and-store is one step. And once the `call` is stored, it stays under the key,
so any later duplicate (even one that arrives after the owner finished) loads the
completed `call` and gets the cached result immediately from the already-closed
`done`. That is the idempotency guarantee: process-once, then serve the same answer
to every retry.

`Forget(key)` deletes the `call` so the next `Do` for that key becomes a fresh
owner and re-executes — the escape hatch for when a result should expire or a
failed operation should be retried. Without `Forget` the result is cached forever,
which is the right default for a truly idempotent operation but wrong if you want
retry-on-failure; the honest design choice is to `Forget` a key whose `fn`
returned an error so a retry can run.

One cost to state plainly: `make(chan struct{})` in the `LoadOrStore` argument
allocates a channel on every call, even a cache hit that throws it away. The
channel is cheap, and the alternative (a lazily-created channel) complicates the
election; for a guard this trade is acceptable. If you needed to eliminate the hit-
path allocation you would switch to the mutex-guarded map that the standard
`golang.org/x/sync/singleflight` uses.

Create `guard.go`:

```go
package idemguard

import "sync"

type call[T any] struct {
	done chan struct{}
	val  T
	err  error
}

// Guard runs a function at most once per key. Concurrent callers for the same
// key share a single execution and receive the same result: the idempotency-key
// pattern for payment/webhook handlers (singleflight-lite).
type Guard[T any] struct {
	calls sync.Map // map[string]*call[T]
}

// Do runs fn for key at most once. The first caller executes fn; concurrent and
// later callers for the same key block until it finishes and receive the same
// (value, error). Call Forget to allow a later re-execution.
func (g *Guard[T]) Do(key string, fn func() (T, error)) (T, error) {
	actual, loaded := g.calls.LoadOrStore(key, &call[T]{done: make(chan struct{})})
	c := actual.(*call[T])
	if !loaded {
		// We are the owner: run fn, publish the result, wake the waiters.
		c.val, c.err = fn()
		close(c.done)
	} else {
		<-c.done
	}
	return c.val, c.err
}

// Forget drops the cached result for key so the next Do re-executes fn.
func (g *Guard[T]) Forget(key string) {
	g.calls.Delete(key)
}
```

### The runnable demo

The demo fires several concurrent `Do` calls for the same key against an `fn` that
counts its own executions, then prints the shared result and the execution count
(1), showing the duplicates collapsed into one run.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"

	"example.com/idemguard"
)

func main() {
	var g idemguard.Guard[string]
	var execs atomic.Int64

	fn := func() (string, error) {
		execs.Add(1)
		return "charge-ok", nil
	}

	var wg sync.WaitGroup
	results := make([]string, 5)
	for i := range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, _ := g.Do("pay-42", fn)
			results[i] = r
		}()
	}
	wg.Wait()

	fmt.Println("result:", results[0])
	fmt.Println("executions:", execs.Load())

	// After Forget, the key runs again.
	g.Forget("pay-42")
	_, _ = g.Do("pay-42", fn)
	fmt.Println("executions after Forget+Do:", execs.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
result: charge-ok
executions: 1
executions after Forget+Do: 2
```

### Tests

`TestOncePerKeyUnderConcurrency` is the contract: K goroutines call `Do` for the
same key with an `fn` that increments an atomic exec counter and blocks briefly to
widen the race; afterward the counter must be exactly 1 and every caller's result
identical. `TestDistinctKeysExecuteIndependently` confirms different keys each run
once. `TestForgetReExecutes` pins that `Forget` allows a fresh execution.

Create `guard_test.go`:

```go
package idemguard

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestOncePerKeyUnderConcurrency(t *testing.T) {
	t.Parallel()

	var g Guard[int]
	var execs atomic.Int64
	fn := func() (int, error) {
		execs.Add(1)
		time.Sleep(time.Millisecond) // widen the race window
		return 7, nil
	}

	const goroutines = 100
	results := make([]int, goroutines)
	var wg sync.WaitGroup
	for i := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := g.Do("k", fn)
			if err != nil {
				t.Errorf("Do returned error: %v", err)
			}
			results[i] = v
		}()
	}
	wg.Wait()

	if n := execs.Load(); n != 1 {
		t.Fatalf("fn executed %d times, want exactly 1", n)
	}
	for i, v := range results {
		if v != 7 {
			t.Fatalf("caller %d got %d, want the shared result 7", i, v)
		}
	}
}

func TestDistinctKeysExecuteIndependently(t *testing.T) {
	t.Parallel()

	var g Guard[string]
	var execs atomic.Int64
	fn := func() (string, error) {
		execs.Add(1)
		return "done", nil
	}

	for _, k := range []string{"a", "b", "c"} {
		if _, err := g.Do(k, fn); err != nil {
			t.Fatalf("Do(%q) error: %v", k, err)
		}
	}
	// A repeat of an existing key does not re-execute.
	if _, err := g.Do("a", fn); err != nil {
		t.Fatalf("repeat Do(a) error: %v", err)
	}
	if n := execs.Load(); n != 3 {
		t.Fatalf("fn executed %d times for 3 distinct keys (plus a repeat), want 3", n)
	}
}

func TestForgetReExecutes(t *testing.T) {
	t.Parallel()

	var g Guard[int]
	var execs atomic.Int64
	fn := func() (int, error) {
		return int(execs.Add(1)), nil
	}

	if v, _ := g.Do("k", fn); v != 1 {
		t.Fatalf("first Do = %d, want 1", v)
	}
	if v, _ := g.Do("k", fn); v != 1 {
		t.Fatalf("cached Do = %d, want 1 (no re-execute)", v)
	}
	g.Forget("k")
	if v, _ := g.Do("k", fn); v != 2 {
		t.Fatalf("Do after Forget = %d, want 2 (re-executed)", v)
	}
}

func ExampleGuard() {
	var g Guard[string]
	calls := 0
	fn := func() (string, error) {
		calls++
		return "value", nil
	}
	a, _ := g.Do("k", fn)
	b, _ := g.Do("k", fn) // cached, no second call
	fmt.Println(a, b, calls)
	// Output: value value 1
}
```

## Review

The guard is correct when concurrent duplicates for one key trigger exactly one
execution and all callers receive the identical result. The mechanism is
`LoadOrStore` electing a single owner atomically, plus a `done` channel whose close
broadcasts the completed result to every waiter with a happens-before edge — so the
waiters read the owner's writes safely without a lock.
`TestOncePerKeyUnderConcurrency` under `-race` is the proof: exec counter exactly 1,
all results equal, no data race. The traps to avoid are running `fn` before
electing the owner (it would execute on every call — the value is that `fn` runs
only on the `!loaded` branch), and caching a failed result forever (a real handler
`Forget`s a key whose `fn` errored so a retry can run). Run `go test -race` to
confirm the concurrent election and result broadcast are clean.

## Resources

- [sync.Map.LoadOrStore](https://pkg.go.dev/sync#Map.LoadOrStore) — the atomic owner election.
- [golang.org/x/sync/singleflight](https://pkg.go.dev/golang.org/x/sync/singleflight) — the production version of this pattern.
- [The Go Memory Model: channels](https://go.dev/ref/mem#chan) — a channel close happens-before a receive that observes it.

---

Back to [08-credential-hot-swap.md](08-credential-hot-swap.md) | Next: [10-benchmark-decision.md](10-benchmark-decision.md)
