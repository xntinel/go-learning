# 15. Paxos Consensus

Paxos is the foundational distributed consensus algorithm. Unlike Raft, which was designed for understandability, Paxos was designed for correctness proofs and generality. The result is a protocol that is harder to understand but deeply instructive: understanding exactly why each message is sent — and why nothing weaker would be safe — clarifies what consensus really requires.

This lesson implements Single-Decree Paxos (agreement on one value) using the classic two-phase Prepare/Promise and Accept/Accepted protocol. All roles communicate through direct synchronous method calls on in-process Acceptor values; acceptor state is guarded by a mutex so the acceptors are safe for concurrent use. The implementation exposes the safety property (no two learners ever see different values) and the liveness hazard (dueling proposers).

```text
paxos/
  go.mod
  paxos.go          -- Proposer, Acceptor, Learner + all message types
  paxos_test.go     -- table-driven correctness tests
  cmd/demo/main.go  -- runnable demonstration
```

## Concepts

### The Safety Problem Paxos Solves

A distributed system achieves consensus when a set of nodes all agree on the same value and that agreement is permanent. Two hard constraints apply:

- **Safety**: At most one value is ever chosen. No two nodes commit different values.
- **Validity**: The chosen value must be one that was actually proposed.
- **Termination** (liveness): Eventually some value is chosen (requires additional assumptions like a leader or bounded message delays).

Paxos guarantees safety unconditionally. Liveness is conditional and is handled separately (typically by electing a distinguished proposer).

### The Two Roles That Matter: Proposer and Acceptor

A **proposer** wants a value to be chosen. It drives the protocol by contacting a quorum of acceptors.

An **acceptor** is the durable part of the system. Acceptors promise to respect constraints and persist their state across crashes. The key invariant is:

> Once an acceptor promises proposal number n, it will never accept a proposal numbered less than n.

A **learner** observes accepted messages and learns the chosen value once a quorum of acceptors have accepted the same proposal number.

### Phase 1 — Prepare and Promise

A proposer with proposal number `n` sends `Prepare(n)` to all acceptors. Each acceptor responds with `Promise(n, highestAccepted, acceptedValue)` if `n` is higher than any proposal number it has already promised. If the acceptor has already accepted a value, it includes that value in the promise.

The proposer must receive promises from a **majority quorum** (more than half the acceptors) before proceeding.

### Phase 2 — Accept and Accepted

If any promise contains an already-accepted value, the proposer **must** adopt the highest-numbered accepted value (not its own). This is the key safety mechanism: if a previous round partially committed a value, this round continues it rather than overwriting it.

The proposer then sends `Accept(n, v)` to all acceptors. An acceptor accepts if it has not promised a higher proposal number since the Prepare phase.

The value is **chosen** when a quorum of acceptors have accepted the same proposal number.

### Proposal Numbers and Uniqueness

Proposal numbers must be totally ordered and unique across all proposers. A common scheme is `(round, proposerID)` with lexicographic ordering. The `(round, proposerID)` pair prevents two proposers in the same round from using the same number.

### Why Dueling Proposers Are a Liveness Problem, Not a Safety Problem

Two proposers can livelock: A sends Prepare(1), B sends Prepare(2) aborting A's round, A sends Prepare(3) aborting B's round, and so on. No value is ever chosen. Safety is never violated — the acceptors keep their invariant — but the system makes no progress. The fix is a **distinguished proposer** (leader) that is the only node allowed to propose, elected by a separate mechanism.

### Multi-Paxos: Extending to a Log

Single-Decree Paxos agrees on one value. Multi-Paxos runs one Paxos instance per log slot. The leader optimization: once a leader completes Phase 1 with a given proposal number for a slot, it can skip Phase 1 for subsequent slots (using the same proposal number) and go directly to Phase 2. This makes steady-state latency one round-trip.

## Exercises

This is a library, not a program: the Paxos logic lives in `package paxos`. Verify it with `go test`.

### Exercise 1: Message Types and Acceptor State

Create `paxos.go`:

```go
// paxos.go
package paxos

import (
	"errors"
	"fmt"
	"sync"
)

// ErrQuorumNotReached is returned when the proposer cannot collect a majority.
var ErrQuorumNotReached = errors.New("paxos: quorum not reached")

// ErrRejected is returned when an acceptor rejects a proposal because it has
// already promised a higher proposal number.
var ErrRejected = errors.New("paxos: proposal rejected by acceptor")

// ProposalID is a totally ordered, globally unique proposal identifier.
// Ordering is (Round, ProposerID) lexicographically: higher Round wins; ties
// broken by ProposerID.
type ProposalID struct {
	Round      int
	ProposerID int
}

// Less reports whether p is strictly less than q.
func (p ProposalID) Less(q ProposalID) bool {
	if p.Round != q.Round {
		return p.Round < q.Round
	}
	return p.ProposerID < q.ProposerID
}

// Zero returns true for the zero value, which is never a valid proposal number.
func (p ProposalID) Zero() bool {
	return p.Round == 0 && p.ProposerID == 0
}

// Promise is the Phase-1 response from an Acceptor to a Proposer.
type Promise struct {
	// PromisedID is the proposal number the acceptor is now committing to.
	PromisedID ProposalID
	// AcceptedID is the highest proposal number the acceptor has already
	// accepted, or the zero ProposalID if it has accepted nothing.
	AcceptedID ProposalID
	// AcceptedValue is the value associated with AcceptedID; nil if none.
	AcceptedValue []byte
}

// Accepted is the Phase-2 response from an Acceptor to a Proposer/Learner.
type Accepted struct {
	ProposalID ProposalID
	Value      []byte
}

// Acceptor is a single Paxos acceptor.  It is safe for concurrent use.
type Acceptor struct {
	mu            sync.Mutex
	promisedID    ProposalID // highest promised; zero means none
	acceptedID    ProposalID // highest accepted; zero means none
	acceptedValue []byte
}

// Prepare handles Phase-1 Prepare(n).  It returns a Promise if n is higher
// than any previously promised proposal, or ErrRejected otherwise.
func (a *Acceptor) Prepare(id ProposalID) (Promise, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if !a.promisedID.Zero() && !a.promisedID.Less(id) {
		return Promise{}, fmt.Errorf("%w: promised=%v got=%v", ErrRejected, a.promisedID, id)
	}
	a.promisedID = id
	return Promise{
		PromisedID:    id,
		AcceptedID:    a.acceptedID,
		AcceptedValue: a.acceptedValue,
	}, nil
}

// Accept handles Phase-2 Accept(n, v).  It accepts the proposal if the
// acceptor has not promised a higher proposal since Phase 1.
func (a *Acceptor) Accept(id ProposalID, value []byte) (Accepted, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if !a.promisedID.Zero() && id.Less(a.promisedID) {
		return Accepted{}, fmt.Errorf("%w: promised=%v got=%v", ErrRejected, a.promisedID, id)
	}
	a.acceptedID = id
	a.acceptedValue = value
	return Accepted{ProposalID: id, Value: value}, nil
}

// PromisedID returns the highest proposal number this acceptor has promised.
func (a *Acceptor) PromisedID() ProposalID {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.promisedID
}

// AcceptedValue returns the current accepted value (nil if none).
func (a *Acceptor) AcceptedValue() []byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.acceptedValue
}

// Learner collects Accepted messages and detects when a quorum has accepted
// the same proposal.
type Learner struct {
	mu       sync.Mutex
	quorum   int
	accepted map[ProposalID][]byte // id -> value
	counts   map[ProposalID]int    // id -> accept count
	chosen   []byte
}

// NewLearner creates a Learner that considers a value chosen once quorum
// acceptors have sent Accepted for the same ProposalID.
func NewLearner(quorum int) *Learner {
	return &Learner{
		quorum:   quorum,
		accepted: make(map[ProposalID][]byte),
		counts:   make(map[ProposalID]int),
	}
}

// Observe records one Accepted message.  If this causes a quorum, Chosen
// will return the value from the next call.
func (l *Learner) Observe(a Accepted) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.chosen != nil {
		return
	}
	l.accepted[a.ProposalID] = a.Value
	l.counts[a.ProposalID]++
	if l.counts[a.ProposalID] >= l.quorum {
		l.chosen = a.Value
	}
}

// Chosen returns the chosen value, or nil if no value has been chosen yet.
func (l *Learner) Chosen() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.chosen
}

// Propose runs both phases of Single-Decree Paxos against the given acceptors.
// It returns the value that was chosen (which may differ from proposed if a
// previous round had already partially committed a value).
//
// Propose is not re-entrant: callers that need to retry (e.g., after rejection)
// should call Propose again with a higher proposal ID.
func Propose(id ProposalID, value []byte, acceptors []*Acceptor) ([]byte, error) {
	quorum := len(acceptors)/2 + 1

	// Phase 1: Prepare
	promises := make([]Promise, 0, len(acceptors))
	for _, a := range acceptors {
		p, err := a.Prepare(id)
		if err != nil {
			continue
		}
		promises = append(promises, p)
	}
	if len(promises) < quorum {
		return nil, fmt.Errorf("%w: got %d of %d", ErrQuorumNotReached, len(promises), quorum)
	}

	// If any promise carries an already-accepted value, adopt the one with the
	// highest accepted proposal ID (Paxos safety rule).
	chosen := value
	var highestAccepted ProposalID
	for _, p := range promises {
		if !p.AcceptedID.Zero() && highestAccepted.Less(p.AcceptedID) {
			highestAccepted = p.AcceptedID
			chosen = p.AcceptedValue
		}
	}

	// Phase 2: Accept
	accepted := 0
	for _, a := range acceptors {
		_, err := a.Accept(id, chosen)
		if err != nil {
			continue
		}
		accepted++
	}
	if accepted < quorum {
		return nil, fmt.Errorf("%w: accepted by %d of %d", ErrQuorumNotReached, accepted, quorum)
	}
	return chosen, nil
}
```

The core invariant is in `Acceptor.Accept`: an acceptor accepts only if the proposal ID is at least as high as the one it promised in Phase 1. The `Propose` function enforces the safety rule: if any promise includes an already-accepted value, the proposer adopts the highest-numbered one.

### Exercise 2: Verify Acceptor Safety Invariants Directly

The `Prepare` and `Accept` methods enforce the core Paxos safety invariant: once an acceptor promises proposal number `n`, it must reject any proposal numbered strictly less than `n`, and it must accept any proposal numbered at least `n`. Write a test that exercises both boundaries directly.

Add to `paxos_test.go`:

```go
func TestAcceptorAcceptsHigherProposalAfterPromise(t *testing.T) {
	t.Parallel()

	a := &Acceptor{}
	if _, err := a.Prepare(pid(1, 0)); err != nil {
		t.Fatal(err)
	}
	// Phase-2 with a strictly higher proposal number must be accepted.
	acc, err := a.Accept(pid(2, 0), []byte("newer"))
	if err != nil {
		t.Fatalf("Accept higher id: %v", err)
	}
	if !bytes.Equal(acc.Value, []byte("newer")) {
		t.Fatalf("value = %q, want newer", acc.Value)
	}
}

func TestAcceptorRejectsLowerPhase2AfterHigherPromise(t *testing.T) {
	t.Parallel()

	a := &Acceptor{}
	if _, err := a.Prepare(pid(5, 0)); err != nil {
		t.Fatal(err)
	}
	_, err := a.Accept(pid(3, 0), []byte("stale"))
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("err = %v, want ErrRejected", err)
	}
}
```

### Exercise 3: Table-Driven Tests

Create `paxos_test.go`:

```go
// paxos_test.go
package paxos

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

// helpers

func makeAcceptors(n int) []*Acceptor {
	a := make([]*Acceptor, n)
	for i := range a {
		a[i] = &Acceptor{}
	}
	return a
}

func pid(round, proposer int) ProposalID {
	return ProposalID{Round: round, ProposerID: proposer}
}

// ProposalID ordering

func TestProposalIDLess(t *testing.T) {
	t.Parallel()

	cases := []struct {
		p, q ProposalID
		want bool
	}{
		{pid(1, 0), pid(2, 0), true},
		{pid(2, 0), pid(1, 0), false},
		{pid(1, 0), pid(1, 1), true},
		{pid(1, 1), pid(1, 0), false},
		{pid(1, 1), pid(1, 1), false},
	}
	for _, c := range cases {
		c := c
		t.Run(fmt.Sprintf("%v<%v", c.p, c.q), func(t *testing.T) {
			t.Parallel()
			if got := c.p.Less(c.q); got != c.want {
				t.Fatalf("Less = %v, want %v", got, c.want)
			}
		})
	}
}

// Acceptor Phase 1

func TestAcceptorPreparePersistsPromise(t *testing.T) {
	t.Parallel()

	a := &Acceptor{}
	p, err := a.Prepare(pid(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if p.PromisedID != pid(1, 0) {
		t.Fatalf("PromisedID = %v", p.PromisedID)
	}
	if a.PromisedID() != pid(1, 0) {
		t.Fatal("acceptor did not persist promise")
	}
}

func TestAcceptorRejectsStalePrepare(t *testing.T) {
	t.Parallel()

	a := &Acceptor{}
	if _, err := a.Prepare(pid(2, 0)); err != nil {
		t.Fatal(err)
	}
	_, err := a.Prepare(pid(1, 0))
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("err = %v, want ErrRejected", err)
	}
}

func TestAcceptorIncludesAcceptedValueInPromise(t *testing.T) {
	t.Parallel()

	a := &Acceptor{}
	// Phase 1 round 1
	if _, err := a.Prepare(pid(1, 0)); err != nil {
		t.Fatal(err)
	}
	// Phase 2 round 1
	if _, err := a.Accept(pid(1, 0), []byte("v1")); err != nil {
		t.Fatal(err)
	}
	// Phase 1 round 2 — promise must carry the accepted value
	p, err := a.Prepare(pid(2, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(p.AcceptedValue, []byte("v1")) {
		t.Fatalf("AcceptedValue = %q, want v1", p.AcceptedValue)
	}
	if p.AcceptedID != pid(1, 0) {
		t.Fatalf("AcceptedID = %v, want pid(1,0)", p.AcceptedID)
	}
}

// Acceptor Phase 2

func TestAcceptorAcceptsMatchingProposal(t *testing.T) {
	t.Parallel()

	a := &Acceptor{}
	if _, err := a.Prepare(pid(1, 0)); err != nil {
		t.Fatal(err)
	}
	acc, err := a.Accept(pid(1, 0), []byte("hello"))
	if err != nil {
		t.Fatalf("Accept error = %v", err)
	}
	if !bytes.Equal(acc.Value, []byte("hello")) {
		t.Fatalf("accepted value = %q", acc.Value)
	}
}

func TestAcceptorRejectsLowerProposal(t *testing.T) {
	t.Parallel()

	a := &Acceptor{}
	if _, err := a.Prepare(pid(3, 0)); err != nil {
		t.Fatal(err)
	}
	_, err := a.Accept(pid(1, 0), []byte("old"))
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("err = %v, want ErrRejected", err)
	}
}

// Full single-decree Paxos

func TestProposeConvergesWithSingleProposer(t *testing.T) {
	t.Parallel()

	acceptors := makeAcceptors(3)
	v, err := Propose(pid(1, 0), []byte("value"), acceptors)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(v, []byte("value")) {
		t.Fatalf("chosen = %q, want value", v)
	}
}

func TestProposeAdoptsPreviouslyAcceptedValue(t *testing.T) {
	t.Parallel()

	// Three acceptors; round 1 proposer (ID 0) gets value "old" accepted by a
	// majority.  Round 2 proposer (ID 1) must adopt "old", not its own "new".
	acceptors := makeAcceptors(3)

	// Round 1: value "old" accepted on all three acceptors.
	if _, err := Propose(pid(1, 0), []byte("old"), acceptors); err != nil {
		t.Fatal(err)
	}

	// Round 2: a new proposer tries to impose "new".
	v, err := Propose(pid(2, 1), []byte("new"), acceptors)
	if err != nil {
		t.Fatal(err)
	}
	// Safety: the chosen value must still be "old" because round 1 committed it.
	if !bytes.Equal(v, []byte("old")) {
		t.Fatalf("safety violation: round 2 chose %q, expected old", v)
	}
}

func TestProposeFailsWithoutQuorum(t *testing.T) {
	t.Parallel()

	// Only one acceptor; quorum requires 1 of 1 — always succeeds.
	// Use zero acceptors to trigger failure.
	v, err := Propose(pid(1, 0), []byte("x"), []*Acceptor{})
	if err == nil {
		t.Fatalf("expected error, got %q", v)
	}
	if !errors.Is(err, ErrQuorumNotReached) {
		t.Fatalf("err = %v, want ErrQuorumNotReached", err)
	}
}

func TestLearnerDetectsChosen(t *testing.T) {
	t.Parallel()

	l := NewLearner(2) // quorum = 2
	if l.Chosen() != nil {
		t.Fatal("Chosen before quorum should be nil")
	}
	l.Observe(Accepted{ProposalID: pid(1, 0), Value: []byte("ok")})
	if l.Chosen() != nil {
		t.Fatal("one accept is not quorum")
	}
	l.Observe(Accepted{ProposalID: pid(1, 0), Value: []byte("ok")})
	if !bytes.Equal(l.Chosen(), []byte("ok")) {
		t.Fatalf("Chosen = %q, want ok", l.Chosen())
	}
}

func TestLearnerIgnoresDifferentProposals(t *testing.T) {
	t.Parallel()

	l := NewLearner(2)
	l.Observe(Accepted{ProposalID: pid(1, 0), Value: []byte("a")})
	l.Observe(Accepted{ProposalID: pid(2, 0), Value: []byte("b")})
	// Two different proposals, neither has quorum of 2.
	if l.Chosen() != nil {
		t.Fatal("different proposals should not reach quorum")
	}
}

// Example for auto-verification under go test.

func ExamplePropose() {
	acceptors := makeAcceptors(3)
	v, err := Propose(ProposalID{Round: 1, ProposerID: 0}, []byte("hello"), acceptors)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(string(v))
	// Output: hello
}
```

Your turn: add `TestProposeWithFiveAcceptorsToleratesMinorityFailure` — create five acceptors, call `Propose` against all five, then verify the result is the proposed value. This exercises that a majority of three out of five is sufficient.

### Exercise 4: Runnable Demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"log"

	"example.com/paxos"
)

func main() {
	// Single-Decree Paxos with 5 acceptors.
	acceptors := make([]*paxos.Acceptor, 5)
	for i := range acceptors {
		acceptors[i] = &paxos.Acceptor{}
	}

	id1 := paxos.ProposalID{Round: 1, ProposerID: 0}
	v1, err := paxos.Propose(id1, []byte("first-value"), acceptors)
	if err != nil {
		log.Fatalf("propose round 1: %v", err)
	}
	fmt.Printf("round 1 chose: %s\n", v1)

	// A second proposer in round 2 tries to override; Paxos safety ensures it
	// adopts "first-value" from round 1 instead.
	id2 := paxos.ProposalID{Round: 2, ProposerID: 1}
	v2, err := paxos.Propose(id2, []byte("override"), acceptors)
	if err != nil {
		log.Fatalf("propose round 2: %v", err)
	}
	fmt.Printf("round 2 chose: %s\n", v2)

	// Learner observes the accepted messages.
	learner := paxos.NewLearner(3)
	acc1 := paxos.Accepted{ProposalID: id2, Value: v2}
	learner.Observe(acc1)
	learner.Observe(acc1)
	learner.Observe(acc1)
	fmt.Printf("learner sees: %s\n", learner.Chosen())
}
```

## Common Mistakes

### Accepting Any Proposal in Phase 2 Without Checking the Promised ID

Wrong: the acceptor's `Accept` method accepts unconditionally, ignoring `promisedID`.

What happens: a proposer that was superseded by a higher-round proposer can still force a value through, breaking safety. Two learners may see different values.

Fix: `Accept` must check `id >= promisedID`. The implementation above does this; without it `TestProposeAdoptsPreviouslyAcceptedValue` fails.

### Ignoring Accepted Values in Phase-1 Promises

Wrong: the proposer always proposes its own value, ignoring any `AcceptedValue` in the promises it receives.

What happens: if a previous round partially committed value `v` to a quorum, a new round can override it with a different value, violating safety.

Fix: scan all promises for the highest-numbered accepted value and use that as the proposed value. The `Propose` function does this in the block labeled "Paxos safety rule".

### Using a Non-Unique Proposal Number Scheme

Wrong: using a simple integer counter per proposer without the proposer ID. Two proposers in the same round choose the same number.

What happens: both proposers collect promises for the same number. The one that sends Phase 2 first wins, but which value is committed is a race condition.

Fix: make the proposal ID `(round, proposerID)` with lexicographic ordering so no two proposers can generate the same ID.

### Confusing ErrRejected in Phase 2 With a Protocol Error

Wrong: treating a Phase-2 rejection as a fatal error rather than as a signal to retry with a higher proposal number.

What happens: the proposer gives up. No value is ever chosen.

Fix: catch `ErrRejected` or `ErrQuorumNotReached` from `Propose`, increment the round, and retry. The lesson does not implement retry inside `Propose` (to keep the protocol visible); production code wraps `Propose` in a retry loop with randomized backoff.

## Verification

From `~/go-exercises/paxos`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

All five must succeed. The race detector is important: `Acceptor` uses a mutex; removing it should cause a race failure.

## Summary

- Single-Decree Paxos agrees on one value via two phases: Prepare/Promise (Phase 1) and Accept/Accepted (Phase 2).
- Safety is absolute: once a value is chosen, no subsequent proposer can choose a different value.
- The safety mechanism is in Phase 2: the proposer must adopt the highest-numbered already-accepted value from any promise.
- Proposal IDs must be totally ordered and unique; `(round, proposerID)` achieves this.
- Liveness requires a distinguished proposer to prevent dueling; randomized backoff is the simplest mitigation.
- Multi-Paxos runs one Single-Decree instance per log slot and skips Phase 1 once a leader is stable.

## What's Next

Next: [Two-Phase Commit](../16-two-phase-commit/16-two-phase-commit.md).

## Resources

- [Paxos Made Simple — Leslie Lamport (2001)](https://lamport.azurewebsites.net/pubs/paxos-simple.pdf) — the clearest description of the protocol; read Sections 2 and 3.
- [The Part-Time Parliament — Leslie Lamport (1998)](https://lamport.azurewebsites.net/pubs/lamport-paxos.pdf) — the original Paxos paper with formal proofs.
- [Paxos Made Live — Chandra, Griesemer, Redstone (2007)](https://research.google/pubs/pub33002/) — Google's production experience implementing Multi-Paxos in Chubby.
- [sync package — pkg.go.dev](https://pkg.go.dev/sync) — documentation for `sync.Mutex` used in Acceptor and Learner.
- [Go Memory Model — go.dev/ref/mem](https://go.dev/ref/mem) — explains the happens-before guarantees that make the mutex usage correct.
