# Exercise 34: Coordinated two-phase commit (prepare + commit phases)

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Two-phase commit is how a coordinator gets several independent participants
— separate services, separate databases, separate shards — to agree on a
single atomic outcome for one transaction: every participant votes to
commit or abort during a prepare phase, and the coordinator decides the
final outcome only once it has heard from everyone, or given up waiting.
The one rule that must never bend is the safe default under uncertainty: if
any participant votes to abort, or fails to respond before a deadline, the
whole transaction aborts — a coordinator that commits on an optimistic
guess can leave one participant durably committed while another rolled
back, which is exactly the split-brain outcome two-phase commit exists to
prevent. This module is fully self-contained: its own `go mod init`, all
code inline, its own demo and tests.

## What you'll build

```text
twopc/                      independent module: example.com/twopc
  go.mod                     go 1.24
  twopc.go                     Vote, Participant, Outcome; Coordinate, Broadcast
  cmd/
    demo/
      main.go                runnable demo: unanimous commit, one dissenter, an unreachable participant
  twopc_test.go                table-style tests: all-commit, one abort, no participants, deadline timeout, straggler-only timeout, duplicate-vote handling, broadcast
```

- Files: `twopc.go`, `cmd/demo/main.go`, `twopc_test.go`.
- Implement: `Coordinate(txnID, participants, deadline) Outcome`, launching every participant's prepare logic concurrently and collecting votes until quorum is decided, one abort arrives, or the deadline fires; `Broadcast(participants, outcome, notify)` for the commit/abort phase.
- Test: unanimous commit, one abort vote aborting the whole transaction, an empty participant set, a deadline aborting unresponsive participants, a deadline that only ever times out the actual stragglers, a duplicate vote being recorded without being double-counted, and broadcast reaching every participant.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/06-labels-break-continue-goto/34-two-phase-commit-coordinator/cmd/demo
cd go-solutions/03-control-flow/06-labels-break-continue-goto/34-two-phase-commit-coordinator
go mod edit -go=1.24
```

### Why the collection loop needs both a labeled break and a labeled continue

`Coordinate` starts every participant concurrently and then runs a single
`for`-`select` — the collection loop — racing three outcomes against each
other: a vote arrives, the deadline fires, or every distinct participant has
now voted. This is the same for-select shape this entire chapter is built
around, and both jump statements inside it exist for a reason specific to
two-phase commit, not just style.

`continue collect`, in the duplicate-vote branch, states explicitly which
loop resumes when a participant's retried vote arrives: the OUTER collection
loop, not the `select` statement the `continue` is textually written inside
of. A retried vote (a participant that assumed its first attempt was lost
to the network and resent) must not be double-counted toward quorum, and
must not reset any decision already made — it is simply noise to record and
ignore, waiting for whichever participants have genuinely not voted yet.

`break collect`, in the single-abort-vote branch and the deadline branch,
is not optional: a bare `break` in either spot would leave only the
`select`, and the `for` would loop right back into it, waiting for votes
from participants whose votes can no longer change an outcome that is
already decided — one abort vote is final, and a fired deadline is final.
Continuing to wait past either of those wastes exactly the time two-phase
commit is meant to bound, and in a real system extends how long every
participant sits holding a local lock for a transaction that has already
been decided.

Create `twopc.go`:

```go
package twopc

// Vote is one participant's response to a Prepare request for a
// transaction.
type Vote struct {
	Participant string
	Commit      bool
}

// Participant runs its own prepare logic (validating local constraints,
// acquiring local locks) and sends exactly one Vote on votes if it
// responds -- or never sends anything if it is unreachable or too slow,
// which the coordinator must not wait forever for.
type Participant func(txnID string, votes chan<- Vote)

// Outcome is the coordinator's final decision for a transaction.
type Outcome struct {
	Commit    bool
	TimedOut  []string // participants that never voted before the deadline
	Duplicate []string // participants whose vote arrived more than once
}

// Coordinate runs the prepare phase of two-phase commit: it starts every
// participant's Prepare concurrently, then collects votes until either
// every participant has voted exactly once, a single abort vote arrives (no
// point waiting for the rest -- one abort already decides the outcome), or
// deadline fires before all votes are in. A duplicate vote (a participant
// retrying after what it believed was a lost network call) is recorded and
// ignored rather than double-counted.
func Coordinate(txnID string, participants map[string]Participant, deadline <-chan struct{}) Outcome {
	votes := make(chan Vote, len(participants))
	for _, p := range participants {
		go p(txnID, votes)
	}

	voted := make(map[string]bool, len(participants))
	out := Outcome{Commit: true}

collect:
	for len(voted) < len(participants) {
		select {
		case v := <-votes:
			if voted[v.Participant] {
				// A retried vote from a participant that already
				// responded: ignore it and keep waiting for whichever
				// participants have not voted yet. continue collect states
				// explicitly which loop this resumes -- the OUTER collect
				// loop, not the select statement it is written inside of.
				out.Duplicate = append(out.Duplicate, v.Participant)
				continue collect
			}
			voted[v.Participant] = true
			if !v.Commit {
				// One abort vote already decides the whole transaction:
				// waiting for the remaining participants cannot change the
				// outcome, only delay it. break collect leaves the ENTIRE
				// collection loop, not just this select -- a bare break
				// here would only leave the select, and the for would loop
				// back around waiting for votes nobody needs to send.
				out.Commit = false
				break collect
			}
		case <-deadline:
			// Whichever participants have not voted yet are presumed
			// unreachable; two-phase commit's safe default under
			// uncertainty is to abort, never to commit on a guess.
			out.Commit = false
			for name := range participants {
				if !voted[name] {
					out.TimedOut = append(out.TimedOut, name)
				}
			}
			break collect
		}
	}
	return out
}

// Broadcast notifies every participant of the final outcome by calling
// notify for each one. A real coordinator sends a Commit or Abort message
// over the network here and retries any participant that does not
// acknowledge -- a retry loop with the exact same for-select shape as
// Coordinate's collection phase -- but phase two is kept deliberately
// simple in this exercise so the labeled control flow above stays the
// focus.
func Broadcast(participants map[string]Participant, outcome Outcome, notify func(name string, commit bool)) {
	for name := range participants {
		notify(name, outcome.Commit)
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sort"

	"example.com/twopc"
)

func vote(name string, commit bool) twopc.Participant {
	return func(txnID string, votes chan<- twopc.Vote) {
		votes <- twopc.Vote{Participant: name, Commit: commit}
	}
}

func neverVotes(name string) twopc.Participant {
	return func(txnID string, votes chan<- twopc.Vote) {
		// Simulates an unreachable or permanently stalled participant.
	}
}

func printOutcome(label string, out twopc.Outcome) {
	timedOut := append([]string{}, out.TimedOut...)
	sort.Strings(timedOut)
	fmt.Printf("%s: commit=%v timedOut=%v duplicates=%v\n", label, out.Commit, timedOut, out.Duplicate)
}

func main() {
	// Scenario 1: every participant votes to commit.
	allCommit := map[string]twopc.Participant{
		"inventory": vote("inventory", true),
		"billing":   vote("billing", true),
		"shipping":  vote("shipping", true),
	}
	printOutcome("all commit", twopc.Coordinate("txn-1", allCommit, make(chan struct{})))

	// Scenario 2: one participant votes to abort; the transaction aborts
	// regardless of what the other two decide.
	oneAborts := map[string]twopc.Participant{
		"inventory": vote("inventory", true),
		"billing":   vote("billing", false),
		"shipping":  vote("shipping", true),
	}
	printOutcome("one aborts", twopc.Coordinate("txn-2", oneAborts, make(chan struct{})))

	// Scenario 3: a participant never responds at all; the deadline (closed
	// immediately, since nothing will ever arrive on votes) forces an abort.
	unreachable := map[string]twopc.Participant{
		"inventory": neverVotes("inventory"),
		"billing":   neverVotes("billing"),
	}
	deadline := make(chan struct{})
	close(deadline)
	printOutcome("unreachable participant", twopc.Coordinate("txn-3", unreachable, deadline))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
all commit: commit=true timedOut=[] duplicates=[]
one aborts: commit=false timedOut=[] duplicates=[]
unreachable participant: commit=false timedOut=[billing inventory] duplicates=[]
```

The third scenario's participants never send anything at all, so the only
statement in the `select` that can ever become ready is the already-closed
`deadline` — both `billing` and `inventory` are reported as timed out,
deterministically, every time.

### Tests

`TestCoordinateAllCommit`, `TestCoordinateOneAbortVoteAbortsTheWholeTransaction`,
and `TestCoordinateNoParticipants` cover the ordinary outcomes.
`TestCoordinateDeadlineAbortsUnresponsiveParticipants` and
`TestCoordinateDeadlineOnlyTimesOutStragglers` cover the timeout path, the
second one deliberately racing a real vote against an already-fired
deadline and asserting only the invariant that holds regardless of which
one the `select` happens to observe first: the straggler is always reported,
the fast participant may or may not be. `TestCoordinateDuplicateVoteIsIgnoredNotDoubleCounted`
uses an explicit channel-close synchronization (never a sleep) to
deterministically guarantee a duplicate vote is observed before the
transaction resolves. `TestBroadcastNotifiesEveryParticipant` covers phase
two.

Create `twopc_test.go`:

```go
package twopc

import (
	"slices"
	"sort"
	"testing"
)

func vote(name string, commit bool) Participant {
	return func(txnID string, votes chan<- Vote) {
		votes <- Vote{Participant: name, Commit: commit}
	}
}

func neverVotes() Participant {
	return func(txnID string, votes chan<- Vote) {}
}

func TestCoordinateAllCommit(t *testing.T) {
	t.Parallel()

	participants := map[string]Participant{
		"a": vote("a", true),
		"b": vote("b", true),
		"c": vote("c", true),
	}
	out := Coordinate("txn", participants, make(chan struct{}))
	if !out.Commit {
		t.Fatalf("Commit = false, want true when every participant votes to commit")
	}
	if len(out.TimedOut) != 0 || len(out.Duplicate) != 0 {
		t.Fatalf("unexpected TimedOut=%v Duplicate=%v", out.TimedOut, out.Duplicate)
	}
}

func TestCoordinateOneAbortVoteAbortsTheWholeTransaction(t *testing.T) {
	t.Parallel()

	participants := map[string]Participant{
		"a": vote("a", true),
		"b": vote("b", false),
		"c": vote("c", true),
	}
	out := Coordinate("txn", participants, make(chan struct{}))
	if out.Commit {
		t.Fatal("Commit = true, want false when any participant votes to abort")
	}
}

func TestCoordinateNoParticipants(t *testing.T) {
	t.Parallel()

	out := Coordinate("txn", map[string]Participant{}, make(chan struct{}))
	if !out.Commit {
		t.Fatal("an empty participant set should trivially commit")
	}
}

func TestCoordinateDeadlineAbortsUnresponsiveParticipants(t *testing.T) {
	t.Parallel()

	participants := map[string]Participant{
		"a": neverVotes(),
		"b": neverVotes(),
	}
	deadline := make(chan struct{})
	close(deadline) // already expired: nothing will ever arrive on votes

	out := Coordinate("txn", participants, deadline)
	if out.Commit {
		t.Fatal("Commit = true, want false when the deadline fires before any vote arrives")
	}
	want := []string{"a", "b"}
	got := append([]string{}, out.TimedOut...)
	sort.Strings(got)
	if !slices.Equal(got, want) {
		t.Fatalf("TimedOut = %v, want %v", got, want)
	}
}

func TestCoordinateDeadlineOnlyTimesOutStragglers(t *testing.T) {
	t.Parallel()

	participants := map[string]Participant{
		"fast": vote("fast", true),
		"slow": neverVotes(),
	}
	deadline := make(chan struct{})
	close(deadline)

	out := Coordinate("txn", participants, deadline)
	if out.Commit {
		t.Fatal("Commit = true, want false: not every participant voted before the deadline")
	}
	// "fast" may or may not have been drained before the deadline fired
	// (both channels are ready), but "slow" can never have voted, so it
	// must always be reported.
	if !slices.Contains(out.TimedOut, "slow") {
		t.Fatalf("TimedOut = %v, want it to contain %q", out.TimedOut, "slow")
	}
}

func TestCoordinateDuplicateVoteIsIgnoredNotDoubleCounted(t *testing.T) {
	t.Parallel()

	// "b" deterministically waits until "a" has queued BOTH of its votes
	// before sending its own. Because the votes channel is FIFO and both of
	// "a"'s sends complete (buffer capacity covers both) strictly before
	// "b" even attempts to send, the coordinator is guaranteed to observe
	// a's two votes before b's -- no real time or sleep involved, only a
	// channel-close happens-before relationship.
	aSentBoth := make(chan struct{})
	participants := map[string]Participant{
		"a": func(txnID string, votes chan<- Vote) {
			votes <- Vote{Participant: "a", Commit: true}
			votes <- Vote{Participant: "a", Commit: true} // retried vote
			close(aSentBoth)
		},
		"b": func(txnID string, votes chan<- Vote) {
			<-aSentBoth
			votes <- Vote{Participant: "b", Commit: true}
		},
	}
	out := Coordinate("txn", participants, make(chan struct{}))
	if !out.Commit {
		t.Fatal("Commit = false, want true: every distinct participant voted to commit")
	}
	if !slices.Contains(out.Duplicate, "a") {
		t.Fatalf("Duplicate = %v, want it to contain %q", out.Duplicate, "a")
	}
}

func TestBroadcastNotifiesEveryParticipant(t *testing.T) {
	t.Parallel()

	participants := map[string]Participant{
		"a": vote("a", true),
		"b": vote("b", true),
	}
	var notified []string
	Broadcast(participants, Outcome{Commit: true}, func(name string, commit bool) {
		if !commit {
			t.Errorf("notify(%q) commit = false, want true", name)
		}
		notified = append(notified, name)
	})
	sort.Strings(notified)
	if !slices.Equal(notified, []string{"a", "b"}) {
		t.Fatalf("notified = %v, want [a b]", notified)
	}
}
```

Verify:

```bash
go test -count=1 -race ./...
```

## Review

`Coordinate` is correct when a single abort vote or a fired deadline stops
the collection loop immediately rather than waiting out the remaining
participants — the bug this exercise guards against is a bare `break` in
either branch, which would leave only the `select` and spin the `for` back
around waiting on votes that can no longer matter. The duplicate-vote test
is the one that proves `continue collect` is doing real work: without it
(or with a bare `continue`, which happens to behave the same way here since
this `for` is the only enclosing loop), a naively restructured version that
tried to `break` on ANY vote received would double-count a retried vote
toward quorum. The deadline-only-times-out-stragglers test pins a subtlety
worth internalizing: the coordinator's behavior is deterministic in its
*outcome* (abort, and the true straggler is always named) even though the
exact interleaving between a real vote and an already-fired deadline is
not — which is exactly the kind of assertion a concurrent test should make.

## Resources

- [Consensus on Transaction Commit (Gray & Lamport, 2004)](https://www.microsoft.com/en-us/research/publication/consensus-on-transaction-commit/) — two-phase commit's blocking problem and how consensus-based commit protocols address it.
- [Go Specification: Select statements](https://go.dev/ref/spec#Select_statements) — a bare `break` inside a `select` case leaves only the `select`.
- [The Go Memory Model: channel communication](https://go.dev/ref/mem#tmp_7) — the happens-before guarantees a closed channel provides, used to make the duplicate-vote test deterministic.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [33-gossip-protocol-eventual-consistency.md](33-gossip-protocol-eventual-consistency.md) | Next: [../07-defer-semantics-and-ordering/00-concepts.md](../07-defer-semantics-and-ordering/00-concepts.md)
