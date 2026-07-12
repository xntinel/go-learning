# Exercise 3: Transactional Error Classification and Read-Committed Isolation

The operational hardening layer of an exactly-once pipeline is a classifier that
maps a Kafka transactional error to the correct action: retry the same operation,
retry-as-abort, abort-and-continue, or tear the client down because it has been
fenced. This exercise builds that classifier as a pure function over the real
`kerr` error singletons, plus the read-committed consumer constructor and a note
on why read-uncommitted would surface rolled-back records.

This module is self-contained: its own `go mod init`, its own demo, its own tests.
Nothing here imports another exercise.

## What you'll build

```text
txnerr/                            independent module: example.com/txnerr
  go.mod                           go 1.25; requires github.com/twmb/franz-go
  txnerr.go                        Action, Classify (pure, no broker)
  run.go                           //go:build kafka: NewReadCommittedClient
  cmd/
    demo/
      main.go                      offline demo classifying each transactional error
  txnerr_test.go                   table-driven tests over Classify against kerr singletons
```

- Files: `txnerr.go`, `run.go`, `cmd/demo/main.go`, `txnerr_test.go`.
- Implement: an `Action` enum with a `String`, and `Classify(err) Action` using `errors.Is` against `kerr` singletons; the tagged `NewReadCommittedClient`.
- Test: table cases mapping each transactional error (and a wrapped copy, and an unknown error) to its action, with nil mapping to `ActionOK`.
- Verify: `go test -count=1 -race ./...` (the classifier needs only the franz-go module, no broker; the client constructor needs `-tags kafka`).

### The recovery rules, and why they are what they are

Transactional errors are not interchangeable, and reacting to all of them with a
blind retry loop is a classic production incident. The classifier encodes Kafka's
actual recoverability rules:

- `nil` maps to `ActionOK` — nothing to do.
- `OPERATION_NOT_ATTEMPTED` maps to `ActionRetryAsAbort`. The broker skipped the
  end in a batched RPC, so the transaction is still open and you must issue a
  second `EndTransaction(TryAbort)`.
- `TRANSACTION_ABORTABLE` and `UNKNOWN_PRODUCER_ID` map to `ActionAbortContinue`.
  These are recoverable: abort the current transaction and continue with a fresh
  one.
- `CONCURRENT_TRANSACTIONS` maps to `ActionRetry`. It is transient — a previous
  transaction for this id is still finalizing — so retry the operation after a
  short backoff.
- `PRODUCER_FENCED`, `INVALID_PRODUCER_EPOCH`, and `INVALID_TXN_STATE` map to
  `ActionFatal`. A newer instance with the same `TransactionalID` bumped the epoch
  and fenced this one, or the producer's transactional state machine is broken.
  Retrying can never succeed; the only correct response is to close the client and
  build a new one.
- Any other, unrecognized error maps conservatively to `ActionAbortContinue`:
  abort what is open and move on, rather than risk committing partial work.

The classifier uses `errors.Is` rather than `==` so that a `kerr` singleton
wrapped in context (`fmt.Errorf("end: %w", err)`) is still classified correctly —
franz-go returns the exact singleton, but callers frequently wrap it before it
reaches your handler.

### Why read_committed, restated at the wiring layer

The consuming side of any exactly-once pipeline must set read_committed. The
default, `ReadUncommitted`, returns every record including those from
transactions that later abort, so a downstream stage would process phantom
records that never really happened. read_committed instead returns records only up
to the Last Stable Offset and filters aborted transactions out. The cost is that
read_committed lag is bounded by open transactions — which is the operational
reason for the short `TransactionTimeout` on the producing side.

Set up the module:

```bash
go mod edit -go=1.25
go get github.com/twmb/franz-go@latest
```

Create `txnerr.go`. This file has no build tag and touches no broker:

```go
package txnerr

import (
	"errors"

	"github.com/twmb/franz-go/pkg/kerr"
)

// Action is the recovery step a transactional error calls for.
type Action int

const (
	// ActionOK means no error; proceed.
	ActionOK Action = iota
	// ActionRetry means retry the same operation after a short backoff.
	ActionRetry
	// ActionRetryAsAbort means the end was not attempted; call EndTransaction
	// again with TryAbort.
	ActionRetryAsAbort
	// ActionAbortContinue means abort the current transaction and continue with
	// a fresh one.
	ActionAbortContinue
	// ActionFatal means the producer is fenced or broken; recreate the client.
	ActionFatal
)

func (a Action) String() string {
	switch a {
	case ActionOK:
		return "ok"
	case ActionRetry:
		return "retry"
	case ActionRetryAsAbort:
		return "retry-as-abort"
	case ActionAbortContinue:
		return "abort-and-continue"
	case ActionFatal:
		return "fatal-recreate-client"
	default:
		return "unknown"
	}
}

// Classify maps a Kafka transactional error to the recovery action it requires.
// It matches with errors.Is so a wrapped singleton is still recognized.
func Classify(err error) Action {
	switch {
	case err == nil:
		return ActionOK
	case errors.Is(err, kerr.OperationNotAttempted):
		return ActionRetryAsAbort
	case errors.Is(err, kerr.ConcurrentTransactions):
		return ActionRetry
	case errors.Is(err, kerr.TransactionAbortable),
		errors.Is(err, kerr.UnknownProducerID):
		return ActionAbortContinue
	case errors.Is(err, kerr.ProducerFenced),
		errors.Is(err, kerr.InvalidProducerEpoch),
		errors.Is(err, kerr.InvalidTxnState):
		return ActionFatal
	default:
		return ActionAbortContinue
	}
}
```

### The read-committed client, behind a build tag

The constructor opens a network client, so it is compiled only under
`//go:build kafka`. The single non-obvious line is
`FetchIsolationLevel(kgo.ReadCommitted())`; everything else is a normal group
consumer.

Create `run.go`:

```go
//go:build kafka

package txnerr

import (
	"fmt"

	"github.com/twmb/franz-go/pkg/kgo"
)

// NewReadCommittedClient builds a group consumer that only sees committed
// records. read_committed is opt-in: the default is ReadUncommitted, which would
// surface records from transactions that later abort.
func NewReadCommittedClient(group, topic string, seeds ...string) (*kgo.Client, error) {
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(seeds...),
		kgo.ConsumerGroup(group),
		kgo.ConsumeTopics(topic),
		kgo.FetchIsolationLevel(kgo.ReadCommitted()),
	)
	if err != nil {
		return nil, fmt.Errorf("new client: %w", err)
	}
	return cl, nil
}
```

### The runnable demo

The demo classifies each transactional error and prints its action, with no
broker. Because `Classify` only reads the `kerr` singletons, the output is fully
deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"github.com/twmb/franz-go/pkg/kerr"

	"example.com/txnerr"
)

func main() {
	cases := []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"OPERATION_NOT_ATTEMPTED", kerr.OperationNotAttempted},
		{"CONCURRENT_TRANSACTIONS", kerr.ConcurrentTransactions},
		{"TRANSACTION_ABORTABLE", kerr.TransactionAbortable},
		{"UNKNOWN_PRODUCER_ID", kerr.UnknownProducerID},
		{"PRODUCER_FENCED", kerr.ProducerFenced},
		{"INVALID_PRODUCER_EPOCH", kerr.InvalidProducerEpoch},
		{"INVALID_TXN_STATE", kerr.InvalidTxnState},
		{"unknown", errors.New("some non-kerr error")},
	}
	for _, c := range cases {
		fmt.Printf("%-24s -> %s\n", c.name, txnerr.Classify(c.err))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
nil                      -> ok
OPERATION_NOT_ATTEMPTED  -> retry-as-abort
CONCURRENT_TRANSACTIONS  -> retry
TRANSACTION_ABORTABLE    -> abort-and-continue
UNKNOWN_PRODUCER_ID      -> abort-and-continue
PRODUCER_FENCED          -> fatal-recreate-client
INVALID_PRODUCER_EPOCH   -> fatal-recreate-client
INVALID_TXN_STATE        -> fatal-recreate-client
unknown                  -> abort-and-continue
```

### Tests

The table covers every branch of the classifier against the real `kerr`
singletons, plus a wrapped singleton (to prove `errors.Is` matching) and an
unknown error (to prove the conservative default). This exercise's classifier
needs no broker, so its tests run wherever the franz-go module is available.

Create `txnerr_test.go`:

```go
package txnerr

import (
	"errors"
	"fmt"
	"testing"

	"github.com/twmb/franz-go/pkg/kerr"
)

func TestClassify(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want Action
	}{
		{"nil is ok", nil, ActionOK},
		{"operation not attempted", kerr.OperationNotAttempted, ActionRetryAsAbort},
		{"concurrent transactions", kerr.ConcurrentTransactions, ActionRetry},
		{"transaction abortable", kerr.TransactionAbortable, ActionAbortContinue},
		{"unknown producer id", kerr.UnknownProducerID, ActionAbortContinue},
		{"producer fenced", kerr.ProducerFenced, ActionFatal},
		{"invalid producer epoch", kerr.InvalidProducerEpoch, ActionFatal},
		{"invalid txn state", kerr.InvalidTxnState, ActionFatal},
		{"wrapped fatal is still fatal", fmt.Errorf("end: %w", kerr.ProducerFenced), ActionFatal},
		{"unknown error aborts and continues", errors.New("boom"), ActionAbortContinue},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Classify(tc.err); got != tc.want {
				t.Fatalf("Classify(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func ExampleClassify() {
	// read_committed vs read_uncommitted intent: a read_committed consumer never
	// sees records from an aborted transaction, so the fatal path below is about
	// the producer, not the reader.
	fmt.Println(Classify(kerr.ProducerFenced))
	fmt.Println(Classify(kerr.ConcurrentTransactions))
	// Output:
	// fatal-recreate-client
	// retry
}
```

## Review

The classifier is correct when each error class maps to its documented recovery
action and unknown errors fail safe. The traps are the ones this table guards
against. Do not retry on `PRODUCER_FENCED`, `INVALID_PRODUCER_EPOCH`, or
`INVALID_TXN_STATE`: a fenced producer never recovers, so the action is to
recreate the client. Do not collapse `OPERATION_NOT_ATTEMPTED` into a generic
retry — it is specifically retry-as-abort. Match with `errors.Is`, not `==`, so a
wrapped error is still classified (the wrapped-fatal test proves this). And on the
consuming side, do not forget `FetchIsolationLevel(kgo.ReadCommitted())`; the
default reads uncommitted records that may later abort. Confirm with
`go test -count=1 -race ./...`; the read-committed client constructor is validated
with `-tags kafka` against a real cluster.

## Resources

- [`kerr` package reference](https://pkg.go.dev/github.com/twmb/franz-go/pkg/kerr) — the transactional error singletons and their codes.
- [franz-go transactions guide](https://github.com/twmb/franz-go/blob/master/docs/transactions.md) — recoverable vs fatal transactional errors and the end protocol.
- [Transactions in Apache Kafka](https://www.confluent.io/blog/transactions-apache-kafka/) — fencing, read_committed, and the Last Stable Offset from the design side.

---

Back to [02-eos-read-process-write.md](02-eos-read-process-write.md) | Next: [../03-nats-jetstream-persistence/00-concepts.md](../03-nats-jetstream-persistence/00-concepts.md)
