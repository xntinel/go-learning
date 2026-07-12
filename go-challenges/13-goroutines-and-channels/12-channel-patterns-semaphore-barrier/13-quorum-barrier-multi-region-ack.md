# Exercise 13: K-of-N Quorum Barrier for Multi-Region Write Acknowledgment

**Level: Advanced**

A coordinator replicates a write to N regions and must return success the instant
a write quorum of K regions acknowledges, without waiting for stragglers, and must
fail fast the moment enough regions have reported failure that K can never be
reached. The naive "wait for all N" gate stalls on the slowest region and cannot
express partial success; the naive "count acks with an atomic and poll" spins or
races on the broadcast. This exercise builds a barrier that releases on partial
(K-of-N) arrival: the Kth ack closes a broadcast channel exactly once, and late
acks or fails past the decision point neither panic nor block.

This module is self-contained: its own module, a `quorum` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
quorum/                      independent module: example.com/quorum
  go.mod                     go 1.26; requires go.uber.org/goleak (test only)
  quorum.go                  type Latch: New, Ack, Fail, Wait, Acks; ErrQuorumImpossible
  cmd/demo/main.go           runnable demo: quorum reached, fail-fast, deadline
  quorum_test.go             quorum release, straggler safety, fail-fast, deadline, leak-freedom
```

- Files: `quorum.go`, `cmd/demo/main.go`, `quorum_test.go`.
- Implement: `New(total, need int) *Latch`, `(*Latch).Ack()`, `(*Latch).Fail()`, `(*Latch).Wait(ctx context.Context) error`, `(*Latch).Acks() int`, and `ErrQuorumImpossible`.
- Test: three acks release `Wait` with nil and later Ack/Fail calls neither panic nor flip the verdict; three fails release `Wait` with `ErrQuorumImpossible` before any deadline; two acks under a short deadline return `context.DeadlineExceeded`; a `goleak` `TestMain` proves straggler goroutines exit cleanly.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/12-channel-patterns-semaphore-barrier/13-quorum-barrier-multi-region-ack/cmd/demo
cd go-solutions/13-goroutines-and-channels/12-channel-patterns-semaphore-barrier/13-quorum-barrier-multi-region-ack
go mod edit -go=1.26
go get go.uber.org/goleak
go mod tidy
```

### A barrier that releases on K-of-N, not all-N

A readiness gate releases when the *last* participant arrives: close the channel
on the N-th arrival and everyone parked on the receive proceeds. A quorum barrier
is different — it must release on the *K-th* arrival while N-K regions are still
outstanding, and it must also release early on failure. Two verdicts share one
broadcast channel, and the protocol is this:

1. `Ack` increments the ack count under the mutex. When the count first reaches
   `need`, that call closes `done` and marks the verdict as success.
2. `Fail` increments the fail count under the mutex. When `total-fails < need`
   — the outstanding regions can no longer supply the required acks — that call
   closes `done` and marks the verdict as impossible.
3. `Wait` selects on `done` and on `ctx.Done()`. When `done` fires, it reads the
   verdict flag under the mutex and returns nil or `ErrQuorumImpossible`; when
   the context fires first, it returns `ctx.Err()`.

The single hard invariant is that `done` is closed exactly once. Regions call
`Ack` and `Fail` concurrently, so two goroutines can both observe a
close-worthy condition in the same window; a second `close` panics. The guard is
a `settled` boolean checked and set under the same mutex that guards the counts,
so the first goroutine to cross a threshold flips `settled`, closes the channel,
and every later crossing sees `settled` already true and does nothing. That same
mutex discipline is why the two verdicts are mutually exclusive: if `fails` ever
made quorum impossible, then `acks <= total-fails < need`, so `Ack` can never have
closed the channel first, and vice versa.

Because the ack count is mutated only while holding the mutex, and the Kth ack's
`close(done)` happens under that same lock, the close *happens-after* all K
increments in the Go memory model. A waiter released by the close is therefore
guaranteed to observe `Acks() >= need` — there is no window where the barrier has
opened but the count still reads stale.

The final property is straggler safety, which is what the goleak test proves. A
region whose network was slow may call `Ack` long after the coordinator already
returned success and moved on. That late call must find `settled` true, increment
the count, and return without touching the closed channel and without blocking —
so its goroutine exits cleanly and leaks nothing.

Create `quorum.go`:

```go
// Package quorum implements a K-of-N barrier: a coordinator replicating a write
// to N regions can wait for the first K acknowledgments and fail fast the moment
// enough regions have reported failure that K can never be reached.
package quorum

import (
	"context"
	"errors"
	"sync"
)

// ErrQuorumImpossible is returned by Wait once so many regions have failed that
// the remaining acks can never reach the required quorum.
var ErrQuorumImpossible = errors.New("quorum can no longer be reached")

// Latch is a one-shot K-of-N barrier. It releases waiters when either the Kth
// ack arrives (success) or so many fails arrive that K is unreachable (failure).
// It is safe for concurrent use by all N region goroutines plus the waiter.
type Latch struct {
	mu         sync.Mutex
	total      int  // N: total regions the write was sent to
	need       int  // K: acks required for quorum
	acks       int  // successful acknowledgments so far
	fails      int  // reported failures so far
	settled    bool // guards the single close of done
	impossible bool // true when done was closed by fail-fast rather than quorum
	done       chan struct{}
}

// New returns a Latch that reaches quorum at need acks out of total regions.
func New(total, need int) *Latch {
	return &Latch{total: total, need: need, done: make(chan struct{})}
}

// Ack records one region's successful write. The need-th ack broadcasts by
// closing done exactly once; every later ack is counted but never re-closes.
func (l *Latch) Ack() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.acks++
	if l.acks >= l.need && !l.settled {
		l.settled = true
		close(l.done) // broadcast: releases the waiter with a success verdict
	}
}

// Fail records one region's failed write. When the regions still outstanding
// (total-fails) can no longer supply need acks, it settles the latch as
// impossible, closing done exactly once with a failure verdict.
func (l *Latch) Fail() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fails++
	if l.total-l.fails < l.need && !l.settled {
		l.settled = true
		l.impossible = true
		close(l.done) // broadcast: releases the waiter with a fail-fast verdict
	}
}

// Wait blocks until the latch settles or ctx is done. It returns nil once the
// quorum of need acks is visible, ErrQuorumImpossible on fail-fast, or ctx.Err()
// if the deadline fires before either verdict.
func (l *Latch) Wait(ctx context.Context) error {
	select {
	case <-l.done:
		l.mu.Lock()
		impossible := l.impossible
		l.mu.Unlock()
		if impossible {
			return ErrQuorumImpossible
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Acks reports how many successful acknowledgments have been recorded.
func (l *Latch) Acks() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.acks
}
```

### The runnable demo

The demo runs three scenarios sequentially so the output is deterministic even
though stragglers ack concurrently: quorum reached with two late stragglers, a
fail-fast that settles before its context deadline, and a deadline that fires
because only two of the three required acks ever arrive.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"example.com/quorum"
)

func main() {
	// Scenario 1: quorum reached, then stragglers ack after the decision.
	// total=5, need=3. Three acks settle the latch; two late acks must neither
	// panic (guarded single close) nor change the success verdict.
	{
		l := quorum.New(5, 3)
		for range 3 {
			l.Ack()
		}
		verdict := verdictOf(l.Wait(context.Background()))
		acksAtDecision := l.Acks()

		// Late stragglers ack after the barrier already released the waiter.
		var wg sync.WaitGroup
		for range 2 {
			wg.Go(func() { l.Ack() })
		}
		wg.Wait()

		fmt.Printf("scenario=quorum-reached verdict=%s acks_at_decision=%d final_acks=%d\n",
			verdict, acksAtDecision, l.Acks())
	}

	// Scenario 2: fail-fast. total=5, need=3. After three fails only two regions
	// remain, so quorum is unreachable and Wait returns before any deadline.
	{
		l := quorum.New(5, 3)
		for range 3 {
			l.Fail()
		}
		verdict := verdictOf(l.Wait(context.Background()))
		fmt.Printf("scenario=fail-fast verdict=%s acks=%d\n", verdict, l.Acks())
	}

	// Scenario 3: deadline. need=3 but only two regions ever ack, so Wait blocks
	// until the context deadline fires.
	{
		l := quorum.New(5, 3)
		l.Ack()
		l.Ack()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		defer cancel()
		verdict := verdictOf(l.Wait(ctx))
		fmt.Printf("scenario=deadline verdict=%s acks=%d\n", verdict, l.Acks())
	}
}

func verdictOf(err error) string {
	switch {
	case err == nil:
		return "quorum-ok"
	case errors.Is(err, quorum.ErrQuorumImpossible):
		return "quorum-impossible"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline-exceeded"
	default:
		return "error:" + err.Error()
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
scenario=quorum-reached verdict=quorum-ok acks_at_decision=3 final_acks=5
scenario=fail-fast verdict=quorum-impossible acks=0
scenario=deadline verdict=deadline-exceeded acks=2
```

### Tests

`TestQuorumReachedThenStragglers` records three acks, asserts `Wait` returns nil
with `Acks() == 3` at the decision, then fires a straggler `Ack` and `Fail` and
asserts neither panics and the verdict stays nil. `TestConcurrentStragglersNoPanic`
drives all five regions from concurrent goroutines so the increments and the
single close race under `-race`. `TestFailFast` records three fails and asserts
`Wait` returns `ErrQuorumImpossible` under a one-minute deadline that must not be
the reason it returns. `TestDeadline` records two acks and asserts a short
deadline yields `context.DeadlineExceeded`. `TestSuccessSticksDespiteLateFails`
proves a settled success is not flipped by later fails. The `TestMain` runs every
test under `goleak.VerifyTestMain` to prove no straggler goroutine leaks.

Create `quorum_test.go`:

```go
package quorum

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// TestMain runs every test under goleak: any region goroutine that acks or fails
// after the quorum decided must exit cleanly, leaving no leaked goroutine.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestQuorumReachedThenStragglers pins the core property: three acks release
// Wait with nil, and two further Ack/Fail calls afterward neither panic (the
// single close is guarded) nor flip the verdict.
func TestQuorumReachedThenStragglers(t *testing.T) {
	t.Parallel()

	l := New(5, 3)
	for range 3 {
		l.Ack()
	}
	if err := l.Wait(context.Background()); err != nil {
		t.Fatalf("Wait after 3 acks = %v, want nil", err)
	}
	if got := l.Acks(); got != 3 {
		t.Fatalf("Acks at decision = %d, want 3", got)
	}

	// Stragglers arrive after the barrier already released. A second close would
	// panic; a guarded close makes these safe. Wait must still report success.
	l.Ack()
	l.Fail()
	if err := l.Wait(context.Background()); err != nil {
		t.Fatalf("Wait after stragglers = %v, want nil", err)
	}
	if got := l.Acks(); got != 4 {
		t.Fatalf("Acks after straggler ack = %d, want 4", got)
	}
}

// TestConcurrentStragglersNoPanic drives all N regions concurrently so the ack
// increments and the single close race under -race, proving the mutex guard
// makes exactly one close happen regardless of arrival order.
func TestConcurrentStragglersNoPanic(t *testing.T) {
	t.Parallel()

	l := New(5, 3)
	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() { l.Ack() })
	}
	wg.Wait()

	if err := l.Wait(context.Background()); err != nil {
		t.Fatalf("Wait after 5 concurrent acks = %v, want nil", err)
	}
	if got := l.Acks(); got != 5 {
		t.Fatalf("Acks = %d, want 5", got)
	}
}

// TestFailFast pins that three fails out of five regions make quorum of three
// unreachable, so Wait returns ErrQuorumImpossible before any deadline elapses.
func TestFailFast(t *testing.T) {
	t.Parallel()

	l := New(5, 3)
	for range 3 {
		l.Fail()
	}
	// A generous deadline that must NOT be the reason Wait returns: the fail-fast
	// verdict must win well before it.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := l.Wait(ctx); !errors.Is(err, ErrQuorumImpossible) {
		t.Fatalf("Wait after 3 fails = %v, want ErrQuorumImpossible", err)
	}
}

// TestDeadline pins that with only two acks and no third in sight, a short
// context deadline makes Wait return context.DeadlineExceeded.
func TestDeadline(t *testing.T) {
	t.Parallel()

	l := New(5, 3)
	l.Ack()
	l.Ack()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := l.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait with 2 acks = %v, want context.DeadlineExceeded", err)
	}
}

// TestSuccessSticksDespiteLateFails pins that once quorum is reached, later
// fails that would make (total-fails) < need cannot flip the settled verdict.
func TestSuccessSticksDespiteLateFails(t *testing.T) {
	t.Parallel()

	l := New(5, 3)
	for range 3 {
		l.Ack()
	}
	// Two late fails: total-fails = 3 which is not < need, and even a third would
	// arrive after the latch already settled as success.
	l.Fail()
	l.Fail()
	l.Fail()
	if err := l.Wait(context.Background()); err != nil {
		t.Fatalf("Wait = %v, want nil (success must stick)", err)
	}
}
```

## Review

Correct here means `Wait` returns the right verdict for K-of-N arrival: nil the
instant the Kth ack is visible, `ErrQuorumImpossible` the instant the outstanding
regions can no longer supply K acks, and `ctx.Err()` if neither happens before the
deadline — with `done` closed exactly once no matter how the concurrent acks and
fails interleave. The `settled` boolean checked and set under the same mutex that
guards the counts is the single invariant that guarantees it: it makes the close
one-shot, makes the two verdicts mutually exclusive, and — because the count is
mutated only under that lock — makes the Kth-ack close happen-after all K
increments, so a released waiter never reads a stale count. The tests prove it by
pinning each verdict, by racing all N regions and later stragglers under `-race`
and `-count=2`, and by running everything under `goleak` so a straggler that acks
past the decision is shown to exit cleanly rather than block on a full channel or
panic on a second close. The production bug this prevents is the multi-region
coordinator that either hangs on the slowest region because it waited for all N,
or panics under load when two regions report their result in the same instant and
both try to close the broadcast — the classic double-close on a barrier.

## Resources

- [The Go Memory Model](https://go.dev/ref/mem) -- why the mutex-guarded increment makes the Kth-ack close happen-after all K increments.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) -- close-as-broadcast and context-driven early exit, the mechanisms this latch combines.
- [go.uber.org/goleak](https://pkg.go.dev/go.uber.org/goleak) -- the TestMain leak detector that proves straggler goroutines exit cleanly.
- [sync.Mutex](https://pkg.go.dev/sync#Mutex) -- the guard that makes the single close and the two verdicts mutually exclusive.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-admission-control-inflight-limiter.md](12-admission-control-inflight-limiter.md) | Next: [14-fair-fifo-semaphore-pool-acquire.md](14-fair-fifo-semaphore-pool-acquire.md)
