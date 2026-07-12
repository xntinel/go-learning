# Exercise 27: Token Bucket Refill Race Overwrites Consumption

**Nivel: Intermedio** — validacion rapida (un test corto).

A token-bucket rate limiter looks like two independent operations: a
background timer periodically tops the bucket back up, and request
handlers pull a token off it on the hot path. Treating them as
independent because they run on different goroutines is the mistake —
they both read and write the exact same integer, and a plain
`tokens--` or `tokens += n` compiles down to a read, a modification,
and a write that are not atomic with each other. Two goroutines
interleaving those three steps on the same counter lose updates in
either direction: a refill can silently undo a request's consumption,
handing back capacity that was already legitimately spent, which is
strictly worse for a rate limiter than losing a refill — it means the
limiter is quietly permitting more traffic than its configured rate.
This module is fully self-contained: its own `go mod init`, all code
inline, its own demo and tests.

## What you'll build

```text
tokenbucket/                 independent module: example.com/token-bucket-refill-concurrent-race
  go.mod                      go 1.21
  tokenbucket.go               Bucket, New, Take, Refill, Tokens
  cmd/
    demo/
      main.go                  runnable demo: concurrent takes, a refill, more concurrent takes
  tokenbucket_test.go           concurrent take/refill conservation test, exhaustion case
```

- Files: `tokenbucket.go`, `cmd/demo/main.go`, `tokenbucket_test.go`.
- Implement: `Bucket.Take() bool` and `Bucket.Refill(n int)` sharing one mutex-guarded counter, plus `Tokens() int` for observing it.
- Test: a stress test mixing concurrent takes and refills asserting the final count exactly matches the arithmetic sum of every individual change; a sequential exhaustion-then-refill case.
- Verify: `go test -count=1 -race ./...`.

### Why an unguarded refill can hand back capacity a request already spent

The version that ships first often guards nothing, on the theory that
`int` increments and decrements are "just numbers" and therefore cheap
enough not to need a lock:

```go
// BUG: read-modify-write without a lock. Take and Refill running on
// different goroutines can interleave between the read and the write of
// b.tokens, so one goroutine's update is silently lost.
func (b *Bucket) Take() bool {
	if b.tokens <= 0 {
		return false
	}
	b.tokens--
	return true
}

func (b *Bucket) Refill(n int) {
	b.tokens += n
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}
}
```

Picture one token left and a refill of one arriving at nearly the same
instant. A request's `Take` reads `tokens == 1`, decides to proceed,
and is about to write `tokens = 0` — but the scheduler switches to the
refill goroutine first, which reads the *same* `tokens == 1`, computes
`tokens + 1 = 2`, and writes it. Control returns to the request
goroutine, which now writes its own stale computation, `tokens = 0`,
clobbering the refill's `2` entirely. The bucket ends the sequence at
`0` tokens when the correct answer — one consumed, one refilled — is
`1`. Run the same interleaving with the read and write reversed and
the loss goes the other way: a refill's increment can vanish under a
request's decrement instead. Neither direction of loss is rare under
real load; it is the default outcome of any unguarded read-modify-write
racing across goroutines, which is exactly why `go test -race` exists
to catch it even when a given run happens not to produce a visibly
wrong final count.

The fix treats `Take` and `Refill` as two operations on one piece of
shared state that must never run at the same time, full stop — both
acquire the same mutex before touching `tokens`:

```go
func (b *Bucket) Take() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tokens <= 0 {
		return false
	}
	b.tokens--
	return true
}
```

Now the refill and the request's decrement can never interleave: one
of them holds the mutex for its entire read-modify-write, so the other
sees either the state before that change or fully after it, never a
half-applied version of it.

Create `tokenbucket.go`:

```go
package tokenbucket

import "sync"

// Bucket is a token-bucket rate limiter: Take consumes one token if any
// are available, and Refill periodically returns tokens up to capacity.
// Both operations mutate the same shared counter, so both must go through
// the same mutex -- a refill running concurrently with a consumption is
// exactly as much a shared-state update as two concurrent consumptions
// are, even though it moves the counter in the opposite direction.
type Bucket struct {
	mu       sync.Mutex
	tokens   int
	capacity int
}

// New creates a Bucket starting at full capacity.
func New(capacity int) *Bucket {
	return &Bucket{tokens: capacity, capacity: capacity}
}

// Take consumes one token and reports whether it was available.
func (b *Bucket) Take() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tokens <= 0 {
		return false
	}
	b.tokens--
	return true
}

// Refill adds n tokens, capped at capacity.
func (b *Bucket) Refill(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tokens += n
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}
}

// Tokens returns the current token count.
func (b *Bucket) Tokens() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tokens
}
```

### The runnable demo

The demo sends 10 concurrent takes against a 10-token bucket (all
succeed, draining it), refills 4 tokens sequentially, then sends 6 more
concurrent takes — only 4 of which can possibly succeed. Every printed
number is deterministic: which specific goroutine wins is not, but how
many succeed always is, because the mutex makes every update land.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"

	"example.com/token-bucket-refill-concurrent-race"
)

func concurrentTakes(b *tokenbucket.Bucket, n int) int64 {
	var succeeded int64
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if b.Take() {
				atomic.AddInt64(&succeeded, 1)
			}
		}()
	}
	wg.Wait()
	return succeeded
}

func main() {
	b := tokenbucket.New(10)

	ok := concurrentTakes(b, 10)
	fmt.Println("phase 1: concurrent takes succeeded:", ok, "remaining:", b.Tokens())

	b.Refill(4)
	fmt.Println("phase 2: after refilling 4, remaining:", b.Tokens())

	ok = concurrentTakes(b, 6)
	fmt.Println("phase 3: concurrent takes succeeded:", ok, "remaining:", b.Tokens())
}
```

```bash
go run ./cmd/demo
```

Expected output:

```text
phase 1: concurrent takes succeeded: 10 remaining: 0
phase 2: after refilling 4, remaining: 4
phase 3: concurrent takes succeeded: 4 remaining: 0
```

### Tests

`TestConcurrentTakeAndRefillConserveTokens` is the concurrency case: it
fires 300 concurrent takes and 50 concurrent refills of 2 tokens each
against a bucket sized so capacity never clamps, then asserts the
final token count exactly equals `capacity - succeededTakes +
totalRefilled` — an arithmetic identity that any lost update, in
either direction, would break. `TestTakeRejectsWhenExhausted` pins the
simple sequential contract: an exhausted bucket rejects further takes
until a refill arrives.

Create `tokenbucket_test.go`:

```go
package tokenbucket

import (
	"sync"
	"sync/atomic"
	"testing"
)

// TestConcurrentTakeAndRefillConserveTokens is the concurrency case: many
// goroutines decrement and many goroutines increment the same shared
// counter at once. A correct implementation guards both operations with
// one mutex, so the final count must exactly equal the arithmetic sum of
// every individual change, regardless of interleaving -- an unsynchronized
// read-modify-write would lose some of those changes under -race.
func TestConcurrentTakeAndRefillConserveTokens(t *testing.T) {
	const (
		capacity     = 1000 // large enough that clamping to capacity never triggers
		takers       = 300
		refillers    = 50
		refillAmount = 2
	)

	b := New(capacity)

	var succeeded int64
	var wg sync.WaitGroup
	wg.Add(takers + refillers)

	for i := 0; i < takers; i++ {
		go func() {
			defer wg.Done()
			if b.Take() {
				atomic.AddInt64(&succeeded, 1)
			}
		}()
	}
	for i := 0; i < refillers; i++ {
		go func() {
			defer wg.Done()
			b.Refill(refillAmount)
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt64(&succeeded); got != takers {
		t.Fatalf("succeeded takes = %d, want %d (capacity never came close to exhausted)", got, takers)
	}

	wantTokens := capacity - int(succeeded) + refillers*refillAmount
	if got := b.Tokens(); got != wantTokens {
		t.Fatalf("tokens = %d, want %d (capacity - takes + total refilled: a concurrent refill overwrote a concurrent take's deduction, or vice versa)", got, wantTokens)
	}
}

// TestTakeRejectsWhenExhausted pins the simple sequential case: once every
// token is consumed, further takes fail until a refill arrives.
func TestTakeRejectsWhenExhausted(t *testing.T) {
	b := New(2)
	if !b.Take() || !b.Take() {
		t.Fatal("expected the first two takes on a 2-token bucket to succeed")
	}
	if b.Take() {
		t.Fatal("Take() succeeded on an exhausted bucket, want false")
	}
	b.Refill(1)
	if !b.Take() {
		t.Fatal("Take() failed right after a refill, want true")
	}
}
```

Run: `go test -count=1 -race ./...`.

## Review

`Bucket` is correct when the token count after any mix of concurrent
takes and refills exactly equals the arithmetic sum of every
individual change — proven with a stress test large enough (300 takers,
50 refillers) that a lost update would show up as a wrong final number,
not just as a `-race` warning. The mistake this design avoids is
believing that because `Take` and `Refill` move the counter in opposite
directions, they cannot interfere with each other; a read-modify-write
race does not care about the sign of the modification, only about
whether two goroutines can observe the same starting value before
either has written back its result. The fix is the least clever thing
possible: one mutex, held for the full duration of every operation
that touches `tokens`, with no operation on the shared counter allowed
to happen outside it.

## Resources

- [sync.Mutex](https://pkg.go.dev/sync#Mutex) — guarding a shared counter so read-modify-write sequences from different goroutines cannot interleave.
- [Go Race Detector](https://go.dev/doc/articles/race_detector) — `-race` flags the unsynchronized access even on runs where the final count happens to look right.
- [Token bucket algorithm (Wikipedia)](https://en.wikipedia.org/wiki/Token_bucket) — the algorithm this Bucket implements, including the refill-and-cap-at-capacity behavior.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [26-singleflight-request-dedup-map-race.md](26-singleflight-request-dedup-map-race.md) | Next: [28-bloom-filter-inverted-assumption.md](28-bloom-filter-inverted-assumption.md)
