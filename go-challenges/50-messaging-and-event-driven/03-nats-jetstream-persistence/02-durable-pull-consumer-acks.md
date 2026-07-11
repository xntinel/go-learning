# Exercise 2: A Durable Pull Consumer with Acks, Redelivery, and Poison Handling

The consumer, not the stream, owns delivery position and retry policy, so a
worker's correctness lives in how it acknowledges each message. This exercise
builds a durable pull consumer whose per-message decision — Ack, Nak with
backoff, or Term — is a pure function of the handler's outcome and the delivery
count, so the retry topology and the poison ceiling are unit-tested with no
broker.

This module is fully self-contained. It begins with its own `go mod init`,
defines every type it needs, and ships its own demo and tests. Nothing here
imports any other exercise.

## What you'll build

```text
jsworker/                    independent module: example.com/jsworker
  go.mod                     go 1.26
  policy.go                  Outcome/Action enums; Classify; Decide; BackoffFor (pure)
  worker_online.go           //go:build online — durable pull consumer + ack loop
  cmd/
    demo/
      main.go                runnable pure demo: the decision table for each attempt
  worker_test.go             offline table-driven tests + ExampleDecide_poison + a stub
  worker_online_test.go      //go:build online — real redelivery/term integration test
```

- Files: `policy.go`, `worker_online.go`, `cmd/demo/main.go`, `worker_test.go`, `worker_online_test.go`.
- Implement: `Classify(error) Outcome` (nil/poison/retryable), `Decide(Outcome, numDelivered, maxDeliver, backoff) AckDecision` (Ack/Nak+delay/Term with the poison ceiling), and `BackoffFor(numDelivered, backoff)`; the online `Worker` binds a durable pull consumer via `Consume` and applies the decision, using `DoubleAck` on success and `InProgress` heartbeats for long work.
- Test: offline table-driven tests of `Classify`, `Decide` (Term exactly at the ceiling, monotonic backoff, unlimited-never-terms) and `BackoffFor`, with sentinel errors via `errors.Is` and an `Example` with `// Output:`; a `//go:build online` test that forces redelivery and asserts termination at `MaxDeliver`.
- Verify: `go test -count=1 -race ./...` (offline core); the online test runs against a real `nats-server -js`.

Set up the module:

```bash
mkdir -p ~/go-exercises/jsworker/cmd/demo
cd ~/go-exercises/jsworker
go mod init example.com/jsworker
go mod edit -go=1.26
```

### The decision policy is the whole game

With `AckExplicitPolicy` every delivered message needs exactly one terminal
signal, and choosing the wrong one is a production incident: forget to ack and the
message redelivers until `MaxDeliver` (forever, if that is unlimited); `Nak` a
message that can never succeed and it poison-loops; `Ack` a message whose side
effect might be lost and you risk a duplicate. So the interesting logic is a small
state machine — outcome plus attempt number maps to an action — and that state
machine is exactly the kind of thing that should be a pure function you can
enumerate in a table test.

`Classify` reduces a handler's error to one of three outcomes. A nil error is
success. An error wrapping `ErrPoison` is permanent. Everything else — including
an error wrapping `ErrRetryable` and any error the handler did not classify — is
treated as retryable. Defaulting the unknown case to retryable is a deliberate
safety choice: an unexpected error is retried up to the ceiling rather than
silently dropped, and if it really is unrecoverable the ceiling eventually turns
it into a Term.

`Decide` maps `(outcome, numDelivered, maxDeliver, backoff)` to an `AckDecision`.
Success acks. Permanent terms immediately — there is no point burning retries on a
message that will never succeed. Retryable naks with a backoff delay *until* the
delivery count reaches `maxDeliver`, at which point it terms. Terminating exactly
at the ceiling is the key design decision: once the server has delivered a message
`MaxDeliver` times it will stop on its own, but an explicit `Term` at that moment
converts an exhausted retry into a clear dead-letter signal (and gives you a hook
to route the payload to a DLQ) instead of a message that quietly disappears.
`maxDeliver <= 0` means unlimited, so the ceiling never triggers.

`BackoffFor` indexes the backoff slice by `numDelivered-1`, clamping to the last
element so attempts beyond the slice reuse the final interval — the same
"last interval repeats" rule JetStream's own `BackOff` field uses. This keeps the
Go-side delay consistent with what the server would compute, and makes the delay
monotonic in the attempt number for any non-decreasing slice.

Create `policy.go`:

```go
package jsworker

import (
	"errors"
	"time"
)

// Outcome classifies what a message handler decided about a message.
type Outcome int

const (
	// OutcomeSuccess: processed cleanly; acknowledge and advance.
	OutcomeSuccess Outcome = iota
	// OutcomeRetryable: a transient failure; redeliver and try again, up to the
	// delivery ceiling.
	OutcomeRetryable
	// OutcomePermanent: a poison message that will never succeed; drop it now.
	OutcomePermanent
)

func (o Outcome) String() string {
	switch o {
	case OutcomeSuccess:
		return "success"
	case OutcomeRetryable:
		return "retryable"
	case OutcomePermanent:
		return "permanent"
	default:
		return "unknown"
	}
}

// Action is the JetStream acknowledgement a worker will apply to a message.
type Action int

const (
	// ActionAck: acknowledge success (the worker uses DoubleAck so a lost ack
	// cannot cause a duplicate).
	ActionAck Action = iota
	// ActionNak: negative-acknowledge with a delay; the server redelivers later.
	ActionNak
	// ActionTerm: terminate delivery permanently, regardless of MaxDeliver. This
	// is the poison-message / dead-letter signal.
	ActionTerm
)

func (a Action) String() string {
	switch a {
	case ActionAck:
		return "ack"
	case ActionNak:
		return "nak"
	case ActionTerm:
		return "term"
	default:
		return "unknown"
	}
}

// Sentinel errors a handler wraps with %w to declare intent. Classify inspects
// them with errors.Is.
var (
	// ErrRetryable marks a transient failure worth redelivering.
	ErrRetryable = errors.New("retryable failure")
	// ErrPoison marks a message that will never succeed and must be dropped.
	ErrPoison = errors.New("poison message")
)

// Classify turns a handler error into an Outcome. A nil error is success; an
// error wrapping ErrPoison is permanent; anything else (including an error
// wrapping ErrRetryable and any unclassified error) is treated as retryable, so
// an unexpected error is retried up to the ceiling rather than silently dropped.
func Classify(err error) Outcome {
	switch {
	case err == nil:
		return OutcomeSuccess
	case errors.Is(err, ErrPoison):
		return OutcomePermanent
	default:
		return OutcomeRetryable
	}
}

// AckDecision is the resolved action plus, for a Nak, how long to wait before
// redelivery.
type AckDecision struct {
	Action Action
	Delay  time.Duration
}

// Decide maps a handler outcome and the current delivery attempt onto an ack
// action. This is the pure core of the retry topology and the only place the
// poison ceiling is enforced.
//
//   - Success        -> Ack.
//   - Permanent      -> Term (drop immediately; no point retrying).
//   - Retryable, and this is not yet the last allowed attempt -> Nak with a
//     backoff delay computed from the attempt number.
//   - Retryable, but numDelivered has reached maxDeliver -> Term. Terminating
//     exactly at the ceiling turns an exhausted retry into an explicit
//     dead-letter signal instead of a message the server silently stops
//     redelivering.
//
// numDelivered is Metadata().NumDelivered (1 on the first delivery). maxDeliver
// <= 0 means unlimited, so the ceiling never triggers.
func Decide(o Outcome, numDelivered uint64, maxDeliver int, backoff []time.Duration) AckDecision {
	switch o {
	case OutcomeSuccess:
		return AckDecision{Action: ActionAck}
	case OutcomePermanent:
		return AckDecision{Action: ActionTerm}
	default: // OutcomeRetryable
		if maxDeliver > 0 && numDelivered >= uint64(maxDeliver) {
			return AckDecision{Action: ActionTerm}
		}
		return AckDecision{Action: ActionNak, Delay: BackoffFor(numDelivered, backoff)}
	}
}

// BackoffFor returns the delay before the next redelivery given the current
// attempt number. It indexes the backoff slice by (numDelivered-1), clamping to
// the last element so attempts beyond the slice reuse the final interval — the
// same "last interval repeats" rule JetStream's own BackOff field uses. An empty
// slice yields no delay (immediate redelivery). Because a sane backoff slice is
// non-decreasing, the returned delay is monotonic in numDelivered.
func BackoffFor(numDelivered uint64, backoff []time.Duration) time.Duration {
	if len(backoff) == 0 {
		return 0
	}
	idx := 0
	if numDelivered > 0 {
		idx = int(numDelivered - 1)
	}
	if idx >= len(backoff) {
		idx = len(backoff) - 1
	}
	return backoff[idx]
}
```

### The consumer loop (online)

The online worker binds a durable pull consumer with `CreateOrUpdateConsumer` and
`AckExplicitPolicy`, then drives it with `Consume`, whose callback receives each
`jetstream.Msg`. For every message it reads `Metadata()` (for `NumDelivered`),
runs the handler, asks `Decide` what to do, and applies it: `DoubleAck` on
success (so a lost ack cannot cause a duplicate side effect), `NakWithDelay` for a
retry, or `TermWithReason` for a poison drop. Long-running handlers get
`InProgress` heartbeats on a ticker at a third of `AckWait`, so the server does
not treat a slow-but-healthy handler as failed and redeliver it mid-flight.
`PullMaxMessages` is sized to `MaxAckPending` so outstanding work is bounded, and
`Run` returns the `ConsumeContext.Stop` function so the caller can `defer` it and
avoid leaking the consume loop's goroutines.

Create `worker_online.go`:

```go
//go:build online

// This file holds the JetStream consumer I/O. It is excluded from the default
// build and compiled only with -tags online against a real server. The decision
// policy it applies (Classify, Decide, BackoffFor) is pure and tested offline.
package jsworker

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Handler processes one decoded message body and returns an error whose class
// (via Classify) drives the ack decision. Return nil on success, an error
// wrapping ErrPoison for a permanent failure, or any other error to retry.
type Handler func(ctx context.Context, data []byte) error

// WorkerConfig binds a durable pull consumer. AckWait, MaxDeliver, BackOff and
// MaxAckPending shape the redelivery topology; BackOff overrides AckWait when set.
type WorkerConfig struct {
	Stream        string
	Durable       string
	FilterSubject string
	AckWait       time.Duration
	MaxDeliver    int
	BackOff       []time.Duration
	MaxAckPending int
}

// Worker consumes a stream with a durable pull consumer and applies the pure
// decision policy to every message.
type Worker struct {
	cfg     WorkerConfig
	handler Handler
}

// NewWorker builds a worker; the consumer is created when Run is called.
func NewWorker(cfg WorkerConfig, h Handler) *Worker {
	return &Worker{cfg: cfg, handler: h}
}

// Run creates (or updates) the durable consumer and starts consuming. It returns
// a stop function that must be called to release the consume loop's goroutines.
func (w *Worker) Run(ctx context.Context, js jetstream.JetStream) (func(), error) {
	stream, err := js.Stream(ctx, w.cfg.Stream)
	if err != nil {
		return nil, fmt.Errorf("lookup stream %q: %w", w.cfg.Stream, err)
	}
	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       w.cfg.Durable,
		AckPolicy:     jetstream.AckExplicitPolicy,
		FilterSubject: w.cfg.FilterSubject,
		AckWait:       w.cfg.AckWait,
		MaxDeliver:    w.cfg.MaxDeliver,
		BackOff:       w.cfg.BackOff,
		MaxAckPending: w.cfg.MaxAckPending,
	})
	if err != nil {
		return nil, fmt.Errorf("create consumer %q: %w", w.cfg.Durable, err)
	}

	cc, err := cons.Consume(func(msg jetstream.Msg) {
		w.dispatch(ctx, msg)
	}, jetstream.PullMaxMessages(w.cfg.MaxAckPending))
	if err != nil {
		return nil, fmt.Errorf("consume: %w", err)
	}
	return cc.Stop, nil
}

// dispatch runs the handler and applies the ack decision to one message.
func (w *Worker) dispatch(ctx context.Context, msg jetstream.Msg) {
	md, err := msg.Metadata()
	if err != nil {
		_ = msg.Nak()
		return
	}

	herr := w.processWithHeartbeat(ctx, msg)
	decision := Decide(Classify(herr), md.NumDelivered, w.cfg.MaxDeliver, w.cfg.BackOff)

	switch decision.Action {
	case ActionAck:
		// DoubleAck: wait for the server to confirm, so a lost ack cannot cause
		// a redelivery-driven duplicate side effect.
		_ = msg.DoubleAck(ctx)
	case ActionNak:
		_ = msg.NakWithDelay(decision.Delay)
	case ActionTerm:
		_ = msg.TermWithReason(fmt.Sprintf("poison after %d deliveries: %v", md.NumDelivered, herr))
	}
}

// processWithHeartbeat runs the handler while sending InProgress heartbeats so a
// long-running handler is not treated as failed and redelivered mid-flight. Each
// InProgress resets the consumer's AckWait timer.
func (w *Worker) processWithHeartbeat(ctx context.Context, msg jetstream.Msg) error {
	done := make(chan error, 1)
	go func() { done <- w.handler(ctx, msg.Data()) }()

	beat := time.NewTicker(w.heartbeatInterval())
	defer beat.Stop()
	for {
		select {
		case err := <-done:
			return err
		case <-beat.C:
			_ = msg.InProgress()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// heartbeatInterval sends a heartbeat well within AckWait (a third of it), or a
// fixed fallback when AckWait is unset.
func (w *Worker) heartbeatInterval() time.Duration {
	if w.cfg.AckWait > 0 {
		return w.cfg.AckWait / 3
	}
	return 10 * time.Second
}
```

### The runnable demo

The demo prints the decision table the pure policy produces: a retryable message
climbing the backoff and terminating at the ceiling, a poison message dropped on
first delivery, and a success acked. It runs with no server.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/jsworker"
)

func main() {
	backoff := []time.Duration{time.Second, 5 * time.Second, 30 * time.Second}
	const maxDeliver = 4

	fmt.Println("attempt  outcome    action  delay")
	// A retryable message climbs the backoff and is terminated at the ceiling.
	for attempt := uint64(1); attempt <= maxDeliver; attempt++ {
		d := jsworker.Decide(jsworker.OutcomeRetryable, attempt, maxDeliver, backoff)
		fmt.Printf("%7d  %-9s  %-6s  %s\n", attempt, jsworker.OutcomeRetryable, d.Action, d.Delay)
	}

	// A poison message is dropped on the first delivery.
	p := jsworker.Decide(jsworker.OutcomePermanent, 1, maxDeliver, backoff)
	fmt.Printf("%7d  %-9s  %-6s  %s\n", 1, jsworker.OutcomePermanent, p.Action, p.Delay)

	// A success is acked.
	s := jsworker.Decide(jsworker.OutcomeSuccess, 1, maxDeliver, backoff)
	fmt.Printf("%7d  %-9s  %-6s  %s\n", 1, jsworker.OutcomeSuccess, s.Action, s.Delay)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
attempt  outcome    action  delay
      1  retryable  nak     1s
      2  retryable  nak     5s
      3  retryable  nak     30s
      4  retryable  term    0s
      1  permanent  term    0s
      1  success    ack     0s
```

### Tests

The offline tests enumerate the policy. `TestClassify` covers nil, the poison
sentinel, the retryable sentinel, and an unclassified error. `TestDecide` is a
table over outcome and attempt, asserting the action and the backoff delay per
attempt. `TestDecideTermExactlyAtCeiling` pins the boundary — nak on the
second-to-last attempt, term at the ceiling. `TestDecideUnlimitedNeverTerms`
proves `maxDeliver <= 0` never terminates. `TestBackoffForMonotonic` asserts the
delay never decreases and that the last interval repeats past the slice length.
`ExampleDecide_poison` locks the action for a poison message. The final
`TestBackoffForJitter` is a skipped "your turn" stub.

Create `worker_test.go`:

```go
package jsworker

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want Outcome
	}{
		{"nil is success", nil, OutcomeSuccess},
		{"poison sentinel", fmt.Errorf("decode: %w", ErrPoison), OutcomePermanent},
		{"retryable sentinel", fmt.Errorf("downstream: %w", ErrRetryable), OutcomeRetryable},
		{"unclassified defaults retryable", errors.New("boom"), OutcomeRetryable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Classify(tc.err); got != tc.want {
				t.Errorf("Classify(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestDecide(t *testing.T) {
	t.Parallel()
	backoff := []time.Duration{time.Second, 5 * time.Second, 30 * time.Second}
	const maxDeliver = 4

	tests := []struct {
		name         string
		outcome      Outcome
		numDelivered uint64
		wantAction   Action
		wantDelay    time.Duration
	}{
		{"success acks", OutcomeSuccess, 1, ActionAck, 0},
		{"permanent terms immediately", OutcomePermanent, 1, ActionTerm, 0},
		{"retryable first attempt naks with backoff[0]", OutcomeRetryable, 1, ActionNak, time.Second},
		{"retryable second attempt naks with backoff[1]", OutcomeRetryable, 2, ActionNak, 5 * time.Second},
		{"retryable third attempt naks with backoff[2]", OutcomeRetryable, 3, ActionNak, 30 * time.Second},
		{"retryable terms at ceiling", OutcomeRetryable, maxDeliver, ActionTerm, 0},
		{"retryable terms past ceiling", OutcomeRetryable, maxDeliver + 1, ActionTerm, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Decide(tc.outcome, tc.numDelivered, maxDeliver, backoff)
			if got.Action != tc.wantAction {
				t.Errorf("Action = %v, want %v", got.Action, tc.wantAction)
			}
			if got.Delay != tc.wantDelay {
				t.Errorf("Delay = %v, want %v", got.Delay, tc.wantDelay)
			}
		})
	}
}

func TestDecideTermExactlyAtCeiling(t *testing.T) {
	t.Parallel()
	backoff := []time.Duration{time.Second}
	const maxDeliver = 3
	// The last attempt below the ceiling still naks; at the ceiling it terms.
	if d := Decide(OutcomeRetryable, maxDeliver-1, maxDeliver, backoff); d.Action != ActionNak {
		t.Fatalf("attempt %d: Action = %v, want nak", maxDeliver-1, d.Action)
	}
	if d := Decide(OutcomeRetryable, maxDeliver, maxDeliver, backoff); d.Action != ActionTerm {
		t.Fatalf("attempt %d (ceiling): Action = %v, want term", maxDeliver, d.Action)
	}
}

func TestDecideUnlimitedNeverTerms(t *testing.T) {
	t.Parallel()
	// maxDeliver <= 0 means unlimited: a retryable message always naks.
	d := Decide(OutcomeRetryable, 1000, 0, []time.Duration{time.Second})
	if d.Action != ActionNak {
		t.Fatalf("unlimited: Action = %v, want nak", d.Action)
	}
}

func TestBackoffForMonotonic(t *testing.T) {
	t.Parallel()
	backoff := []time.Duration{time.Second, 5 * time.Second, 30 * time.Second}
	var prev time.Duration
	for attempt := uint64(1); attempt <= 6; attempt++ {
		got := BackoffFor(attempt, backoff)
		if got < prev {
			t.Fatalf("BackoffFor(%d) = %v decreased from %v", attempt, got, prev)
		}
		prev = got
	}
	// Beyond the slice length the last interval repeats.
	if got := BackoffFor(6, backoff); got != 30*time.Second {
		t.Fatalf("BackoffFor(6) = %v, want last interval 30s", got)
	}
}

func TestBackoffForEmpty(t *testing.T) {
	t.Parallel()
	if got := BackoffFor(3, nil); got != 0 {
		t.Fatalf("BackoffFor with empty backoff = %v, want 0", got)
	}
}

// Your turn: add a jittered backoff so many workers retrying at once do not
// thundering-herd the downstream. Implement
//
//	func BackoffForJitter(numDelivered uint64, backoff []time.Duration, frac float64, rng *rand.Rand) time.Duration
//
// returning BackoffFor(...) reduced by up to frac (e.g. 0.2) of a deterministic
// pseudo-random amount, then unskip this test and assert the result stays within
// [base*(1-frac), base] and that a fixed seed is reproducible.
func TestBackoffForJitter(t *testing.T) {
	t.Skip("your turn: implement BackoffForJitter and assert bounds + reproducibility")
}

func ExampleDecide_poison() {
	d := Decide(OutcomePermanent, 1, 5, nil)
	fmt.Println(d.Action)
	// Output: term
}
```

The online integration test proves the redelivery-and-terminate loop against a
real server. It binds a durable consumer with `MaxDeliver=3`, publishes one
message, and on each delivery applies `Decide` (nak below the ceiling, term at
it), asserting `NumDelivered` climbs 1, 2, 3 and that no fourth delivery arrives.
It is behind `//go:build online` and deferred to a networked run.

Create `worker_online_test.go`:

```go
//go:build online

package jsworker

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// TestRedeliveryAndTerm is a networked integration test (nats-server -js on
// localhost). It binds a durable consumer with MaxDeliver=3, publishes one
// message, and on every delivery naks (until the ceiling) or terms (at the
// ceiling) exactly as Decide dictates. It asserts NumDelivered climbs 1,2,3 and
// that no fourth delivery arrives. Excluded from the offline gate; deferred to a
// networked run.
func TestRedeliveryAndTerm(t *testing.T) {
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
	stream, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:     "JOBS_TEST",
		Subjects: []string{"JOBS_TEST.>"},
		Storage:  jetstream.MemoryStorage,
	})
	if err != nil {
		t.Fatalf("create stream: %v", err)
	}
	if _, err := js.Publish(ctx, "JOBS_TEST.work", []byte("boom")); err != nil {
		t.Fatalf("publish: %v", err)
	}

	const maxDeliver = 3
	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:    "jobs_worker",
		AckPolicy:  jetstream.AckExplicitPolicy,
		AckWait:    500 * time.Millisecond,
		MaxDeliver: maxDeliver,
		BackOff:    []time.Duration{100 * time.Millisecond, 100 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("create consumer: %v", err)
	}

	deliveries := make(chan uint64, 8)
	cc, err := cons.Consume(func(msg jetstream.Msg) {
		md, err := msg.Metadata()
		if err != nil {
			_ = msg.Nak()
			return
		}
		deliveries <- md.NumDelivered
		// Always retryable: nak below the ceiling, term at it.
		d := Decide(OutcomeRetryable, md.NumDelivered, maxDeliver, nil)
		switch d.Action {
		case ActionTerm:
			_ = msg.Term()
		default:
			_ = msg.Nak()
		}
	})
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	defer cc.Stop()

	var seen []uint64
	timeout := time.After(10 * time.Second)
	for len(seen) < maxDeliver {
		select {
		case n := <-deliveries:
			seen = append(seen, n)
		case <-timeout:
			t.Fatalf("only saw deliveries %v, want three", seen)
		}
	}
	for i, n := range seen {
		if n != uint64(i+1) {
			t.Fatalf("delivery %d had NumDelivered %d, want %d", i, n, i+1)
		}
	}

	// After Term at the ceiling, no fourth delivery should arrive.
	select {
	case n := <-deliveries:
		t.Fatalf("message redelivered after Term: NumDelivered %d", n)
	case <-time.After(2 * time.Second):
	}
}
```

## Review

The mistakes here are the classic ack bugs. Naking a permanent failure poison-
loops the message up to `MaxDeliver`; `Decide` prevents that by terming on
`OutcomePermanent` immediately, and `Classify` is what routes a poison error
there — so a handler must wrap unrecoverable failures with `ErrPoison`. Forgetting
a terminal action entirely means the message redelivers on `AckWait` forever; the
`dispatch` switch has an arm for every action so every path acks, naks, or terms.
Using a plain `Ack` when a lost ack could double a side effect is why success uses
`DoubleAck`. And setting both `AckWait` and `BackOff` while expecting `AckWait` to
govern timing is a misread of the API: `BackOff` overrides it, which is why
`BackoffFor` mirrors the server's "last interval repeats" rule rather than falling
back to `AckWait`.

Confirm the offline core with `go test -race ./...`: the table must show Term
landing exactly at the ceiling and the backoff never decreasing. To exercise the
real loop, start `nats-server -js` and run
`go test -tags online -run TestRedeliveryAndTerm`; a passing run shows three
deliveries and no fourth, which is the consumer honoring `MaxDeliver` and the
`Term` stopping redelivery.

## Resources

- [NATS JetStream Consumers](https://docs.nats.io/nats-concepts/jetstream/consumers) — pull vs push, AckWait, MaxDeliver, BackOff, MaxAckPending.
- [`nats.go/jetstream` package reference](https://pkg.go.dev/github.com/nats-io/nats.go/jetstream) — `ConsumerConfig`, `Consume`, `Msg` ack methods, `MsgMetadata`.
- [`nats.go/jetstream` migration guide](https://github.com/nats-io/nats.go/blob/main/jetstream/MIGRATION.md) — `DoubleAck`, `TermWithReason`, and the new consumer API.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-durable-stream-publisher.md](01-durable-stream-publisher.md) | Next: [03-stream-replay-projection.md](03-stream-replay-projection.md)
