# Exercise 11: Streaming Export Writer That Drains the Remainder on a Write Failure

**Level: Intermediate**

A bulk-export endpoint streams every matching row to an `io.Writer` (an HTTP
response body, an S3 multipart part) while a producer goroutine feeds records over
a channel. When a write fails partway through, the naive consumer `break`s out of
its range and returns the error — and strands the still-running producer blocked on
its next send, a goroutine leak that surfaces as a hung request handler. This
exercise builds the terminal sink that stops writing on failure but keeps draining
the channel to completion, so the producer always finishes.

This module is self-contained: its own module, an `exportsink` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
exportsink/                  independent module: example.com/exportsink
  go.mod                     go 1.26
  exportsink.go              type Record, Encoder; Export(w, in, enc) (written int, err error)
  cmd/demo/main.go           runnable demo: full stream, a mid-stream failure, and a drain proof
  exportsink_test.go         full-stream ordering, mid-stream drain, first-error, empty input, large-N
```

- Files: `exportsink.go`, `cmd/demo/main.go`, `exportsink_test.go`.
- Implement: `Export(w io.Writer, in <-chan Record, enc Encoder) (written int, err error)` that ranges `in` to completion, encoding each record until `enc` returns an error, then stops writing but keeps draining.
- Test: full stream encodes every record in order with `written == count` and `err == nil`; a failure at record k returns `written == k-1` and that error while proving the producer drained to close under goleak; empty-closed input returns `(0, nil)`; a large-N run pins ordering and bytes.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/exportsink/cmd/demo
cd ~/go-exercises/exportsink
go mod init example.com/exportsink
go get go.uber.org/goleak
go mod tidy
```

### Why the terminal sink must drain, not break

The rule from the concept file is blunt: `break` stops consuming but does not close
or drain the channel, and if the producer keeps sending, it blocks forever on its
next send. In a streaming export that rule has teeth. The producer is a live
goroutine — it queried the database and is copying rows into an unbuffered channel,
one send at a time, each send blocking until the sink receives it. The sink is the
only receiver. The moment a write to the response body fails (client disconnected,
S3 part rejected, disk full) the instinct is to return the error immediately. Do
that with a `break` and the producer, mid-send on row k+1, parks forever. The HTTP
handler that spawned it returns, but the goroutine never does: a leak per failed
export, and under load the process climbs in goroutine count until it falls over.

The fix is a two-phase range that never leaves the loop early:

1. Range `in` to its close, unconditionally. The loop only ends when the producer
   closes the channel, which is the producer's own signal that it has stopped
   sending. That is the sole safe exit.
2. While no error has occurred, encode each record and count it.
3. On the first encoder error, record the error and flip into drain mode: keep
   receiving every remaining record so the producer's sends all complete, but write
   nothing more. The first error is sticky — later records are discarded, not
   re-encoded, and a subsequent encoder failure never overwrites it.

The signature encodes the contract. `in <-chan Record` is receive-only, so the sink
physically cannot close or send on it — closing is the producer's job, matching the
ownership rule. The return `(written int, err error)` reports exactly how far the
export got: `written` is the count actually flushed to `w`, and `err` is the first
failure or `nil`. A caller can trust that when `err != nil`, `written` records were
durably encoded and everything after was dropped by design, not by accident.

Create `exportsink.go`:

```go
package exportsink

import "io"

// Record is one row streamed to the export sink.
type Record struct {
	ID   int
	Body string
}

// Encoder writes one record to w. A non-nil error means the write failed and no
// further records should be written to w.
type Encoder func(w io.Writer, rec Record) error

// Export ranges in to completion, encoding each record until enc returns an
// error. On the first encoder error it stops writing but keeps ranging in so the
// producer is never stranded blocking on its next send. It returns the count of
// records successfully written and the first error (nil if all were written).
//
// The drain-the-remainder discipline is the point: a plain break here would leave
// the producer parked on a send with nobody receiving. Ranging to close guarantees
// the producer's for-range or defer close(in) completes.
func Export(w io.Writer, in <-chan Record, enc Encoder) (written int, err error) {
	for rec := range in {
		if err != nil {
			// Already failed: keep draining, but write nothing more.
			continue
		}
		if encErr := enc(w, rec); encErr != nil {
			err = encErr
			continue
		}
		written++
	}
	return written, err
}
```

### The runnable demo

The demo runs three scenarios against real producer goroutines that close their
channels: a full stream, a stream whose encoder fails at record 3, and an explicit
drain proof where a `WaitGroup` returns only after the producer's send loop and
`close` both complete — if `Export` failed to drain, `wg.Wait` would hang.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"strings"
	"sync"

	"example.com/exportsink"
)

// produce sends records 1..n over a fresh channel from a goroutine that closes
// the channel when done, then returns the channel. Because Export always drains
// to close, this producer can never strand on a send.
func produce(n int) <-chan exportsink.Record {
	out := make(chan exportsink.Record)
	go func() {
		defer close(out)
		for i := 1; i <= n; i++ {
			out <- exportsink.Record{ID: i, Body: fmt.Sprintf("row-%d", i)}
		}
	}()
	return out
}

// lineEncoder writes "ID:Body\n". failAt > 0 makes it fail deterministically on
// the record with that ID, simulating a mid-stream write failure.
func lineEncoder(failAt int) exportsink.Encoder {
	return func(w io.Writer, rec exportsink.Record) error {
		if failAt > 0 && rec.ID == failAt {
			return fmt.Errorf("write failed at record %d", rec.ID)
		}
		_, err := fmt.Fprintf(w, "%d:%s\n", rec.ID, rec.Body)
		return err
	}
}

func main() {
	// Case 1: full stream, no failure.
	var full strings.Builder
	written, err := exportsink.Export(&full, produce(4), lineEncoder(0))
	fmt.Printf("full:   written=%d err=%v\n", written, err)
	fmt.Printf("body:   %s\n", strings.ReplaceAll(strings.TrimRight(full.String(), "\n"), "\n", " | "))

	// Case 2: encoder fails at record 3. Export stops writing but drains the rest,
	// so the producer goroutine completes and closes without blocking.
	var partial strings.Builder
	written, err = exportsink.Export(&partial, produce(4), lineEncoder(3))
	fmt.Printf("failure: written=%d err=%v\n", written, err)
	fmt.Printf("body:    %s\n", strings.ReplaceAll(strings.TrimRight(partial.String(), "\n"), "\n", " | "))

	// The producers above already proved drain-on-error indirectly. Make it
	// explicit: a producer that signals via WaitGroup.Done only after its send
	// loop and close both complete. If Export failed to drain, wg.Wait would hang.
	var wg sync.WaitGroup
	ch := make(chan exportsink.Record)
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(ch)
		for i := 1; i <= 5; i++ {
			ch <- exportsink.Record{ID: i, Body: fmt.Sprintf("row-%d", i)}
		}
	}()
	written, err = exportsink.Export(io.Discard, ch, lineEncoder(2))
	wg.Wait()
	fmt.Printf("drained: written=%d err=%v producer=done\n", written, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
full:   written=4 err=<nil>
body:   1:row-1 | 2:row-2 | 3:row-3 | 4:row-4
failure: written=2 err=write failed at record 3
body:    1:row-1 | 2:row-2
drained: written=1 err=write failed at record 2 producer=done
```

### Tests

`TestFullStreamEncodesEveryRecordInOrder` runs a live producer of five records and
asserts `written == 5`, `err == nil`, and the exact byte output in order.
`TestMidStreamFailureDrainsRemainderAndReturnsFirstError` fails at record 4 of six:
it asserts `written == 3`, the returned error is the encoder's, only records 1..3
were written, and — the load-bearing part — `wg.Wait` returns, proving records 5
and 6 were drained after the failure. Were `Export` to break, the producer would
block on send(5) and `wg.Wait` would hang, which `goleak.VerifyTestMain` catches as
a leaked goroutine. `TestFirstErrorWins` fails on every record from 2 onward and
asserts the returned error is the first one. `TestEmptyClosedInputWritesNothing`
returns `(0, nil)` with no bytes. `TestLargeStreamOrderingAndBytes` streams 10,000
records and byte-compares the whole output; `TestLargeStreamFailureMidwayStillDrains`
fails at 5000 and confirms `written == 4999` while draining all 10,000.

Create `exportsink_test.go`:

```go
package exportsink

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// lineEnc writes "ID:Body\n"; failAt > 0 fails deterministically on that ID.
func lineEnc(failAt int) (Encoder, *error) {
	sentinel := fmt.Errorf("encode failed at record %d", failAt)
	captured := new(error)
	return func(w io.Writer, rec Record) error {
		if failAt > 0 && rec.ID == failAt {
			*captured = sentinel
			return sentinel
		}
		_, err := fmt.Fprintf(w, "%d:%s\n", rec.ID, rec.Body)
		return err
	}, captured
}

// produceClose returns a channel fed 1..n from a goroutine that closes it, plus a
// WaitGroup whose Wait returns only after the producer's send loop AND close both
// finish. If Export fails to drain, wg.Wait blocks and goleak reports the leak.
func produceClose(n int) (<-chan Record, *sync.WaitGroup) {
	out := make(chan Record)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(out)
		for i := 1; i <= n; i++ {
			out <- Record{ID: i, Body: fmt.Sprintf("row-%d", i)}
		}
	}()
	return out, &wg
}

func TestFullStreamEncodesEveryRecordInOrder(t *testing.T) {
	t.Parallel()
	ch, wg := produceClose(5)
	enc, _ := lineEnc(0)

	var buf bytes.Buffer
	written, err := Export(&buf, ch, enc)
	wg.Wait() // producer completed: channel fully drained and closed

	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if written != 5 {
		t.Fatalf("written = %d, want 5", written)
	}
	want := "1:row-1\n2:row-2\n3:row-3\n4:row-4\n5:row-5\n"
	if buf.String() != want {
		t.Fatalf("body = %q, want %q", buf.String(), want)
	}
}

func TestMidStreamFailureDrainsRemainderAndReturnsFirstError(t *testing.T) {
	t.Parallel()
	ch, wg := produceClose(6)
	enc, captured := lineEnc(4) // fails on record 4

	var buf bytes.Buffer
	written, err := Export(&buf, ch, enc)
	// wg.Wait returns only if Export drained records 5 and 6 after the failure.
	// Were Export to break at the error, the producer would block on send(5) and
	// this Wait would hang forever, tripping goleak.
	wg.Wait()

	if written != 3 {
		t.Fatalf("written = %d, want 3 (records 1..3 before the failure at 4)", written)
	}
	if !errors.Is(err, *captured) {
		t.Fatalf("err = %v, want the encoder error %v", err, *captured)
	}
	// Only the pre-failure records were written; nothing after the failure.
	want := "1:row-1\n2:row-2\n3:row-3\n"
	if buf.String() != want {
		t.Fatalf("body = %q, want %q (no writes after the failure)", buf.String(), want)
	}
}

func TestFirstErrorWins(t *testing.T) {
	t.Parallel()
	// An encoder that fails on every record from 2 onward. Export must return the
	// FIRST error (from record 2), not a later one, and stop writing.
	var firstErr error
	enc := func(w io.Writer, rec Record) error {
		if rec.ID >= 2 {
			e := fmt.Errorf("fail on %d", rec.ID)
			if firstErr == nil {
				firstErr = e
			}
			return e
		}
		_, err := fmt.Fprintf(w, "%d\n", rec.ID)
		return err
	}
	ch, wg := produceClose(5)

	var buf bytes.Buffer
	written, err := Export(&buf, ch, enc)
	wg.Wait()

	if written != 1 {
		t.Fatalf("written = %d, want 1", written)
	}
	if err == nil || err.Error() != "fail on 2" {
		t.Fatalf("err = %v, want first error \"fail on 2\"", err)
	}
	if buf.String() != "1\n" {
		t.Fatalf("body = %q, want %q", buf.String(), "1\n")
	}
}

func TestEmptyClosedInputWritesNothing(t *testing.T) {
	t.Parallel()
	ch := make(chan Record)
	close(ch)
	enc, _ := lineEnc(0)

	var buf bytes.Buffer
	written, err := Export(&buf, ch, enc)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if written != 0 {
		t.Fatalf("written = %d, want 0", written)
	}
	if buf.Len() != 0 {
		t.Fatalf("body = %q, want empty", buf.String())
	}
}

func TestLargeStreamOrderingAndBytes(t *testing.T) {
	t.Parallel()
	const n = 10_000
	ch, wg := produceClose(n)
	enc, _ := lineEnc(0)

	var buf bytes.Buffer
	written, err := Export(&buf, ch, enc)
	wg.Wait()

	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if written != n {
		t.Fatalf("written = %d, want %d", written, n)
	}
	var want strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&want, "%d:row-%d\n", i, i)
	}
	if buf.String() != want.String() {
		t.Fatal("large-stream body mismatch: ordering or bytes wrong")
	}
}

func TestLargeStreamFailureMidwayStillDrains(t *testing.T) {
	t.Parallel()
	const n = 10_000
	ch, wg := produceClose(n)
	enc, captured := lineEnc(5000) // fail exactly halfway

	written, err := Export(io.Discard, ch, enc)
	wg.Wait() // proves all 10k records were consumed despite the mid-stream failure

	if written != 4999 {
		t.Fatalf("written = %d, want 4999", written)
	}
	if !errors.Is(err, *captured) {
		t.Fatalf("err = %v, want encoder error", err)
	}
}
```

## Review

The sink is correct when, on a write failure at record k, it returns `k-1` written
and the first error while still consuming every remaining record so the producer
completes — and when a clean run encodes all records in order and returns
`(count, nil)`. The invariant that guarantees it is that `Export` never leaves its
`range` early: the loop ends only when the producer closes the channel, so a failed
encode flips the loop into a write-nothing drain rather than a `break`. The mid-stream
tests prove drainage structurally, not by timing: the producer's `WaitGroup.Done`
fires only after its send loop and `close` both return, so `wg.Wait` unblocking is
direct evidence that records after the failure were received, and
`goleak.VerifyTestMain` fails the run if any producer goroutine is left parked on a
send. The production bug this prevents is the one that hides in a `return err` on the
first write failure: a stranded producer goroutine per failed export, leaking until
the process exhausts memory or its goroutine budget and the export endpoint hangs.

## Resources

- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) -- the ownership and draining discipline for producer/consumer channel stages.
- [pkg.go.dev: io.Writer](https://pkg.go.dev/io#Writer) -- the terminal sink interface and the contract that a failed write returns a non-nil error.
- [pkg.go.dev: go.uber.org/goleak](https://pkg.go.dev/go.uber.org/goleak) -- verifying no goroutine (here, a stranded producer) leaks past a test.
- [Go Wiki: Channel design](https://go.dev/wiki/CodeReviewComments#goroutine-lifetimes) -- keeping goroutine lifetimes bounded so a consumer never abandons a live sender.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-bounded-window-consumer.md](10-bounded-window-consumer.md) | Next: [12-streaming-usage-rollup-by-key.md](12-streaming-usage-rollup-by-key.md)
