# Exercise 2: A JetStream DLQ Topology — Backoff Ladder, Term-on-Poison, and an Advisory-Driven Relay

This is the mirrors-real-on-the-job exercise: the operable DLQ plumbing a team
wishes it had at 3am. You configure a JetStream consumer with a bounded
redelivery ladder, terminate poison immediately, and — because JetStream has no
built-in DLQ — build the relay that listens for the max-deliveries advisory,
re-fetches the failed message by its stream sequence, and parks it in a
dedicated DLQ stream enriched with failure metadata.

This module is fully self-contained. The parts that carry real bugs — advisory
decoding, the backoff-ladder builder, poison classification, and the failure
envelope — are pure functions with offline tests. The JetStream I/O lives in one
file behind `//go:build integration`; it is compiled only against a real
`nats-server -js`, so the offline gate does not build it. Nothing here imports
another exercise.

## What you'll build

```text
jsdlq/                        independent module: example.com/jsdlq
  go.mod                      go 1.26; requires nats.go under the integration tag
  advisory.go                 MaxDeliverAdvisory; ParseMaxDeliverAdvisory; subject builder (pure)
  ladder.go                   BuildBackoffLadder; ErrPoison/IsPoison (pure)
  envelope.go                 FailureInfo; Headers() failure envelope (pure)
  relay_integration.go        //go:build integration — consumer, ack/nak/term, DLQ relay
  cmd/
    demo/
      main.go                 runnable pure demo: parse an advisory, build an envelope
  jsdlq_test.go               advisory + envelope table tests + Examples
  ladder_test.go              backoff-ladder + poison-classifier tests + Example
  relay_integration_test.go   //go:build integration — end-to-end poison-to-DLQ test
```

- Files: `advisory.go`, `ladder.go`, `envelope.go`, `relay_integration.go`, `cmd/demo/main.go`, `jsdlq_test.go`, `ladder_test.go`, `relay_integration_test.go`.
- Implement: `ParseMaxDeliverAdvisory` (validate schema + extract `stream_seq`), `BuildBackoffLadder` (capped exponential slice for `ConsumerConfig.BackOff`), `IsPoison`, and `FailureInfo.Headers`; the integration `Consume` (ack/nak/term) and `Relay` (advisory → re-fetch → park).
- Test: offline table-driven tests for advisory decoding, the ladder, poison classification, and the envelope, with `errors.Is` and `Example`s; a `//go:build integration` end-to-end test that exhausts `MaxDeliver` and asserts the message lands in the DLQ stream with a full envelope.
- Verify: `go test -count=1 -race ./...` (offline core); the integration test runs against a real `nats-server -js`.

Set up the module. The pure core needs no dependency; the integration file does:

```bash
mkdir -p go-solutions/50-messaging-and-event-driven/10-dead-letter-and-retry-topologies/02-jetstream-dlq-topology/cmd/demo
cd go-solutions/50-messaging-and-event-driven/10-dead-letter-and-retry-topologies/02-jetstream-dlq-topology
go mod edit -go=1.26
go get github.com/nats-io/nats.go   # only needed to compile/run with -tags integration
```

### Why a relay at all: JetStream has no DLQ

JetStream will stop redelivering a message after `MaxDeliver` attempts, but it
does not move it anywhere — it simply stops, and the message stays in the stream
until it ages out. What JetStream *does* do is publish an *advisory*: a small
JSON event on `$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.<STREAM>.<CONSUMER>`,
with schema `io.nats.jetstream.advisory.v1.max_deliver`, carrying the
`stream_seq` of the message that exhausted its deliveries. The DLQ pattern on
JetStream is therefore a *relay*: subscribe to that advisory, read the
`stream_seq`, re-fetch the original message from the source stream by sequence,
wrap it in a failure envelope, and republish it to a dedicated DLQ stream. The
re-fetch matters — the advisory does not carry the message body, so trusting it
alone would give you a DLQ entry with no payload. Fetching by sequence produces
a faithful copy.

### The pure core carries the bugs

The JetStream calls themselves are thin: `CreateOrUpdateConsumer`, `Consume`,
`GetMsg`, `PublishMsg`. The parts that actually break are decisions and parsing,
so they live in pure files. `ParseMaxDeliverAdvisory` validates the schema type
(a wildcard subscription can receive other advisory types on adjacent subjects,
and a wrong type must be dropped, not parked) and rejects a zero `stream_seq`
(which would make the re-fetch impossible) — each as a wrapped sentinel so the
relay can log and drop rather than crash. `FailureInfo.Headers` builds the DLQ
envelope as a stdlib `map[string][]string`, the same shape as `nats.Header`, so
the envelope logic is testable with no NATS dependency; the integration adapter
copies these into a real header before republishing.

Create `advisory.go`:

```go
package jsdlq

import (
	"encoding/json"
	"errors"
	"fmt"
)

// MaxDeliverSchema is the schema tag JetStream stamps on a max-deliveries
// advisory. The relay validates it so an unrelated advisory on the same wildcard
// subject is never mistaken for a poison notification.
const MaxDeliverSchema = "io.nats.jetstream.advisory.v1.max_deliver"

// Sentinel errors, wrapped with %w so callers assert with errors.Is.
var (
	ErrWrongSchema = errors.New("advisory: wrong schema type")
	ErrNoStreamSeq = errors.New("advisory: missing stream_seq")
	ErrBadJSON     = errors.New("advisory: invalid json")
)

// MaxDeliverAdvisory is the payload JetStream publishes to
// $JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.<STREAM>.<CONSUMER> when a message
// has been redelivered MaxDeliver times without an ack. Its stream_seq is the
// key: it points at the message in the stream so the relay can re-fetch and park
// it. Only the fields the relay needs are decoded.
type MaxDeliverAdvisory struct {
	Type       string `json:"type"`
	ID         string `json:"id"`
	Stream     string `json:"stream"`
	Consumer   string `json:"consumer"`
	StreamSeq  uint64 `json:"stream_seq"`
	Deliveries int    `json:"deliveries"`
}

// ParseMaxDeliverAdvisory decodes and validates a raw advisory. It rejects a
// wrong schema type and a zero stream_seq (which would make the re-fetch
// impossible), each as a wrapped sentinel so the relay can log and drop rather
// than crash on an unexpected advisory.
func ParseMaxDeliverAdvisory(data []byte) (MaxDeliverAdvisory, error) {
	var a MaxDeliverAdvisory
	if err := json.Unmarshal(data, &a); err != nil {
		return MaxDeliverAdvisory{}, fmt.Errorf("%w: %v", ErrBadJSON, err)
	}
	if a.Type != MaxDeliverSchema {
		return MaxDeliverAdvisory{}, fmt.Errorf("%w: got %q", ErrWrongSchema, a.Type)
	}
	if a.StreamSeq == 0 {
		return MaxDeliverAdvisory{}, fmt.Errorf("%w", ErrNoStreamSeq)
	}
	return a, nil
}

// MaxDeliverSubject builds the advisory subject to subscribe to for one
// stream/consumer pair. A relay can also subscribe to the wildcard
// $JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.> and validate each advisory.
func MaxDeliverSubject(stream, consumer string) string {
	return fmt.Sprintf("$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.%s.%s", stream, consumer)
}
```

### The backoff ladder is broker-side, not in-process

JetStream's `ConsumerConfig` takes a `BackOff []time.Duration`: the delay before
each redelivery, applied by the server on `Nak`. This is the in-broker delay the
concepts file argued for — a plain `msg.Nak()` schedules the next attempt using
the ladder, so the worker is freed immediately and no ack lease is held during
the wait. `BuildBackoffLadder` produces that slice with the same capped
exponential reasoning as Exercise 1's schedule, and the rule that ties it to the
counter is: set `MaxDeliver` to `len(ladder)+1`, so every rung is used once
before JetStream gives up and fires the advisory. `ErrPoison`/`IsPoison` is the
classification hook: a handler that wraps `ErrPoison` makes the consumer
`TermWithReason` instead of `Nak`, so poison never enters the ladder at all.

Create `ladder.go`:

```go
package jsdlq

import (
	"errors"
	"time"
)

// ErrPoison marks a terminal, non-retryable failure. A handler returns an error
// wrapping ErrPoison (fmt.Errorf("bad payload: %w", ErrPoison)) to make the
// consumer TermWithReason the message instead of Nak-ing it for redelivery.
var ErrPoison = errors.New("poison message")

// IsPoison reports whether err (or anything it wraps) is a terminal failure.
func IsPoison(err error) bool { return errors.Is(err, ErrPoison) }

// BuildBackoffLadder builds the per-redelivery delay slice for a JetStream
// consumer's BackOff field: an exponential, capped ladder of length steps. Set
// the consumer's MaxDeliver to steps+1 so every rung is used once before the
// message is terminated. JetStream applies these delays on Nak automatically, so
// the wait happens broker-side and never holds a worker slot.
func BuildBackoffLadder(base, max time.Duration, mult float64, steps int) []time.Duration {
	if steps <= 0 {
		return nil
	}
	if mult < 1 {
		mult = 1
	}
	ladder := make([]time.Duration, steps)
	d := float64(base)
	for i := range steps {
		cur := time.Duration(d)
		if max > 0 && cur > max {
			cur = max
		}
		ladder[i] = cur
		d *= mult
		if d > 1e18 { // guard the time.Duration int64 range
			d = 1e18
		}
	}
	return ladder
}
```

### The failure envelope

Create `envelope.go`:

```go
package jsdlq

import "strconv"

// DLQ envelope header keys. A parked message carries enough context to triage
// and redrive it without opening the payload: why it died, how many times it was
// tried, and where it came from.
const (
	HeaderReason   = "Dlq-Reason"
	HeaderAttempts = "Dlq-Attempts"
	HeaderSubject  = "Dlq-Orig-Subject"
	HeaderSeq      = "Dlq-Orig-Seq"
	HeaderStream   = "Dlq-Orig-Stream"
	HeaderConsumer = "Dlq-Orig-Consumer"
)

// FailureInfo is the metadata attached to a message when it is parked in the DLQ
// stream. Building it as a plain struct with a stdlib-typed Headers method keeps
// the envelope logic unit-testable with no NATS dependency; the online adapter
// copies these into a nats.Header before republishing.
type FailureInfo struct {
	Reason        string
	Attempts      int
	OrigSubject   string
	OrigStreamSeq uint64
	Stream        string
	Consumer      string
}

// Headers renders the failure metadata as a stdlib header map (the same shape as
// nats.Header, which is a map[string][]string). Deterministic ordering is not
// required because header maps are unordered by construction.
func (f FailureInfo) Headers() map[string][]string {
	return map[string][]string{
		HeaderReason:   {f.Reason},
		HeaderAttempts: {strconv.Itoa(f.Attempts)},
		HeaderSubject:  {f.OrigSubject},
		HeaderSeq:      {strconv.FormatUint(f.OrigStreamSeq, 10)},
		HeaderStream:   {f.Stream},
		HeaderConsumer: {f.Consumer},
	}
}
```

### The JetStream adapter (integration)

This is the thin edge, and it is behind `//go:build integration` because it
imports `github.com/nats-io/nats.go`. `DLQConsumerConfig` sets `MaxDeliver =
len(ladder)+1`, the `BackOff` ladder, and an `AckWait`. `Consume` wires the
handler to the ack/nak/term decision: `nil` acks, a poison error terminates with
a reason so it never loops, and any other error `Nak`s — deliberately with *no*
delay argument, because the configured `BackOff` ladder supplies the interval
broker-side. `Relay.Start` subscribes to the advisory, and `park` re-fetches the
failed message with `Stream.GetMsg(ctx, seq)`, copies the failure envelope into a
`nats.Header`, and republishes to the DLQ subject with `PublishMsg`.

Create `relay_integration.go`:

```go
//go:build integration

// This file holds the JetStream I/O for the DLQ topology. It is excluded from
// the default build (the offline gate) and compiled only with -tags integration
// against a real nats-server with JetStream, because it imports
// github.com/nats-io/nats.go. The pure logic it relies on (advisory parsing, the
// backoff ladder, the failure envelope, poison classification) lives in the
// untagged files and is tested offline.
package jsdlq

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// DLQConsumerConfig builds a consumer whose redelivery topology is a bounded,
// broker-side backoff ladder. MaxDeliver is len(ladder)+1 so each rung is used
// once before JetStream stops redelivering and fires the max-deliveries advisory
// the relay listens for.
func DLQConsumerConfig(durable, filterSubject string, ladder []time.Duration, ackWait time.Duration) jetstream.ConsumerConfig {
	return jetstream.ConsumerConfig{
		Durable:       durable,
		FilterSubject: filterSubject,
		AckPolicy:     jetstream.AckExplicitPolicy,
		MaxDeliver:    len(ladder) + 1,
		BackOff:       ladder,
		AckWait:       ackWait,
	}
}

// Process is the business handler. Returning nil acks; returning an error that
// wraps ErrPoison terminates the message; any other error naks it for
// broker-side backoff redelivery.
type Process func(ctx context.Context, msg jetstream.Msg) error

// Consume wires Process to the ack/nak/term decision. It does not sleep for the
// backoff: a plain Nak lets JetStream apply the consumer's BackOff ladder, so the
// wait is broker-side and the worker is freed immediately. A poison error is
// terminated with a reason so it never loops, and lands in the advisory only via
// MaxDeliver exhaustion for transient failures.
func Consume(ctx context.Context, cons jetstream.Consumer, process Process) (jetstream.ConsumeContext, error) {
	return cons.Consume(func(msg jetstream.Msg) {
		err := process(ctx, msg)
		switch {
		case err == nil:
			if ackErr := msg.Ack(); ackErr != nil {
				// A failed ack means the message will be redelivered; nothing
				// else to do but let the ladder handle it.
				_ = ackErr
			}
		case IsPoison(err):
			_ = msg.TermWithReason(err.Error())
		default:
			// Nak with no delay: JetStream uses the configured BackOff ladder for
			// the redelivery interval, keyed on how many times it was delivered.
			_ = msg.Nak()
		}
	})
}

// Relay parks poison messages in a dedicated DLQ stream. It subscribes to the
// max-deliveries advisory for a stream/consumer, re-fetches the failed message by
// its stream sequence, wraps it in a failure envelope, and republishes it to the
// DLQ subject. Re-fetching by sequence (rather than trusting the advisory to
// carry the body) is what makes the DLQ entry a faithful copy of the original.
type Relay struct {
	nc      *nats.Conn
	js      jetstream.JetStream
	src     jetstream.Stream
	dlqSubj string
}

// NewRelay builds a relay reading from the source stream and writing parked
// messages to dlqSubject (which must be covered by a separate DLQ stream).
func NewRelay(nc *nats.Conn, js jetstream.JetStream, src jetstream.Stream, dlqSubject string) *Relay {
	return &Relay{nc: nc, js: js, src: src, dlqSubj: dlqSubject}
}

// Start subscribes to the advisory subject and parks each poison message. The
// returned subscription is drained by the caller on shutdown.
func (r *Relay) Start(ctx context.Context, stream, consumer string) (*nats.Subscription, error) {
	subj := MaxDeliverSubject(stream, consumer)
	return r.nc.Subscribe(subj, func(m *nats.Msg) {
		adv, err := ParseMaxDeliverAdvisory(m.Data)
		if err != nil {
			return // not a max-deliver advisory we can act on
		}
		if perr := r.park(ctx, adv); perr != nil {
			// In production this increments a metric and logs; the advisory will
			// not be re-sent, so a park failure needs its own alert.
			_ = perr
		}
	})
}

// park re-fetches the failed message by sequence and republishes it to the DLQ
// subject with a failure envelope.
func (r *Relay) park(ctx context.Context, adv MaxDeliverAdvisory) error {
	raw, err := r.src.GetMsg(ctx, adv.StreamSeq)
	if err != nil {
		return fmt.Errorf("get msg seq %d: %w", adv.StreamSeq, err)
	}
	info := FailureInfo{
		Reason:        "max deliveries exceeded",
		Attempts:      adv.Deliveries,
		OrigSubject:   raw.Subject,
		OrigStreamSeq: adv.StreamSeq,
		Stream:        adv.Stream,
		Consumer:      adv.Consumer,
	}
	out := nats.NewMsg(r.dlqSubj)
	out.Data = raw.Data
	for k, vs := range info.Headers() {
		for _, v := range vs {
			out.Header.Add(k, v)
		}
	}
	if _, err := r.js.PublishMsg(ctx, out); err != nil {
		return fmt.Errorf("publish to dlq %s: %w", r.dlqSubj, err)
	}
	return nil
}
```

### The runnable demo

The demo stays offline: it parses a real max-deliveries advisory, prints the
extracted sequence and the subject the relay subscribes to, builds a failure
envelope, and shows that an advisory of the wrong schema is rejected. It touches
only the pure functions, so it runs with no server.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/jsdlq"
)

func main() {
	// A real max-deliveries advisory as JetStream emits it (fields trimmed to
	// what the relay reads).
	raw := []byte(`{
		"type": "io.nats.jetstream.advisory.v1.max_deliver",
		"id": "l3TQF8...",
		"stream": "ORDERS",
		"consumer": "fulfilment",
		"stream_seq": 4242,
		"deliveries": 5
	}`)

	adv, err := jsdlq.ParseMaxDeliverAdvisory(raw)
	if err != nil {
		fmt.Println("parse error:", err)
		return
	}
	fmt.Printf("poison: stream=%s consumer=%s seq=%d deliveries=%d\n",
		adv.Stream, adv.Consumer, adv.StreamSeq, adv.Deliveries)
	fmt.Println("advisory subject:", jsdlq.MaxDeliverSubject(adv.Stream, adv.Consumer))

	info := jsdlq.FailureInfo{
		Reason:        "max deliveries exceeded",
		Attempts:      adv.Deliveries,
		OrigSubject:   "ORDERS.new",
		OrigStreamSeq: adv.StreamSeq,
		Stream:        adv.Stream,
		Consumer:      adv.Consumer,
	}
	h := info.Headers()
	fmt.Println("dlq envelope headers:")
	for _, k := range []string{
		jsdlq.HeaderReason, jsdlq.HeaderAttempts, jsdlq.HeaderSubject,
		jsdlq.HeaderSeq, jsdlq.HeaderStream, jsdlq.HeaderConsumer,
	} {
		fmt.Printf("  %s: %s\n", k, h[k][0])
	}

	// A stray advisory of another type is rejected, not parked.
	_, err = jsdlq.ParseMaxDeliverAdvisory([]byte(`{"type":"io.nats.jetstream.advisory.v1.api"}`))
	fmt.Println("wrong-schema rejected:", err != nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
poison: stream=ORDERS consumer=fulfilment seq=4242 deliveries=5
advisory subject: $JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.ORDERS.fulfilment
dlq envelope headers:
  Dlq-Reason: max deliveries exceeded
  Dlq-Attempts: 5
  Dlq-Orig-Subject: ORDERS.new
  Dlq-Orig-Seq: 4242
  Dlq-Orig-Stream: ORDERS
  Dlq-Orig-Consumer: fulfilment
wrong-schema rejected: true
```

### Tests

The offline tests are the real proof. `TestParseMaxDeliverAdvisory` is
table-driven over a valid advisory and each failure mode (wrong schema, zero
`stream_seq`, malformed JSON), matching every error with `errors.Is`.
`TestMaxDeliverSubject` pins the exact advisory subject.
`TestFailureInfoHeaders` proves every envelope key is present with the right
value. `TestBuildBackoffLadder` pins the capped exponential slice (with the last
rung capped) and the nil-on-zero-steps case, and `TestIsPoison` proves the
classifier matches a wrapped `ErrPoison` and rejects a plain error. Two
`Example`s lock the advisory decode and the ladder.

Create `jsdlq_test.go`:

```go
package jsdlq

import (
	"errors"
	"fmt"
	"testing"
)

func TestParseMaxDeliverAdvisory(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		data    string
		wantErr error
		wantSeq uint64
	}{
		{
			name:    "valid",
			data:    `{"type":"io.nats.jetstream.advisory.v1.max_deliver","stream":"ORDERS","consumer":"fulfilment","stream_seq":99,"deliveries":5}`,
			wantSeq: 99,
		},
		{
			name:    "wrong schema",
			data:    `{"type":"io.nats.jetstream.advisory.v1.api","stream_seq":99}`,
			wantErr: ErrWrongSchema,
		},
		{
			name:    "zero stream_seq",
			data:    `{"type":"io.nats.jetstream.advisory.v1.max_deliver","stream_seq":0}`,
			wantErr: ErrNoStreamSeq,
		},
		{
			name:    "malformed json",
			data:    `{not json`,
			wantErr: ErrBadJSON,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			adv, err := ParseMaxDeliverAdvisory([]byte(tc.data))
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want errors.Is %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if adv.StreamSeq != tc.wantSeq {
				t.Errorf("StreamSeq = %d, want %d", adv.StreamSeq, tc.wantSeq)
			}
		})
	}
}

func TestMaxDeliverSubject(t *testing.T) {
	t.Parallel()
	got := MaxDeliverSubject("ORDERS", "fulfilment")
	want := "$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.ORDERS.fulfilment"
	if got != want {
		t.Fatalf("subject = %q, want %q", got, want)
	}
}

func TestFailureInfoHeaders(t *testing.T) {
	t.Parallel()
	info := FailureInfo{
		Reason:        "max deliveries exceeded",
		Attempts:      5,
		OrigSubject:   "ORDERS.new",
		OrigStreamSeq: 4242,
		Stream:        "ORDERS",
		Consumer:      "fulfilment",
	}
	h := info.Headers()
	want := map[string]string{
		HeaderReason:   "max deliveries exceeded",
		HeaderAttempts: "5",
		HeaderSubject:  "ORDERS.new",
		HeaderSeq:      "4242",
		HeaderStream:   "ORDERS",
		HeaderConsumer: "fulfilment",
	}
	for k, v := range want {
		got, ok := h[k]
		if !ok || len(got) != 1 || got[0] != v {
			t.Errorf("header %q = %v, want [%q]", k, got, v)
		}
	}
}

func ExampleParseMaxDeliverAdvisory() {
	raw := []byte(`{"type":"io.nats.jetstream.advisory.v1.max_deliver","stream":"ORDERS","consumer":"fulfilment","stream_seq":4242,"deliveries":5}`)
	adv, err := ParseMaxDeliverAdvisory(raw)
	fmt.Println(err)
	fmt.Printf("%s/%s seq=%d\n", adv.Stream, adv.Consumer, adv.StreamSeq)
	// Output:
	// <nil>
	// ORDERS/fulfilment seq=4242
}
```

Create `ladder_test.go`:

```go
package jsdlq

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestBuildBackoffLadder(t *testing.T) {
	t.Parallel()
	got := BuildBackoffLadder(time.Second, 30*time.Second, 2, 6)
	want := []time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second,
		8 * time.Second, 16 * time.Second, 30 * time.Second, // 32s capped to 30s
	}
	if len(got) != len(want) {
		t.Fatalf("ladder len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ladder[%d] = %s, want %s", i, got[i], want[i])
		}
	}
	if BuildBackoffLadder(time.Second, 0, 2, 0) != nil {
		t.Error("zero steps should yield a nil ladder")
	}
}

func TestIsPoison(t *testing.T) {
	t.Parallel()
	if !IsPoison(fmt.Errorf("bad payload: %w", ErrPoison)) {
		t.Error("wrapped ErrPoison should be poison")
	}
	if IsPoison(errors.New("timeout")) {
		t.Error("a plain error must not be classified poison")
	}
}

func ExampleBuildBackoffLadder() {
	for _, d := range BuildBackoffLadder(time.Second, 30*time.Second, 2, 6) {
		fmt.Println(d)
	}
	// Output:
	// 1s
	// 2s
	// 4s
	// 8s
	// 16s
	// 30s
}
```

The integration test proves the whole topology end to end against a real server.
It is behind `//go:build integration`, publishes one message whose handler always
fails transiently, lets the consumer exhaust its short ladder, and asserts the
relay parked the message in the DLQ stream with the failure envelope and the
original body. It is deferred to a networked run and never compiled by the
offline gate.

Create `relay_integration_test.go`:

```go
//go:build integration

package jsdlq

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// TestPoisonRelayEndToEnd is a networked integration test. It is excluded from
// the offline gate and requires a nats-server with JetStream on localhost:4222
// (run: `nats-server -js`). It publishes one message whose handler always fails
// transiently, lets the consumer exhaust its short redelivery ladder, and asserts
// the relay parked the message in the DLQ stream with a full failure envelope.
func TestPoisonRelayEndToEnd(t *testing.T) {
	nc, err := nats.Connect(nats.DefaultURL)
	if err != nil {
		t.Skipf("no nats-server available: %v", err)
	}
	defer nc.Close()

	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream.New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const stream, consumer, subject = "ORDERS_IT", "fulfilment_it", "ORDERS_IT.new"
	const dlqStream, dlqSubject = "ORDERS_IT_DLQ", "DLQ.ORDERS_IT"

	src, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:     stream,
		Subjects: []string{"ORDERS_IT.>"},
		Storage:  jetstream.MemoryStorage,
	})
	if err != nil {
		t.Fatalf("create source stream: %v", err)
	}
	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:     dlqStream,
		Subjects: []string{"DLQ.>"},
		Storage:  jetstream.MemoryStorage,
	}); err != nil {
		t.Fatalf("create dlq stream: %v", err)
	}

	// A short ladder so the test exhausts deliveries quickly.
	ladder := BuildBackoffLadder(50*time.Millisecond, 200*time.Millisecond, 2, 2)
	cons, err := js.CreateOrUpdateConsumer(ctx, stream,
		DLQConsumerConfig(consumer, subject, ladder, time.Second))
	if err != nil {
		t.Fatalf("create consumer: %v", err)
	}

	relay := NewRelay(nc, js, src, dlqSubject)
	sub, err := relay.Start(ctx, stream, consumer)
	if err != nil {
		t.Fatalf("relay start: %v", err)
	}
	defer sub.Unsubscribe()

	// Handler that always fails transiently, so the message exhausts MaxDeliver.
	cc, err := Consume(ctx, cons, func(context.Context, jetstream.Msg) error {
		return errors.New("dependency down")
	})
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	defer cc.Stop()

	if _, err := js.Publish(ctx, subject, []byte(`{"order":1}`)); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Poll the DLQ stream until the parked message shows up.
	dlq, err := js.Stream(ctx, dlqStream)
	if err != nil {
		t.Fatalf("lookup dlq stream: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		raw, gerr := dlq.GetMsg(ctx, 1)
		if gerr == nil {
			if got := raw.Header.Get(HeaderReason); got != "max deliveries exceeded" {
				t.Fatalf("dlq reason header = %q, want %q", got, "max deliveries exceeded")
			}
			if got := raw.Header.Get(HeaderSubject); got != subject {
				t.Fatalf("dlq orig-subject header = %q, want %q", got, subject)
			}
			if string(raw.Data) != `{"order":1}` {
				t.Fatalf("dlq body = %q, want original payload", raw.Data)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("message never parked in DLQ: %v", gerr)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
```

## Review

The topology is correct when four things hold. First, the classifier terminates
poison before it can enter the ladder: `Consume` calls `TermWithReason` for a
wrapped `ErrPoison` and `Nak` for everything else, so a malformed payload is
parked immediately rather than redelivered `MaxDeliver` times. Second, the ladder
and the counter agree: `MaxDeliver = len(ladder)+1`, so a transient failure walks
every rung exactly once and then fires the advisory — off-by-one here either DLQs
early or grants an extra delivery. Third, the relay validates the advisory schema
and a non-zero `stream_seq` before acting, so a stray advisory on the wildcard
subject is dropped, not parked with an empty body. Fourth, `park` re-fetches by
sequence, so the DLQ entry is a faithful copy with the original payload plus the
envelope.

The mistakes to avoid: do not `Nak` poison (it loops forever and never DLQs); do
not sleep in the handler for the backoff (it holds the `AckWait` lease and causes
redelivery to another consumer) — let the broker-side `BackOff` ladder do the
waiting on a plain `Nak`; and do not trust the advisory to carry the body, since
it only carries `stream_seq`. Confirm the offline core with
`go test -count=1 -race ./...`; to exercise the real path, start `nats-server
-js` and run `go test -tags integration -run TestPoisonRelayEndToEnd`, which
publishes one always-failing message and asserts it lands in the DLQ stream with
a full envelope.

## Resources

- [NATS JetStream Consumers](https://docs.nats.io/nats-concepts/jetstream/consumers) — `MaxDeliver`, `BackOff`, `AckWait`, and the max-deliveries advisory.
- [`nats.go/jetstream` package reference](https://pkg.go.dev/github.com/nats-io/nats.go/jetstream) — `ConsumerConfig`, `Consume`, `Msg.Nak`/`TermWithReason`, `Stream.GetMsg`, `PublishMsg`.
- [NATS System Events and Advisories](https://docs.nats.io/running-a-nats-service/nats_admin/monitoring/jetstream_and_system_events) — the advisory subjects and their schemas, including `MAX_DELIVERIES`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-retry-policy-engine.md](01-retry-policy-engine.md) | Next: [03-redis-streams-poison-reclaimer.md](03-redis-streams-poison-reclaimer.md)
