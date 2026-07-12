# Exercise 1: A Request-Scoped Batch Loader From Scratch

Before reaching for a library, build the mechanism. This exercise implements a
dependency-free, generic dataloader: it collects keys within a short window,
issues one user-supplied batch fetch, distributes results back by position,
deduplicates identical in-flight keys, and caches results for the loader's
lifetime. Building it makes the thunk/window/single-flight model concrete and
lets you reason about the sharp edges — coalescing, per-key errors, batch
capacity, and context cancellation — instead of trusting them.

This module is fully self-contained. It begins with its own `go mod init`,
defines every type it needs, and ships its own demo and tests. Nothing here
imports any other exercise.

## What you'll build

```text
batchloader/                 independent module: example.com/batchloader
  go.mod                     go 1.26
  batchloader.go             Loader[K,V]; New, Load, LoadThunk, LoadAll, Prime, Clear
                             FetchFunc, Option (WithWait, WithBatchCapacity, WithoutCache)
                             ErrorSlice (multi-error Unwrap for errors.Is)
  cmd/
    demo/
      main.go                runnable demo: prime, LoadAll with a duplicate key, Load
  batchloader_test.go        coalescing, alignment, dedup, per-key + whole-batch errors,
                             capacity split, context cancellation, an Example
```

- Files: `batchloader.go`, `cmd/demo/main.go`, `batchloader_test.go`.
- Implement: a generic `Loader[K comparable, V any]` that batches `Load` calls within a time window (or up to a capacity), fans results back by position, deduplicates identical keys via a lifetime cache, and exposes `Load`, `LoadThunk`, `LoadAll`, `Prime`, and `Clear`.
- Test: table-style tests proving M concurrent loads coalesce into one fetch, positional alignment, duplicate-key dedup, per-key vs whole-batch error partitioning via `errors.Is`, capacity splitting into multiple batches, and context-cancellation via the thunk.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/51-rpc-and-api-design/05-graphql-dataloaders-n-plus-1/01-batch-loader-from-scratch/cmd/demo
cd go-solutions/51-rpc-and-api-design/05-graphql-dataloaders-n-plus-1/01-batch-loader-from-scratch
go mod edit -go=1.26
```

### The thunk model, and why Load does not fetch

The central trick is that `Load` does not fetch. It records the key and hands
back a *thunk* — a `func() (V, error)` that, when called, blocks until the batch
this key joined has resolved. Separating "register the key" from "await the
result" is what lets many sibling resolvers register their keys before any of them
blocks: the keys accumulate in one batch, one fetch runs, and every thunk then
returns its slice.

A batch is a growing pair of parallel slices — the keys and one `*result` per
key. A `result` is a value/error pair plus a `done` channel that the batch runner
closes when the fetch completes; a thunk simply waits on that channel. Because the
channel is closed exactly once and read after, the happens-before edge that
`close`/receive gives us means the value is safely published without any extra
lock around the result fields.

### How a batch fills and dispatches

The loader holds at most one open batch. The first `Load` after a batch closes
creates a new one and arms a `time.AfterFunc` timer for the wait window. Every
subsequent `Load` appends to that open batch. The batch dispatches — runs the
fetch once — on whichever comes first:

- the timer fires after `wait`, or
- the batch reaches `maxBatch` keys (when `WithBatchCapacity` is set).

Both paths guard against double-dispatch with a `fired` flag set under the mutex,
and the capacity path stops the timer so it cannot also fire. Dispatch detaches
the batch (`l.batch = nil`) so new keys start a fresh one, then runs the fetch on
its own goroutine so the caller of `Load` never blocks inside the fetch.

### Single-flight and the lifetime cache

The cache maps each key to its `*result`. `LoadThunk` checks it first: a key
already loaded (or in flight) returns the existing result's thunk without touching
any batch. That is single-flight — ten references to user 7 in one request cost
one fetch — and it is also the identity cache that gives the request a consistent
snapshot. `WithoutCache` skips *storing* new results (for mutation-heavy paths),
while `Prime` seeds a value and `Clear` drops one after a write. This cache is the
loader's lifetime cache; scoping that lifetime to a single request is the subject
of Exercise 3.

### The fetch contract and error granularity

`FetchFunc` returns `([]V, []error)`. The values must align positionally with the
keys it received. The errors slice encodes granularity: empty means every key
succeeded; length one means a whole-batch failure applied to every key; otherwise
`errors[i]` is the error for `keys[i]`. `LoadAll` aggregates any per-key errors
into an `ErrorSlice` whose `Unwrap() []error` lets `errors.Is` match a sentinel
wrapped with `%w` inside any element — so one failing key does not poison its
siblings, and callers can still test for a specific cause.

Create `batchloader.go`:

```go
package batchloader

import (
	"context"
	"sync"
	"time"
)

// FetchFunc loads a batch of keys in one call. It must return a values slice
// aligned by position with keys (values[i] is the result for keys[i]). The
// errors slice encodes granularity: empty means all keys succeeded, length one
// is a whole-batch error applied to every key, otherwise errors[i] is the error
// for keys[i].
type FetchFunc[K comparable, V any] func(ctx context.Context, keys []K) ([]V, []error)

// Loader batches and caches calls to a FetchFunc for the lifetime of the loader.
// It is safe for concurrent use and is meant to be constructed once per request.
type Loader[K comparable, V any] struct {
	fetch    FetchFunc[K, V]
	wait     time.Duration
	maxBatch int
	noCache  bool

	mu    sync.Mutex
	cache map[K]*result[V]
	batch *batch[K, V]
}

type result[V any] struct {
	value V
	err   error
	done  chan struct{}
}

type batch[K comparable, V any] struct {
	keys  []K
	reses []*result[V]
	timer *time.Timer
	fired bool
}

type config struct {
	wait     time.Duration
	maxBatch int
	noCache  bool
}

// Option configures a Loader.
type Option func(*config)

// WithWait sets how long a batch buffers keys before it dispatches.
func WithWait(d time.Duration) Option { return func(c *config) { c.wait = d } }

// WithBatchCapacity caps how many keys go into one fetch; extra keys form
// further batches.
func WithBatchCapacity(n int) Option { return func(c *config) { c.maxBatch = n } }

// WithoutCache stops the loader from storing new results, for mutation paths
// that must not read a stale cached value.
func WithoutCache() Option { return func(c *config) { c.noCache = true } }

// New builds a Loader around fetch. The default wait window is 16ms.
func New[K comparable, V any](fetch FetchFunc[K, V], opts ...Option) *Loader[K, V] {
	c := config{wait: 16 * time.Millisecond}
	for _, o := range opts {
		o(&c)
	}
	return &Loader[K, V]{
		fetch:    fetch,
		wait:     c.wait,
		maxBatch: c.maxBatch,
		noCache:  c.noCache,
		cache:    make(map[K]*result[V]),
	}
}

// Load registers key and blocks until its batch resolves.
func (l *Loader[K, V]) Load(ctx context.Context, key K) (V, error) {
	return l.LoadThunk(ctx, key)()
}

// LoadThunk registers key and returns a function that blocks until the result
// is ready. Registering is cheap and non-blocking, so many callers can register
// before any of them awaits, which is what lets keys coalesce into one batch.
func (l *Loader[K, V]) LoadThunk(ctx context.Context, key K) func() (V, error) {
	l.mu.Lock()
	if r, ok := l.cache[key]; ok {
		l.mu.Unlock()
		return l.thunk(ctx, r)
	}
	r := &result[V]{done: make(chan struct{})}
	if !l.noCache {
		l.cache[key] = r
	}
	if l.batch == nil {
		l.batch = l.newBatch(ctx)
	}
	b := l.batch
	b.keys = append(b.keys, key)
	b.reses = append(b.reses, r)
	if l.maxBatch > 0 && len(b.keys) >= l.maxBatch {
		b.fired = true
		if b.timer != nil {
			b.timer.Stop()
		}
		l.batch = nil
		go l.run(ctx, b)
	}
	l.mu.Unlock()
	return l.thunk(ctx, r)
}

// thunk waits for either the result or the caller's context to end.
func (l *Loader[K, V]) thunk(ctx context.Context, r *result[V]) func() (V, error) {
	return func() (V, error) {
		select {
		case <-r.done:
			return r.value, r.err
		case <-ctx.Done():
			var zero V
			return zero, ctx.Err()
		}
	}
}

// newBatch creates an empty batch and arms the wait-window timer.
func (l *Loader[K, V]) newBatch(ctx context.Context) *batch[K, V] {
	b := &batch[K, V]{}
	b.timer = time.AfterFunc(l.wait, func() {
		l.mu.Lock()
		if b.fired {
			l.mu.Unlock()
			return
		}
		b.fired = true
		if l.batch == b {
			l.batch = nil
		}
		l.mu.Unlock()
		l.run(ctx, b)
	})
	return b
}

// run performs the single batch fetch and distributes results by position.
func (l *Loader[K, V]) run(ctx context.Context, b *batch[K, V]) {
	values, errs := l.fetch(ctx, b.keys)
	for i, r := range b.reses {
		if i < len(values) {
			r.value = values[i]
		}
		switch {
		case len(errs) == 0:
			// every key succeeded
		case len(errs) == 1:
			r.err = errs[0] // whole-batch error hits every sibling
		default:
			if i < len(errs) {
				r.err = errs[i] // per-key error
			}
		}
		close(r.done)
	}
}

// LoadAll registers every key, then collects the results in order. Per-key
// errors are aggregated into an ErrorSlice; the values slice is always the same
// length as keys.
func (l *Loader[K, V]) LoadAll(ctx context.Context, keys []K) ([]V, error) {
	thunks := make([]func() (V, error), len(keys))
	for i, k := range keys {
		thunks[i] = l.LoadThunk(ctx, k)
	}
	values := make([]V, len(keys))
	var errs ErrorSlice
	for i, t := range thunks {
		v, err := t()
		values[i] = v
		if err != nil {
			if errs == nil {
				errs = make(ErrorSlice, len(keys))
			}
			errs[i] = err
		}
	}
	if errs == nil {
		return values, nil
	}
	return values, errs
}

// Prime seeds a value into the cache without a fetch. It reports false if the
// key was already present.
func (l *Loader[K, V]) Prime(key K, value V) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.cache[key]; ok {
		return false
	}
	r := &result[V]{value: value, done: make(chan struct{})}
	close(r.done)
	l.cache[key] = r
	return true
}

// Clear drops one key from the cache, e.g. after a mutation invalidates it.
func (l *Loader[K, V]) Clear(key K) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.cache, key)
}

// ErrorSlice aggregates per-key errors from LoadAll. Its Unwrap returns the
// element errors so errors.Is matches a sentinel wrapped inside any of them.
type ErrorSlice []error

func (e ErrorSlice) Error() string {
	for _, err := range e {
		if err != nil {
			return err.Error()
		}
	}
	return "batchloader: no error"
}

func (e ErrorSlice) Unwrap() []error { return e }
```

### The runnable demo

The demo primes one value, issues a `LoadAll` that includes a duplicate key and
the primed key, and then a single `Load` that hits the cache — proving that a
duplicate key and a cache hit add nothing to the fetch count.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"example.com/batchloader"
)

func main() {
	var batches atomic.Int64
	fetch := func(ctx context.Context, ids []int) ([]string, []error) {
		batches.Add(1)
		names := make([]string, len(ids))
		for i, id := range ids {
			names[i] = fmt.Sprintf("user-%d", id)
		}
		return names, nil
	}

	loader := batchloader.New(fetch, batchloader.WithWait(2*time.Millisecond))
	ctx := context.Background()

	loader.Prime(99, "user-99-cached")

	names, _ := loader.LoadAll(ctx, []int{1, 2, 3, 1, 99})
	fmt.Println(names)

	one, _ := loader.Load(ctx, 2) // served from the request cache, no new batch
	fmt.Println("single:", one)

	fmt.Println("batches:", batches.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
[user-1 user-2 user-3 user-1 user-99-cached]
single: user-2
batches: 1
```

### Tests

The tests prove the properties that make a dataloader worth using.
`TestCoalescesConcurrentLoads` registers all keys from many goroutines before any
thunk is awaited (using `WithBatchCapacity` so the full batch dispatches once) and
asserts exactly one fetch. `TestPositionalAlignment` and
`TestDedupCollapsesIdenticalKeys` check the fetch contract and single-flight.
`TestPerKeyErrorDoesNotPoisonSiblings` and `TestWholeBatchErrorFailsEverySibling`
check both error granularities via `errors.Is` against a wrapped sentinel.
`TestBatchCapacitySplits` confirms capacity splits a `LoadAll` into three fetches,
and `TestContextCancellation` confirms a cancelled context unblocks the thunk even
while the batch is still pending.

Create `batchloader_test.go`:

```go
package batchloader

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var errMissing = errors.New("batchloader: user not found")

func countingFetch(calls *atomic.Int64) FetchFunc[int, string] {
	return func(ctx context.Context, keys []int) ([]string, []error) {
		calls.Add(1)
		out := make([]string, len(keys))
		for i, k := range keys {
			out[i] = fmt.Sprintf("user-%d", k)
		}
		return out, nil
	}
}

func TestCoalescesConcurrentLoads(t *testing.T) {
	t.Parallel()
	const n = 50
	var calls atomic.Int64
	// A long wait plus a capacity of n means the batch dispatches exactly once,
	// when the n-th key registers.
	l := New(countingFetch(&calls), WithWait(time.Minute), WithBatchCapacity(n))

	thunks := make([]func() (string, error), n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			thunks[i] = l.LoadThunk(context.Background(), i)
		}()
	}
	wg.Wait()

	for i, th := range thunks {
		v, err := th()
		if err != nil {
			t.Fatalf("Load(%d) error: %v", i, err)
		}
		if want := fmt.Sprintf("user-%d", i); v != want {
			t.Errorf("Load(%d) = %q; want %q", i, v, want)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("fetch called %d times; want 1 (all loads coalesced)", got)
	}
}

func TestPositionalAlignment(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	l := New(countingFetch(&calls), WithWait(5*time.Millisecond))
	keys := []int{7, 3, 9, 1}
	got, err := l.LoadAll(context.Background(), keys)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	for i, k := range keys {
		if want := fmt.Sprintf("user-%d", k); got[i] != want {
			t.Errorf("value[%d] = %q; want %q", i, got[i], want)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("fetch called %d times; want 1", calls.Load())
	}
}

func TestDedupCollapsesIdenticalKeys(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	var mu sync.Mutex
	var seen []int
	fetch := func(ctx context.Context, keys []int) ([]string, []error) {
		calls.Add(1)
		mu.Lock()
		seen = append(seen, keys...)
		mu.Unlock()
		out := make([]string, len(keys))
		for i, k := range keys {
			out[i] = fmt.Sprintf("user-%d", k)
		}
		return out, nil
	}
	l := New(fetch, WithWait(5*time.Millisecond))
	got, err := l.LoadAll(context.Background(), []int{4, 4, 4, 5})
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	want := []string{"user-4", "user-4", "user-4", "user-5"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("value[%d] = %q; want %q", i, got[i], want[i])
		}
	}
	if len(seen) != 2 {
		t.Errorf("fetch saw keys %v; want exactly 2 unique keys", seen)
	}
	if calls.Load() != 1 {
		t.Errorf("fetch called %d times; want 1", calls.Load())
	}
}

func TestPerKeyErrorDoesNotPoisonSiblings(t *testing.T) {
	t.Parallel()
	fetch := func(ctx context.Context, keys []int) ([]string, []error) {
		vals := make([]string, len(keys))
		errs := make([]error, len(keys))
		for i, k := range keys {
			if k == 0 {
				errs[i] = fmt.Errorf("load user %d: %w", k, errMissing)
				continue
			}
			vals[i] = fmt.Sprintf("user-%d", k)
		}
		return vals, errs
	}
	l := New(fetch, WithWait(5*time.Millisecond))
	vals, err := l.LoadAll(context.Background(), []int{1, 0, 2})
	if !errors.Is(err, errMissing) {
		t.Fatalf("LoadAll error = %v; want an ErrorSlice wrapping errMissing", err)
	}
	if vals[0] != "user-1" || vals[2] != "user-2" {
		t.Errorf("siblings poisoned by one bad key: got %v", vals)
	}
	if vals[1] != "" {
		t.Errorf("failed key should hold the zero value; got %q", vals[1])
	}
}

func TestWholeBatchErrorFailsEverySibling(t *testing.T) {
	t.Parallel()
	fetch := func(ctx context.Context, keys []int) ([]string, []error) {
		return nil, []error{fmt.Errorf("db down: %w", errMissing)}
	}
	l := New(fetch, WithWait(5*time.Millisecond))
	vals, err := l.LoadAll(context.Background(), []int{1, 2, 3})
	if !errors.Is(err, errMissing) {
		t.Fatalf("LoadAll error = %v; want wrapped errMissing", err)
	}
	for i, v := range vals {
		if v != "" {
			t.Errorf("value[%d] = %q; want zero on whole-batch error", i, v)
		}
	}
}

func TestBatchCapacitySplits(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	l := New(countingFetch(&calls), WithWait(20*time.Millisecond), WithBatchCapacity(2))
	got, err := l.LoadAll(context.Background(), []int{1, 2, 3, 4, 5})
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d values; want 5", len(got))
	}
	if calls.Load() != 3 {
		t.Errorf("fetch called %d times; want 3 (2+2+1)", calls.Load())
	}
}

func TestContextCancellation(t *testing.T) {
	t.Parallel()
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	fetch := func(ctx context.Context, keys []int) ([]string, []error) {
		<-block // stall the batch so cancellation, not completion, unblocks us
		return make([]string, len(keys)), nil
	}
	l := New(fetch, WithWait(time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	th := l.LoadThunk(ctx, 1)
	cancel()

	if _, err := th(); !errors.Is(err, context.Canceled) {
		t.Fatalf("thunk error = %v; want context.Canceled", err)
	}
}

func Example() {
	fetch := func(ctx context.Context, keys []int) ([]string, []error) {
		out := make([]string, len(keys))
		for i, k := range keys {
			out[i] = fmt.Sprintf("user-%d", k)
		}
		return out, nil
	}
	l := New(fetch)
	names, _ := l.LoadAll(context.Background(), []int{10, 20, 20})
	fmt.Println(names)
	// Output: [user-10 user-20 user-20]
}
```

## Review

The loader is correct when three invariants hold. First, coalescing: many keys
registered inside one window produce exactly one fetch, which
`TestCoalescesConcurrentLoads` asserts by counting invocations. Second, the fetch
contract: `values[i]` belongs to `keys[i]`, and identical keys collapse to one
fetched key via the cache — if alignment or dedup breaks, todos get the wrong
user, which is silent corruption rather than an error. Third, error granularity:
a per-key error fails only its own thunk while siblings resolve, and a length-one
errors slice fails the whole batch; both are matched with `errors.Is` because
`LoadAll` returns an `ErrorSlice` whose `Unwrap() []error` exposes the wrapped
sentinel.

The traps are the ones the concepts flagged. Do not hold the mutex across the
fetch — `run` executes on its own goroutine after the batch is detached, so a slow
datastore never blocks other `Load` callers. Do not let a batch dispatch twice —
the `fired` flag under the lock plus stopping the timer on the capacity path is
what prevents it. Do not forget cancellation — the thunk selects on `ctx.Done()`
so a dead request stops waiting even though the batch may still complete in the
background. Run `go test -race` to confirm the mutex and the close/receive
publication of results are actually race-free.

## Resources

- [gqlgen: Dataloaders](https://gqlgen.com/reference/dataloaders/) — the official explanation of why field-by-field resolution creates N+1 and how batching fixes it.
- [`sync` package](https://pkg.go.dev/sync) — `sync.Mutex` and the memory model guarantees this loader relies on.
- [`time.AfterFunc`](https://pkg.go.dev/time#AfterFunc) — the timer that closes the batch window.
- [Go 1.20 multiple-error wrapping](https://go.dev/doc/go1.20#errors) — the `Unwrap() []error` form that makes `errors.Is` traverse an `ErrorSlice`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-dataloadgen-loaders.md](02-dataloadgen-loaders.md)
