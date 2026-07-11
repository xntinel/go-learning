# Exercise 5: Spinlock with Atomic CAS

A spinlock is the simplest possible mutex: `Lock()` spins in a CAS loop until it
wins the lock, `Unlock()` atomically releases it. No OS involvement, no goroutine
parking, no scheduler interaction -- when the lock is uncontended it acquires in
a single compare-and-swap, faster than anything else. Building one from scratch
sharpens your grasp of CAS and exposes the trade-off that makes Go's standard
library refuse to ship one: a spinning goroutine burns CPU instead of sleeping,
and with `GOMAXPROCS=1` it can hold the only OS thread hostage and deadlock the
program.

## What you'll build

```text
05-spinlock-with-atomic/
  main.go        a CAS-based Spinlock, then benchmarks against sync.Mutex
                 and atomics, plus the missing-Gosched deadlock demo
```

- Build: a `Spinlock` built from `atomic.CompareAndSwapInt32`, then measured against `sync.Mutex` and raw atomics under varying contention.
- Implement: `Lock`/`Unlock`/`TryLock`, a contention benchmark harness, the no-`Gosched` deadlock demonstration under `GOMAXPROCS=1`, and a three-way counter comparison (spinlock vs mutex vs atomic).
- Verify: `go run main.go`, and `go run -race main.go` to confirm the spinlock actually guards the counter.

### Why sync.Mutex wins in Go

Building one from scratch solidifies your understanding of CAS and reveals a
fundamental trade-off: a spinlock wastes CPU while waiting. The goroutine runs a
tight loop instead of sleeping. In Go, where the runtime multiplexes thousands
of goroutines onto a few OS threads, a spinning goroutine holds an OS thread
hostage and prevents other goroutines from running. With `GOMAXPROCS=1`, this
causes deadlock.

`sync.Mutex` is a hybrid: it spins briefly (optimistic fast path) then parks the
goroutine with the OS scheduler (pessimistic slow path). You get spinlock speed
when the lock is quickly available, and sleeping efficiency when it is not. For
virtually all Go code, `sync.Mutex` is the right tool.

This exercise is educational. You should understand how spinlocks work so you
can recognize them in systems code and understand why Go's standard library
avoids exposing them.

## Step 1 -- Build a Spinlock and Prove Correctness

Implement `Lock()`, `Unlock()`, and `TryLock()` using `atomic.CompareAndSwapInt32`:

```go
package main

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
)

const (
	spinUnlocked int32 = 0
	spinLocked   int32 = 1

	goroutineCount   = 100
	incrementsPerG   = 1000
	expectedTotal    = goroutineCount * incrementsPerG
)

type Spinlock struct {
	state int32
}

func (s *Spinlock) Lock() {
	for !atomic.CompareAndSwapInt32(&s.state, spinUnlocked, spinLocked) {
		runtime.Gosched()
	}
}

func (s *Spinlock) Unlock() {
	atomic.StoreInt32(&s.state, spinUnlocked)
}

func (s *Spinlock) TryLock() bool {
	return atomic.CompareAndSwapInt32(&s.state, spinUnlocked, spinLocked)
}

func incrementWithSpinlock(lock *Spinlock, counter *int64, wg *sync.WaitGroup) {
	defer wg.Done()
	for j := 0; j < incrementsPerG; j++ {
		lock.Lock()
		*counter++
		lock.Unlock()
	}
}

func verifySpinlockCorrectness(lock *Spinlock) {
	var counter int64
	var wg sync.WaitGroup

	for i := 0; i < goroutineCount; i++ {
		wg.Add(1)
		go incrementWithSpinlock(lock, &counter, &wg)
	}

	wg.Wait()
	fmt.Printf("Expected: %d, Got: %d\n", expectedTotal, counter)
}

func demonstrateTryLock(lock *Spinlock) {
	lock.Lock()
	fmt.Printf("TryLock while held: %v (expected false)\n", lock.TryLock())
	lock.Unlock()
	fmt.Printf("TryLock while free: %v (expected true)\n", lock.TryLock())
	lock.Unlock()
}

func main() {
	var lock Spinlock
	verifySpinlockCorrectness(&lock)
	demonstrateTryLock(&lock)
}
```

### Verification
```bash
go run -race main.go
```
Counter is exactly 100,000. TryLock returns false when held, true when free. No race warnings.

## Step 2 -- Measure CPU Waste Under Contention

Show that spinlocks burn CPU while waiting. Compare wall-clock time AND CPU time between spinlock and mutex under high contention:

```go
package main

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

const (
	spinUnlocked int32 = 0
	spinLocked   int32 = 1
)

type Spinlock struct {
	state int32
}

func (s *Spinlock) Lock() {
	for !atomic.CompareAndSwapInt32(&s.state, spinUnlocked, spinLocked) {
		runtime.Gosched()
	}
}

func (s *Spinlock) Unlock() {
	atomic.StoreInt32(&s.state, spinUnlocked)
}

type ContentionScenario struct {
	Name       string
	Goroutines int
	Iterations int
	WorkNanos  int
}

type ComparisonResult struct {
	SpinlockTime time.Duration
	MutexTime    time.Duration
}

func (r ComparisonResult) Winner() string {
	if float64(r.SpinlockTime)/float64(r.MutexTime) < 1 {
		return "Spinlock"
	}
	return "Mutex"
}

func (r ComparisonResult) Ratio() float64 {
	return float64(r.SpinlockTime) / float64(r.MutexTime)
}

func simulateCriticalSection(workNanos int) {
	deadline := time.Now().Add(time.Duration(workNanos))
	for time.Now().Before(deadline) {
	}
}

func benchmarkSpinlock(goroutines, iterations, workNanos int) time.Duration {
	var lock Spinlock
	var counter int64
	var wg sync.WaitGroup

	start := time.Now()
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				lock.Lock()
				counter++
				simulateCriticalSection(workNanos)
				lock.Unlock()
			}
		}()
	}
	wg.Wait()
	return time.Since(start)
}

func benchmarkMutex(goroutines, iterations, workNanos int) time.Duration {
	var mu sync.Mutex
	var counter int64
	var wg sync.WaitGroup

	start := time.Now()
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				mu.Lock()
				counter++
				simulateCriticalSection(workNanos)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return time.Since(start)
}

func runComparison(s ContentionScenario) ComparisonResult {
	return ComparisonResult{
		SpinlockTime: benchmarkSpinlock(s.Goroutines, s.Iterations, s.WorkNanos),
		MutexTime:    benchmarkMutex(s.Goroutines, s.Iterations, s.WorkNanos),
	}
}

func printComparison(s ContentionScenario, r ComparisonResult) {
	fmt.Printf("%s:\n", s.Name)
	fmt.Printf("  Spinlock: %v\n", r.SpinlockTime)
	fmt.Printf("  Mutex:    %v\n", r.MutexTime)
	fmt.Printf("  Winner:   %s (spin/mutex ratio: %.2f)\n\n", r.Winner(), r.Ratio())
}

func main() {
	fmt.Printf("GOMAXPROCS: %d\n\n", runtime.GOMAXPROCS(0))

	scenarios := []ContentionScenario{
		{"Tiny critical section (no work)", 100, 1000, 0},
		{"Short critical section (100ns)", 100, 100, 100},
		{"Medium critical section (1us)", 50, 100, 1000},
	}

	for _, s := range scenarios {
		result := runComparison(s)
		printComparison(s, result)
	}
}
```

### Verification
```bash
go run main.go
```
With tiny critical sections, spinlock may be competitive. As the critical section grows, mutex wins because blocked goroutines sleep instead of burning CPU. Under high contention, the spinlock wastes significant CPU time on failed CAS attempts and Gosched calls.

## Step 3 -- Demonstrate the Deadlock Risk Without Gosched

Show why `runtime.Gosched()` is essential in Go spinlocks. Without it, the spinning goroutine holds its OS thread and can prevent the lock holder from running:

```go
package main

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

const (
	spinUnlocked   int32 = 0
	spinLocked     int32 = 1
	testGoroutines       = 10
	testIterations       = 100
	deadlockTimeout      = 500 * time.Millisecond
)

type NoYieldSpinlock struct {
	state int32
}

func (s *NoYieldSpinlock) Lock() {
	for !atomic.CompareAndSwapInt32(&s.state, spinUnlocked, spinLocked) {
		// NO Gosched! Tight spin holds the OS thread.
	}
}

func (s *NoYieldSpinlock) Unlock() {
	atomic.StoreInt32(&s.state, spinUnlocked)
}

type YieldingSpinlock struct {
	state int32
}

func (s *YieldingSpinlock) Lock() {
	for !atomic.CompareAndSwapInt32(&s.state, spinUnlocked, spinLocked) {
		runtime.Gosched()
	}
}

func (s *YieldingSpinlock) Unlock() {
	atomic.StoreInt32(&s.state, spinUnlocked)
}

func testYieldingSpinlock() {
	fmt.Println("Testing YieldingSpinlock...")
	var lock YieldingSpinlock
	var wg sync.WaitGroup
	var counter int64

	start := time.Now()
	for i := 0; i < testGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < testIterations; j++ {
				lock.Lock()
				counter++
				lock.Unlock()
			}
		}()
	}
	wg.Wait()
	fmt.Printf("YieldingSpinlock: counter=%d, time=%v\n\n", counter, time.Since(start))
}

func testNoYieldDeadlock() {
	fmt.Println("Testing NoYieldSpinlock with timeout protection...")
	var lock NoYieldSpinlock
	done := make(chan bool, 1)

	go func() {
		lock.Lock()
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			lock.Lock() // with GOMAXPROCS=1, this spins forever
			lock.Unlock()
		}()

		time.Sleep(100 * time.Millisecond)
		lock.Unlock()

		ch := make(chan struct{})
		go func() {
			wg.Wait()
			close(ch)
		}()

		select {
		case <-ch:
			done <- true
		case <-time.After(deadlockTimeout):
			done <- false
		}
	}()

	if <-done {
		fmt.Println("NoYieldSpinlock: completed (got lucky with scheduling)")
	} else {
		fmt.Println("NoYieldSpinlock: TIMED OUT - spinning goroutine starved the lock holder")
	}
}

func main() {
	runtime.GOMAXPROCS(1)
	fmt.Println("Running with GOMAXPROCS=1")
	fmt.Println()

	testYieldingSpinlock()
	testNoYieldDeadlock()

	runtime.GOMAXPROCS(runtime.NumCPU())
	fmt.Printf("\nRestored GOMAXPROCS=%d\n", runtime.GOMAXPROCS(0))
}
```

### Verification
```bash
go run main.go
```
GoodSpinLock completes. BadSpinLock either times out or takes much longer, demonstrating that without Gosched, the spinning goroutine starves other goroutines when OS threads are limited.

## Step 4 -- When Spinlocks Actually Make Sense

Show the narrow scenario where spinlocks can outperform mutexes: an extremely short critical section (single memory operation) with low contention. Then show why even in this case, `atomic.AddInt64` is the better answer:

```go
package main

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

const (
	spinUnlocked int32 = 0
	spinLocked   int32 = 1
	goroutines         = 4
	iterations         = 100000
)

type Spinlock struct {
	state int32
}

func (s *Spinlock) Lock() {
	for !atomic.CompareAndSwapInt32(&s.state, spinUnlocked, spinLocked) {
		runtime.Gosched()
	}
}

func (s *Spinlock) Unlock() {
	atomic.StoreInt32(&s.state, spinUnlocked)
}

type ThreeWayResult struct {
	SpinlockTime  time.Duration
	SpinlockCount int64
	MutexTime     time.Duration
	MutexCount    int64
	AtomicTime    time.Duration
	AtomicCount   int64
}

func benchmarkSpinlockCounter() (time.Duration, int64) {
	var lock Spinlock
	var counter int64
	var wg sync.WaitGroup

	start := time.Now()
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				lock.Lock()
				counter++
				lock.Unlock()
			}
		}()
	}
	wg.Wait()
	return time.Since(start), counter
}

func benchmarkMutexCounter() (time.Duration, int64) {
	var mu sync.Mutex
	var counter int64
	var wg sync.WaitGroup

	start := time.Now()
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				mu.Lock()
				counter++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return time.Since(start), counter
}

func benchmarkAtomicCounter() (time.Duration, int64) {
	var counter atomic.Int64
	var wg sync.WaitGroup

	start := time.Now()
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				counter.Add(1)
			}
		}()
	}
	wg.Wait()
	return time.Since(start), counter.Load()
}

func runThreeWayComparison() ThreeWayResult {
	spinT, spinC := benchmarkSpinlockCounter()
	mutexT, mutexC := benchmarkMutexCounter()
	atomicT, atomicC := benchmarkAtomicCounter()
	return ThreeWayResult{
		SpinlockTime: spinT, SpinlockCount: spinC,
		MutexTime: mutexT, MutexCount: mutexC,
		AtomicTime: atomicT, AtomicCount: atomicC,
	}
}

func printResults(r ThreeWayResult) {
	fmt.Printf("=== Single Counter, %d goroutines x %d iterations ===\n\n", goroutines, iterations)
	fmt.Printf("Spinlock: %v (counter=%d)\n", r.SpinlockTime, r.SpinlockCount)
	fmt.Printf("Mutex:    %v (counter=%d)\n", r.MutexTime, r.MutexCount)
	fmt.Printf("Atomic:   %v (counter=%d)\n", r.AtomicTime, r.AtomicCount)
}

func printInsights() {
	fmt.Println()
	fmt.Println("Key insight: if your critical section is just a counter increment,")
	fmt.Println("use atomic.Add -- it is faster than any lock, spinlock or otherwise.")
	fmt.Println()
	fmt.Println("Spinlocks only make sense when:")
	fmt.Println("  1. The critical section is a few nanoseconds (not expressible as atomic)")
	fmt.Println("  2. Contention is very low (few goroutines, short hold times)")
	fmt.Println("  3. Goroutine parking overhead is unacceptable (real-time constraints)")
	fmt.Println("  4. You have measured and proven it is actually faster for YOUR case")
}

func main() {
	result := runThreeWayComparison()
	printResults(result)
	printInsights()
}
```

### Verification
```bash
go run main.go
```
Atomic is fastest. SpinLock and Mutex are close for this trivial critical section. The lesson: if you can express the operation as an atomic, do so. If you need a lock, use `sync.Mutex`.

## Verification

Run the race detector on each step:
```bash
go run -race main.go
```
All steps should pass with zero race warnings.

## Common Mistakes

### Omitting runtime.Gosched in the Spin Loop

**Wrong:**
```go
package main

import "sync/atomic"

type TightSpinlock struct{ state int32 }

func (s *TightSpinlock) Lock() {
	for !atomic.CompareAndSwapInt32(&s.state, 0, 1) {
		// tight spin -- holds OS thread, starves other goroutines
	}
}

func (s *TightSpinlock) Unlock() {
	atomic.StoreInt32(&s.state, 0)
}

func main() {
	var lock TightSpinlock
	lock.Lock()
	lock.Unlock()
}
```

**What happens:** With `GOMAXPROCS=1`, the spinner holds the only OS thread and the lock holder cannot run `Unlock()`. Deadlock.

**Fix:** Always call `runtime.Gosched()` in the spin loop.

### Unlocking Without Atomic Store

**Wrong:**
```go
func (s *SpinLock) Unlock() {
    s.state = 0 // non-atomic write -- data race with CAS in Lock()
}
```

**Fix:** Use `atomic.StoreInt32(&s.state, 0)`.

### Using a Spinlock for Long Critical Sections

**Wrong:**
```go
lock.Lock()
result := callExternalAPI() // holds lock for milliseconds
lock.Unlock()
```

**What happens:** Other goroutines spin for milliseconds, burning CPU. With 100 contending goroutines, that is 100 CPU-milliseconds wasted per call.

**Fix:** Use `sync.Mutex` for anything beyond a few nanoseconds. Mutex parks waiting goroutines instead of spinning.

### Deploying a Custom Spinlock in Production

The Go runtime's `sync.Mutex` is a sophisticated hybrid that spins briefly then parks. It handles edge cases (starvation mode, handoff) that a naive spinlock does not. Unless you have exceptional performance requirements backed by benchmark evidence, use `sync.Mutex`.

## Review

A spinlock is CAS and nothing more: `Lock` compare-and-swaps the state word from
0 to 1 in a loop, `Unlock` atomically stores 0, and `TryLock` is a single CAS
that reports whether it won. The one non-negotiable detail is `runtime.Gosched()`
inside the loop -- without it the spinner never yields its OS thread, and under
`GOMAXPROCS=1` the goroutine holding the lock can never get scheduled to release
it, so the program deadlocks. Everything else is a lesson in when not to use one.
A spinlock burns CPU while it waits, whereas `sync.Mutex` is a hybrid that spins
briefly and then parks the blocked goroutine, so the mutex wins as soon as the
critical section grows or contention rises. And if the critical section is just a
counter bump, no lock belongs there at all -- `atomic.Add` beats every lock.

You should be able to answer four questions from memory. Why does a spinlock
under `GOMAXPROCS=1` risk deadlock without `runtime.Gosched()`? What makes
`sync.Mutex` a hybrid rather than a pure spinlock or a pure parking lock? In what
narrow scenario -- a critical section of a few nanoseconds, very low contention,
real-time constraints -- could a hand-rolled spinlock actually beat it? And why
is `atomic.Add` the right answer for a simple counter increment when any lock
would also be correct? If you can justify each, you understand both how to build
a spinlock and why you almost never should.

## Resources
- [sync.Mutex implementation (Go source)](https://github.com/golang/go/blob/master/src/sync/mutex.go) -- the real spin-then-park hybrid, with starvation mode and handoff.
- [runtime.Gosched](https://pkg.go.dev/runtime#Gosched) -- the yield that keeps the spin loop from starving the lock holder.
- [Spinlock (Wikipedia)](https://en.wikipedia.org/wiki/Spinlock) -- the general concept and its busy-wait trade-off across systems.

---

Back to [Concurrency](../../concurrency.md) | Next: [06-happens-before-guarantees](../06-happens-before-guarantees/06-happens-before-guarantees.md)
