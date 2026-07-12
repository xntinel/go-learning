# 9. Coroutine Library

Building a coroutine library in Go is a study in the duality between cooperative scheduling and channel communication. The hard part is not the idea — it is making the bidirectional rendezvous correct: the caller and the coroutine must hand control to each other without deadlocking, leaking goroutines, or losing panic information. This lesson implements that protocol from the channel level up, then wraps it in a generator abstraction and a round-robin scheduler.

```text
coroutine/
  go.mod
  coroutine.go      (core types, New, Resume, Yield)
  generator.go      (Iterator, Generator)
  scheduler.go      (Scheduler)
  coroutine_test.go
  cmd/demo/main.go
```

## Concepts

### The Channel Rendezvous Protocol

Each coroutine owns two unbuffered channels: `sendCh chan S` carries values from the caller into the coroutine; `yieldCh chan yieldMsg[Y]` carries values from the coroutine back to the caller.

The protocol for `Resume` and `Yield` is a strict alternating rendezvous:

```
caller                    goroutine
------                    ---------
sendCh <- s    ------>    v := <-sendCh   (Yield returns v)
                          ... fn runs ...
<-yieldCh      <------    yieldCh <- msg  (Yield sends result)
```

Both channels are unbuffered. `Resume` blocks until the goroutine yields; `Yield` blocks until the caller resumes. Only one side executes at any moment — the other is parked on a channel operation. This is cooperative multitasking implemented entirely with Go's scheduler.

### The Initial-Send Convention

When `New` creates a coroutine, the backing goroutine immediately parks on `<-sendCh`, waiting for the first `Resume` call. The first `Resume` sends an S value to unblock the goroutine, but that value is discarded — the goroutine has not called `Yield` yet, so there is no Yield return value to populate. The first meaningful S value reaches the coroutine as the return value of the first `Yield` call (on the second `Resume`).

This off-by-one is intentional and mirrors Lua's coroutine protocol and Python's `send(None)` requirement. Callers that want to pass an initial argument can change the coroutine function signature to accept it as a closure variable rather than via Resume.

### State Machine

```
Created --> Running --> Suspended --> Running --> Dead
               |                        |
               +--------> Dead <--------+  (via panic or normal return)
```

- `Created`: `New` was called; the goroutine is parked on `<-sendCh`.
- `Running`: `Resume` has been called; the goroutine is executing `fn`.
- `Suspended`: `fn` called `Yield`; the goroutine is parked on `<-sendCh`.
- `Dead`: `fn` returned or panicked; the goroutine has exited.

`Resume` on a `Dead` coroutine returns `ErrDead`. A mutex guards the `state` field to prevent data races if `State()` is polled from a different goroutine while `Resume` is in-flight. `Resume` itself must not be called concurrently (that is a usage error, detected by the `ErrRunning` guard).

### Panic Containment

A `defer recover()` wraps the coroutine goroutine. When `fn` panics, `recover()` catches the value and sends it to `yieldCh` inside a `yieldMsg` with a non-nil `panic` field. The backing goroutine exits cleanly. The caller's next `Resume` receives the panic message, sets state to `Dead`, and returns a wrapped error — the panic never propagates past the library boundary.

### Generator as a Degenerate Coroutine

A generator only yields; it never receives meaningful input. This is just a coroutine with `S = struct{}` (the zero-size unit type). The `Generator` wrapper creates the coroutine, hides the `struct{}` send value, and exposes a pull-style `Iterator[T]` with a `Next() (T, bool)` method. The iterator is exhausted when the coroutine's underlying function returns.

### Cooperative Scheduler

The `Scheduler` holds a list of registered coroutines and drives them in round-robin order on a single goroutine (the caller's). Each pass through the list resumes every non-Dead coroutine once. When a full pass produces no alive coroutine, the scheduler exits. The key property: the scheduler never runs two coroutines simultaneously; the channel rendezvous guarantees that exactly one goroutine is executing at any moment.

## Exercises

### Exercise 1: Core Types and State Machine

Create `coroutine.go`. The `yieldMsg` struct carries the three possible outcomes of a yield (value, done, panic) in a single channel send, keeping `yieldCh` a single typed channel:

```go
package coroutine

import (
	"errors"
	"fmt"
	"sync"
)

// State describes the lifecycle phase of a coroutine.
type State uint8

const (
	// Created: New was called but Resume has not been called yet.
	Created State = iota
	// Running: Resume has been called and the coroutine has not yet yielded.
	Running
	// Suspended: the coroutine called Yield and is waiting for the next Resume.
	Suspended
	// Dead: the coroutine function returned normally or panicked.
	Dead
)

// String returns the name of the state.
func (s State) String() string {
	switch s {
	case Created:
		return "Created"
	case Running:
		return "Running"
	case Suspended:
		return "Suspended"
	case Dead:
		return "Dead"
	default:
		return fmt.Sprintf("State(%d)", int(s))
	}
}

// Sentinel errors returned by Resume.
var (
	// ErrDead is returned when Resume is called on a Dead coroutine.
	ErrDead = errors.New("coroutine: resume of dead coroutine")
	// ErrRunning is returned when Resume is called on a Running coroutine.
	ErrRunning = errors.New("coroutine: concurrent resume")
)

// yieldMsg is the internal wire type sent from the coroutine goroutine to the
// caller over yieldCh.
type yieldMsg[Y any] struct {
	value Y
	done  bool
	panic any // non-nil when the coroutine panicked
}

// CoroutineContext is the handle given to the coroutine body function. It
// exposes Yield for suspending execution, and Set/Get for coroutine-local
// storage that persists across yields.
type CoroutineContext[S, Y any] struct {
	sendCh  <-chan S
	yieldCh chan<- yieldMsg[Y]
	locals  map[any]any
}

// Yield suspends the coroutine and delivers value to the caller. The return
// value is the S passed to the next Resume call. On the first Resume the
// initial S value is unused — the first meaningful S reaches the coroutine as
// the return value of the first Yield.
func (c *CoroutineContext[S, Y]) Yield(value Y) S {
	c.yieldCh <- yieldMsg[Y]{value: value}
	return <-c.sendCh
}

// Set stores value under key in coroutine-local storage.
func (c *CoroutineContext[S, Y]) Set(key, value any) {
	c.locals[key] = value
}

// Get retrieves a value from coroutine-local storage. It returns the value and
// true if key was found, or the zero value of any and false otherwise.
func (c *CoroutineContext[S, Y]) Get(key any) (any, bool) {
	v, ok := c.locals[key]
	return v, ok
}

// Coroutine is a cooperative coroutine parameterised on:
//
//   - S: the type of values sent in by the caller via Resume.
//   - Y: the type of values yielded out to the caller via Yield.
//
// A single Coroutine must not be resumed concurrently from multiple goroutines.
type Coroutine[S, Y any] struct {
	mu      sync.Mutex
	state   State
	sendCh  chan S
	yieldCh chan yieldMsg[Y]
}

// New creates a coroutine from fn. The coroutine body does not begin executing
// until the first call to Resume.
func New[S, Y any](fn func(co *CoroutineContext[S, Y])) *Coroutine[S, Y] {
	co := &Coroutine[S, Y]{
		state:   Created,
		sendCh:  make(chan S),
		yieldCh: make(chan yieldMsg[Y]),
	}
	go func() {
		var zeroY Y
		defer func() {
			r := recover()
			if r != nil {
				co.yieldCh <- yieldMsg[Y]{panic: r, value: zeroY}
			} else {
				co.yieldCh <- yieldMsg[Y]{done: true, value: zeroY}
			}
		}()
		// Park until the first Resume. The first S value is discarded; it
		// serves only as a start signal.
		<-co.sendCh
		fn(&CoroutineContext[S, Y]{
			sendCh:  co.sendCh,
			yieldCh: co.yieldCh,
			locals:  make(map[any]any),
		})
	}()
	return co
}

// State returns the current lifecycle state. Safe to call from any goroutine.
func (co *Coroutine[S, Y]) State() State {
	co.mu.Lock()
	defer co.mu.Unlock()
	return co.state
}

// Resume resumes (or starts) the coroutine, passing s as the value that
// the coroutine's next Yield call will return. On the first Resume s is
// unused. Resume blocks until the coroutine yields or returns.
//
//   - (value, true, nil)        — the coroutine called Yield(value).
//   - (zero, false, nil)        — the coroutine function returned normally.
//   - (zero, false, err)        — the coroutine panicked.
//   - (zero, false, ErrDead)    — coroutine is already Dead.
//   - (zero, false, ErrRunning) — concurrent Resume detected.
func (co *Coroutine[S, Y]) Resume(s S) (Y, bool, error) {
	co.mu.Lock()
	switch co.state {
	case Dead:
		var zero Y
		co.mu.Unlock()
		return zero, false, fmt.Errorf("%w", ErrDead)
	case Running:
		var zero Y
		co.mu.Unlock()
		return zero, false, fmt.Errorf("%w", ErrRunning)
	}
	co.state = Running
	co.mu.Unlock()

	// Unblock the goroutine: on first Resume this starts fn; subsequently
	// it unblocks the goroutine parked inside Yield.
	co.sendCh <- s
	// Wait for the next yield, normal return, or panic.
	msg := <-co.yieldCh

	co.mu.Lock()
	defer co.mu.Unlock()
	if msg.panic != nil {
		co.state = Dead
		return msg.value, false, fmt.Errorf("coroutine panic: %v", msg.panic)
	}
	if msg.done {
		co.state = Dead
		return msg.value, false, nil
	}
	co.state = Suspended
	return msg.value, true, nil
}
```

### Exercise 2: Generator and Iterator

Create `generator.go`. `Generator` is a one-directional coroutine (`S = struct{}`):

```go
package coroutine

// Iterator is a pull-based sequence of T values produced by a Generator.
type Iterator[T any] struct {
	next func() (T, bool)
}

// Next advances the iterator and returns the next value and true, or the zero
// value and false when the sequence is exhausted.
func (it *Iterator[T]) Next() (T, bool) { return it.next() }

// Generator wraps fn in a coroutine and returns a pull-style Iterator. fn
// calls yield(value) to produce each element. When fn returns the iterator is
// exhausted. The generator uses struct{} as the send type because it only
// yields; it never receives meaningful input.
func Generator[T any](fn func(yield func(T))) *Iterator[T] {
	co := New[struct{}, T](func(ctx *CoroutineContext[struct{}, T]) {
		fn(func(v T) { ctx.Yield(v) })
	})
	return &Iterator[T]{
		next: func() (T, bool) {
			v, alive, _ := co.Resume(struct{}{})
			return v, alive
		},
	}
}
```

### Exercise 3: Round-Robin Scheduler

Create `scheduler.go`. The scheduler drives coroutines on the caller's goroutine in strict round-robin order:

```go
package coroutine

// Scheduler drives a set of coroutines in round-robin order on the calling
// goroutine. All coroutines must share the same S and Y type parameters.
// Scheduler is not safe for concurrent use.
type Scheduler[S, Y any] struct {
	entries []schedEntry[S, Y]
}

type schedEntry[S, Y any] struct {
	co      *Coroutine[S, Y]
	sendVal S
}

// Add registers co with the Scheduler. sendVal is the S passed to every Resume
// of co. Coroutines are resumed in the order they are added.
func (s *Scheduler[S, Y]) Add(co *Coroutine[S, Y], sendVal S) {
	s.entries = append(s.entries, schedEntry[S, Y]{co: co, sendVal: sendVal})
}

// Run resumes all non-Dead coroutines once per round, in add order, until all
// are Dead. onYield is called after each successful Yield with the coroutine
// index and the yielded value; it may be nil.
func (s *Scheduler[S, Y]) Run(onYield func(idx int, value Y)) {
	for {
		anyAlive := false
		for i, e := range s.entries {
			if e.co.State() == Dead {
				continue
			}
			v, alive, _ := e.co.Resume(e.sendVal)
			if alive {
				anyAlive = true
				if onYield != nil {
					onYield(i, v)
				}
			}
		}
		if !anyAlive {
			break
		}
	}
}
```

### Exercise 4: Tests

Create `coroutine_test.go`. The tests are the verification — there is no eyeballed `main` output:

```go
package coroutine

import (
	"errors"
	"fmt"
	"runtime"
	"testing"
	"time"
)

// TestStateTransitions verifies Created -> Running -> Suspended -> Dead.
func TestStateTransitions(t *testing.T) {
	t.Parallel()

	co := New[struct{}, int](func(ctx *CoroutineContext[struct{}, int]) {
		ctx.Yield(1)
	})

	if got := co.State(); got != Created {
		t.Fatalf("initial state = %s, want Created", got)
	}

	type result struct {
		v     int
		alive bool
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		v, alive, err := co.Resume(struct{}{})
		ch <- result{v, alive, err}
	}()

	var got result
	select {
	case got = <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("Resume did not complete within 2s")
	}

	if !got.alive || got.err != nil || got.v != 1 {
		t.Fatalf("first Resume: v=%d alive=%v err=%v, want v=1 alive=true err=nil",
			got.v, got.alive, got.err)
	}
	if s := co.State(); s != Suspended {
		t.Fatalf("after first yield: state = %s, want Suspended", s)
	}

	v2, alive2, err2 := co.Resume(struct{}{})
	if alive2 || err2 != nil || v2 != 0 {
		t.Fatalf("second Resume: v=%d alive=%v err=%v, want v=0 alive=false err=nil",
			v2, alive2, err2)
	}
	if s := co.State(); s != Dead {
		t.Fatalf("after return: state = %s, want Dead", s)
	}
}

// TestYieldSequence verifies that a coroutine yielding N values produces
// exactly those values and that the (N+1)th Resume returns alive=false.
func TestYieldSequence(t *testing.T) {
	t.Parallel()

	co := New[struct{}, int](func(ctx *CoroutineContext[struct{}, int]) {
		ctx.Yield(10)
		ctx.Yield(20)
		ctx.Yield(30)
	})

	want := []int{10, 20, 30}
	for i, w := range want {
		v, alive, err := co.Resume(struct{}{})
		if err != nil {
			t.Fatalf("Resume %d: unexpected error: %v", i+1, err)
		}
		if !alive {
			t.Fatalf("Resume %d: alive=false, want true", i+1)
		}
		if v != w {
			t.Fatalf("Resume %d: v=%d, want %d", i+1, v, w)
		}
	}

	_, alive, err := co.Resume(struct{}{})
	if alive || err != nil {
		t.Fatalf("post-return Resume: alive=%v err=%v, want alive=false err=nil", alive, err)
	}
}

// TestDeadCoroutineError verifies that resuming a Dead coroutine returns ErrDead.
func TestDeadCoroutineError(t *testing.T) {
	t.Parallel()

	co := New[struct{}, int](func(_ *CoroutineContext[struct{}, int]) {})
	co.Resume(struct{}{})

	_, _, err := co.Resume(struct{}{})
	if !errors.Is(err, ErrDead) {
		t.Fatalf("err = %v, want ErrDead", err)
	}
}

// TestBidirectionalCommunication verifies that the value sent via Resume
// reaches the coroutine as the return value of Yield.
//
// Protocol: `v = ctx.Yield(v*2)` sends the current result to the caller and
// receives the next input in a single rendezvous. The first Resume starts fn
// and receives the startup sentinel (0*2=0), which is discarded.
func TestBidirectionalCommunication(t *testing.T) {
	t.Parallel()

	co := New[int, int](func(ctx *CoroutineContext[int, int]) {
		var v int
		for {
			v = ctx.Yield(v * 2) // yield result; receive next input
		}
	})
	co.Resume(0) // start fn; discard startup sentinel (0*2=0)

	cases := []struct{ in, want int }{{3, 6}, {7, 14}, {100, 200}}
	for _, tc := range cases {
		got, alive, err := co.Resume(tc.in)
		if err != nil || !alive {
			t.Fatalf("Resume(%d): alive=%v err=%v", tc.in, alive, err)
		}
		if got != tc.want {
			t.Errorf("in=%d: got=%d, want=%d", tc.in, got, tc.want)
		}
	}
}

// TestPanicContainment verifies that a panicking coroutine does not crash the
// program, that an error is returned on the next Resume, and that subsequent
// calls return ErrDead.
func TestPanicContainment(t *testing.T) {
	t.Parallel()

	co := New[struct{}, int](func(ctx *CoroutineContext[struct{}, int]) {
		ctx.Yield(1)
		panic("something went wrong")
	})

	v1, alive1, err1 := co.Resume(struct{}{})
	if !alive1 || err1 != nil || v1 != 1 {
		t.Fatalf("first Resume: v=%d alive=%v err=%v", v1, alive1, err1)
	}

	_, alive2, err2 := co.Resume(struct{}{})
	if alive2 {
		t.Fatal("alive=true after panic, want false")
	}
	if err2 == nil {
		t.Fatal("err=nil after panic, want non-nil error")
	}
	if s := co.State(); s != Dead {
		t.Fatalf("state = %s, want Dead", s)
	}

	_, _, err3 := co.Resume(struct{}{})
	if !errors.Is(err3, ErrDead) {
		t.Fatalf("err = %v, want ErrDead", err3)
	}
}

// TestCoroutineLocalStorage verifies that Set/Get preserve values across yields.
func TestCoroutineLocalStorage(t *testing.T) {
	t.Parallel()

	type colorKey struct{}

	co := New[struct{}, string](func(ctx *CoroutineContext[struct{}, string]) {
		ctx.Set(colorKey{}, "red")
		ctx.Yield("first")
		v, _ := ctx.Get(colorKey{})
		ctx.Yield(v.(string))
	})

	co.Resume(struct{}{}) // "first"
	got, _, _ := co.Resume(struct{}{})
	if got != "red" {
		t.Fatalf("local storage: got %q, want %q", got, "red")
	}
}

// TestGenerator verifies that Generator produces an incremental Fibonacci sequence.
func TestGenerator(t *testing.T) {
	t.Parallel()

	fib := Generator(func(yield func(int)) {
		a, b := 0, 1
		for {
			yield(a)
			a, b = b, a+b
		}
	})

	want := []int{0, 1, 1, 2, 3, 5, 8, 13, 21, 34}
	for i, w := range want {
		v, ok := fib.Next()
		if !ok {
			t.Fatalf("Next() returned false at index %d", i)
		}
		if v != w {
			t.Errorf("fib[%d] = %d, want %d", i, v, w)
		}
	}
}

// TestGeneratorExhaustion verifies that a finite generator returns false when done.
func TestGeneratorExhaustion(t *testing.T) {
	t.Parallel()

	it := Generator(func(yield func(int)) {
		yield(1)
		yield(2)
	})

	it.Next()
	it.Next()
	_, ok := it.Next()
	if ok {
		t.Fatal("Next() returned true after generator exhausted")
	}
}

// TestSchedulerRoundRobin verifies that 4 coroutines each yielding 5 times are
// interleaved in strict round-robin order (indices 0 1 2 3 repeated 5 times).
func TestSchedulerRoundRobin(t *testing.T) {
	t.Parallel()

	const (
		numCoroutines = 4
		yieldsEach    = 5
	)

	var order []int
	s := &Scheduler[struct{}, int]{}

	for id := 0; id < numCoroutines; id++ {
		id := id
		co := New[struct{}, int](func(ctx *CoroutineContext[struct{}, int]) {
			for i := 0; i < yieldsEach; i++ {
				ctx.Yield(id)
			}
		})
		s.Add(co, struct{}{})
	}

	s.Run(func(idx int, value int) {
		order = append(order, value)
	})

	if len(order) != numCoroutines*yieldsEach {
		t.Fatalf("len(order) = %d, want %d", len(order), numCoroutines*yieldsEach)
	}
	for round := 0; round < yieldsEach; round++ {
		for id := 0; id < numCoroutines; id++ {
			pos := round*numCoroutines + id
			if order[pos] != id {
				t.Errorf("order[%d] = %d, want %d", pos, order[pos], id)
			}
		}
	}
}

// TestNoGoroutineLeaks verifies that coroutines that complete cleanly do not
// leave backing goroutines running.
func TestNoGoroutineLeaks(t *testing.T) {
	t.Parallel()

	runtime.GC()
	runtime.Gosched()
	before := runtime.NumGoroutine()

	for i := 0; i < 10; i++ {
		co := New[struct{}, int](func(ctx *CoroutineContext[struct{}, int]) {
			ctx.Yield(1)
			ctx.Yield(2)
		})
		co.Resume(struct{}{})
		co.Resume(struct{}{})
		co.Resume(struct{}{}) // fn returns; goroutine exits after sending done
	}

	runtime.Gosched()
	time.Sleep(10 * time.Millisecond)
	after := runtime.NumGoroutine()
	if after > before {
		t.Fatalf("goroutine leak: before=%d after=%d (leaked %d)", before, after, after-before)
	}
}

// ExampleGenerator demonstrates the pull-style Fibonacci iterator.
func ExampleGenerator() {
	fib := Generator(func(yield func(int)) {
		a, b := 0, 1
		for {
			yield(a)
			a, b = b, a+b
		}
	})

	for i := 0; i < 8; i++ {
		v, _ := fib.Next()
		if i > 0 {
			fmt.Print(" ")
		}
		fmt.Print(v)
	}
	fmt.Println()
	// Output:
	// 0 1 1 2 3 5 8 13
}
```

Your turn: add `TestLocalStorageIsolated` that creates two coroutines, has each store a different value under the same key, runs them alternately with the Scheduler, and asserts that each sees only its own value when resumed.

### Exercise 5: Demo

Create `cmd/demo/main.go`. Because `cmd/demo` is `package main`, it can only touch exported API:

```go
package main

import (
	"fmt"

	"example.com/coroutine"
)

func main() {
	fmt.Println("=== Fibonacci generator (first 10 values) ===")
	fib := coroutine.Generator(func(yield func(int)) {
		a, b := 0, 1
		for {
			yield(a)
			a, b = b, a+b
		}
	})
	for i := 0; i < 10; i++ {
		v, _ := fib.Next()
		fmt.Printf("  fib[%d] = %d\n", i, v)
	}

	fmt.Println()
	fmt.Println("=== Bidirectional double coroutine ===")
	// v = ctx.Yield(v*2) delivers the current result to the caller and receives
	// the next input in a single channel rendezvous.
	echo := coroutine.New[int, int](func(ctx *coroutine.CoroutineContext[int, int]) {
		var v int
		for {
			v = ctx.Yield(v * 2) // yield result; receive next input
		}
	})
	echo.Resume(0) // start fn; discard startup sentinel (0*2=0)
	for _, n := range []int{3, 7, 42} {
		doubled, _, _ := echo.Resume(n)
		fmt.Printf("  %d * 2 = %d\n", n, doubled)
	}

	fmt.Println()
	fmt.Println("=== Round-robin scheduler (3 coroutines x 3 yields) ===")
	names := []string{"alpha", "beta", "gamma"}
	sched := &coroutine.Scheduler[struct{}, string]{}
	for _, name := range names {
		name := name
		co := coroutine.New[struct{}, string](func(ctx *coroutine.CoroutineContext[struct{}, string]) {
			for i := 1; i <= 3; i++ {
				ctx.Yield(fmt.Sprintf("%s:%d", name, i))
			}
		})
		sched.Add(co, struct{}{})
	}
	sched.Run(func(_ int, value string) {
		fmt.Printf("  %s\n", value)
	})
}
```

Run with `go run ./cmd/demo` from the module root.

## Common Mistakes

### Sending on the Wrong Channel at the Wrong Time

Wrong: sending to `sendCh` twice before the coroutine has a chance to receive — for example, calling `Resume` on a `Running` coroutine from a second goroutine. One send goes to the goroutine's `<-sendCh` inside the rendezvous; the second blocks indefinitely, deadlocking.

What happens: the second `Resume` blocks on `sendCh <- s` because the goroutine is already executing `fn` (not waiting on `sendCh`). Since `sendCh` is unbuffered, neither side can proceed.

Fix: the `ErrRunning` guard detects this case and returns an error instead of blocking. For single-threaded use inside a `Scheduler`, this cannot happen; the check matters only for defensive programming.

### Losing the Doubled Value in Bidirectional Communication

Wrong:
```go
co.Resume(input)            // sends input; receives doubled result — and discards it
got, _, _ := co.Resume(0)  // gets the NEXT sentinel (0), not the doubled value
```

What happens: `co.Resume(input)` returns the doubled result. Calling `co.Resume(0)` immediately after sends `0` to the Yield that was waiting for the next input; fn doubles 0 and yields 0 — not the expected `input*2`.

Fix: capture the return value of the first call: `got, _, _ := co.Resume(input)`. The rendezvous delivers the result in the same call that provides the input.

### Expecting the First Resume's S to Reach the Coroutine

Wrong:
```go
co := New[int, string](func(ctx *CoroutineContext[int, string]) {
	// trying to read the initial value from Resume...
	// there is no way to get it here
})
co.Resume(42) // 42 is discarded
```

What happens: the goroutine's first `<-sendCh` discards the value (it is a start signal). The coroutine body has no way to observe it.

Fix: pass initial data via a closure variable, not via the first Resume:
```go
initialVal := 42
co := New[struct{}, string](func(ctx *CoroutineContext[struct{}, string]) {
	_ = initialVal // captured from outer scope
})
```

### Assuming a Panicking Coroutine Returns the Panic Value From the Same Resume

Wrong: expecting `Resume` to return the panic on the same call that triggered it. The coroutine panics mid-execution; the panic reaches the caller on the *next* `Resume` call because the goroutine's `defer recover()` sends to `yieldCh` *after* the panic, which `Resume` is already waiting on.

Actually, in this implementation, the panic IS returned on the `Resume` that caused the panic — because `Resume` is already blocked on `<-co.yieldCh` when the defer fires. The mistake is assuming the coroutine re-panics on *subsequent* resumes. It does not: state is `Dead`, so subsequent calls return `ErrDead`.

## Verification

From `~/go-exercises/coroutine`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

All five commands must pass. `go test` is the primary verification. The `go run ./cmd/demo` output shows the Fibonacci sequence, bidirectional doubling, and round-robin scheduler interleaving in a single readable trace.

## Summary

- Coroutines in Go are implemented with a pair of unbuffered channels: one carrying S values from caller to coroutine, one carrying Y values from coroutine to caller.
- The channel rendezvous guarantees mutual exclusion without a mutex on the execution path: at any moment exactly one goroutine is executing.
- The first Resume sends a start signal that is discarded; the first meaningful S value reaches the coroutine as the return value of the first Yield.
- A `defer recover()` wraps the coroutine goroutine, converting panics into `yieldMsg` values. The goroutine always exits cleanly; state becomes `Dead`.
- `Generator[T]` is `Coroutine[struct{}, T]` — a coroutine that only yields.
- The round-robin `Scheduler` drives all coroutines from the caller's goroutine: it loops until every entry is `Dead`.
- Coroutine-local storage (`Set`/`Get` on `CoroutineContext`) is a `map[any]any` allocated in `New` and reachable only from inside the coroutine body.

## What's Next

Next: [Wait-Free Stack](../10-wait-free-stack/10-wait-free-stack.md).

## Resources

- [Go specification: Channel types and send statements](https://go.dev/ref/spec#Channel_types) — the formal semantics of unbuffered channel rendezvous.
- [Go Blog: Share Memory by Communicating](https://go.dev/blog/codelab-share) — the design philosophy behind channel-based coordination.
- [pkg.go.dev/sync](https://pkg.go.dev/sync) — Mutex and other synchronization primitives used to guard the state field.
- [Revisiting Coroutines (de Moura & Ierusalimschy, 2009)](https://dl.acm.org/doi/10.1145/1462166.1462167) — the foundational paper classifying coroutine semantics; section 2 covers symmetric vs. asymmetric coroutines.
- [Go 1.23 iter package](https://pkg.go.dev/iter) — the stdlib pull-iterator interface (`iter.Seq`, `iter.Seq2`) that formalizes the same generator pattern built here.
