# Exercise 25: Kafka Offset Commit Checkpoint

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Exactly-once processing in a consumer group hinges on offset commits being
monotonic and durably verified — a commit that silently regresses causes
reprocessing, and a commit that reports success without actually landing
causes message loss on the next rebalance. This exercise builds
`Coordinator.CommitOffset(partition, offset) (committedOffset int64,
committed bool, error)`, rejecting regressive commits, treating duplicate
commits as idempotent, and reading back every write to verify it actually
persisted before reporting success.

This module is fully self-contained: its own `go mod init`, all code
inline, its own demo and tests.

## What you'll build

```text
offsetcommit/                 independent module: example.com/kafka-offset-commit-verify
  go.mod                      go 1.24
  offsetcommit.go               package offsetcommit; offsetStore; Coordinator; CommitOffset(partition,offset) (committedOffset,committed,error)
  cmd/
    demo/
      main.go                   commit, duplicate commit, stale commit, advance, forced broker failure
  offsetcommit_test.go           full sequence; store-failure case; independent partitions; concurrent monotonic commits (-race)
```

- Files: `offsetcommit.go`, `cmd/demo/main.go`, `offsetcommit_test.go`.
- Implement: `(*Coordinator).CommitOffset(partition int32, offset int64) (committedOffset int64, committed bool, err error)`, checking the current durable offset per partition under a mutex, rejecting a lower offset, treating an equal offset as an idempotent no-op, and verifying every new write with a read-back before reporting `committed == true`.
- Test: a monotonic sequence of commits succeeds in order; a duplicate commit is reported as already committed with no error; a stale (regressive) commit is rejected with `committed == false, err == nil`; a forced store failure returns a non-nil error and leaves the durable offset unchanged; concurrent commits to the same partition are race-free and converge on the highest offset attempted.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/01-function-declaration-and-multiple-return-values/25-kafka-offset-commit-verify/cmd/demo
cd go-solutions/04-functions/01-function-declaration-and-multiple-return-values/25-kafka-offset-commit-verify
go mod edit -go=1.24
```

### Four outcomes, one lock, no daylight between check and act

A commit request against a real broker can land in one of four states, and
conflating any two of them breaks exactly-once semantics somewhere
downstream:

- **regressive** (`offset < current`): a straggler consumer, still running
  against a stale assignment after a rebalance, tries to commit behind
  where the group has already progressed. This is not an error — it is an
  entirely ordinary race in consumer-group protocols — so it is rejected
  quietly: `(current, false, nil)`.
- **duplicate** (`offset == current`): a retry of a commit whose response
  was lost, not a new commit. Reporting this as an error would make a
  network hiccup on the *acknowledgment* look like a real failure, so it
  is treated as an idempotent success: `(current, true, nil)`.
- **write failure**: the broker is actually unreachable. `(current, false,
  err)` — the durable offset is provably unchanged, because the write
  never happened.
- **write succeeds but verification fails**: the store reports success but
  a read-back disagrees. This should not happen in a correct store, but
  wiring the check in anyway is what "verify" means in this exercise's
  title — a coordinator that trusts a bare `nil` from a write call without
  reading it back is trusting the store's word for something it could
  independently confirm.

All four decisions happen under one `sync.Mutex` held for the whole call:

```go
c.mu.Lock()
defer c.mu.Unlock()

current, exists := c.store.read(partition)
if exists && offset < current {
	return current, false, nil
}
if exists && offset == current {
	return current, true, nil
}
if err := c.store.write(partition, offset); err != nil {
	return current, false, fmt.Errorf("commit partition %d offset %d: %w", partition, offset, err)
}
verified, ok := c.store.read(partition)
if !ok || verified != offset {
	return current, false, fmt.Errorf("commit partition %d offset %d: verification mismatch, store has %v (ok=%t)", partition, offset, verified, ok)
}
return verified, true, nil
```

Splitting the read-current-offset check from the write across two lock
acquisitions would let two concurrent commits for the same partition both
read the same `current`, both decide they are advancing it, and both
write — the second write silently overwriting information the first commit
thought it had durably recorded.

Create `offsetcommit.go`:

```go
package offsetcommit

import (
	"fmt"
	"sync"
)

// offsetStore simulates the durable broker-side offset log. failNext, when
// set, makes the next write fail, standing in for a broker timeout or a
// lost leader election.
type offsetStore struct {
	data     map[int32]int64
	failNext error
}

func newOffsetStore() *offsetStore {
	return &offsetStore{data: make(map[int32]int64)}
}

func (s *offsetStore) write(partition int32, offset int64) error {
	if s.failNext != nil {
		err := s.failNext
		s.failNext = nil
		return err
	}
	s.data[partition] = offset
	return nil
}

func (s *offsetStore) read(partition int32) (int64, bool) {
	v, ok := s.data[partition]
	return v, ok
}

// Coordinator commits consumer-group offsets with exactly-once semantics:
// commits must be monotonic per partition, a commit exactly repeating the
// current offset is treated as an idempotent no-op (not an error), and
// every successful write is verified by reading it back before being
// reported as committed. It is safe for concurrent use.
type Coordinator struct {
	mu    sync.Mutex
	store *offsetStore
}

func NewCoordinator() *Coordinator {
	return &Coordinator{store: newOffsetStore()}
}

// FailNextWith forces the next store write to fail with err, simulating a
// broker outage on the next CommitOffset call that needs to write.
func (c *Coordinator) FailNextWith(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.store.failNext = err
}

// CommitOffset commits offset for partition and reports the partition's
// resulting durable offset, whether this call's offset is now committed,
// and any operational error. The check (is this offset valid to commit,
// given what is already durable) and the act (write it, then verify it
// really landed) happen under one lock, so two racing commits for the
// same partition can never both believe they made progress.
//
//   - regressive commit (offset < current durable offset): rejected, not
//     an error -- (current, false, nil). A straggler consumer after a
//     rebalance hits this constantly; it is expected, not exceptional.
//   - duplicate commit (offset == current durable offset): idempotent,
//     reported as already committed -- (offset, true, nil).
//   - new commit that the store fails to persist: (current, false, err).
//   - new commit that persists but reads back wrong: (current, false, err)
//     -- this should never happen in a correct store; the exercise wires
//     verification in anyway, the way a strict consumer group would.
//   - new commit that persists and verifies: (offset, true, nil).
func (c *Coordinator) CommitOffset(partition int32, offset int64) (committedOffset int64, committed bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	current, exists := c.store.read(partition)

	if exists && offset < current {
		return current, false, nil
	}
	if exists && offset == current {
		return current, true, nil
	}

	if err := c.store.write(partition, offset); err != nil {
		return current, false, fmt.Errorf("commit partition %d offset %d: %w", partition, offset, err)
	}

	verified, ok := c.store.read(partition)
	if !ok || verified != offset {
		return current, false, fmt.Errorf("commit partition %d offset %d: verification mismatch, store has %v (ok=%t)", partition, offset, verified, ok)
	}

	return verified, true, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/kafka-offset-commit-verify"
)

func main() {
	coord := offsetcommit.NewCoordinator()

	offset, committed, err := coord.CommitOffset(0, 100)
	fmt.Printf("commit 100:        offset=%d committed=%t err=%v\n", offset, committed, err)

	offset, committed, err = coord.CommitOffset(0, 100) // duplicate/retry
	fmt.Printf("re-commit 100:     offset=%d committed=%t err=%v\n", offset, committed, err)

	offset, committed, err = coord.CommitOffset(0, 50) // stale, from a lagging consumer
	fmt.Printf("stale commit 50:   offset=%d committed=%t err=%v\n", offset, committed, err)

	offset, committed, err = coord.CommitOffset(0, 150)
	fmt.Printf("advance to 150:    offset=%d committed=%t err=%v\n", offset, committed, err)

	coord.FailNextWith(errors.New("broker unavailable"))
	offset, committed, err = coord.CommitOffset(0, 200)
	fmt.Printf("broker down:       offset=%d committed=%t err=%v\n", offset, committed, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
commit 100:        offset=100 committed=true err=<nil>
re-commit 100:     offset=100 committed=true err=<nil>
stale commit 50:   offset=100 committed=false err=<nil>
advance to 150:    offset=150 committed=true err=<nil>
broker down:       offset=150 committed=false err=commit partition 0 offset 200: broker unavailable
```

### Tests

Create `offsetcommit_test.go`:

```go
package offsetcommit

import (
	"errors"
	"sync"
	"testing"
)

func TestCommitOffsetSequence(t *testing.T) {
	t.Parallel()
	coord := NewCoordinator()

	offset, committed, err := coord.CommitOffset(0, 100)
	if err != nil || !committed || offset != 100 {
		t.Fatalf("first commit: offset=%d committed=%t err=%v, want 100/true/nil", offset, committed, err)
	}

	offset, committed, err = coord.CommitOffset(0, 100)
	if err != nil || !committed || offset != 100 {
		t.Fatalf("duplicate commit: offset=%d committed=%t err=%v, want 100/true/nil", offset, committed, err)
	}

	offset, committed, err = coord.CommitOffset(0, 50)
	if err != nil || committed || offset != 100 {
		t.Fatalf("stale commit: offset=%d committed=%t err=%v, want 100/false/nil", offset, committed, err)
	}

	offset, committed, err = coord.CommitOffset(0, 150)
	if err != nil || !committed || offset != 150 {
		t.Fatalf("advancing commit: offset=%d committed=%t err=%v, want 150/true/nil", offset, committed, err)
	}
}

func TestCommitOffsetStoreFailureIsNotCommitted(t *testing.T) {
	t.Parallel()
	coord := NewCoordinator()
	if _, _, err := coord.CommitOffset(0, 100); err != nil {
		t.Fatalf("setup commit: %v", err)
	}

	coord.FailNextWith(errors.New("broker unavailable"))
	offset, committed, err := coord.CommitOffset(0, 200)
	if err == nil {
		t.Fatal("want an error when the store write fails")
	}
	if committed {
		t.Fatal("committed = true despite a store failure")
	}
	if offset != 100 {
		t.Fatalf("offset = %d, want 100 (the last durable offset, unchanged)", offset)
	}
}

func TestCommitOffsetIndependentPartitions(t *testing.T) {
	t.Parallel()
	coord := NewCoordinator()

	if _, committed, err := coord.CommitOffset(0, 10); err != nil || !committed {
		t.Fatalf("partition 0 commit: committed=%t err=%v", committed, err)
	}
	if _, committed, err := coord.CommitOffset(1, 5); err != nil || !committed {
		t.Fatalf("partition 1 commit: committed=%t err=%v", committed, err)
	}
	// A low offset on partition 1 must not be judged against partition 0's state.
	offset, committed, err := coord.CommitOffset(1, 5)
	if err != nil || !committed || offset != 5 {
		t.Fatalf("partition 1 re-commit: offset=%d committed=%t err=%v, want 5/true/nil", offset, committed, err)
	}
}

func TestCommitOffsetConcurrentIsMonotonicAndRaceFree(t *testing.T) {
	t.Parallel()
	coord := NewCoordinator()

	const highest = int64(99)
	var wg sync.WaitGroup
	for i := int64(0); i <= highest; i++ {
		wg.Add(1)
		go func(offset int64) {
			defer wg.Done()
			if _, _, err := coord.CommitOffset(0, offset); err != nil {
				t.Errorf("commit offset %d: %v", offset, err)
			}
		}(i)
	}
	wg.Wait()

	// Whatever order the goroutines ran in, the monotonic check guarantees
	// the highest attempted offset ends up durable: it can never be
	// rejected as stale (nothing higher was ever attempted), so it always
	// gets written, either before or after every lower one.
	finalOffset, committed, err := coord.CommitOffset(0, highest)
	if err != nil || !committed || finalOffset != highest {
		t.Fatalf("final state: offset=%d committed=%t err=%v, want %d/true/nil", finalOffset, committed, err, highest)
	}
}
```

## Review

`CommitOffset` is correct when the four outcomes — regressive, duplicate,
failed write, verified success — never blur into each other, and when the
per-partition durable offset only ever moves forward. `TestCommitOffsetSequence`
walks all three non-error outcomes in the order a real consumer would hit
them; `TestCommitOffsetConcurrentIsMonotonicAndRaceFree` is the load-bearing
concurrency test — it proves the monotonic invariant holds regardless of
goroutine scheduling, not just under a convenient ordering, and `-race`
proves the read-then-write in `CommitOffset` never splits across two lock
acquisitions.

The mistake to avoid is treating a regressive commit as an error rather
than a quiet rejection — a consumer group under constant rebalancing hits
that path routinely, and surfacing it as an `error` would flood logs and
alerting with noise for a condition the protocol expects to happen.
Equally, skipping the read-back verification after `store.write` succeeds
would make `committed == true` mean "the store's write call returned nil",
not "this offset is actually durable" — a distinction that matters
precisely when the store is the thing most likely to be lying.

## Resources

- [Kafka: Consumer offset management](https://kafka.apache.org/documentation/#impl_offsettracking) — how consumer groups track and commit per-partition offsets.
- [Kafka: Exactly-once semantics](https://kafka.apache.org/documentation/#semantics) — the delivery-guarantee vocabulary (at-least-once, exactly-once) this exercise's monotonic-commit check supports.
- [sync.Mutex](https://pkg.go.dev/sync#Mutex) — guarding the read-check-write-verify sequence as one atomic critical section.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [24-feature-flag-resolve-audited.md](24-feature-flag-resolve-audited.md) | Next: [26-json-number-safe-coerce.md](26-json-number-safe-coerce.md)
