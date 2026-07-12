# Exercise 3: Consumer-Group Lag Monitor (an on-the-job SRE task)

Lag is the number you page on. When a consumer group falls behind, the signal is
per-partition lag climbing; when a member is stuck or a group is stuck
rebalancing, the signal is flat high lag with no active member. This exercise
builds the tool an SRE actually runs during an incident: a lag reporter that turns
a group's committed offsets and the partitions' end offsets into a sorted,
deterministic report with a state per partition and a threshold flag. The lag
arithmetic — including the awkward edge cases — is pure and unit-tested offline;
the live query against a cluster uses `kadm` behind a build tag.

This module is self-contained. The report logic and its formatter build and test
offline; the `kadm`-backed live query lives behind `//go:build kafka`.

## What you'll build

```text
lagmonitor/                independent module: example.com/lagmonitor
  go.mod                   go 1.26
  lag.go                   TP, LagRow, Report, FormatReport (pure)
  monitor.go               //go:build kafka — LiveReport via kadm.Client.Lag
  lag_test.go              pure tests: lag math, states, sort stability, Example
  monitor_integration_test.go  //go:build kafka — reported lag == produced-committed
  cmd/
    demo/
      main.go              prints a report from synthetic offsets (real output)
```

Files: `lag.go`, `monitor.go`, `lag_test.go`, `monitor_integration_test.go`, `cmd/demo/main.go`.
Implement: `Report` (committed/end/active maps to sorted `LagRow`s with states and a threshold flag), `FormatReport`, and `LiveReport` translating `kadm` lag into the same report.
Test: pure tests over synthetic offsets covering lag math, the no-commit and idle edge cases, threshold flagging, and sort stability; an integration test asserting reported lag equals produced-minus-committed.
Verify: `go test -race ./...` offline; `go test -tags kafka -race ./...` against a broker.

Set up the module:

```bash
mkdir -p go-solutions/50-messaging-and-event-driven/01-kafka-with-franz-go/03-consumer-lag-monitor/cmd/demo
cd go-solutions/50-messaging-and-event-driven/01-kafka-with-franz-go/03-consumer-lag-monitor
go mod edit -go=1.26
go get github.com/twmb/franz-go/pkg/kgo
go get github.com/twmb/franz-go/pkg/kadm
```

### Lag is arithmetic with sharp edges

For one partition, `lag = end - committed`: how many records exist past the last
offset the group committed. The naive version is a subtraction, but the two edge
cases are where hand-rolled monitors go wrong, so the pure `Report` handles them
explicitly.

The first edge case is a partition the group has **never committed** to (a new
partition, or a topic the group just subscribed to). There is no committed
offset, so the lag is the entire log — `end` itself — and the partition's state
is `no-commit`. Representing "no commit" is why `committed` is a
`map[TP]*int64`: a nil pointer means "never committed", distinct from a committed
offset that happens to be zero.

The second edge case is a partition with a committed offset but **no active
member** consuming it — an idle group, or one stuck mid-rebalance. Its lag is real
and worth reporting, but the state is `idle`, not `lagging`, because the fix is
different: `lagging` means "consumers can't keep up, scale them"; `idle` means
"no one is consuming this, investigate the group". The `active` map carries that
distinction. Everything else with a live member is `ok` below the threshold and
`lagging` above it.

The report is sorted by topic then partition so the output is deterministic and
diffable — an alerting pipeline or a human comparing two snapshots needs stable
ordering, not Go's randomized map iteration.

Create `lag.go`:

```go
package lagmonitor

import (
	"fmt"
	"io"
	"sort"
	"strconv"
)

// TP identifies a topic partition.
type TP struct {
	Topic     string
	Partition int32
}

// LagRow is one line of a lag report.
type LagRow struct {
	Topic     string
	Partition int32
	End       int64
	Committed int64
	HasCommit bool
	Lag       int64
	State     string
}

// Report computes a sorted, deterministic lag report. committed maps a partition
// to its committed offset; a missing key or nil pointer means the group never
// committed there. end maps a partition to its log-end offset. active marks
// partitions with a live group member. threshold flags a partition as "lagging"
// when its lag exceeds it. A partition with no commit lags by its full end
// offset and is "no-commit"; a partition with a commit but no active member is
// "idle" regardless of lag.
func Report(committed map[TP]*int64, end map[TP]int64, active map[TP]bool, threshold int64) []LagRow {
	rows := make([]LagRow, 0, len(end))
	for tp, endOff := range end {
		row := LagRow{Topic: tp.Topic, Partition: tp.Partition, End: endOff}
		c, ok := committed[tp]
		switch {
		case !ok || c == nil:
			row.Lag = endOff
			row.State = "no-commit"
		default:
			row.Committed = *c
			row.HasCommit = true
			row.Lag = endOff - *c
			if row.Lag < 0 {
				row.Lag = 0
			}
			switch {
			case !active[tp]:
				row.State = "idle"
			case row.Lag > threshold:
				row.State = "lagging"
			default:
				row.State = "ok"
			}
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Topic != rows[j].Topic {
			return rows[i].Topic < rows[j].Topic
		}
		return rows[i].Partition < rows[j].Partition
	})
	return rows
}

// FormatReport writes rows as an aligned, fixed-width table.
func FormatReport(w io.Writer, rows []LagRow) {
	fmt.Fprintf(w, "%-10s %-5s %-6s %-10s %-5s %s\n", "TOPIC", "PART", "END", "COMMITTED", "LAG", "STATE")
	for _, r := range rows {
		committed := "-"
		if r.HasCommit {
			committed = strconv.FormatInt(r.Committed, 10)
		}
		fmt.Fprintf(w, "%-10s %-5d %-6d %-10s %-5d %s\n", r.Topic, r.Partition, r.End, committed, r.Lag, r.State)
	}
}
```

### The live query (network code, behind the build tag)

`LiveReport` is the thin network shell over the pure core. `kadm.Client.Lag`
describes the group and lists end offsets in one call and returns
`DescribedGroupLags`; internally it uses `CalculateGroupLag`, so you never
re-derive the offset math. We translate its `GroupMemberLag` entries into the same
`committed`/`end`/`active` maps the pure `Report` consumes: `Commit.At` is the
committed offset (`kadm` uses `-1` for no commit), `End.Offset` is the log end,
and a non-nil `Member` means the partition has a live consumer. Reusing `Report`
means the states, thresholds, and sorting are identical whether the offsets came
from a synthetic test or a real cluster.

Create `monitor.go`:

```go
//go:build kafka

package lagmonitor

import (
	"context"
	"fmt"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
)

// LiveReport queries a running cluster for a group's lag and returns the same
// sorted report as Report, built from kadm's computed offsets. It reuses the pure
// arithmetic so live and synthetic reports are identical in shape.
func LiveReport(ctx context.Context, cl *kgo.Client, group string, threshold int64) ([]LagRow, error) {
	admin := kadm.NewClient(cl)
	lags, err := admin.Lag(ctx, group)
	if err != nil {
		return nil, fmt.Errorf("describe lag: %w", err)
	}
	if err := lags.Error(); err != nil {
		return nil, fmt.Errorf("group lag: %w", err)
	}

	end := map[TP]int64{}
	committed := map[TP]*int64{}
	active := map[TP]bool{}
	for topic, parts := range lags[group].Lag {
		for p, ml := range parts {
			tp := TP{Topic: topic, Partition: p}
			end[tp] = ml.End.Offset
			active[tp] = ml.Member != nil
			if ml.Commit.At >= 0 {
				at := ml.Commit.At
				committed[tp] = &at
			}
		}
	}
	return Report(committed, end, active, threshold), nil
}
```

### The runnable demo

The demo is the tool itself, run against synthetic offsets so it produces real,
deterministic output offline. It shows all four states at once: `orders/0` lagging
a little (ok), `orders/1` past the threshold (lagging), `orders/2` never consumed
(no-commit, lag is the whole log), and `payments/0` with a commit but no active
member (idle).

Create `cmd/demo/main.go`:

```go
package main

import (
	"os"

	"example.com/lagmonitor"
)

func commit(v int64) *int64 { return &v }

func main() {
	end := map[lagmonitor.TP]int64{
		{Topic: "orders", Partition: 0}:   1000,
		{Topic: "orders", Partition: 1}:   2000,
		{Topic: "orders", Partition: 2}:   500,
		{Topic: "payments", Partition: 0}: 300,
	}
	committed := map[lagmonitor.TP]*int64{
		{Topic: "orders", Partition: 0}:   commit(950),
		{Topic: "orders", Partition: 1}:   commit(1200),
		{Topic: "payments", Partition: 0}: commit(250),
	}
	active := map[lagmonitor.TP]bool{
		{Topic: "orders", Partition: 0}: true,
		{Topic: "orders", Partition: 1}: true,
		{Topic: "orders", Partition: 2}: true,
	}

	rows := lagmonitor.Report(committed, end, active, 500)
	lagmonitor.FormatReport(os.Stdout, rows)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
TOPIC      PART  END    COMMITTED  LAG   STATE
orders     0     1000   950        50    ok
orders     1     2000   1200       800   lagging
orders     2     500    -          500   no-commit
payments   0     300    250        50    idle
```

### Tests

The pure tests are the substance: synthetic offset maps in, asserted rows out, no
broker. `TestReportStates` drives all four states through the threshold in one
table. `TestReportSorted` feeds partitions out of order and asserts the output is
sorted, pinning the determinism an alerting pipeline depends on. The `Example`
locks the exact formatted output, so a formatting regression fails the gate.

Create `lag_test.go`:

```go
package lagmonitor

import (
	"os"
	"testing"
)

func ptr(v int64) *int64 { return &v }

func TestReportStates(t *testing.T) {
	t.Parallel()
	end := map[TP]int64{
		{"orders", 0}: 1000,
		{"orders", 1}: 2000,
		{"orders", 2}: 500,
		{"orders", 3}: 300,
	}
	committed := map[TP]*int64{
		{"orders", 0}: ptr(950),  // lag 50, active   -> ok
		{"orders", 1}: ptr(1200), // lag 800, active  -> lagging
		{"orders", 3}: ptr(250),  // lag 50, inactive -> idle
		// orders/2 has no commit -> no-commit, lag = end
	}
	active := map[TP]bool{
		{"orders", 0}: true,
		{"orders", 1}: true,
		{"orders", 2}: true,
	}
	rows := Report(committed, end, active, 500)

	want := map[int32]struct {
		lag   int64
		state string
	}{
		0: {50, "ok"},
		1: {800, "lagging"},
		2: {500, "no-commit"},
		3: {50, "idle"},
	}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d", len(rows), len(want))
	}
	for _, r := range rows {
		w := want[r.Partition]
		if r.Lag != w.lag || r.State != w.state {
			t.Fatalf("partition %d = (lag %d, %s), want (lag %d, %s)", r.Partition, r.Lag, r.State, w.lag, w.state)
		}
	}
}

func TestReportSorted(t *testing.T) {
	t.Parallel()
	end := map[TP]int64{
		{"payments", 0}: 10,
		{"orders", 2}:   10,
		{"orders", 0}:   10,
		{"orders", 1}:   10,
	}
	rows := Report(nil, end, nil, 0)
	got := make([]TP, len(rows))
	for i, r := range rows {
		got[i] = TP{r.Topic, r.Partition}
	}
	want := []TP{{"orders", 0}, {"orders", 1}, {"orders", 2}, {"payments", 0}}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d = %+v, want %+v (order: %v)", i, got[i], want[i], got)
		}
	}
}

func Example() {
	commit := func(v int64) *int64 { return &v }
	end := map[TP]int64{
		{"orders", 0}:   1000,
		{"orders", 1}:   2000,
		{"orders", 2}:   500,
		{"payments", 0}: 300,
	}
	committed := map[TP]*int64{
		{"orders", 0}:   commit(950),
		{"orders", 1}:   commit(1200),
		{"payments", 0}: commit(250),
	}
	active := map[TP]bool{
		{"orders", 0}: true,
		{"orders", 1}: true,
		{"orders", 2}: true,
	}
	FormatReport(os.Stdout, Report(committed, end, active, 500))
	// Output:
	// TOPIC      PART  END    COMMITTED  LAG   STATE
	// orders     0     1000   950        50    ok
	// orders     1     2000   1200       800   lagging
	// orders     2     500    -          500   no-commit
	// payments   0     300    250        50    idle
}
```

The integration test produces records, consumes part of them with a group, then
calls `kadm.Client.Lag` and asserts the reported lag equals produced-minus-committed
for at least one partition. It is skipped unless `KAFKA_SEEDS` is set.

Create `monitor_integration_test.go`:

```go
//go:build kafka

package lagmonitor

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
)

func TestLiveReportMatchesArithmetic(t *testing.T) {
	seeds := os.Getenv("KAFKA_SEEDS")
	if seeds == "" {
		t.Skip("set KAFKA_SEEDS (comma-separated) to run the integration test")
	}
	brokers := strings.Split(seeds, ",")
	ctx := t.Context()

	prod, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	if err != nil {
		t.Fatalf("producer: %v", err)
	}
	defer prod.Close()
	admin := kadm.NewClient(prod)

	topic := fmt.Sprintf("lag-test-%d", time.Now().UnixNano())
	group := fmt.Sprintf("lag-grp-%d", time.Now().UnixNano())
	if _, err := admin.CreateTopic(ctx, 1, 1, nil, topic); err != nil {
		t.Fatalf("create topic: %v", err)
	}
	defer admin.DeleteTopics(ctx, topic)

	const produced = 20
	recs := make([]*kgo.Record, 0, produced)
	for i := range produced {
		recs = append(recs, &kgo.Record{Topic: topic, Value: []byte(fmt.Sprintf("v%d", i))})
	}
	if err := prod.ProduceSync(ctx, recs...).FirstErr(); err != nil {
		t.Fatalf("produce: %v", err)
	}

	// Consume and commit exactly half.
	const consumed = 10
	cons, err := kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		kgo.ConsumerGroup(group),
		kgo.ConsumeTopics(topic),
		kgo.DisableAutoCommit(),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
	)
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	got := 0
	for got < consumed {
		fs := cons.PollRecords(pctx, consumed-got)
		if fs.IsClientClosed() || pctx.Err() != nil {
			break
		}
		var batch []*kgo.Record
		fs.EachRecord(func(r *kgo.Record) { batch = append(batch, r) })
		if err := cons.CommitRecords(ctx, batch...); err != nil {
			t.Fatalf("commit: %v", err)
		}
		got += len(batch)
	}
	cons.Close()

	rows, err := LiveReport(ctx, prod, group, 0)
	if err != nil {
		t.Fatalf("live report: %v", err)
	}
	var found bool
	for _, r := range rows {
		if r.Topic == topic && r.Partition == 0 {
			found = true
			if r.Lag != produced-consumed {
				t.Fatalf("lag = %d, want %d", r.Lag, produced-consumed)
			}
		}
	}
	if !found {
		t.Fatalf("no lag row for %s/0", topic)
	}
}
```

## Review

The monitor is correct when lag is a pure function of the committed and end
offsets and the two edge cases are handled explicitly. `TestReportStates` proves
all four states and their thresholds; `TestReportSorted` proves the output order
is stable regardless of map iteration; the `Example` proves the exact rendered
table. Together they mean the tool an SRE stares at during an incident is
deterministic and trustworthy without a broker in the loop.

The mistakes to avoid are the ones that make a monitor lie. Do not conflate "no
commit" with "committed at zero" — that is why `committed` is a pointer map, and
why a never-consumed partition reports its full log as lag rather than zero. Do
not report an idle partition as `lagging`: the remediation differs, so the state
must. And do not re-implement the offset math against a live cluster; `LiveReport`
delegates to `kadm.Client.Lag` / `CalculateGroupLag` and only maps the result into
the pure `Report`, so the arithmetic exists in exactly one place. Run
`go test -race ./...` for the pure report and `go test -tags kafka -race ./...`
against a broker to confirm the live lag matches produced-minus-committed.

## Resources

- [franz-go `kadm` package reference](https://pkg.go.dev/github.com/twmb/franz-go/pkg/kadm) — `Client.Lag`, `CalculateGroupLag`, `GroupMemberLag`, `FetchOffsets`, `ListEndOffsets`.
- [franz-go `kgo` package reference](https://pkg.go.dev/github.com/twmb/franz-go/pkg/kgo) — the client `kadm` wraps.
- [Apache Kafka: monitoring consumer lag](https://kafka.apache.org/documentation/#monitoring) — the operational meaning of lag as a health signal.
- [Confluent: understanding consumer lag](https://developer.confluent.io/courses/kafka-fundamentals/consumers/) — end offset versus committed offset and why lag matters.

---

Back to [02-consumer-group-manual-commit.md](02-consumer-group-manual-commit.md) | Next: [../02-kafka-exactly-once-transactions/00-concepts.md](../02-kafka-exactly-once-transactions/00-concepts.md)
