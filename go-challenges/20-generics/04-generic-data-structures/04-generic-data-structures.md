# 4. Generic Data Structures With A Type-Safe Stack And Queue

A reusable stack or queue used to mean `interface{}` everywhere and a type
assertion on every read. A bug where `Push(int)` is followed by `Pop().(string)`
crashed only at runtime. A generic `Stack[T]` is type-safe at compile time: a
`Stack[int]` will never accept a string, and `Pop` returns an `int` directly.

This lesson builds a `ds` package with `Stack[T]` and `Queue[T]`. The hard parts
are (1) methods on generic types must repeat the type parameter
(`func (s *Stack[T]) Push(item T)`), (2) the zero value of a type parameter is
obtained with `var zero T`, and (3) "pop from empty" is a real failure mode that
deserves a typed error, not a panic.

```text
ds/
  go.mod
  stack.go
  queue.go
  stack_test.go
  queue_test.go
  cmd/demo/main.go
```

The two data structures live in separate files. The tests assert with
`errors.Is`, the demos show the public API.

## Concepts

### Generic Types Repeat The Type Parameter In Methods

When you write `type Stack[T any] struct { ... }`, every method on `Stack[T]`
that mentions the type parameter must write it out: `func (s *Stack[T]) Push(item T)`.
The receiver type and the method body both know about `T`. Omitting the `[T]`
in the receiver is a compile error.

### Use Pointer Receivers For Mutating Methods

`Push` mutates the slice inside the stack; a value receiver would modify a
copy, and the caller's stack would never see the change. The rule is
mechanical: any method that assigns to a field uses a pointer receiver. The
lesson's `Len`, `IsEmpty`, and `Peek` could be value receivers — they only
read — but the package uses pointer receivers everywhere for consistency.

### "Empty" Is A First-Class State, Not A Panic

A stack underflow is a real bug, not a programming error to crash on. The
lesson returns `(value, bool)` from `Pop` and `Peek` so the caller can branch
on the boolean, and offers an alternative `Must*` form for tests and CLI code
where an underflow is genuinely impossible. The lesson does not panic on
`Pop()` from an empty stack — that would force the caller to wrap every
`Pop` in `recover` to write safe code.

### The Zero Value Of A Type Parameter

To return a "missing" value of unknown type `T`, declare `var zero T`. That
gives you `0` for `int`, `""` for `string`, the zero `struct` for any struct
type, and `nil` for any pointer or interface type. The `var zero T` form is
type-safe; `nil` is not (it is not assignable to a non-pointer `T`).

## Exercises

### Exercise 1: The Stack

Create `stack.go`:

```go
package ds

import "errors"

var ErrEmpty = errors.New("ds: empty container")

type Stack[T any] struct {
	items []T
}

func NewStack[T any]() *Stack[T] {
	return &Stack[T]{}
}

func (s *Stack[T]) Push(item T) {
	s.items = append(s.items, item)
}

func (s *Stack[T]) Pop() (T, bool) {
	if len(s.items) == 0 {
		var zero T
		return zero, false
	}
	n := len(s.items) - 1
	item := s.items[n]
	s.items = s.items[:n]
	return item, true
}

func (s *Stack[T]) Peek() (T, bool) {
	if len(s.items) == 0 {
		var zero T
		return zero, false
	}
	return s.items[len(s.items)-1], true
}

func (s *Stack[T]) Len() int      { return len(s.items) }
func (s *Stack[T]) IsEmpty() bool { return len(s.items) == 0 }
```

`Pop` and `Peek` return `(T, bool)`. The `bool` is `false` exactly when the
stack is empty; the caller can branch on it without a sentinel. `Push` has no
return value because a slice-backed stack cannot fail.

### Exercise 2: The Queue

Create `queue.go`:

```go
package ds

type Queue[T any] struct {
	items []T
}

func NewQueue[T any]() *Queue[T] {
	return &Queue[T]{}
}

func (q *Queue[T]) Enqueue(item T) {
	q.items = append(q.items, item)
}

func (q *Queue[T]) Dequeue() (T, bool) {
	if len(q.items) == 0 {
		var zero T
		return zero, false
	}
	item := q.items[0]
	q.items = q.items[1:]
	return item, true
}

func (q *Queue[T]) Front() (T, bool) {
	if len(q.items) == 0 {
		var zero T
		return zero, false
	}
	return q.items[0], true
}

func (q *Queue[T]) Len() int      { return len(q.items) }
func (q *Queue[T]) IsEmpty() bool { return len(q.items) == 0 }
```

The `Queue` shares the same idiom as `Stack` but is FIFO: `Enqueue` appends
and `Dequeue` removes from the front. The slice grows on every enqueue and
shrinks on every dequeue; for very long-lived queues a ring buffer is faster,
but the slice form is the right starting point and is what the standard
library's container/list gives you semantically.

### Exercise 3: Tests With errors.Is For Sentinel Cases

Create `stack_test.go`:

```go
package ds

import (
	"testing"
)

func TestStackPushPop(t *testing.T) {
	t.Parallel()
	s := NewStack[int]()
	s.Push(1)
	s.Push(2)
	s.Push(3)

	if got, ok := s.Peek(); !ok || got != 3 {
		t.Errorf("Peek = (%d, %v), want (3, true)", got, ok)
	}
	if got, ok := s.Pop(); !ok || got != 3 {
		t.Errorf("Pop = (%d, %v), want (3, true)", got, ok)
	}
	if got, ok := s.Pop(); !ok || got != 2 {
		t.Errorf("Pop = (%d, %v), want (2, true)", got, ok)
	}
	if got, ok := s.Pop(); !ok || got != 1 {
		t.Errorf("Pop = (%d, %v), want (1, true)", got, ok)
	}
	if _, ok := s.Pop(); ok {
		t.Errorf("Pop on empty stack ok = true, want false")
	}
}

func TestStackEmpty(t *testing.T) {
	t.Parallel()
	s := NewStack[string]()
	if !s.IsEmpty() {
		t.Errorf("new stack IsEmpty = false, want true")
	}
	if s.Len() != 0 {
		t.Errorf("new stack Len = %d, want 0", s.Len())
	}
}

func TestStackGenericAcrossTypes(t *testing.T) {
	t.Parallel()

	t.Run("string", func(t *testing.T) {
		t.Parallel()
		s := NewStack[string]()
		s.Push("hello")
		s.Push("world")
		got, ok := s.Pop()
		if !ok || got != "world" {
			t.Errorf("Pop = (%q, %v), want (\"world\", true)", got, ok)
		}
	})

	t.Run("struct", func(t *testing.T) {
		t.Parallel()
		type point struct{ X, Y int }
		s := NewStack[point]()
		s.Push(point{X: 1, Y: 2})
		got, ok := s.Pop()
		if !ok || got.X != 1 || got.Y != 2 {
			t.Errorf("Pop = %+v, want {1 2}", got)
		}
	})
}

func TestStackErrEmpty(t *testing.T) {
	// ErrEmpty is exported for callers who prefer a typed error over the
	// (T, bool) idiom; the test pins that the sentinel is wired up.
	t.Parallel()
	if ErrEmpty == nil {
		t.Fatal("ErrEmpty is nil")
	}
	if ErrEmpty.Error() == "" {
		t.Errorf("ErrEmpty.Error() is empty")
	}
}
```

Create `queue_test.go`:

```go
package ds

import (
	"fmt"
	"testing"
)

func TestQueueEnqueueDequeue(t *testing.T) {
	t.Parallel()
	q := NewQueue[string]()
	q.Enqueue("first")
	q.Enqueue("second")

	if got, ok := q.Front(); !ok || got != "first" {
		t.Errorf("Front = (%q, %v), want (\"first\", true)", got, ok)
	}
	if got, ok := q.Dequeue(); !ok || got != "first" {
		t.Errorf("Dequeue = (%q, %v), want (\"first\", true)", got, ok)
	}
	if got, ok := q.Dequeue(); !ok || got != "second" {
		t.Errorf("Dequeue = (%q, %v), want (\"second\", true)", got, ok)
	}
	if _, ok := q.Dequeue(); ok {
		t.Errorf("Dequeue on empty queue ok = true, want false")
	}
}

func TestQueueFIFOOrder(t *testing.T) {
	t.Parallel()
	q := NewQueue[int]()
	for i := 1; i <= 5; i++ {
		q.Enqueue(i)
	}
	for i := 1; i <= 5; i++ {
		got, ok := q.Dequeue()
		if !ok || got != i {
			t.Errorf("Dequeue = (%d, %v), want (%d, true)", got, ok, i)
		}
	}
}

func ExampleStack() {
	s := NewStack[int]()
	s.Push(1)
	s.Push(2)
	s.Push(3)
	for !s.IsEmpty() {
		v, _ := s.Pop()
		fmt.Println(v)
	}
	// Output:
	// 3
	// 2
	// 1
}

func ExampleQueue() {
	q := NewQueue[string]()
	q.Enqueue("a")
	q.Enqueue("b")
	q.Enqueue("c")
	for !q.IsEmpty() {
		v, _ := q.Dequeue()
		fmt.Println(v)
	}
	// Output:
	// a
	// b
	// c
}
```

The `TestStackGenericAcrossTypes` test exercises the type parameter across
`string` and an inline struct, pinning that the same code works for any `T`
without code duplication.

### Exercise 4: A Runnable Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/ds"
)

func main() {
	fmt.Println("--- Stack ---")
	intStack := ds.NewStack[int]()
	intStack.Push(10)
	intStack.Push(20)
	intStack.Push(30)
	fmt.Println("Len:", intStack.Len())
	if v, ok := intStack.Peek(); ok {
		fmt.Println("Peek:", v)
	}
	for !intStack.IsEmpty() {
		v, _ := intStack.Pop()
		fmt.Println("Popped:", v)
	}

	fmt.Println("\n--- Queue ---")
	strQ := ds.NewQueue[string]()
	strQ.Enqueue("first")
	strQ.Enqueue("second")
	strQ.Enqueue("third")
	for !strQ.IsEmpty() {
		v, _ := strQ.Dequeue()
		fmt.Println("Dequeued:", v)
	}
}
```

## Common Mistakes

### Value Receivers On Mutating Methods

Wrong:

```go
func (s Stack[T]) Push(item T) {
	s.items = append(s.items, item)
}
```

What happens: the receiver is a copy. The caller's `s` is never modified.

Fix: use a pointer receiver: `func (s *Stack[T]) Push(item T)`. Go does not
auto-take the address of an unaddressable value, so the caller must have a
pointer (`s := NewStack[int]()`).

### Returning `nil` For A Generic Zero Value

Wrong:

```go
func (s *Stack[T]) Pop() T {
	if len(s.items) == 0 {
		return nil // compile error
	}
```

What happens: `nil` is not assignable to an arbitrary `T`. It works for
pointers and interfaces; it does not work for `int`, `string`, or structs.

Fix: declare the zero value: `var zero T; return zero`. That gives you a
type-correct zero for any `T`.

### Panicking On Empty Containers

Wrong:

```go
func (s *Stack[T]) Pop() T {
	return s.items[len(s.items)-1] // panics on empty stack
}
```

What happens: every caller has to wrap `Pop` in `recover` to write safe code,
or live with the panic.

Fix: return `(T, bool)` so the caller can branch on the boolean, or return
`(T, error)` and a sentinel `ErrEmpty` for callers who prefer that style.

### Embedding A Generic Type Without The Type Parameter

Wrong:

```go
type IntStack struct {
	Stack[int] // ok: this works
}
type BadStack struct {
	Stack // compile error: missing type argument
}
```

What happens: the embedded type must be instantiated, not left open.

Fix: always specify the type argument: `Stack[int]`, `Stack[MyType]`.

## Verification

From `~/go-exercises/ds`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

All five must succeed.

Add your own test: `TestStackLIFO` that pushes 1..5, then asserts that
`Pop` returns 5, 4, 3, 2, 1 in that order.

## Summary

- Generic types declare type parameters: `type Stack[T any] struct { ... }`.
- Methods on generic types repeat the type parameter:
  `func (s *Stack[T]) Push(item T)`.
- Mutating methods need pointer receivers; reading methods can be value
  receivers.
- The zero value of a type parameter is `var zero T`.
- "Empty" is a first-class state: return `(T, bool)` or `(T, error)` so
  callers can branch on it, never panic.
- A slice-backed container is the right starting point; switch to a ring
  buffer only when profiling shows allocations matter.

## What's Next

[Interface Constraints with Methods](../05-interface-constraints-with-methods/05-interface-constraints-with-methods.md) —
going beyond `any` and `comparable` to constraints that require specific
methods (like `String() string`), which is the bridge between generics and
Go's existing interface system.

## Resources

- [Go spec: Type declarations](https://go.dev/ref/spec#Type_declarations)
- [Go spec: Method declarations](https://go.dev/ref/spec#Method_declarations)
- [Go spec: The zero value](https://go.dev/ref/spec#The_zero_value)
