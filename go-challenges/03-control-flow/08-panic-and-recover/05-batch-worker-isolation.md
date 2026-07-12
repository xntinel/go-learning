# Exercise 5: Batch Consumer: Per-Item Recover So One Poison Message Can't Kill the Batch

When you pull a batch of messages off SQS or Kafka and process them in a loop, one
handler that panics on a malformed record must not abort the whole batch and force
you to nack all of it. The production pattern is a per-item `defer`/`recover`: a
panic on message 7 records a failure for that item and the loop continues through
message N. This module builds `ProcessBatch`, which isolates every item, collects
the successes, and aggregates the failures with `errors.Join`.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
batch/                     independent module: example.com/batch
  go.mod                   go 1.26
  batch.go                 Message, Result, ProcessBatch, processOne
  cmd/
    demo/
      main.go              runnable demo: a 5-message batch with one poison item
  batch_test.go            one panic + one error in a batch; both surfaced
```

Files: `batch.go`, `cmd/demo/main.go`, `batch_test.go`.
Implement: `ProcessBatch(ctx, msgs []Message, handle func(context.Context, Message) error) ([]Result, error)` that wraps each item in its own `defer`/`recover` and joins failures with `errors.Join`.
Test: a batch of 5 where index 2 panics and index 4 returns an error; assert 3 successes, the joined error `errors.Is` each failure, processing reached the last item, and a second panicking item is also caught (recover is re-armed each iteration).
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/08-panic-and-recover/05-batch-worker-isolation/cmd/demo
cd go-solutions/03-control-flow/08-panic-and-recover/05-batch-worker-isolation
```

### Why the recover goes inside a per-item helper

The naive version wraps the whole loop in one `defer`/`recover`. That is worse
than nothing: the first panic unwinds out of the loop, the deferred recover fires
once, and every message after the poison one is silently dropped — you processed
half a batch and reported success. The correct structure puts the recover around a
*single* item, in a helper `processOne` that the loop calls once per message. A
panic in `handle` unwinds only out of `processOne`, whose deferred recover
converts it to an error and returns it; the loop sees a normal error return and
moves to the next message. The recover is re-armed on every iteration because it
lives in a fresh `processOne` call each time — a second poison message N iterations
later is caught exactly the same way.

`processOne` uses a named return value `err` so the deferred recover can set the
function's result. When the recovered value is already an `error`, it wraps it
with `%w` (`fmt.Errorf("message %s panicked: %w", msg.ID, e)`) so the original
survives and `errors.Is` still finds it; otherwise it formats with `%v`. The
capture of `debug.Stack()` happens inside the recover so a poison message leaves a
usable trace for the operator.

`ProcessBatch` collects per-item outcomes into a `[]Result` (one entry per message,
in order, marked OK or not) and accumulates the failing errors into a slice, which
it hands to `errors.Join`. `errors.Join` builds a single error whose `Is`/`As`
walks each joined member, so the caller can test `errors.Is(err, errPoison)` for
any specific failure and still see the aggregate in one value. Successes and the
joined error come back together — the whole point of the pattern is that you commit
the 3 that worked and only redeliver the 2 that failed, rather than nacking all 5.

Create `batch.go`:

```go
package batch

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
)

// Message is one record pulled from a queue.
type Message struct {
	ID   string
	Body string
}

// Result is the per-item outcome, in batch order.
type Result struct {
	ID    string
	OK    bool
	Stack []byte // populated only when the item panicked
}

// ProcessBatch runs handle over every message, isolating each in its own
// recover so one poison item does not abort the batch. It returns a Result per
// message and the joined error of all failures.
func ProcessBatch(ctx context.Context, msgs []Message, handle func(context.Context, Message) error) ([]Result, error) {
	results := make([]Result, 0, len(msgs))
	var errs []error

	for i := range msgs {
		msg := msgs[i]
		err, stack := processOne(ctx, msg, handle)
		if err != nil {
			errs = append(errs, err)
			results = append(results, Result{ID: msg.ID, OK: false, Stack: stack})
			continue
		}
		results = append(results, Result{ID: msg.ID, OK: true})
	}

	return results, errors.Join(errs...)
}

// processOne runs handle for a single message, converting a panic into an error
// so the caller's loop can continue to the next message.
func processOne(ctx context.Context, msg Message, handle func(context.Context, Message) error) (err error, stack []byte) {
	defer func() {
		if rec := recover(); rec != nil {
			stack = debug.Stack()
			if e, ok := rec.(error); ok {
				err = fmt.Errorf("message %s panicked: %w", msg.ID, e)
				return
			}
			err = fmt.Errorf("message %s panicked: %v", msg.ID, rec)
		}
	}()
	return handle(ctx, msg), nil
}
```

### The runnable demo

The demo processes five messages; `msg-3` panics and `msg-5` returns an ordinary
error. The demo prints each item's outcome and the count of successes, showing the
loop reached the end despite the poison item.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/batch"
)

func main() {
	msgs := []batch.Message{
		{ID: "msg-1", Body: "ok"},
		{ID: "msg-2", Body: "ok"},
		{ID: "msg-3", Body: "poison"},
		{ID: "msg-4", Body: "ok"},
		{ID: "msg-5", Body: "reject"},
	}

	handle := func(_ context.Context, m batch.Message) error {
		switch m.Body {
		case "poison":
			panic(errors.New("cannot decode record"))
		case "reject":
			return errors.New("business rule rejected")
		default:
			return nil
		}
	}

	results, err := batch.ProcessBatch(context.Background(), msgs, handle)

	ok := 0
	for _, r := range results {
		if r.OK {
			ok++
			fmt.Printf("%s: ok\n", r.ID)
		} else {
			fmt.Printf("%s: failed\n", r.ID)
		}
	}
	fmt.Printf("%d of %d succeeded; batch not aborted\n", ok, len(results))
	fmt.Printf("aggregate error present: %v\n", err != nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
msg-1: ok
msg-2: ok
msg-3: failed
msg-4: ok
msg-5: failed
3 of 5 succeeded; batch not aborted
aggregate error present: true
```

### Tests

`TestBatchIsolatesFailures` builds a batch where index 2 panics with a sentinel
and index 4 returns a different sentinel, then asserts three successes, that the
joined error `errors.Is` each sentinel, and that the last message was reached (its
`Result` exists). `TestRecoverReArmedEachIteration` uses two panicking items to
prove the recover fires more than once. `TestEmptyBatch` confirms a nil-safe empty
result and a `nil` joined error.

Create `batch_test.go`:

```go
package batch

import (
	"context"
	"errors"
	"testing"
)

var (
	errPoison = errors.New("poison record")
	errReject = errors.New("rejected by rule")
)

func TestBatchIsolatesFailures(t *testing.T) {
	t.Parallel()

	msgs := []Message{
		{ID: "0"}, {ID: "1"}, {ID: "2"}, {ID: "3"}, {ID: "4"},
	}
	handle := func(_ context.Context, m Message) error {
		switch m.ID {
		case "2":
			panic(errPoison)
		case "4":
			return errReject
		default:
			return nil
		}
	}

	results, err := ProcessBatch(context.Background(), msgs, handle)

	if len(results) != 5 {
		t.Fatalf("len(results) = %d, want 5 (loop must reach the last item)", len(results))
	}
	if !results[4].OK && results[4].ID != "4" {
		t.Fatalf("last result = %+v, want the id-4 message", results[4])
	}
	okCount := 0
	for _, r := range results {
		if r.OK {
			okCount++
		}
	}
	if okCount != 3 {
		t.Fatalf("successes = %d, want 3", okCount)
	}
	if !errors.Is(err, errPoison) {
		t.Fatalf("joined error missing the panic failure: %v", err)
	}
	if !errors.Is(err, errReject) {
		t.Fatalf("joined error missing the returned failure: %v", err)
	}
}

func TestRecoverReArmedEachIteration(t *testing.T) {
	t.Parallel()

	msgs := []Message{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	handle := func(_ context.Context, m Message) error {
		if m.ID == "a" || m.ID == "c" {
			panic(errPoison)
		}
		return nil
	}

	results, err := ProcessBatch(context.Background(), msgs, handle)

	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3", len(results))
	}
	if results[0].OK || results[2].OK {
		t.Fatal("both panicking items should be marked failed")
	}
	if !results[1].OK {
		t.Fatal("the middle item should have succeeded")
	}
	if !errors.Is(err, errPoison) {
		t.Fatalf("joined error missing poison failures: %v", err)
	}
}

func TestEmptyBatch(t *testing.T) {
	t.Parallel()

	results, err := ProcessBatch(context.Background(), nil, func(context.Context, Message) error {
		return nil
	})
	if len(results) != 0 {
		t.Fatalf("len(results) = %d, want 0", len(results))
	}
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
}
```

## Review

`ProcessBatch` is correct when the loop always reaches the last message regardless
of how many earlier items panicked, the successes come back separately from the
failures, and the joined error lets the caller `errors.Is` each specific failure.
The structural rule is the whole lesson: the recover must wrap one item
(`processOne`), never the whole loop — wrapping the loop drops every message after
the first panic. `errors.Join` is the right aggregator because its `Is`/`As` walks
each member, so one returned value carries every failure without flattening them
into a string. Run the tests with `-race`; the pattern is single-goroutine here,
but a real consumer often fans items out, and the same per-item recover applies to
each worker.

## Resources

- [errors.Join](https://pkg.go.dev/errors#Join) — aggregating multiple failures into one value whose Is/As walks each.
- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — the per-call recover mechanism.
- [fmt.Errorf and %w](https://pkg.go.dev/fmt#Errorf) — preserving a panicked error through the wrap.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-parser-internal-panic.md](06-parser-internal-panic.md)
