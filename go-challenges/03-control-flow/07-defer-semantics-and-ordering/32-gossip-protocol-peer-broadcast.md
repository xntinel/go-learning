# Exercise 32: Gossip Protocol — Deferred Batch Flush of Peer Updates Under Fanout

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Gossip-based membership protocols — the family SWIM, Serf, and
Cassandra's ring gossip belong to — disseminate state by having each
node periodically send whatever it has learned to a small, bounded
subset of peers (the fanout) rather than broadcasting to everyone: with
enough rounds, information reaches the whole cluster without any single
node paying the bandwidth cost of talking to all of it at once. A
realistic node buffers updates from several local sources between
rounds and, at the end of a round, must flush that buffer to its fanout
targets concurrently — round latency is bounded by the slowest peer, not
the sum of all of them — while making sure a peer that is temporarily
unreachable does not stop the others from receiving their copy, and
does not get silently swallowed either. This module builds that flush
and the round wrapper that defers it. The module is fully
self-contained: its own `go mod init`, all code inline, its own demo and
tests.

## What you'll build

```text
gossip/                       independent module: example.com/gossip-protocol-peer-broadcast
  go.mod                       go 1.24
  gossip.go                     Update, Transport, Node (AddUpdate, FlushToFanout), ProcessRound
  cmd/
    demo/
      main.go                  runnable demo: 3 concurrent producers, fanout of 2, one peer unreachable
  gossip_test.go                error-joining under concurrent fanout sends (-race); buffer-drain and round-error cases
```

- Files: `gossip.go`, `cmd/demo/main.go`, `gossip_test.go`.
- Implement: `Node` (`AddUpdate`, `FlushToFanout`) and `ProcessRound(n *Node, fanout int, send Transport, fn func() error) (err error)`.
- Test: a concurrent-fanout case proving every peer's error is joined into the result, plus buffer-drain and round-error-joining cases.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/gossip-protocol-peer-broadcast/cmd/demo
cd ~/go-exercises/gossip-protocol-peer-broadcast
go mod init example.com/gossip-protocol-peer-broadcast
go mod edit -go=1.24
```

### Why the error has to be read inside the same deferred closure that waits

`FlushToFanout` sends to every fanout peer concurrently, each send in
its own goroutine, and each failing send appends to a shared `errs`
slice under a mutex. The tempting shortcut is a plain `defer
wg.Wait()` followed by `return errors.Join(errs...)` as the function's
last statement — but that is a bug, and it is exactly the kind of
argument-evaluation-timing bug this lesson's concepts warn about,
transplanted from arguments onto return values: Go evaluates a
function's return expression *before* running its deferred calls, so
`errors.Join(errs...)` would read `errs` while some of the fanout
goroutines that still append to it are potentially still running,
racing under `-race` and, worse, sometimes returning an incomplete
error set even without a detected race. The fix is the same shape as a
deferred rollback that reads a named `err`: put both the `wg.Wait()` and
the `errors.Join(errs...)` read inside one deferred closure that writes
to a *named* return `err`. That closure runs at the function's true
return, after every send goroutine has definitely finished, so the
`errs` slice it reads is complete and stable.

`ProcessRound` layers the lesson's now-familiar named-return-plus-defer
idiom on top: it defers exactly one call to `FlushToFanout`, so the
round's buffered updates go out to peers on every exit from `fn` —
success or failure — and joins any flush failure onto whatever error
`fn` itself returned, so a caller sees both problems instead of only
the first one encountered.

Create `gossip.go`:

```go
package gossip

import (
	"errors"
	"fmt"
	"sync"
)

// Update is one piece of membership/state info a peer wants to
// disseminate.
type Update struct {
	Peer  string
	Value string
}

// Transport delivers a batch of updates to one peer.
type Transport func(peer string, updates []Update) error

// Node buffers incoming updates from concurrent local producers and, when
// FlushToFanout runs, sends the whole buffer to a fanout subset of known
// peers.
type Node struct {
	peers []string

	mu     sync.Mutex
	buffer []Update
}

// NewNode returns a Node aware of the given peer set.
func NewNode(peers []string) *Node {
	return &Node{peers: append([]string(nil), peers...)}
}

// AddUpdate buffers one update; safe for concurrent callers.
func (n *Node) AddUpdate(u Update) {
	n.mu.Lock()
	n.buffer = append(n.buffer, u)
	n.mu.Unlock()
}

// FlushToFanout drains the buffer and sends the drained batch, concurrently,
// to the first fanout peers of the node's configured peer set. It returns
// nil if there was nothing to send, or the joined errors from every peer
// that failed to receive the batch.
//
// The deferred closure both waits for every fanout goroutine to finish and
// only then reads errs -- reading errs before every sender has finished
// (for example, by joining it as a plain return expression evaluated
// before a separately deferred Wait) would race against goroutines still
// appending to it. Requiring the Wait to happen inside the same deferred
// closure that produces the final err is what keeps this safe under -race.
func (n *Node) FlushToFanout(fanout int, send Transport) (err error) {
	n.mu.Lock()
	batch := n.buffer
	n.buffer = nil
	n.mu.Unlock()

	if len(batch) == 0 {
		return nil
	}

	targets := n.peers
	if fanout < len(targets) {
		targets = targets[:fanout]
	}

	var (
		mu   sync.Mutex
		errs []error
		wg   sync.WaitGroup
	)

	defer func() {
		wg.Wait()
		err = errors.Join(errs...)
	}()

	for _, peer := range targets {
		wg.Add(1)
		go func(peer string) {
			defer wg.Done()
			if sendErr := send(peer, batch); sendErr != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("peer %s: %w", peer, sendErr))
				mu.Unlock()
			}
		}(peer)
	}
	return nil
}

// ProcessRound runs fn -- which typically buffers updates via concurrent
// calls to AddUpdate -- and defers exactly one flush of the round's
// buffered updates to peers, regardless of how fn exits. Any error fn
// returns and any error the flush produces are both joined into the final
// result, so a caller sees every failure this round experienced instead of
// only the first one.
func ProcessRound(n *Node, fanout int, send Transport, fn func() error) (err error) {
	defer func() {
		if ferr := n.FlushToFanout(fanout, send); ferr != nil {
			err = errors.Join(err, ferr)
		}
	}()
	return fn()
}
```

### The runnable demo

Three local goroutines add updates concurrently during a round; the
node's fanout is 2, so only the first two of its three configured peers
(`peer-a`, `peer-b`) get this round's batch — `peer-c` is outside the
fanout entirely. `peer-b` reports unreachable; `peer-a` succeeds.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"sync"

	"example.com/gossip-protocol-peer-broadcast"
)

func main() {
	node := gossip.NewNode([]string{"peer-a", "peer-b", "peer-c"})

	send := func(peer string, updates []gossip.Update) error {
		if peer == "peer-b" {
			return errors.New("unreachable")
		}
		fmt.Printf("sent %d updates to %s\n", len(updates), peer)
		return nil
	}

	err := gossip.ProcessRound(node, 2, send, func() error {
		var wg sync.WaitGroup
		for i, id := range []string{"node-1", "node-2", "node-3"} {
			wg.Add(1)
			go func(i int, id string) {
				defer wg.Done()
				node.AddUpdate(gossip.Update{Peer: id, Value: fmt.Sprintf("state-%d", i)})
			}(i, id)
		}
		wg.Wait()
		return nil
	})

	fmt.Println("round finished with:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
sent 3 updates to peer-a
round finished with: peer peer-b: unreachable
```

### Tests

`TestFlushToFanoutWaitsForAllSendsBeforeJoiningErrors` sends to four
peers concurrently, two of which fail, and confirms both failures are
present in the joined error — run under `-race`, this is what actually
exercises the wait-then-read ordering the deferred closure enforces.
`TestFlushToFanoutDrainsBufferExactlyOnce` and
`TestConcurrentAddUpdateThenFlushSeesEveryUpdate` cover the buffer's
drain-once and concurrent-producer behavior.
`TestProcessRoundJoinsRoundErrorWithFlushError` proves the round wrapper
merges a round-level failure with a flush-level one instead of one
shadowing the other.

Create `gossip_test.go`:

```go
package gossip

import (
	"errors"
	"sync"
	"testing"
)

func TestFlushToFanoutWaitsForAllSendsBeforeJoiningErrors(t *testing.T) {
	n := NewNode([]string{"p1", "p2", "p3", "p4"})
	for i := 0; i < 10; i++ {
		n.AddUpdate(Update{Peer: "local", Value: "v"})
	}

	errP2 := errors.New("p2 down")
	errP4 := errors.New("p4 down")
	send := func(peer string, updates []Update) error {
		switch peer {
		case "p2":
			return errP2
		case "p4":
			return errP4
		default:
			return nil
		}
	}

	err := n.FlushToFanout(4, send)
	if !errors.Is(err, errP2) || !errors.Is(err, errP4) {
		t.Fatalf("err = %v, want it to wrap both p2 and p4 failures", err)
	}
}

func TestFlushToFanoutDrainsBufferExactlyOnce(t *testing.T) {
	n := NewNode([]string{"p1"})
	n.AddUpdate(Update{Peer: "x", Value: "1"})

	var mu sync.Mutex
	var calls int
	send := func(peer string, updates []Update) error {
		mu.Lock()
		calls++
		mu.Unlock()
		if len(updates) != 1 {
			t.Errorf("got %d updates, want 1", len(updates))
		}
		return nil
	}

	if err := n.FlushToFanout(1, send); err != nil {
		t.Fatalf("first flush: %v", err)
	}
	if err := n.FlushToFanout(1, send); err != nil {
		t.Fatalf("second flush: %v", err)
	}
	if calls != 1 {
		t.Fatalf("send called %d times, want 1 (second flush had nothing pending)", calls)
	}
}

func TestConcurrentAddUpdateThenFlushSeesEveryUpdate(t *testing.T) {
	n := NewNode([]string{"p1"})

	const producers = 20
	var wg sync.WaitGroup
	for i := 0; i < producers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			n.AddUpdate(Update{Peer: "x", Value: "v"})
		}(i)
	}
	wg.Wait()

	var got int
	send := func(peer string, updates []Update) error {
		got = len(updates)
		return nil
	}
	if err := n.FlushToFanout(1, send); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got != producers {
		t.Fatalf("flushed %d updates, want %d", got, producers)
	}
}

func TestProcessRoundJoinsRoundErrorWithFlushError(t *testing.T) {
	n := NewNode([]string{"p1"})
	n.AddUpdate(Update{Peer: "x", Value: "v"})

	roundErr := errors.New("round failed")
	sendErr := errors.New("send failed")
	send := func(peer string, updates []Update) error { return sendErr }

	err := ProcessRound(n, 1, send, func() error { return roundErr })
	if !errors.Is(err, roundErr) || !errors.Is(err, sendErr) {
		t.Fatalf("err = %v, want it to wrap both %v and %v", err, roundErr, sendErr)
	}
}
```

Verify: `go test -count=1 -race ./...`

## Review

`FlushToFanout` is correct when the error it returns always accounts for
every fanout peer that failed, never a partial subset determined by
scheduling luck. Putting both `wg.Wait()` and the `errors.Join` read
inside the same deferred closure, writing to a named return, is what
enforces that — the closure cannot execute until every sender goroutine
has finished, because it explicitly waits for them first. The mistake
this design avoids is the more "obvious"-looking version — a separately
deferred `wg.Wait()` alongside a plain `return errors.Join(errs...)`
statement — which looks correct at a glance but is actually broken: Go
evaluates a return statement's expression before running any deferred
calls, so that `errors.Join` would run first, against a slice that
concurrent goroutines might still be appending to.

## Resources

- [errors.Join](https://pkg.go.dev/errors#Join) — combining every fanout peer's failure into one error `errors.Is` can still inspect individually.
- [Go Specification: Return statements](https://go.dev/ref/spec#Return_statements) — for a function with named results, `return` sets the result parameters before deferred functions execute.
- [The SWIM Membership Protocol](https://www.cs.cornell.edu/projects/Quicksilver/public_pdfs/SWIM.pdf) — the fanout-based dissemination model this exercise's `FlushToFanout` mirrors.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [31-mvcc-snapshot-isolation-transaction.md](31-mvcc-snapshot-isolation-transaction.md) | Next: [33-bloom-filter-dynamic-hashfunc-finalization.md](33-bloom-filter-dynamic-hashfunc-finalization.md)
