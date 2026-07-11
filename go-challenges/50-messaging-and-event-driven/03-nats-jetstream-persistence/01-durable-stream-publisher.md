# Exercise 1: Provisioning a Durable Stream and an Idempotent Publisher

A publisher that a service can call safely under retries has two moving parts:
it must declaratively converge a stream to the config it expects, and it must
publish with a deterministic message id so that an at-least-once retry collapses
to a single stored message. This exercise builds both, with the JetStream I/O
kept behind a build tag and the parts that carry real bugs — the config builder
and the id derivation — as pure functions with offline tests.

This module is fully self-contained. It begins with its own `go mod init`,
defines every type it needs, and ships its own demo and tests. Nothing here
imports any other exercise.

## What you'll build

```text
jspublisher/                 independent module: example.com/jspublisher
  go.mod                     go 1.26
  policy.go                  domain types; BuildStreamSpec; sentinel errors (pure)
  msgid.go                   Event; MessageID deterministic id derivation (pure)
  stream_online.go           //go:build online — jetstream.New/CreateOrUpdateStream/Publish
  cmd/
    demo/
      main.go                runnable pure demo: build a spec, derive ids
  publisher_test.go          offline table-driven tests + ExampleMessageID
  publisher_online_test.go   //go:build online — real dedup integration test
```

- Files: `policy.go`, `msgid.go`, `stream_online.go`, `cmd/demo/main.go`, `publisher_test.go`, `publisher_online_test.go`.
- Implement: `BuildStreamSpec(StreamPolicy) (StreamSpec, error)` mapping a domain policy to a resolved spec per retention policy, and `MessageID(Event) string` deriving a stable id from an event's business key; the online `Publisher` maps the spec to `jetstream.StreamConfig` and publishes with `WithMsgID`.
- Test: offline table-driven tests over the config builder and id derivation, sentinel errors asserted with `errors.Is`, and an `Example` with `// Output:`; a `//go:build online` test that publishes the same id twice and asserts the second `PubAck.Duplicate`.
- Verify: `go test -count=1 -race ./...` (offline core); the online test runs against a real `nats-server -js`.

Set up the module:

```bash
mkdir -p ~/go-exercises/jspublisher/cmd/demo
cd ~/go-exercises/jspublisher
go mod init example.com/jspublisher
go mod edit -go=1.26
```

### Why the split: pure decisions, I/O at the edge

The JetStream-specific work in a publisher is thin: call `jetstream.New`, call
`CreateOrUpdateStream`, call `Publish`. The parts that actually break in
production are decisions, not calls: did you choose the right retention policy for
a stream you intend to replay, and is your message id stable enough that a retry
deduplicates? So the design keeps those decisions in pure Go you can unit-test
with no broker, and puts the `nats.go` calls in one file behind `//go:build
online`. The offline gate compiles and tests the pure core; the online file is
excluded from the default build and only compiled with `-tags online` against a
real server.

Concretely, the domain layer never names a JetStream type. `StreamPolicy` is what
a service *intends* ("an ORDERS stream, replayable, on disk, one-day retention,
two-minute dedup window"). `BuildStreamSpec` validates it and produces a
`StreamSpec` whose fields mirror `jetstream.StreamConfig` but use only stdlib
types. The single function that touches JetStream, `toStreamConfig`, lives in the
online file and is a trivial field-for-field mapping. This is the structure to
copy for any broker integration: the mapping to the vendor type is one boring
function; the validation and policy choices are testable.

### Retention as validation, and why a zero dedup window is an error

`BuildStreamSpec` rejects a zero `DedupWindow`. That is not arbitrary strictness:
producer-side deduplication only works inside the stream's `Duplicates` window,
so a publisher that sets `WithMsgID` but leaves the window at zero has an id that
never deduplicates — the exact silent bug this whole exercise exists to prevent.
Making it a validation error means a misconfigured idempotent publisher fails at
startup, not in production under retry. The builder also rejects an empty name,
an empty subject list, an empty subject string, and an out-of-range retention
constant, each as a distinct sentinel error wrapped with `%w` so a caller can
branch on `errors.Is`.

Create `policy.go`:

```go
package jspublisher

import (
	"errors"
	"fmt"
	"time"
)

// Retention names who owns deletion of a stream's messages. It is a correctness
// decision, not a tuning knob: the wrong choice silently discards data.
type Retention int

const (
	// LimitsRetention keeps messages until a limit (MaxAge/MaxMsgs/MaxBytes) is
	// reached; acknowledgement never deletes. Use for replayable event logs.
	LimitsRetention Retention = iota
	// InterestRetention deletes a message once every bound consumer has acked.
	InterestRetention
	// WorkQueueRetention deletes a message on the first ack: a single-owner queue.
	WorkQueueRetention
)

func (r Retention) String() string {
	switch r {
	case LimitsRetention:
		return "limits"
	case InterestRetention:
		return "interest"
	case WorkQueueRetention:
		return "workqueue"
	default:
		return "unknown"
	}
}

// Storage selects durable on-disk (FileStore) vs volatile in-memory (MemoryStore).
type Storage int

const (
	FileStore Storage = iota
	MemoryStore
)

func (s Storage) String() string {
	switch s {
	case FileStore:
		return "file"
	case MemoryStore:
		return "memory"
	default:
		return "unknown"
	}
}

// Sentinel errors, wrapped with %w so callers assert with errors.Is.
var (
	ErrEmptyName        = errors.New("stream policy: empty name")
	ErrNoSubjects       = errors.New("stream policy: no subjects")
	ErrEmptySubject     = errors.New("stream policy: empty subject")
	ErrZeroDedupWindow  = errors.New("stream policy: zero dedup window")
	ErrInvalidRetention = errors.New("stream policy: invalid retention")
)

// StreamPolicy is the domain-level description of a stream: what a service
// intends, expressed in plain terms with no JetStream types. The pure builder
// turns it into a resolved StreamSpec; the online adapter maps that spec onto a
// jetstream.StreamConfig at the edge.
type StreamPolicy struct {
	Name        string
	Subjects    []string
	Retention   Retention
	Storage     Storage
	MaxAge      time.Duration // 0 means no age limit
	DedupWindow time.Duration // Nats-Msg-Id dedup window; must be > 0
}

// StreamSpec is the validated, resolved shape of a stream. It mirrors the fields
// of jetstream.StreamConfig using only stdlib types, so it is unit-testable with
// no broker and no external dependency.
type StreamSpec struct {
	Name       string
	Subjects   []string
	Retention  Retention
	Storage    Storage
	MaxAge     time.Duration
	Duplicates time.Duration
}

// BuildStreamSpec validates a policy and resolves it into a StreamSpec. A zero
// DedupWindow is rejected: without a dedup window, WithMsgID producer-retry
// deduplication cannot work, which is the whole point of an idempotent publisher.
func BuildStreamSpec(p StreamPolicy) (StreamSpec, error) {
	if p.Name == "" {
		return StreamSpec{}, fmt.Errorf("build stream spec: %w", ErrEmptyName)
	}
	if len(p.Subjects) == 0 {
		return StreamSpec{}, fmt.Errorf("build stream spec %q: %w", p.Name, ErrNoSubjects)
	}
	for _, s := range p.Subjects {
		if s == "" {
			return StreamSpec{}, fmt.Errorf("build stream spec %q: %w", p.Name, ErrEmptySubject)
		}
	}
	if p.Retention < LimitsRetention || p.Retention > WorkQueueRetention {
		return StreamSpec{}, fmt.Errorf("build stream spec %q: %w", p.Name, ErrInvalidRetention)
	}
	if p.DedupWindow <= 0 {
		return StreamSpec{}, fmt.Errorf("build stream spec %q: %w", p.Name, ErrZeroDedupWindow)
	}
	subjects := make([]string, len(p.Subjects))
	copy(subjects, p.Subjects)
	return StreamSpec{
		Name:       p.Name,
		Subjects:   subjects,
		Retention:  p.Retention,
		Storage:    p.Storage,
		MaxAge:     p.MaxAge,
		Duplicates: p.DedupWindow,
	}, nil
}
```

### Deriving a stable message id

The id is a pure function of the event's business key, `(Aggregate, Version)`.
Two publishes of the same logical event — an original and its at-least-once retry
— produce the same id, so JetStream stores one message and reports `Duplicate` on
the second. The id deliberately excludes the payload bytes: a retry that
re-serialized the payload with different field ordering is still the same logical
event, and identity is the business key, not the bytes. Using `time.Now()` or a
per-attempt UUID here would give every retry a new id and defeat deduplication
entirely.

Create `msgid.go`:

```go
package jspublisher

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// Event is a domain event a producer publishes. Aggregate identifies the entity
// (e.g. an order id), Version is the per-aggregate revision, and Type names the
// change. Together (Aggregate, Version) is the natural business key: there is at
// most one event per aggregate per version.
type Event struct {
	Aggregate string
	Version   uint64
	Type      string
	Payload   []byte
}

// MessageID derives a stable Nats-Msg-Id from an event's identity. It is a pure
// function of (Aggregate, Version): the same logical event always yields the same
// id, so an at-least-once producer retry reuses the id and JetStream collapses it
// to one stored message inside the stream's Duplicates window. Deriving the id
// from time.Now() or a fresh UUID per attempt would defeat that entirely.
//
// The id excludes Payload deliberately: two publishes of the same (Aggregate,
// Version) are the same logical event even if a retry re-serialized the payload
// with different byte ordering. The business key, not the bytes, defines identity.
func MessageID(e Event) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%d", e.Aggregate, e.Version)
	return hex.EncodeToString(h.Sum(nil))
}
```

### The JetStream adapter (online)

This is the thin edge. `toRetention`/`toStorage`/`toStreamConfig` map the domain
enums to JetStream constants; `EnsureStream` converges the server with
`CreateOrUpdateStream` (idempotent — safe to call on every startup); `Publish`
sends the payload with `WithMsgID(MessageID(e))` and returns the `*PubAck`, whose
`Sequence` is the assigned stream position and whose `Duplicate` flag tells you a
retry was collapsed. The file is behind `//go:build online` because it imports
`github.com/nats-io/nats.go`; it is not part of the offline gate.

Create `stream_online.go`:

```go
//go:build online

// This file holds the JetStream I/O. It is excluded from the default build
// (offline gate) and compiled only with -tags online against a real server,
// because it imports github.com/nats-io/nats.go. The pure logic it depends on
// (BuildStreamSpec, MessageID) lives in the untagged files and is tested offline.
package jspublisher

import (
	"context"
	"fmt"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// toRetention maps the domain retention onto the JetStream constant.
func toRetention(r Retention) jetstream.RetentionPolicy {
	switch r {
	case InterestRetention:
		return jetstream.InterestPolicy
	case WorkQueueRetention:
		return jetstream.WorkQueuePolicy
	default:
		return jetstream.LimitsPolicy
	}
}

// toStorage maps the domain storage onto the JetStream constant.
func toStorage(s Storage) jetstream.StorageType {
	if s == MemoryStore {
		return jetstream.MemoryStorage
	}
	return jetstream.FileStorage
}

// toStreamConfig turns a validated spec into a jetstream.StreamConfig. This is
// the only place the domain model touches JetStream types.
func toStreamConfig(s StreamSpec) jetstream.StreamConfig {
	return jetstream.StreamConfig{
		Name:       s.Name,
		Subjects:   s.Subjects,
		Retention:  toRetention(s.Retention),
		Storage:    toStorage(s.Storage),
		MaxAge:     s.MaxAge,
		Duplicates: s.Duplicates,
	}
}

// Publisher declaratively provisions a stream and publishes idempotently.
type Publisher struct {
	js     jetstream.JetStream
	spec   StreamSpec
	stream jetstream.Stream
}

// NewPublisher connects the JetStream context and validates the policy up front.
func NewPublisher(nc *nats.Conn, p StreamPolicy) (*Publisher, error) {
	spec, err := BuildStreamSpec(p)
	if err != nil {
		return nil, err
	}
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("jetstream new: %w", err)
	}
	return &Publisher{js: js, spec: spec}, nil
}

// EnsureStream creates the stream or updates it to match the spec. It is
// idempotent: calling it on every startup converges the server to the spec.
func (p *Publisher) EnsureStream(ctx context.Context) error {
	s, err := p.js.CreateOrUpdateStream(ctx, toStreamConfig(p.spec))
	if err != nil {
		return fmt.Errorf("create or update stream %q: %w", p.spec.Name, err)
	}
	p.stream = s
	return nil
}

// Publish writes one event to subject with a deterministic Nats-Msg-Id. The
// returned PubAck reports the assigned stream Sequence and whether the server
// treated this publish as a Duplicate (true when a retry with the same id lands
// inside the Duplicates window).
func (p *Publisher) Publish(ctx context.Context, subject string, e Event) (*jetstream.PubAck, error) {
	ack, err := p.js.Publish(ctx, subject, e.Payload, jetstream.WithMsgID(MessageID(e)))
	if err != nil {
		return nil, fmt.Errorf("publish %s: %w", subject, err)
	}
	return ack, nil
}
```

### The runnable demo

The demo stays offline: it builds a spec from a policy, prints the resolved
fields, and shows that an event and its retry derive the same id while a
different version derives a different one. It exercises only the pure functions,
so it runs with no server.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/jspublisher"
)

func main() {
	policy := jspublisher.StreamPolicy{
		Name:        "ORDERS",
		Subjects:    []string{"ORDERS.>"},
		Retention:   jspublisher.LimitsRetention,
		Storage:     jspublisher.FileStore,
		MaxAge:      24 * time.Hour,
		DedupWindow: 2 * time.Minute,
	}

	spec, err := jspublisher.BuildStreamSpec(policy)
	if err != nil {
		fmt.Println("invalid policy:", err)
		return
	}
	fmt.Printf("stream=%s retention=%s storage=%s dedup=%s\n",
		spec.Name, spec.Retention, spec.Storage, spec.Duplicates)

	// A first publish and its at-least-once retry derive the same id, so the
	// server would collapse them to one stored message.
	e := jspublisher.Event{Aggregate: "order-42", Version: 3, Type: "OrderPlaced"}
	retry := jspublisher.Event{Aggregate: "order-42", Version: 3, Type: "OrderPlaced"}
	other := jspublisher.Event{Aggregate: "order-42", Version: 4, Type: "OrderShipped"}

	fmt.Println("id(v3)       =", jspublisher.MessageID(e)[:16])
	fmt.Println("id(v3 retry) =", jspublisher.MessageID(retry)[:16])
	fmt.Println("id(v4)       =", jspublisher.MessageID(other)[:16])
	fmt.Println("retry collapses:", jspublisher.MessageID(e) == jspublisher.MessageID(retry))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
stream=ORDERS retention=limits storage=file dedup=2m0s
id(v3)       = 5e4352d26872e1b0
id(v3 retry) = 5e4352d26872e1b0
id(v4)       = 0110a0bc3dee5032
retry collapses: true
```

### Tests

The offline tests are the real proof. `TestBuildStreamSpec` is table-driven over
each retention policy and each validation failure, asserting the resolved fields
and matching every error with `errors.Is`. `TestBuildStreamSpecCopiesSubjects`
proves the builder does not alias the caller's slice. `TestMessageIDStable` pins
the two invariants that make dedup work: same key gives the same id, distinct
keys give distinct ids. `TestMessageIDCollisionFree` sweeps a grid of keys and
asserts no collisions. `ExampleMessageID` locks the exact id for a fixed event.

Create `publisher_test.go`:

```go
package jspublisher

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestBuildStreamSpec(t *testing.T) {
	t.Parallel()

	base := func() StreamPolicy {
		return StreamPolicy{
			Name:        "ORDERS",
			Subjects:    []string{"ORDERS.>"},
			Retention:   LimitsRetention,
			Storage:     FileStore,
			MaxAge:      time.Hour,
			DedupWindow: time.Minute,
		}
	}

	tests := []struct {
		name          string
		mutate        func(*StreamPolicy)
		wantErr       error
		wantRetention Retention
		wantStorage   Storage
		wantDup       time.Duration
	}{
		{
			name:          "limits policy",
			mutate:        func(p *StreamPolicy) { p.Retention = LimitsRetention },
			wantRetention: LimitsRetention,
			wantStorage:   FileStore,
			wantDup:       time.Minute,
		},
		{
			name:          "interest policy",
			mutate:        func(p *StreamPolicy) { p.Retention = InterestRetention },
			wantRetention: InterestRetention,
			wantStorage:   FileStore,
			wantDup:       time.Minute,
		},
		{
			name:          "workqueue on memory",
			mutate:        func(p *StreamPolicy) { p.Retention = WorkQueueRetention; p.Storage = MemoryStore },
			wantRetention: WorkQueueRetention,
			wantStorage:   MemoryStore,
			wantDup:       time.Minute,
		},
		{
			name:    "empty name",
			mutate:  func(p *StreamPolicy) { p.Name = "" },
			wantErr: ErrEmptyName,
		},
		{
			name:    "no subjects",
			mutate:  func(p *StreamPolicy) { p.Subjects = nil },
			wantErr: ErrNoSubjects,
		},
		{
			name:    "empty subject",
			mutate:  func(p *StreamPolicy) { p.Subjects = []string{"ORDERS.>", ""} },
			wantErr: ErrEmptySubject,
		},
		{
			name:    "zero dedup window",
			mutate:  func(p *StreamPolicy) { p.DedupWindow = 0 },
			wantErr: ErrZeroDedupWindow,
		},
		{
			name:    "invalid retention",
			mutate:  func(p *StreamPolicy) { p.Retention = Retention(99) },
			wantErr: ErrInvalidRetention,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := base()
			tc.mutate(&p)
			spec, err := BuildStreamSpec(p)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("BuildStreamSpec err = %v, want errors.Is %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("BuildStreamSpec unexpected err = %v", err)
			}
			if spec.Retention != tc.wantRetention {
				t.Errorf("Retention = %v, want %v", spec.Retention, tc.wantRetention)
			}
			if spec.Storage != tc.wantStorage {
				t.Errorf("Storage = %v, want %v", spec.Storage, tc.wantStorage)
			}
			if spec.Duplicates != tc.wantDup {
				t.Errorf("Duplicates = %v, want %v", spec.Duplicates, tc.wantDup)
			}
		})
	}
}

func TestBuildStreamSpecCopiesSubjects(t *testing.T) {
	t.Parallel()
	subjects := []string{"ORDERS.new"}
	p := StreamPolicy{
		Name:        "ORDERS",
		Subjects:    subjects,
		DedupWindow: time.Minute,
	}
	spec, err := BuildStreamSpec(p)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	subjects[0] = "MUTATED"
	if spec.Subjects[0] != "ORDERS.new" {
		t.Fatalf("spec aliased caller slice: got %q", spec.Subjects[0])
	}
}

func TestMessageIDStable(t *testing.T) {
	t.Parallel()

	// Same business key -> same id, even if the payload bytes differ (a retry
	// re-serialized). Different version -> different id.
	a := Event{Aggregate: "order-42", Version: 3, Type: "OrderPlaced", Payload: []byte("a")}
	retry := Event{Aggregate: "order-42", Version: 3, Type: "OrderPlaced", Payload: []byte("b")}
	next := Event{Aggregate: "order-42", Version: 4, Type: "OrderShipped"}
	otherAgg := Event{Aggregate: "order-99", Version: 3, Type: "OrderPlaced"}

	if MessageID(a) != MessageID(retry) {
		t.Error("same (aggregate,version) produced different ids; retries would not dedup")
	}
	if MessageID(a) == MessageID(next) {
		t.Error("different version produced the same id; distinct events would collide")
	}
	if MessageID(a) == MessageID(otherAgg) {
		t.Error("different aggregate produced the same id; distinct events would collide")
	}
}

func TestMessageIDCollisionFree(t *testing.T) {
	t.Parallel()
	seen := make(map[string]string)
	for agg := range 50 {
		for v := range uint64(50) {
			e := Event{Aggregate: fmt.Sprintf("agg-%d", agg), Version: v}
			id := MessageID(e)
			key := fmt.Sprintf("%d/%d", agg, v)
			if prev, ok := seen[id]; ok {
				t.Fatalf("id collision: %s and %s share id %s", prev, key, id)
			}
			seen[id] = key
		}
	}
}

func ExampleMessageID() {
	e := Event{Aggregate: "order-42", Version: 3, Type: "OrderPlaced"}
	fmt.Println(MessageID(e))
	// Output: 5e4352d26872e1b0dce6edd9c04446678d61ffa97b71ca062d78ca5b934d345a
}
```

The online integration test proves the end-to-end dedup guarantee against a real
server. It is behind `//go:build online`, connects to `nats-server -js` on
localhost, publishes the same event twice, and asserts the second `PubAck` has
`Duplicate` set and the same `Sequence`. It is deferred to a networked run and
skipped (never compiled) by the offline gate.

Create `publisher_online_test.go`:

```go
//go:build online

package jspublisher

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// TestPublishDedup is a networked integration test. It is excluded from the
// offline gate and requires a nats-server with JetStream on localhost:4222
// (run: `nats-server -js`). It publishes the same event twice and asserts the
// second PubAck reports Duplicate, proving the Nats-Msg-Id dedup window works.
func TestPublishDedup(t *testing.T) {
	nc, err := nats.Connect(nats.DefaultURL)
	if err != nil {
		t.Skipf("no nats-server available: %v", err)
	}
	defer nc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pub, err := NewPublisher(nc, StreamPolicy{
		Name:        "ORDERS_TEST",
		Subjects:    []string{"ORDERS_TEST.>"},
		Retention:   LimitsRetention,
		Storage:     MemoryStore,
		MaxAge:      time.Minute,
		DedupWindow: time.Minute,
	})
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	if err := pub.EnsureStream(ctx); err != nil {
		t.Fatalf("EnsureStream: %v", err)
	}

	e := Event{Aggregate: "order-1", Version: 1, Type: "OrderPlaced", Payload: []byte("{}")}

	first, err := pub.Publish(ctx, "ORDERS_TEST.new", e)
	if err != nil {
		t.Fatalf("first publish: %v", err)
	}
	if first.Duplicate {
		t.Fatalf("first publish reported Duplicate")
	}

	second, err := pub.Publish(ctx, "ORDERS_TEST.new", e)
	if err != nil {
		t.Fatalf("second publish: %v", err)
	}
	if !second.Duplicate {
		t.Fatalf("second publish with same id not reported Duplicate")
	}
	if second.Sequence != first.Sequence {
		t.Fatalf("dedup should collapse to one message: seq %d != %d", second.Sequence, first.Sequence)
	}
}
```

## Review

The common mistakes this exercise defends against are all silent. The first is a
non-deterministic id: if `MessageID` folded in `time.Now()` or a per-call random
value, `TestMessageIDStable` would fail because a retry would not match, and in
production `WithMsgID` would never deduplicate. The second is choosing
`WorkQueueRetention` for an ORDERS stream you intend to replay or fan out — the
builder does not stop you (WorkQueue is a legitimate choice for a queue), so the
policy is a deliberate decision you must get right; the concepts file explains why
the first ack would then delete history. The third is a zero dedup window paired
with `WithMsgID`, which the builder rejects with `ErrZeroDedupWindow` precisely
because it is an idempotent publisher that cannot actually deduplicate.

Confirm correctness by running `go test -race ./...` for the offline core: the
table test must cover every retention and every sentinel error, and the `Example`
must reproduce the exact id. To exercise the real dedup path, start
`nats-server -js` and run `go test -tags online -run TestPublishDedup`; a passing
run shows the second publish returning `Duplicate` with an unchanged `Sequence`,
which is the broker collapsing the retry to one stored message.

## Resources

- [`nats.go/jetstream` package reference](https://pkg.go.dev/github.com/nats-io/nats.go/jetstream) — `New`, `CreateOrUpdateStream`, `StreamConfig`, `Publish`, `WithMsgID`, `PubAck`.
- [`nats.go/jetstream` README](https://github.com/nats-io/nats.go/blob/main/jetstream/README.md) — publish, message ids, and the stream/consumer model in code.
- [NATS JetStream Streams](https://docs.nats.io/nats-concepts/jetstream/streams) — retention policies, storage types, and the deduplication window.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-durable-pull-consumer-acks.md](02-durable-pull-consumer-acks.md)
