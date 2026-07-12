# Exercise 2: Pending Entries and Crash Recovery

Redis has no visibility timeout: an entry a consumer read but never acked stays in
that consumer's Pending Entries List forever, even after the consumer crashes.
This exercise builds the recovery half of a durable worker — the code that turns
at-least-once into actually-redelivered. On startup a consumer re-reads its own
PEL with the literal ID `0`; on a ticker it reclaims entries stranded by crashed
peers with `XAUTOCLAIM`, gated by a minimum idle time and paged with a cursor.

This module is fully self-contained: its own `go mod init`, its own event codec,
its own recovery functions, its own demo and tests over an in-process miniredis.
Nothing here imports another exercise.

## What you'll build

```text
streamrecover/                independent module: example.com/streamrecover
  go.mod                      go 1.24; requires go-redis/v9 and miniredis/v2
  event.go                    OrderEvent; Encode/Decode (string-valued codec)
  stream.go                   EnsureGroup; Worker.ReadNew (deliver '>' without ack)
  recover.go                  eligibleForReclaim, drainAutoClaim, ReclaimStranded,
                              DrainOwnPEL, EligiblePending
  cmd/
    demo/
      main.go                 miniredis: A reads-then-crashes, B reclaims after aging
  streamrecover_test.go       predicate + cursor-loop unit tests, miniredis reclaim
```

- Files: `event.go`, `stream.go`, `recover.go`, `cmd/demo/main.go`, `streamrecover_test.go`.
- Implement: a pure `eligibleForReclaim` idle predicate, a pure `drainAutoClaim` cursor loop, `ReclaimStranded` (XAUTOCLAIM paging), `DrainOwnPEL` (self-recovery with ID `0`), and `EligiblePending` (client-side observability).
- Test: table-driven predicate and cursor-loop tests, plus a miniredis end-to-end test where consumer A reads three entries and never acks, the clock is advanced past the idle gate, and consumer B reclaims exactly those three.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/50-messaging-and-event-driven/04-redis-streams-consumer-groups/02-pending-entries-and-crash-recovery/cmd/demo
cd go-solutions/50-messaging-and-event-driven/04-redis-streams-consumer-groups/02-pending-entries-and-crash-recovery
go mod edit -go=1.24
go get github.com/redis/go-redis/v9
go get github.com/alicebob/miniredis/v2
```

### Why recovery is your job, and the two paths it takes

When a consumer reads with `XREADGROUP ... >`, every delivered entry lands in that
consumer's PEL and stays there until `XACK`. If the process dies between reading
and acking, those entries are owned by a dead consumer and Redis never re-serves
them — there is no SQS-style visibility timeout. A durable worker therefore needs
two recovery paths.

The first is self-recovery on startup. A consumer that restarts under the same
name still owns whatever it read before crashing. Reading with the special ID `0`
(`XREADGROUP ... STREAMS s 0`) returns *this consumer's own pending entries*
rather than new ones, so it can finish them. Using `>` here would be the classic
mistake — `>` only ever returns brand-new, never-delivered messages, so it would
silently skip the very entries you are trying to recover.

The second is reclaiming peers' stranded entries. `XAUTOCLAIM` scans the group's
PEL from a cursor and transfers ownership of every entry idle longer than a
`min-idle-time` to the caller, returning the claimed messages and the next cursor.
You start at the cursor `0-0`, feed the returned cursor back in, and stop when it
comes back as `0-0`. The `min-idle-time` is the interlock that keeps two *live*
consumers from stealing each other's in-flight work: an entry being actively
processed has a small idle time and is not eligible. Reclaiming is not acking —
`XAUTOCLAIM` transfers ownership; the new owner still has to process and `XACK`.

### The pure core: an idle predicate and a cursor loop

Two pieces of the recovery logic have nothing to do with the network and are
extracted so they can be unit-tested exhaustively. `eligibleForReclaim` is the
idle gate — inclusive, so an entry idle exactly `minIdle` is eligible.
`drainAutoClaim` is the cursor loop expressed over an injected `fetch` function:
it accumulates claimed messages across pages and terminates when `fetch` reports
the terminal cursor `0-0`. Expressing the loop this way means the paging and
termination logic is tested with a fake `fetch` that returns a couple of pages,
with no server and no timing, while `ReclaimStranded` supplies the real
`XAUTOCLAIM` fetch.

`EligiblePending` shows the predicate in a real path: it lists all pending entries
with `XPENDINGEXT` and filters client-side with `eligibleForReclaim`, reporting
what a sweep would claim without taking ownership — the kind of read-only check a
monitoring endpoint exposes.

Create `event.go`:

```go
package streamrecover

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
package streamrecover

import (
	"context"
	"errors"
	"strings"

	"github.com/redis/go-redis/v9"
)

// EnsureGroup creates the stream and group idempotently, treating BUSYGROUP
// (group already exists) as success.
func EnsureGroup(ctx context.Context, rdb *redis.Client, stream, group string) error {
	err := rdb.XGroupCreateMkStream(ctx, stream, group, "0").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return err
	}
	return nil
}

// Worker delivers new entries to a named consumer. Here it is used to simulate a
// consumer that reads and then crashes before acking.
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
// returns them without acking, so they remain pending.
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

Create `recover.go`:

```go
package streamrecover

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// ReclaimConfig parameterises a reclaim sweep.
type ReclaimConfig struct {
	Stream   string
	Group    string
	Consumer string
	MinIdle  time.Duration
	Count    int64
}

// eligibleForReclaim reports whether an entry idle for idle is stale enough to
// reclaim given the min-idle gate. The gate is inclusive.
func eligibleForReclaim(idle, minIdle time.Duration) bool {
	return idle >= minIdle
}

// drainAutoClaim runs the XAUTOCLAIM cursor loop to completion: starting at the
// "0-0" cursor it accumulates every claimed message across pages and stops when
// fetch reports the terminal cursor "0-0". fetch performs one XAUTOCLAIM call
// for the given cursor and returns the claimed messages plus the next cursor.
func drainAutoClaim(fetch func(cursor string) ([]redis.XMessage, string, error)) ([]redis.XMessage, error) {
	var claimed []redis.XMessage
	cursor := "0-0"
	for {
		msgs, next, err := fetch(cursor)
		if err != nil {
			return claimed, err
		}
		claimed = append(claimed, msgs...)
		if next == "0-0" {
			return claimed, nil
		}
		cursor = next
	}
}

// ReclaimStranded transfers every entry idle at least cfg.MinIdle to
// cfg.Consumer, paging the whole PEL with XAUTOCLAIM, and returns the claimed
// messages. It does not ack: the caller must process and XACK.
func ReclaimStranded(ctx context.Context, rdb *redis.Client, cfg ReclaimConfig) ([]redis.XMessage, error) {
	return drainAutoClaim(func(cursor string) ([]redis.XMessage, string, error) {
		msgs, next, err := rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
			Stream:   cfg.Stream,
			Group:    cfg.Group,
			Consumer: cfg.Consumer,
			MinIdle:  cfg.MinIdle,
			Start:    cursor,
			Count:    cfg.Count,
		}).Result()
		return msgs, next, err
	})
}

// DrainOwnPEL re-reads this consumer's own pending entries by passing the literal
// ID "0" to XREADGROUP. This is startup self-recovery: '>' would return only new
// messages, never the entries this consumer already holds un-acked.
func DrainOwnPEL(ctx context.Context, rdb *redis.Client, stream, group, consumer string) ([]redis.XMessage, error) {
	streams, err := rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    group,
		Consumer: consumer,
		Streams:  []string{stream, "0"},
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

// EligiblePending lists pending entries and returns those idle at least minIdle,
// filtering client-side with eligibleForReclaim. It reports what a sweep would
// claim without taking ownership.
func EligiblePending(ctx context.Context, rdb *redis.Client, stream, group string, minIdle time.Duration, count int64) ([]redis.XPendingExt, error) {
	all, err := rdb.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: stream,
		Group:  group,
		Start:  "-",
		End:    "+",
		Count:  count,
	}).Result()
	if err != nil {
		return nil, err
	}
	var eligible []redis.XPendingExt
	for _, pe := range all {
		if eligibleForReclaim(pe.Idle, minIdle) {
			eligible = append(eligible, pe)
		}
	}
	return eligible, nil
}
```

### The runnable demo

The demo makes the "no automatic redelivery" property concrete. Because miniredis
has a controllable clock, the demo pins a base time, has consumer `worker-a` read
three entries and then "crash" (never ack), advances the clock sixty seconds so
the PEL entries age past the thirty-second idle gate, and then has `worker-b`
reclaim them. It prints the reclaimed entries' domain IDs (the server-assigned
stream IDs are timestamps and would not be reproducible) and confirms the group's
pending count returns to zero once `worker-b` processes and acks. One subtlety
worth internalizing: miniredis's `FastForward` only decrements key TTLs; it does
*not* move the clock the stream commands read for idle time, so the demo advances
time with `SetTime`, which does.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"example.com/streamrecover"
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
	const stream, group = "orders", "fulfilment"
	if err := streamrecover.EnsureGroup(ctx, rdb, stream, group); err != nil {
		panic(err)
	}

	for i := range 3 {
		values := streamrecover.Encode(streamrecover.OrderEvent{
			ID: fmt.Sprintf("order-%d", i), Type: "created", Amount: int64(i), Currency: "USD",
		})
		if err := rdb.XAdd(ctx, &redis.XAddArgs{Stream: stream, Values: values}).Err(); err != nil {
			panic(err)
		}
	}

	crashed := streamrecover.NewWorker(rdb, stream, group, "worker-a")
	got, err := crashed.ReadNew(ctx, 10)
	if err != nil {
		panic(err)
	}
	fmt.Printf("worker-a read %d, acked 0 (crashed)\n", len(got))

	// Time passes; the PEL entries age past the reclaim gate.
	srv.SetTime(base.Add(60 * time.Second))

	eligible, err := streamrecover.EligiblePending(ctx, rdb, stream, group, 30*time.Second, 100)
	if err != nil {
		panic(err)
	}
	fmt.Printf("eligible for reclaim: %d\n", len(eligible))

	reclaimed, err := streamrecover.ReclaimStranded(ctx, rdb, streamrecover.ReclaimConfig{
		Stream: stream, Group: group, Consumer: "worker-b", MinIdle: 30 * time.Second, Count: 100,
	})
	if err != nil {
		panic(err)
	}

	domainIDs := make([]string, 0, len(reclaimed))
	for _, m := range reclaimed {
		e, err := streamrecover.Decode(m.Values)
		if err != nil {
			panic(err)
		}
		domainIDs = append(domainIDs, e.ID)
		if err := rdb.XAck(ctx, stream, group, m.ID).Err(); err != nil {
			panic(err)
		}
	}
	sort.Strings(domainIDs)
	fmt.Printf("worker-b reclaimed %d: %v\n", len(domainIDs), domainIDs)

	pending, err := rdb.XPending(ctx, stream, group).Result()
	if err != nil {
		panic(err)
	}
	fmt.Printf("group pending after B processes: %d\n", pending.Count)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
worker-a read 3, acked 0 (crashed)
eligible for reclaim: 3
worker-b reclaimed 3: [order-0 order-1 order-2]
group pending after B processes: 0
```

### Tests

`TestEligibleForReclaim` pins the idle gate at its boundary, including the
inclusive case where idle equals `minIdle`. `TestDrainAutoClaim` drives the cursor
loop with a fake `fetch` that returns two pages and then the terminal cursor,
proving it accumulates across pages and stops — no server, no clock. The miniredis
tests prove both recovery paths: `TestReclaimStranded` has consumer A read three
entries and never ack, advances the clock past the idle gate, and asserts B claims
exactly those three while A's per-consumer pending drops to zero and B's rises to
three; `TestSelfRecovery` proves that after A reads without acking, re-reading A's
own PEL with ID `0` returns those same entries.

Create `streamrecover_test.go`:

```go
package streamrecover

import (
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestEligibleForReclaim(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		idle    time.Duration
		minIdle time.Duration
		want    bool
	}{
		{"below gate", 5 * time.Second, 10 * time.Second, false},
		{"at gate", 10 * time.Second, 10 * time.Second, true},
		{"above gate", 30 * time.Second, 10 * time.Second, true},
		{"zero gate", 0, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := eligibleForReclaim(tc.idle, tc.minIdle); got != tc.want {
				t.Errorf("eligibleForReclaim(%v,%v) = %v, want %v", tc.idle, tc.minIdle, got, tc.want)
			}
		})
	}
}

// pagedFetch returns a fetch closure yielding the given pages in order, then the
// terminal cursor "0-0".
func pagedFetch(pages [][]redis.XMessage) func(string) ([]redis.XMessage, string, error) {
	i := 0
	return func(string) ([]redis.XMessage, string, error) {
		if i >= len(pages) {
			return nil, "0-0", nil
		}
		page := pages[i]
		i++
		next := "42-0"
		if i >= len(pages) {
			next = "0-0"
		}
		return page, next, nil
	}
}

func TestDrainAutoClaim(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		pages [][]redis.XMessage
		want  []string
	}{
		{
			name:  "two pages",
			pages: [][]redis.XMessage{{{ID: "1-0"}, {ID: "2-0"}}, {{ID: "3-0"}}},
			want:  []string{"1-0", "2-0", "3-0"},
		},
		{
			name:  "single empty page",
			pages: [][]redis.XMessage{{}},
			want:  nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			msgs, err := drainAutoClaim(pagedFetch(tc.pages))
			if err != nil {
				t.Fatalf("drainAutoClaim: %v", err)
			}
			var got []string
			for _, m := range msgs {
				got = append(got, m.ID)
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReclaimStranded(t *testing.T) {
	t.Parallel()
	srv := miniredis.RunT(t)
	base := time.Now()
	srv.SetTime(base)
	rdb := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	t.Cleanup(func() { rdb.Close() })

	ctx := t.Context()
	const stream, group = "orders", "fulfilment"
	if err := EnsureGroup(ctx, rdb, stream, group); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	for i := range 3 {
		values := Encode(OrderEvent{ID: fmt.Sprintf("o%d", i), Type: "created", Amount: int64(i), Currency: "USD"})
		if err := rdb.XAdd(ctx, &redis.XAddArgs{Stream: stream, Values: values}).Err(); err != nil {
			t.Fatalf("XAdd: %v", err)
		}
	}

	a := NewWorker(rdb, stream, group, "worker-a")
	got, err := a.ReadNew(ctx, 10)
	if err != nil {
		t.Fatalf("ReadNew: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("worker-a read %d, want 3", len(got))
	}

	srv.SetTime(base.Add(60 * time.Second))

	reclaimed, err := ReclaimStranded(ctx, rdb, ReclaimConfig{
		Stream: stream, Group: group, Consumer: "worker-b", MinIdle: 30 * time.Second, Count: 100,
	})
	if err != nil {
		t.Fatalf("ReclaimStranded: %v", err)
	}
	if len(reclaimed) != 3 {
		t.Fatalf("reclaimed %d, want 3", len(reclaimed))
	}

	pending, err := rdb.XPending(ctx, stream, group).Result()
	if err != nil {
		t.Fatalf("XPending: %v", err)
	}
	if pending.Consumers["worker-a"] != 0 {
		t.Errorf("worker-a pending = %d, want 0", pending.Consumers["worker-a"])
	}
	if pending.Consumers["worker-b"] != 3 {
		t.Errorf("worker-b pending = %d, want 3", pending.Consumers["worker-b"])
	}
}

func TestSelfRecovery(t *testing.T) {
	t.Parallel()
	srv := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	t.Cleanup(func() { rdb.Close() })

	ctx := t.Context()
	const stream, group = "orders", "fulfilment"
	if err := EnsureGroup(ctx, rdb, stream, group); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	for i := range 2 {
		values := Encode(OrderEvent{ID: fmt.Sprintf("o%d", i), Type: "created", Amount: int64(i), Currency: "USD"})
		if err := rdb.XAdd(ctx, &redis.XAddArgs{Stream: stream, Values: values}).Err(); err != nil {
			t.Fatalf("XAdd: %v", err)
		}
	}

	a := NewWorker(rdb, stream, group, "worker-a")
	if _, err := a.ReadNew(ctx, 10); err != nil {
		t.Fatalf("ReadNew: %v", err)
	}

	own, err := DrainOwnPEL(ctx, rdb, stream, group, "worker-a")
	if err != nil {
		t.Fatalf("DrainOwnPEL: %v", err)
	}
	if len(own) != 2 {
		t.Fatalf("self-recovery returned %d, want 2", len(own))
	}
}

func Example() {
	msgs, _ := drainAutoClaim(pagedFetch([][]redis.XMessage{
		{{ID: "1-0"}, {ID: "2-0"}},
		{{ID: "3-0"}},
	}))
	for _, m := range msgs {
		fmt.Print(m.ID, " ")
	}
	fmt.Println()
	// Output: 1-0 2-0 3-0
}
```

## Review

The recovery code is correct when three things hold. The idle gate is inclusive
and never lets a live consumer's in-flight work be stolen — `min-idle-time` must
sit well above worst-case processing latency, and `TestEligibleForReclaim` pins
the boundary. The cursor loop accumulates across pages and terminates on the
`0-0` sentinel — never on the first page — which `TestDrainAutoClaim` proves with a
fake fetch. And the two special IDs are used correctly: `>` for new work, `0` for
this consumer's own PEL; swapping them is the mistake that makes self-recovery
silently skip the entries it was meant to finish.

The traps: do not expect `FastForward` to age the PEL — it only touches TTLs, so
the tests advance the clock with `SetTime`; do not treat reclaim as completion —
`XAUTOCLAIM` transfers ownership but the new owner still must process and `XACK`,
which is why `TestReclaimStranded` checks that pending simply moved from A to B
rather than dropping to zero; and remember reclaim increments each entry's
delivery count, which the next exercise turns into a poison-message ceiling.
Confirm with `go test -count=1 -race ./...`.

## Resources

- [XAUTOCLAIM](https://redis.io/docs/latest/commands/xautoclaim/) — min-idle-time, the cursor, and the return shape.
- [XPENDING](https://redis.io/docs/latest/commands/xpending/) — the summary and extended (per-entry idle and delivery-count) forms.
- [go-redis v9](https://pkg.go.dev/github.com/redis/go-redis/v9) — `XAutoClaimArgs`, `XPendingExtArgs`, `XPendingExt`, and `XMessage`.
- [miniredis v2](https://pkg.go.dev/github.com/alicebob/miniredis/v2) — `SetTime` for driving the stream clock in tests.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-consumer-group-worker.md](01-consumer-group-worker.md) | Next: [03-poison-messages-and-dead-letter-stream.md](03-poison-messages-and-dead-letter-stream.md)
