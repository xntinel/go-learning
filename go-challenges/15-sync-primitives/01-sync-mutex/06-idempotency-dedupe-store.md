# Exercise 6: First-writer-wins idempotency guard

An idempotent `POST`/`PUT` handler, or an at-least-once message consumer, must
process each request key exactly once even when duplicates arrive concurrently.
This module builds the store behind that guarantee: `Do(key, fn)` runs `fn`
exactly once per key, and every concurrent or later caller with the same key
blocks until the first finishes and observes its identical result. It is the
canonical non-atomic check-then-act bug, fixed.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
idem/                        independent module: example.com/idem
  go.mod                     go 1.26
  store.go                   type Store; New, Do
  cmd/
    demo/
      main.go                runnable demo: same key twice, fn runs once
  store_test.go              exactly-once under contention, distinct keys, error, -race
```

- Files: `store.go`, `cmd/demo/main.go`, `store_test.go`.
- Implement: a `Store` (mutex + `map[string]*call`) with `Do(key, fn) (any, error)` that runs `fn` once per key and shares its result with all callers of that key.
- Test: M goroutines with the same key assert `fn` ran exactly once (atomic counter) and all callers got the identical result; distinct keys all execute; `fn`'s error is propagated to every caller.
- Verify: `go test -count=1 -race ./...`

```bash
mkdir -p ~/go-exercises/idem/cmd/demo
cd ~/go-exercises/idem
go mod init example.com/idem
```

### Why the check and the claim must be one critical section

The naive idempotency guard is two steps: "is this key already in the map? no —
run `fn` and store the result." Written as two separate lock/unlock pairs, that
is a data race in the logical sense: two goroutines both take the lock, both see
the key absent, both release, and both run `fn`. The request is processed twice —
a double charge, a duplicate order, exactly the failure idempotency exists to
prevent. The fix is that the *claim* — deciding to be the one who runs `fn` and
recording that decision — must happen under a single continuous hold of the lock.

But `fn` itself can be slow (it hits a database, calls a payment API), and you
must not run it under the lock, or every other key's request blocks behind it and
the store becomes a global bottleneck. So the design separates the fast claim
from the slow execution. Under the lock, a caller checks the map for the key. If
a `call` record already exists, it releases the lock and waits on that record's
`done` channel, then reads the shared result. If not, it creates a `call`,
stores it, and releases the lock — that store is the atomic claim — then runs
`fn` *outside* the lock, records the result on the `call`, and closes `done` to
release every waiter. Reading the result after receiving from a closed channel is
safe: the channel close establishes a happens-before edge from the writer to
every reader.

This is the shape of `singleflight`, specialized to keep results forever rather
than only for the duration of one in-flight call. The mutex protects only the
map and the claim; `fn` runs concurrently across distinct keys with no
contention.

Create `store.go`:

```go
package idem

import "sync"

// call holds the shared outcome of running fn for one key. done is closed when
// the result is ready, releasing every waiter with a happens-before edge.
type call struct {
	done   chan struct{}
	result any
	err    error
}

// Store runs a function at most once per key and shares its result with all
// concurrent and later callers of that key.
type Store struct {
	mu    sync.Mutex
	calls map[string]*call
}

// New returns an empty Store.
func New() *Store {
	return &Store{calls: make(map[string]*call)}
}

// Do runs fn exactly once for key. Concurrent and later callers with the same
// key block until the first call finishes and receive its identical result. fn
// runs outside the lock, so distinct keys never contend.
func (s *Store) Do(key string, fn func() (any, error)) (any, error) {
	s.mu.Lock()
	if c, ok := s.calls[key]; ok {
		s.mu.Unlock()
		<-c.done
		return c.result, c.err
	}
	c := &call{done: make(chan struct{})}
	s.calls[key] = c // the atomic claim: this goroutine will run fn
	s.mu.Unlock()

	c.result, c.err = fn()
	close(c.done)
	return c.result, c.err
}
```

### The runnable demo

The demo calls `Do` with the same key twice and a distinct key once, printing how
many times the underlying function actually ran.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync/atomic"

	"example.com/idem"
)

func main() {
	s := idem.New()
	var runs atomic.Int64

	charge := func() (any, error) {
		runs.Add(1)
		return "charged", nil
	}

	r1, _ := s.Do("order-42", charge)
	r2, _ := s.Do("order-42", charge) // duplicate: fn does not run again
	r3, _ := s.Do("order-99", charge)

	fmt.Printf("order-42 first:  %v\n", r1)
	fmt.Printf("order-42 second: %v\n", r2)
	fmt.Printf("order-99:        %v\n", r3)
	fmt.Printf("fn runs:         %d\n", runs.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
order-42 first:  charged
order-42 second: charged
order-99:        charged
fn runs:         2
```

### Tests

`TestExactlyOnceUnderContention` fires M goroutines at the same key, counts real
executions with an `atomic.Int64`, and asserts it is exactly 1 while every caller
received the identical result — the check-then-act atomicity a naive
`contains()+insert` would break. `TestDistinctKeysAllRun` proves separate keys
each execute. `TestErrorIsShared` proves a failed `fn`'s error reaches every
caller via `errors.Is`.

Create `store_test.go`:

```go
package idem

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

var errCharge = errors.New("charge failed")

func TestExactlyOnceUnderContention(t *testing.T) {
	t.Parallel()

	s := New()
	var runs atomic.Int64
	fn := func() (any, error) {
		runs.Add(1)
		return "result", nil
	}

	const callers = 200
	results := make([]any, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := range callers {
		go func() {
			defer wg.Done()
			r, _ := s.Do("k", fn)
			results[i] = r
		}()
	}
	wg.Wait()

	if got := runs.Load(); got != 1 {
		t.Fatalf("fn ran %d times, want exactly 1", got)
	}
	for i, r := range results {
		if r != "result" {
			t.Fatalf("caller %d got %v, want the shared result", i, r)
		}
	}
}

func TestDistinctKeysAllRun(t *testing.T) {
	t.Parallel()

	s := New()
	var runs atomic.Int64
	fn := func() (any, error) {
		runs.Add(1)
		return nil, nil
	}

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Do(string(rune('a'+i%50)), fn)
		}()
	}
	wg.Wait()

	if got := runs.Load(); got != 50 {
		t.Fatalf("distinct keys ran fn %d times, want 50", got)
	}
}

func TestErrorIsShared(t *testing.T) {
	t.Parallel()

	s := New()
	fn := func() (any, error) { return nil, errCharge }

	_, err1 := s.Do("k", fn)
	_, err2 := s.Do("k", fn) // duplicate: sees the first call's error
	if !errors.Is(err1, errCharge) {
		t.Fatalf("first error = %v, want errCharge", err1)
	}
	if !errors.Is(err2, errCharge) {
		t.Fatalf("duplicate error = %v, want the shared errCharge", err2)
	}
}
```

## Review

The store is correct when the claim — inserting the `call` for a new key — and
the check that precedes it happen under one continuous lock, and when `fn` runs
strictly outside that lock. The exactly-once test is the proof: with 200
goroutines on one key, `runs` must be exactly 1, and any value above 1 means the
check-then-act was split and two goroutines both claimed the key. Sharing the
result through a closed channel gives every caller a race-free view of the
outcome.

The traps: running `fn` under the lock turns the store into a global bottleneck
where one slow key blocks all others; and splitting the check from the claim (two
lock/unlock pairs) reopens the double-processing window. A subtler decision is
lifetime — this store keeps every result forever, which is right for an
idempotency ledger but would be a memory leak for a request-coalescing cache;
there you would delete the `call` once it completes. Run `go test -race`.

## Resources

- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — guarding the map and the claim.
- [`golang.org/x/sync/singleflight`](https://pkg.go.dev/golang.org/x/sync/singleflight) — the production version of this coalescing pattern.
- [The Go Memory Model](https://go.dev/ref/mem) — why a closed channel publishes the result to every reader.
- [Data Race Detector](https://go.dev/doc/articles/race_detector) — proving exactly-once under contention.

---

Back to [05-token-bucket-limiter.md](05-token-bucket-limiter.md) | Next: [07-copylocks-bug-fix.md](07-copylocks-bug-fix.md)
