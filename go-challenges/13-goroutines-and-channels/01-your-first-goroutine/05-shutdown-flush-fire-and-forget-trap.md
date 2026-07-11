# Exercise 5: Guarantee a Fire-and-Forget Writer Flushes Before Shutdown

The most expensive lesson about goroutines is usually learned in production: the
logs, metrics, or audit records buffered by a fire-and-forget writer vanish on the
next deploy, because when the last goroutine returns the process exits and every
un-joined goroutine is killed mid-work. This exercise builds the correct version —
a background writer with a `Close` that blocks until the drain goroutine has
persisted everything — and demonstrates the failure mode deterministically.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
logwriter/                   independent module: example.com/logwriter
  go.mod
  logwriter.go               Store; Writer (drain goroutine + joining Close);
                             BrokenWriter (CloseNoWait — models the loss)
  cmd/
    demo/
      main.go                write K records, Close, count persisted
  logwriter_test.go          Close persists exactly K; broken variant loses records
```

Files: `logwriter.go`, `cmd/demo/main.go`, `logwriter_test.go`.
Implement: a `Writer` whose `Write` enqueues a record and whose `Close` closes the queue and blocks on a `WaitGroup` until the drain goroutine has persisted everything; a `BrokenWriter` whose `CloseNoWait` returns without joining.
Test: feed K records to `Writer`, `Close`, assert the store holds exactly K; a controlled `BrokenWriter` test shows a count below K to document the loss.
Verify: `go test -race -count=5 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/logwriter/cmd/demo
cd ~/go-exercises/logwriter
go mod init example.com/logwriter
```

### The shutdown-flush contract

A background writer decouples the hot path from the slow persistence: callers
push records into an in-memory queue and return immediately, while a single drain
goroutine pulls from the queue and writes them to the store. The queue smooths
bursts and keeps the request path fast. The danger is entirely at shutdown. If the
process exits while records are still in the queue — or in flight in the drain
goroutine — they are gone. The runtime does not flush your buffers for you; it
simply stops.

The correct shape has three parts. The drain goroutine ranges over the queue
channel: `for rec := range w.ch { w.store.Append(rec) }`. `Close` does exactly two
things in order: `close(w.ch)`, which tells the drain loop no more records are
coming (the `range` ends once the buffer is empty), and then `w.wg.Wait()`, which
blocks until the drain goroutine has returned — meaning every buffered record has
been persisted. After `Close` returns, the store provably contains everything that
was written. This is the join that a shutdown path must perform before the process
is allowed to exit.

Contrast the broken writer. `CloseNoWait` calls `close(w.ch)` but does *not* wait
for the drain goroutine. In production, `main` then returns, the process exits,
and any records still in the queue or being written are lost. You cannot exit the
process inside a unit test, so to demonstrate the loss deterministically the
broken writer's drain goroutine is gated: it will not persist anything until a
`release` channel is signaled. The test writes K records, calls `CloseNoWait`
(which returns immediately without draining), and reads the store while the drain
is still parked at the gate — observing zero persisted records, exactly modelling
"the process exited before the drain ran". The test then releases the gate and
joins, so the goroutine is not leaked.

Create `logwriter.go`:

```go
package logwriter

import "sync"

// Store is a concurrency-safe destination for persisted records.
type Store struct {
	mu   sync.Mutex
	recs []string
}

func (s *Store) Append(rec string) {
	s.mu.Lock()
	s.recs = append(s.recs, rec)
	s.mu.Unlock()
}

func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.recs)
}

// Writer buffers records and drains them to a Store in a background goroutine.
// Close joins that goroutine, guaranteeing every buffered record is persisted
// before Close returns.
type Writer struct {
	ch    chan string
	wg    sync.WaitGroup
	store *Store
}

func NewWriter(store *Store, buffer int) *Writer {
	w := &Writer{ch: make(chan string, buffer), store: store}
	w.wg.Add(1)
	go w.drain()
	return w
}

func (w *Writer) drain() {
	defer w.wg.Done()
	for rec := range w.ch {
		w.store.Append(rec)
	}
}

// Write enqueues a record for background persistence.
func (w *Writer) Write(rec string) { w.ch <- rec }

// Close stops accepting records and blocks until the drain goroutine has
// persisted everything already enqueued.
func (w *Writer) Close() {
	close(w.ch)
	w.wg.Wait()
}

// BrokenWriter models the fire-and-forget bug: CloseNoWait does not join the
// drain goroutine. Its drain is gated by release so a test can demonstrate the
// loss deterministically instead of relying on process exit.
type BrokenWriter struct {
	ch      chan string
	wg      sync.WaitGroup
	store   *Store
	release chan struct{}
}

func NewBrokenWriter(store *Store, buffer int) *BrokenWriter {
	w := &BrokenWriter{
		ch:      make(chan string, buffer),
		store:   store,
		release: make(chan struct{}),
	}
	w.wg.Add(1)
	go w.drain()
	return w
}

func (w *BrokenWriter) drain() {
	defer w.wg.Done()
	<-w.release // models "the scheduler had not run the drain before process exit"
	for rec := range w.ch {
		w.store.Append(rec)
	}
}

func (w *BrokenWriter) Write(rec string) { w.ch <- rec }

// CloseNoWait closes the queue but does NOT wait for the drain goroutine.
func (w *BrokenWriter) CloseNoWait() { close(w.ch) }

// ReleaseAndJoin is a test helper: it unblocks the gated drain and waits for it,
// so the demonstration does not leak the goroutine.
func (w *BrokenWriter) ReleaseAndJoin() {
	close(w.release)
	w.wg.Wait()
}
```

### The runnable demo

The demo writes five records, closes the writer (which flushes), and prints how
many were persisted. Because `Close` joins the drain, the count is exact every
run.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/logwriter"
)

func main() {
	var store logwriter.Store
	w := logwriter.NewWriter(&store, 1024)

	for i := range 5 {
		w.Write(fmt.Sprintf("record-%d", i))
	}
	w.Close() // flushes: blocks until every buffered record is persisted

	fmt.Printf("persisted %d records\n", store.Len())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
persisted 5 records
```

### Tests

`TestWriterFlushesEverythingOnClose` writes K records, calls `Close`, and asserts
the store holds exactly K — the flush contract. `TestBrokenWriterLosesRecords`
documents the failure mode deterministically: it writes K records to a
`BrokenWriter`, calls `CloseNoWait`, and asserts the store is empty while the
drain is still gated (modelling loss at process exit), then releases and joins to
avoid a leak. Running with `-race -count=5` shakes out any timing-dependent write.

Create `logwriter_test.go`:

```go
package logwriter

import (
	"fmt"
	"testing"
)

func TestWriterFlushesEverythingOnClose(t *testing.T) {
	t.Parallel()

	const k = 1000
	var store Store
	w := NewWriter(&store, 64)
	for i := range k {
		w.Write(fmt.Sprintf("r%d", i))
	}
	w.Close()

	if got := store.Len(); got != k {
		t.Fatalf("persisted %d records after Close, want %d", got, k)
	}
}

func TestBrokenWriterLosesRecords(t *testing.T) {
	t.Parallel()

	const k = 1000
	var store Store
	w := NewBrokenWriter(&store, k) // buffer >= k so Write never blocks
	for i := range k {
		w.Write(fmt.Sprintf("r%d", i))
	}
	w.CloseNoWait() // returns without draining

	// The drain goroutine is gated, modelling a process that exited before it
	// ran: none of the buffered records were persisted.
	if got := store.Len(); got != 0 {
		t.Fatalf("broken writer persisted %d records, want 0 (loss not demonstrated)", got)
	}

	w.ReleaseAndJoin() // clean up so the goroutine is not leaked
	if got := store.Len(); got != k {
		t.Fatalf("after release, persisted %d records, want %d", got, k)
	}
}

func ExampleWriter() {
	var store Store
	w := NewWriter(&store, 8)
	w.Write("a")
	w.Write("b")
	w.Close()
	fmt.Println(store.Len())
	// Output: 2
}
```

## Review

The writer is correct when `Close` is a true flush barrier: after it returns, the
store contains every record that was written, with no loss. The mechanism is the
join — `close(ch)` to end the drain loop, then `wg.Wait()` to confirm the drain
goroutine returned. Drop the `Wait` and you have `CloseNoWait`, the exact bug that
loses buffered records when the process exits; the broken-writer test makes that
loss visible without relying on process death. Run `-race -count=5`: the writer
must be flush-exact on every interleaving, and the `Store`'s mutex must hold under
concurrent `Append` from the drain and `Len` from the test.

## Resources

- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup)
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels)
- [Go Language Specification: Close](https://go.dev/ref/spec#Close)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-per-request-audit-dispatch-capture.md](04-per-request-audit-dispatch-capture.md) | Next: [06-safego-panic-recovery-supervisor.md](06-safego-panic-recovery-supervisor.md)
