# Exercise 3: Concurrent Stress Harness: No Lost, No Duplicated Items

A lock-free structure is not done when its unit tests pass; it is done when a
stress harness has hammered it with real parallelism under `-race` and the
invariants held, and when benchmarks against the mutex twin justify its
existence. This exercise builds that harness: the no-lost-items and mixed-churn
invariant tests, plus comparative benchmarks at three parallelism levels that
show where CAS wins and where cache-line ping-pong hands the win back to the
mutex.

## What you'll build

```text
stackstress/                     independent module: example.com/stackstress
  go.mod
  stack/
    stack.go                     lock-free Stack[T] + MutexStack[T] (both inline)
    stress_test.go               TestStackNoLostItems, TestStackMixedPushPop,
                                 TestPushDrainScenario, ExampleStack_drain
    bench_test.go                BenchmarkLockFreeStack / BenchmarkMutexStack
                                 under b.RunParallel at SetParallelism 1, 4, 16
  cmd/
    demo/
      main.go                    100 concurrent pushers, then a drain loop
```

- Files: `stack/stack.go`, `stack/stress_test.go`, `stack/bench_test.go`, `cmd/demo/main.go`.
- Implement: both stack implementations inline, a deterministic drain helper, and the benchmark pair.
- Test: `TestStackNoLostItems` (50 goroutines x 200 pushes, concurrent drain counts exactly 10000), `TestStackMixedPushPop`, and the old demo scenario as a deterministic test.
- Verify: `go test -count=1 -race ./...` then `go test -bench=. -benchmem ./stack`

### What a stress test must prove

Unit tests prove the sequential contract. A stress test proves the *linearized*
contract under real interleavings: every pushed item is popped exactly once —
none lost (a CAS that overwrote a concurrent push), none duplicated (a CAS that
resurrected a popped node). `TestStackNoLostItems` pushes 10000 items from 50
goroutines, asserts the quiesced `Size` is exact, then drains with 50 concurrent
poppers counting into an `atomic.Int64`; any discrepancy between pushed and
popped is an algorithm bug, not a flake. `TestStackMixedPushPop` interleaves
pushes and pops in the same goroutines — the churn pattern that maximizes CAS
failures on both ends of the structure at once.

Run these under `-race` always. The race detector does not merely catch data
races here; it validates the happens-before edges your correctness argument
claims exist. A pass without `-race` is not a pass.

Create `stack/stack.go` — both implementations, self-contained:

```go
package stack

import (
	"sync"
	"sync/atomic"
)

type node[T any] struct {
	value T
	next  *node[T]
}

// Stack is a lock-free LIFO stack. The zero value is empty and ready
// to use. Stack must not be copied after first use.
type Stack[T any] struct {
	head atomic.Pointer[node[T]]
	size atomic.Int64
}

// Push adds value on top of the stack.
func (s *Stack[T]) Push(value T) {
	n := &node[T]{value: value}
	for {
		old := s.head.Load()
		n.next = old
		if s.head.CompareAndSwap(old, n) {
			s.size.Add(1)
			return
		}
	}
}

// Pop removes and returns the top value. The second result is false
// when the stack is empty.
func (s *Stack[T]) Pop() (T, bool) {
	for {
		old := s.head.Load()
		if old == nil {
			var zero T
			return zero, false
		}
		if s.head.CompareAndSwap(old, old.next) {
			s.size.Add(-1)
			return old.value, true
		}
	}
}

// Size returns the current approximate size; exact only when no
// operation is in flight.
func (s *Stack[T]) Size() int64 {
	return s.size.Load()
}

// MutexStack is the mutex-guarded twin with the same contract.
// MutexStack must not be copied after first use.
type MutexStack[T any] struct {
	mu   sync.Mutex
	head *node[T]
	size int
}

// Push adds value on top of the stack.
func (s *MutexStack[T]) Push(value T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.head = &node[T]{value: value, next: s.head}
	s.size++
}

// Pop removes and returns the top value. The second result is false
// when the stack is empty.
func (s *MutexStack[T]) Pop() (T, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.head == nil {
		var zero T
		return zero, false
	}
	v := s.head.value
	s.head = s.head.next
	s.size--
	return v, true
}

// Size returns the exact current size.
func (s *MutexStack[T]) Size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.size
}
```

### The invariant tests

`TestPushDrainScenario` is the original runnable demo turned into what it always
wanted to be: a deterministic test. 100 goroutines push, one goroutine drains,
and the assertions replace eyeballing printed numbers.

Create `stack/stress_test.go`:

```go
package stack

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func TestStackNoLostItems(t *testing.T) {
	t.Parallel()

	const goroutines = 50
	const perGoroutine = 200

	s := &Stack[int]{}
	var wg sync.WaitGroup
	for i := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range perGoroutine {
				s.Push(i*perGoroutine + j)
			}
		}()
	}
	wg.Wait()

	if got, want := s.Size(), int64(goroutines*perGoroutine); got != want {
		t.Fatalf("Size = %d, want %d", got, want)
	}

	var popped atomic.Int64
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if _, ok := s.Pop(); !ok {
					return
				}
				popped.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := popped.Load(); got != int64(goroutines*perGoroutine) {
		t.Fatalf("popped = %d, want %d", got, goroutines*perGoroutine)
	}
	if got := s.Size(); got != 0 {
		t.Fatalf("Size after drain = %d, want 0", got)
	}
}

func TestStackMixedPushPop(t *testing.T) {
	t.Parallel()

	s := &Stack[int]{}
	var wg sync.WaitGroup
	var popped atomic.Int64
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 1000 {
				s.Push(j)
				if j%5 == 0 {
					if _, ok := s.Pop(); ok {
						popped.Add(1)
					}
				}
			}
		}()
	}
	wg.Wait()

	// Conservation at quiescence: what was pushed and not successfully
	// popped is still on the stack. (A pop can miss if the stack is
	// transiently empty, so successes are counted, not assumed.)
	if got, want := s.Size(), int64(20*1000)-popped.Load(); got != want {
		t.Fatalf("Size after mixed churn = %d, want %d", got, want)
	}
}

func TestPushDrainScenario(t *testing.T) {
	t.Parallel()

	s := &Stack[int]{}
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Push(i * 10)
		}()
	}
	wg.Wait()

	if got := s.Size(); got != 100 {
		t.Fatalf("Size after pushes = %d, want 100", got)
	}

	popped := 0
	for {
		if _, ok := s.Pop(); !ok {
			break
		}
		popped++
	}
	if popped != 100 {
		t.Fatalf("popped = %d, want 100", popped)
	}
	if got := s.Size(); got != 0 {
		t.Fatalf("Size after drain = %d, want 0", got)
	}
}

func ExampleStack_drain() {
	s := &Stack[string]{}
	s.Push("first")
	s.Push("second")
	for {
		v, ok := s.Pop()
		if !ok {
			break
		}
		fmt.Println(v)
	}
	// Output:
	// second
	// first
}
```

### The benchmarks: where CAS wins and where it loses

`b.RunParallel` splits `b.N` iterations across `GOMAXPROCS` goroutines;
`b.SetParallelism(p)` multiplies that goroutine count by `p`, oversubscribing the
CPUs to raise contention. Each iteration does a push and a pop — the worst case
for both implementations, since every operation touches the single hot head (or
lock). Expect the shape, not exact numbers: at low parallelism the two are close
(both fast paths are a handful of atomic operations); as parallelism grows, CAS
failure rates and cache-line ping-pong climb for the lock-free stack while the
mutex parks excess waiters. On many machines the mutex *wins* the extreme case —
which is the honest lesson: this decision is empirical.

Create `stack/bench_test.go`:

```go
package stack

import (
	"fmt"
	"testing"
)

type lifo[T any] interface {
	Push(T)
	Pop() (T, bool)
}

func benchStack(b *testing.B, make func() lifo[int]) {
	for _, p := range []int{1, 4, 16} {
		b.Run(fmt.Sprintf("parallelism=%d", p), func(b *testing.B) {
			s := make()
			b.SetParallelism(p)
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					s.Push(1)
					s.Pop()
				}
			})
		})
	}
}

func BenchmarkLockFreeStack(b *testing.B) {
	benchStack(b, func() lifo[int] { return &Stack[int]{} })
}

func BenchmarkMutexStack(b *testing.B) {
	benchStack(b, func() lifo[int] { return &MutexStack[int]{} })
}
```

Run and record the numbers at all three levels:

```bash
go test -count=1 -race ./...
go test -bench=. -benchmem ./stack
```

On an 8-core machine the output looks like this (your absolute numbers will
differ; the crossover shape is what to look for):

```
BenchmarkLockFreeStack/parallelism=1-8    ...  ns/op  24 B/op  1 allocs/op
BenchmarkLockFreeStack/parallelism=4-8    ...  ns/op  24 B/op  1 allocs/op
BenchmarkLockFreeStack/parallelism=16-8   ...  ns/op  24 B/op  1 allocs/op
BenchmarkMutexStack/parallelism=1-8       ...  ns/op  24 B/op  1 allocs/op
BenchmarkMutexStack/parallelism=4-8       ...  ns/op  24 B/op  1 allocs/op
BenchmarkMutexStack/parallelism=16-8      ...  ns/op  24 B/op  1 allocs/op
```

Write down the ns/op at each level for both stacks and explain the trend you see
in one paragraph: where the CAS loop's optimism pays, and where its failed
retries plus MESI ownership transfers cost more than the mutex's parking. That
paragraph — not the code — is the deliverable a production decision would hang
on.

### The demo

The preserved concurrent demo: 100 goroutines push, then a single drain loop
empties the stack. Both printed lines are deterministic because every assertion
point is after a `wg.Wait()` quiescence.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/stackstress/stack"
)

func main() {
	s := &stack.Stack[int]{}
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Push(i * 10)
		}()
	}
	wg.Wait()
	fmt.Printf("pushed; size=%d\n", s.Size())

	var popped int64
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			if _, ok := s.Pop(); !ok {
				return
			}
			popped++
		}
	}()
	wg.Wait()
	fmt.Printf("popped=%d size=%d\n", popped, s.Size())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
pushed; size=100
popped=100 size=0
```

## Review

The harness is trustworthy when its counts are exact, not probabilistic: pushed
and popped totals must match to the item under `-race`, and the mixed-churn size
is computable because every pop is paired with a same-goroutine push. The
benchmark's most common mistake is constructing the stack *inside*
`b.RunParallel`'s body function (each goroutine gets its own stack and the
contention disappears); the stack must be created once per `b.Run` and shared.
Second mistake: comparing implementations at a single parallelism level —
the entire point is the crossover, and one data point cannot show it. Finally,
resist promoting `Size` assertions into concurrent phases; both stress tests
only read it at quiescence, which is the only place it is exact.

## Resources

- [testing: B.RunParallel](https://pkg.go.dev/testing#B.RunParallel) — how iterations are distributed and how `SetParallelism` multiplies goroutines.
- [Go blog: Introducing the Go Race Detector](https://go.dev/blog/race-detector) — what `-race` actually validates.
- [Non-blocking algorithm](https://en.wikipedia.org/wiki/Non-blocking_algorithm) — the progress-guarantee vocabulary the benchmark results illustrate.
- [MESI protocol](https://en.wikipedia.org/wiki/MESI_protocol) — why a contended cache line makes failed CAS expensive.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-mutex-stack-baseline.md](02-mutex-stack-baseline.md) | Next: [04-sharded-request-counters.md](04-sharded-request-counters.md)
