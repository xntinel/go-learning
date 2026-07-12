# Exercise 1: Atomic Batch Producer with Kafka Transactions

A transactional producer writes a batch of domain events all-or-nothing: begin a
transaction, produce every record, then commit only if all of them succeeded,
otherwise abort. This exercise builds that producer with franz-go, factoring the
commit/abort decision and the `OPERATION_NOT_ATTEMPTED` retry-as-abort protocol
into pure functions you can unit-test without a broker, and putting the client
loop behind a `//go:build kafka` tag.

This module is self-contained: its own `go mod init`, its own demo, its own tests.
Nothing here imports another exercise.

## What you'll build

```text
txnproducer/                       independent module: example.com/txnproducer
  go.mod                           go 1.25; requires github.com/twmb/franz-go
  txnproducer.go                   Event, CommitDecision, NeedsAbortRetry, abortOutcome, recordsFor (no broker)
  run.go                           //go:build kafka: Producer, NewProducer, PublishBatch (the client loop)
  cmd/
    demo/
      main.go                      offline demo of the pure decision functions
  txnproducer_test.go              table-driven tests for the pure decision + protocol functions
```

- Files: `txnproducer.go`, `run.go`, `cmd/demo/main.go`, `txnproducer_test.go`.
- Implement: `CommitDecision(produceErr) kgo.TransactionEndTry`, `NeedsAbortRetry(endErr) bool`, `abortOutcome(cause) error` wrapping a sentinel, and the tagged `PublishBatch` that drives begin/produce/flush/end.
- Test: table cases proving nil produce error commits and any produce error aborts; that `OPERATION_NOT_ATTEMPTED` (and a wrapped copy) demands a follow-up abort while other errors do not; and that an aborted batch's error unwraps to both the sentinel and the cause.
- Verify: `go test -count=1 -race ./...` (needs the franz-go module and, for the broker loop, `-tags kafka` against a real cluster).

### Why the decision is a pure function

The hard part of a transactional producer is not the API calls; it is deciding,
from the aggregated result of N asynchronous produces, whether to commit or abort,
and then handling the one end-transaction error that is a protocol step rather
than a failure. Both of those are pure logic, so we lift them out of the network
code where they can be exhaustively table-tested.

`CommitDecision` collapses the whole batch to a single bit: if any record failed,
abort; otherwise commit. Because `TransactionEndTry` is a named bool
(`TryCommit == true`, `TryAbort == false`), the decision is literally
`kgo.TransactionEndTry(produceErr == nil)`. The aggregated `produceErr` comes from
franz-go's `AbortingFirstErrPromise`: you hand its `Promise()` to every `Produce`
call, and after flushing you read `Err()`, which is the first error any record
saw (nil if all succeeded). The "Aborting" variant also calls
`AbortBufferedRecords` on the first error so the rest of the batch fails fast
instead of lingering.

`NeedsAbortRetry` encodes the `OPERATION_NOT_ATTEMPTED` protocol. When
`EndTransaction` returns that error, the broker did not attempt the end for this
producer (it was skipped inside a batched RPC), so the transaction is still open
and you must call `EndTransaction` again with `TryAbort`. It is neither a success
nor a generic retryable error, so it gets its own predicate, implemented with
`errors.Is` against `kerr.OperationNotAttempted` so a wrapped copy is still
recognized.

`abortOutcome` builds the error a caller sees when a batch aborts. It wraps a
package sentinel *and* the underlying cause using two `%w` verbs, so a caller can
match `errors.Is(err, ErrBatchAborted)` for control flow and still recover the
original produce failure for logging.

Set up the module:

```bash
go mod edit -go=1.25
go get github.com/twmb/franz-go@latest
```

Create `txnproducer.go`. This file has no build tag and touches no broker; it is
the testable core:

```go
package txnproducer

import (
	"errors"
	"fmt"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
)

// ErrBatchAborted is returned (wrapped) when a transactional batch is aborted
// rather than committed.
var ErrBatchAborted = errors.New("transactional batch aborted")

// Event is a domain event destined for one Kafka topic.
type Event struct {
	Key     string
	Payload string
}

// CommitDecision collapses the aggregated produce error of a batch into a
// transaction end decision: no error commits, any error aborts. TransactionEndTry
// is a named bool, so this is the whole rule.
func CommitDecision(produceErr error) kgo.TransactionEndTry {
	return kgo.TransactionEndTry(produceErr == nil)
}

// NeedsAbortRetry reports whether an EndTransaction error requires a follow-up
// EndTransaction(ctx, TryAbort). OPERATION_NOT_ATTEMPTED means the broker did not
// attempt the end, so the transaction is still open and must be aborted.
func NeedsAbortRetry(endErr error) bool {
	return errors.Is(endErr, kerr.OperationNotAttempted)
}

// abortOutcome builds the error a caller sees for an aborted batch, wrapping both
// the sentinel and the underlying cause so either can be matched with errors.Is.
func abortOutcome(cause error) error {
	if cause == nil {
		return ErrBatchAborted
	}
	return fmt.Errorf("%w: %w", ErrBatchAborted, cause)
}

// recordsFor turns domain events into Kafka records addressed to topic.
func recordsFor(topic string, events []Event) []*kgo.Record {
	recs := make([]*kgo.Record, len(events))
	for i, ev := range events {
		r := kgo.StringRecord(ev.Payload)
		r.Topic = topic
		r.Key = []byte(ev.Key)
		recs[i] = r
	}
	return recs
}
```

### The broker loop, behind a build tag

The loop that actually talks to Kafka is compiled only under `//go:build kafka`,
so `go build ./...` and the default test run stay green in CI without a cluster.
The sequence is exact and order-sensitive: `BeginTransaction`, then `Produce`
every record with the shared promise, then `Flush` (because `EndTransaction` does
*not* flush — it requires all records already sent), then decide, then
`EndTransaction`. On a clean commit we are done; on a `TryAbort` decision the end
returns nil and we surface `abortOutcome`; on `OPERATION_NOT_ATTEMPTED` we issue
the mandatory second `EndTransaction(TryAbort)`; anything else is wrapped and
returned.

Create `run.go`:

```go
//go:build kafka

package txnproducer

import (
	"context"
	"fmt"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
)

// Producer writes batches of events to one topic atomically.
type Producer struct {
	cl    *kgo.Client
	topic string
}

// NewProducer builds a transactional producer. txnID must be stable per logical
// instance (stable across restarts, unique across live instances) so fencing
// works. TransactionTimeout is kept short so a stalled producer does not hold the
// Last Stable Offset and stall downstream read_committed consumers.
func NewProducer(topic, txnID string, seeds ...string) (*Producer, error) {
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(seeds...),
		kgo.TransactionalID(txnID),
		kgo.TransactionTimeout(10*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("new client: %w", err)
	}
	return &Producer{cl: cl, topic: topic}, nil
}

// Close releases the underlying client.
func (p *Producer) Close() { p.cl.Close() }

// PublishBatch writes every event in one transaction: all commit or none do.
func (p *Producer) PublishBatch(ctx context.Context, events []Event) error {
	if err := p.cl.BeginTransaction(); err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	e := kgo.AbortingFirstErrPromise(p.cl)
	for _, r := range recordsFor(p.topic, events) {
		p.cl.Produce(ctx, r, e.Promise())
	}

	// EndTransaction does not flush; force all produces to complete first.
	if err := p.cl.Flush(ctx); err != nil {
		_ = p.cl.EndTransaction(ctx, kgo.TryAbort)
		return fmt.Errorf("flush: %w", err)
	}

	commit := CommitDecision(e.Err())
	switch err := p.cl.EndTransaction(ctx, commit); {
	case err == nil:
		if commit == kgo.TryAbort {
			return abortOutcome(e.Err())
		}
		return nil
	case NeedsAbortRetry(err):
		if aerr := p.cl.EndTransaction(ctx, kgo.TryAbort); aerr != nil {
			return fmt.Errorf("abort after operation-not-attempted: %w", aerr)
		}
		return abortOutcome(e.Err())
	default:
		return fmt.Errorf("end transaction: %w", err)
	}
}
```

### The runnable demo

The demo needs no broker: it exercises the pure decision functions with known
inputs so the output is deterministic. A clean batch commits; a batch whose
aggregated error is non-nil aborts; `OPERATION_NOT_ATTEMPTED` demands an abort
retry while an ordinary end error does not.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"

	"example.com/txnproducer"
)

func label(t kgo.TransactionEndTry) string {
	if t == kgo.TryCommit {
		return "commit"
	}
	return "abort"
}

func main() {
	produceFailed := errors.New("produce: partition 3 has no leader")

	fmt.Printf("clean batch  -> %s\n", label(txnproducer.CommitDecision(nil)))
	fmt.Printf("failed batch -> %s\n", label(txnproducer.CommitDecision(produceFailed)))
	fmt.Printf("operation-not-attempted retry-as-abort -> %v\n",
		txnproducer.NeedsAbortRetry(kerr.OperationNotAttempted))
	fmt.Printf("generic end error       retry-as-abort -> %v\n",
		txnproducer.NeedsAbortRetry(errors.New("some other end error")))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
clean batch  -> commit
failed batch -> abort
operation-not-attempted retry-as-abort -> true
generic end error       retry-as-abort -> false
```

### Tests

The tests pin the two pieces of logic that decide correctness. `TestCommitDecision`
proves the collapse rule in both directions. `TestNeedsAbortRetry` proves the
protocol predicate against the real `kerr.OperationNotAttempted` singleton, a
wrapped copy of it, a different `kerr` error, and nil — the wrapped case is what
justifies using `errors.Is` rather than `==`. `TestAbortOutcome` proves the abort
error unwraps to both the sentinel and the cause, and degrades to the bare
sentinel when there is no cause.

Create `txnproducer_test.go`:

```go
package txnproducer

import (
	"errors"
	"fmt"
	"testing"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
)

func TestCommitDecision(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		produceErr error
		want       kgo.TransactionEndTry
	}{
		{"clean batch commits", nil, kgo.TryCommit},
		{"failed batch aborts", errors.New("produce failed"), kgo.TryAbort},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := CommitDecision(tc.produceErr); got != tc.want {
				t.Fatalf("CommitDecision(%v) = %v, want %v", tc.produceErr, got, tc.want)
			}
		})
	}
}

func TestNeedsAbortRetry(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		endErr error
		want   bool
	}{
		{"operation not attempted", kerr.OperationNotAttempted, true},
		{"wrapped operation not attempted", fmt.Errorf("end: %w", kerr.OperationNotAttempted), true},
		{"different kerr error", kerr.ProducerFenced, false},
		{"generic error", errors.New("boom"), false},
		{"nil", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := NeedsAbortRetry(tc.endErr); got != tc.want {
				t.Fatalf("NeedsAbortRetry(%v) = %v, want %v", tc.endErr, got, tc.want)
			}
		})
	}
}

func TestAbortOutcome(t *testing.T) {
	t.Parallel()
	cause := errors.New("produce: broker down")
	err := abortOutcome(cause)
	if !errors.Is(err, ErrBatchAborted) {
		t.Fatalf("abortOutcome(cause) does not wrap ErrBatchAborted: %v", err)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("abortOutcome(cause) does not wrap the cause: %v", err)
	}
	if got := abortOutcome(nil); !errors.Is(got, ErrBatchAborted) || errors.Unwrap(got) != nil {
		t.Fatalf("abortOutcome(nil) = %v, want bare ErrBatchAborted", got)
	}
}

func ExampleCommitDecision() {
	fmt.Println(CommitDecision(nil) == kgo.TryCommit)
	fmt.Println(CommitDecision(errors.New("x")) == kgo.TryAbort)
	// Output:
	// true
	// true
}
```

## Review

The producer is correct when the commit/abort decision is a pure function of the
aggregated produce error and the `OPERATION_NOT_ATTEMPTED` protocol is honored.
The classic mistakes are all encoded as tests here. Do not treat
`TransactionEndTry` as an int: it is a bool, so the decision is
`kgo.TransactionEndTry(produceErr == nil)`, not a comparison against some enum
value. Do not swallow `OPERATION_NOT_ATTEMPTED` as success or push it through a
generic retry — the transaction is still open and you must issue a second
`EndTransaction(TryAbort)`; the test with a wrapped copy is why the predicate uses
`errors.Is`. Do not call `EndTransaction` before `Flush`: it does not flush, and
committing before your produces have landed loses records. Confirm correctness by
running `go test -count=1 -race ./...`; the broker loop in `run.go` is validated
separately with `-tags kafka` against a real cluster, where an aborted batch must
be invisible to a `read_committed` consumer.

## Resources

- [franz-go transactions guide](https://github.com/twmb/franz-go/blob/master/docs/transactions.md) — the producer transaction lifecycle and the end-transaction error protocol.
- [franz-go transactions example](https://github.com/twmb/franz-go/blob/master/examples/transactions/main.go) — the produce-mode loop this exercise mirrors.
- [`kgo` package reference](https://pkg.go.dev/github.com/twmb/franz-go/pkg/kgo) — `BeginTransaction`, `EndTransaction`, `AbortingFirstErrPromise`, `TransactionEndTry`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-eos-read-process-write.md](02-eos-read-process-write.md)
