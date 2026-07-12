# Exercise 2: Exactly-Once Read-Process-Write with GroupTransactSession

The canonical exactly-once pipeline consumes from an input topic in a consumer
group, transforms each record, produces derived records to an output topic, and
commits the input offsets *inside the same transaction*. This exercise builds that
loop with franz-go's `GroupTransactSession`, which turns a pending commit into an
abort when a rebalance revokes partitions mid-transaction — the safety rail that
keeps a zombie from committing work another member will redo.

This module is self-contained: its own `go mod init`, its own demo, its own tests.
Nothing here imports another exercise.

## What you'll build

```text
eospipe/                           independent module: example.com/eospipe
  go.mod                           go 1.25; requires github.com/twmb/franz-go
  eospipe.go                       Transform, EndTry, DescribeEnd, abortErr (no broker)
  run.go                           //go:build kafka: Pipeline, NewPipeline, ProcessOnce (the session loop)
  cmd/
    demo/
      main.go                      offline demo of the pure transform + end helpers
  eospipe_test.go                  table-driven tests for Transform, EndTry, DescribeEnd, abortErr
```

- Files: `eospipe.go`, `run.go`, `cmd/demo/main.go`, `eospipe_test.go`.
- Implement: `Transform([]byte) []byte` (a tombstone yields no output), `EndTry(firstErr) kgo.TransactionEndTry`, `DescribeEnd(committed, err)` classifying the three end outcomes, `abortErr(cause)` wrapping a sentinel, and the tagged `ProcessOnce` that runs one consume-transform-produce cycle.
- Test: `Transform` over nil, empty, and real values; `EndTry` over nil and non-nil; `DescribeEnd` over committed, rebalance-abort (committed=false, err=nil), and error; that the abort error unwraps to the sentinel and cause.
- Verify: `go test -count=1 -race ./...` (needs franz-go; the session loop needs `-tags kafka` and a real cluster).

### Why offsets ride inside the transaction

The whole point of exactly-once-in-stream is that "I produced the outputs" and "I
consumed these inputs" become a single atomic fact. `GroupTransactSession` makes
that literal: its `End` commits the group's consumer offsets as part of committing
the transaction. If you instead produced in a transaction and committed offsets
with a separate call, a crash between the two would reprocess the inputs and
duplicate the outputs — the exact gap transactions exist to close.

The session also guards rebalances. If a partition you were processing is revoked
mid-transaction, another member now owns that input and will reprocess it; a naive
producer that committed anyway would double-process it. `GroupTransactSession`
hooks revocation and forces `End` to *abort* in that case. The visible signature of
this is that `End` returns `(committed=false, err=nil)`: nothing failed, the
transaction was safely aborted because of a rebalance, and the batch will be
redone by whoever owns the partition now. That is why `DescribeEnd` treats a
non-committed, error-free end as an expected retry rather than a fault.

`Transform` is the business logic, kept pure so it is trivially testable. Here it
filters and normalizes: a tombstone (nil or empty value) produces no output
record, and any other value is tagged and upper-cased into a derived event. The
`nil` return is the signal to skip producing for that input.

`EndTry` is the one-liner that derives the commit/abort argument from the batch
result — `kgo.TransactionEndTry(firstErr == nil)` — reused so the loop reads
cleanly.

Set up the module:

```bash
go mod edit -go=1.25
go get github.com/twmb/franz-go@latest
```

Create `eospipe.go`:

```go
package eospipe

import (
	"errors"
	"fmt"
	"strings"

	"github.com/twmb/franz-go/pkg/kgo"
)

// ErrProcessAborted is returned (wrapped) when a read-process-write batch is
// aborted instead of committed, including the benign rebalance-abort case.
var ErrProcessAborted = errors.New("read-process-write batch aborted")

// Transform derives an output value from an input record value. A nil or empty
// input (a tombstone) yields nil, meaning no derived record is produced; any
// other value is tagged and upper-cased.
func Transform(in []byte) []byte {
	if len(in) == 0 {
		return nil
	}
	return []byte("evt:" + strings.ToUpper(string(in)))
}

// EndTry derives the transaction end decision from the batch's first produce
// error: no error commits, any error aborts.
func EndTry(firstErr error) kgo.TransactionEndTry {
	return kgo.TransactionEndTry(firstErr == nil)
}

// DescribeEnd classifies the three possible outcomes of GroupTransactSession.End.
// End retries internally, so a non-nil error is a real failure; committed==false
// with a nil error is the benign rebalance-abort (the input will be reprocessed
// by the new partition owner).
func DescribeEnd(committed bool, err error) string {
	switch {
	case err != nil:
		return "error"
	case committed:
		return "committed"
	default:
		return "aborted-by-rebalance"
	}
}

// abortErr builds the error a caller sees for an aborted batch, wrapping the
// sentinel and (when present) the cause so either can be matched with errors.Is.
func abortErr(cause error) error {
	if cause == nil {
		return ErrProcessAborted
	}
	return fmt.Errorf("%w: %w", ErrProcessAborted, cause)
}
```

### The session loop, behind a build tag

The loop is compiled only under `//go:build kafka`. `NewPipeline` wires the
critical options: `TransactionalID` for fencing, `ConsumerGroup` and
`ConsumeTopics` for input, `DefaultProduceTopic` so produced records need no
explicit topic, and — the one everyone forgets —
`FetchIsolationLevel(kgo.ReadCommitted())`, because the default is
`ReadUncommitted` and would surface records that later abort.

`ProcessOnce` runs one cycle: poll, check fetch errors, `Begin`, produce a derived
record for each non-tombstone input, then `End`. Note we do *not* call `Flush`
here: unlike the raw client, `GroupTransactSession.End` flushes (or aborts) for
you. `End` returns `(committed, err)`; we map that through the same three outcomes
`DescribeEnd` documents — a real error is wrapped and returned, a rebalance-abort
returns the sentinel so the caller simply retries, and a commit returns nil.

Create `run.go`:

```go
//go:build kafka

package eospipe

import (
	"context"
	"fmt"

	"github.com/twmb/franz-go/pkg/kgo"
)

// Pipeline runs an exactly-once consume-transform-produce loop.
type Pipeline struct {
	sess *kgo.GroupTransactSession
}

// NewPipeline builds the transactional session. read_committed is set explicitly
// because the default isolation level would read records from transactions that
// later abort.
func NewPipeline(group, inTopic, outTopic, txnID string, seeds ...string) (*Pipeline, error) {
	sess, err := kgo.NewGroupTransactSession(
		kgo.SeedBrokers(seeds...),
		kgo.TransactionalID(txnID),
		kgo.FetchIsolationLevel(kgo.ReadCommitted()),
		kgo.ConsumerGroup(group),
		kgo.ConsumeTopics(inTopic),
		kgo.DefaultProduceTopic(outTopic),
	)
	if err != nil {
		return nil, fmt.Errorf("new session: %w", err)
	}
	return &Pipeline{sess: sess}, nil
}

// Close releases the session.
func (p *Pipeline) Close() { p.sess.Close() }

// ProcessOnce runs one consume-transform-produce transaction. A nil return means
// the batch committed; ErrProcessAborted means it aborted (often a rebalance) and
// the caller should simply loop again.
func (p *Pipeline) ProcessOnce(ctx context.Context) error {
	fetches := p.sess.PollFetches(ctx)
	if errs := fetches.Errors(); len(errs) > 0 {
		return fmt.Errorf("poll fetches: %w", errs[0].Err)
	}

	if err := p.sess.Begin(); err != nil {
		return fmt.Errorf("begin: %w", err)
	}

	e := kgo.AbortingFirstErrPromise(p.sess.Client())
	fetches.EachRecord(func(r *kgo.Record) {
		out := Transform(r.Value)
		if out == nil {
			return
		}
		p.sess.Produce(ctx, &kgo.Record{Key: r.Key, Value: out}, e.Promise())
	})

	committed, err := p.sess.End(ctx, EndTry(e.Err()))
	switch {
	case err != nil:
		return fmt.Errorf("end: %w", err)
	case !committed:
		return abortErr(e.Err())
	default:
		return nil
	}
}
```

### The runnable demo

The demo needs no broker: it feeds known inputs through the pure helpers so the
output is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"github.com/twmb/franz-go/pkg/kgo"

	"example.com/eospipe"
)

func show(in string) string {
	out := eospipe.Transform([]byte(in))
	if out == nil {
		return "<tombstone: no output>"
	}
	return string(out)
}

func main() {
	fmt.Printf("transform(%q) -> %s\n", "login", show("login"))
	fmt.Printf("transform(%q)     -> %s\n", "", show(""))

	fmt.Printf("end (no error)      -> %v\n", eospipe.EndTry(nil) == kgo.TryCommit)
	fmt.Printf("end (produce error) -> %v\n", eospipe.EndTry(errors.New("x")) == kgo.TryAbort)

	fmt.Printf("end outcome (committed):  %s\n", eospipe.DescribeEnd(true, nil))
	fmt.Printf("end outcome (rebalance):  %s\n", eospipe.DescribeEnd(false, nil))
	fmt.Printf("end outcome (real error): %s\n", eospipe.DescribeEnd(false, errors.New("boom")))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
transform("login") -> evt:LOGIN
transform("")     -> <tombstone: no output>
end (no error)      -> true
end (produce error) -> true
end outcome (committed):  committed
end outcome (rebalance):  aborted-by-rebalance
end outcome (real error): error
```

### Tests

The tests pin the pure logic that determines correctness. `TestTransform` covers
the tombstone cases and a real value. `TestEndTry` proves the bool-derivation.
`TestDescribeEnd` is the important one: it asserts that `committed=false, err=nil`
classifies as a rebalance-abort (not an error), which is the semantic senior
engineers most often get wrong. `TestAbortErr` proves the wrapping.

Create `eospipe_test.go`:

```go
package eospipe

import (
	"errors"
	"fmt"
	"testing"

	"github.com/twmb/franz-go/pkg/kgo"
)

func TestTransform(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   []byte
		want []byte
	}{
		{"nil is tombstone", nil, nil},
		{"empty is tombstone", []byte(""), nil},
		{"value is tagged", []byte("login"), []byte("evt:LOGIN")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Transform(tc.in)
			if string(got) != string(tc.want) {
				t.Fatalf("Transform(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestEndTry(t *testing.T) {
	t.Parallel()
	if EndTry(nil) != kgo.TryCommit {
		t.Fatal("EndTry(nil) should commit")
	}
	if EndTry(errors.New("x")) != kgo.TryAbort {
		t.Fatal("EndTry(err) should abort")
	}
}

func TestDescribeEnd(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		committed bool
		err       error
		want      string
	}{
		{"committed", true, nil, "committed"},
		{"rebalance abort", false, nil, "aborted-by-rebalance"},
		{"real error", false, errors.New("boom"), "error"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := DescribeEnd(tc.committed, tc.err); got != tc.want {
				t.Fatalf("DescribeEnd(%v,%v) = %q, want %q", tc.committed, tc.err, got, tc.want)
			}
		})
	}
}

func TestAbortErr(t *testing.T) {
	t.Parallel()
	cause := errors.New("produce: broker down")
	err := abortErr(cause)
	if !errors.Is(err, ErrProcessAborted) || !errors.Is(err, cause) {
		t.Fatalf("abortErr(cause) = %v; want wraps sentinel and cause", err)
	}
	if got := abortErr(nil); !errors.Is(got, ErrProcessAborted) || errors.Unwrap(got) != nil {
		t.Fatalf("abortErr(nil) = %v, want bare sentinel", got)
	}
}

func ExampleDescribeEnd() {
	fmt.Println(DescribeEnd(true, nil))
	fmt.Println(DescribeEnd(false, nil))
	// Output:
	// committed
	// aborted-by-rebalance
}
```

## Review

The pipeline is correct when input offsets are committed inside the same
transaction as the produced outputs, and when a rebalance mid-transaction aborts
rather than commits. The mistakes to avoid are concrete. Do not commit offsets
separately from the transaction — `GroupTransactSession.End` commits them as part
of the transaction, and any separate commit reopens the reprocessing gap. Do not
omit `FetchIsolationLevel(kgo.ReadCommitted())`; the default surfaces records that
later abort. Do not call `Flush` before `End` here — the session flushes for you,
unlike the raw client. And do not treat `End` returning `committed=false` with a
nil error as a failure: it is the rebalance safety rail doing its job, and the
caller should just loop and reprocess. Confirm the pure logic with
`go test -count=1 -race ./...`; validate the session loop with `-tags kafka`
against a real cluster, asserting each input produces its output exactly once and
offsets advance atomically.

## Resources

- [franz-go transactions guide](https://github.com/twmb/franz-go/blob/master/docs/transactions.md) — `GroupTransactSession`, offset commits inside the transaction, KIP-447.
- [franz-go transactions example](https://github.com/twmb/franz-go/blob/master/examples/transactions/main.go) — the eos-mode consume-transform-produce loop this exercise mirrors.
- [`kgo` package reference](https://pkg.go.dev/github.com/twmb/franz-go/pkg/kgo) — `NewGroupTransactSession`, `PollFetches`, `Begin`, `Produce`, `End`, `FetchIsolationLevel`.

---

Back to [01-transactional-producer.md](01-transactional-producer.md) | Next: [03-txn-error-recovery-and-isolation.md](03-txn-error-recovery-and-isolation.md)
