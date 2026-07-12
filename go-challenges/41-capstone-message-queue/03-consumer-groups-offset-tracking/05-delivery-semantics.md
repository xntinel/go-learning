# Exercise 5: At-Least-Once vs At-Most-Once Delivery

The order of two non-atomic steps - process the message, commit the offset - is the entire difference between losing a message and processing it twice. This exercise builds a tiny consumer that can crash in the window between those steps, runs it both ways, and demonstrates with a counting processor that commit-before-process loses the message while commit-after-process redelivers it.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
delivery.go          Mode (AtMostOnce/AtLeastOnce); Log; OffsetStore; Processor; Consume
cmd/
  demo/
    main.go          crash at offset 2 + restart under each mode; print per-offset process counts
delivery_test.go     lost-on-crash, redelivered-on-crash, clean run, resume-from-committed
```

- Files: `delivery.go`, `cmd/demo/main.go`, `delivery_test.go`.
- Implement: `Consume` with the two-step ordering selected by `Mode`, a crash injection point, the `Log`, `OffsetStore`, and counting `Processor`.
- Test: a crash under `AtMostOnce` loses the message (process count 0), the same crash under `AtLeastOnce` redelivers it (process count 2), a clean run processes each message exactly once, and a consumer resumes from its committed offset.
- Verify: `go test -race ./...`

### The vulnerable window between process and commit

Processing a message and recording that you processed it are two separate writes to two separate places - the side effect goes to your application, the offset goes to the offset store - and nothing makes them atomic. A crash can fall in the window between them, and which step you put first decides what that crash costs.

Put the commit first (`AtMostOnce`). The consumer records "offset 5 done" and then crashes before actually doing the work. On restart it fetches the committed offset, 5, and resumes at 6 - message 5 is never processed. No duplicate, but a silent loss. This is the right choice only when a dropped message is cheaper than a duplicate, such as high-rate metrics where one missing sample does not matter.

Put the processing first (`AtLeastOnce`). The consumer does the work for message 5 and then crashes before committing. On restart it fetches the committed offset - still 4, because the commit for 5 never happened - and resumes at 5, reprocessing it. Every message is handled at least once, at the cost of a duplicate on exactly the messages caught in the crash window. This is the default for almost everything, because the duplicate can be made harmless with idempotent processing (deduplicate by offset, or make the side effect naturally idempotent), whereas a lost message often cannot be recovered at all.

The exercise models the crash precisely. `Consume` resumes from `committed + 1`, then for each message performs its two steps in the order set by `Mode`. The crash injection - `crashAt` - aborts the pass after the first step and before the second, which is exactly the vulnerable window. A first pass crashes at offset 2; a second pass with `crashAt = -1` runs cleanly, modeling the restart. A `Processor` that counts how many times each offset is handled is what makes the difference observable: a lost message has count 0, a redelivered message has count 2, and everything else has count 1.

Create `delivery.go`:

```go
package delivery

import (
	"errors"
	"sync"
)

// Mode selects when a consumer commits an offset relative to processing a
// message. The choice is the only difference between the two delivery
// guarantees this package demonstrates.
type Mode int

const (
	// AtMostOnce commits the offset BEFORE processing. A crash in the window
	// between the commit and the processing loses the message: on restart the
	// consumer resumes past an offset it never actually processed.
	AtMostOnce Mode = iota
	// AtLeastOnce commits the offset AFTER processing. A crash in the window
	// between processing and the commit redelivers the message: on restart the
	// consumer resumes from an offset it already processed and processes it
	// again.
	AtLeastOnce
)

// errCrash aborts a consume pass to model a process that died mid-loop.
var errCrash = errors.New("simulated crash")

// Message is one record in a partition log.
type Message struct {
	Offset int64
	Value  string
}

// Log is an ordered, append-only partition log starting at offset 0.
type Log struct {
	msgs []Message
}

// NewLog builds a Log whose i-th message has offset i and the given value.
func NewLog(values ...string) *Log {
	l := &Log{}
	for i, v := range values {
		l.msgs = append(l.msgs, Message{Offset: int64(i), Value: v})
	}
	return l
}

// from returns the messages at offsets >= start.
func (l *Log) from(start int64) []Message {
	if start < 0 {
		start = 0
	}
	if int(start) >= len(l.msgs) {
		return nil
	}
	return l.msgs[start:]
}

// OffsetStore holds one committed offset per partition, with -1 meaning nothing
// committed. It is the durable record that survives a consumer crash.
type OffsetStore struct {
	mu     sync.Mutex
	commit map[int]int64
}

// NewOffsetStore returns an empty offset store.
func NewOffsetStore() *OffsetStore {
	return &OffsetStore{commit: make(map[int]int64)}
}

// Commit records that messages up to and including offset are processed.
func (s *OffsetStore) Commit(partition int, offset int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commit[partition] = offset
}

// Fetch returns the committed offset for partition, or -1 if none.
func (s *OffsetStore) Fetch(partition int) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if off, ok := s.commit[partition]; ok {
		return off
	}
	return -1
}

// Processor counts how many times each offset was processed, so a test can
// distinguish a lost message (count 0) from a redelivered one (count 2).
type Processor struct {
	mu     sync.Mutex
	counts map[int64]int
}

// NewProcessor returns an empty Processor.
func NewProcessor() *Processor {
	return &Processor{counts: make(map[int64]int)}
}

func (p *Processor) handle(m Message) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.counts[m.Offset]++
}

// Count returns how many times the message at offset was processed.
func (p *Processor) Count(offset int64) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.counts[offset]
}

// Consume processes messages on partition starting just after the committed
// offset. For each message it performs two non-atomic steps whose order is set
// by mode. If crashAt >= 0, the pass returns errCrash after the FIRST step of
// the message at that offset and before the second, modeling a crash in the
// vulnerable window. Pass crashAt = -1 for a clean run (used on restart).
func Consume(log *Log, store *OffsetStore, partition int, mode Mode, proc *Processor, crashAt int64) error {
	start := store.Fetch(partition) + 1
	for _, m := range log.from(start) {
		switch mode {
		case AtMostOnce:
			store.Commit(partition, m.Offset) // step 1: commit first
			if crashAt == m.Offset {
				return errCrash // died before processing this offset
			}
			proc.handle(m) // step 2: process
		case AtLeastOnce:
			proc.handle(m) // step 1: process first
			if crashAt == m.Offset {
				return errCrash // died before committing this offset
			}
			store.Commit(partition, m.Offset) // step 2: commit
		}
	}
	return nil
}
```

The two `case` arms of `Consume` differ only in the order of the `Commit` and `handle` calls, with the `crashAt` check wedged between them. That is the whole lesson reduced to two lines of control flow: under `AtMostOnce` the commit happens before the crash check, so a crash leaves the offset advanced past an unprocessed message; under `AtLeastOnce` the processing happens before the crash check, so a crash leaves a processed message uncommitted and therefore redelivered.

### The runnable demo

The demo runs the identical crash-at-offset-2-then-restart scenario under both modes and prints each offset's process count, so the loss and the duplicate sit side by side.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/delivery"
)

func scenario(mode delivery.Mode, label string) {
	log := delivery.NewLog("m0", "m1", "m2", "m3", "m4")
	store := delivery.NewOffsetStore()
	proc := delivery.NewProcessor()

	// First pass crashes in the vulnerable window of offset 2.
	delivery.Consume(log, store, 0, mode, proc, 2)
	// Restart: resume cleanly from the committed offset.
	delivery.Consume(log, store, 0, mode, proc, -1)

	fmt.Printf("%s\n", label)
	fmt.Printf("  committed offset after crash+restart: %d\n", store.Fetch(0))
	for off := int64(0); off < 5; off++ {
		fmt.Printf("  offset %d processed %d time(s)\n", off, proc.Count(off))
	}
}

func main() {
	scenario(delivery.AtMostOnce, "AtMostOnce (commit before process): crash at offset 2 LOSES it")
	fmt.Println()
	scenario(delivery.AtLeastOnce, "AtLeastOnce (commit after process): crash at offset 2 REDELIVERS it")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
AtMostOnce (commit before process): crash at offset 2 LOSES it
  committed offset after crash+restart: 4
  offset 0 processed 1 time(s)
  offset 1 processed 1 time(s)
  offset 2 processed 0 time(s)
  offset 3 processed 1 time(s)
  offset 4 processed 1 time(s)

AtLeastOnce (commit after process): crash at offset 2 REDELIVERS it
  committed offset after crash+restart: 4
  offset 0 processed 1 time(s)
  offset 1 processed 1 time(s)
  offset 2 processed 2 time(s)
  offset 3 processed 1 time(s)
  offset 4 processed 1 time(s)
```

Both runs end with the same committed offset, 4, and the same total of five messages in the log - but offset 2 is processed zero times under `AtMostOnce` and twice under `AtLeastOnce`. Same crash, same restart, opposite outcome, and the only thing that changed was which of the two steps came first.

### Tests

`TestAtMostOnceLosesMessageOnCrash` asserts offset 2 has process count 0 and every other offset count 1 after the crash-and-restart. `TestAtLeastOnceRedeliversMessageOnCrash` asserts offset 2 has count 2. `TestNoCrashProcessesEachMessageExactlyOnce` confirms that without a crash both modes process every message exactly once - the difference only appears at the crash boundary. `TestResumeFromCommittedOffset` confirms a consumer that already committed through offset 2 only sees offsets 3 and 4. All run under `-race`.

Create `delivery_test.go`:

```go
package delivery

import "testing"

// runCrashThenRestart consumes once with a crash at crashAt, then restarts and
// consumes cleanly, returning the processor and the final committed offset.
func runCrashThenRestart(mode Mode, crashAt int64) (*Processor, int64) {
	log := NewLog("m0", "m1", "m2", "m3", "m4")
	store := NewOffsetStore()
	proc := NewProcessor()
	Consume(log, store, 0, mode, proc, crashAt)
	Consume(log, store, 0, mode, proc, -1)
	return proc, store.Fetch(0)
}

func TestAtMostOnceLosesMessageOnCrash(t *testing.T) {
	t.Parallel()

	proc, committed := runCrashThenRestart(AtMostOnce, 2)

	// Offset 2 was committed before processing, then the crash happened. On
	// restart the consumer resumes past it, so it is never processed: LOST.
	if got := proc.Count(2); got != 0 {
		t.Fatalf("AtMostOnce: offset 2 processed %d times, want 0 (lost)", got)
	}
	for _, off := range []int64{0, 1, 3, 4} {
		if got := proc.Count(off); got != 1 {
			t.Fatalf("AtMostOnce: offset %d processed %d times, want exactly 1", off, got)
		}
	}
	if committed != 4 {
		t.Fatalf("AtMostOnce: final committed offset = %d, want 4", committed)
	}
}

func TestAtLeastOnceRedeliversMessageOnCrash(t *testing.T) {
	t.Parallel()

	proc, committed := runCrashThenRestart(AtLeastOnce, 2)

	// Offset 2 was processed, then the crash happened before its commit. On
	// restart the consumer resumes from offset 2 and processes it again:
	// REDELIVERED (processed twice).
	if got := proc.Count(2); got != 2 {
		t.Fatalf("AtLeastOnce: offset 2 processed %d times, want 2 (redelivered)", got)
	}
	for _, off := range []int64{0, 1, 3, 4} {
		if got := proc.Count(off); got != 1 {
			t.Fatalf("AtLeastOnce: offset %d processed %d times, want exactly 1", off, got)
		}
	}
	if committed != 4 {
		t.Fatalf("AtLeastOnce: final committed offset = %d, want 4", committed)
	}
}

func TestNoCrashProcessesEachMessageExactlyOnce(t *testing.T) {
	t.Parallel()

	for _, mode := range []Mode{AtMostOnce, AtLeastOnce} {
		log := NewLog("m0", "m1", "m2", "m3", "m4")
		store := NewOffsetStore()
		proc := NewProcessor()
		if err := Consume(log, store, 0, mode, proc, -1); err != nil {
			t.Fatalf("clean Consume returned error: %v", err)
		}
		for off := int64(0); off < 5; off++ {
			if got := proc.Count(off); got != 1 {
				t.Fatalf("mode %d: offset %d processed %d times, want 1", mode, off, got)
			}
		}
		if got := store.Fetch(0); got != 4 {
			t.Fatalf("mode %d: committed = %d, want 4", mode, got)
		}
	}
}

func TestResumeFromCommittedOffset(t *testing.T) {
	t.Parallel()

	// A consumer that has already committed offset 2 only sees 3 and 4.
	log := NewLog("m0", "m1", "m2", "m3", "m4")
	store := NewOffsetStore()
	store.Commit(0, 2)
	proc := NewProcessor()
	Consume(log, store, 0, AtLeastOnce, proc, -1)

	for _, off := range []int64{0, 1, 2} {
		if got := proc.Count(off); got != 0 {
			t.Fatalf("offset %d should be skipped (already committed), processed %d", off, got)
		}
	}
	for _, off := range []int64{3, 4} {
		if got := proc.Count(off); got != 1 {
			t.Fatalf("offset %d should be processed once, got %d", off, got)
		}
	}
}
```

## Review

The demonstration is sound when the same crash produces opposite outcomes under the two modes: process count 0 for the lost message under `AtMostOnce`, count 2 for the redelivered one under `AtLeastOnce`, and count 1 everywhere else in both. Confirm that with no crash the two modes are indistinguishable - each message processed once - which proves the difference is entirely about the failure window and not about steady-state behavior. The resume test is the linchpin: a consumer must restart from `committed + 1`, so the committed offset is the only state that survives a crash and the only thing that determines what gets reprocessed.

Common mistakes for this feature. The first is committing before processing without realizing it is a choice - calling commit on fetch, before the work is durable - which silently selects at-most-once and loses messages on any crash. The second is assuming at-least-once means exactly-once; it does not, and a consumer that is not idempotent will double-apply the redelivered message (charge the card twice, send the email twice). The third is resuming from the committed offset itself rather than one past it, which reprocesses the last successfully committed message on every clean restart.

## Resources

- [Kafka: Message Delivery Semantics](https://kafka.apache.org/documentation/#semantics) - the canonical description of at-most-once, at-least-once, and exactly-once and how commit ordering selects between them.
- [`errors` package](https://pkg.go.dev/errors) - the sentinel-error pattern used to model the crash abort.
- [Confluent: Exactly-Once Semantics in Apache Kafka](https://www.confluent.io/blog/exactly-once-semantics-are-possible-heres-how-apache-kafka-does-it/) - what it takes to turn at-least-once into exactly-once with transactional commits.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../04-producer-api-batching/00-concepts.md](../04-producer-api-batching/00-concepts.md)
