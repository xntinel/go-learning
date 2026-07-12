# Exercise 10: A Deferred Audit-Hook Queue That Does Not Let Closures Pin Buffers

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

API gateways rarely write an audit record synchronously on the request path,
because a slow audit sink (a SIEM ingest endpoint, a compliance log shipper)
would then set the gateway's own latency floor. The standard fix, used by
Envoy's deferred access logging and by Kong's audit-log plugin, is to queue a
small unit of deferred work per request and run the whole queue together on a
timer or a size threshold. In Go the natural unit of deferred work is a
closure: `func()`. That convenience is also the trap. A closure captures its
free variables by reference, not by value, so `func() { audit(buf) }` does not
capture "the audit record" -- it captures `buf`, whatever `buf` is, for as
long as the closure itself is reachable. If `buf` is the full request or
response body, every queued hook pins its own copy of that body until the
queue is flushed, and a gateway under bursty traffic with a five-second flush
interval can be holding hundreds of megabytes of bodies nobody will ever read
again, purely because the hook closed over the wrong thing.

The failure is invisible in the type signature. `Register(fn func())` looks
identical whether `fn` captures a four-byte status code or a four-megabyte
body -- the compiler cannot tell you which, and neither can a code reviewer
without tracing every closure back to what it references. This is the same
"reachability, not scope, decides liveness" principle behind every other
module in this lesson, applied to a value the language lets you capture
implicitly instead of one you pass explicitly. The fix is a discipline, not a
type change: extract the small value a hook needs *before* building the
closure, so the closure's free variables are already small by the time
`Register` sees them.

This module builds the queue itself -- a bounded `Recorder` that queues hooks,
runs them in order on `Flush`, and is safe for concurrent registration and
flushing. The queue cannot enforce what a caller's closure captures; what it
guarantees is that once `Flush` returns, nothing it ran is reachable through
the `Recorder` anymore, because `Flush` swaps in a fresh backing array instead
of reusing the old one.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
audithooks/               module example.com/audithooks
  go.mod                  go 1.24
  audithooks.go           Recorder; New, Register, Flush, Pending, Capacity; two sentinel errors
  audithooks_test.go      capacity table, full-queue rejection, flush order, the closure-pin
                          contrast via MemStats, concurrency, ExampleRecorder_Flush
```

- Files: `audithooks.go`, `audithooks_test.go`.
- Implement: `New(capacity int) (*Recorder, error)` rejecting a non-positive capacity with `ErrInvalidCapacity`; `(*Recorder).Register(fn func()) error` queuing `fn`, a no-op for `fn == nil`, returning `ErrRecorderFull` once the queue holds `capacity` hooks; `(*Recorder).Flush() int` running every queued hook in order, replacing the internal slice with a fresh one, and returning how many hooks ran; `(*Recorder).Pending() int` and `(*Recorder).Capacity() int`.
- Test: rejected construction; a full queue rejecting a further `Register`; a nil hook consuming no capacity; `Flush` running hooks in registration order and leaving `Pending() == 0`; `Flush` on an empty recorder; the closure-pin contrast -- a hook that captures a large buffer directly keeps it reachable until `Flush`, one that captures only an extracted field does not; safe concurrent `Register`/`Flush`; and `ExampleRecorder_Flush` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### A closure is a reference, and a queued closure is a long-lived one

A Go closure that reads a free variable does not copy it in; it keeps a
reference to the variable itself (or, for a variable captured by a pointer
receiver pattern, to whatever that pointer targets). That reference is exactly
as strong as any other pointer for reachability purposes: as long as the
closure is reachable, so is everything the closure reads. A closure sitting in
a slice inside a `Recorder` is reachable for as long as the `Recorder` holds
it -- which, for a deferred audit hook, means for the entire interval between
the request finishing and the next `Flush`.

```go
// The trap: fn closes directly over buf, the full response body.
func logResponse(r *Recorder, buf []byte) error {
    return r.Register(func() {
        emit(buf) // buf is reachable through fn until Flush runs
    })
}
```

Nothing here is a type error, a race, or a nil pointer. `buf` is used exactly
once, inside the closure, exactly where you would write it if you were only
thinking about correctness. The cost shows up as memory: every request that
calls `logResponse` keeps its whole body alive for the length of the flush
interval, multiplied by however many requests land in that window. The fix
moves one line:

```go
func logResponse(r *Recorder, buf []byte) error {
    status := len(buf)          // extracted now, while buf is still needed anyway
    return r.Register(func() {
        emitStatus(status)      // fn's only free variable is a four-byte int
    })
}
```

`buf` is no longer referenced anywhere the closure can reach, so once
`logResponse` returns and the caller's own reference to `buf` goes out of
scope, the body is ordinary garbage -- reclaimable immediately, not at the
next `Flush`. The `Recorder` cannot see the difference between these two
calls; both pass a plain `func()` to `Register`. The discipline lives entirely
in what the caller writes before that call, which is exactly why the tests
below measure it rather than merely asserting it by inspection.

Create `audithooks.go`:

```go
// Package audithooks batches deferred audit-emission callbacks registered by
// request handlers and runs them together on a flush cycle, the pattern an
// API gateway uses to keep slow audit-log writes off the request's hot path
// (compare Envoy's deferred access logging or Kong's audit-log plugin).
//
// The package cannot enforce the one rule that makes this safe: a
// registered hook must close over only the small values it needs to emit
// one audit record, never a request or response buffer. See the tests for
// what a closure that breaks that rule costs.
package audithooks

import (
	"errors"
	"fmt"
	"sync"
)

var (
	// ErrInvalidCapacity means New was called with a non-positive capacity.
	ErrInvalidCapacity = errors.New("audithooks: capacity must be positive")
	// ErrRecorderFull means Register was called with the queue already at
	// capacity; the caller should flush more often or drop the record.
	ErrRecorderFull = errors.New("audithooks: recorder is full")
)

// Recorder queues deferred hooks up to a fixed capacity and runs them in
// registration order on Flush.
//
// Recorder is safe for concurrent use: Register may be called from any
// number of request-handling goroutines while Flush runs concurrently from
// one dedicated background goroutine.
type Recorder struct {
	mu       sync.Mutex
	hooks    []func()
	capacity int
}

// New returns a Recorder that holds at most capacity pending hooks between
// flushes. It returns ErrInvalidCapacity if capacity is not positive.
func New(capacity int) (*Recorder, error) {
	if capacity <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidCapacity, capacity)
	}
	return &Recorder{hooks: make([]func(), 0, capacity), capacity: capacity}, nil
}

// Capacity reports the maximum number of pending hooks this Recorder holds
// between flushes.
func (r *Recorder) Capacity() int { return r.capacity }

// Pending reports how many hooks are queued right now.
func (r *Recorder) Pending() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.hooks)
}

// Register queues fn to run on the next Flush and returns ErrRecorderFull
// if the queue is already at capacity. A nil fn is a no-op.
//
// fn is the caller's responsibility to build correctly: it must close over
// only the small values needed to emit one audit record. Extract those
// values before calling Register, not inside fn -- a closure that captures
// a request or response buffer directly keeps that buffer reachable for as
// long as the closure itself is reachable, which is until Flush runs.
func (r *Recorder) Register(fn func()) error {
	if fn == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.hooks) >= r.capacity {
		return fmt.Errorf("%w: capacity %d", ErrRecorderFull, r.capacity)
	}
	r.hooks = append(r.hooks, fn)
	return nil
}

// Flush runs every queued hook in registration order, empties the queue,
// and reports how many hooks ran.
//
// Flush swaps in a fresh, empty backing array rather than reusing the old
// one, and runs the hooks without holding the lock, so Register may enqueue
// hooks for the next cycle concurrently with this Flush executing the
// current one. Once Flush returns, the old array -- and every hook in it,
// and everything each hook's closure captured -- has no path back to the
// Recorder and is ordinary GC-reclaimable garbage.
func (r *Recorder) Flush() int {
	r.mu.Lock()
	hooks := r.hooks
	r.hooks = make([]func(), 0, r.capacity)
	r.mu.Unlock()

	for _, fn := range hooks {
		fn()
	}
	return len(hooks)
}
```

### Using it

Construct one `Recorder` at startup with the queue depth your flush interval
and traffic volume can tolerate, register a hook per request from any
handler goroutine, and run a single background goroutine that calls `Flush`
on a timer. `ErrRecorderFull` is the backpressure signal: if it fires
routinely, either the flush interval is too long for the traffic volume, or
individual hooks are too slow, and widening the queue only defers the
decision, it does not remove it.

The one contract `Recorder` cannot check for you is what each hook closes
over, and that is deliberate -- forcing extraction to happen at the type level
would mean inventing a second, restricted closure type, which is exactly the
kind of bespoke abstraction this package avoids by using a plain `func()`.
The discipline is: call `Register` with a closure built from values you have
already pulled out of any buffer you do not want to keep alive past the
current call.

`ExampleRecorder_Flush` is the runnable demonstration of this module: `go
test` runs it and compares its standard output against the `// Output:`
comment, so the usage shown below cannot drift from the code.

```go
func ExampleRecorder_Flush() {
	r, err := New(4)
	if err != nil {
		panic(err)
	}

	// Correct usage: the caller extracts the small value it needs (here,
	// just a byte count) before building the closure. The raw response
	// buffer itself never enters the hook, so it never gets pinned by it.
	logResponse := func(body []byte) error {
		status := len(body)
		return r.Register(func() {
			fmt.Printf("audit: served %d bytes\n", status)
		})
	}

	if err := logResponse([]byte("ok")); err != nil {
		panic(err)
	}
	if err := logResponse([]byte("a longer response body")); err != nil {
		panic(err)
	}

	fmt.Println("pending:", r.Pending())
	fmt.Println("ran:", r.Flush())
	fmt.Println("pending:", r.Pending())

	// Output:
	// pending: 2
	// audit: served 2 bytes
	// audit: served 22 bytes
	// ran: 2
	// pending: 0
}
```

### Tests

`TestNewRejectsNonPositiveCapacity` and `TestRegisterRejectsFullQueue` pin the
two sentinel errors against `errors.Is`. `TestRegisterNilIsNoOp` checks that a
nil hook costs nothing against the capacity budget.
`TestFlushRunsInOrderAndEmptiesQueue` and `TestFlushOnEmptyRecorder` pin the
ordering and idempotence of `Flush`.

`TestLeakyHookPinsBufferUntilFlush` and `TestEagerExtractionDoesNotPinBuffer`
are the heart of the module. They reuse the discipline from Exercise 2: force
GC twice, read `HeapAlloc`, compare an 8 MiB allocation's delta against a
4 MiB threshold. The leaky test registers `registerLeaky`, an unexported test
helper that closes directly over the buffer -- never exported, never
reachable from the package API -- and shows the buffer is still pinned right
up until `Flush` runs, then reclaimed after. The eager test performs the
extraction first and shows the buffer never gets pinned in the first place,
even though `Flush` has not run yet. Neither test calls `t.Parallel`:
`HeapAlloc` is process-global, so a concurrently allocating test would
perturb the reading.

`TestRecorderIsSafeForConcurrentUse` registers 200 hooks from 200 goroutines
and flushes once, under `-race`.

Create `audithooks_test.go`:

```go
package audithooks

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
)

// readHeap returns HeapAlloc after two full GC cycles. The second GC
// completes the sweep started by the first, so the reading is stable.
func readHeap() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// fillBuf allocates an n-byte slice and writes a pattern across it so the
// pages are committed and the object is a genuine, individually reclaimable
// large allocation.
func fillBuf(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

// registerLeaky is the antipattern this module exists to catch: it closes
// directly over the caller's buffer instead of extracting what the hook
// needs first, so the buffer stays reachable through the hook -- and
// through the Recorder's queue -- until Flush finally runs it.
func registerLeaky(r *Recorder, buf []byte) error {
	return r.Register(func() {
		_ = len(buf) // pretend to use buf when the hook eventually runs
	})
}

func TestNewRejectsNonPositiveCapacity(t *testing.T) {
	t.Parallel()

	for _, c := range []int{0, -1} {
		if _, err := New(c); !errors.Is(err, ErrInvalidCapacity) {
			t.Errorf("New(%d) error = %v, want ErrInvalidCapacity", c, err)
		}
	}
}

func TestRegisterRejectsFullQueue(t *testing.T) {
	t.Parallel()

	r, err := New(2)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	noop := func() {}
	if err := r.Register(noop); err != nil {
		t.Fatalf("Register 1: %v", err)
	}
	if err := r.Register(noop); err != nil {
		t.Fatalf("Register 2: %v", err)
	}
	if err := r.Register(noop); !errors.Is(err, ErrRecorderFull) {
		t.Fatalf("Register 3 error = %v, want ErrRecorderFull", err)
	}
	if got := r.Pending(); got != 2 {
		t.Fatalf("Pending() = %d, want 2", got)
	}
}

func TestRegisterNilIsNoOp(t *testing.T) {
	t.Parallel()

	r, err := New(1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := r.Register(nil); err != nil {
		t.Fatalf("Register(nil) = %v, want nil", err)
	}
	if got := r.Pending(); got != 0 {
		t.Fatalf("Pending() = %d, want 0", got)
	}
	// A nil hook must not consume a capacity slot.
	if err := r.Register(func() {}); err != nil {
		t.Fatalf("Register after nil: %v", err)
	}
}

func TestFlushRunsInOrderAndEmptiesQueue(t *testing.T) {
	t.Parallel()

	r, err := New(8)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var order []int
	for i := range 5 {
		i := i
		if err := r.Register(func() { order = append(order, i) }); err != nil {
			t.Fatalf("Register %d: %v", i, err)
		}
	}
	if ran := r.Flush(); ran != 5 {
		t.Fatalf("Flush ran = %d, want 5", ran)
	}
	want := []int{0, 1, 2, 3, 4}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i, v := range want {
		if order[i] != v {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
	if got := r.Pending(); got != 0 {
		t.Fatalf("Pending() after Flush = %d, want 0", got)
	}
}

func TestFlushOnEmptyRecorder(t *testing.T) {
	t.Parallel()

	r, err := New(4)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if ran := r.Flush(); ran != 0 {
		t.Fatalf("Flush on empty = %d, want 0", ran)
	}
}

// TestLeakyHookPinsBufferUntilFlush is the core of this module: a hook that
// closes directly over a large buffer keeps that buffer reachable through
// the Recorder's queue, and only Flush -- which drops the whole queue --
// releases it.
//
// This test deliberately does not call t.Parallel: it forces GC and reads
// process-global heap stats, which a concurrently allocating goroutine
// would perturb.
func TestLeakyHookPinsBufferUntilFlush(t *testing.T) {
	const bufSize = 8 << 20 // 8 MiB
	half := int64(bufSize) / 2

	base := readHeap()

	r, err := New(1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	buf := fillBuf(bufSize)
	if err := registerLeaky(r, buf); err != nil {
		t.Fatalf("registerLeaky: %v", err)
	}
	buf = nil // drop the only other reference; the hook alone can pin it now

	pinned := readHeap()
	if delta := int64(pinned) - int64(base); delta < half {
		t.Fatalf("leaky hook did not pin the buffer before Flush: delta %d bytes, want >= %d", delta, half)
	}

	if ran := r.Flush(); ran != 1 {
		t.Fatalf("Flush ran = %d, want 1", ran)
	}
	after := readHeap()
	if delta := int64(after) - int64(base); delta >= half {
		t.Fatalf("buffer still pinned after Flush: delta %d bytes, want < %d", delta, half)
	}
}

// TestEagerExtractionDoesNotPinBuffer is the fix, pinned the same way: when
// the caller extracts the small value it needs before building the
// closure, the large buffer is reclaimable immediately, without waiting
// for Flush at all.
func TestEagerExtractionDoesNotPinBuffer(t *testing.T) {
	const bufSize = 8 << 20
	half := int64(bufSize) / 2

	base := readHeap()

	r, err := New(1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	buf := fillBuf(bufSize)
	size := len(buf) // extracted before the closure is built
	if err := r.Register(func() { _ = size }); err != nil {
		t.Fatalf("Register: %v", err)
	}
	buf = nil

	after := readHeap()
	if delta := int64(after) - int64(base); delta >= half {
		t.Fatalf("eager extraction still pinned the buffer: delta %d bytes, want < %d", delta, half)
	}
	if ran := r.Flush(); ran != 1 {
		t.Fatalf("Flush ran = %d, want 1", ran)
	}
}

func TestRecorderIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	r, err := New(1000)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	ran := 0
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.Register(func() {
				mu.Lock()
				ran++
				mu.Unlock()
			})
		}()
	}
	wg.Wait()
	r.Flush()

	mu.Lock()
	defer mu.Unlock()
	if ran == 0 {
		t.Fatal("no hooks ran; expected concurrently registered hooks to be flushed")
	}
}

// ExampleRecorder_Flush is the runnable demonstration of this module: go
// test executes it and compares its stdout against the Output comment.
func ExampleRecorder_Flush() {
	r, err := New(4)
	if err != nil {
		panic(err)
	}

	// Correct usage: the caller extracts the small value it needs (here,
	// just a byte count) before building the closure. The raw response
	// buffer itself never enters the hook, so it never gets pinned by it.
	logResponse := func(body []byte) error {
		status := len(body)
		return r.Register(func() {
			fmt.Printf("audit: served %d bytes\n", status)
		})
	}

	if err := logResponse([]byte("ok")); err != nil {
		panic(err)
	}
	if err := logResponse([]byte("a longer response body")); err != nil {
		panic(err)
	}

	fmt.Println("pending:", r.Pending())
	fmt.Println("ran:", r.Flush())
	fmt.Println("pending:", r.Pending())

	// Output:
	// pending: 2
	// audit: served 2 bytes
	// audit: served 22 bytes
	// ran: 2
	// pending: 0
}
```

## Review

`Recorder` is correct when a hook queued via `Register` runs exactly once, in
order, on the next `Flush`, and when `Flush` leaves nothing behind it ran --
`TestFlushRunsInOrderAndEmptiesQueue` pins both halves of that. The lesson the
module exists to teach is not in `Recorder` at all: it is in what the caller's
closure captures. A hook that closes directly over a large buffer keeps that
buffer reachable for as long as the hook is queued, which can be an entire
flush interval under bursty traffic; a hook that closes over a value already
extracted from the buffer lets the buffer go the moment the caller's own
reference drops. `TestLeakyHookPinsBufferUntilFlush` and
`TestEagerExtractionDoesNotPinBuffer` measure that difference directly with a
`runtime.ReadMemStats` delta rather than asserting it by inspection.
`ErrInvalidCapacity` and `ErrRecorderFull` are both checkable with
`errors.Is`, and `Recorder` is safe for concurrent `Register` and `Flush`
because `Flush` swaps in a fresh backing array under the lock and runs hooks
without holding it. Run `go test -count=1 -race ./...`.

## Resources

- [Go Language Specification: Closures](https://go.dev/ref/spec#Function_literals) — function literals capture variables by reference, not by value.
- [`runtime.MemStats` and `ReadMemStats`](https://pkg.go.dev/runtime#MemStats) — the leak-detection technique this module's core test reuses from Exercise 2.
- [Envoy documentation: Access logging](https://www.envoyproxy.io/docs/envoy/latest/configuration/observability/access_log/access_log) — a real deferred, batched audit/access-log pipeline of the shape this module models.
- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — the synchronization primitive protecting `Recorder`'s internal queue.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-weak-canonicalization-cache.md](09-weak-canonicalization-cache.md) | Next: [11-ct-merkle-leaf-array-dedup.md](11-ct-merkle-leaf-array-dedup.md)
