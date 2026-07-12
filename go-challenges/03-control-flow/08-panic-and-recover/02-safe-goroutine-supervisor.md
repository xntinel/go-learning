# Exercise 2: SafeGo — A Panic-Isolating Goroutine Supervisor

A panic in a goroutine you launched with `go` is invisible to the launcher and
crashes the whole process. Any long-lived backend — a worker pool, a fan-out
dispatcher, a set of background consumers — needs every spawned goroutine to
recover itself and report the failure instead of taking the process down. This
module builds `SafeGo`, which runs a function in its own goroutine with a
deferred recover, converts any panic into an error (preserving it with `%w` when
it is already an error), and delivers the result on a channel; then a
`WaitGroup`-based `Pool` that keeps running after any single worker dies.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
safego/                    independent module: example.com/safego
  go.mod                   go 1.26
  safego.go                Result, SafeGo, panicToError, Pool{Go, Wait}
  cmd/
    demo/
      main.go              runnable demo: a pool where one worker panics
  safego_test.go           table test (panic error/panic string/normal) + pool test
```

Files: `safego.go`, `cmd/demo/main.go`, `safego_test.go`.
Implement: `SafeGo(ctx, name, fn) <-chan Result`, a `Pool` with `Go(ctx, name, fn)` and `Wait() []Result`, and `panicToError` that wraps with `%w` for an error value.
Test: a table asserting `panic(errors.New(...))` yields a `%w`-wrapped error (`errors.Is` passes), `panic("str")` yields a formatted error, a normal return yields its error/nil; a concurrency test where one of N workers panics and the other N-1 still complete and `Pool.Wait` returns all N results.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/08-panic-and-recover/02-safe-goroutine-supervisor/cmd/demo
cd go-solutions/03-control-flow/08-panic-and-recover/02-safe-goroutine-supervisor
```

### The recover must live inside the spawned goroutine

The defining rule of this module is goroutine scope: the deferred `recover` has
to be installed *inside* the goroutine that runs `fn`, not in the launcher. If
`SafeGo` deferred a recover in the calling goroutine and then did `go fn()`, the
recover would be useless — `fn`'s panic is on the child's stack. So `SafeGo`
starts the goroutine and the *first* thing that goroutine's closure does is
`defer` its own recover.

`SafeGo` returns a buffered (size 1) `<-chan Result` so the goroutine can always
deliver exactly one result without blocking, whether `fn` returned normally or
panicked. The structure is deliberate: the closure defers the recover, then runs
`err := fn(ctx)` and sends a success `Result`. If `fn` panics, the `err := ...`
line never completes, control jumps to the deferred function, `recover()` returns
the panic value, and it sends a failure `Result` built by `panicToError`. Because
exactly one of those two paths runs, exactly one value is sent — no double-send,
no missing send.

`panicToError` is where the `%w` discipline lives. If the recovered value is
already an `error`, wrap it with `fmt.Errorf("panic in %s: %w", name, err)` so the
original is preserved and `errors.Is`/`errors.As` still reach it downstream. If it
is anything else (a string, an int), format it with `%v` — there is nothing to
unwrap. This mirrors the `Wrap(name, fn)` helper the original lesson shipped,
generalized to carry a context and deliver its result asynchronously.

The `Pool` is the real supervisor: `Go` adds to a `WaitGroup` and spawns a
recovering goroutine that records its `Result` under a mutex; `Wait` blocks on
the `WaitGroup` and returns a snapshot of all results. One worker panicking
records a failure `Result` and decrements the `WaitGroup` (the deferred
`wg.Done()` is registered first, so it runs after the recover) — the other
workers are entirely unaffected. That is the property a production pool needs:
one poison task never stalls or crashes the rest.

Create `safego.go`:

```go
package safego

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
)

// Result is the outcome of one supervised unit of work.
type Result struct {
	Name  string
	Err   error
	Stack []byte // populated only when Err came from a recovered panic
}

// panicToError converts a recovered panic value into an error, preserving an
// underlying error with %w so errors.Is/As keep working.
func panicToError(name string, rec any) error {
	if err, ok := rec.(error); ok {
		return fmt.Errorf("panic in %s: %w", name, err)
	}
	return fmt.Errorf("panic in %s: %v", name, rec)
}

// SafeGo runs fn in its own goroutine with a deferred recover installed inside
// that goroutine, and delivers exactly one Result on the returned channel.
func SafeGo(ctx context.Context, name string, fn func(context.Context) error) <-chan Result {
	ch := make(chan Result, 1)
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				ch <- Result{Name: name, Err: panicToError(name, rec), Stack: debug.Stack()}
			}
		}()
		err := fn(ctx)
		ch <- Result{Name: name, Err: err}
	}()
	return ch
}

// Pool supervises a set of goroutines. A panic in one worker is converted to a
// recorded Result and never crashes the process or disturbs the other workers.
type Pool struct {
	wg      sync.WaitGroup
	mu      sync.Mutex
	results []Result
}

// Go launches a supervised worker.
func (p *Pool) Go(ctx context.Context, name string, fn func(context.Context) error) {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer func() {
			if rec := recover(); rec != nil {
				p.record(Result{Name: name, Err: panicToError(name, rec), Stack: debug.Stack()})
			}
		}()
		err := fn(ctx)
		p.record(Result{Name: name, Err: err})
	}()
}

func (p *Pool) record(r Result) {
	p.mu.Lock()
	p.results = append(p.results, r)
	p.mu.Unlock()
}

// Wait blocks until every worker has finished and returns a snapshot of results.
func (p *Pool) Wait() []Result {
	p.wg.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Result, len(p.results))
	copy(out, p.results)
	return out
}
```

### The runnable demo

The demo launches four workers into a `Pool`; worker "job-2" panics. The pool
waits for all four and reports how many failed — proving three completed normally
while one was isolated.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"example.com/safego"
)

func main() {
	var pool safego.Pool
	ctx := context.Background()

	for i := 1; i <= 4; i++ {
		name := fmt.Sprintf("job-%d", i)
		pool.Go(ctx, name, func(context.Context) error {
			if i == 2 {
				panic(errors.New("bad record"))
			}
			return nil
		})
	}

	results := pool.Wait()
	sort.Slice(results, func(a, b int) bool { return results[a].Name < results[b].Name })

	failed := 0
	for _, r := range results {
		if r.Err != nil {
			failed++
			fmt.Printf("%s: FAILED (%v)\n", r.Name, r.Err)
		} else {
			fmt.Printf("%s: ok\n", r.Name)
		}
	}
	fmt.Printf("%d of %d workers failed; process survived\n", failed, len(results))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
job-1: ok
job-2: FAILED (panic in job-2: bad record)
job-3: ok
job-4: ok
1 of 4 workers failed; process survived
```

### Tests

`TestSafeGo` is a table over the three outcomes: a panic with an error (assert
`errors.Is` still finds the sentinel through the `%w` wrap), a panic with a
string (assert the formatted message), and a normal return of an error and of
`nil`. `TestPoolIsolatesPanic` launches 20 workers where index 7 panics, waits,
and asserts all 20 results are present with exactly one error — proving the pool's
`WaitGroup` completed and the other 19 ran to completion.

Create `safego_test.go`:

```go
package safego

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

var errWork = errors.New("work failed")

func TestSafeGo(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("panic sentinel")

	tests := []struct {
		name      string
		fn        func(context.Context) error
		wantErr   bool
		wantIs    error  // errors.Is target, if any
		wantSubst string // substring of Err.Error(), if any
	}{
		{
			name:      "panic with error preserves %w",
			fn:        func(context.Context) error { panic(sentinel) },
			wantErr:   true,
			wantIs:    sentinel,
			wantSubst: "panic in",
		},
		{
			name:      "panic with string is formatted",
			fn:        func(context.Context) error { panic("raw string") },
			wantErr:   true,
			wantSubst: "raw string",
		},
		{
			name:    "normal return of error",
			fn:      func(context.Context) error { return errWork },
			wantErr: true,
			wantIs:  errWork,
		},
		{
			name:    "normal return of nil",
			fn:      func(context.Context) error { return nil },
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := <-SafeGo(context.Background(), "w", tc.fn)
			if (res.Err != nil) != tc.wantErr {
				t.Fatalf("Err = %v, wantErr = %v", res.Err, tc.wantErr)
			}
			if tc.wantIs != nil && !errors.Is(res.Err, tc.wantIs) {
				t.Fatalf("errors.Is(%v, %v) = false, want true", res.Err, tc.wantIs)
			}
			if tc.wantSubst != "" && !strings.Contains(res.Err.Error(), tc.wantSubst) {
				t.Fatalf("Err = %q, want substring %q", res.Err, tc.wantSubst)
			}
		})
	}
}

func TestPoolIsolatesPanic(t *testing.T) {
	t.Parallel()

	const n = 20
	const poison = 7

	var completed atomic.Int64
	var pool Pool
	ctx := context.Background()

	for i := range n {
		pool.Go(ctx, "worker", func(context.Context) error {
			if i == poison {
				panic(errors.New("poison task"))
			}
			completed.Add(1)
			return nil
		})
	}

	results := pool.Wait()

	if len(results) != n {
		t.Fatalf("len(results) = %d, want %d", len(results), n)
	}
	if got := completed.Load(); got != n-1 {
		t.Fatalf("completed = %d, want %d", got, n-1)
	}
	errCount := 0
	for _, r := range results {
		if r.Err != nil {
			errCount++
		}
	}
	if errCount != 1 {
		t.Fatalf("error count = %d, want 1", errCount)
	}
}
```

## Review

The supervisor is correct when a panic in any worker never escapes its goroutine:
`SafeGo` always delivers exactly one `Result`, and `Pool.Wait` always returns
after every worker finished regardless of how many panicked. The `%w` wrap in
`panicToError` is what keeps a recovered error usable downstream — assert it with
`errors.Is`, not string matching. The two traps to avoid: installing the recover
in the launcher instead of inside the spawned goroutine (it would catch nothing),
and forgetting to buffer the result channel (an unbuffered channel plus a caller
that never reads leaks the goroutine). Run `go test -race` to confirm the pool's
mutex actually guards the results slice under concurrent `record`.

## Resources

- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — why a spawned goroutine must recover itself.
- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup) — coordinating the pool's completion.
- [fmt.Errorf and %w](https://pkg.go.dev/fmt#Errorf) — wrapping a recovered error so errors.Is/As reach it.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-abort-handler-sentinel.md](03-abort-handler-sentinel.md)
