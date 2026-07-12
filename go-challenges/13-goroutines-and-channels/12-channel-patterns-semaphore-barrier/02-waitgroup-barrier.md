# Exercise 2: A One-Shot Barrier for Coordinated Release

A phased batch job often needs every shard to finish phase A before any shard
starts phase B — all extracts complete before the first load. That rendezvous is
a barrier: participants block until the last one arrives, then all proceed at
once. This exercise builds the one-shot barrier and proves it releases neither
early nor late.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
barrier/                    independent module: example.com/barrier
  go.mod                    go 1.26
  barrier.go                type Barrier; NewBarrier, Wait (mutex count + close broadcast)
  cmd/
    demo/
      main.go               3 shards rendezvous at a barrier, then all report
  barrier_test.go           releases-all-at-once and releases-after-all-arrive (-race)
```

- Files: `barrier.go`, `cmd/demo/main.go`, `barrier_test.go`.
- Implement: a `Barrier` for N participants whose `Wait` returns a receive-only channel that unblocks all participants once the Nth arrives.
- Test: prove no participant is released before the Nth arrives; prove N-1 arrivals release nobody and the Nth releases everyone.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Why close() is the broadcast

A barrier needs to wake *every* waiter at the same instant. A single value sent
on a channel wakes exactly one receiver, so that is the wrong tool. Closing a
channel, however, is a broadcast: every goroutine blocked on a receive from a
closed channel unblocks immediately, each getting the zero value, and any future
receive also returns instantly. So the barrier hands each participant the *same*
channel and closes it once the last participant arrives.

The arrival count must be guarded, because participants call `Wait` from
different goroutines and a lost increment would either fire the barrier early or
never. A mutex around `count++` and the `count == n` check is enough. `Wait`
returns the channel as a receive-only `<-chan struct{}` — the caller can only
wait on it, never close or send, which keeps the close a private responsibility
of the barrier. The critical ordering: the first N-1 arrivals increment and
return the still-open channel, then park on the receive; the Nth increment hits
`count == n`, closes the channel, and all N receives unblock together.

Create `barrier.go`:

```go
package barrier

import "sync"

// Barrier is a one-shot rendezvous for n participants. Every participant that
// calls Wait blocks on the returned channel until the nth participant arrives,
// at which point all are released simultaneously.
type Barrier struct {
	n     int
	mu    sync.Mutex
	count int
	ch    chan struct{}
}

// NewBarrier returns a barrier that releases once n participants have called
// Wait. It is single-use: after the nth arrival the barrier stays open.
func NewBarrier(n int) *Barrier {
	return &Barrier{n: n, ch: make(chan struct{})}
}

// Wait records this participant's arrival and returns a receive-only channel
// that is closed once all n participants have arrived. Receive from it to block
// until the barrier releases: all waiters are released at the same instant.
func (b *Barrier) Wait() <-chan struct{} {
	b.mu.Lock()
	b.count++
	if b.count == b.n {
		close(b.ch) // broadcast: every waiter's receive unblocks at once
	}
	b.mu.Unlock()
	return b.ch
}
```

### The runnable demo

Three shards each finish an "extract" phase, arrive at the barrier, and block.
Only when all three have arrived does the barrier release, and all three report
they are proceeding to the next phase. The demo prints a deterministic summary,
not per-shard interleaving, so the output is stable.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"

	"example.com/barrier"
)

func main() {
	const shards = 3
	b := barrier.NewBarrier(shards)

	var proceeded atomic.Int64
	var wg sync.WaitGroup
	for range shards {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// ... extract phase happens here ...
			<-b.Wait() // rendezvous: nobody proceeds until all arrive
			proceeded.Add(1)
		}()
	}
	wg.Wait()

	fmt.Printf("shards=%d proceeded=%d\n", shards, proceeded.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
shards=3 proceeded=3
```

### Tests

`TestBarrierReleasesAllAtOnce` launches three participants that push a token onto
a buffered channel once released, then drains the channel to confirm all three
were released. `TestBarrierReleasesAfterAllArrive` is the sharper property: with
a barrier of 2 and only one participant present, it asserts nobody has proceeded;
then the main goroutine itself arrives as the second participant, and confirms
the first is now released. Running under `-race` catches any unguarded access to
the count.

Create `barrier_test.go`:

```go
package barrier

import "testing"

func TestBarrierReleasesAllAtOnce(t *testing.T) {
	t.Parallel()

	const n = 3
	b := NewBarrier(n)
	released := make(chan struct{}, n)
	for range n {
		go func() {
			<-b.Wait()
			released <- struct{}{}
		}()
	}
	// All n participants eventually pass the barrier and report.
	for range n {
		<-released
	}
}

func TestBarrierReleasesAfterAllArrive(t *testing.T) {
	t.Parallel()

	b := NewBarrier(2)
	released := make(chan struct{})
	go func() {
		<-b.Wait() // participant 1: blocks until participant 2 arrives
		released <- struct{}{}
	}()

	// With only participant 1 present, nobody may proceed.
	select {
	case <-released:
		t.Fatal("barrier released with only 1 of 2 participants")
	default:
	}

	// The main goroutine is participant 2; its arrival releases both.
	<-b.Wait()
	<-released
}
```

## Review

The barrier is correct when it releases exactly when the Nth participant arrives:
never with N-1 present, always once N are. The broadcast comes from `close`, not
a send, because a send would wake only one waiter. Two failure modes dominate.
The first is the wrong count — a barrier built for N when only N-1 ever arrive
never releases, and every participant deadlocks; derive N from the real
participant set, not a guess. The second is reuse: this barrier is one-shot
because a channel can be closed exactly once, so calling `Wait` for a second
round returns the already-closed channel (immediate release) or, in a naive
variant that re-closes, panics. The next cyclic-barrier exercise fixes reuse
properly. Run `-race` to confirm the count guard holds under concurrent arrivals.

## Resources

- [Go spec: Close](https://go.dev/ref/spec#Close) — closing a channel and the receive-from-closed semantics that make the broadcast work.
- [Go Memory Model: channel close](https://go.dev/ref/mem#chan) — the happens-before guarantee that a close is observed by every waiting receive.
- [sync.Mutex](https://pkg.go.dev/sync#Mutex) — guarding the arrival count against concurrent increments.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-buffered-channel-semaphore.md](01-buffered-channel-semaphore.md) | Next: [03-context-aware-semaphore-acquire.md](03-context-aware-semaphore-acquire.md)
