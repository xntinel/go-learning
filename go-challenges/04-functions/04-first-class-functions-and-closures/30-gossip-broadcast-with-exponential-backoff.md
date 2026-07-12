# Exercise 30: Gossip Broadcast to Peers with Exponential Backoff Retry

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye concurrencia).

A node in a peer-to-peer cluster propagating a state update cannot let one
slow or flaky peer block the whole broadcast, but it also should not give up
on the first transient error — a peer that is momentarily overloaded often
recovers within a retry or two. `NewBroadcaster` closes over this node's
identity and its peer list; the returned closure sends to every peer,
retrying a failing peer with a delay that doubles each attempt (capped) plus
jitter, before moving on. `send`, `sleep`, and `jitter` are all injected, so
the retry and backoff timing is asserted exactly, with no real network call
and no real time passing.

## What you'll build

```text
gossip-broadcast/            independent module: example.com/gossip-broadcast
  go.mod                      go 1.24
  gossip.go                   NewBroadcaster returns func(msg string) map[string]error
  cmd/
    demo/
      main.go                  one peer ok, one recovers, one always fails
  gossip_test.go               table test: retry+succeed, exhaust retries, cap, concurrency
```

- Files: `gossip.go`, `cmd/demo/main.go`, `gossip_test.go`.
- Implement: `NewBroadcaster(self string, peers []string, send func(from, to, msg string) error, sleep func(time.Duration), jitter func() time.Duration, maxAttempts int, baseDelay, maxDelay time.Duration) func(msg string) map[string]error`.
- Test: a peer that fails twice then succeeds is retried exactly that many times with doubling delays; a peer that always fails exhausts `maxAttempts` and reports the final error with no trailing sleep after the last attempt; delays never exceed `maxDelay` even after many doublings; many goroutines calling the same broadcaster concurrently never interfere under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/04-first-class-functions-and-closures/30-gossip-broadcast-with-exponential-backoff/cmd/demo
cd go-solutions/04-functions/04-first-class-functions-and-closures/30-gossip-broadcast-with-exponential-backoff
go mod edit -go=1.24
```

### Per-peer backoff, not a global one

`NewBroadcaster` closes over `self`, `peers`, and the three injected
collaborators; the returned closure resets a *fresh* `delay` variable to
`baseDelay` at the start of each peer's loop, so one peer's retry history
never bleeds into the next peer's. For each peer it calls `send` up to
`maxAttempts` times; on any error but the last attempt it calls
`sleep(delay + jitter())` and then doubles `delay`, capping it at
`maxDelay`. The final attempt's result — success or the last error — is
recorded in the returned `map[string]error`, one entry per peer, so a caller
can see exactly which peers accepted the update and which never did.

Because `send`, `sleep`, and `jitter` are parameters, a test can make a peer
fail exactly twice then succeed and assert the recorded delays are `[base,
2*base]` with no third sleep (the loop stops retrying once it succeeds), and
a peer that always fails produces `maxAttempts - 1` sleeps, never
`maxAttempts` — there is no reason to back off after the final, exhausted
attempt. None of this touches a real clock or a real socket.

Create `gossip.go`:

```go
// Package gossip implements a peer-to-peer broadcast with per-peer
// exponential backoff retry, the pattern gossip protocols use to propagate
// an update to every node without a central coordinator.
package gossip

import "time"

// NewBroadcaster returns a closure over this node's identity and its peer
// list. Calling it broadcasts msg to every peer, retrying a peer that
// returns an error up to maxAttempts times with a delay that doubles from
// baseDelay each retry (capped at maxDelay) plus jitter, before giving up on
// that peer and moving to the next one. send, sleep, and jitter are
// injected so tests never touch a real network or a real clock.
func NewBroadcaster(
	self string,
	peers []string,
	send func(from, to, msg string) error,
	sleep func(time.Duration),
	jitter func() time.Duration,
	maxAttempts int,
	baseDelay, maxDelay time.Duration,
) func(msg string) map[string]error {
	return func(msg string) map[string]error {
		results := make(map[string]error, len(peers))
		for _, peer := range peers {
			delay := baseDelay
			var lastErr error
			for attempt := 1; attempt <= maxAttempts; attempt++ {
				lastErr = send(self, peer, msg)
				if lastErr == nil {
					break
				}
				if attempt < maxAttempts {
					sleep(delay + jitter())
					delay *= 2
					if delay > maxDelay {
						delay = maxDelay
					}
				}
			}
			results[peer] = lastErr
		}
		return results
	}
}
```

### The runnable demo

`peer-a` succeeds immediately, `peer-b` fails twice before succeeding on the
third attempt, and `peer-c` always fails and exhausts its retries.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/gossip-broadcast"
)

func main() {
	// peer-b fails its first two attempts, then succeeds; peer-c always fails.
	attemptsByPeer := map[string]int{}
	send := func(from, to, msg string) error {
		attemptsByPeer[to]++
		switch to {
		case "peer-a":
			return nil
		case "peer-b":
			if attemptsByPeer[to] < 3 {
				return fmt.Errorf("peer-b: transient error (attempt %d)", attemptsByPeer[to])
			}
			return nil
		case "peer-c":
			return fmt.Errorf("peer-c: unreachable (attempt %d)", attemptsByPeer[to])
		}
		return nil
	}

	sleep := func(d time.Duration) { fmt.Printf("backing off %v\n", d) }
	jitter := func() time.Duration { return 0 } // deterministic demo output

	broadcast := gossip.NewBroadcaster(
		"node-1", []string{"peer-a", "peer-b", "peer-c"},
		send, sleep, jitter,
		3, 100*time.Millisecond, 400*time.Millisecond,
	)

	results := broadcast("state-update-v7")
	for _, peer := range []string{"peer-a", "peer-b", "peer-c"} {
		fmt.Printf("%s: err=%v\n", peer, results[peer])
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
backing off 100ms
backing off 200ms
backing off 100ms
backing off 200ms
peer-a: err=<nil>
peer-b: err=<nil>
peer-c: err=peer-c: unreachable (attempt 3)
```

### Tests

Create `gossip_test.go`:

```go
package gossip

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestBroadcastRetriesThenSucceeds(t *testing.T) {
	var mu sync.Mutex
	attempts := map[string]int{}
	send := func(from, to, msg string) error {
		mu.Lock()
		defer mu.Unlock()
		attempts[to]++
		if to == "peer-flaky" && attempts[to] < 3 {
			return fmt.Errorf("transient failure attempt %d", attempts[to])
		}
		return nil
	}

	var delays []time.Duration
	sleep := func(d time.Duration) { delays = append(delays, d) }
	jitter := func() time.Duration { return 0 }

	broadcast := NewBroadcaster(
		"node-1", []string{"peer-flaky"},
		send, sleep, jitter,
		5, 10*time.Millisecond, 1*time.Second,
	)

	results := broadcast("update")
	if err := results["peer-flaky"]; err != nil {
		t.Fatalf("results[peer-flaky] = %v, want nil (succeeded on 3rd attempt)", err)
	}
	if attempts["peer-flaky"] != 3 {
		t.Fatalf("attempts = %d, want 3", attempts["peer-flaky"])
	}
	// Backoff doubles from baseDelay: 10ms, then 20ms (two retries before success).
	want := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}
	if fmt.Sprint(delays) != fmt.Sprint(want) {
		t.Fatalf("delays = %v, want %v (exponential backoff)", delays, want)
	}
}

func TestBroadcastGivesUpAfterMaxAttempts(t *testing.T) {
	permanent := errors.New("peer permanently down")
	send := func(from, to, msg string) error { return permanent }

	var delays []time.Duration
	sleep := func(d time.Duration) { delays = append(delays, d) }
	jitter := func() time.Duration { return 0 }

	broadcast := NewBroadcaster(
		"node-1", []string{"peer-dead"},
		send, sleep, jitter,
		3, 50*time.Millisecond, 1*time.Second,
	)

	results := broadcast("update")
	if !errors.Is(results["peer-dead"], permanent) {
		t.Fatalf("results[peer-dead] = %v, want %v", results["peer-dead"], permanent)
	}
	// 3 attempts means 2 retries (no sleep after the final, exhausted attempt).
	if len(delays) != 2 {
		t.Fatalf("len(delays) = %d, want 2", len(delays))
	}
}

func TestBroadcastDelayIsCappedAtMaxDelay(t *testing.T) {
	send := func(from, to, msg string) error { return errors.New("always fails") }
	var delays []time.Duration
	sleep := func(d time.Duration) { delays = append(delays, d) }
	jitter := func() time.Duration { return 0 }

	broadcast := NewBroadcaster(
		"node-1", []string{"peer-dead"},
		send, sleep, jitter,
		6, 100*time.Millisecond, 300*time.Millisecond,
	)
	broadcast("update")

	// Uncapped doubling would be 100, 200, 400, 800, 1600ms; capped at 300ms
	// it must plateau at 300ms instead of continuing to grow.
	want := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		300 * time.Millisecond,
		300 * time.Millisecond,
		300 * time.Millisecond,
	}
	if fmt.Sprint(delays) != fmt.Sprint(want) {
		t.Fatalf("delays = %v, want %v (capped at maxDelay)", delays, want)
	}
}

func TestBroadcastConcurrentCallsAreIndependent(t *testing.T) {
	send := func(from, to, msg string) error { return nil }
	sleep := func(time.Duration) {}
	jitter := func() time.Duration { return 0 }

	broadcast := NewBroadcaster(
		"node-1", []string{"peer-a", "peer-b", "peer-c"},
		send, sleep, jitter,
		3, time.Millisecond, 10*time.Millisecond,
	)

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results := broadcast("concurrent-update")
			for peer, err := range results {
				if err != nil {
					t.Errorf("results[%s] = %v, want nil", peer, err)
				}
			}
		}()
	}
	wg.Wait()
}
```

Verify: `go test -count=1 -race ./...`

## Review

The retry-then-succeed test pins the exact backoff sequence for a
recovering peer; the give-up test proves a permanently failing peer stops
after `maxAttempts` and never sleeps after the final attempt; the cap test
proves the doubling plateaus instead of growing unbounded. The concurrency
test is the structural guarantee this whole lesson relies on: `broadcast` is
a pure function of its captured, read-only `peers` and collaborators — it
allocates a fresh `results` map and a fresh `delay` per call — so twenty
goroutines calling the same broadcaster simultaneously never share mutable
state and the race detector stays quiet.

## Resources

- [AWS Builders' Library: Timeouts, retries, and backoff with jitter](https://aws.amazon.com/builders-library/timeouts-retries-and-backoff-with-jitter/) — the exponential-backoff-with-jitter strategy this exercise implements.
- [pkg.go.dev: time.Duration](https://pkg.go.dev/time#Duration) — the type the injected `sleep`/`jitter` closures operate on.
- [Wikipedia: Gossip protocol](https://en.wikipedia.org/wiki/Gossip_protocol) — the peer-to-peer broadcast pattern this broadcaster models.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [29-write-ahead-log-sequential-replay.md](29-write-ahead-log-sequential-replay.md) | Next: [31-connection-pool-fast-fail-when-exhausted.md](31-connection-pool-fast-fail-when-exhausted.md)
