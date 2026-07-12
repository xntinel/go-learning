# Exercise 8: Batch API: Fan Out Requests, Collect Ordered Replies

A batch endpoint — multi-key cache lookup, bulk store fetch — is request-reply at
scale: one caller submits N keys and expects N results back, aligned to the input
order, with per-item errors for the keys that failed. This exercise builds that on
top of a request-reply store: fan out N sub-requests each with its own reply
channel, collect them preserving index order, and aggregate partial failures with
`errors.Join`, all cancellable through a context.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
batch/                     independent module: example.com/batch
  go.mod
  batch.go                 type Store; Get and BatchGet(ctx, keys) preserving order
  cmd/
    demo/
      main.go              runnable demo: batch of hits and misses
  batch_test.go            ordered-results, partial-failure, cancellation tests
```

- Files: `batch.go`, `cmd/demo/main.go`, `batch_test.go`.
- Implement: an actor `Store` over a map; `BatchGet(ctx, keys)` that fans out one sub-request per key (each with its own reply channel), collects results into a slice indexed by input position, records per-item errors, and returns the aggregate via `errors.Join`; both fan-out and collect select on `ctx.Done()`.
- Test: a mixed batch of hits and misses returns values aligned to input indices with per-item `ErrNotFound` wrapped; the aggregate satisfies `errors.Is(err, ErrNotFound)`; a cancelled context aborts the batch with the context cause.
- Verify: `go test -count=1 -race ./...`

### Fan out, then collect in order

`BatchGet` runs in two phases. In the fan-out phase it loops over the input keys
and, for each, creates a private capacity-one reply channel and sends a sub-request
onto the store's inbox. It keeps the reply channels in a slice indexed by input
position — `replies[i]` belongs to `keys[i]`. In the collect phase it loops the
same indices and receives from `replies[i]`, writing the value into `results[i]`.
Order is preserved not by any ordering guarantee from the store, but because the
caller remembers which reply channel maps to which input slot. The store can
answer sub-requests in any order; the index is the alignment.

Partial failure is per item. A key that misses comes back with `ErrNotFound` in
its response; `BatchGet` records `results[i]` as the zero value and appends a
wrapped error (`fmt.Errorf("key %q: %w", ...)`) to a slice. At the end it returns
`errors.Join(errs...)`, which is `nil` when every key hit and otherwise a single
error that still satisfies `errors.Is(err, ErrNotFound)` for the misses. The
caller gets both the values it *could* fetch and a precise account of what failed —
the shape a real bulk API needs.

Both phases select on `ctx.Done()`. If the context is cancelled during fan-out
(the inbox is full or the store is slow) or during collect (a sub-request is
slow), `BatchGet` abandons and returns `context.Cause(ctx)`. Because each reply
channel is buffered at capacity one, the store's in-flight answers do not leak
even when the batch aborts early.

Create `batch.go`:

```go
package batch

import (
	"context"
	"errors"
	"fmt"
)

// ErrNotFound is returned for a key that is not in the store.
var ErrNotFound = errors.New("batch: key not found")

// ErrShuttingDown is returned once the store has been shut down.
var ErrShuttingDown = errors.New("batch: shutting down")

type request struct {
	key   string
	reply chan response
}

type response struct {
	value int
	err   error
}

// Store is an actor over a read-only map. A single Run goroutine owns the map.
type Store struct {
	data     map[string]int
	requests chan request
	quit     chan struct{}
	done     chan struct{}
}

// New returns a Store over the given data. Start it with go s.Run().
func New(data map[string]int) *Store {
	return &Store{
		data:     data,
		requests: make(chan request),
		quit:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Run is the actor loop: it owns the map and answers one lookup at a time.
func (s *Store) Run() {
	defer close(s.done)
	for {
		select {
		case req := <-s.requests:
			if v, ok := s.data[req.key]; ok {
				req.reply <- response{value: v}
			} else {
				req.reply <- response{err: ErrNotFound}
			}
		case <-s.quit:
			return
		}
	}
}

// Shutdown stops the store and waits for the loop to exit.
func (s *Store) Shutdown() {
	close(s.quit)
	<-s.done
}

// Get looks up a single key.
func (s *Store) Get(ctx context.Context, key string) (int, error) {
	reply := make(chan response, 1)
	select {
	case s.requests <- request{key: key, reply: reply}:
	case <-ctx.Done():
		return 0, context.Cause(ctx)
	case <-s.quit:
		return 0, ErrShuttingDown
	}
	select {
	case resp := <-reply:
		return resp.value, resp.err
	case <-ctx.Done():
		return 0, context.Cause(ctx)
	case <-s.quit:
		return 0, ErrShuttingDown
	}
}

// BatchGet looks up all keys, returning values aligned to the input order and an
// aggregate error (via errors.Join) carrying every per-key failure.
func (s *Store) BatchGet(ctx context.Context, keys []string) ([]int, error) {
	results := make([]int, len(keys))
	replies := make([]chan response, len(keys))

	// Fan out: one sub-request per key.
	for i, key := range keys {
		reply := make(chan response, 1)
		replies[i] = reply
		select {
		case s.requests <- request{key: key, reply: reply}:
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		case <-s.quit:
			return nil, ErrShuttingDown
		}
	}

	// Collect in input order, recording per-item errors.
	var errs []error
	for i := range keys {
		select {
		case resp := <-replies[i]:
			results[i] = resp.value
			if resp.err != nil {
				errs = append(errs, fmt.Errorf("key %q: %w", keys[i], resp.err))
			}
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		case <-s.quit:
			return nil, ErrShuttingDown
		}
	}
	return results, errors.Join(errs...)
}
```

### The runnable demo

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
	s := batch.New(map[string]int{"a": 1, "b": 2, "c": 3})
	go s.Run()
	defer s.Shutdown()

	keys := []string{"a", "x", "c"}
	results, err := s.BatchGet(context.Background(), keys)
	fmt.Println("results:", results)
	if errors.Is(err, batch.ErrNotFound) {
		fmt.Println("had a miss:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
results: [1 0 3]
had a miss: key "x": batch: key not found
```

### Tests

`TestBatchGetOrdered` submits a mixed batch and asserts values land at the right
indices, misses carry the wrapped `ErrNotFound`, and the aggregate satisfies
`errors.Is`. `TestBatchGetAllHit` asserts a nil aggregate when nothing fails.
`TestBatchGetCanceled` cancels the context before calling, with the store's worker
not started so the fan-out send blocks, and asserts the batch aborts with the
context cause.

Create `batch_test.go`:

```go
package batch

import (
	"context"
	"errors"
	"slices"
	"testing"
)

func TestBatchGetOrdered(t *testing.T) {
	t.Parallel()
	s := New(map[string]int{"a": 1, "b": 2, "c": 3})
	go s.Run()
	defer s.Shutdown()

	keys := []string{"a", "x", "b", "y", "c"}
	results, err := s.BatchGet(t.Context(), keys)

	want := []int{1, 0, 2, 0, 3}
	if !slices.Equal(results, want) {
		t.Fatalf("results = %v, want %v", results, want)
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("aggregate error = %v, want to satisfy ErrNotFound", err)
	}
}

func TestBatchGetAllHit(t *testing.T) {
	t.Parallel()
	s := New(map[string]int{"a": 1, "b": 2})
	go s.Run()
	defer s.Shutdown()

	results, err := s.BatchGet(t.Context(), []string{"b", "a", "b"})
	if err != nil {
		t.Fatalf("error = %v, want nil", err)
	}
	if want := []int{2, 1, 2}; !slices.Equal(results, want) {
		t.Fatalf("results = %v, want %v", results, want)
	}
}

func TestBatchGetCanceled(t *testing.T) {
	t.Parallel()
	s := New(map[string]int{"a": 1})
	// Worker deliberately not started, so the fan-out send blocks and the
	// cancelled context wins.
	defer close(s.quit)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := s.BatchGet(ctx, []string{"a", "b"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}
```

`TestBatchGetCanceled` closes `s.quit` directly instead of calling `Shutdown`,
because `Shutdown` also waits on the `done` channel that only `Run` closes — and
`Run` was never started here.

## Review

`BatchGet` is correct when its output slice is a positional image of its input:
`results[i]` is the value for `keys[i]` or the zero value if that key failed, and
the failures are reported per item and aggregated. `TestBatchGetOrdered` pins that
with a deliberately interleaved batch of hits and misses; because collection reads
`replies[i]` in index order, the store's answer order is irrelevant. `errors.Join`
gives the caller one aggregate error that still matches `ErrNotFound` via
`errors.Is`, so both coarse and per-key handling work.

The mistakes to avoid: do not collect by receiving from a shared channel in
completion order — you lose the input alignment; keep one reply channel per index.
Do not forget the `ctx.Done()` arm in both phases, or a slow or stalled store hangs
the whole batch past the caller's deadline. And keep each reply channel buffered
so an aborted batch does not leak the store's in-flight sends. Run `go test -race`
to confirm the fan-out and collect are race-free.

## Resources

- [`errors.Join`](https://pkg.go.dev/errors#Join) — aggregating multiple errors into one that `errors.Is` can still match.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — matching a wrapped sentinel across a joined error.
- [`context.Cause`](https://pkg.go.dev/context#Cause) — the cancellation reason returned on abort.
- [`slices.Equal`](https://pkg.go.dev/slices#Equal) — comparing the ordered result slice in tests.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-actor-self-observability.md](09-actor-self-observability.md)
