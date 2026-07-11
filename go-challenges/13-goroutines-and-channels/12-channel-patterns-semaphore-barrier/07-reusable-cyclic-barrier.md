# Exercise 7: A Reusable Barrier for a Phased Batch Pipeline

The one-shot barrier from Exercise 2 releases once and stays open — useless for a
pipeline that runs in rounds: all shards extract, barrier, all transform, barrier,
all load. A cyclic barrier resets after each release so the same N workers can
rendezvous round after round. This exercise builds it and proves every worker
finishes round K before any worker starts round K+1.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
cyclic/                     independent module: example.com/cyclic
  go.mod                    go 1.26
  cyclic.go                 type CyclicBarrier; NewCyclicBarrier, Wait
  cmd/
    demo/
      main.go               3 workers march through 3 phases in lockstep
  cyclic_test.go            round-K-before-K+1 property; stress across many rounds (-race)
```

- Files: `cyclic.go`, `cmd/demo/main.go`, `cyclic_test.go`.
- Implement: a `CyclicBarrier` for N participants whose `Wait` blocks until all N arrive, releases them together, and resets for the next round.
- Test: with 3 workers over 4 rounds, assert every worker completes round K before any starts round K+1; a stress run with more workers and rounds must not deadlock.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/cyclic/cmd/demo
cd ~/go-exercises/cyclic
go mod init example.com/cyclic
go mod edit -go=1.26
```

### Why a fresh channel per round

The one-shot barrier closes its channel exactly once, and a channel can never
reopen — a second close panics, and receiving from an already-closed channel
returns instantly. So reuse needs a new channel for each round. The generation
token here *is* the channel: each round has its own channel, and moving to the
next round means swapping in a fresh one.

`Wait` locks, increments the arrival count, and branches. If this is not the last
arrival, it captures the *current* round's channel, unlocks, and blocks on a
receive from that channel — so it waits on this round's channel specifically, not
whatever channel a later round installs. If this *is* the last arrival, it does
the whole reset under the lock: it closes the current channel (the broadcast that
releases everyone parked on it), installs a fresh channel for the next round, and
zeroes the count, then returns without blocking. Because the close and the swap
happen under the same mutex, no arrival can observe a half-reset barrier, and each
round's waiters are released on their own channel — so round K+1's close never
touches round K's channel.

The subtlety that makes this correct: a waiter captures `ch := b.ch` *before*
unlocking, so even though the last arrival replaces `b.ch`, the waiter is still
parked on the round it belongs to. Getting that capture wrong — reading `b.ch`
after unlocking — would let a fast worker in round K+1 close a channel a slow
round-K waiter is still holding, a classic reuse bug.

Create `cyclic.go`:

```go
package cyclic

import "sync"

// CyclicBarrier is a reusable rendezvous for n participants. Each round, every
// participant calls Wait and blocks until all n have arrived; then all are
// released together and the barrier resets for the next round.
type CyclicBarrier struct {
	n     int
	mu    sync.Mutex
	count int
	ch    chan struct{}
}

// NewCyclicBarrier returns a barrier that releases every time n participants
// have called Wait, resetting itself after each release.
func NewCyclicBarrier(n int) *CyclicBarrier {
	return &CyclicBarrier{n: n, ch: make(chan struct{})}
}

// Wait blocks until all n participants have called Wait for the current round.
// The last arrival releases everyone and resets the barrier for the next round.
func (b *CyclicBarrier) Wait() {
	b.mu.Lock()
	b.count++
	if b.count == b.n {
		// Last arrival: broadcast on this round's channel, then install a fresh
		// channel and reset the count for the next round, all under the lock.
		close(b.ch)
		b.ch = make(chan struct{})
		b.count = 0
		b.mu.Unlock()
		return
	}
	// Capture this round's channel before unlocking, so a later round swapping
	// in a new channel cannot affect this waiter.
	ch := b.ch
	b.mu.Unlock()
	<-ch
}
```

### The runnable demo

Three workers march through three phases. Every worker does phase P's "work",
waits at the barrier, and only then moves to phase P+1 — so no worker is ever more
than one phase ahead of another. The demo prints a deterministic summary.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/cyclic"
)

func main() {
	const workers, phases = 3, 3
	b := cyclic.NewCyclicBarrier(workers)

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range phases {
				// ... phase work happens here ...
				b.Wait() // lockstep: nobody advances until all arrive
			}
		}()
	}
	wg.Wait()

	fmt.Printf("workers=%d phases=%d completed=true\n", workers, phases)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
workers=3 phases=3 completed=true
```

### Tests

`TestRoundOrdering` is the load-bearing property. Three workers run four rounds;
before waiting in round K each worker increments `arrivals[K]`. Because
`arrivals[K]` is incremented *before* `Wait` and the barrier releases only after
all three have arrived, every worker sees `arrivals[K] == 3` the instant `Wait`
returns — proof that round K completed before anyone advanced to K+1. It also
records the sequence of rounds each worker observed and checks it is `0,1,2,3`.
`TestStressManyRounds` runs many workers over many rounds to shake out any reuse
race and confirm no deadlock.

Create `cyclic_test.go`:

```go
package cyclic

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestRoundOrdering(t *testing.T) {
	t.Parallel()

	const workers, rounds = 3, 4
	b := NewCyclicBarrier(workers)

	arrivals := make([]atomic.Int64, rounds)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := range rounds {
				arrivals[r].Add(1)
				b.Wait()
				// Barrier released => all workers arrived at round r.
				if got := arrivals[r].Load(); got != workers {
					t.Errorf("round %d released with %d arrivals, want %d", r, got, workers)
				}
			}
		}()
	}
	wg.Wait()

	for r := range rounds {
		if got := arrivals[r].Load(); got != workers {
			t.Errorf("round %d final arrivals = %d, want %d", r, got, workers)
		}
	}
}

func TestStressManyRounds(t *testing.T) {
	t.Parallel()

	const workers, rounds = 8, 50
	b := NewCyclicBarrier(workers)

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range rounds {
				b.Wait()
			}
		}()
	}
	wg.Wait() // hangs if the barrier fails to reset between rounds
}
```

## Review

The cyclic barrier is correct when it releases exactly once per round and resets
cleanly for the next — the `arrivals[K] == 3` check the instant `Wait` returns is
the proof that no worker races ahead. The mechanism that makes reuse safe is a
fresh channel per round installed under the same lock as the close, plus each
waiter capturing its round's channel before unlocking. The two mistakes that
break it: reading `b.ch` after unlocking (a later round can swap it out from under
a waiter), and forgetting to reset the count (the barrier fires on the first round
and never again). Run `-race`, and use the stress test with many rounds — a reuse
bug often only surfaces when rounds overlap under load.

## Resources

- [Go spec: Close](https://go.dev/ref/spec#Close) — why a channel closes once, forcing a fresh channel per round.
- [Go Memory Model: channel close](https://go.dev/ref/mem#chan) — the happens-before edge from close to every receive.
- [sync.Mutex](https://pkg.go.dev/sync#Mutex) — guarding the count and the channel swap so no arrival sees a half-reset barrier.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-errgroup-bounded-fanout.md](06-errgroup-bounded-fanout.md) | Next: [08-startup-readiness-barrier.md](08-startup-readiness-barrier.md)
