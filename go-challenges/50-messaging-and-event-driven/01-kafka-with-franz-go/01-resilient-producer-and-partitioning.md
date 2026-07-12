# Exercise 1: A Resilient, Idempotent Producer with Key-Based Partitioning

A production producer is not `cl.Produce(ctx, rec, nil)`. It is a wrapper that
picks a durable `acks` level, keeps the idempotent producer on, routes each event
to a partition by a deliberately chosen key so per-aggregate ordering holds, and
reports one aggregated error over an asynchronous batch instead of losing produce
failures into ignored promises. This exercise builds that wrapper and, crucially,
extracts the one decision that determines your ordering guarantee — which field
becomes the record key — into a pure function you can test without a broker.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. The pure routing logic builds and tests offline; the client wrapper and
the integration test that talks to a real broker live behind a `//go:build kafka`
tag so `go test ./...` stays green on a machine with no Kafka.

## What you'll build

```text
kafkaproducer/                  independent module: example.com/kafkaproducer
  go.mod                        go 1.26
  routing.go                    Event, RouteKey (pure: key derivation), Encode
  producer.go                   //go:build kafka — Producer over *kgo.Client
  routing_test.go               pure table tests for RouteKey + Example
  producer_integration_test.go  //go:build kafka — same-key-same-partition test
  cmd/
    demo/
      main.go                   groups a batch by routing key (offline, real output)
```

Files: `routing.go`, `producer.go`, `routing_test.go`, `producer_integration_test.go`, `cmd/demo/main.go`.
Implement: `RouteKey` (the pure partition-key rule), and a `Producer` wrapper configuring the idempotent producer, `acks=all`, the sticky-key partitioner, and asynchronous batch produce with a `FirstErrPromise`.
Test: pure table tests that same-aggregate events yield an identical key and empty keys are handled; an integration test that same-key records land on one partition.
Verify: `go test -race ./...` offline; `go test -tags kafka -race ./...` against a broker.

Set up the module:

```bash
mkdir -p go-solutions/50-messaging-and-event-driven/01-kafka-with-franz-go/01-resilient-producer-and-partitioning/cmd/demo
cd go-solutions/50-messaging-and-event-driven/01-kafka-with-franz-go/01-resilient-producer-and-partitioning
go mod edit -go=1.26
go get github.com/twmb/franz-go/pkg/kgo
go get github.com/twmb/franz-go/pkg/kadm
```

### The one decision that owns your ordering: the key

Kafka guarantees order only within a partition, and the partition is chosen by
hashing the record key. So the single most consequential line in a producer is
the one that computes the key. Put related events under the same key and they
stay ordered; put them under different keys and Kafka is free to reorder them
across partitions. That decision is pure business logic — it has nothing to do
with the network — so it belongs in a function you can test in microseconds.

`RouteKey` encodes the rule for an order-events stream: order events are keyed by
their customer, so every event for one customer is ordered on one partition
(create before pay before ship). If an event somehow has no customer, we fall
back to the order id so at least a single order stays self-ordered; if it has
neither, we return a nil key and let the partitioner spread it, accepting that
such an event has no ordering guarantee. The senior caveat lives here too: keying
by customer means a single dominant customer becomes a hot partition. Whether
that is acceptable is a capacity decision, and it is visible precisely because the
key rule is one readable function rather than buried in the produce call.

Create `routing.go`:

```go
package kafkaproducer

import "encoding/json"

// Event is a domain event destined for an order-events topic. CustomerID is the
// ordering aggregate: every event for one customer must land on one partition so
// its lifecycle (created -> paid -> shipped) stays ordered.
type Event struct {
	OrderID    string `json:"order_id"`
	CustomerID string `json:"customer_id"`
	Type       string `json:"type"`
	TraceID    string `json:"trace_id"`
}

// RouteKey derives the record key that fixes an event's partition. Records that
// share a key hash to the same partition and are therefore mutually ordered.
// Ordering is per customer; an event with no customer falls back to its order id
// so a single order still stays ordered; an event with neither gets a nil key
// and is spread by the partitioner with no ordering guarantee.
func RouteKey(e Event) []byte {
	switch {
	case e.CustomerID != "":
		return []byte(e.CustomerID)
	case e.OrderID != "":
		return []byte(e.OrderID)
	default:
		return nil
	}
}

// Encode serializes an event as the JSON record value.
func (e Event) Encode() []byte {
	b, _ := json.Marshal(e)
	return b
}
```

### The producer wrapper (network code, behind the build tag)

The wrapper configures the contract described in the concepts file. Idempotence
is on by default in franz-go and requires `acks=all`, so `RequiredAcks` is set to
`AllISRAcks` — asking for a weaker ack while idempotence is on makes `NewClient`
fail, which is the library refusing to pair a durability level with a producer
mode that cannot honor it. `StickyKeyPartitioner(nil)` selects Kafka's default
murmur2 key hashing, so this producer routes keys the same way any other
Kafka-compatible client does. `ProducerLinger` batches records for a few
milliseconds to trade a little latency for throughput, and `ProducerBatchMaxBytes`
caps a batch.

`ProduceBatch` shows the correct asynchronous error story. `Produce` returns
nothing about whether the record succeeded — the error is delivered later to the
promise. A `FirstErrPromise` collects the first failing error across the whole
batch: you hand `fp.Promise()` to each `Produce` call, then `fp.Err()` blocks
until every promise has resolved and returns the first error (or nil). That is
the difference between "I fired a batch and it all landed" and "I fired a batch
and ignored the failures".

Create `producer.go`:

```go
//go:build kafka

package kafkaproducer

import (
	"context"
	"fmt"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
)

// Producer wraps a *kgo.Client configured as an idempotent, key-partitioned
// producer. The client is long-lived and goroutine-safe; create one per process.
type Producer struct {
	cl *kgo.Client
}

// NewProducer builds an idempotent producer. Idempotence is on by default and
// requires acks=all, so RequiredAcks(AllISRAcks) is the matching, durable choice;
// LeaderAck or NoAck would require DisableIdempotentWrite and trade durability
// for latency. StickyKeyPartitioner selects Kafka-compatible murmur2 hashing so
// keys route the same way across clients.
func NewProducer(seeds []string, topic string) (*Producer, error) {
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(seeds...),
		kgo.DefaultProduceTopic(topic),
		kgo.RequiredAcks(kgo.AllISRAcks()),
		kgo.ProducerBatchMaxBytes(16<<20),
		kgo.ProducerLinger(5*time.Millisecond),
		kgo.RecordPartitioner(kgo.StickyKeyPartitioner(nil)),
	)
	if err != nil {
		return nil, fmt.Errorf("new kafka client: %w", err)
	}
	return &Producer{cl: cl}, nil
}

// Close flushes buffered records and releases connections. Always call it.
func (p *Producer) Close() { p.cl.Close() }

// ProduceBatch produces every event asynchronously and returns the first error
// once all promises resolve. The async Produce call never returns the produce
// error itself; it is delivered to the promise, so a FirstErrPromise is how a
// failed batch is detected rather than silently dropped.
func (p *Producer) ProduceBatch(ctx context.Context, events []Event) error {
	var fp kgo.FirstErrPromise
	for _, e := range events {
		rec := &kgo.Record{
			Key:   RouteKey(e),
			Value: e.Encode(),
			Headers: []kgo.RecordHeader{
				{Key: "trace-id", Value: []byte(e.TraceID)},
			},
		}
		p.cl.Produce(ctx, rec, fp.Promise())
	}
	return fp.Err()
}
```

### The runnable demo

The demo runs offline. It cannot reach a broker, so instead of producing it makes
the routing decision visible: it groups a batch of events by their routing key,
which is exactly the grouping Kafka will apply when it hashes those keys to
partitions. Two events for `acme` share a key (one partition, ordered); the
event with no customer falls back to its order id.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sort"

	"example.com/kafkaproducer"
)

func main() {
	events := []kafkaproducer.Event{
		{OrderID: "1001", CustomerID: "acme", Type: "created"},
		{OrderID: "1002", CustomerID: "acme", Type: "paid"},
		{OrderID: "1003", CustomerID: "globex", Type: "created"},
		{OrderID: "1004", CustomerID: "", Type: "created"},
	}

	byKey := map[string][]string{}
	for _, e := range events {
		k := string(kafkaproducer.RouteKey(e))
		byKey[k] = append(byKey[k], e.OrderID)
	}

	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		fmt.Printf("key=%s -> %v\n", k, byKey[k])
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
key=1004 -> [1004]
key=acme -> [1001 1002]
key=globex -> [1003]
```

### Tests

The pure tests pin the ordering guarantee: two events for the same customer must
produce byte-identical keys (same key means same partition means ordered), and
the fallbacks are covered. These run always, offline, in microseconds.

Create `routing_test.go`:

```go
package kafkaproducer

import (
	"bytes"
	"fmt"
	"testing"
)

func TestRouteKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   Event
		want []byte
	}{
		{"customer keyed", Event{OrderID: "1", CustomerID: "acme"}, []byte("acme")},
		{"fallback to order", Event{OrderID: "7", CustomerID: ""}, []byte("7")},
		{"no key", Event{}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := RouteKey(tc.in)
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("RouteKey(%+v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRouteKeyStableForAggregate(t *testing.T) {
	t.Parallel()
	a := RouteKey(Event{OrderID: "1001", CustomerID: "acme", Type: "created"})
	b := RouteKey(Event{OrderID: "1002", CustomerID: "acme", Type: "paid"})
	if !bytes.Equal(a, b) {
		t.Fatalf("same customer must map to identical key: %q vs %q", a, b)
	}
}

func ExampleRouteKey() {
	fmt.Printf("%s\n", RouteKey(Event{CustomerID: "acme"}))
	fmt.Printf("%s\n", RouteKey(Event{OrderID: "1004"}))
	// Output:
	// acme
	// 1004
}
```

The integration test proves the partitioning claim against a real broker. It
creates an ephemeral 3-partition topic with `kadm`, synchronously produces a
batch keyed by two customers, and asserts through the returned `ProduceResults`
that each key consistently maps to exactly one partition. It also asserts that a
record with an empty topic surfaces an error via `FirstErr`, confirming that
produce failures are reported rather than swallowed. It is skipped unless
`KAFKA_SEEDS` is set, so it never runs in an offline `go test`.

Create `producer_integration_test.go`:

```go
//go:build kafka

package kafkaproducer

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
)

func TestSameKeySamePartition(t *testing.T) {
	seeds := os.Getenv("KAFKA_SEEDS")
	if seeds == "" {
		t.Skip("set KAFKA_SEEDS (comma-separated) to run the integration test")
	}
	brokers := strings.Split(seeds, ",")
	ctx := t.Context()

	cl, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer cl.Close()

	admin := kadm.NewClient(cl)
	topic := fmt.Sprintf("orders-test-%d", time.Now().UnixNano())
	if _, err := admin.CreateTopic(ctx, 3, 1, nil, topic); err != nil {
		t.Fatalf("create topic: %v", err)
	}
	defer admin.DeleteTopics(ctx, topic)

	events := []Event{
		{OrderID: "1001", CustomerID: "acme", Type: "created"},
		{OrderID: "1002", CustomerID: "acme", Type: "paid"},
		{OrderID: "1003", CustomerID: "globex", Type: "created"},
		{OrderID: "1004", CustomerID: "globex", Type: "paid"},
	}
	recs := make([]*kgo.Record, 0, len(events))
	for _, e := range events {
		recs = append(recs, &kgo.Record{Topic: topic, Key: RouteKey(e), Value: e.Encode()})
	}

	results := cl.ProduceSync(ctx, recs...)
	if err := results.FirstErr(); err != nil {
		t.Fatalf("produce: %v", err)
	}

	keyPart := map[string]int32{}
	for _, r := range results {
		k := string(r.Record.Key)
		if prev, ok := keyPart[k]; ok && prev != r.Record.Partition {
			t.Fatalf("key %q spanned partitions %d and %d", k, prev, r.Record.Partition)
		}
		keyPart[k] = r.Record.Partition
	}
	if len(keyPart) != 2 {
		t.Fatalf("expected 2 distinct keys, got %d", len(keyPart))
	}

	// An empty topic must surface as an error through the promise, not be lost.
	bad := cl.ProduceSync(ctx, &kgo.Record{Topic: "", Value: []byte("x")})
	if bad.FirstErr() == nil {
		t.Fatal("expected produce to empty topic to error")
	}
}
```

## Review

The producer is correct when the key rule is the only thing that decides
ordering. Confirm it two ways: the pure `TestRouteKeyStableForAggregate` proves
that two events for one customer are byte-identical keys (so Kafka will place them
on one partition), and the integration `TestSameKeySamePartition` proves the same
claim end to end against a live 3-partition topic. If the integration test ever
sees a key on two partitions, either the partitioner was changed to something not
key-consistent or a different client wrote the topic with a mismatched hash.

The mistakes to avoid are the ones the concepts file names. Do not weaken `acks`
while idempotence is on — `NewClient` will reject it, and if you "fix" that by
disabling idempotence you have reintroduced duplicate-on-retry. Do not read the
absence of a return value from `Produce` as success; the `FirstErrPromise` is
what turns ignored promises into one asserted error. And keep the hot-partition
trade-off in view: keying by customer is what gives you per-customer ordering, and
it is also what makes one giant customer a throughput ceiling — a property you can
see only because the key lives in a single function. Run `go test -race ./...` for
the pure guarantees and `go test -tags kafka -race ./...` against a broker for the
partitioning proof.

## Resources

- [franz-go `kgo` package reference](https://pkg.go.dev/github.com/twmb/franz-go/pkg/kgo) — `NewClient`, producer options, `Record`, `FirstErrPromise`, `ProduceSync`.
- [franz-go: Producing and Consuming](https://github.com/twmb/franz-go/blob/master/docs/producing-and-consuming.md) — the record model, `DefaultProduceTopic`, and the promise helpers.
- [franz-go `kadm` package reference](https://pkg.go.dev/github.com/twmb/franz-go/pkg/kadm) — `CreateTopic` and admin operations used by the integration test.
- [Apache Kafka: producer acks and idempotence](https://kafka.apache.org/documentation/#producerconfigs) — the broker-side meaning of `acks` and `enable.idempotence`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-consumer-group-manual-commit.md](02-consumer-group-manual-commit.md)
