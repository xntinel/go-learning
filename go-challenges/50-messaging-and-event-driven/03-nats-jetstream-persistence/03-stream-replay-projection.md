# Exercise 3: Replaying a Stream to Rebuild a Projection

Because the consumer owns the delivery cursor, a stream can be replayed from any
point to rebuild a read model — no snapshot machinery required. This exercise
builds a projection rebuilder: an ordered ephemeral consumer that folds every
event into an in-memory read model and stops the instant it catches up to the
live tail. The fold reducer and the catch-up predicate are pure and fully
unit-tested; the replay loop is behind a build tag.

This module is fully self-contained. It begins with its own `go mod init`,
defines every type it needs, and ships its own demo and tests. Nothing here
imports any other exercise.

## What you'll build

```text
jsreplay/                    independent module: example.com/jsreplay
  go.mod                     go 1.26
  projection.go              DecodedEvent; Projection; Fold; CaughtUp (pure)
  rebuilder_online.go        //go:build online — OrderedConsumer replay loop
  cmd/
    demo/
      main.go                runnable pure demo: fold a fixed ledger into a state
  projection_test.go         offline table-driven fold tests + ExampleProjection_Fold
  rebuilder_online_test.go   //go:build online — real replay-from-sequence test
```

- Files: `projection.go`, `rebuilder_online.go`, `cmd/demo/main.go`, `projection_test.go`, `rebuilder_online_test.go`.
- Implement: a `Projection` with `Fold(DecodedEvent) (Projection, error)` that is pure and idempotent (a repeated stream sequence is a no-op) and `CaughtUp(numPending) bool`; the online `Rebuild` drives an `OrderedConsumer` with a `DeliverPolicy`/`OptStartSeq`/`OptStartTime` chosen from a `ReplayStart`.
- Test: offline table-driven fold tests (empty-state start, accumulation, idempotent re-application, unknown-type sentinel via `errors.Is`, no input mutation) and `CaughtUp` boundaries, plus an `Example` with `// Output:`; a `//go:build online` test that publishes events, replays from a sequence, and asserts the rebuilt projection and termination at `NumPending == 0`.
- Verify: `go test -count=1 -race ./...` (offline core); the online test runs against a real `nats-server -js`.

Set up the module:

```bash
mkdir -p go-solutions/50-messaging-and-event-driven/03-nats-jetstream-persistence/03-stream-replay-projection/cmd/demo
cd go-solutions/50-messaging-and-event-driven/03-nats-jetstream-persistence/03-stream-replay-projection
go mod edit -go=1.26
```

### Replay is a fold, and the fold is where the bugs live

Rebuilding a read model from an event stream is a left fold: start from an empty
state and apply each event in order. The JetStream part — pick a start point,
create an ordered consumer, iterate — is mechanical. The part that carries the
bugs is the reducer: does re-applying an event you already saw double-count? does
it start correctly from nothing? does an unknown event type corrupt the state or
fail loudly? So the reducer is a pure function with an exhaustive table test, and
the replay loop is a thin online adapter that calls it.

`Fold` takes a `Projection` and one `DecodedEvent` and returns the next
`Projection`. Two properties make it safe to use for a rebuild. First, it is
idempotent on the stream sequence: `DecodedEvent.Sequence` is the message's stream
sequence, which is stable and monotonic, so `Fold` records applied sequences and
treats a repeat as a no-op. That matters because replays overlap in practice —
you might replay from a sequence you have already partially processed, or retry a
rebuild that half-finished — and an overlapping replay must not double the
balances. Second, it does not mutate its input: it clones the maps and returns a
new value, so an earlier projection value is never corrupted by a later fold. An
unrecognized event type returns `ErrUnknownEventType` (wrapped with `%w`) and
leaves the state untouched, so a bad event fails loudly instead of silently
skewing the read model.

`CaughtUp` is the second pure piece: during delivery, `Metadata().NumPending` is
the count of matching messages not yet delivered, so `NumPending == 0` means the
message in hand is the last one and the rebuild has reached the live tail. That is
how a replay knows to stop instead of blocking forever waiting for a message that
will not come.

Create `projection.go`:

```go
package jsreplay

import (
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"
)

// Event types carried on the ledger stream.
const (
	TypeCredit = "credit"
	TypeDebit  = "debit"
)

// ErrUnknownEventType is returned by Fold for an event Type it cannot apply.
var ErrUnknownEventType = errors.New("unknown event type")

// DecodedEvent is a ledger event already decoded from a stream message. Sequence
// is the message's stream sequence, which is stable and monotonic and therefore
// a natural idempotency key for a replay.
type DecodedEvent struct {
	Sequence uint64
	Account  string
	Type     string
	Amount   int64
}

// Projection is the read model rebuilt from the stream: a balance per account.
// It also records which stream sequences it has already folded, so re-applying
// the same event (a replay overlap, a retried rebuild) is a no-op. Both maps are
// treated as immutable; Fold returns a new Projection rather than mutating.
type Projection struct {
	Balances map[string]int64
	applied  map[uint64]bool
}

// NewProjection returns the empty read model: the initial state a replay folds
// events into ("out of nothing").
func NewProjection() Projection {
	return Projection{
		Balances: map[string]int64{},
		applied:  map[uint64]bool{},
	}
}

// Fold applies one event and returns the resulting Projection. It is pure and
// idempotent: applying an event whose Sequence was already folded returns an
// equal state, so replaying an overlapping range does not double-count. An
// unrecognized Type yields ErrUnknownEventType wrapped with %w and leaves the
// state unchanged.
func (p Projection) Fold(e DecodedEvent) (Projection, error) {
	if p.applied[e.Sequence] {
		return p, nil
	}
	var delta int64
	switch e.Type {
	case TypeCredit:
		delta = e.Amount
	case TypeDebit:
		delta = -e.Amount
	default:
		return p, fmt.Errorf("fold seq %d account %q: %w", e.Sequence, e.Account, ErrUnknownEventType)
	}
	next := Projection{
		Balances: maps.Clone(p.Balances),
		applied:  maps.Clone(p.applied),
	}
	next.Balances[e.Account] += delta
	next.applied[e.Sequence] = true
	return next, nil
}

// Applied reports how many distinct events the projection has folded.
func (p Projection) Applied() int { return len(p.applied) }

// String renders balances in deterministic account order for stable output.
func (p Projection) String() string {
	accounts := make([]string, 0, len(p.Balances))
	for a := range p.Balances {
		accounts = append(accounts, a)
	}
	sort.Strings(accounts)
	parts := make([]string, len(accounts))
	for i, a := range accounts {
		parts[i] = fmt.Sprintf("%s=%d", a, p.Balances[a])
	}
	return strings.Join(parts, " ")
}

// CaughtUp reports whether a replay has reached the live tail. During delivery,
// Metadata().NumPending is the count of matching messages not yet delivered, so
// NumPending == 0 means the message in hand is the last one and the rebuild is
// complete.
func CaughtUp(numPending uint64) bool { return numPending == 0 }
```

### The ordered-consumer replay loop (online)

The online rebuilder chooses a start point from a `ReplayStart` and maps it onto
an `OrderedConsumerConfig`: `DeliverAllPolicy` for the whole stream,
`DeliverByStartSequencePolicy` with `OptStartSeq`, or `DeliverByStartTimePolicy`
with `OptStartTime`. An ordered consumer is the right tool for a rebuild because
it delivers strictly in order, gap-free, and transparently recreates itself on
transient error — so the fold sees a clean sequence. The loop iterates with
`Messages()`/`Next()`, reads the stream sequence from `Metadata().Sequence.Stream`,
decodes the JSON body, folds it, and returns the moment `CaughtUp(md.NumPending)`
is true. The ordered consumer is ephemeral, and `it.Stop()` is deferred, so the
replay leaves nothing on the server.

Create `rebuilder_online.go`:

```go
//go:build online

// This file holds the JetStream replay I/O. It is excluded from the default
// build and compiled only with -tags online against a real server. The reducer
// (Projection.Fold) and the catch-up predicate (CaughtUp) it drives are pure and
// tested offline.
package jsreplay

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// StartKind selects where a replay begins.
type StartKind int

const (
	// StartAll replays the whole stream from the first message.
	StartAll StartKind = iota
	// StartFromSequence replays from a specific stream sequence.
	StartFromSequence
	// StartFromTime replays from a wall-clock instant.
	StartFromTime
)

// ReplayStart is the domain description of a replay's starting point, expressed
// without JetStream types.
type ReplayStart struct {
	Kind StartKind
	Seq  uint64
	Time time.Time
}

// wirePayload is the JSON body published for each ledger event. The stream
// sequence is not in the body; it comes from message metadata.
type wirePayload struct {
	Account string `json:"account"`
	Type    string `json:"type"`
	Amount  int64  `json:"amount"`
}

// decodeEvent turns a raw message body plus its stream sequence into a
// DecodedEvent the reducer can fold.
func decodeEvent(seq uint64, data []byte) (DecodedEvent, error) {
	var w wirePayload
	if err := json.Unmarshal(data, &w); err != nil {
		return DecodedEvent{}, fmt.Errorf("decode seq %d: %w", seq, err)
	}
	return DecodedEvent{
		Sequence: seq,
		Account:  w.Account,
		Type:     w.Type,
		Amount:   w.Amount,
	}, nil
}

// toOrderedConfig maps a ReplayStart onto an OrderedConsumerConfig. An ordered
// consumer gives strictly ordered, gap-free ephemeral delivery, which is exactly
// what rebuilding a projection needs.
func toOrderedConfig(filter string, start ReplayStart) jetstream.OrderedConsumerConfig {
	cfg := jetstream.OrderedConsumerConfig{FilterSubjects: []string{filter}}
	switch start.Kind {
	case StartFromSequence:
		cfg.DeliverPolicy = jetstream.DeliverByStartSequencePolicy
		cfg.OptStartSeq = start.Seq
	case StartFromTime:
		cfg.DeliverPolicy = jetstream.DeliverByStartTimePolicy
		t := start.Time
		cfg.OptStartTime = &t
	default:
		cfg.DeliverPolicy = jetstream.DeliverAllPolicy
	}
	return cfg
}

// Rebuild replays stream from start, folding every event into a fresh Projection,
// and returns once the replay catches up to the live tail (NumPending == 0). The
// ordered consumer is ephemeral, so nothing is left behind on the server.
func Rebuild(ctx context.Context, js jetstream.JetStream, stream, filter string, start ReplayStart) (Projection, error) {
	cons, err := js.OrderedConsumer(ctx, stream, toOrderedConfig(filter, start))
	if err != nil {
		return Projection{}, fmt.Errorf("ordered consumer on %q: %w", stream, err)
	}
	it, err := cons.Messages()
	if err != nil {
		return Projection{}, fmt.Errorf("messages iterator: %w", err)
	}
	defer it.Stop()

	proj := NewProjection()
	for {
		msg, err := it.Next()
		if err != nil {
			return proj, fmt.Errorf("next message: %w", err)
		}
		md, err := msg.Metadata()
		if err != nil {
			return proj, fmt.Errorf("metadata: %w", err)
		}
		ev, err := decodeEvent(md.Sequence.Stream, msg.Data())
		if err != nil {
			return proj, err
		}
		proj, err = proj.Fold(ev)
		if err != nil {
			return proj, err
		}
		if CaughtUp(md.NumPending) {
			return proj, nil
		}
	}
}
```

### The runnable demo

The demo folds a fixed ledger — including a duplicate of sequence 1, to show the
idempotency — and prints the resulting balances and the catch-up predicate at two
`NumPending` values. It runs with no server.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/jsreplay"
)

func main() {
	events := []jsreplay.DecodedEvent{
		{Sequence: 1, Account: "alice", Type: jsreplay.TypeCredit, Amount: 100},
		{Sequence: 2, Account: "bob", Type: jsreplay.TypeCredit, Amount: 50},
		{Sequence: 3, Account: "alice", Type: jsreplay.TypeDebit, Amount: 30},
		// A duplicate of sequence 1 (an overlapping replay); idempotent, ignored.
		{Sequence: 1, Account: "alice", Type: jsreplay.TypeCredit, Amount: 100},
		{Sequence: 4, Account: "bob", Type: jsreplay.TypeCredit, Amount: 25},
	}

	proj := jsreplay.NewProjection()
	for _, e := range events {
		next, err := proj.Fold(e)
		if err != nil {
			fmt.Println("fold error:", err)
			return
		}
		proj = next
	}

	fmt.Println("applied:", proj.Applied())
	fmt.Println("balances:", proj)
	fmt.Println("caught up at NumPending 0:", jsreplay.CaughtUp(0))
	fmt.Println("caught up at NumPending 3:", jsreplay.CaughtUp(3))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
applied: 4
balances: alice=70 bob=75
caught up at NumPending 0: true
caught up at NumPending 3: false
```

### Tests

The offline tests pin the reducer. `TestFold` is a table covering the
out-of-nothing initial state, credit/debit accumulation, and an idempotent
duplicate sequence. `TestFoldUnknownType` asserts `ErrUnknownEventType` via
`errors.Is`. `TestFoldDoesNotMutateInput` proves a later fold does not corrupt an
earlier projection value. `TestFoldIdempotentReapplication` re-applies a whole
event slice and asserts the state is unchanged. `TestCaughtUp` pins the boundary.
`ExampleProjection_Fold` locks the rendered balances.

Create `projection_test.go`:

```go
package jsreplay

import (
	"errors"
	"fmt"
	"testing"
)

// foldAll folds a slice of events, failing the test on any fold error.
func foldAll(t *testing.T, events []DecodedEvent) Projection {
	t.Helper()
	p := NewProjection()
	for _, e := range events {
		next, err := p.Fold(e)
		if err != nil {
			t.Fatalf("Fold(seq %d) error: %v", e.Sequence, err)
		}
		p = next
	}
	return p
}

func TestFold(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		events      []DecodedEvent
		wantBalance map[string]int64
		wantApplied int
	}{
		{
			name:        "empty stream yields empty projection",
			events:      nil,
			wantBalance: map[string]int64{},
			wantApplied: 0,
		},
		{
			name: "credits and debits accumulate",
			events: []DecodedEvent{
				{Sequence: 1, Account: "alice", Type: TypeCredit, Amount: 100},
				{Sequence: 2, Account: "alice", Type: TypeDebit, Amount: 30},
				{Sequence: 3, Account: "bob", Type: TypeCredit, Amount: 50},
			},
			wantBalance: map[string]int64{"alice": 70, "bob": 50},
			wantApplied: 3,
		},
		{
			name: "duplicate sequence is idempotent",
			events: []DecodedEvent{
				{Sequence: 1, Account: "alice", Type: TypeCredit, Amount: 100},
				{Sequence: 1, Account: "alice", Type: TypeCredit, Amount: 100},
				{Sequence: 2, Account: "alice", Type: TypeCredit, Amount: 10},
			},
			wantBalance: map[string]int64{"alice": 110},
			wantApplied: 2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := foldAll(t, tc.events)
			if p.Applied() != tc.wantApplied {
				t.Errorf("Applied() = %d, want %d", p.Applied(), tc.wantApplied)
			}
			if len(p.Balances) != len(tc.wantBalance) {
				t.Fatalf("balances = %v, want %v", p.Balances, tc.wantBalance)
			}
			for acct, want := range tc.wantBalance {
				if p.Balances[acct] != want {
					t.Errorf("balance[%s] = %d, want %d", acct, p.Balances[acct], want)
				}
			}
		})
	}
}

func TestFoldUnknownType(t *testing.T) {
	t.Parallel()
	p := NewProjection()
	_, err := p.Fold(DecodedEvent{Sequence: 1, Account: "alice", Type: "transfer", Amount: 5})
	if !errors.Is(err, ErrUnknownEventType) {
		t.Fatalf("Fold err = %v, want errors.Is ErrUnknownEventType", err)
	}
}

func TestFoldDoesNotMutateInput(t *testing.T) {
	t.Parallel()
	base := foldAll(t, []DecodedEvent{
		{Sequence: 1, Account: "alice", Type: TypeCredit, Amount: 100},
	})
	// Folding a further event must not change the earlier projection value.
	_, err := base.Fold(DecodedEvent{Sequence: 2, Account: "alice", Type: TypeCredit, Amount: 5})
	if err != nil {
		t.Fatalf("Fold error: %v", err)
	}
	if base.Balances["alice"] != 100 {
		t.Fatalf("Fold mutated the input projection: alice = %d, want 100", base.Balances["alice"])
	}
	if base.Applied() != 1 {
		t.Fatalf("Fold mutated the input applied set: %d, want 1", base.Applied())
	}
}

func TestFoldIdempotentReapplication(t *testing.T) {
	t.Parallel()
	events := []DecodedEvent{
		{Sequence: 1, Account: "alice", Type: TypeCredit, Amount: 100},
		{Sequence: 2, Account: "bob", Type: TypeCredit, Amount: 40},
	}
	once := foldAll(t, events)
	// Re-apply the same events on top; the state must not change.
	twice := once
	for _, e := range events {
		next, err := twice.Fold(e)
		if err != nil {
			t.Fatalf("Fold error: %v", err)
		}
		twice = next
	}
	if twice.String() != once.String() {
		t.Fatalf("re-application changed state: %q != %q", twice.String(), once.String())
	}
}

func TestCaughtUp(t *testing.T) {
	t.Parallel()
	if !CaughtUp(0) {
		t.Error("CaughtUp(0) = false, want true")
	}
	if CaughtUp(1) {
		t.Error("CaughtUp(1) = true, want false")
	}
}

func ExampleProjection_Fold() {
	events := []DecodedEvent{
		{Sequence: 1, Account: "alice", Type: TypeCredit, Amount: 100},
		{Sequence: 2, Account: "bob", Type: TypeCredit, Amount: 50},
		{Sequence: 3, Account: "alice", Type: TypeDebit, Amount: 30},
	}
	p := NewProjection()
	for _, e := range events {
		p, _ = p.Fold(e)
	}
	fmt.Println(p)
	// Output: alice=70 bob=50
}
```

The online integration test proves the replay end to end. It publishes a ledger,
replays from sequence 3 with an ordered consumer, and asserts the rebuilt
projection equals the pure fold of just the replayed suffix — proving both that
`OptStartSeq` positions the replay correctly and that the loop terminates when
`NumPending` reaches 0. It is behind `//go:build online` and deferred to a
networked run.

Create `rebuilder_online_test.go`:

```go
//go:build online

package jsreplay

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// TestReplayFromSequence is a networked integration test (nats-server -js on
// localhost). It publishes a ledger of events, replays from a chosen start
// sequence with an ordered consumer, and asserts the rebuilt projection matches
// the pure fold of the replayed suffix and that the replay terminates when
// NumPending reaches 0. Excluded from the offline gate; deferred to a networked
// run.
func TestReplayFromSequence(t *testing.T) {
	nc, err := nats.Connect(nats.DefaultURL)
	if err != nil {
		t.Skipf("no nats-server available: %v", err)
	}
	defer nc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream new: %v", err)
	}
	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:     "LEDGER_TEST",
		Subjects: []string{"LEDGER_TEST.>"},
		Storage:  jetstream.MemoryStorage,
	}); err != nil {
		t.Fatalf("create stream: %v", err)
	}

	ledger := []wirePayload{
		{Account: "alice", Type: TypeCredit, Amount: 100},
		{Account: "bob", Type: TypeCredit, Amount: 50},
		{Account: "alice", Type: TypeDebit, Amount: 30},
		{Account: "bob", Type: TypeCredit, Amount: 25},
	}
	for _, w := range ledger {
		body, _ := json.Marshal(w)
		if _, err := js.Publish(ctx, "LEDGER_TEST.tx", body); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	// Replay from sequence 3: only the alice debit and the second bob credit.
	got, err := Rebuild(ctx, js, "LEDGER_TEST", "LEDGER_TEST.>", ReplayStart{
		Kind: StartFromSequence,
		Seq:  3,
	})
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	want := NewProjection()
	want, _ = want.Fold(DecodedEvent{Sequence: 3, Account: "alice", Type: TypeDebit, Amount: 30})
	want, _ = want.Fold(DecodedEvent{Sequence: 4, Account: "bob", Type: TypeCredit, Amount: 25})

	if got.String() != want.String() {
		t.Fatalf("rebuilt projection = %q, want %q", got.String(), want.String())
	}
}
```

## Review

The subtle bugs a replay introduces are all about idempotency and termination.
Folding without an applied-set would double-count when replays overlap; `Fold`
keys idempotency on the stable stream sequence, and
`TestFoldIdempotentReapplication` proves a repeated range does not move the
balances. Mutating the input projection in place would corrupt earlier snapshots
and break the "fold is a pure function" contract; `Fold` clones its maps, and
`TestFoldDoesNotMutateInput` guards it. Blocking forever at the end of history is
the other classic mistake — a naive `Next()` loop never stops — which is why the
loop checks `CaughtUp(NumPending)` and returns the moment the live tail is
reached. Choosing the wrong `DeliverPolicy` (for example `DeliverNewPolicy` when
you meant to rebuild from the start) would silently produce an empty or partial
projection; the `ReplayStart` mapping makes that an explicit, testable choice.

Confirm the offline core with `go test -race ./...`; the fold table and the
`Example` must reproduce exactly. To exercise the real replay, start
`nats-server -js` and run `go test -tags online -run TestReplayFromSequence`; a
passing run shows the projection rebuilt from sequence 3 matching the pure fold of
the suffix, which is the ordered consumer honoring `OptStartSeq` and the loop
stopping at `NumPending == 0`.

## Resources

- [`nats.go/jetstream` README — ordered consumers](https://github.com/nats-io/nats.go/blob/main/jetstream/README.md) — `OrderedConsumer`, `Messages`/`Next`, and gap-free replay.
- [NATS JetStream Consumers — DeliverPolicy](https://docs.nats.io/nats-concepts/jetstream/consumers) — `DeliverAll`/`ByStartSequence`/`ByStartTime` and `NumPending`.
- [`nats.go/jetstream` package reference](https://pkg.go.dev/github.com/nats-io/nats.go/jetstream) — `OrderedConsumerConfig`, `MsgMetadata`, `SequencePair`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-durable-pull-consumer-acks.md](02-durable-pull-consumer-acks.md) | Next: [../04-redis-streams-consumer-groups/00-concepts.md](../04-redis-streams-consumer-groups/00-concepts.md)
