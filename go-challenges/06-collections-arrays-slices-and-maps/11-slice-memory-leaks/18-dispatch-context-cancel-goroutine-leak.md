# Exercise 18: The Missing cancel() That Leaks a Goroutine and Its Buffer

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Every discussion of Go memory leaks eventually arrives at slices and maps
that pin backing arrays, and every one of those bugs has a return path: a
function returns, a struct is garbage collected, the array becomes
unreachable. The leak in this module has no return path. A worker goroutine
started to process one request blocks forever on a channel send nobody is
receiving from, and a blocked goroutine is not garbage: its stack is a root
the collector traces from, exactly like a global variable or another
goroutine's local variable. Everything that goroutine's closure captured --
here, the request payload it was asked to process -- stays reachable for as
long as the goroutine stays parked, which in this shape of bug is forever.
This is, by a wide margin, the most common goroutine and memory leak
reported against real Go services: an RPC client, an HTTP handler, or a
worker-pool dispatcher that starts a cancelable operation and then, on some
code path, neither reads its result nor cancels its context.

The fix has nothing to do with slices or maps directly and everything to do
with reachability, the foundational idea underneath every other module in
this lesson: an object lives as long as something can still reach it, and a
parked goroutine counts as "something." `context.Context` exists
specifically to give a blocked goroutine a second way out of its blocking
operation besides completing it -- and every `context.WithCancel` or
`context.WithTimeout` comes with a `cancel` function that a caller must
call, even on the success path, or that second way out is never taken. Go
vet's `lostcancel` check exists because engineers drop that call constantly;
this module builds the component correctly and reproduces, deterministically
and without a single `time.Sleep`, exactly what happens when a caller drops
it too.

This module builds `dispatch`, a small job-dispatch package whose
`Dispatch` method is the one correct way to start a cancelable worker. The
caller mistake that leaks it is not part of that API -- there is no
`DispatchAndForget` or `unsafe` flag. It lives in the test file, as the
thing the tests prove wrong.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
dispatch/                module example.com/dispatch
  go.mod                 go 1.24
  dispatch.go            Job, Result, Handle, Dispatcher; NewDispatcher, Dispatch
  dispatch_test.go        checksum table, nil-context and buffer edges, the
                          leaked-vs-released goroutine contrast, ExampleDispatcher_Dispatch
```

- Files: `dispatch.go`, `dispatch_test.go`.
- Implement: `NewDispatcher(resultBuffer int) (*Dispatcher, error)` rejecting a negative buffer with `ErrInvalidBufferSize`; `(*Dispatcher).Dispatch(ctx context.Context, job Job) (*Handle, error)` rejecting a nil context with `ErrNilContext`, spawning a worker goroutine that selects between sending its `Result` and `ctx.Done()`; `Handle.Results() <-chan Result` and `Handle.Done() <-chan struct{}`.
- Test: a checksum table across empty, single-byte, wrapping, and ordinary payloads; `NewDispatcher` and `Dispatch` rejecting their two invalid inputs; the contrast between `leakyCaller`, which dispatches and neither reads nor cancels, and the documented pattern of a deferred `cancel` alongside reading the result; `ExampleDispatcher_Dispatch` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### A parked goroutine is a GC root, not a stray reference

Every slice and map leak in this lesson shares one shape: a small,
reachable value keeps a large one alive because the small value holds a
pointer into it. This module's leak has the same shape, but the "small,
reachable value" is a goroutine's own stack -- specifically, the local
variables a running (or blocked) goroutine still has in scope, including
whatever its closure captured when it was started. The runtime cannot tell
the difference between "this goroutine is doing useful work" and "this
goroutine is parked forever waiting for something that will never happen";
both keep every value the goroutine can still reach exactly as alive.

The naive version of a dispatch helper looks entirely reasonable:

```go
// dispatch.go -- the bug, if Dispatch had no way to give up.
func (d *Dispatcher) dispatchAndForget(job Job) <-chan Result {
    results := make(chan Result) // unbuffered
    go func() {
        results <- Result{ID: job.ID, Sum: checksum(job.Payload)} // blocks forever if nobody reads
    }()
    return results
}
```

If the caller reads from the returned channel, this works. If the caller
decides it no longer needs the result -- a client disconnected, a request
was superseded, an error elsewhere made the work moot -- the goroutine
above has no way to find out. It blocks on `results <-` permanently.
`job.Payload`, captured by the closure, is unreachable from anywhere else
in the program but perfectly reachable from that goroutine's stack, so it
is never collected. One leaked goroutine is nothing; a service leaking one
per abandoned request, under sustained load, is a slow, silent climb in RSS
that a profiler eventually traces back to a `chan Result` nobody drained.

The fix is to give the goroutine a second condition to select on --
`ctx.Done()` -- and to make every caller responsible for closing that door,
via `cancel()`, even when it also plans to read the result. That is the
whole of what `Dispatch` below does differently.

Create `dispatch.go`:

```go
// Package dispatch runs a job in its own goroutine and hands the caller a
// Handle to observe it, the pattern underneath most RPC and job-queue
// client wrappers (an outbound gRPC call, a task dispatched to a worker
// pool). It exists to make one failure mode visible: the worker goroutine
// captures the job's payload in its closure, and it only ever stops
// running -- releasing that payload to the garbage collector -- if the
// caller either reads its result or cancels the context passed to
// Dispatch. A caller that does neither leaves the goroutine parked forever
// on a channel send nobody is receiving from, and the payload it captured
// stays reachable, and therefore unreclaimed, for the life of the process.
// See the package tests for that leak reproduced and released.
package dispatch

import (
	"context"
	"errors"
	"fmt"
)

// Sentinel errors returned by NewDispatcher and Dispatch. Callers should
// test for them with errors.Is rather than by comparing error strings.
var (
	// ErrInvalidBufferSize means the configured result buffer size was negative.
	ErrInvalidBufferSize = errors.New("dispatch: result buffer size must not be negative")
	// ErrNilContext means Dispatch was called with a nil context.Context.
	ErrNilContext = errors.New("dispatch: ctx must not be nil")
)

// Job is one unit of work submitted to a Dispatcher.
type Job struct {
	ID string
	// Payload is the data the job processes. Dispatch retains a reference
	// to it inside the spawned goroutine's closure for as long as that
	// goroutine runs; see Dispatcher's doc comment for what keeps it
	// running.
	Payload []byte
}

// Result is what a Job produces.
type Result struct {
	ID  string
	Sum byte
}

// checksum stands in for real per-job processing: a deterministic,
// order-independent digest of Payload.
func checksum(payload []byte) byte {
	var sum byte
	for _, b := range payload {
		sum += b
	}
	return sum
}

// Handle represents one in-flight dispatched Job.
//
// The caller must eventually either receive from Results or cancel the
// context given to Dispatch (ideally both: a deferred cancel alongside a
// received result, so an early return still releases the worker). Doing
// neither leaks the goroutine Dispatch started.
type Handle struct {
	results <-chan Result
	done    <-chan struct{}
}

// Results returns the channel the job's result is sent on. It carries at
// most one value and is never closed; receive at most once from it.
func (h *Handle) Results() <-chan Result { return h.results }

// Done returns a channel that is closed when the worker goroutine backing
// this Handle has returned, whether because its result was received or
// because its context was canceled. It is intended for tests and
// diagnostics that need to observe the goroutine's lifetime; ordinary
// callers only need Results.
func (h *Handle) Done() <-chan struct{} { return h.done }

// Dispatcher starts jobs and reports their results through a Handle.
//
// Dispatcher holds no mutable state after construction and is safe for
// concurrent use by multiple goroutines.
type Dispatcher struct {
	resultBuffer int
}

// NewDispatcher returns a Dispatcher whose result channels are buffered by
// resultBuffer slots. It returns ErrInvalidBufferSize if resultBuffer is
// negative. A buffer of 0 is valid: it makes every result channel
// unbuffered, which is what makes the leak in this package's tests
// reproducible on demand rather than only under timing pressure.
func NewDispatcher(resultBuffer int) (*Dispatcher, error) {
	if resultBuffer < 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidBufferSize, resultBuffer)
	}
	return &Dispatcher{resultBuffer: resultBuffer}, nil
}

// Dispatch starts job in its own goroutine and returns a Handle to observe
// it. It returns ErrNilContext if ctx is nil.
//
// The spawned goroutine computes job's result and then blocks on a select
// between sending that result on the Handle's Results channel and ctx
// being done. If the caller never receives from Results and never cancels
// ctx, that select blocks forever: the goroutine, and everything its
// closure captured -- including job.Payload -- stays reachable for the
// life of the process. This is not a defect in Dispatch; it is the
// ordinary cost of a blocking channel send, and it is why every caller
// must pair Dispatch with either draining Results or canceling ctx.
func (d *Dispatcher) Dispatch(ctx context.Context, job Job) (*Handle, error) {
	if ctx == nil {
		return nil, ErrNilContext
	}
	results := make(chan Result, d.resultBuffer)
	done := make(chan struct{})
	go func() {
		defer close(done)
		res := Result{ID: job.ID, Sum: checksum(job.Payload)}
		select {
		case results <- res:
		case <-ctx.Done():
		}
	}()
	return &Handle{results: results, done: done}, nil
}
```

### Using it

Construct one `Dispatcher` per process or per subsystem -- it holds no
per-call state, so a single value is safe to share across every goroutine
that submits jobs. Every call to `Dispatch` must be paired with a `defer
cancel()` immediately after it, exactly as with any `context.WithCancel`:
that is what guarantees the worker goroutine exits even if the caller
returns early, panics and recovers elsewhere, or simply never gets around
to reading `Results`. Reading `Results` is what a normal, successful call
does in addition to that; it is not a substitute for canceling.

`ExampleDispatcher_Dispatch`, in the test file below, is the runnable
demonstration of this module: `go test` runs it and checks its output
against the `// Output:` comment, so it cannot drift from the code it
demonstrates. `Handle.Done()` is not part of that ordinary usage pattern --
it exists for tests and leak diagnostics that need to observe when the
worker goroutine has actually returned, which is exactly what this
module's tests use it for.

### Tests

`TestDispatchComputesChecksum` is the ordinary correctness table: empty
payload, a single byte, a sum that wraps past 255, and ordinary text,
each dispatched and read back through the documented `defer
cancel()`-plus-receive pattern. `TestNewDispatcherRejectsNegativeBuffer` and
`TestDispatchRejectsNilContext` cover the two constructor and call-site
edges.

`TestNotReceivingOrCancelingLeaksTheWorkerGoroutine` is the module's center
of gravity. `leakyCaller` is unexported and unreachable from the package
API; it dispatches with `context.Background()` -- a context that is never
canceled -- and returns without ever touching `Results`. Immediately
afterward, a *non-blocking* receive on `Done` (a `select` with a `default`
case, not a real wait) proves the worker has not exited: nothing in the
leaky path could possibly have unblocked its `select` yet, so this
assertion is deterministic, not a race against timing. The test then drains
`Results` itself, purely so the test suite does not exit with a goroutine
still parked, and confirms the drain is what finally lets `Done` close.
`TestCancelReleasesTheWorkerWithoutReceiving` is the direct contrast: a
caller that calls `cancel()` without ever reading `Results` still releases
the worker, because the goroutine's `select` picks the `ctx.Done()` branch
instead. Both tests use blocking or non-blocking channel operations
exclusively -- no `time.Sleep` anywhere -- which is what makes the leak
proof reproducible on every run rather than flaky under load.

Create `dispatch_test.go`:

```go
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestDispatchComputesChecksum(t *testing.T) {
	t.Parallel()

	d, err := NewDispatcher(1)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}

	tests := []struct {
		name    string
		payload []byte
		want    byte
	}{
		{name: "empty payload", payload: nil, want: 0},
		{name: "single byte", payload: []byte{7}, want: 7},
		{name: "wraps mod 256", payload: []byte{255, 2}, want: 1},
		{name: "ordinary text", payload: []byte("hi"), want: 'h' + 'i'},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			h, err := d.Dispatch(ctx, Job{ID: tc.name, Payload: tc.payload})
			if err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			res := <-h.Results()
			<-h.Done()
			if res.Sum != tc.want {
				t.Errorf("Sum = %d, want %d", res.Sum, tc.want)
			}
		})
	}
}

func TestNewDispatcherRejectsNegativeBuffer(t *testing.T) {
	t.Parallel()

	if _, err := NewDispatcher(-1); !errors.Is(err, ErrInvalidBufferSize) {
		t.Errorf("NewDispatcher(-1) error = %v, want ErrInvalidBufferSize", err)
	}
}

func TestDispatchRejectsNilContext(t *testing.T) {
	t.Parallel()

	d, err := NewDispatcher(0)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	if _, err := d.Dispatch(nil, Job{ID: "x"}); !errors.Is(err, ErrNilContext) {
		t.Errorf("Dispatch(nil, ...) error = %v, want ErrNilContext", err)
	}
}

// leakyCaller reproduces the mistake this package's doc comment warns
// against: it dispatches a job and returns immediately, never receiving
// from Results and never canceling the context it passed in. It is never
// exported and never reachable from the package API; it exists only so
// the tests can pin the leak it causes.
func leakyCaller(t *testing.T, d *Dispatcher, job Job) *Handle {
	t.Helper()
	h, err := d.Dispatch(context.Background(), job)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	return h
}

// TestNotReceivingOrCancelingLeaksTheWorkerGoroutine is the heart of the
// module. leakyCaller dispatches without ever draining Results or
// canceling its context, so the worker goroutine's select has nothing
// ready to pick: it stays parked, and job.Payload stays reachable through
// its closure. A non-blocking receive on Done immediately after Dispatch
// returns proves the goroutine has not exited -- deterministically, since
// nothing in the leaky path could possibly have unblocked it yet.
func TestNotReceivingOrCancelingLeaksTheWorkerGoroutine(t *testing.T) {
	t.Parallel()

	d, err := NewDispatcher(0)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}

	payload := make([]byte, 4096) // stands in for a large retained buffer
	h := leakyCaller(t, d, Job{ID: "leaky", Payload: payload})

	select {
	case <-h.Done():
		t.Fatal("worker exited without its result being read or its context being canceled")
	default:
	}

	// Release it for hygiene, proving the leak is a stuck send rather than
	// a permanently blocked goroutine: draining Results unblocks the
	// select, and the goroutine then exits normally.
	<-h.Results()
	<-h.Done()
}

// TestCancelReleasesTheWorkerWithoutReceiving is the contrast: a caller
// that gives up on the result still releases the worker, as long as it
// cancels the context it passed to Dispatch.
func TestCancelReleasesTheWorkerWithoutReceiving(t *testing.T) {
	t.Parallel()

	d, err := NewDispatcher(0)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	h, err := d.Dispatch(ctx, Job{ID: "canceled", Payload: []byte("x")})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	cancel() // the correct release when the caller gives up on the result
	<-h.Done()
}

// ExampleDispatcher_Dispatch is the runnable demonstration of this module:
// go test executes it and compares its stdout against the Output comment
// below.
func ExampleDispatcher_Dispatch() {
	d, err := NewDispatcher(1)
	if err != nil {
		panic(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // always released, whether or not the result is read

	h, err := d.Dispatch(ctx, Job{ID: "job-1", Payload: []byte("hello")})
	if err != nil {
		panic(err)
	}

	res := <-h.Results()
	<-h.Done()
	fmt.Printf("id=%s sum=%d\n", res.ID, res.Sum)

	// Output:
	// id=job-1 sum=20
}
```

## Review

`Dispatch` is correct when its worker goroutine is guaranteed to exit under
every caller behavior: reading the result, canceling early, or both. It
achieves that with one `select` over two cases, not with a timeout, a
retry, or a background sweeper -- the fix for a goroutine leak of this
shape is always "give the blocked operation a second way out," and
`ctx.Done()` is that second way out. `NewDispatcher` rejects a negative
result buffer with `ErrInvalidBufferSize`, and `Dispatch` rejects a nil
context with `ErrNilContext`, both checkable with `errors.Is`. The leak
itself is never reachable through the package API -- `leakyCaller` lives
only in the test file, confined to demonstrating the caller mistake, not
offered as a mode. The pair of goroutine-lifetime tests is what proves the
mechanism deterministically: a non-blocking receive on `Done` immediately
after a leaky dispatch proves the worker has not exited, and a blocking
receive on `Done` immediately after `cancel()` proves it has, with no
`time.Sleep` and no timing assumption anywhere in either proof. Run `go
test -count=1 -race ./...`.

## Resources

- [`context` package](https://pkg.go.dev/context) — `WithCancel`, `Done`, and the contract that every `cancel` function must be called.
- [Go vet: lostcancel](https://pkg.go.dev/cmd/vet#hdr-lostcancel) — the static check that flags a `context.WithCancel` whose `cancel` is never called on some path.
- [Go Blog: Contexts and structs](https://go.dev/blog/context-and-structs) — why cancellation is threaded explicitly through call chains rather than stored.
- [`runtime.Stack`](https://pkg.go.dev/runtime#Stack) — the tool that turns "the process is leaking memory" into "these N goroutines are all parked at the same select" in a real incident.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [17-replay-queue-ack-compaction.md](17-replay-queue-ack-compaction.md) | Next: [19-hashring-snapshot-in-place-corruption.md](19-hashring-snapshot-in-place-corruption.md)
