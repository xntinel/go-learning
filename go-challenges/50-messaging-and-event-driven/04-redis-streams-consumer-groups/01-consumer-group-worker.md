# Exercise 1: A Consumer-Group Worker Over Redis Streams

This exercise builds the core of every Redis Streams system: a producer that
appends domain events to a capped stream, and a consumer-group worker that reads
its share of new messages, processes them, and acknowledges. Getting this cycle —
`XADD` then `XREADGROUP('>')` then process then `XACK` — exactly right, and
understanding what it does and does not guarantee, is the foundation the recovery
and dead-letter exercises build on.

This module is fully self-contained: its own `go mod init`, its own event codec,
its own producer and worker, its own demo and tests. Nothing here imports another
exercise. The network calls go through `github.com/redis/go-redis/v9`, and the
demo and tests run against an in-process `github.com/alicebob/miniredis/v2`
server, so the whole loop runs with no external Redis.

## What you'll build

```text
streamworker/                 independent module: example.com/streamworker
  go.mod                      go 1.24; requires go-redis/v9 and miniredis/v2
  event.go                    OrderEvent; Encode/Decode (pure codec, string values)
  worker.go                   BuildXAddArgs, Producer, EnsureGroup, Worker.Poll/Drain
  cmd/
    demo/
      main.go                 miniredis demo: 6 events, two consumers share them
  streamworker_test.go        codec round-trip, BuildXAddArgs table, miniredis fan-out
```

- Files: `event.go`, `worker.go`, `cmd/demo/main.go`, `streamworker_test.go`.
- Implement: an `OrderEvent` codec to and from a Redis field map, `BuildXAddArgs` with approximate `MAXLEN` capping, a `Producer.Publish`, an idempotent `EnsureGroup`, and a `Worker` whose `Poll` reads new messages with `>`, processes, and `XACK`s.
- Test: table-driven codec and `BuildXAddArgs` tests, plus a miniredis end-to-end test that runs two consumers in one group and asserts the six messages are split with no overlap and zero pending after ack.
- Verify: `go test -count=1 -race ./...`

Set up the module and fetch the two dependencies:

```bash
mkdir -p ~/go-exercises/streamworker/cmd/demo
cd ~/go-exercises/streamworker
go mod init example.com/streamworker
go mod edit -go=1.24
go get github.com/redis/go-redis/v9
go get github.com/alicebob/miniredis/v2
```

### Why the codec is a pure function, and why every value is a string

Redis stream entries are field/value maps, and Redis is untyped: whatever you
store, you read back as a string. That single fact drives the codec design. If you
store an `int64` amount and later read `Values["amount"].(int64)`, the type
assertion panics — the value came back as the string `"4200"`. So `Encode` renders
every field as a string (`strconv.FormatInt` for the amount), and `Decode` parses
deliberately, returning a wrapped sentinel `ErrMalformedEvent` on a missing field,
a wrong dynamic type, or an unparseable number. Keeping encode and decode as pure
functions — no Redis client, no context — means the round-trip and the failure
modes are unit-testable in microseconds with a table, independent of any server.

`Decode` takes `map[string]interface{}` because that is exactly the type of
`redis.XMessage.Values`. The helper `field` does the `interface{}`-to-`string`
coercion once and reports a malformed event if the value is missing or not a
string, so the numeric parse and the field extraction each have one clear failure
path wrapped with `%w`.

Create `event.go`:

```go
package streamworker

import (
	"errors"
	"fmt"
	"strconv"
)

// ErrMalformedEvent is returned when a Redis field map cannot be decoded into an
// OrderEvent. Callers match it with errors.Is.
var ErrMalformedEvent = errors.New("malformed order event")

// OrderEvent is the domain event carried on the stream.
type OrderEvent struct {
	ID       string
	Type     string
	Amount   int64
	Currency string
}

// Encode renders an event as a Redis field map. Every value is a string, which
// is exactly how Redis stores and returns stream fields.
func Encode(e OrderEvent) map[string]interface{} {
	return map[string]interface{}{
		"id":       e.ID,
		"type":     e.Type,
		"amount":   strconv.FormatInt(e.Amount, 10),
		"currency": e.Currency,
	}
}

// Decode parses a Redis field map (values arrive as strings) back into an
// OrderEvent. A missing, wrongly typed, or non-numeric field yields a wrapped
// ErrMalformedEvent.
func Decode(m map[string]interface{}) (OrderEvent, error) {
	id, err := field(m, "id")
	if err != nil {
		return OrderEvent{}, err
	}
	typ, err := field(m, "type")
	if err != nil {
		return OrderEvent{}, err
	}
	amountStr, err := field(m, "amount")
	if err != nil {
		return OrderEvent{}, err
	}
	amount, err := strconv.ParseInt(amountStr, 10, 64)
	if err != nil {
		return OrderEvent{}, fmt.Errorf("%w: amount %q: %v", ErrMalformedEvent, amountStr, err)
	}
	currency, err := field(m, "currency")
	if err != nil {
		return OrderEvent{}, err
	}
	return OrderEvent{ID: id, Type: typ, Amount: amount, Currency: currency}, nil
}

// field extracts a string field, coercing the interface{} the way Redis returns
// it. A missing or non-string field is a malformed event.
func field(m map[string]interface{}, key string) (string, error) {
	v, ok := m[key]
	if !ok {
		return "", fmt.Errorf("%w: missing field %q", ErrMalformedEvent, key)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%w: field %q is %T, want string", ErrMalformedEvent, key, v)
	}
	return s, nil
}
```

### The producer, the group, and the read cycle

`BuildXAddArgs` is extracted as a pure function so the trimming policy can be
tested without a server. A `maxLen` of zero disables trimming; a positive `maxLen`
sets `Approx = true`, which makes go-redis emit `MAXLEN ~ N` — approximate
trimming that lets Redis drop whole macro-nodes efficiently instead of doing exact
work on every append. `ID` is left empty, so the server assigns the next entry ID
(the equivalent of `XADD ... *`).

`EnsureGroup` is the idempotent group creator. `XGROUP CREATE ... MKSTREAM`
creates the stream and the group in one call, but on the second and later boots it
returns a `BUSYGROUP` error because the group already exists. go-redis has no typed
error for this, so the pattern is to detect the `BUSYGROUP` substring and treat it
as success; any other error is fatal. Start ID `0` means a fresh group replays the
whole existing stream.

`Worker.Poll` is the read cycle. `XREADGROUP` with the special ID `>` atomically
delivers up to `count` never-before-delivered entries to this consumer and records
them in this consumer's Pending Entries List. For each message it decodes,
processes with the handler, and — only on success — `XACK`s, which removes the
entry from the PEL. If the handler or decode fails, `Poll` returns the error with
that entry left un-acked (still pending), which is exactly the at-least-once
behavior the next exercises rely on. `Block` is set to `-1`; go-redis omits the
`BLOCK` argument entirely for a negative value, giving a non-blocking read that
returns `redis.Nil` when no new entries are waiting — treated here as "nothing to
do". A production worker would instead pass a positive `Block` to long-poll and
loop forever.

Create `worker.go`:

```go
package streamworker

import (
	"context"
	"errors"
	"strings"

	"github.com/redis/go-redis/v9"
)

// BuildXAddArgs builds the XADD arguments for appending an event to a capped
// stream. A maxLen of 0 disables trimming; a positive maxLen uses approximate
// trimming (MAXLEN ~ N). ID is left empty so the server assigns the entry ID.
func BuildXAddArgs(stream string, e OrderEvent, maxLen int64) *redis.XAddArgs {
	args := &redis.XAddArgs{
		Stream: stream,
		Values: Encode(e),
	}
	if maxLen > 0 {
		args.MaxLen = maxLen
		args.Approx = true
	}
	return args
}

// Producer appends domain events to a capped stream.
type Producer struct {
	rdb    *redis.Client
	stream string
	maxLen int64
}

// NewProducer returns a Producer writing to stream, trimmed to about maxLen
// entries (0 disables trimming).
func NewProducer(rdb *redis.Client, stream string, maxLen int64) *Producer {
	return &Producer{rdb: rdb, stream: stream, maxLen: maxLen}
}

// Publish appends one event and returns its server-assigned entry ID.
func (p *Producer) Publish(ctx context.Context, e OrderEvent) (string, error) {
	return p.rdb.XAdd(ctx, BuildXAddArgs(p.stream, e, p.maxLen)).Result()
}

// EnsureGroup creates the stream and consumer group idempotently, starting the
// group at "0" so a brand-new group replays the existing stream. A BUSYGROUP
// error means the group already exists and is treated as success.
func EnsureGroup(ctx context.Context, rdb *redis.Client, stream, group string) error {
	err := rdb.XGroupCreateMkStream(ctx, stream, group, "0").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return err
	}
	return nil
}

// Handler processes a single decoded event.
type Handler func(context.Context, OrderEvent) error

// Worker is one consumer in a group. Run many with the same group and distinct
// consumer names to load-balance a stream horizontally.
type Worker struct {
	rdb      *redis.Client
	stream   string
	group    string
	consumer string
	count    int64
	handler  Handler
}

// NewWorker returns a Worker reading up to count entries per poll.
func NewWorker(rdb *redis.Client, stream, group, consumer string, count int64, h Handler) *Worker {
	return &Worker{rdb: rdb, stream: stream, group: group, consumer: consumer, count: count, handler: h}
}

// Poll reads one batch of new messages ('>'), processes each, and acknowledges
// the ones that succeed, returning their IDs. Block is negative, which go-redis
// translates to a non-blocking read, so Poll returns promptly (redis.Nil) when
// the stream has no new entries.
func (w *Worker) Poll(ctx context.Context) ([]string, error) {
	streams, err := w.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    w.group,
		Consumer: w.consumer,
		Streams:  []string{w.stream, ">"},
		Count:    w.count,
		Block:    -1,
	}).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var acked []string
	for _, s := range streams {
		for _, msg := range s.Messages {
			e, err := Decode(msg.Values)
			if err != nil {
				return acked, err
			}
			if err := w.handler(ctx, e); err != nil {
				return acked, err
			}
			if err := w.rdb.XAck(ctx, w.stream, w.group, msg.ID).Err(); err != nil {
				return acked, err
			}
			acked = append(acked, msg.ID)
		}
	}
	return acked, nil
}

// Drain polls repeatedly until no new messages remain, returning every ID it
// processed. It is the non-blocking form used by tests; a production worker
// loops Poll with a positive Block instead.
func (w *Worker) Drain(ctx context.Context) ([]string, error) {
	var all []string
	for {
		ids, err := w.Poll(ctx)
		if err != nil {
			return all, err
		}
		if len(ids) == 0 {
			return all, nil
		}
		all = append(all, ids...)
	}
}
```

### The runnable demo

The demo starts an in-process miniredis, produces six order events to a stream
capped at about a thousand entries, then runs two consumers in the same group.
It interleaves single `Poll`s (one batch each per round) so the two consumers
visibly split the six messages rather than one draining them all; with a `COUNT`
of two, `worker-a` takes the first two and the fifth and sixth, `worker-b` takes
the third and fourth. After both have acked, the group's pending count is zero.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sort"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"example.com/streamworker"
)

func main() {
	srv, err := miniredis.Run()
	if err != nil {
		panic(err)
	}
	defer srv.Close()

	rdb := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	defer rdb.Close()

	ctx := context.Background()
	const stream, group = "orders", "fulfilment"

	if err := streamworker.EnsureGroup(ctx, rdb, stream, group); err != nil {
		panic(err)
	}

	prod := streamworker.NewProducer(rdb, stream, 1000)
	for i := range 6 {
		_, err := prod.Publish(ctx, streamworker.OrderEvent{
			ID:       fmt.Sprintf("order-%d", i),
			Type:     "created",
			Amount:   int64(100 + i),
			Currency: "USD",
		})
		if err != nil {
			panic(err)
		}
	}

	var processed []string
	record := func(_ context.Context, e streamworker.OrderEvent) error {
		processed = append(processed, e.ID)
		return nil
	}

	a := streamworker.NewWorker(rdb, stream, group, "worker-a", 2, record)
	b := streamworker.NewWorker(rdb, stream, group, "worker-b", 2, record)

	for {
		ida, err := a.Poll(ctx)
		if err != nil {
			panic(err)
		}
		idb, err := b.Poll(ctx)
		if err != nil {
			panic(err)
		}
		if len(ida)+len(idb) == 0 {
			break
		}
	}

	sort.Strings(processed)
	fmt.Printf("processed %d events: %v\n", len(processed), processed)

	pending, err := rdb.XPending(ctx, stream, group).Result()
	if err != nil {
		panic(err)
	}
	fmt.Printf("pending after ack: %d\n", pending.Count)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
processed 6 events: [order-0 order-1 order-2 order-3 order-4 order-5]
pending after ack: 0
```

### Tests

The codec tests are the fast, pure core: `TestEncodeDecodeRoundTrip` proves an
event survives `Encode` then `Decode` for typical, zero, and negative amounts,
and `TestDecodeMalformed` proves each failure mode (missing field, non-numeric
amount, non-string value) returns a wrapped `ErrMalformedEvent` matched with
`errors.Is`. `TestBuildXAddArgs` pins the trimming policy: no cap leaves `MaxLen`
zero and `Approx` false, a positive cap sets both and `Approx` true, and the ID
is always empty so the server assigns it. `TestFanOut` is the end-to-end proof
over miniredis: six messages, two consumers in one group, and the assertions are
the two properties that define correct competing-consumer behavior — the union of
processed IDs equals all produced IDs with each seen exactly once (load-balanced,
no overlap), and after `XACK` the group's pending count is zero.

Create `streamworker_test.go`:

```go
package streamworker

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		event OrderEvent
	}{
		{"typical", OrderEvent{ID: "o1", Type: "created", Amount: 4200, Currency: "USD"}},
		{"zero amount", OrderEvent{ID: "o2", Type: "cancelled", Amount: 0, Currency: "EUR"}},
		{"negative amount", OrderEvent{ID: "o3", Type: "refund", Amount: -150, Currency: "GBP"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Decode(Encode(tc.event))
			if err != nil {
				t.Fatalf("Decode(Encode(%+v)) error: %v", tc.event, err)
			}
			if got != tc.event {
				t.Fatalf("round trip = %+v, want %+v", got, tc.event)
			}
		})
	}
}

func TestDecodeMalformed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		m    map[string]interface{}
	}{
		{"missing id", map[string]interface{}{"type": "created", "amount": "1", "currency": "USD"}},
		{"non-numeric amount", map[string]interface{}{"id": "o1", "type": "created", "amount": "NaN", "currency": "USD"}},
		{"non-string field", map[string]interface{}{"id": 7, "type": "created", "amount": "1", "currency": "USD"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Decode(tc.m)
			if !errors.Is(err, ErrMalformedEvent) {
				t.Fatalf("Decode(%v) error = %v, want ErrMalformedEvent", tc.m, err)
			}
		})
	}
}

func TestBuildXAddArgs(t *testing.T) {
	t.Parallel()
	e := OrderEvent{ID: "o1", Type: "created", Amount: 1, Currency: "USD"}
	tests := []struct {
		name       string
		maxLen     int64
		wantMaxLen int64
		wantApprox bool
	}{
		{"uncapped", 0, 0, false},
		{"capped approx", 1000, 1000, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			args := BuildXAddArgs("orders", e, tc.maxLen)
			if args.Stream != "orders" {
				t.Errorf("Stream = %q, want orders", args.Stream)
			}
			if args.ID != "" {
				t.Errorf("ID = %q, want empty (server-assigned)", args.ID)
			}
			if args.MaxLen != tc.wantMaxLen {
				t.Errorf("MaxLen = %d, want %d", args.MaxLen, tc.wantMaxLen)
			}
			if args.Approx != tc.wantApprox {
				t.Errorf("Approx = %v, want %v", args.Approx, tc.wantApprox)
			}
		})
	}
}

func TestFanOut(t *testing.T) {
	t.Parallel()
	srv := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	t.Cleanup(func() { rdb.Close() })

	ctx := t.Context()
	const stream, group = "orders", "fulfilment"
	if err := EnsureGroup(ctx, rdb, stream, group); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}

	prod := NewProducer(rdb, stream, 1000)
	want := map[string]bool{}
	for i := range 6 {
		id, err := prod.Publish(ctx, OrderEvent{ID: fmt.Sprintf("o%d", i), Type: "created", Amount: int64(i), Currency: "USD"})
		if err != nil {
			t.Fatalf("Publish: %v", err)
		}
		want[id] = true
	}

	seen := map[string]int{}
	record := func(_ context.Context, _ OrderEvent) error { return nil }
	a := NewWorker(rdb, stream, group, "a", 2, record)
	b := NewWorker(rdb, stream, group, "b", 2, record)

	for {
		ida, err := a.Poll(ctx)
		if err != nil {
			t.Fatalf("a.Poll: %v", err)
		}
		idb, err := b.Poll(ctx)
		if err != nil {
			t.Fatalf("b.Poll: %v", err)
		}
		for _, id := range ida {
			seen[id]++
		}
		for _, id := range idb {
			seen[id]++
		}
		if len(ida)+len(idb) == 0 {
			break
		}
	}

	if len(seen) != len(want) {
		t.Fatalf("processed %d distinct IDs, want %d", len(seen), len(want))
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("id %s processed %d times, want exactly once", id, n)
		}
		if !want[id] {
			t.Errorf("processed unknown id %s", id)
		}
	}

	pending, err := rdb.XPending(ctx, stream, group).Result()
	if err != nil {
		t.Fatalf("XPending: %v", err)
	}
	if pending.Count != 0 {
		t.Errorf("pending after ack = %d, want 0", pending.Count)
	}
}

func Example() {
	e := OrderEvent{ID: "o1", Type: "created", Amount: 4200, Currency: "USD"}
	decoded, _ := Decode(Encode(e))
	fmt.Printf("%s %s %d %s\n", decoded.ID, decoded.Type, decoded.Amount, decoded.Currency)
	// Output: o1 created 4200 USD
}
```

## Review

The worker is correct when three invariants hold. First, the codec is total: every
field Redis returns is a string, so `Decode` must coerce and parse deliberately and
report `ErrMalformedEvent` (wrapped with `%w`) rather than panic on a wrong type —
`TestDecodeMalformed` is what proves it. Second, an entry is acked only after its
handler succeeds; returning early on a handler error leaves the entry pending,
which is the at-least-once behavior the recovery exercise depends on, not a bug.
Third, `EnsureGroup` swallows exactly `BUSYGROUP` and nothing else, so a restart
does not crash and a real connection error still surfaces.

The mistakes to avoid: do not assert `Values["amount"].(int64)` — it is a string;
do not forget `XACK`, or the PEL grows without bound even though processing looks
healthy; do not treat `>` as a way to re-read your own pending entries (it only
returns new ones — that is the next exercise); and do not use `Block: 0` in a real
loop, which blocks forever and pins a pooled connection. Confirm correctness with
`go test -count=1 -race ./...`: the fan-out test proves the six messages are split
with no overlap and drop to zero pending after ack, and `-race` proves the codec
and worker are clean under the miniredis client's internal concurrency.

## Resources

- [Redis Streams](https://redis.io/docs/latest/develop/data-types/streams/) — the stream data type, consumer groups, the PEL, and `XREADGROUP`/`XACK`.
- [XREADGROUP](https://redis.io/docs/latest/commands/xreadgroup/) — the `>` special ID, `COUNT`, and `BLOCK` semantics.
- [go-redis v9](https://pkg.go.dev/github.com/redis/go-redis/v9) — `XAddArgs`, `XReadGroupArgs`, `XStream`, `XMessage`, and `redis.Nil`.
- [miniredis v2](https://pkg.go.dev/github.com/alicebob/miniredis/v2) — the in-process Redis used by the demo and tests.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-pending-entries-and-crash-recovery.md](02-pending-entries-and-crash-recovery.md)
