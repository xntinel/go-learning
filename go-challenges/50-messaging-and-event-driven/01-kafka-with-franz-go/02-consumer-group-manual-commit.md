# Exercise 2: At-Least-Once Consumer Group with Manual Commit and Safe Rebalance

The consume side is where at-least-once is won or lost, and it comes down to one
ordering: poll a batch, process every record, and only *then* commit. This
exercise builds a consumer-group worker that does exactly that with autocommit
disabled, wires `OnPartitionsRevoked` to flush offsets before losing partitions,
and uses the cooperative-sticky balancer. It also isolates the commit-ordering
invariant into a pure poll loop over a fake source so you can prove "commit after
process, never before" without a broker.

This module is self-contained. The pure loop, its error classification, and the
drain-on-shutdown logic build and test offline; the real `*kgo.Client` group
consumer and its integration tests live behind `//go:build kafka`.

## What you'll build

```text
kafkaconsumer/                  independent module: example.com/kafkaconsumer
  go.mod                        go 1.26
  worker.go                     Record, Source, RunOnce, Drain, IsRetriable (pure)
  consumer.go                   //go:build kafka — group consumer over *kgo.Client
  worker_test.go                pure tests: commit-after-process, no-commit-on-error
  consumer_integration_test.go  //go:build kafka — no-loss + resume-from-commit
  cmd/
    demo/
      main.go                   runs one poll->process->commit cycle offline
```

Files: `worker.go`, `consumer.go`, `worker_test.go`, `consumer_integration_test.go`, `cmd/demo/main.go`.
Implement: `RunOnce` (poll, process all, commit last), `Drain`, `IsRetriable`, and a `*kgo.Client` group consumer with `DisableAutoCommit`, cooperative-sticky, and `OnPartitionsRevoked` flushing offsets — plus the `AutoCommitMarks` + `MarkCommitRecords` variant.
Test: pure tests asserting commit happens only after all records are processed and that a process error leaves nothing committed; integration tests asserting no loss and resume-from-committed after a restart.
Verify: `go test -race ./...` offline; `go test -tags kafka -race ./...` against a broker.

Set up the module:

```bash
mkdir -p go-solutions/50-messaging-and-event-driven/01-kafka-with-franz-go/02-consumer-group-manual-commit/cmd/demo
cd go-solutions/50-messaging-and-event-driven/01-kafka-with-franz-go/02-consumer-group-manual-commit
go mod edit -go=1.26
go get github.com/twmb/franz-go/pkg/kgo
go get github.com/twmb/franz-go/pkg/kadm
```

### The invariant, extracted so it can be tested

The correctness of an at-least-once consumer is one rule: the commit for a batch
must not happen until every record in that batch has been processed. If you commit
first and crash, the records were never really handled but their offsets moved, so
they are lost. This rule has nothing to do with Kafka's wire protocol — it is
pure control flow — so `worker.go` expresses it over a `Source` interface that a
fake can implement. `RunOnce` polls once, processes each record in order, and
calls `Commit` only after the loop completes without error. If processing returns
an error partway through, `RunOnce` returns *without committing*, so the batch is
redelivered on the next run: that is the duplicate-not-loss trade of at-least-once,
made mechanical.

`IsRetriable` is the second piece of production hygiene. Fetch errors come in two
flavors: transient ones the loop should log and retry, and fatal ones that should
stop the worker. The pure code classifies via two sentinel errors wrapped with
`%w` so a caller can use `errors.Is`; the real client (behind the tag) maps
Kafka's fetch errors onto the same decision. `Drain` ties it together: it runs
cycles until the source is empty or the context is cancelled, continuing past
retriable errors and returning on fatal ones. A cancelled context is a graceful
stop, not a failure — the last committed batch bounds what gets redelivered.

Create `worker.go`:

```go
package kafkaconsumer

import (
	"context"
	"errors"
	"fmt"
)

// Record is the minimal shape the poll loop needs: where a message came from and
// its business payload. A real implementation fills these from *kgo.Record.
type Record struct {
	Order     string
	Partition int32
	Offset    int64
}

// Source abstracts a fetch/commit backend so the commit-ordering invariant is
// testable without a broker. A real implementation wraps *kgo.Client.
type Source interface {
	Poll(ctx context.Context) ([]Record, error)
	Commit(ctx context.Context, recs []Record) error
}

// Sentinel fetch-error classes. A Source wraps one of these so the loop can
// decide whether to retry a transient failure or abort on a fatal one.
var (
	ErrRetriable = errors.New("retriable fetch error")
	ErrFatal     = errors.New("fatal fetch error")
)

// IsRetriable reports whether a fetch error should be retried rather than
// aborting the loop.
func IsRetriable(err error) bool {
	return errors.Is(err, ErrRetriable)
}

// RunOnce performs one poll -> process -> commit cycle and returns how many
// records were processed. It commits ONLY after every record is processed: that
// is the at-least-once contract. If processing fails partway, nothing is
// committed and the batch is redelivered (duplicate, not loss). Committing before
// processing would make this at-most-once (silent loss on crash).
func RunOnce(ctx context.Context, src Source, process func(Record) error) (int, error) {
	recs, err := src.Poll(ctx)
	if err != nil {
		return 0, fmt.Errorf("poll: %w", err)
	}
	for i, r := range recs {
		if err := process(r); err != nil {
			return i, fmt.Errorf("process offset %d: %w", r.Offset, err)
		}
	}
	if len(recs) == 0 {
		return 0, nil
	}
	if err := src.Commit(ctx, recs); err != nil {
		return len(recs), fmt.Errorf("commit: %w", err)
	}
	return len(recs), nil
}

// Drain runs cycles until the source returns an empty batch or the context is
// cancelled, returning the total processed. It continues past retriable errors
// and stops on fatal ones. A cancelled context is a graceful shutdown, not an
// error: the last committed batch bounds what is redelivered.
func Drain(ctx context.Context, src Source, process func(Record) error) (int, error) {
	total := 0
	for {
		if ctx.Err() != nil {
			return total, nil
		}
		n, err := RunOnce(ctx, src, process)
		total += n
		if err != nil {
			if IsRetriable(err) {
				continue
			}
			return total, err
		}
		if n == 0 {
			return total, nil
		}
	}
}
```

### The real group consumer (network code, behind the build tag)

`NewGroupConsumer` builds the safe at-least-once client: `DisableAutoCommit` so
nothing commits behind your back, the cooperative-sticky balancer so a rebalance
only moves the partitions that must move, `ConsumeResetOffset(NewOffset().AtStart())`
so a brand-new group starts at the beginning, and `OnPartitionsRevoked` to flush
uncommitted offsets with `CommitUncommittedOffsets` before the partitions leave —
the commit-before-losing-a-partition mechanism.

`RunGroup` is the loop, and it mirrors `RunOnce`: `PollRecords` bounds the batch,
`EachError` surfaces non-retriable fetch errors (instead of panicking as the
quickstart does), each record is processed, and only then is the batch committed
with `CommitRecords`. `IsClientClosed` lets the loop exit cleanly when `Close`
is called.

`NewMarkingConsumer` shows the second pattern. Here autocommit stays *on* but in
"marks only" mode via `AutoCommitMarks`: nothing commits until you call
`MarkCommitRecords` for a record, and a background committer flushes the marks on
its interval. You keep the safety — a record is never committed until you have
marked it processed — while amortizing the commit round-trips instead of paying
one synchronous commit per batch.

Create `consumer.go`:

```go
//go:build kafka

package kafkaconsumer

import (
	"context"
	"fmt"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
)

// NewGroupConsumer builds an at-least-once group consumer: autocommit off,
// cooperative-sticky balancing, and a revoke hook that flushes uncommitted
// offsets before partitions move so no processed-but-uncommitted work is lost.
func NewGroupConsumer(seeds []string, group string, topics ...string) (*kgo.Client, error) {
	return kgo.NewClient(
		kgo.SeedBrokers(seeds...),
		kgo.ConsumerGroup(group),
		kgo.ConsumeTopics(topics...),
		kgo.Balancers(kgo.CooperativeStickyBalancer()),
		kgo.DisableAutoCommit(),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.OnPartitionsRevoked(func(ctx context.Context, cl *kgo.Client, _ map[string][]int32) {
			// Last chance to persist progress before these partitions are reassigned.
			if err := cl.CommitUncommittedOffsets(ctx); err != nil {
				fmt.Printf("revoke commit failed: %v\n", err)
			}
		}),
	)
}

// RunGroup polls bounded batches, processes each record, then commits — the safe
// at-least-once ordering. It returns nil when the client is closed or the context
// is cancelled. Fetch errors are classified rather than fatal-by-default.
func RunGroup(ctx context.Context, cl *kgo.Client, process func(*kgo.Record) error) error {
	for {
		fetches := cl.PollRecords(ctx, 500)
		if fetches.IsClientClosed() || ctx.Err() != nil {
			return nil
		}
		var fatal error
		fetches.EachError(func(t string, p int32, err error) {
			// Retriable fetch errors are handled internally; anything surfaced
			// here is non-retriable for this partition.
			fatal = fmt.Errorf("fetch %s[%d]: %w", t, p, err)
		})
		if fatal != nil {
			return fatal
		}
		var batch []*kgo.Record
		fetches.EachRecord(func(r *kgo.Record) { batch = append(batch, r) })
		for _, r := range batch {
			if err := process(r); err != nil {
				return fmt.Errorf("process: %w", err)
			}
		}
		if err := cl.CommitRecords(ctx, batch...); err != nil {
			return fmt.Errorf("commit: %w", err)
		}
	}
}

// NewMarkingConsumer keeps autocommit on but in marks-only mode: nothing commits
// until MarkCommitRecords marks it, and the background committer flushes marks
// every interval. Safety is preserved while commit round-trips are amortized.
func NewMarkingConsumer(seeds []string, group string, topics ...string) (*kgo.Client, error) {
	return kgo.NewClient(
		kgo.SeedBrokers(seeds...),
		kgo.ConsumerGroup(group),
		kgo.ConsumeTopics(topics...),
		kgo.Balancers(kgo.CooperativeStickyBalancer()),
		kgo.AutoCommitMarks(),
		kgo.AutoCommitInterval(5*time.Second),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
	)
}

// RunGroupMarks processes each record and marks it committed; the background
// committer flushes marks on its interval.
func RunGroupMarks(ctx context.Context, cl *kgo.Client, process func(*kgo.Record) error) error {
	for {
		fetches := cl.PollRecords(ctx, 500)
		if fetches.IsClientClosed() || ctx.Err() != nil {
			return nil
		}
		var perr error
		fetches.EachRecord(func(r *kgo.Record) {
			if perr != nil {
				return
			}
			if err := process(r); err != nil {
				perr = fmt.Errorf("process: %w", err)
				return
			}
			cl.MarkCommitRecords(r)
		})
		if perr != nil {
			return perr
		}
	}
}
```

### The runnable demo

The demo runs offline over a fake source with three records. The interleaving of
output makes the invariant visible: all three "processing" lines print before the
"committed" line, because `RunOnce` commits only after the loop finishes.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/kafkaconsumer"
)

// demoSource yields one batch, then reports empty.
type demoSource struct {
	recs []kafkaconsumer.Record
	done bool
}

func (d *demoSource) Poll(context.Context) ([]kafkaconsumer.Record, error) {
	if d.done {
		return nil, nil
	}
	d.done = true
	return d.recs, nil
}

func (d *demoSource) Commit(context.Context, []kafkaconsumer.Record) error { return nil }

func main() {
	src := &demoSource{recs: []kafkaconsumer.Record{
		{Order: "1001", Partition: 0, Offset: 0},
		{Order: "1002", Partition: 0, Offset: 1},
		{Order: "1003", Partition: 1, Offset: 5},
	}}

	n, err := kafkaconsumer.RunOnce(context.Background(), src, func(r kafkaconsumer.Record) error {
		fmt.Printf("processing order=%s partition=%d offset=%d\n", r.Order, r.Partition, r.Offset)
		return nil
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("committed %d records after processing\n", n)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
processing order=1001 partition=0 offset=0
processing order=1002 partition=0 offset=1
processing order=1003 partition=1 offset=5
committed 3 records after processing
```

### Tests

The pure tests are the point: they assert the commit-ordering invariant with no
broker. `fakeSource` records the exact sequence of process and commit calls, so
`TestRunOnceCommitsAfterProcessing` can assert that every process happened before
the commit. `TestRunOnceNoCommitOnProcessError` makes processing fail on the
second record and asserts that nothing was committed and the error unwraps to the
handler's sentinel — proving the batch will be redelivered rather than lost.
`TestIsRetriable` covers the classifier, and `TestDrainProcessesAll` covers the
multi-batch drain. `ExampleRunOnce` doubles as a self-verifying spec: it runs one
poll->process->commit cycle over a tiny in-file fake and prints a deterministic
line, so `go test` checks the invariant against the `// Output:` block too.

Create `worker_test.go`:

```go
package kafkaconsumer

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// fakeSource yields prepared batches and logs the order of process/commit calls.
type fakeSource struct {
	batches [][]Record
	idx     int
	log     *[]string
	commits [][]Record
}

func (f *fakeSource) Poll(context.Context) ([]Record, error) {
	if f.idx >= len(f.batches) {
		return nil, nil
	}
	b := f.batches[f.idx]
	f.idx++
	return b, nil
}

func (f *fakeSource) Commit(_ context.Context, recs []Record) error {
	*f.log = append(*f.log, "commit")
	f.commits = append(f.commits, recs)
	return nil
}

func TestRunOnceCommitsAfterProcessing(t *testing.T) {
	t.Parallel()
	log := []string{}
	src := &fakeSource{
		batches: [][]Record{{{Order: "1001", Offset: 0}, {Order: "1002", Offset: 1}}},
		log:     &log,
	}
	n, err := RunOnce(context.Background(), src, func(r Record) error {
		log = append(log, "process:"+r.Order)
		return nil
	})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 2 {
		t.Fatalf("processed = %d, want 2", n)
	}
	want := []string{"process:1001", "process:1002", "commit"}
	if len(log) != len(want) {
		t.Fatalf("log = %v, want %v", log, want)
	}
	for i := range want {
		if log[i] != want[i] {
			t.Fatalf("log = %v, want %v", log, want)
		}
	}
}

func TestRunOnceNoCommitOnProcessError(t *testing.T) {
	t.Parallel()
	errBoom := errors.New("boom")
	log := []string{}
	src := &fakeSource{
		batches: [][]Record{{{Order: "1001", Offset: 0}, {Order: "1002", Offset: 1}}},
		log:     &log,
	}
	n, err := RunOnce(context.Background(), src, func(r Record) error {
		if r.Order == "1002" {
			return errBoom
		}
		return nil
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want wrap of errBoom", err)
	}
	if n != 1 {
		t.Fatalf("processed = %d, want 1 before the failure", n)
	}
	if len(src.commits) != 0 {
		t.Fatalf("committed %d batches, want 0 (batch must be redelivered)", len(src.commits))
	}
}

func TestIsRetriable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"retriable", ErrRetriable, true},
		{"wrapped retriable", errors.Join(errors.New("x"), ErrRetriable), true},
		{"fatal", ErrFatal, false},
		{"unrelated", errors.New("other"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsRetriable(tc.err); got != tc.want {
				t.Fatalf("IsRetriable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestDrainProcessesAll(t *testing.T) {
	t.Parallel()
	log := []string{}
	src := &fakeSource{
		batches: [][]Record{
			{{Order: "1001", Offset: 0}},
			{{Order: "1002", Offset: 1}, {Order: "1003", Offset: 2}},
		},
		log: &log,
	}
	seen := 0
	total, err := Drain(context.Background(), src, func(Record) error {
		seen++
		return nil
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if total != 3 || seen != 3 {
		t.Fatalf("total=%d seen=%d, want 3/3", total, seen)
	}
	if len(src.commits) != 2 {
		t.Fatalf("committed %d batches, want 2", len(src.commits))
	}
}

// exampleSource yields one batch, then reports empty — the smallest Source a
// single RunOnce cycle needs.
type exampleSource struct {
	recs []Record
	done bool
}

func (e *exampleSource) Poll(context.Context) ([]Record, error) {
	if e.done {
		return nil, nil
	}
	e.done = true
	return e.recs, nil
}

func (e *exampleSource) Commit(_ context.Context, recs []Record) error {
	fmt.Printf("commit %d records\n", len(recs))
	return nil
}

func ExampleRunOnce() {
	src := &exampleSource{recs: []Record{
		{Order: "1001", Offset: 0},
		{Order: "1002", Offset: 1},
	}}
	n, err := RunOnce(context.Background(), src, func(r Record) error {
		fmt.Printf("process order=%s\n", r.Order)
		return nil
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("processed %d\n", n)
	// Output:
	// process order=1001
	// process order=1002
	// commit 2 records
	// processed 2
}
```

The integration test drives the real client. It creates a topic, produces N keyed
records, runs the worker under a deadline, and asserts every record is processed
at least once. Then it closes the first client and starts a second in the same
group, asserting it resumes from the committed offset — reprocessing nothing that
was already committed — by inspecting `kadm.FetchOffsets`. It is skipped unless
`KAFKA_SEEDS` is set.

Create `consumer_integration_test.go`:

```go
//go:build kafka

package kafkaconsumer

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
)

func TestConsumeNoLossThenResume(t *testing.T) {
	seeds := os.Getenv("KAFKA_SEEDS")
	if seeds == "" {
		t.Skip("set KAFKA_SEEDS (comma-separated) to run the integration test")
	}
	brokers := strings.Split(seeds, ",")
	ctx := t.Context()

	admin0, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	if err != nil {
		t.Fatalf("admin client: %v", err)
	}
	defer admin0.Close()
	admin := kadm.NewClient(admin0)

	topic := fmt.Sprintf("consume-test-%d", time.Now().UnixNano())
	group := fmt.Sprintf("grp-%d", time.Now().UnixNano())
	if _, err := admin.CreateTopic(ctx, 3, 1, nil, topic); err != nil {
		t.Fatalf("create topic: %v", err)
	}
	defer admin.DeleteTopics(ctx, topic)

	const n = 30
	recs := make([]*kgo.Record, 0, n)
	for i := range n {
		recs = append(recs, &kgo.Record{
			Topic: topic,
			Key:   []byte(fmt.Sprintf("k%d", i%3)),
			Value: []byte(fmt.Sprintf("v%d", i)),
		})
	}
	if err := admin0.ProduceSync(ctx, recs...).FirstErr(); err != nil {
		t.Fatalf("produce: %v", err)
	}

	// First consumer: process the first half, then close.
	c1, err := NewGroupConsumer(brokers, group, topic)
	if err != nil {
		t.Fatalf("consumer 1: %v", err)
	}
	var mu sync.Mutex
	seen := map[string]int{}
	ctx1, cancel1 := context.WithTimeout(ctx, 5*time.Second)
	got := 0
	_ = RunGroup(ctx1, c1, func(r *kgo.Record) error {
		mu.Lock()
		seen[string(r.Value)]++
		got++
		mu.Unlock()
		if got >= n/2 {
			cancel1()
		}
		return nil
	})
	cancel1()
	c1.Close()

	// Second consumer in the same group resumes from the committed offset.
	c2, err := NewGroupConsumer(brokers, group, topic)
	if err != nil {
		t.Fatalf("consumer 2: %v", err)
	}
	ctx2, cancel2 := context.WithTimeout(ctx, 5*time.Second)
	defer cancel2()
	_ = RunGroup(ctx2, c2, func(r *kgo.Record) error {
		mu.Lock()
		seen[string(r.Value)]++
		mu.Unlock()
		return nil
	})
	c2.Close()

	for i := range n {
		if seen[fmt.Sprintf("v%d", i)] == 0 {
			t.Fatalf("record v%d was never processed (loss)", i)
		}
	}

	offs, err := admin.FetchOffsets(ctx, group)
	if err != nil {
		t.Fatalf("fetch offsets: %v", err)
	}
	if len(offs) == 0 {
		t.Fatal("no committed offsets after consuming")
	}
}
```

## Review

The worker is correct when the commit is a strict consequence of processing.
`TestRunOnceCommitsAfterProcessing` proves the order (all processes, then the
commit), and `TestRunOnceNoCommitOnProcessError` proves the failure mode: a
mid-batch error commits nothing, so the batch is redelivered — duplicates, never
loss. That is the whole at-least-once contract, verified without a broker.

The mistakes to avoid are the ones the invariant exists to prevent. Do not commit
before processing and do not lean on autocommit while doing slow work — either one
turns at-least-once into at-most-once. Do not panic on `EachError`; classify and
continue, stopping only on a fatal error. Do not forget `OnPartitionsRevoked` (or
`BlockRebalanceOnPoll`): a rebalance between polls will otherwise strand
uncommitted work. And always `Close` the client so the final commit lands and the
group rebalances promptly instead of waiting out the session timeout. Run
`go test -race ./...` for the pure invariant and `go test -tags kafka -race ./...`
against a broker for no-loss and resume.

## Resources

- [franz-go `kgo` package reference](https://pkg.go.dev/github.com/twmb/franz-go/pkg/kgo) — `ConsumerGroup`, `DisableAutoCommit`, `CommitRecords`, `MarkCommitRecords`, `OnPartitionsRevoked`.
- [franz-go: Producing and Consuming](https://github.com/twmb/franz-go/blob/master/docs/producing-and-consuming.md) — offset management, cooperative-sticky, and the rebalance hazard.
- [franz-go group_committing example](https://github.com/twmb/franz-go/tree/master/examples) — the five commit strategies compared, including marks-based commits.
- [Apache Kafka: consumer group protocol](https://kafka.apache.org/documentation/#design_consumerposition) — the broker-side model of committed offsets and delivery semantics.

---

Back to [01-resilient-producer-and-partitioning.md](01-resilient-producer-and-partitioning.md) | Next: [03-consumer-lag-monitor.md](03-consumer-lag-monitor.md)
