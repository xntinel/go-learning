# Exercise 3: Poison Messages and a Dead-Letter Stream

A message whose handler always fails is never acked, so the reclaim machinery from
the previous exercise re-serves it forever — its delivery count climbing on every
claim while it starves the group. Redis never dead-letters on its own. This
exercise adds the missing ceiling: read the delivery count from `XPENDINGEXT`, and
once an entry crosses a threshold, move it to a dead-letter stream with failure
metadata and ack the original so it leaves the source PEL for good.

This module is fully self-contained: its own `go mod init`, event codec, worker,
reclaim-and-route logic, demo, and tests over an in-process miniredis. Nothing
here imports another exercise.

## What you'll build

```text
streamdlq/                    independent module: example.com/streamdlq
  go.mod                      go 1.24; requires go-redis/v9 and miniredis/v2
  event.go                    OrderEvent; Encode/Decode (string-valued codec)
  stream.go                   EnsureGroup; Worker.ReadNew (first delivery)
  dlq.go                      routeToDLQ, Reclaimer.RunOnce, moveToDLQ
  cmd/
    demo/
      main.go                 miniredis: one poison message driven to the DLQ
  streamdlq_test.go           codec round-trip + routing table + miniredis DLQ end-to-end
```

- Files: `event.go`, `stream.go`, `dlq.go`, `cmd/demo/main.go`, `streamdlq_test.go`.
- Implement: a pure `routeToDLQ` predicate over `(retryCount, maxDeliveries)`, and a `Reclaimer.RunOnce` that sweeps stranded entries, claims each, and either retries it or moves it to a dead-letter stream (`XADD` + `XACK`) once it crosses the ceiling.
- Test: a codec round-trip that also asserts a wrapped `ErrMalformedEvent` on bad input, a table-driven predicate test covering the off-by-one at the threshold, plus a miniredis end-to-end test that drives one always-failing message to the DLQ after the configured number of deliveries and proves it is acked in the source and not double-routed.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/50-messaging-and-event-driven/04-redis-streams-consumer-groups/03-poison-messages-and-dead-letter-stream/cmd/demo
cd go-solutions/50-messaging-and-event-driven/04-redis-streams-consumer-groups/03-poison-messages-and-dead-letter-stream
go mod edit -go=1.24
go get github.com/redis/go-redis/v9
go get github.com/alicebob/miniredis/v2
```

### Why poison messages need an explicit ceiling

Every delivery bumps a counter: the initial `XREADGROUP` sets it to one, and each
`XCLAIM`/`XAUTOCLAIM` without `JUSTID` increments it. `XPENDINGEXT` surfaces that
counter as `RetryCount`. For a healthy message this is harmless; for a poison
message — one whose handler can never succeed — it is the whole problem. The
reclaim janitor keeps finding it idle, keeps claiming it, keeps handing it to a
handler that fails, and its delivery count grows without bound while it occupies a
slot the group could spend on messages that would succeed. Redis will not stop
this for you.

The ceiling is a max-deliveries threshold. `routeToDLQ(retryCount, maxDeliveries)`
is a pure function of two integers: once deliveries reach the maximum, the entry
is poison and goes to a dead-letter stream instead of being retried. This is the
easiest thing in the system to get subtly wrong — the off-by-one at the threshold
is exactly where a test earns its keep — and the easiest to test, because it has
no dependencies at all.

### Claim, then route, so ownership and metadata are correct

`RunOnce` is one sweep of the janitor. It lists stranded entries with
`XPENDINGEXT` (idle at least `minIdle`), which gives each entry's current
`RetryCount` *before* this pass touches it. For each entry it then `XCLAIM`s the
message — which both takes ownership (resetting idle so a peer will not grab it
mid-decision) and returns the message body it needs to build the dead-letter
record. With the body in hand it applies the decision made from the pre-claim
`RetryCount`: if the entry is at or over the ceiling, `moveToDLQ` appends it to the
dead-letter stream — original payload plus `dlq_source_id` and `dlq_attempts` —
and then `XACK`s it in the source group; otherwise the handler runs again and, on
failure, the entry is left pending for the next pass.

Acking the original after the dead-letter write is what makes the routing
idempotent. Once the source PEL no longer holds the entry, a later sweep cannot
find it, so it can be dead-lettered at most once — running `RunOnce` again after a
message is routed is a no-op, which the test asserts directly. If a claimed slice
comes back empty (the entry was trimmed out from under the PEL), `RunOnce` skips
it rather than dereferencing nothing.

Create `event.go`:

```go
package streamdlq

import (
	"errors"
	"fmt"
	"strconv"
)

// ErrMalformedEvent is returned when a Redis field map cannot be decoded.
var ErrMalformedEvent = errors.New("malformed order event")

// OrderEvent is the domain event carried on the stream.
type OrderEvent struct {
	ID       string
	Type     string
	Amount   int64
	Currency string
}

// Encode renders an event as a Redis field map with string values.
func Encode(e OrderEvent) map[string]interface{} {
	return map[string]interface{}{
		"id":       e.ID,
		"type":     e.Type,
		"amount":   strconv.FormatInt(e.Amount, 10),
		"currency": e.Currency,
	}
}

// Decode parses a Redis field map back into an OrderEvent, wrapping
// ErrMalformedEvent on a missing, wrongly typed, or non-numeric field.
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

Create `stream.go`:

```go
package streamdlq

import (
	"context"
	"errors"
	"strings"

	"github.com/redis/go-redis/v9"
)

// EnsureGroup creates the stream and group idempotently, treating BUSYGROUP as
// success.
func EnsureGroup(ctx context.Context, rdb *redis.Client, stream, group string) error {
	err := rdb.XGroupCreateMkStream(ctx, stream, group, "0").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return err
	}
	return nil
}

// Worker performs the first delivery of new entries to a named consumer.
type Worker struct {
	rdb      *redis.Client
	stream   string
	group    string
	consumer string
}

// NewWorker returns a Worker for the given consumer name.
func NewWorker(rdb *redis.Client, stream, group, consumer string) *Worker {
	return &Worker{rdb: rdb, stream: stream, group: group, consumer: consumer}
}

// ReadNew delivers up to count new entries ('>') into this consumer's PEL and
// returns them without acking; this is the first, failed delivery attempt.
func (w *Worker) ReadNew(ctx context.Context, count int64) ([]redis.XMessage, error) {
	streams, err := w.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    w.group,
		Consumer: w.consumer,
		Streams:  []string{w.stream, ">"},
		Count:    count,
		Block:    -1,
	}).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var msgs []redis.XMessage
	for _, s := range streams {
		msgs = append(msgs, s.Messages...)
	}
	return msgs, nil
}
```

Create `dlq.go`:

```go
package streamdlq

import (
	"context"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// routeToDLQ reports whether an entry delivered retryCount times should be
// dead-lettered rather than retried. The threshold is inclusive: once deliveries
// reach maxDeliveries, the entry is poison.
func routeToDLQ(retryCount, maxDeliveries int64) bool {
	return retryCount >= maxDeliveries
}

// Handler processes one claimed message. Returning an error leaves the entry
// pending for a future reclaim.
type Handler func(context.Context, redis.XMessage) error

// Reclaimer sweeps a group's stranded entries and either retries them or, once
// they cross the delivery ceiling, moves them to a dead-letter stream.
type Reclaimer struct {
	rdb           *redis.Client
	stream        string
	dlqStream     string
	group         string
	consumer      string
	minIdle       time.Duration
	maxDeliveries int64
	count         int64
	handler       Handler
}

// NewReclaimer returns a Reclaimer. Entries delivered maxDeliveries times or more
// are routed to dlqStream instead of retried.
func NewReclaimer(rdb *redis.Client, stream, dlqStream, group, consumer string, minIdle time.Duration, maxDeliveries, count int64, h Handler) *Reclaimer {
	return &Reclaimer{
		rdb:           rdb,
		stream:        stream,
		dlqStream:     dlqStream,
		group:         group,
		consumer:      consumer,
		minIdle:       minIdle,
		maxDeliveries: maxDeliveries,
		count:         count,
		handler:       h,
	}
}

// RunOnce performs one sweep: it lists entries idle at least minIdle, claims each
// to take ownership and read its body, and routes it. Entries whose pre-claim
// delivery count is at or over the ceiling are dead-lettered and acked; the rest
// are retried and left pending on failure. It returns how many it dead-lettered.
func (r *Reclaimer) RunOnce(ctx context.Context) (int, error) {
	pending, err := r.rdb.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: r.stream,
		Group:  r.group,
		Idle:   r.minIdle,
		Start:  "-",
		End:    "+",
		Count:  r.count,
	}).Result()
	if err != nil {
		return 0, err
	}
	routed := 0
	for _, pe := range pending {
		claimed, err := r.rdb.XClaim(ctx, &redis.XClaimArgs{
			Stream:   r.stream,
			Group:    r.group,
			Consumer: r.consumer,
			MinIdle:  r.minIdle,
			Messages: []string{pe.ID},
		}).Result()
		if err != nil {
			return routed, err
		}
		if len(claimed) == 0 {
			continue // trimmed out from under the PEL; nothing to claim
		}
		msg := claimed[0]
		if routeToDLQ(pe.RetryCount, r.maxDeliveries) {
			if err := r.moveToDLQ(ctx, msg, pe.RetryCount); err != nil {
				return routed, err
			}
			routed++
			continue
		}
		if err := r.handler(ctx, msg); err != nil {
			continue // leave pending for the next pass
		}
		if err := r.rdb.XAck(ctx, r.stream, r.group, msg.ID).Err(); err != nil {
			return routed, err
		}
	}
	return routed, nil
}

// moveToDLQ appends the poison entry to the dead-letter stream with its original
// payload plus failure metadata, then acks it in the source group so it leaves
// the PEL and cannot be reclaimed again.
func (r *Reclaimer) moveToDLQ(ctx context.Context, msg redis.XMessage, attempts int64) error {
	values := make(map[string]interface{}, len(msg.Values)+2)
	for k, v := range msg.Values {
		values[k] = v
	}
	values["dlq_source_id"] = msg.ID
	values["dlq_attempts"] = strconv.FormatInt(attempts, 10)
	if err := r.rdb.XAdd(ctx, &redis.XAddArgs{Stream: r.dlqStream, Values: values}).Err(); err != nil {
		return err
	}
	return r.rdb.XAck(ctx, r.stream, r.group, msg.ID).Err()
}
```

### The runnable demo

The demo feeds one order whose handler always fails, with `maxDeliveries` set to
three. After the first delivery (`ReadNew`), it runs the reclaim janitor in a loop,
advancing the miniredis clock a minute before each pass so the entry ages past the
idle gate. The first two passes retry (delivery count one, then two — below the
ceiling); the third pass sees a delivery count of three and routes the entry to the
dead-letter stream, acking it in the source. The demo then confirms the
dead-letter stream holds exactly one entry with `dlq_attempts` of three and that
the source group has zero pending.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"example.com/streamdlq"
)

func main() {
	srv, err := miniredis.Run()
	if err != nil {
		panic(err)
	}
	defer srv.Close()

	base := time.Now()
	srv.SetTime(base)

	rdb := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	defer rdb.Close()

	ctx := context.Background()
	const stream, dlq, group = "orders", "orders:dead", "fulfilment"
	if err := streamdlq.EnsureGroup(ctx, rdb, stream, group); err != nil {
		panic(err)
	}

	values := streamdlq.Encode(streamdlq.OrderEvent{ID: "order-poison", Type: "created", Amount: 1, Currency: "USD"})
	if err := rdb.XAdd(ctx, &redis.XAddArgs{Stream: stream, Values: values}).Err(); err != nil {
		panic(err)
	}

	w := streamdlq.NewWorker(rdb, stream, group, "worker-a")
	delivered, err := w.ReadNew(ctx, 10)
	if err != nil {
		panic(err)
	}
	fmt.Printf("delivered %d poison message\n", len(delivered))

	failing := func(context.Context, redis.XMessage) error { return errors.New("permanent failure") }
	rec := streamdlq.NewReclaimer(rdb, stream, dlq, group, "worker-b", 30*time.Second, 3, 100, failing)

	passes := 0
	for {
		passes++
		srv.SetTime(base.Add(time.Duration(passes) * time.Minute))
		routed, err := rec.RunOnce(ctx)
		if err != nil {
			panic(err)
		}
		if routed > 0 {
			break
		}
		if passes >= 10 {
			panic("message was never dead-lettered")
		}
	}
	fmt.Printf("routed to dead-letter after %d reclaim passes\n", passes)

	entries, err := rdb.XRange(ctx, dlq, "-", "+").Result()
	if err != nil {
		panic(err)
	}
	fmt.Printf("dead-letter stream length: %d\n", len(entries))
	fmt.Printf("dlq attempts recorded: %s\n", entries[0].Values["dlq_attempts"])

	pending, err := rdb.XPending(ctx, stream, group).Result()
	if err != nil {
		panic(err)
	}
	fmt.Printf("source pending: %d\n", pending.Count)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
delivered 1 poison message
routed to dead-letter after 3 reclaim passes
dead-letter stream length: 1
dlq attempts recorded: 3
source pending: 0
```

### Tests

`TestEncodeDecodeRoundTrip` proves the codec is symmetric — an event survives
`Encode` then `Decode` unchanged — and `TestDecodeMalformed` exercises the failure
branches where they live: a missing field, a non-string value, and a non-numeric
amount each surface a wrapped `ErrMalformedEvent` asserted via `errors.Is`.
`TestRouteToDLQ` walks the predicate across the threshold, including the exact
off-by-one: `(2,3)` is a retry, `(3,3)` and `(4,3)` are dead-letters, and the
minimal `(1,1)` boundary routes while `(0,1)` does not. `TestPoisonToDeadLetter`
is the end-to-end proof: an always-failing handler, `maxDeliveries` of three, and
after enough reclaim passes the entry appears exactly once in the dead-letter
stream — with `dlq_attempts` of three, a `dlq_source_id`, and its original payload
preserved — while the source group's pending count for it is zero. It then runs
one more sweep and asserts the dead-letter length stays one, proving the routing is
idempotent and a routed message is never dead-lettered twice.

Create `streamdlq_test.go`:

```go
package streamdlq

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	t.Parallel()
	want := OrderEvent{ID: "order-1", Type: "created", Amount: 4200, Currency: "USD"}
	got, err := Decode(Encode(want))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got != want {
		t.Errorf("round-trip = %+v, want %+v", got, want)
	}
}

func TestDecodeMalformed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   map[string]interface{}
	}{
		{"missing field", map[string]interface{}{"id": "o1", "type": "created", "currency": "USD"}},
		{"non-string value", map[string]interface{}{"id": "o1", "type": "created", "amount": 42, "currency": "USD"}},
		{"non-numeric amount", map[string]interface{}{"id": "o1", "type": "created", "amount": "NaN", "currency": "USD"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Decode(tc.in); !errors.Is(err, ErrMalformedEvent) {
				t.Errorf("Decode(%v) error = %v, want ErrMalformedEvent", tc.in, err)
			}
		})
	}
}

func TestRouteToDLQ(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		retryCount    int64
		maxDeliveries int64
		want          bool
	}{
		{"below threshold", 2, 3, false},
		{"at threshold", 3, 3, true},
		{"over threshold", 4, 3, true},
		{"minimal retry", 0, 1, false},
		{"minimal route", 1, 1, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := routeToDLQ(tc.retryCount, tc.maxDeliveries); got != tc.want {
				t.Errorf("routeToDLQ(%d,%d) = %v, want %v", tc.retryCount, tc.maxDeliveries, got, tc.want)
			}
		})
	}
}

func TestPoisonToDeadLetter(t *testing.T) {
	t.Parallel()
	srv := miniredis.RunT(t)
	base := time.Now()
	srv.SetTime(base)
	rdb := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	t.Cleanup(func() { rdb.Close() })

	ctx := t.Context()
	const stream, dlq, group = "orders", "orders:dead", "fulfilment"
	if err := EnsureGroup(ctx, rdb, stream, group); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}

	values := Encode(OrderEvent{ID: "order-poison", Type: "created", Amount: 1, Currency: "USD"})
	if err := rdb.XAdd(ctx, &redis.XAddArgs{Stream: stream, Values: values}).Err(); err != nil {
		t.Fatalf("XAdd: %v", err)
	}

	w := NewWorker(rdb, stream, group, "worker-a")
	if _, err := w.ReadNew(ctx, 10); err != nil {
		t.Fatalf("ReadNew: %v", err)
	}

	failing := func(context.Context, redis.XMessage) error { return errors.New("permanent failure") }
	rec := NewReclaimer(rdb, stream, dlq, group, "worker-b", 30*time.Second, 3, 100, failing)

	routedTotal := 0
	for pass := 1; pass <= 10 && routedTotal == 0; pass++ {
		srv.SetTime(base.Add(time.Duration(pass) * time.Minute))
		routed, err := rec.RunOnce(ctx)
		if err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		routedTotal += routed
	}
	if routedTotal != 1 {
		t.Fatalf("routed %d to DLQ, want 1", routedTotal)
	}

	entries, err := rdb.XRange(ctx, dlq, "-", "+").Result()
	if err != nil {
		t.Fatalf("XRange: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("dead-letter length = %d, want 1", len(entries))
	}
	if got := entries[0].Values["dlq_attempts"]; got != "3" {
		t.Errorf("dlq_attempts = %v, want 3", got)
	}
	if entries[0].Values["dlq_source_id"] == "" {
		t.Error("dlq_source_id missing")
	}
	if got := entries[0].Values["id"]; got != "order-poison" {
		t.Errorf("original payload id = %v, want order-poison", got)
	}

	pending, err := rdb.XPending(ctx, stream, group).Result()
	if err != nil {
		t.Fatalf("XPending: %v", err)
	}
	if pending.Count != 0 {
		t.Errorf("source pending = %d, want 0", pending.Count)
	}

	// Idempotency: another sweep must not dead-letter it again.
	srv.SetTime(base.Add(20 * time.Minute))
	if routed, err := rec.RunOnce(ctx); err != nil || routed != 0 {
		t.Fatalf("second RunOnce routed=%d err=%v, want 0,nil", routed, err)
	}
	again, err := rdb.XLen(ctx, dlq).Result()
	if err != nil {
		t.Fatalf("XLen: %v", err)
	}
	if again != 1 {
		t.Errorf("dead-letter length after re-sweep = %d, want 1", again)
	}
}

func Example() {
	fmt.Println(routeToDLQ(2, 3), routeToDLQ(3, 3), routeToDLQ(4, 3))
	// Output: false true true
}
```

## Review

The routing is correct when three properties hold. The predicate is inclusive at
the threshold — `routeToDLQ(max, max)` is true — so a message is dead-lettered on
exactly the delivery that reaches the ceiling, not one before or after;
`TestRouteToDLQ` pins that off-by-one. The dead-letter write happens before the
source ack, and the ack removes the entry from the PEL, so a routed message cannot
be found by a later sweep — that is what makes routing idempotent and is why the
second `RunOnce` is a no-op. And the dead-letter record carries enough to
investigate: the original payload plus `dlq_source_id` and `dlq_attempts`.

The traps: never route without a ceiling, or a permanently-failing message is
reclaimed forever and starves the group; claim before deciding so ownership and
idle reset are correct and a peer cannot grab the entry mid-decision; and remember
`RetryCount` counts deliveries, not failures — the initial `XREADGROUP` already
counts as one, so `maxDeliveries` is a delivery budget, not a retry budget.
Confirm with `go test -count=1 -race ./...`: the end-to-end test proves the entry
lands in the dead-letter stream exactly once, is acked in the source, and is not
double-routed.

## Resources

- [XCLAIM](https://redis.io/docs/latest/commands/xclaim/) — claiming specific IDs, min-idle-time, and the delivery-count increment.
- [XPENDING](https://redis.io/docs/latest/commands/xpending/) — the extended form's per-entry `RetryCount`.
- [Redis Streams](https://redis.io/docs/latest/develop/data-types/streams/) — why Redis does not dead-letter and how consumer groups track deliveries.
- [go-redis v9](https://pkg.go.dev/github.com/redis/go-redis/v9) — `XClaimArgs`, `XPendingExtArgs`, `XAddArgs`, and `XRange`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-pending-entries-and-crash-recovery.md](02-pending-entries-and-crash-recovery.md) | Next: [../05-watermill-event-pipelines/00-concepts.md](../05-watermill-event-pipelines/00-concepts.md)
