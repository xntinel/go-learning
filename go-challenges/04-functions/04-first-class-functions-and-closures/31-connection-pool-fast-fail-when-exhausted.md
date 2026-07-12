# Exercise 31: Connection Pool Fast-Fail Decorator When Exhausted

**Nivel: Intermedio** — validacion rapida (un test corto).

When every connection in a database pool is busy, queuing the next caller
feels safer than rejecting it — until the queue itself becomes the outage,
because every blocked goroutine holds a request open, a timer running, and
memory pinned, and the backlog only grows while the downstream stays slow.
`NewFastFailQuery` decorates a real query function with a closure over a
mutex-guarded, capacity-capped counter: once the pool is full, it returns an
error immediately instead of waiting for a slot, so a struggling database
degrades a fraction of requests fast instead of stalling all of them slowly.

## What you'll build

```text
connection-pool/             independent module: example.com/connection-pool
  go.mod                      go 1.24
  connpool.go                 ErrPoolExhausted, NewFastFailQuery
  cmd/
    demo/
      main.go                  one query holds the only slot, a second fast-fails
  connpool_test.go             table test: fast-fail while exhausted, succeeds once freed
```

- Files: `connpool.go`, `cmd/demo/main.go`, `connpool_test.go`.
- Implement: `NewFastFailQuery(capacity int, query func(sql string) (string, error)) func(sql string) (string, error)`, closing over a mutex-guarded in-use counter.
- Test: with one in-flight query holding the pool's only slot, a second concurrent call returns `ErrPoolExhausted` immediately (no waiting); once the first query releases its slot, the next call runs normally; calls within capacity always run the real query.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Reject fast, never queue

`NewFastFailQuery` captures `inUse`, an integer counter, and a mutex,
alongside the wrapped `query` function. Two small closures, `acquire` and
`release`, do the check-then-act under the same lock: `acquire` returns
`false` immediately if `inUse >= capacity` (no waiting, no `sync.Cond`, no
channel with a buffer to sit in) and otherwise increments `inUse` and
returns `true`. The returned decorator calls `acquire`; on failure it
returns `ErrPoolExhausted` without ever touching `query`; on success it
always calls `release` via `defer`, so a panic or an early return inside
`query` still frees the slot.

The alternative design — a buffered channel of size `capacity` used as a
semaphore, blocking on send when full — trades an instant, cheap rejection
for an unbounded queue of blocked goroutines. This decorator makes the
opposite, deliberate trade-off: a caller finding the pool exhausted learns
that immediately and can retry elsewhere, apply its own backoff, or fail the
request outward, rather than piling up behind a database that is already
struggling.

Create `connpool.go`:

```go
// Package connpool decorates a database query function with fast-fail pool
// admission: instead of queuing callers behind in-flight queries once the
// pool is full, it rejects immediately.
package connpool

import (
	"errors"
	"sync"
)

// ErrPoolExhausted is returned immediately when the pool has no free
// connections, instead of queuing the caller behind in-flight queries.
var ErrPoolExhausted = errors.New("connpool: pool exhausted, fast-failing")

// NewFastFailQuery returns a closure over a mutex-guarded in-use counter
// capped at capacity. The returned function wraps query: if the pool is
// already at capacity it returns ErrPoolExhausted immediately -- no
// waiting, no queuing -- so a struggling downstream never builds an
// ever-growing backlog of blocked callers. Otherwise it reserves a slot,
// runs query, and always releases the slot afterward.
func NewFastFailQuery(capacity int, query func(sql string) (string, error)) func(sql string) (string, error) {
	var mu sync.Mutex
	inUse := 0

	acquire := func() bool {
		mu.Lock()
		defer mu.Unlock()
		if inUse >= capacity {
			return false
		}
		inUse++
		return true
	}
	release := func() {
		mu.Lock()
		defer mu.Unlock()
		inUse--
	}

	return func(sql string) (string, error) {
		if !acquire() {
			return "", ErrPoolExhausted
		}
		defer release()
		return query(sql)
	}
}
```

### The runnable demo

A first query holds the pool's only slot on a channel while a second call
fast-fails; releasing the first slot lets a third call run normally.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/connection-pool"
)

func main() {
	release := make(chan struct{})
	started := make(chan struct{})

	slowQuery := func(sql string) (string, error) {
		started <- struct{}{}
		<-release
		return "row-1", nil
	}

	query := connpool.NewFastFailQuery(1, slowQuery)

	var wg sync.WaitGroup
	var firstResult, secondResult string
	var secondErr error

	wg.Add(1)
	go func() {
		defer wg.Done()
		firstResult, _ = query("SELECT * FROM accounts")
	}()
	<-started // first query has acquired the pool's only slot and is blocked

	secondResult, secondErr = query("SELECT * FROM orders")
	fmt.Printf("second call while pool exhausted: result=%q err=%v\n", secondResult, secondErr)

	release <- struct{}{} // let the first query finish
	wg.Wait()
	fmt.Printf("first call result: %q\n", firstResult)

	var thirdResult string
	var thirdErr error
	thirdDone := make(chan struct{})
	go func() {
		defer close(thirdDone)
		thirdResult, thirdErr = query("SELECT * FROM accounts")
	}()
	<-started             // third call acquired the now-free slot and is running
	release <- struct{}{} // let it finish
	<-thirdDone
	fmt.Printf("third call after slot freed: result=%q err=%v\n", thirdResult, thirdErr)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
second call while pool exhausted: result="" err=connpool: pool exhausted, fast-failing
first call result: "row-1"
third call after slot freed: result="row-1" err=<nil>
```

### Tests

Create `connpool_test.go`:

```go
package connpool

import (
	"errors"
	"sync"
	"testing"
)

func TestFastFailWhenPoolExhausted(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	blockingQuery := func(sql string) (string, error) {
		started <- struct{}{}
		<-release
		return "ok", nil
	}

	query := NewFastFailQuery(1, blockingQuery)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := query("first"); err != nil {
			t.Errorf("first query error = %v, want nil", err)
		}
	}()
	<-started // first query now holds the pool's only slot

	if _, err := query("second"); !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("second query error = %v, want %v", err, ErrPoolExhausted)
	}

	release <- struct{}{}
	wg.Wait()
}

func TestQuerySucceedsAfterSlotFreed(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	blockingQuery := func(sql string) (string, error) {
		started <- struct{}{}
		<-release
		return "ok", nil
	}

	query := NewFastFailQuery(1, blockingQuery)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		query("first")
	}()
	<-started
	release <- struct{}{}
	wg.Wait()

	// The slot is free again; this call must run the real query, not
	// fast-fail. blockingQuery still synchronizes on started/release, so
	// drive it from a goroutine and unblock it from the test body.
	var got string
	var err error
	done := make(chan struct{})
	go func() {
		defer close(done)
		got, err = query("second")
	}()
	<-started
	release <- struct{}{}
	<-done

	if err != nil {
		t.Fatalf("second query error = %v, want nil", err)
	}
	if got != "ok" {
		t.Fatalf("second query result = %q, want %q", got, "ok")
	}
}

func TestQueriesRunWhenWithinCapacity(t *testing.T) {
	instant := func(sql string) (string, error) { return "result:" + sql, nil }
	query := NewFastFailQuery(2, instant)

	got1, err1 := query("a")
	got2, err2 := query("b")
	if err1 != nil || err2 != nil {
		t.Fatalf("errors = %v, %v, want nil, nil", err1, err2)
	}
	if got1 != "result:a" || got2 != "result:b" {
		t.Fatalf("results = %q, %q, want result:a, result:b", got1, got2)
	}
}
```

Verify: `go test -count=1 -race ./...`

## Review

`TestFastFailWhenPoolExhausted` is the contract this whole exercise exists
for: while one query genuinely holds the pool's only slot, a second call
returns `ErrPoolExhausted` synchronously — the test uses an unbuffered
channel handshake, not a sleep, to prove the second call never blocked
waiting for the first. `TestQuerySucceedsAfterSlotFreed` proves the flip
side: once `release` runs, the counter drops and the very next call is
treated as ordinary capacity, not as still-exhausted. `TestQueriesRunWhenWithinCapacity`
is the baseline: calls that never approach capacity always reach the real
`query` unchanged.

## Resources

- [pkg.go.dev: sync.Mutex](https://pkg.go.dev/sync#Mutex) — guards the shared in-use counter's full check-then-act.
- [database/sql: SetMaxOpenConns](https://pkg.go.dev/database/sql#DB.SetMaxOpenConns) — the real connection-pool capacity this exercise models a fast-fail decorator around.
- [Google SRE Workbook: Handling Overload](https://sre.google/sre-book/handling-overload/) — why fast rejection beats unbounded queuing under sustained overload.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [30-gossip-broadcast-with-exponential-backoff.md](30-gossip-broadcast-with-exponential-backoff.md) | Next: [32-idempotency-key-dedup-with-ttl-window.md](32-idempotency-key-dedup-with-ttl-window.md)
