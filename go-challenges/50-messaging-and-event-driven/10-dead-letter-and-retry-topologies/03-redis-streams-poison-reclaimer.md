# Exercise 3: A Redis Streams Poison Reclaimer with DLQ Parking and Redrive

Redis Streams gives you consumer groups and a Pending Entries List, but no
timeout-based redelivery and no dead-letter queue — you build both. This exercise
builds the reclaimer: a background loop that finds stuck pending entries, decides
per entry whether to reprocess or park them as poison based on their delivery
count, moves poison to a `<stream>:dlq` stream with triage metadata, and — the
part you want at 3am — a redrive that returns fixed messages to the live stream.

This module is fully self-contained. The decision core (`Decide`,
`BuildDLQValues`) is pure and offline-tested; the `XAUTOCLAIM`/`XPENDING`/
`XADD`/`XACK` reclaim and redrive loop lives behind `//go:build integration`
because miniredis does not implement those stream commands fully enough, so the
integration test runs against a real Redis. Nothing here imports another
exercise.

## What you'll build

```text
redisreclaimer/                  independent module: example.com/redisreclaimer
  go.mod                         go 1.26; requires go-redis under the integration tag
  decision.go                    ReclaimAction; Decide; BuildDLQValues (pure)
  reclaimer_integration.go       //go:build integration — Reclaimer, park, reprocess, Redrive
  cmd/
    demo/
      main.go                    runnable pure demo: decisions + a DLQ entry
  decision_test.go               Decide + BuildDLQValues table tests + Example
  reclaimer_integration_test.go  //go:build integration — reclaim-to-DLQ then redrive
```

- Files: `decision.go`, `reclaimer_integration.go`, `cmd/demo/main.go`, `decision_test.go`, `reclaimer_integration_test.go`.
- Implement: `Decide(retryCount, idle, minIdle, threshold)` returning skip/reprocess/park, `BuildDLQValues` enriching a parked entry, and the integration `Reclaimer` with `ReclaimOnce`, `park`, `reprocess`, and `Redrive`.
- Test: table-driven `Decide` and `BuildDLQValues` with an `Example`; a `//go:build integration` test that delivers entries without acking, parks them as poison in one pass, and redrives them back.
- Verify: `go test -count=1 -race ./...` (offline core); the integration test runs against a real Redis on localhost:6379.

Set up the module. The pure core needs no dependency; the integration file does:

```bash
go mod edit -go=1.26
go get github.com/redis/go-redis/v9   # only needed to compile/run with -tags integration
```

### The reclaimer, and why Redis needs one

When a consumer in a Redis Streams group reads with `XREADGROUP`, the entry
enters that consumer's Pending Entries List (PEL) and stays there until it is
`XACK`ed. If the consumer crashes mid-processing, the entry is stuck in the PEL
forever — Redis, unlike SQS or JetStream, does not automatically redeliver it on
a timeout. The recovery mechanism is `XAUTOCLAIM`/`XCLAIM`: another consumer
*claims* entries that have been idle longer than a minimum, taking ownership so it
can retry them. Each claim increments the entry's delivery count, visible through
`XPENDING`. The reclaimer is the background loop that does this systematically:
survey the stuck entries, and for each one decide — using the delivery count —
whether it is merely stuck (reprocess) or genuinely poison (park in a DLQ).

### The decision is pure; the Redis calls are the edge

`Decide` is the entire policy in one testable function. Its inputs are the
entry's `RetryCount` (from `XPENDING`), its idle time, the minimum idle before an
entry counts as stuck, and the poison threshold. The ordering is deliberate: an
entry that has not been idle long enough is *skipped* even if its retry count is
high, because its current owner may still be mid-flight and about to ack — only a
genuinely stuck entry is a candidate for parking or reprocessing. Past `minIdle`,
an entry at or above the threshold is *parked* (poison), and one below it is
*reprocessed*. `BuildDLQValues` builds the field map for the parked entry: it
copies the original event fields and adds triage metadata (`dlq_orig_id`,
`dlq_attempts`, `dlq_first_seen`, `dlq_last_error`), all as strings so the DLQ
entry round-trips through the same decoder as a live one.

Create `decision.go`:

```go
package redisreclaimer

import (
	"strconv"
	"time"
)

// ReclaimAction is the decision for one stuck pending entry.
type ReclaimAction int

const (
	// Skip leaves the entry in the Pending Entries List: it has not been idle
	// long enough to be considered stuck, so its original consumer may still ack.
	Skip ReclaimAction = iota
	// Reprocess claims the entry to this reclaimer and retries it: stuck, but its
	// delivery count is still under the poison threshold.
	Reprocess
	// Park moves the entry to the DLQ stream and acks the original: its delivery
	// count has reached the threshold, so it is treated as poison.
	Park
)

func (a ReclaimAction) String() string {
	switch a {
	case Reprocess:
		return "reprocess"
	case Park:
		return "park"
	default:
		return "skip"
	}
}

// Decide chooses what to do with a pending entry given how many times it has been
// delivered (RetryCount from XPENDING), how long it has been idle, the minimum
// idle time before an entry is considered stuck, and the poison threshold. The
// order is deliberate: an entry that is not yet idle enough is skipped even if its
// retry count is high, because its current owner may be mid-flight; only a
// genuinely stuck entry is parked or reprocessed.
func Decide(retryCount int64, idle, minIdle time.Duration, threshold int64) ReclaimAction {
	if idle < minIdle {
		return Skip
	}
	if retryCount >= threshold {
		return Park
	}
	return Reprocess
}

// DLQ field keys added to a parked entry alongside the original event fields.
const (
	FieldOrigID    = "dlq_orig_id"
	FieldAttempts  = "dlq_attempts"
	FieldFirstSeen = "dlq_first_seen"
	FieldLastError = "dlq_last_error"
)

// BuildDLQValues builds the field map for the XADD that parks an entry in the DLQ
// stream. It copies the original event fields and adds triage metadata: the
// source entry id, the delivery count, when it was first seen, and the last error.
// Storing everything as strings matches how Redis returns stream fields, so the
// DLQ entry round-trips through the same decoder as a live one.
func BuildDLQValues(origID string, attempts int64, firstSeen time.Time, lastErr string, body map[string]string) map[string]any {
	out := make(map[string]any, len(body)+4)
	for k, v := range body {
		out[k] = v
	}
	out[FieldOrigID] = origID
	out[FieldAttempts] = strconv.FormatInt(attempts, 10)
	out[FieldFirstSeen] = firstSeen.UTC().Format(time.RFC3339)
	out[FieldLastError] = lastErr
	return out
}
```

### The reclaimer adapter (integration)

The Redis I/O is behind `//go:build integration` because it imports
`github.com/redis/go-redis/v9`, and because miniredis's `XAUTOCLAIM`/`XPENDING`
coverage is incomplete — this path needs a real instance. `ReclaimOnce` surveys
the pending entries idle for at least `minIdle` via `XPENDING` with the IDLE
filter, then applies the pure `Decide` to each. `park` claims the entry with
`XCLAIM` (so its body is readable and no other consumer competes), copies it into
the DLQ stream with `XADD` and the enriched values, then `XACK`s the original so
it leaves the PEL — the ack is what makes the removal durable. `reprocess` claims
and runs the handler, acking only on success; on failure it leaves the entry
pending so a later cycle sees a higher delivery count and eventually parks it.
`Redrive` is the operator tool: it reads DLQ entries with `XRANGE`, strips the
`dlq_*` triage fields so the redriven event matches its original shape, `XADD`s
each back to the live stream, and `XDEL`s it from the DLQ.

Create `reclaimer_integration.go`:

```go
//go:build integration

// This file holds the Redis I/O for the poison reclaimer. It is excluded from the
// default build (the offline gate) and compiled only with -tags integration
// against a real Redis, because it imports github.com/redis/go-redis/v9.
// miniredis does not implement XAUTOCLAIM/XPENDING fully enough to exercise this
// path, so a real instance is required. The pure decision core (Decide,
// BuildDLQValues) lives in the untagged files and is tested offline.
package redisreclaimer

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Reclaimer surveys the pending entries of a consumer group, parks poison entries
// in a DLQ stream, and reprocesses the merely-stuck ones. It also implements the
// redrive path that returns fixed messages to the live stream.
type Reclaimer struct {
	rdb       *redis.Client
	stream    string
	group     string
	consumer  string // the reclaimer's own consumer name in the group
	dlqStream string
	minIdle   time.Duration
	threshold int64
	batch     int64
	handler   func(context.Context, redis.XMessage) error
}

// NewReclaimer builds a reclaimer. handler reprocesses a claimed entry; returning
// an error leaves it pending so a later cycle re-evaluates it (its delivery count
// will have grown toward the poison threshold).
func NewReclaimer(rdb *redis.Client, stream, group, consumer, dlqStream string, minIdle time.Duration, threshold, batch int64, handler func(context.Context, redis.XMessage) error) *Reclaimer {
	return &Reclaimer{
		rdb:       rdb,
		stream:    stream,
		group:     group,
		consumer:  consumer,
		dlqStream: dlqStream,
		minIdle:   minIdle,
		threshold: threshold,
		batch:     batch,
		handler:   handler,
	}
}

// ReclaimOnce runs one survey-and-act pass. It reads pending entries idle for at
// least minIdle (XPENDING with the IDLE filter), classifies each with the pure
// Decide, and parks or reprocesses accordingly.
func (r *Reclaimer) ReclaimOnce(ctx context.Context) (parked, reprocessed int, err error) {
	pending, err := r.rdb.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: r.stream,
		Group:  r.group,
		Idle:   r.minIdle,
		Start:  "-",
		End:    "+",
		Count:  r.batch,
	}).Result()
	if err != nil {
		return 0, 0, fmt.Errorf("xpending: %w", err)
	}
	for _, p := range pending {
		switch Decide(p.RetryCount, p.Idle, r.minIdle, r.threshold) {
		case Skip:
			continue
		case Park:
			if perr := r.park(ctx, p); perr != nil {
				return parked, reprocessed, perr
			}
			parked++
		case Reprocess:
			if rerr := r.reprocess(ctx, p); rerr != nil {
				return parked, reprocessed, rerr
			}
			reprocessed++
		}
	}
	return parked, reprocessed, nil
}

// park claims the entry (so its body is readable and no other consumer competes),
// copies it into the DLQ stream with triage metadata, then acks the original so it
// leaves the Pending Entries List.
func (r *Reclaimer) park(ctx context.Context, p redis.XPendingExt) error {
	msgs, err := r.rdb.XClaim(ctx, &redis.XClaimArgs{
		Stream:   r.stream,
		Group:    r.group,
		Consumer: r.consumer,
		MinIdle:  r.minIdle,
		Messages: []string{p.ID},
	}).Result()
	if err != nil {
		return fmt.Errorf("xclaim %s: %w", p.ID, err)
	}
	if len(msgs) == 0 {
		return nil // acked or claimed by someone else between survey and claim
	}
	m := msgs[0]
	vals := BuildDLQValues(m.ID, p.RetryCount, time.Now(), "max attempts exceeded", toStringMap(m.Values))
	if err := r.rdb.XAdd(ctx, &redis.XAddArgs{Stream: r.dlqStream, Values: vals}).Err(); err != nil {
		return fmt.Errorf("xadd dlq: %w", err)
	}
	if err := r.rdb.XAck(ctx, r.stream, r.group, m.ID).Err(); err != nil {
		return fmt.Errorf("xack %s: %w", m.ID, err)
	}
	return nil
}

// reprocess claims the stuck entry and runs the handler; on success it acks. On
// failure it leaves the entry pending (unacked) so a later cycle sees a higher
// delivery count and eventually parks it.
func (r *Reclaimer) reprocess(ctx context.Context, p redis.XPendingExt) error {
	msgs, err := r.rdb.XClaim(ctx, &redis.XClaimArgs{
		Stream:   r.stream,
		Group:    r.group,
		Consumer: r.consumer,
		MinIdle:  r.minIdle,
		Messages: []string{p.ID},
	}).Result()
	if err != nil {
		return fmt.Errorf("xclaim %s: %w", p.ID, err)
	}
	if len(msgs) == 0 {
		return nil
	}
	if herr := r.handler(ctx, msgs[0]); herr != nil {
		return nil // leave pending for the next cycle
	}
	if err := r.rdb.XAck(ctx, r.stream, r.group, msgs[0].ID).Err(); err != nil {
		return fmt.Errorf("xack %s: %w", msgs[0].ID, err)
	}
	return nil
}

// Redrive moves up to count DLQ entries back to the live stream after a fix,
// stripping the dlq_* triage fields so the redriven event matches its original
// shape, and deleting each from the DLQ once it is re-added.
func (r *Reclaimer) Redrive(ctx context.Context, count int64) (int, error) {
	entries, err := r.rdb.XRangeN(ctx, r.dlqStream, "-", "+", count).Result()
	if err != nil {
		return 0, fmt.Errorf("xrange dlq: %w", err)
	}
	n := 0
	for _, e := range entries {
		body := make(map[string]any, len(e.Values))
		for k, v := range e.Values {
			if strings.HasPrefix(k, "dlq_") {
				continue
			}
			body[k] = v
		}
		if err := r.rdb.XAdd(ctx, &redis.XAddArgs{Stream: r.stream, Values: body}).Err(); err != nil {
			return n, fmt.Errorf("xadd redrive: %w", err)
		}
		if err := r.rdb.XDel(ctx, r.dlqStream, e.ID).Err(); err != nil {
			return n, fmt.Errorf("xdel dlq: %w", err)
		}
		n++
	}
	return n, nil
}

// Run drives ReclaimOnce on a ticker until ctx is cancelled.
func (r *Reclaimer) Run(ctx context.Context, interval time.Duration) error {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if _, _, err := r.ReclaimOnce(ctx); err != nil {
				return err
			}
		}
	}
}

// toStringMap coerces the interface{}-valued field map Redis returns into strings.
func toStringMap(v map[string]interface{}) map[string]string {
	out := make(map[string]string, len(v))
	for k, val := range v {
		if s, ok := val.(string); ok {
			out[k] = s
			continue
		}
		out[k] = fmt.Sprint(val)
	}
	return out
}
```

### The runnable demo

The demo stays offline: it prints the reclaim decision for a range of
`(retryCount, idle)` inputs and shows a fully-built DLQ entry. It touches only
the pure functions, so it runs with no Redis.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sort"
	"time"

	"example.com/redisreclaimer"
)

func main() {
	const minIdle = 30 * time.Second
	const threshold = 5

	fmt.Println("reclaim decisions (minIdle=30s, threshold=5):")
	cases := []struct {
		retry int64
		idle  time.Duration
	}{
		{1, 5 * time.Second}, // fresh: owner may still ack
		{2, time.Minute},     // stuck, under threshold: reprocess
		{5, time.Minute},     // stuck, at threshold: park as poison
		{9, 2 * time.Minute}, // stuck, over threshold: park
	}
	for _, c := range cases {
		a := redisreclaimer.Decide(c.retry, c.idle, minIdle, threshold)
		fmt.Printf("  retry=%d idle=%-8s -> %s\n", c.retry, c.idle, a)
	}

	firstSeen := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	vals := redisreclaimer.BuildDLQValues(
		"1690000000000-0", 5, firstSeen, "decode: unknown type",
		map[string]string{"id": "order-7", "type": "created"},
	)
	fmt.Println("dlq entry fields:")
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %s = %v\n", k, vals[k])
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
reclaim decisions (minIdle=30s, threshold=5):
  retry=1 idle=5s       -> skip
  retry=2 idle=1m0s     -> reprocess
  retry=5 idle=1m0s     -> park
  retry=9 idle=2m0s     -> park
dlq entry fields:
  dlq_attempts = 5
  dlq_first_seen = 2026-07-01T12:00:00Z
  dlq_last_error = decode: unknown type
  dlq_orig_id = 1690000000000-0
  id = order-7
  type = created
```

### Tests

The offline tests are the fast, pure core. `TestDecide` is table-driven over the
three actions and the boundaries (not idle enough, under/at/over threshold, and
exactly `minIdle`), pinning the `>=` threshold comparison that decides poison.
`TestBuildDLQValues` proves the parked entry carries every original field plus the
four `dlq_*` triage fields with the right values. `ExampleDecide` locks the
decision output for a fresh, a stuck-under, and an at-threshold entry.

Create `decision_test.go`:

```go
package redisreclaimer

import (
	"fmt"
	"testing"
	"time"
)

func TestDecide(t *testing.T) {
	t.Parallel()
	const minIdle = 30 * time.Second
	const threshold int64 = 5
	tests := []struct {
		name  string
		retry int64
		idle  time.Duration
		want  ReclaimAction
	}{
		{"not idle enough", 9, time.Second, Skip},
		{"stuck under threshold", 2, time.Minute, Reprocess},
		{"stuck at threshold", 5, time.Minute, Park},
		{"stuck over threshold", 8, 2 * time.Minute, Park},
		{"exactly minIdle, under threshold", 1, minIdle, Reprocess},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Decide(tc.retry, tc.idle, minIdle, threshold); got != tc.want {
				t.Errorf("Decide(retry=%d, idle=%s) = %s, want %s", tc.retry, tc.idle, got, tc.want)
			}
		})
	}
}

func TestBuildDLQValues(t *testing.T) {
	t.Parallel()
	firstSeen := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	vals := BuildDLQValues("100-0", 5, firstSeen, "boom", map[string]string{"id": "o1", "type": "created"})

	want := map[string]string{
		"id":           "o1",
		"type":         "created",
		FieldOrigID:    "100-0",
		FieldAttempts:  "5",
		FieldFirstSeen: "2026-07-01T12:00:00Z",
		FieldLastError: "boom",
	}
	if len(vals) != len(want) {
		t.Fatalf("got %d fields, want %d", len(vals), len(want))
	}
	for k, v := range want {
		got, ok := vals[k].(string)
		if !ok || got != v {
			t.Errorf("field %q = %v, want %q", k, vals[k], v)
		}
	}
}

func ExampleDecide() {
	const minIdle = 30 * time.Second
	fmt.Println(Decide(1, 5*time.Second, minIdle, 5)) // fresh
	fmt.Println(Decide(2, time.Minute, minIdle, 5))   // stuck, under
	fmt.Println(Decide(5, time.Minute, minIdle, 5))   // stuck, at threshold
	// Output:
	// skip
	// reprocess
	// park
}
```

The integration test proves the reclaim-and-redrive cycle against a real Redis.
It delivers two entries without acking (so they sit in the PEL with a delivery
count of 1), runs one reclaim pass with `threshold: 1` and `minIdle: 0` so both
are parked as poison, asserts the DLQ holds two enriched entries and the PEL is
empty, then redrives them back and asserts the DLQ is empty and the live stream
holds the redriven copies. It is deferred to a networked run and never compiled
by the offline gate.

Create `reclaimer_integration_test.go`:

```go
//go:build integration

package redisreclaimer

import (
	"context"
	"testing"

	"github.com/redis/go-redis/v9"
)

// TestReclaimAndRedrive is a networked integration test. It is excluded from the
// offline gate and requires a real Redis on localhost:6379 (miniredis does not
// implement XAUTOCLAIM/XPENDING fully). It delivers two entries without acking,
// runs one reclaim pass that parks them as poison, and then redrives them back.
func TestReclaimAndRedrive(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("no redis available: %v", err)
	}
	defer rdb.Close()
	if err := rdb.FlushDB(ctx).Err(); err != nil {
		t.Fatalf("flushdb: %v", err)
	}

	const stream, group, dlq = "orders_it", "fulfil_it", "orders_it:dlq"

	if err := rdb.XGroupCreateMkStream(ctx, stream, group, "0").Err(); err != nil {
		t.Fatalf("xgroup create: %v", err)
	}
	for _, id := range []string{"a", "b"} {
		if err := rdb.XAdd(ctx, &redis.XAddArgs{
			Stream: stream,
			Values: map[string]any{"id": id, "type": "created"},
		}).Err(); err != nil {
			t.Fatalf("xadd: %v", err)
		}
	}

	// Deliver both to a consumer that never acks: they land in the PEL with a
	// delivery count of 1.
	if _, err := rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    group,
		Consumer: "worker-1",
		Streams:  []string{stream, ">"},
		Count:    10,
	}).Result(); err != nil {
		t.Fatalf("xreadgroup: %v", err)
	}

	// threshold 1 and minIdle 0 make both entries poison on the first pass.
	handled := 0
	rc := NewReclaimer(rdb, stream, group, "reclaimer", dlq, 0, 1, 100,
		func(context.Context, redis.XMessage) error { handled++; return nil })

	parked, reprocessed, err := rc.ReclaimOnce(ctx)
	if err != nil {
		t.Fatalf("ReclaimOnce: %v", err)
	}
	if parked != 2 || reprocessed != 0 {
		t.Fatalf("parked=%d reprocessed=%d, want 2/0", parked, reprocessed)
	}

	// The DLQ now holds two enriched entries and the PEL is empty.
	dlqEntries, err := rdb.XRange(ctx, dlq, "-", "+").Result()
	if err != nil {
		t.Fatalf("xrange dlq: %v", err)
	}
	if len(dlqEntries) != 2 {
		t.Fatalf("dlq has %d entries, want 2", len(dlqEntries))
	}
	if got := dlqEntries[0].Values[FieldLastError]; got != "max attempts exceeded" {
		t.Errorf("dlq last-error = %v, want %q", got, "max attempts exceeded")
	}
	pend, err := rdb.XPending(ctx, stream, group).Result()
	if err != nil {
		t.Fatalf("xpending: %v", err)
	}
	if pend.Count != 0 {
		t.Fatalf("pending after park = %d, want 0", pend.Count)
	}

	// Redrive returns both messages to the live stream and empties the DLQ.
	n, err := rc.Redrive(ctx, 10)
	if err != nil {
		t.Fatalf("Redrive: %v", err)
	}
	if n != 2 {
		t.Fatalf("redrove %d, want 2", n)
	}
	remaining, err := rdb.XLen(ctx, dlq).Result()
	if err != nil {
		t.Fatalf("xlen dlq: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("dlq len after redrive = %d, want 0", remaining)
	}
	live, err := rdb.XRange(ctx, stream, "-", "+").Result()
	if err != nil {
		t.Fatalf("xrange live: %v", err)
	}
	// two original + two redriven
	if len(live) != 4 {
		t.Fatalf("live stream len = %d, want 4", len(live))
	}
}
```

## Review

The reclaimer is correct when three invariants hold. First, the decision uses the
delivery count with a `>=` comparison against the threshold, so a message is
parked exactly when it has been delivered at least `threshold` times — off-by-one
here parks a message one attempt early or grants it one extra retry, which
`TestDecide` pins at the boundary. Second, parking is durable: the entry is
`XADD`ed to the DLQ *and* `XACK`ed on the source so it leaves the PEL; skipping
the ack would leave a ghost pending entry that the next cycle re-parks, producing
DLQ duplicates. Third, redrive strips the `dlq_*` triage fields, so a redriven
message is a faithful copy of the original rather than accumulating metadata each
time it round-trips.

The mistakes to avoid: do not park an entry that is not yet idle enough (its owner
may still ack it — the `minIdle` gate exists for exactly this race); do not forget
the `XACK` after `XADD`, or the PEL grows without bound; and do not treat the DLQ
as write-only — the `Redrive` path is what turns it from a data-loss sink into
recoverable parking. Confirm the offline core with `go test -count=1 -race
./...`; to exercise the real path, start a Redis on localhost:6379 and run
`go test -tags integration -run TestReclaimAndRedrive`, which parks two poison
entries and then redrives them back to the live stream.

## Resources

- [Redis Streams](https://redis.io/docs/latest/develop/data-types/streams/) — consumer groups, the Pending Entries List, and the claim/ack lifecycle.
- [XAUTOCLAIM](https://redis.io/docs/latest/commands/xautoclaim/) and [XPENDING](https://redis.io/docs/latest/commands/xpending/) — claiming idle entries and reading per-entry delivery counts.
- [go-redis v9](https://pkg.go.dev/github.com/redis/go-redis/v9) — `XPendingExtArgs`/`XPendingExt`, `XClaimArgs`, `XAddArgs`, `XAck`, `XRangeN`, and `XDel`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-jetstream-dlq-topology.md](02-jetstream-dlq-topology.md) | Next: [../../51-rpc-and-api-design/01-connectrpc-services/00-concepts.md](../../51-rpc-and-api-design/01-connectrpc-services/00-concepts.md)
