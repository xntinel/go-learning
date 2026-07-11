# 16. Two-Phase Commit

Two-Phase Commit (2PC) is the canonical protocol for achieving atomic distributed transactions. Its strength is a simple correctness guarantee: either every participant commits, or every participant aborts. Its weakness is a hard blocking problem -- if the coordinator fails after collecting votes but before broadcasting the final decision, participants that voted "yes" are stuck with locks held and cannot proceed independently. This lesson builds a working 2PC engine with write-ahead logging (WAL), coordinator recovery, and a test that demonstrates the blocking condition.

```text
twophasecommit/
  go.mod
  twophasecommit.go
  twophasecommit_test.go
  cmd/demo/main.go
```

## Concepts

### The Two Phases

Phase 1 -- Prepare: the coordinator sends a `Prepare(txID)` message to every participant. Each participant writes a durable log entry (`VOTE_YES txid` or `VOTE_NO txid`) and replies. A participant that cannot guarantee it can commit votes No; otherwise it votes Yes and enters a "prepared" state -- it has locked resources and is waiting for the coordinator's final word.

Phase 2 -- Commit or Abort: if every participant voted Yes, the coordinator durably logs `COMMIT txid` and sends `Commit` to all participants. If any participant voted No, the coordinator logs `ABORT txid` and sends `Abort`. Participants log `COMMITTED` or `ABORTED` and release their locks.

### Why the Log Must Come Before the Network

The WAL rule is: log before acting. The coordinator must write `COMMIT txid` to its log before sending commit messages -- if it crashes mid-broadcast, participants that received the message committed; on recovery the coordinator re-reads its log, finds `COMMIT txid`, and resends commit to any participant that did not acknowledge. The same logic applies to participants: `VOTE_YES` is written before sending the Yes reply, so on recovery the participant knows it voted Yes and must await the coordinator.

### The Blocking Problem

A participant that voted Yes has locked its resources. It cannot unilaterally commit (another participant might have voted No) and cannot unilaterally abort (the coordinator might have logged Commit). It is blocked until the coordinator or a surviving peer can supply the final decision.

Cooperative termination: a participant waiting for a decision can contact other participants. If any has already committed, the decision is Commit. If any has aborted, the decision is Abort. If all are in the "prepared-yes" state, they are all blocked -- a classic 2PC scenario. Three-Phase Commit (3PC) adds a pre-commit phase to break this particular case, at the cost of additional latency and new failure modes.

### Log Entry Types

| Entry | Written by | Meaning |
|---|---|---|
| `BEGIN txid` | Coordinator | Transaction started |
| `PREPARE_SENT txid` | Coordinator | Prepare messages dispatched |
| `COMMIT txid` | Coordinator | Final commit decision (durable) |
| `ABORT txid` | Coordinator | Final abort decision (durable) |
| `VOTE_YES txid` | Participant | Will commit if asked |
| `VOTE_NO txid` | Participant | Cannot commit |
| `COMMITTED txid` | Participant | Committed locally |
| `ABORTED txid` | Participant | Aborted locally |

### Trade-offs vs. Alternatives

2PC requires O(2n) messages and blocks on coordinator failure. Paxos Commit (Gray & Lamport 2004) replaces the coordinator with a Paxos group, eliminating the single point of blocking at the cost of higher message complexity. Saga breaks the distributed transaction into a sequence of local transactions with compensating actions: no locking, no blocking, but no atomicity guarantee -- compensations may fail and leave the system in a partially rolled-back state.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/twophasecommit/cmd/demo
cd ~/go-exercises/twophasecommit
go mod init example.com/twophasecommit
```

### Exercise 1: Coordinator and Participant Types

Create `twophasecommit.go`:

```go
// twophasecommit.go
package twophasecommit

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Sentinel errors for transaction outcomes and log anomalies.
var (
	ErrAborted      = errors.New("transaction aborted")
	ErrAlreadyVoted = errors.New("participant already voted for this transaction")
	ErrUnknownTx    = errors.New("unknown transaction")
	ErrNotPrepared  = errors.New("participant is not in prepared state")
)

// Vote is a participant's response to a Prepare message.
type Vote int

const (
	VoteYes Vote = iota
	VoteNo
)

func (v Vote) String() string {
	if v == VoteYes {
		return "YES"
	}
	return "NO"
}

// TxState is the coordinator's view of a transaction's progress.
type TxState int

const (
	TxPending   TxState = iota // Prepare has been sent; collecting votes.
	TxCommitted                // All participants voted Yes; commit sent.
	TxAborted                  // At least one participant voted No; abort sent.
)

func (s TxState) String() string {
	switch s {
	case TxPending:
		return "PENDING"
	case TxCommitted:
		return "COMMITTED"
	default:
		return "ABORTED"
	}
}

// ParticipantState is the participant's own view of a transaction.
type ParticipantState int

const (
	PSIdle      ParticipantState = iota // No prepare received yet.
	PSPreparedY                         // Voted Yes; waiting for decision.
	PSPreparedN                         // Voted No; expecting Abort.
	PSCommitted                         // Committed locally.
	PSAborted                           // Aborted locally.
)

// LogEntry is one record in a write-ahead log.
type LogEntry struct {
	Kind string // e.g. "COMMIT", "VOTE_YES"
	TxID string
}

// WAL is an in-memory write-ahead log (replace with an append-only file in
// production).
type WAL struct {
	mu      sync.Mutex
	entries []LogEntry
}

func (w *WAL) Append(kind, txID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.entries = append(w.entries, LogEntry{Kind: kind, TxID: txID})
}

// Entries returns a snapshot of all log entries.
func (w *WAL) Entries() []LogEntry {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]LogEntry, len(w.entries))
	copy(out, w.entries)
	return out
}

// DecisionFromLog returns the committed/aborted decision for txID from the
// coordinator's log, or TxPending if no decision was recorded.
func (w *WAL) DecisionFromLog(txID string) TxState {
	for _, e := range w.Entries() {
		if e.TxID != txID {
			continue
		}
		if e.Kind == "COMMIT" {
			return TxCommitted
		}
		if e.Kind == "ABORT" {
			return TxAborted
		}
	}
	return TxPending
}

// Coordinator drives a two-phase commit across participants.
type Coordinator struct {
	log    *WAL
	mu     sync.Mutex
	states map[string]TxState // txID -> state
}

// NewCoordinator creates a coordinator with an empty log.
func NewCoordinator() *Coordinator {
	return &Coordinator{
		log:    &WAL{},
		states: make(map[string]TxState),
	}
}

// Log returns the coordinator's WAL (for inspection and recovery tests).
func (c *Coordinator) Log() *WAL { return c.log }

// States returns a snapshot of all known transaction states.
func (c *Coordinator) States() map[string]TxState {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]TxState, len(c.states))
	for k, v := range c.states {
		out[k] = v
	}
	return out
}

// RunTransaction executes a full 2PC round against participants.
// It returns nil if all participants committed, ErrAborted if any voted No,
// or the first network/participant error encountered.
func (c *Coordinator) RunTransaction(txID string, participants []*Participant) error {
	c.log.Append("BEGIN", txID)

	// Phase 1: Prepare.
	c.log.Append("PREPARE_SENT", txID)
	votes := make([]Vote, len(participants))
	for i, p := range participants {
		v, err := p.Prepare(txID)
		if err != nil {
			c.log.Append("ABORT", txID)
			c.mu.Lock()
			c.states[txID] = TxAborted
			c.mu.Unlock()
			_ = c.broadcastAbort(txID, participants)
			return fmt.Errorf("twophasecommit: prepare %s: %w", p.Name(), err)
		}
		votes[i] = v
	}

	// Decision.
	allYes := true
	for _, v := range votes {
		if v == VoteNo {
			allYes = false
			break
		}
	}

	if allYes {
		// Write durable commit decision BEFORE broadcasting.
		c.log.Append("COMMIT", txID)
		c.mu.Lock()
		c.states[txID] = TxCommitted
		c.mu.Unlock()
		return c.broadcastCommit(txID, participants)
	}

	// Abort path.
	c.log.Append("ABORT", txID)
	c.mu.Lock()
	c.states[txID] = TxAborted
	c.mu.Unlock()
	_ = c.broadcastAbort(txID, participants)
	return fmt.Errorf("%w: tx %s", ErrAborted, txID)
}

// RecoverAndDecide re-reads the coordinator's log and rebroadcasts decisions
// for any transaction that already has a COMMIT or ABORT entry. Call this after
// the coordinator restarts to reach participants that missed the first broadcast.
func (c *Coordinator) RecoverAndDecide(txID string, participants []*Participant) error {
	decision := c.log.DecisionFromLog(txID)
	switch decision {
	case TxCommitted:
		c.mu.Lock()
		c.states[txID] = TxCommitted
		c.mu.Unlock()
		return c.broadcastCommit(txID, participants)
	case TxAborted:
		c.mu.Lock()
		c.states[txID] = TxAborted
		c.mu.Unlock()
		return c.broadcastAbort(txID, participants)
	default:
		return fmt.Errorf("%w: %s", ErrUnknownTx, txID)
	}
}

func (c *Coordinator) broadcastCommit(txID string, participants []*Participant) error {
	for _, p := range participants {
		if err := p.Commit(txID); err != nil {
			return err
		}
	}
	return nil
}

func (c *Coordinator) broadcastAbort(txID string, participants []*Participant) error {
	for _, p := range participants {
		_ = p.Abort(txID) // best-effort; participants may already have aborted
	}
	return nil
}

// Participant represents one node in a distributed transaction.
type Participant struct {
	name   string
	log    *WAL
	mu     sync.Mutex
	states map[string]ParticipantState
	// voteFunc customises how this participant votes; nil means always Yes.
	voteFunc func(txID string) Vote
}

// NewParticipant creates a participant with the given name and vote policy.
// Pass nil for voteFunc to get a participant that always votes Yes.
func NewParticipant(name string, voteFunc func(txID string) Vote) *Participant {
	return &Participant{
		name:     name,
		log:      &WAL{},
		states:   make(map[string]ParticipantState),
		voteFunc: voteFunc,
	}
}

// Name returns the participant's name.
func (p *Participant) Name() string { return p.name }

// Log returns the participant's WAL.
func (p *Participant) Log() *WAL { return p.log }

// TxState returns the participant's state for txID.
func (p *Participant) TxState(txID string) ParticipantState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.states[txID]
}

// Prepare handles Phase 1. The participant decides its vote, logs it durably,
// and returns it to the coordinator.
func (p *Participant) Prepare(txID string) (Vote, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, seen := p.states[txID]; seen {
		return VoteNo, fmt.Errorf("%w: tx %s at %s", ErrAlreadyVoted, txID, p.name)
	}
	var v Vote
	if p.voteFunc != nil {
		v = p.voteFunc(txID)
	} else {
		v = VoteYes
	}
	if v == VoteYes {
		p.log.Append("VOTE_YES", txID)
		p.states[txID] = PSPreparedY
	} else {
		p.log.Append("VOTE_NO", txID)
		p.states[txID] = PSPreparedN
	}
	return v, nil
}

// Commit handles a Phase 2 Commit message. The participant must be in
// PSPreparedY state (voted Yes) or PSCommitted (idempotent re-delivery).
func (p *Participant) Commit(txID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.states[txID]
	if st == PSCommitted {
		return nil // idempotent
	}
	if st != PSPreparedY {
		return fmt.Errorf("%w: tx %s at %s state %v", ErrNotPrepared, txID, p.name, st)
	}
	p.log.Append("COMMITTED", txID)
	p.states[txID] = PSCommitted
	return nil
}

// Abort handles a Phase 2 Abort message. Idempotent.
func (p *Participant) Abort(txID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.states[txID]
	if st == PSAborted {
		return nil
	}
	p.log.Append("ABORTED", txID)
	p.states[txID] = PSAborted
	return nil
}

// IsBlocked reports whether this participant is waiting for a decision it
// cannot resolve -- i.e., it voted Yes but has not yet received Commit/Abort.
func (p *Participant) IsBlocked(txID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.states[txID] == PSPreparedY
}

// CooperativeDecision asks peers for the decision on txID. It returns
// TxCommitted or TxAborted if any peer has a final state; TxPending if all
// peers are also prepared (the blocking condition).
func CooperativeDecision(txID string, peers []*Participant) TxState {
	for _, peer := range peers {
		switch peer.TxState(txID) {
		case PSCommitted:
			return TxCommitted
		case PSAborted:
			return TxAborted
		}
	}
	return TxPending // everyone is prepared -- all blocked
}

// LogSummary returns a human-readable summary of a WAL.
func LogSummary(w *WAL) string {
	entries := w.Entries()
	if len(entries) == 0 {
		return "(empty)"
	}
	parts := make([]string, len(entries))
	for i, e := range entries {
		parts[i] = fmt.Sprintf("%s(%s)", e.Kind, e.TxID)
	}
	return strings.Join(parts, " -> ")
}
```

### Exercise 2: Test the Protocol

Create `twophasecommit_test.go`:

```go
// twophasecommit_test.go
package twophasecommit

import (
	"errors"
	"fmt"
	"testing"
)

func alwaysYes(_ string) Vote { return VoteYes }
func alwaysNo(_ string) Vote  { return VoteNo }

// TestHappyPath verifies that three participants all end up in PSCommitted when
// they all vote Yes.
func TestHappyPath(t *testing.T) {
	t.Parallel()

	c := NewCoordinator()
	ps := []*Participant{
		NewParticipant("A", alwaysYes),
		NewParticipant("B", alwaysYes),
		NewParticipant("C", alwaysYes),
	}

	if err := c.RunTransaction("tx1", ps); err != nil {
		t.Fatalf("RunTransaction: %v", err)
	}
	for _, p := range ps {
		if p.TxState("tx1") != PSCommitted {
			t.Errorf("participant %s: state = %v, want PSCommitted", p.Name(), p.TxState("tx1"))
		}
	}
	if c.States()["tx1"] != TxCommitted {
		t.Errorf("coordinator state = %v, want TxCommitted", c.States()["tx1"])
	}
}

// TestOneVoteNoAborts verifies that a single No vote causes all participants to
// abort and the coordinator to record TxAborted.
func TestOneVoteNoAborts(t *testing.T) {
	t.Parallel()

	c := NewCoordinator()
	ps := []*Participant{
		NewParticipant("A", alwaysYes),
		NewParticipant("B", alwaysNo),
		NewParticipant("C", alwaysYes),
	}

	err := c.RunTransaction("tx2", ps)
	if !errors.Is(err, ErrAborted) {
		t.Fatalf("err = %v, want ErrAborted", err)
	}
	for _, p := range ps {
		st := p.TxState("tx2")
		if st == PSCommitted {
			t.Errorf("participant %s committed despite abort", p.Name())
		}
	}
	if c.States()["tx2"] != TxAborted {
		t.Errorf("coordinator state = %v, want TxAborted", c.States()["tx2"])
	}
}

// TestCoordinatorRecoveryAfterCommitLogged simulates a coordinator that crashed
// after writing COMMIT to its log but before broadcasting. On recovery,
// RecoverAndDecide replays the decision and participants commit.
func TestCoordinatorRecoveryAfterCommitLogged(t *testing.T) {
	t.Parallel()

	// Build a coordinator and manually drive Phase 1 to get all Yes votes,
	// write COMMIT to the log, but skip the broadcast (simulate crash).
	c := NewCoordinator()
	ps := []*Participant{
		NewParticipant("A", alwaysYes),
		NewParticipant("B", alwaysYes),
	}

	txID := "tx-recover"
	c.log.Append("BEGIN", txID)
	c.log.Append("PREPARE_SENT", txID)
	for _, p := range ps {
		v, err := p.Prepare(txID)
		if err != nil || v != VoteYes {
			t.Fatalf("Prepare: v=%v err=%v", v, err)
		}
	}
	// Coordinator logs COMMIT but crashes before broadcasting.
	c.log.Append("COMMIT", txID)

	// Participants are blocked: they voted Yes but have not received Commit.
	for _, p := range ps {
		if !p.IsBlocked(txID) {
			t.Errorf("participant %s should be blocked before recovery", p.Name())
		}
	}

	// Coordinator recovers and rebroadcasts.
	if err := c.RecoverAndDecide(txID, ps); err != nil {
		t.Fatalf("RecoverAndDecide: %v", err)
	}
	for _, p := range ps {
		if p.TxState(txID) != PSCommitted {
			t.Errorf("participant %s: state = %v after recovery, want PSCommitted", p.Name(), p.TxState(txID))
		}
	}
}

// TestBlockingCondition shows that participants which voted Yes cannot resolve
// the transaction using CooperativeDecision when all peers are also in the
// "prepared" state -- the canonical 2PC blocking scenario.
func TestBlockingCondition(t *testing.T) {
	t.Parallel()

	ps := []*Participant{
		NewParticipant("A", alwaysYes),
		NewParticipant("B", alwaysYes),
		NewParticipant("C", alwaysYes),
	}
	txID := "tx-block"
	for _, p := range ps {
		if _, err := p.Prepare(txID); err != nil {
			t.Fatal(err)
		}
	}
	// All three are prepared-yes; coordinator has not yet logged a decision.
	// CooperativeDecision must return TxPending (blocking).
	if got := CooperativeDecision(txID, ps); got != TxPending {
		t.Errorf("CooperativeDecision = %v, want TxPending (blocking)", got)
	}
}

// TestCooperativeDecisionResolvesWhenOnePeerCommitted shows that if one
// participant has already committed, cooperative termination resolves to Commit.
func TestCooperativeDecisionResolvesWhenOnePeerCommitted(t *testing.T) {
	t.Parallel()

	ps := []*Participant{
		NewParticipant("A", alwaysYes),
		NewParticipant("B", alwaysYes),
	}
	txID := "tx-coop"
	for _, p := range ps {
		if _, err := p.Prepare(txID); err != nil {
			t.Fatal(err)
		}
	}
	// Participant A received and processed the commit before the query.
	if err := ps[0].Commit(txID); err != nil {
		t.Fatal(err)
	}
	// B queries peers; A has committed, so the answer is TxCommitted.
	if got := CooperativeDecision(txID, []*Participant{ps[0]}); got != TxCommitted {
		t.Errorf("CooperativeDecision = %v, want TxCommitted", got)
	}
}

// TestIdempotentCommitAndAbort verifies that delivering Commit or Abort twice
// does not cause an error.
func TestIdempotentCommitAndAbort(t *testing.T) {
	t.Parallel()

	p := NewParticipant("A", alwaysYes)
	txID := "tx-idem"
	if _, err := p.Prepare(txID); err != nil {
		t.Fatal(err)
	}
	if err := p.Commit(txID); err != nil {
		t.Fatal(err)
	}
	if err := p.Commit(txID); err != nil {
		t.Fatalf("second Commit should be idempotent: %v", err)
	}

	p2 := NewParticipant("B", alwaysNo)
	txID2 := "tx-idem2"
	if _, err := p2.Prepare(txID2); err != nil {
		t.Fatal(err)
	}
	if err := p2.Abort(txID2); err != nil {
		t.Fatal(err)
	}
	if err := p2.Abort(txID2); err != nil {
		t.Fatalf("second Abort should be idempotent: %v", err)
	}
}

// TestAlreadyVotedError verifies that calling Prepare twice on the same
// participant for the same transaction returns ErrAlreadyVoted.
func TestAlreadyVotedError(t *testing.T) {
	t.Parallel()

	p := NewParticipant("A", alwaysYes)
	txID := "tx-dup"
	if _, err := p.Prepare(txID); err != nil {
		t.Fatal(err)
	}
	_, err := p.Prepare(txID)
	if !errors.Is(err, ErrAlreadyVoted) {
		t.Errorf("err = %v, want ErrAlreadyVoted", err)
	}
}

// TestParallelTransactions verifies that the coordinator and participants
// handle independent transactions without data races (run with -race).
func TestParallelTransactions(t *testing.T) {
	t.Parallel()

	c := NewCoordinator()
	for i := 0; i < 20; i++ {
		i := i
		t.Run(fmt.Sprintf("tx%d", i), func(t *testing.T) {
			t.Parallel()
			txID := fmt.Sprintf("par-%d", i)
			ps := []*Participant{
				NewParticipant("X", alwaysYes),
				NewParticipant("Y", alwaysYes),
			}
			if err := c.RunTransaction(txID, ps); err != nil {
				t.Fatalf("RunTransaction(%s): %v", txID, err)
			}
		})
	}
}

func ExampleCoordinator_RunTransaction() {
	c := NewCoordinator()
	ps := []*Participant{
		NewParticipant("db-1", nil),
		NewParticipant("db-2", nil),
	}
	err := c.RunTransaction("order-99", ps)
	if err != nil {
		fmt.Println("aborted:", err)
	} else {
		fmt.Println("committed")
	}
	fmt.Println(ps[0].TxState("order-99") == PSCommitted)
	// Output:
	// committed
	// true
}
```

### Exercise 3: A Runnable Demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/twophasecommit"
)

func main() {
	fmt.Println("=== Happy path: all participants vote Yes ===")
	runDemo("tx-happy",
		twophasecommit.NewParticipant("db-1", nil),
		twophasecommit.NewParticipant("db-2", nil),
		twophasecommit.NewParticipant("db-3", nil),
	)

	fmt.Println()
	fmt.Println("=== Abort path: one participant votes No ===")
	runDemo("tx-abort",
		twophasecommit.NewParticipant("db-1", nil),
		twophasecommit.NewParticipant("db-2", func(_ string) twophasecommit.Vote {
			return twophasecommit.VoteNo
		}),
		twophasecommit.NewParticipant("db-3", nil),
	)

	fmt.Println()
	fmt.Println("=== Blocking condition: coordinator failed after collect, before broadcast ===")
	demonstrateBlocking()
}

func runDemo(txID string, ps ...*twophasecommit.Participant) {
	c := twophasecommit.NewCoordinator()
	err := c.RunTransaction(txID, ps)
	if err != nil {
		fmt.Printf("  outcome: ABORTED (%v)\n", err)
	} else {
		fmt.Println("  outcome: COMMITTED")
	}
	for _, p := range ps {
		st := p.TxState(txID)
		fmt.Printf("  %s log: %s\n", p.Name(), twophasecommit.LogSummary(p.Log()))
		_ = st
	}
	fmt.Printf("  coordinator log: %s\n", twophasecommit.LogSummary(c.Log()))
}

func demonstrateBlocking() {
	ps := []*twophasecommit.Participant{
		twophasecommit.NewParticipant("db-1", nil),
		twophasecommit.NewParticipant("db-2", nil),
	}
	txID := "tx-block"
	// Simulate coordinator sending Prepare but crashing before decision.
	for _, p := range ps {
		if _, err := p.Prepare(txID); err != nil {
			fmt.Println("  prepare error:", err)
			return
		}
	}
	for _, p := range ps {
		fmt.Printf("  %s blocked = %v\n", p.Name(), p.IsBlocked(txID))
	}
	decision := twophasecommit.CooperativeDecision(txID, ps)
	fmt.Printf("  cooperative termination decision = %v (all peers prepared => blocking)\n", decision)
}
```

Your turn: add `TestWALRecordsAllEntries` that creates a `Coordinator`, runs a successful transaction against two always-Yes participants, retrieves the coordinator's log entries via `c.Log().Entries()`, and asserts that entries with kinds `"BEGIN"`, `"PREPARE_SENT"`, and `"COMMIT"` all appear in order for the transaction ID.

## Common Mistakes

### Logging the Decision After Broadcasting

Wrong: the coordinator broadcasts Commit, then writes `COMMIT` to its log. If it crashes between the broadcast and the log write, on recovery the log shows no decision, so the coordinator cannot replay it.

Fix: write `COMMIT` or `ABORT` to the log before sending any commit/abort messages. The code in Exercise 1 does this: `c.log.Append("COMMIT", txID)` precedes the first `broadcastCommit` call.

### Unilaterally Aborting After Voting Yes

Wrong: a participant that voted Yes times out waiting for the coordinator and calls `Abort` on itself. If the coordinator actually logged Commit and has started sending commit messages to other participants, those participants will commit while this participant aborts -- atomicity is broken.

Fix: a participant that voted Yes must not unilaterally abort. It must contact the coordinator or use cooperative termination (`CooperativeDecision`) to learn the outcome. Only if every peer is also in the prepared-yes state (and none has committed or aborted) is the transaction truly blocked, and no safe unilateral decision is possible.

### Missing Idempotency in Commit and Abort Handlers

Wrong: a participant errors or panics on a duplicate Commit or Abort. The coordinator may re-broadcast after recovering from a crash.

Fix: check the current state before acting. If the participant is already in `PSCommitted`, a second `Commit` is a no-op and returns nil. The `Commit` and `Abort` methods in Exercise 1 do this.

### Data Races on the Participant State Map

Wrong: concurrent calls to `Prepare`, `Commit`, or `Abort` on the same participant without a mutex.

Fix: each method acquires `p.mu` before reading or writing `p.states`. Run `go test -race ./...` to catch this.

## Verification

From `~/go-exercises/twophasecommit`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

All five must pass. The `go test -race` run exercises the parallel transaction test and will catch any unsynchronized access to shared state.

## Summary

- 2PC provides atomicity across participants through two round trips: Prepare (collect votes) and Commit/Abort (broadcast decision).
- Write-ahead logging is mandatory: log before acting, so a crash at any point leaves enough information to replay the correct decision on recovery.
- The blocking problem is real and inherent: a participant that voted Yes cannot proceed until the coordinator or a peer with a final state is available.
- Cooperative termination resolves some blocking cases but cannot break the all-prepared deadlock.
- Idempotency in Commit and Abort handlers is required because the coordinator may rebroadcast after recovery.
- 2PC is used in XA transactions (RDBMS), distributed key-value stores, and anywhere strong commit atomicity is needed.

## What's Next

Next: [Saga Orchestrator](../17-saga-orchestrator/17-saga-orchestrator.md).

## Resources

- [Gray & Lamport: Consensus on Transaction Commit (2004)](https://www.microsoft.com/en-us/research/wp-content/uploads/2016/02/tr-2003-96.pdf) -- Paxos Commit replaces 2PC coordinator with a Paxos group
- [pkg.go.dev/sync](https://pkg.go.dev/sync) -- sync.Mutex for safe concurrent state
- [Two-Phase Commit Protocol (Wikipedia)](https://en.wikipedia.org/wiki/Two-phase_commit_protocol) -- protocol state machine and failure modes
- [go.dev/ref/spec#Errors](https://go.dev/ref/spec) -- sentinel errors and error wrapping with %w
- [Designing Data-Intensive Applications, Chapter 9 (Kleppmann)](https://dataintensive.net/) -- distributed transactions, 2PC, and Saga in context
