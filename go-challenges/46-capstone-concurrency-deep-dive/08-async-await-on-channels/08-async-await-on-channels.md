# 8. Async/Await on Channels

Building a `Future[T]` library in Go forces you to solve three problems that async/await language features hide: goroutine ownership (who is responsible for ensuring a goroutine exits?), result caching (can `Await` be called more than once without blocking?), and generic composition (Go methods cannot carry additional type parameters, so `Then[T,U]` must be a package-level function). This lesson builds a complete, non-toy `Future[T]` package with `Then`, `Map`, `FlatMap`, `Recover`, `All`, `Race`, `Any`, `WithTimeout`, and a bounded `Pool`, using only the standard library.

```text
future/
  go.mod
  future.go
  compose.go
  combinator.go
  pool.go
  future_test.go
  example_test.go
  cmd/demo/main.go
```

## Concepts

### How a Future maps to Go primitives

A future is a value that will eventually produce a result. In Go, the natural carrier is a **buffered channel of size one**: the producer writes exactly once; the consumer reads exactly once. A `sync.Once` wraps the consumer side to make `Await` idempotent — after the first call, the result is cached and subsequent calls return immediately without touching the channel.

```text
  producer goroutine            Future[T]                  consumer
  fn(ctx) (T, error) ──write──> chan Result[T] (cap 1) ──> Await() (T, error)
                                 sync.Once caches result      safe to call N times
```

Because a channel and a `sync.Once` both carry pointer semantics, the outer `Future[T]` value must hold a pointer to the shared mutable state. Copying `Future[T]` copies the pointer, not the channel or the mutex — so `Future[T]` is safe to copy and pass by value, exactly like `context.Context`.

### Type parameters on functions, not methods

Go does not allow additional type parameters on methods. A transformation like `Then[T,U]` that converts a `Future[T]` into a `Future[U]` cannot be written as a method on `Future[T]`:

```go
// valid: type parameters on a package-level function
func Then[T, U any](f Future[T], fn func(T) (U, error)) Future[U]

// invalid: does not compile — methods cannot have their own type parameters
// func (f Future[T]) Then[U any](fn func(T) (U, error)) Future[U]
```

This is not a limitation in practice: package-level functions compose cleanly with any `Future[T]` value regardless of where it was created.

### Goroutine ownership and leak prevention

Each `Async` call spawns one goroutine. Each composition function (`Then`, `Map`, `FlatMap`, `Recover`) spawns one additional goroutine per transformation. Parallel combinators (`All`, `Race`, `Any`) spawn one goroutine per input future plus a coordinating goroutine.

These goroutines terminate in one of two ways:

1. The underlying `Await` call returns (the future resolved).
2. The context passed to `Async` is cancelled and the function checks `ctx.Done()`.

The buffered channel of size 1 is the key to preventing goroutine leaks: the producer can always write its result and exit, even if the caller discards the `Future[T]` and never calls `Await`. An unbuffered channel would cause the goroutine to block forever.

### Cancellation is cooperative

Cancelling a context does not kill a goroutine. It signals that the goroutine should stop. The function passed to `Async` must cooperate by checking `ctx.Done()` or passing `ctx` to blocking calls such as `http.NewRequestWithContext` or database operations. A function that ignores its context cannot be cancelled and will run to completion regardless.

### Pool as a semaphore

A bounded pool limits the number of concurrently running goroutines. The Go idiom is a buffered channel of `struct{}` used as a semaphore: sending acquires a slot (`sem <- struct{}{}`); receiving releases it (`<-sem`). The channel capacity is the concurrency limit. A goroutine that cannot acquire a slot blocks until one is released or the context is cancelled.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/future/cmd/demo
cd ~/go-exercises/future
go mod init example.com/future
```

This is a library, not a program. Verify it with `go test`.

### Exercise 1: Core Types and Async/Await

Create `future.go`:

```go
package future

import (
	"context"
	"sync"
)

// Result holds the outcome of an asynchronous computation.
type Result[T any] struct {
	Value T
	Err   error
}

// state is the heap-allocated mutable core of a Future.
// Separating it from the Future value lets Future be copied safely:
// all copies share the same channel and Once.
type state[T any] struct {
	ch   chan Result[T]
	once sync.Once
	res  Result[T]
}

// Future represents a value that will be available asynchronously.
// The zero value is not usable; create futures with Async, Resolved, or Rejected.
// Future is safe to copy and pass by value.
type Future[T any] struct {
	s *state[T]
}

func newFuture[T any]() Future[T] {
	return Future[T]{s: &state[T]{ch: make(chan Result[T], 1)}}
}

// Resolved returns an already-resolved future wrapping v.
// Useful for lifting a plain value into the Future type.
func Resolved[T any](v T) Future[T] {
	f := newFuture[T]()
	f.s.ch <- Result[T]{Value: v}
	return f
}

// Rejected returns a future that immediately resolves with err.
func Rejected[T any](err error) Future[T] {
	f := newFuture[T]()
	var zero T
	f.s.ch <- Result[T]{Value: zero, Err: err}
	return f
}

// Async launches fn in a new goroutine and returns a Future that resolves
// when fn returns. The context is forwarded to fn so the caller can signal
// cancellation by cancelling ctx; fn must check ctx.Done() to cooperate.
func Async[T any](ctx context.Context, fn func(context.Context) (T, error)) Future[T] {
	f := newFuture[T]()
	go func() {
		v, err := fn(ctx)
		f.s.ch <- Result[T]{Value: v, Err: err}
	}()
	return f
}

// Await blocks until the future resolves and returns its value and error.
// After the first return, the result is cached; subsequent calls return
// immediately with the same value without touching the channel.
func (f Future[T]) Await() (T, error) {
	f.s.once.Do(func() {
		f.s.res = <-f.s.ch
	})
	return f.s.res.Value, f.s.res.Err
}
```

`state` is heap-allocated so that copying a `Future[T]` value copies the pointer, not the channel or the `sync.Once`. The buffered channel of size 1 lets the producer goroutine write and exit even if the caller never calls `Await`.

### Exercise 2: Sequential Composition

Create `compose.go`:

```go
package future

// Then chains a transformation: when f resolves successfully, fn is applied
// to the value to produce a new Future[U]. If f resolves with an error,
// the error propagates and fn is never called.
func Then[T, U any](f Future[T], fn func(T) (U, error)) Future[U] {
	out := newFuture[U]()
	go func() {
		v, err := f.Await()
		if err != nil {
			var zero U
			out.s.ch <- Result[U]{Value: zero, Err: err}
			return
		}
		u, err := fn(v)
		out.s.ch <- Result[U]{Value: u, Err: err}
	}()
	return out
}

// Map applies an infallible transformation to the resolved value of f.
// It is equivalent to Then with a function that never returns an error.
func Map[T, U any](f Future[T], fn func(T) U) Future[U] {
	return Then(f, func(v T) (U, error) {
		return fn(v), nil
	})
}

// FlatMap applies fn to the value of f, producing an inner Future[U], then
// awaits the inner future. This is the monadic bind: it flattens
// Future[Future[U]] into Future[U] without nesting.
func FlatMap[T, U any](f Future[T], fn func(T) Future[U]) Future[U] {
	out := newFuture[U]()
	go func() {
		v, err := f.Await()
		if err != nil {
			var zero U
			out.s.ch <- Result[U]{Value: zero, Err: err}
			return
		}
		inner := fn(v)
		u, err := inner.Await()
		out.s.ch <- Result[U]{Value: u, Err: err}
	}()
	return out
}

// Recover calls fn when f resolves with an error, allowing the caller to
// substitute a recovery value or transform the error. If fn itself returns
// an error, the recovered future resolves with that error.
// When f resolves successfully, fn is not called and the value passes through.
func Recover[T any](f Future[T], fn func(error) (T, error)) Future[T] {
	out := newFuture[T]()
	go func() {
		v, err := f.Await()
		if err != nil {
			v, err = fn(err)
		}
		out.s.ch <- Result[T]{Value: v, Err: err}
	}()
	return out
}
```

`FlatMap` is the critical operation: `fn` returns `Future[U]`, not `U`. The outer goroutine awaits `f`, calls `fn` to obtain an inner future, awaits that inner future, then writes the final result to `out`. No extra type machinery is required; the channel does the flattening.

### Exercise 3: Parallel Combinators

Create `combinator.go`:

```go
package future

import (
	"context"
	"sync"
	"time"
)

// All resolves when every future in the slice resolves, returning results in
// the original order. If any future resolves with an error, All returns that
// error as soon as it is observed; remaining goroutines run to completion
// but their results are discarded. If ctx is cancelled before all futures
// complete, All resolves with ctx.Err().
func All[T any](ctx context.Context, futures ...Future[T]) Future[[]T] {
	out := newFuture[[]T]()
	go func() {
		if len(futures) == 0 {
			out.s.ch <- Result[[]T]{Value: []T{}}
			return
		}

		results := make([]T, len(futures))
		errCh := make(chan error, 1)
		doneCh := make(chan struct{})
		var mu sync.Mutex
		var wg sync.WaitGroup

		for i, f := range futures {
			wg.Add(1)
			go func(idx int, f Future[T]) {
				defer wg.Done()
				v, err := f.Await()
				if err != nil {
					select {
					case errCh <- err:
					default:
					}
					return
				}
				mu.Lock()
				results[idx] = v
				mu.Unlock()
			}(i, f)
		}

		go func() {
			wg.Wait()
			close(doneCh)
		}()

		select {
		case err := <-errCh:
			var zero []T
			out.s.ch <- Result[[]T]{Value: zero, Err: err}
		case <-doneCh:
			out.s.ch <- Result[[]T]{Value: results}
		case <-ctx.Done():
			var zero []T
			out.s.ch <- Result[[]T]{Value: zero, Err: ctx.Err()}
		}
	}()
	return out
}

// Race resolves with the result of the first future to complete, success or
// error. The remaining futures run to completion in the background; because
// each future's channel is buffered, no goroutine blocks after the winner
// is chosen.
//
// Go does not support select on a dynamically sized set of channels, so Race
// uses a merge goroutine: each future writes into a shared buffered channel,
// and the merge goroutine reads the first result.
func Race[T any](futures ...Future[T]) Future[T] {
	out := newFuture[T]()
	ch := make(chan Result[T], len(futures))
	for _, f := range futures {
		f := f
		go func() {
			v, err := f.Await()
			ch <- Result[T]{Value: v, Err: err}
		}()
	}
	go func() {
		out.s.ch <- <-ch
	}()
	return out
}

// Any resolves with the first successfully resolved future. Errors are
// suppressed until all futures have completed; if every future fails,
// Any resolves with the last error observed.
func Any[T any](futures ...Future[T]) Future[T] {
	out := newFuture[T]()
	ch := make(chan Result[T], len(futures))
	for _, f := range futures {
		f := f
		go func() {
			v, err := f.Await()
			ch <- Result[T]{Value: v, Err: err}
		}()
	}
	go func() {
		var lastErr error
		for range futures {
			r := <-ch
			if r.Err == nil {
				out.s.ch <- r
				return
			}
			lastErr = r.Err
		}
		var zero T
		out.s.ch <- Result[T]{Value: zero, Err: lastErr}
	}()
	return out
}

// WithTimeout returns a future that resolves with context.DeadlineExceeded
// if f does not complete within d. If f completes in time, its result is
// forwarded unchanged.
func WithTimeout[T any](f Future[T], d time.Duration) Future[T] {
	out := newFuture[T]()
	ch := make(chan Result[T], 1)
	go func() {
		v, err := f.Await()
		ch <- Result[T]{Value: v, Err: err}
	}()
	go func() {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case r := <-ch:
			out.s.ch <- r
		case <-timer.C:
			var zero T
			out.s.ch <- Result[T]{Value: zero, Err: context.DeadlineExceeded}
		}
	}()
	return out
}
```

`Race` cannot use a `select` statement directly: Go's `select` requires the channel set to be known at compile time, not determined at runtime. The merge-goroutine pattern works around this by funneling all futures into a single channel of capacity `len(futures)`. Each goroutine writes and exits; the merge goroutine reads the first result. No goroutine blocks after the winner is chosen because the merge channel is fully buffered.

### Exercise 4: Bounded Pool

Create `pool.go`:

```go
package future

import "context"

// Pool limits the number of concurrently executing async functions.
// Use NewPool to create a valid Pool.
type Pool struct {
	sem chan struct{}
}

// NewPool creates a Pool that allows at most maxConcurrency concurrent
// goroutines. It panics if maxConcurrency is less than 1.
func NewPool(maxConcurrency int) *Pool {
	if maxConcurrency < 1 {
		panic("future: NewPool: maxConcurrency must be >= 1")
	}
	return &Pool{sem: make(chan struct{}, maxConcurrency)}
}

// Submit queues fn for execution through the pool. A background goroutine
// blocks until a concurrency slot is available or ctx is cancelled.
// The returned Future resolves when fn completes or when ctx is cancelled
// before a slot becomes available.
//
// Submit is a package-level function rather than a method because Go methods
// cannot carry additional type parameters.
func Submit[T any](p *Pool, ctx context.Context, fn func(context.Context) (T, error)) Future[T] {
	f := newFuture[T]()
	go func() {
		select {
		case p.sem <- struct{}{}:
		case <-ctx.Done():
			var zero T
			f.s.ch <- Result[T]{Value: zero, Err: ctx.Err()}
			return
		}
		defer func() { <-p.sem }()
		v, err := fn(ctx)
		f.s.ch <- Result[T]{Value: v, Err: err}
	}()
	return f
}
```

The semaphore pattern (`chan struct{}` of capacity `maxConcurrency`) is the standard Go idiom for bounded concurrency. Sending acquires a slot; the deferred receive releases it when `fn` returns regardless of how it returns.

### Exercise 5: Tests

Create `future_test.go`:

```go
package future_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"example.com/future"
)

func TestResolved(t *testing.T) {
	t.Parallel()
	f := future.Resolved(42)
	v, err := f.Await()
	if err != nil || v != 42 {
		t.Fatalf("Resolved(42).Await() = (%v, %v), want (42, nil)", v, err)
	}
}

func TestRejected(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("sentinel")
	f := future.Rejected[int](sentinel)
	_, err := f.Await()
	if !errors.Is(err, sentinel) {
		t.Fatalf("Rejected.Await() err = %v, want sentinel", err)
	}
}

func TestAwaitIsIdempotent(t *testing.T) {
	t.Parallel()
	f := future.Resolved("hello")
	for i := range 5 {
		v, err := f.Await()
		if err != nil || v != "hello" {
			t.Fatalf("Await() call %d = (%v, %v), want (hello, nil)", i, v, err)
		}
	}
}

func TestAsyncReturnsResult(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := future.Async(ctx, func(_ context.Context) (int, error) {
		return 7, nil
	})
	v, err := f.Await()
	if err != nil || v != 7 {
		t.Fatalf("Async.Await() = (%v, %v), want (7, nil)", v, err)
	}
}

func TestAsyncPropagatesError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("boom")
	ctx := context.Background()
	f := future.Async(ctx, func(_ context.Context) (int, error) {
		return 0, sentinel
	})
	_, err := f.Await()
	if !errors.Is(err, sentinel) {
		t.Fatalf("Async.Await() err = %v, want sentinel", err)
	}
}

func TestAsyncHonoursContextCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := future.Async(ctx, func(ctx context.Context) (int, error) {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(5 * time.Second):
			return 1, nil
		}
	})
	_, err := f.Await()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestThenTransformsValue(t *testing.T) {
	t.Parallel()
	f := future.Then(future.Resolved(3), func(v int) (string, error) {
		return fmt.Sprintf("n=%d", v), nil
	})
	s, err := f.Await()
	if err != nil || s != "n=3" {
		t.Fatalf("Then.Await() = (%v, %v), want (n=3, nil)", s, err)
	}
}

func TestThenPropagatesUpstreamError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("upstream")
	called := false
	f := future.Then(future.Rejected[int](sentinel), func(_ int) (string, error) {
		called = true
		return "", nil
	})
	_, err := f.Await()
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want upstream sentinel", err)
	}
	if called {
		t.Fatal("fn must not be called when upstream fails")
	}
}

func TestMapIsInfallible(t *testing.T) {
	t.Parallel()
	f := future.Map(future.Resolved(10), func(v int) string {
		return fmt.Sprintf("%d", v)
	})
	s, err := f.Await()
	if err != nil || s != "10" {
		t.Fatalf("Map.Await() = (%v, %v), want (10, nil)", s, err)
	}
}

func TestFlatMapFlattens(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := future.FlatMap(future.Resolved(5), func(v int) future.Future[string] {
		return future.Async(ctx, func(_ context.Context) (string, error) {
			return fmt.Sprintf("got %d", v), nil
		})
	})
	s, err := f.Await()
	if err != nil || s != "got 5" {
		t.Fatalf("FlatMap.Await() = (%v, %v), want (got 5, nil)", s, err)
	}
}

func TestFlatMapPropagatesInnerError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("inner")
	f := future.FlatMap(future.Resolved(1), func(_ int) future.Future[string] {
		return future.Rejected[string](sentinel)
	})
	_, err := f.Await()
	if !errors.Is(err, sentinel) {
		t.Fatalf("FlatMap err = %v, want inner sentinel", err)
	}
}

func TestRecoverSubstitutesOnError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("original")
	f := future.Recover(future.Rejected[int](sentinel), func(_ error) (int, error) {
		return -1, nil
	})
	v, err := f.Await()
	if err != nil || v != -1 {
		t.Fatalf("Recover.Await() = (%v, %v), want (-1, nil)", v, err)
	}
}

func TestRecoverPassesThroughOnSuccess(t *testing.T) {
	t.Parallel()
	called := false
	f := future.Recover(future.Resolved(99), func(_ error) (int, error) {
		called = true
		return 0, nil
	})
	v, err := f.Await()
	if err != nil || v != 99 {
		t.Fatalf("Recover.Await() = (%v, %v), want (99, nil)", v, err)
	}
	if called {
		t.Fatal("Recover fn must not be called on success")
	}
}

func TestAllCollectsResults(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const n = 10
	futures := make([]future.Future[int], n)
	for i := range n {
		i := i
		futures[i] = future.Async(ctx, func(_ context.Context) (int, error) {
			return i * 2, nil
		})
	}
	results, err := future.All(ctx, futures...).Await()
	if err != nil {
		t.Fatalf("All.Await() err = %v", err)
	}
	if len(results) != n {
		t.Fatalf("len(results) = %d, want %d", len(results), n)
	}
	for i, v := range results {
		if v != i*2 {
			t.Errorf("results[%d] = %d, want %d", i, v, i*2)
		}
	}
}

func TestAllFailsFast(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sentinel := errors.New("one failed")
	futures := []future.Future[int]{
		future.Resolved(1),
		future.Rejected[int](sentinel),
		future.Resolved(3),
	}
	_, err := future.All(ctx, futures...).Await()
	if !errors.Is(err, sentinel) {
		t.Fatalf("All err = %v, want sentinel", err)
	}
}

func TestAllIsParallel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const n = 20
	const delay = 50 * time.Millisecond
	futures := make([]future.Future[int], n)
	for i := range n {
		futures[i] = future.Async(ctx, func(_ context.Context) (int, error) {
			time.Sleep(delay)
			return 0, nil
		})
	}
	start := time.Now()
	future.All(ctx, futures...).Await()
	elapsed := time.Since(start)
	// Parallel execution: should complete in roughly one delay, not n*delay.
	if elapsed > 4*delay {
		t.Fatalf("All took %v; expected ~%v (parallel, not sequential)", elapsed, delay)
	}
}

func TestRaceReturnsFirst(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	slow := future.Async(ctx, func(_ context.Context) (int, error) {
		time.Sleep(200 * time.Millisecond)
		return 100, nil
	})
	fast := future.Resolved(1)
	v, err := future.Race(slow, fast).Await()
	if err != nil || v != 1 {
		t.Fatalf("Race.Await() = (%v, %v), want (1, nil)", v, err)
	}
}

func TestAnyReturnsFirstSuccess(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("fail")
	futures := []future.Future[int]{
		future.Rejected[int](sentinel),
		future.Resolved(42),
	}
	v, err := future.Any(futures...).Await()
	if err != nil || v != 42 {
		t.Fatalf("Any.Await() = (%v, %v), want (42, nil)", v, err)
	}
}

func TestAnyAllFail(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("all bad")
	futures := []future.Future[int]{
		future.Rejected[int](sentinel),
		future.Rejected[int](sentinel),
	}
	_, err := future.Any(futures...).Await()
	if err == nil {
		t.Fatal("Any.Await() err = nil, want non-nil when all fail")
	}
}

func TestWithTimeoutExpires(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	slow := future.Async(ctx, func(_ context.Context) (int, error) {
		time.Sleep(500 * time.Millisecond)
		return 1, nil
	})
	_, err := future.WithTimeout(slow, 20*time.Millisecond).Await()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WithTimeout err = %v, want DeadlineExceeded", err)
	}
}

func TestWithTimeoutSucceedsInTime(t *testing.T) {
	t.Parallel()
	f := future.WithTimeout(future.Resolved(7), 500*time.Millisecond)
	v, err := f.Await()
	if err != nil || v != 7 {
		t.Fatalf("WithTimeout.Await() = (%v, %v), want (7, nil)", v, err)
	}
}

func TestPoolLimitsConcurrency(t *testing.T) {
	t.Parallel()
	const limit = 4
	const tasks = 50
	p := future.NewPool(limit)
	ctx := context.Background()

	var active atomic.Int64
	var maxSeen atomic.Int64

	futures := make([]future.Future[struct{}], tasks)
	for i := range tasks {
		futures[i] = future.Submit(p, ctx, func(_ context.Context) (struct{}, error) {
			cur := active.Add(1)
			for {
				old := maxSeen.Load()
				if cur <= old {
					break
				}
				if maxSeen.CompareAndSwap(old, cur) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			active.Add(-1)
			return struct{}{}, nil
		})
	}
	for _, f := range futures {
		f.Await()
	}
	if got := maxSeen.Load(); got > limit {
		t.Fatalf("peak concurrency = %d, want <= %d", got, limit)
	}
}

func TestNoGoroutineLeak(t *testing.T) {
	t.Parallel()
	baseline := runtime.NumGoroutine()
	ctx := context.Background()
	const n = 100
	futures := make([]future.Future[int], n)
	for i := range n {
		i := i
		futures[i] = future.Async(ctx, func(_ context.Context) (int, error) {
			return i, nil
		})
	}
	future.All(ctx, futures...).Await()
	// Allow time for goroutines to exit after All completes.
	time.Sleep(50 * time.Millisecond)
	after := runtime.NumGoroutine()
	if after > baseline+5 {
		t.Fatalf("goroutine count: before=%d after=%d; possible leak", baseline, after)
	}
}

// Your turn: add TestAllEmptySlice that calls future.All with no futures and
// asserts the result is an empty slice with no error.
```

Create `example_test.go`:

```go
package future_test

import (
	"context"
	"fmt"

	"example.com/future"
)

func ExampleResolved() {
	v, _ := future.Resolved(42).Await()
	fmt.Println(v)
	// Output: 42
}

func ExampleAsync() {
	ctx := context.Background()
	f := future.Async(ctx, func(_ context.Context) (string, error) {
		return "hello", nil
	})
	s, _ := f.Await()
	fmt.Println(s)
	// Output: hello
}

func ExampleThen() {
	f := future.Then(future.Resolved(6), func(v int) (int, error) {
		return v * 7, nil
	})
	v, _ := f.Await()
	fmt.Println(v)
	// Output: 42
}
```

### Exercise 6: Command-Line Demo

Create `cmd/demo/main.go`. Because `cmd/demo` is a separate `package main`, it may only use the exported API:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"example.com/future"
)

// fetchUser simulates a network call returning a user name.
func fetchUser(ctx context.Context, id int) future.Future[string] {
	return future.Async(ctx, func(ctx context.Context) (string, error) {
		select {
		case <-time.After(20 * time.Millisecond):
			return fmt.Sprintf("user-%d", id), nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
}

func main() {
	ctx := context.Background()

	// Basic Async/Await.
	f := future.Async(ctx, func(_ context.Context) (int, error) {
		time.Sleep(10 * time.Millisecond)
		return 42, nil
	})
	v, err := f.Await()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Async result: %d\n", v)

	// Chain with Then.
	doubled := future.Then(future.Resolved(21), func(n int) (int, error) {
		return n * 2, nil
	})
	n, _ := doubled.Await()
	fmt.Printf("Then(21, *2): %d\n", n)

	// Map (infallible).
	label := future.Map(future.Resolved(7), func(v int) string {
		return fmt.Sprintf("value=%d", v)
	})
	s, _ := label.Await()
	fmt.Println(s)

	// FlatMap (monadic bind).
	nested := future.FlatMap(future.Resolved(3), func(v int) future.Future[int] {
		return future.Async(ctx, func(_ context.Context) (int, error) {
			return v * v, nil
		})
	})
	sq, _ := nested.Await()
	fmt.Printf("FlatMap(3, sq): %d\n", sq)

	// Recover from an error.
	recovered := future.Recover(
		future.Rejected[int](fmt.Errorf("transient")),
		func(err error) (int, error) { return 0, nil },
	)
	r, _ := recovered.Await()
	fmt.Printf("Recover: %d\n", r)

	// Parallel fetch with All.
	start := time.Now()
	users := make([]future.Future[string], 5)
	for i := range 5 {
		users[i] = fetchUser(ctx, i+1)
	}
	names, err := future.All(ctx, users...).Await()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("All 5 users in %v: %v\n", time.Since(start).Round(time.Millisecond), names)

	// Race between slow and fast.
	slow := future.Async(ctx, func(_ context.Context) (string, error) {
		time.Sleep(500 * time.Millisecond)
		return "slow", nil
	})
	winner, _ := future.Race(slow, future.Resolved("fast")).Await()
	fmt.Printf("Race winner: %s\n", winner)

	// Any: first success among a failed and a successful future.
	any, _ := future.Any(
		future.Rejected[string](fmt.Errorf("fail")),
		future.Resolved("ok"),
	).Await()
	fmt.Printf("Any: %s\n", any)

	// WithTimeout on a slow future.
	_, err = future.WithTimeout(
		future.Async(ctx, func(_ context.Context) (int, error) {
			time.Sleep(200 * time.Millisecond)
			return 0, nil
		}),
		50*time.Millisecond,
	).Await()
	fmt.Printf("WithTimeout error: %v\n", err)

	// Pool with limited concurrency.
	pool := future.NewPool(2)
	poolFutures := make([]future.Future[int], 6)
	for i := range 6 {
		i := i
		poolFutures[i] = future.Submit(pool, ctx, func(_ context.Context) (int, error) {
			return i * i, nil
		})
	}
	for i, pf := range poolFutures {
		val, _ := pf.Await()
		fmt.Printf("pool[%d]=%d  ", i, val)
	}
	fmt.Println()
}
```

## Common Mistakes

### Unbuffered channel causes the producer goroutine to block forever

Wrong: `ch: make(chan Result[T])` (no capacity). If the caller discards the `Future[T]` and never calls `Await`, the goroutine launched by `Async` blocks indefinitely on the channel write — a goroutine leak.

Fix: `ch: make(chan Result[T], 1)`. The buffered channel lets the producer write and exit regardless of whether `Await` is ever called.

### Copying sync.Once directly in the Future value

Wrong: embedding `sync.Once` as a direct field of `Future[T]` and then copying `Future[T]` by value. Copying a `sync.Once` that has been used is undefined behavior; the second copy behaves as if `Do` has never been called.

Fix: store the `sync.Once` in a heap-allocated `state` struct and hold a pointer to it in `Future[T]`. All copies of the `Future[T]` value share the same `state`, so `Await` is idempotent across all copies.

### Writing to the result channel more than once

Wrong: calling `out.s.ch <- result` in multiple code paths without ensuring exactly one path executes. The channel has capacity 1; a second send blocks forever (or panics if the channel is closed).

Fix: use a single write point per goroutine, with early returns for error paths, and never close the channel from the producer side. The consumer (`Await` via `sync.Once`) reads exactly once.

### Assuming context cancellation stops the goroutine

Wrong: cancelling the context passed to `Async` and expecting the goroutine to stop immediately.

Fix: the function passed to `Async` must check `ctx.Done()` explicitly. A function that does not check its context ignores cancellation and runs to completion, causing operations to take longer than intended and potentially holding resources after cancellation.

### Making Submit a method on Pool

Wrong: writing `func (p *Pool) Submit[T any](...) Future[T]`. This does not compile: Go methods cannot carry additional type parameters.

Fix: make `Submit` a package-level function: `func Submit[T any](p *Pool, ctx context.Context, fn func(context.Context) (T, error)) Future[T]`.

## Verification

From `~/go-exercises/future`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

All five commands must pass without error. The race detector (`-race`) is especially important here: missed synchronization on the `results` slice in `All` or on the `maxSeen` counter in `TestPoolLimitsConcurrency` will surface as data-race reports.

Add one more test: `TestAllEmptySlice` — call `future.All` with no futures and assert the result is `[]T{}` with `err == nil`. This pins the edge-case handling in `All`.

## Summary

- `Future[T]` wraps a buffered channel of size 1 and a `sync.Once`; store them on the heap so `Future[T]` is safe to copy.
- `Await` is idempotent: the first call reads from the channel and caches the result; subsequent calls return the cached value.
- Generic transformations (`Then`, `Map`, `FlatMap`) must be package-level functions because Go methods cannot have additional type parameters.
- `All` uses a `sync.WaitGroup` and a merge channel for error delivery; a `close(doneCh)` signals all-success without polling.
- `Race` and `Any` use the merge-goroutine pattern because `select` cannot operate on a dynamically sized channel set.
- A buffered channel of `struct{}` (capacity = limit) is the idiomatic Go semaphore for bounded concurrency.
- Cancellation is cooperative: `Async` passes the context to the function; the function must check `ctx.Done()`.

## What's Next

Next: [Coroutine Library](../09-coroutine-library/09-coroutine-library.md).

## Resources

- [Go specification: Type parameter declarations](https://go.dev/ref/spec#Type_parameter_declarations) — defines the constraint that methods cannot have their own type parameters.
- [sync package documentation](https://pkg.go.dev/sync) — `sync.Once`, `sync.WaitGroup`, and `sync.Mutex` used throughout this lesson.
- [context package documentation](https://pkg.go.dev/context) — canonical reference for `context.DeadlineExceeded`, `ctx.Done()`, and cooperative cancellation.
- [Go Blog: Share Memory by Communicating](https://go.dev/blog/codelab-share) — the channel-as-ownership-transfer idiom that underlies `Future[T]`.
- [Go Blog: Concurrency Patterns: Pipelines and Cancellation](https://go.dev/blog/pipelines) — merge-goroutine pattern used in `Race` and `Any`.
