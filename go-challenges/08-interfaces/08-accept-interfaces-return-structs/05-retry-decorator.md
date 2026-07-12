# Exercise 5: Add A Retry/Backoff Decorator Over A Flaky Repository

The same accept-interface / return-struct shape that lets a cache wrap a store lets a
retry policy wrap it too. This module builds a `RetryRepository` that retries
*transient* failures with bounded attempts and backoff, returns *permanent* failures
immediately, and aborts the moment the caller's context is cancelled.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports any other
exercise.

## What you'll build

```text
retrydecorator/             independent module: example.com/retrydecorator
  go.mod                    go 1.26
  repo.go                   Item, ErrNotFound, ErrTransient; context-aware Repository; MemoryRepository
  retry.go                  RetryRepository: bounded retry with backoff and ctx cancellation
  cmd/
    demo/
      main.go               a flaky backend succeeding on the third attempt
  retry_test.go             attempt-count assertions; permanent-error fast path; cancellation
```

Files: `repo.go`, `retry.go`, `cmd/demo/main.go`, `retry_test.go`.
Implement: a `RetryRepository` wrapping a context-aware `Repository`, retrying on `errors.Is(err, ErrTransient)` up to `maxAttempts` with a backoff, returning permanent errors immediately and `ctx.Err()` on cancellation.
Test: a fake failing N-1 times then succeeding (assert success and attempt count); a permanent error returned without retry; a cancelled context aborting the loop with `ctx.Err()`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/08-interfaces/08-accept-interfaces-return-structs/05-retry-decorator/cmd/demo
cd go-solutions/08-interfaces/08-accept-interfaces-return-structs/05-retry-decorator
go mod edit -go=1.26
```

### Why the interface carries a context, and why the sentinel matters

To retry correctly, the decorator has to know two things the wrapped call cannot tell
it through a bare `error` value alone: whether the failure is worth retrying, and
whether the caller still wants the result. The first is a *classification* problem,
solved with a sentinel: the backend wraps retryable failures with `%w` around
`ErrTransient`, and the decorator branches with `errors.Is(err, ErrTransient)`. A
permanent error (a validation failure, `ErrNotFound`) is *not* wrapped with
`ErrTransient`, so the decorator returns it immediately — retrying it would waste
attempts and latency on an outcome that will never change. The second is a
*cancellation* problem, which is why the `Repository` methods here take a
`context.Context`: the decorator checks `ctx.Err()` before each attempt and selects on
`ctx.Done()` during the backoff, so a caller who has given up aborts the retry loop at
once with `ctx.Err()` instead of burning through every remaining attempt.

The retry lives in one helper, `do`, shared by `Get`/`Put`/`Delete`. It loops up to
`maxAttempts`, returns on success, returns permanent errors and `ctx.Err()`
immediately, and otherwise backs off before the next try. `RetryRepository` accepts
the interface and is the interface, so it slots into a decorator chain exactly like the
cache did.

Create `repo.go`:

```go
package retrydecorator

import (
	"context"
	"errors"
	"sync"
)

var (
	// ErrNotFound is a permanent outcome: retrying will not make the key appear.
	ErrNotFound = errors.New("retrydecorator: item not found")
	// ErrTransient marks a retryable failure. Backends wrap it with %w.
	ErrTransient = errors.New("retrydecorator: transient failure")
)

type Item struct {
	ID    string
	Name  string
	Price int64
}

// Repository carries a context so decorators can honor cancellation.
type Repository interface {
	Get(ctx context.Context, id string) (Item, error)
	Put(ctx context.Context, item Item) error
	Delete(ctx context.Context, id string) error
}

type MemoryRepository struct {
	mu    sync.RWMutex
	items map[string]Item
}

func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{items: make(map[string]Item)}
}

var _ Repository = (*MemoryRepository)(nil)

func (m *MemoryRepository) Get(ctx context.Context, id string) (Item, error) {
	if err := ctx.Err(); err != nil {
		return Item{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	item, ok := m.items[id]
	if !ok {
		return Item{}, ErrNotFound
	}
	return item, nil
}

func (m *MemoryRepository) Put(ctx context.Context, item Item) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[item.ID] = item
	return nil
}

func (m *MemoryRepository) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.items[id]; !ok {
		return ErrNotFound
	}
	delete(m.items, id)
	return nil
}
```

Create `retry.go`:

```go
package retrydecorator

import (
	"context"
	"errors"
	"time"
)

// RetryRepository retries transient failures from the wrapped Repository. It accepts
// a Repository and is a Repository.
type RetryRepository struct {
	next        Repository
	maxAttempts int
	backoff     time.Duration
}

// NewRetryRepository wraps next with a bounded retry policy and returns the struct.
func NewRetryRepository(next Repository, maxAttempts int, backoff time.Duration) *RetryRepository {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	return &RetryRepository{next: next, maxAttempts: maxAttempts, backoff: backoff}
}

var _ Repository = (*RetryRepository)(nil)

// do runs op up to maxAttempts times. It returns on success, returns a permanent
// error or ctx.Err() immediately, and backs off (cancellably) between transient
// failures.
func (r *RetryRepository) do(ctx context.Context, op func() error) error {
	var lastErr error
	for attempt := 1; attempt <= r.maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := op()
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrTransient) {
			return err // permanent: do not retry
		}
		lastErr = err
		if attempt == r.maxAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.backoff):
		}
	}
	return lastErr
}

func (r *RetryRepository) Get(ctx context.Context, id string) (Item, error) {
	var item Item
	err := r.do(ctx, func() error {
		var e error
		item, e = r.next.Get(ctx, id)
		return e
	})
	return item, err
}

func (r *RetryRepository) Put(ctx context.Context, item Item) error {
	return r.do(ctx, func() error { return r.next.Put(ctx, item) })
}

func (r *RetryRepository) Delete(ctx context.Context, id string) error {
	return r.do(ctx, func() error { return r.next.Delete(ctx, id) })
}
```

### The runnable demo

The demo wraps a backend that fails its first two `Get`s with a transient error and
succeeds on the third, then shows the retry decorator returning the value.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/retrydecorator"
)

// flaky fails failCount times, then serves item. Not safe for concurrent use; the
// demo is single-goroutine.
type flaky struct {
	item      retrydecorator.Item
	failCount int
	calls     int
}

func (f *flaky) Get(ctx context.Context, id string) (retrydecorator.Item, error) {
	f.calls++
	if f.calls <= f.failCount {
		return retrydecorator.Item{}, fmt.Errorf("attempt %d: %w", f.calls, retrydecorator.ErrTransient)
	}
	return f.item, nil
}

func (f *flaky) Put(context.Context, retrydecorator.Item) error { return nil }
func (f *flaky) Delete(context.Context, string) error           { return nil }

func main() {
	backend := &flaky{item: retrydecorator.Item{ID: "sku-1", Name: "widget"}, failCount: 2}
	retrying := retrydecorator.NewRetryRepository(backend, 5, time.Millisecond)

	item, err := retrying.Get(context.Background(), "sku-1")
	if err != nil {
		fmt.Println("get:", err)
		return
	}
	fmt.Printf("got %s after %d attempts\n", item.Name, backend.calls)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
got widget after 3 attempts
```

### Tests

`scriptedRepo` records how many `Get` calls it received and fails the first
`failFirst` of them transiently. The tests assert: success after exactly the right
number of attempts; a permanent error returned on the first attempt with no retry; and
a pre-cancelled context aborting before the backend is ever called, returning
`context.Canceled`.

Create `retry_test.go`:

```go
package retrydecorator

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// scriptedRepo fails its first failFirst Get calls transiently, then succeeds.
type scriptedRepo struct {
	item      Item
	failFirst int64
	calls     atomic.Int64
	permanent error // if set, returned instead of a transient failure
}

func (s *scriptedRepo) Get(ctx context.Context, id string) (Item, error) {
	n := s.calls.Add(1)
	if s.permanent != nil {
		return Item{}, s.permanent
	}
	if n <= s.failFirst {
		return Item{}, fmt.Errorf("call %d: %w", n, ErrTransient)
	}
	return s.item, nil
}

func (s *scriptedRepo) Put(context.Context, Item) error { return nil }
func (s *scriptedRepo) Delete(context.Context, string) error {
	return nil
}

func TestRetrySucceedsAfterTransientFailures(t *testing.T) {
	t.Parallel()
	backend := &scriptedRepo{item: Item{ID: "sku-1", Name: "widget"}, failFirst: 2}
	r := NewRetryRepository(backend, 5, time.Millisecond)

	got, err := r.Get(context.Background(), "sku-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "widget" {
		t.Fatalf("Get = %+v, want widget", got)
	}
	if n := backend.calls.Load(); n != 3 {
		t.Fatalf("backend called %d times, want 3 (2 fail + 1 success)", n)
	}
}

func TestRetryExhaustsAndReturnsLastError(t *testing.T) {
	t.Parallel()
	backend := &scriptedRepo{failFirst: 100} // always fails within our attempt budget
	r := NewRetryRepository(backend, 3, time.Millisecond)

	_, err := r.Get(context.Background(), "sku-1")
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("Get err = %v, want ErrTransient after exhaustion", err)
	}
	if n := backend.calls.Load(); n != 3 {
		t.Fatalf("backend called %d times, want exactly maxAttempts=3", n)
	}
}

func TestPermanentErrorIsNotRetried(t *testing.T) {
	t.Parallel()
	backend := &scriptedRepo{permanent: ErrNotFound}
	r := NewRetryRepository(backend, 5, time.Millisecond)

	_, err := r.Get(context.Background(), "sku-1")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get err = %v, want ErrNotFound", err)
	}
	if n := backend.calls.Load(); n != 1 {
		t.Fatalf("backend called %d times, want 1 (no retry on permanent)", n)
	}
}

func TestCancelledContextAbortsRetry(t *testing.T) {
	t.Parallel()
	backend := &scriptedRepo{failFirst: 100}
	r := NewRetryRepository(backend, 5, time.Hour) // long backoff we must never wait on

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	_, err := r.Get(ctx, "sku-1")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Get err = %v, want context.Canceled", err)
	}
	if n := backend.calls.Load(); n != 0 {
		t.Fatalf("backend called %d times, want 0 (aborted before first attempt)", n)
	}
}
```

## Review

The decorator is correct when a transient-then-success sequence returns the value in
exactly the right number of attempts, when a permanent error (`ErrNotFound`) returns
on the first call with no wasted retries, and when a cancelled context aborts before
any further attempt and returns `ctx.Err()`. Those three behaviors are asserted with
an attempt counter, which is the only honest way to test a retry: a happy-path test
that never counts attempts would pass even if the decorator never retried or retried
forever. The two classic bugs this module rules out are retrying a permanent error
(fixed by the `errors.Is(err, ErrTransient)` gate) and spinning through all attempts
after cancellation (fixed by the `ctx.Err()` check and the `select` on `ctx.Done()`).

## Resources

- [`context` package](https://pkg.go.dev/context) — `context.Context`, `Err`, and `Done` used for cancellation.
- [Go blog: Error handling and Go](https://go.dev/blog/error-handling-and-go) — wrapping and inspecting errors, the basis of transient vs permanent classification.
- [`errors.Is` and `%w`](https://pkg.go.dev/errors#Is) — the sentinel-matching mechanism the retry policy branches on.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-caching-decorator.md](04-caching-decorator.md) | Next: [06-typed-nil-interface-trap.md](06-typed-nil-interface-trap.md)
