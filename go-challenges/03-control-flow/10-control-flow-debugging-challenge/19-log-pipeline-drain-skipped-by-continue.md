# Exercise 19: Log Aggregator Drops Buffered Messages During Shutdown Due to Continue in Error Handler

**Nivel: Intermedio** — validacion rapida (un test corto).

A log-draining routine that writes every buffered message and then syncs
the sink once at the end looks straightforward, but the tempting way to
write "sync after the last message" is to tie the sync call to loop
position — `if i == len(buffered)-1 { sink.Sync() }` — instead of moving
it outside the loop entirely. The moment the *last* message in the batch
fails to write, the ordinary per-message `continue` used for error
handling reaches that iteration's end before the position-gated sync call
does, so every message written successfully before it never gets
persisted. This module is fully self-contained: its own `go mod init`, all
code inline, its own demo and tests.

## What you'll build

```text
logpipe/                    independent module: example.com/log-pipeline-drain-skipped-by-continue
  go.mod
  logpipe.go                 Message, Sink, Drain
  cmd/
    demo/
      main.go                runnable demo: batch where the last message fails to write
  logpipe_test.go             sync still fires on a trailing failure, plus success and sync-error paths
```

- Files: `logpipe.go`, `cmd/demo/main.go`, `logpipe_test.go`.
- Implement: `Drain(sink Sink, buffered []Message) []error` that writes every message and always calls `sink.Sync()` exactly once, regardless of which messages failed to write.
- Test: fail the write for the *last* message in the batch and assert `Sync` was still called and the earlier messages were recorded as written; two more tests cover the full-success and sync-failure paths.
- Verify: `go test -count=1 ./...`.

```bash
mkdir -p ~/go-exercises/log-pipeline-drain-skipped-by-continue/cmd/demo
cd ~/go-exercises/log-pipeline-drain-skipped-by-continue
go mod init example.com/log-pipeline-drain-skipped-by-continue
```

### Why tying Sync to loop position is the trap

The buggy shape keeps the sync call inside the `range`, guarded by an index
check meant to mean "only after the final message":

```go
for i, m := range buffered {
	if err := sink.Write(m); err != nil {
		errs = append(errs, fmt.Errorf("write %s: %w", m.ID, err))
		continue // BUG: on the last message, this skips the sync check below
	}
	if i == len(buffered)-1 {
		if err := sink.Sync(); err != nil {
			errs = append(errs, fmt.Errorf("sync: %w", err))
		}
	}
}
```

The `continue` in the write-error branch is correct in isolation — a bad
message should not stop the rest of the batch from being attempted. The
defect is that the sync call was placed *after* that `continue`, inside the
same loop body, gated on being the last iteration. When the last message's
write fails, `continue` fires first and the loop ends without ever reaching
the `i == len(buffered)-1` check for that iteration — there is no next
iteration to reach it on either, since it *was* the last one. Every message
written successfully before the last one is now sitting in the sink's
internal buffer, unsynced, and the function returns as if drain finished
cleanly except for the one reported write error. If the process exits
shortly after, everything that "succeeded" is lost along with the one that
failed. The fix decouples the sync from the loop's control flow entirely:
call `sink.Sync()` once, after the loop, unconditionally — its execution no
longer depends on which iteration was last or what happened during it.

Create `logpipe.go`:

```go
package logpipe

import "fmt"

// Message is a single buffered log line waiting to be written out.
type Message struct {
	ID   string
	Text string
}

// Sink is the destination for drained log messages. Write appends a
// message to the sink's internal buffer; Sync durably persists everything
// written so far.
type Sink interface {
	Write(Message) error
	Sync() error
}

// Drain writes every buffered message to sink, recording -- but not
// aborting on -- any individual write failure, since one bad message must
// not cost the rest of the batch. Once every message has been attempted,
// Drain calls sink.Sync exactly once, unconditionally, so every message
// that did write successfully is durably persisted even if some other
// message in the same batch failed to write at all.
func Drain(sink Sink, buffered []Message) []error {
	var errs []error

	for _, m := range buffered {
		if err := sink.Write(m); err != nil {
			errs = append(errs, fmt.Errorf("write %s: %w", m.ID, err))
			continue
		}
	}

	if err := sink.Sync(); err != nil {
		errs = append(errs, fmt.Errorf("sync: %w", err))
	}

	return errs
}
```

### The runnable demo

The demo drains a three-message batch where the last message fails to
write, showing that the first two are still recorded and `Sync` still
runs.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/log-pipeline-drain-skipped-by-continue"
)

// memSink records every message it is asked to write and whether Sync was
// called; it fails to write any message whose ID is in failIDs.
type memSink struct {
	failIDs    map[string]bool
	written    []string
	syncCalled bool
}

func (s *memSink) Write(m logpipe.Message) error {
	if s.failIDs[m.ID] {
		return fmt.Errorf("disk full writing %s", m.ID)
	}
	s.written = append(s.written, m.ID)
	return nil
}

func (s *memSink) Sync() error {
	s.syncCalled = true
	return nil
}

func main() {
	buffered := []logpipe.Message{
		{ID: "log-1", Text: "start"},
		{ID: "log-2", Text: "progress"},
		{ID: "log-3", Text: "shutdown requested"},
	}

	// The last message in the batch fails to write.
	sink := &memSink{failIDs: map[string]bool{"log-3": true}}
	errs := logpipe.Drain(sink, buffered)

	fmt.Println("written:", sink.written)
	fmt.Println("sync called:", sink.syncCalled)
	fmt.Println("errors:", errors.Join(errs...))
}
```

```bash
go run ./cmd/demo
```

Expected output:

```text
written: [log-1 log-2]
sync called: true
errors: write log-3: disk full writing log-3
```

### Tests

Create `logpipe_test.go`:

```go
package logpipe

import (
	"errors"
	"fmt"
	"testing"
)

type fakeSink struct {
	failIDs    map[string]bool
	written    []string
	syncCalled bool
	syncErr    error
}

func (s *fakeSink) Write(m Message) error {
	if s.failIDs[m.ID] {
		return fmt.Errorf("disk full writing %s", m.ID)
	}
	s.written = append(s.written, m.ID)
	return nil
}

func (s *fakeSink) Sync() error {
	s.syncCalled = true
	return s.syncErr
}

func TestDrainSyncsEvenWhenLastMessageFailsToWrite(t *testing.T) {
	buffered := []Message{
		{ID: "log-1"},
		{ID: "log-2"},
		{ID: "log-3"},
	}
	sink := &fakeSink{failIDs: map[string]bool{"log-3": true}}

	errs := Drain(sink, buffered)

	if !sink.syncCalled {
		t.Fatal("Sync was not called; earlier successful writes were never durably persisted")
	}
	wantWritten := []string{"log-1", "log-2"}
	if len(sink.written) != len(wantWritten) {
		t.Fatalf("written = %v, want %v", sink.written, wantWritten)
	}
	if len(errs) != 1 {
		t.Fatalf("errs = %v, want exactly 1 error (the failed write)", errs)
	}
}

func TestDrainSyncsOnFullSuccess(t *testing.T) {
	buffered := []Message{{ID: "log-1"}, {ID: "log-2"}}
	sink := &fakeSink{}

	errs := Drain(sink, buffered)

	if !sink.syncCalled {
		t.Fatal("Sync was not called")
	}
	if errs != nil {
		t.Fatalf("errs = %v, want nil", errs)
	}
}

func TestDrainReportsSyncFailure(t *testing.T) {
	buffered := []Message{{ID: "log-1"}}
	syncErr := errors.New("sync backend unavailable")
	sink := &fakeSink{syncErr: syncErr}

	errs := Drain(sink, buffered)

	if len(errs) != 1 || !errors.Is(errs[0], syncErr) {
		t.Fatalf("errs = %v, want exactly 1 error wrapping %v", errs, syncErr)
	}
}
```

Run: `go test -count=1 ./...`.

## Review

`Drain` is correct when `Sync` runs exactly once on every call, regardless
of how many individual writes failed or which one failed — because its
execution no longer depends on loop position at all. The mistake this
design avoids is gating a must-run step on "is this the last iteration"
inside the same loop body as an unrelated `continue`: the two pieces of
control flow look independent but are not, since a `continue` fired for
any reason on that specific iteration silently removes the only place the
must-run step was reachable from. Whenever a step has to happen "once,
after processing everything," write it after the loop, not inside an
`if i == last` check tangled up with per-item error handling.

## Resources

- [Go Specification: For statements with range clause](https://go.dev/ref/spec#For_range) — `continue` ends the current iteration and evaluates the next range value; code after it in the same iteration never runs.
- [errors.Join](https://pkg.go.dev/errors#Join) — combining multiple independent write failures into a single error value.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [18-batch-aggregator-continue-blocks-shutdown.md](18-batch-aggregator-continue-blocks-shutdown.md) | Next: [20-sharded-cache-shadowed-shard-index.md](20-sharded-cache-shadowed-shard-index.md)
