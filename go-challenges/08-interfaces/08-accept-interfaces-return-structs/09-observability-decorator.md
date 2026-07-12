# Exercise 9: Instrument A Repository With A Metrics/Logging Decorator

The last layer of the stack is observability. An `ObservedRepository` wraps any
`Repository`, records call counts and latency, emits a structured `slog` line per call,
and satisfies the same interface — so it drops onto the composition chain built across
the chapter: `Observed(Retry(Cache(Memory)))`, each layer a `Repository`.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports any other
exercise.

## What you'll build

```text
observedecorator/           independent module: example.com/observedecorator
  go.mod                    go 1.26
  repo.go                   Item, ErrNotFound, ErrTransient; context Repository; MemoryRepository
  cache.go                  CachingRepository (a Repository)
  retry.go                  RetryRepository (a Repository)
  observe.go                ObservedRepository: slog + atomic counters (a Repository)
  cmd/
    demo/
      main.go               assembles Observed(Retry(Cache(Memory))) and reports counts
  observe_test.go           log line has op + duration; counters increment; errors at error level; stack compiles
```

Files: `repo.go`, `cache.go`, `retry.go`, `observe.go`, `cmd/demo/main.go`, `observe_test.go`.
Implement: an `ObservedRepository` wrapping a `Repository`, logging each call via `slog` with an `op` and a `dur` attribute and counting calls with `atomic.Int64`; plus cache and retry layers so the full stack exists.
Test: a `bytes.Buffer`-backed `slog` handler; assert a `Get` emits a line with `op=get` and a `dur=` attribute; assert counters increment; assert an error call logs at error level; assert `Observed(Retry(Cache(Memory)))` compiles via compile-time assertions.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/08-interfaces/08-accept-interfaces-return-structs/09-observability-decorator/cmd/demo
cd go-solutions/08-interfaces/08-accept-interfaces-return-structs/09-observability-decorator
go mod edit -go=1.26
```

### Why observability is just another decorator

`ObservedRepository` accepts a `Repository` and is a `Repository`, the same shape as the
cache and the retry layers. That is why it composes with them in any order without a
call-site change. It takes an injected `*slog.Logger` — accept the interface's logging
equivalent — so a test can point it at a `bytes.Buffer` and assert exactly what was
logged, while production points it at stdout or a log pipeline. Each method records a
start time, delegates to `next`, increments an `atomic.Int64` counter, and logs one
structured line carrying the operation name and the measured duration; a failing call
logs at error level with the error attached, a successful one at info level.

Ordering in the stack is a real decision, and this module makes it concrete.
`Observed(Retry(Cache(Memory)))` measures the *whole* stack: an observed `Get` that hits
the cache records a tiny duration and never reaches retry or the backend, so cache hits
are visible in the metrics as fast calls. Put observability innermost instead and it
would measure only the backend, missing the cache entirely. There is no single correct
order — the point is that because every layer is a struct that accepts and satisfies
`Repository`, you choose the order at construction and the compiler proves the chain is
well typed via the `var _ Repository = (*T)(nil)` assertions on each layer.

Create `repo.go`:

```go
package observedecorator

import (
	"context"
	"errors"
	"sync"
)

var (
	ErrNotFound  = errors.New("observedecorator: item not found")
	ErrTransient = errors.New("observedecorator: transient failure")
)

type Item struct {
	ID    string
	Name  string
	Price int64
}

// Repository is the context-aware port every layer implements.
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

Create `cache.go`:

```go
package observedecorator

import (
	"context"
	"sync"
)

// CachingRepository is a read-through cache. It accepts and satisfies Repository.
type CachingRepository struct {
	next  Repository
	mu    sync.RWMutex
	cache map[string]Item
}

func NewCachingRepository(next Repository) *CachingRepository {
	return &CachingRepository{next: next, cache: make(map[string]Item)}
}

var _ Repository = (*CachingRepository)(nil)

func (c *CachingRepository) Get(ctx context.Context, id string) (Item, error) {
	c.mu.RLock()
	item, ok := c.cache[id]
	c.mu.RUnlock()
	if ok {
		return item, nil
	}
	item, err := c.next.Get(ctx, id)
	if err != nil {
		return Item{}, err
	}
	c.mu.Lock()
	c.cache[id] = item
	c.mu.Unlock()
	return item, nil
}

func (c *CachingRepository) Put(ctx context.Context, item Item) error {
	if err := c.next.Put(ctx, item); err != nil {
		return err
	}
	c.mu.Lock()
	c.cache[item.ID] = item
	c.mu.Unlock()
	return nil
}

func (c *CachingRepository) Delete(ctx context.Context, id string) error {
	err := c.next.Delete(ctx, id)
	c.mu.Lock()
	delete(c.cache, id)
	c.mu.Unlock()
	return err
}
```

Create `retry.go`:

```go
package observedecorator

import (
	"context"
	"errors"
	"time"
)

// RetryRepository retries transient failures. It accepts and satisfies Repository.
type RetryRepository struct {
	next        Repository
	maxAttempts int
	backoff     time.Duration
}

func NewRetryRepository(next Repository, maxAttempts int, backoff time.Duration) *RetryRepository {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	return &RetryRepository{next: next, maxAttempts: maxAttempts, backoff: backoff}
}

var _ Repository = (*RetryRepository)(nil)

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
			return err
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

Create `observe.go`:

```go
package observedecorator

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// ObservedRepository records call counts and latency and logs each call. It accepts
// and satisfies Repository, so it drops onto the composition chain.
type ObservedRepository struct {
	next Repository
	log  *slog.Logger

	gets    atomic.Int64
	puts    atomic.Int64
	deletes atomic.Int64
}

// NewObservedRepository wraps next with instrumentation and returns the struct.
func NewObservedRepository(next Repository, log *slog.Logger) *ObservedRepository {
	return &ObservedRepository{next: next, log: log}
}

var _ Repository = (*ObservedRepository)(nil)

func (o *ObservedRepository) Gets() int64 { return o.gets.Load() }

func (o *ObservedRepository) Puts() int64 { return o.puts.Load() }

func (o *ObservedRepository) Deletes() int64 { return o.deletes.Load() }

// record emits one structured line: error level with the error attached on failure,
// info level with the duration on success.
func (o *ObservedRepository) record(ctx context.Context, op, id string, start time.Time, err error) {
	attrs := []any{
		slog.String("op", op),
		slog.String("id", id),
		slog.Duration("dur", time.Since(start)),
	}
	if err != nil {
		o.log.LogAttrs(ctx, slog.LevelError, "repository call failed",
			slog.String("op", op), slog.String("id", id),
			slog.Duration("dur", time.Since(start)), slog.String("err", err.Error()))
		return
	}
	o.log.Info("repository call", attrs...)
}

func (o *ObservedRepository) Get(ctx context.Context, id string) (Item, error) {
	start := time.Now()
	o.gets.Add(1)
	item, err := o.next.Get(ctx, id)
	o.record(ctx, "get", id, start, err)
	return item, err
}

func (o *ObservedRepository) Put(ctx context.Context, item Item) error {
	start := time.Now()
	o.puts.Add(1)
	err := o.next.Put(ctx, item)
	o.record(ctx, "put", item.ID, start, err)
	return err
}

func (o *ObservedRepository) Delete(ctx context.Context, id string) error {
	start := time.Now()
	o.deletes.Add(1)
	err := o.next.Delete(ctx, id)
	o.record(ctx, "delete", id, start, err)
	return err
}
```

### The runnable demo

The demo assembles the full stack, sends logs to `io.Discard` (so output stays
deterministic), performs a write and two reads of the same key — the second served from
cache — and prints the call counts the outer observability layer recorded.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"example.com/observedecorator"
)

func main() {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	stack := observedecorator.NewObservedRepository(
		observedecorator.NewRetryRepository(
			observedecorator.NewCachingRepository(
				observedecorator.NewMemoryRepository(),
			), 3, time.Millisecond,
		), log,
	)

	ctx := context.Background()
	_ = stack.Put(ctx, observedecorator.Item{ID: "sku-1", Name: "widget", Price: 1299})
	_, _ = stack.Get(ctx, "sku-1") // backend
	_, _ = stack.Get(ctx, "sku-1") // cache hit

	fmt.Printf("gets=%d puts=%d deletes=%d\n", stack.Gets(), stack.Puts(), stack.Deletes())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
gets=2 puts=1 deletes=0
```

### Tests

The tests point the logger at a `bytes.Buffer` and read back what was emitted. One
asserts a successful `Get` logs a line containing `op=get` and a `dur=` attribute; one
asserts the counters increment per call; one asserts a failing call logs at
`level=ERROR`. A compile-time block proves the full `Observed(Retry(Cache(Memory)))`
stack type-checks, layer by layer.

Create `observe_test.go`:

```go
package observedecorator

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// failingRepo always fails Get, to exercise the error-level log path.
type failingRepo struct{}

func (failingRepo) Get(context.Context, string) (Item, error) { return Item{}, ErrNotFound }
func (failingRepo) Put(context.Context, Item) error           { return nil }
func (failingRepo) Delete(context.Context, string) error      { return nil }

func newBufLogger() (*bytes.Buffer, *slog.Logger) {
	var buf bytes.Buffer
	return &buf, slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func TestObservedGetLogsOpAndDuration(t *testing.T) {
	t.Parallel()
	buf, log := newBufLogger()
	backend := NewMemoryRepository()
	_ = backend.Put(context.Background(), Item{ID: "sku-1", Name: "widget"})
	obs := NewObservedRepository(backend, log)

	if _, err := obs.Get(context.Background(), "sku-1"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	line := buf.String()
	if !strings.Contains(line, "op=get") {
		t.Fatalf("log = %q, want op=get", line)
	}
	if !strings.Contains(line, "dur=") {
		t.Fatalf("log = %q, want a dur= attribute", line)
	}
}

func TestObservedCountersIncrement(t *testing.T) {
	t.Parallel()
	_, log := newBufLogger()
	obs := NewObservedRepository(NewMemoryRepository(), log)
	ctx := context.Background()

	_ = obs.Put(ctx, Item{ID: "a"})
	_, _ = obs.Get(ctx, "a")
	_, _ = obs.Get(ctx, "a")
	_ = obs.Delete(ctx, "a")

	if obs.Gets() != 2 || obs.Puts() != 1 || obs.Deletes() != 1 {
		t.Fatalf("counts gets=%d puts=%d deletes=%d, want 2/1/1",
			obs.Gets(), obs.Puts(), obs.Deletes())
	}
}

func TestObservedErrorLogsAtErrorLevel(t *testing.T) {
	t.Parallel()
	buf, log := newBufLogger()
	obs := NewObservedRepository(failingRepo{}, log)

	if _, err := obs.Get(context.Background(), "missing"); err == nil {
		t.Fatal("expected error from failingRepo")
	}
	line := buf.String()
	if !strings.Contains(line, "level=ERROR") {
		t.Fatalf("log = %q, want level=ERROR", line)
	}
	if !strings.Contains(line, "err=") {
		t.Fatalf("log = %q, want err= attribute", line)
	}
}

// TestFullStackComposes proves each layer satisfies Repository so the chain type-checks.
func TestFullStackComposes(t *testing.T) {
	t.Parallel()
	_, log := newBufLogger()
	var stack Repository = NewObservedRepository(
		NewRetryRepository(
			NewCachingRepository(NewMemoryRepository()), 3, time.Millisecond,
		), log,
	)
	ctx := context.Background()
	if err := stack.Put(ctx, Item{ID: "sku-1", Name: "widget"}); err != nil {
		t.Fatalf("Put through stack: %v", err)
	}
	got, err := stack.Get(ctx, "sku-1")
	if err != nil || got.Name != "widget" {
		t.Fatalf("Get through stack = %+v, %v", got, err)
	}
}

// compile-time proof that every layer satisfies the same port.
var (
	_ Repository = (*MemoryRepository)(nil)
	_ Repository = (*CachingRepository)(nil)
	_ Repository = (*RetryRepository)(nil)
	_ Repository = (*ObservedRepository)(nil)
)
```

## Review

The decorator is correct when a `Get` emits one structured line carrying `op=get` and a
`dur=` measurement, the counters advance exactly once per call, and a failing call logs
at `level=ERROR` with the error attached — all read back from a `bytes.Buffer` handler,
so the observability is asserted, not assumed. The capstone check is
`TestFullStackComposes` plus the compile-time assertions: because every layer accepts a
`Repository` and is a `Repository`, `Observed(Retry(Cache(Memory)))` type-checks and
runs, and the ordering — observability outermost, so it times cache hits too — is a
construction-time choice, not a rewrite. That is the whole chapter in one wiring
expression: cross-cutting concerns are layers you stack, because the constructor accepts
the interface and returns the struct.

## Resources

- [`log/slog`](https://pkg.go.dev/log/slog) — structured logging: `slog.New`, `NewTextHandler`, `Logger.LogAttrs`, and attributes.
- [The Go Blog: Structured Logging with slog](https://go.dev/blog/slog) — attributes, levels, and handlers.
- [`sync/atomic`](https://pkg.go.dev/sync/atomic#Int64) — `atomic.Int64` for race-free counters under concurrent calls.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-config-loader-io-reader.md](08-config-loader-io-reader.md) | Next: [../09-interface-internals/00-concepts.md](../09-interface-internals/00-concepts.md)
