# Exercise 2: Mutex Baseline: Same Contract, Boring Implementation

Every lock-free structure you ship must have a boring mutex twin: the version you
write first, ship first, and benchmark against. This exercise builds the
mutex-based stack with the exact `Push`/`Pop`/`Size` contract of the lock-free
one, and — the senior part — a single behavioral contract suite that runs against
*both* implementations, so a future swap cannot silently change semantics.

## What you'll build

```text
mutexstack/                      independent module: example.com/mutexstack
  go.mod
  stack/
    stack.go                     lock-free Stack[T] (the contract's other party, inline)
    mutex_stack.go               MutexStack[T]: sync.Mutex around head/size
    contract_test.go             one suite, table of constructors, run against both stacks
  cmd/
    demo/
      main.go                    same workload through both implementations, same answers
```

- Files: `stack/stack.go`, `stack/mutex_stack.go`, `stack/contract_test.go`, `cmd/demo/main.go`.
- Implement: `MutexStack[T]` with `Push`, `Pop`, `Size` guarded by one `sync.Mutex`, next to the lock-free `Stack[T]`.
- Test: the preserved `TestMutexStackLIFOOrder` plus `TestContract`, a suite parameterized over a `lifo[T]` interface and a table of constructors covering both implementations.
- Verify: `go test -count=1 -race ./...`

### Why the boring version comes first

The mutex stack is trivially correct: one lock serializes every operation, the
size field is exactly synchronized with the list (no best-effort caveat), and
anyone can review it in a minute. That is the version that ships on day one. The
lock-free version exists only if a profiler later shows the lock is hot — and
when it lands, it must be a drop-in behavioral replacement. The way to enforce
"drop-in" is not code review; it is a contract test suite that both
implementations must pass, written against the narrow interface the callers
actually use.

Note one honest difference the contract deliberately does *not* cover:
`MutexStack.Size` is exact at all times, while the lock-free `Size` is exact
only at quiescence. The contract suite therefore asserts size only when no
operation is in flight — the strongest guarantee *both* implementations make.
Writing contracts at the weaker of the two guarantees is what lets you swap
implementations without lying to callers.

Create `stack/stack.go` — the lock-free implementation this module's contract
tests against (self-contained copy, identical to Exercise 1):

```go
package stack

import "sync/atomic"

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
```

### The mutex twin

The mutex version reuses the same `node[T]` type but guards a plain pointer and a
plain `int` with one lock. `defer s.mu.Unlock()` keeps every early return safe;
at this level of contention the tiny cost of `defer` is irrelevant next to the
lock itself. Like its lock-free twin, a `MutexStack` must not be copied after
first use — copying a `sync.Mutex` desynchronizes the copies, and `go vet`'s
copylocks check flags it.

Create `stack/mutex_stack.go`:

```go
package stack

import "sync"

// MutexStack is a mutex-guarded LIFO stack with the same contract as
// Stack. The zero value is empty and ready to use. MutexStack must
// not be copied after first use.
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

### The contract suite

The suite defines the interface callers rely on — `Push` and `Pop` — and a table
of constructors, one per implementation. Each behavioral case runs as a subtest
under each constructor via `t.Run`, so a failure names the implementation and
the case (`TestContract/mutex/lifo_order`). The concurrency case pushes disjoint
value ranges from 8 goroutines and drains into a set: no lost items, no
duplicates, under `-race`, for both stacks.

Create `stack/contract_test.go`:

```go
package stack

import (
	"fmt"
	"sync"
	"testing"
)

type lifo[T any] interface {
	Push(T)
	Pop() (T, bool)
}

func TestContract(t *testing.T) {
	t.Parallel()

	impls := []struct {
		name string
		make func() lifo[int]
	}{
		{"lockfree", func() lifo[int] { return &Stack[int]{} }},
		{"mutex", func() lifo[int] { return &MutexStack[int]{} }},
	}

	for _, impl := range impls {
		t.Run(impl.name, func(t *testing.T) {
			t.Parallel()

			t.Run("lifo_order", func(t *testing.T) {
				t.Parallel()
				s := impl.make()
				for i := range 10 {
					s.Push(i)
				}
				for i := 9; i >= 0; i-- {
					v, ok := s.Pop()
					if !ok || v != i {
						t.Fatalf("Pop = %d,%v, want %d,true", v, ok, i)
					}
				}
			})

			t.Run("pop_empty", func(t *testing.T) {
				t.Parallel()
				s := impl.make()
				if v, ok := s.Pop(); ok || v != 0 {
					t.Fatalf("Pop on empty = %d,%v, want 0,false", v, ok)
				}
			})

			t.Run("no_lost_no_dup", func(t *testing.T) {
				t.Parallel()
				const goroutines = 8
				const perGoroutine = 250
				s := impl.make()
				var wg sync.WaitGroup
				for g := range goroutines {
					wg.Add(1)
					go func() {
						defer wg.Done()
						for j := range perGoroutine {
							s.Push(g*perGoroutine + j)
						}
					}()
				}
				wg.Wait()

				seen := make(map[int]bool, goroutines*perGoroutine)
				for {
					v, ok := s.Pop()
					if !ok {
						break
					}
					if seen[v] {
						t.Fatalf("value %d popped twice", v)
					}
					seen[v] = true
				}
				if len(seen) != goroutines*perGoroutine {
					t.Fatalf("distinct values = %d, want %d",
						len(seen), goroutines*perGoroutine)
				}
			})
		})
	}
}

func TestMutexStackLIFOOrder(t *testing.T) {
	t.Parallel()

	s := &MutexStack[string]{}
	for _, v := range []string{"a", "b", "c"} {
		s.Push(v)
	}
	want := []string{"c", "b", "a"}
	for i, w := range want {
		got, ok := s.Pop()
		if !ok || got != w {
			t.Fatalf("Pop[%d] = %q ok=%v, want %q true", i, got, ok, w)
		}
	}
	if got := s.Size(); got != 0 {
		t.Fatalf("Size after drain = %d, want 0", got)
	}
}

func ExampleMutexStack() {
	s := &MutexStack[int]{}
	s.Push(1)
	s.Push(2)
	v, _ := s.Pop()
	fmt.Println(v, s.Size())
	// Output: 2 1
}
```

### The demo

The demo pushes the same three values through both implementations and drains
them, printing identical answers — the point being that callers cannot tell the
implementations apart, which is exactly what the contract suite enforces.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"

	"example.com/mutexstack/stack"
)

type lifo interface {
	Push(string)
	Pop() (string, bool)
}

func drain(s lifo) string {
	var out []string
	for {
		v, ok := s.Pop()
		if !ok {
			return strings.Join(out, " ")
		}
		out = append(out, v)
	}
}

func main() {
	impls := []struct {
		name string
		s    lifo
	}{
		{"lockfree", &stack.Stack[string]{}},
		{"mutex", &stack.MutexStack[string]{}},
	}
	for _, impl := range impls {
		for _, v := range []string{"a", "b", "c"} {
			impl.s.Push(v)
		}
		fmt.Printf("%-8s pop order: %s\n", impl.name, drain(impl.s))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
lockfree pop order: c b a
mutex    pop order: c b a
```

## Review

The baseline is right when it is boring: one mutex, `defer` unlocks, exact size,
no cleverness. The contract suite is right when it tests only what both
implementations guarantee — behavior through `Push`/`Pop`, size at quiescence —
and when adding a third implementation is a one-line constructor entry. Two traps
to watch: writing the contract against the *stronger* implementation's guarantees
(asserting mid-flight `Size`, which only the mutex version satisfies) locks you
out of the lock-free swap; and copying either stack by value silently forks its
state — `go vet` catches the mutex copy via copylocks, but the atomic-only stack
copy it may not, so the "must not be copied" doc comment is doing real work. Run
the suite under `-race`: for the mutex version it proves the lock actually covers
every field; for the lock-free version it re-proves Exercise 1 inside this
module.

## Resources

- [sync.Mutex](https://pkg.go.dev/sync#Mutex) — Lock/Unlock semantics and the "must not be copied" rule.
- [testing: subtests with T.Run](https://pkg.go.dev/testing#T.Run) — the mechanism behind the per-implementation contract table.
- [Go Wiki: Go Code Review Comments](https://go.dev/wiki/CodeReviewComments) — the idioms this baseline sticks to (early returns, small interfaces).

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-lock-free-treiber-stack.md](01-lock-free-treiber-stack.md) | Next: [03-stress-and-invariant-suite.md](03-stress-and-invariant-suite.md)
