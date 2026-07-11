# 9. Generic Iterator Patterns

Go's `iter` package gives generic code a standard lazy iteration shape. This lesson builds a small iterator package with map, filter, take, and collect operations that stop as soon as the downstream consumer stops.

## Concepts

### `iter.Seq` Is A Push Iterator

An `iter.Seq[T]` is a function that receives `yield func(T) bool`. The iterator pushes values into `yield`, and it must stop when `yield` returns `false`.

### Combinators Should Preserve Laziness

`Map`, `Filter`, and `Take` should return a new sequence rather than allocate a slice. Values are transformed only when a caller ranges over the sequence.

### Validate The Combinator Boundary

A negative limit is a caller error. The constructor returns a wrapped sentinel error before it creates a misleading iterator.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/iterators/cmd/demo
cd ~/go-exercises/iterators
go mod init example.com/verify
```

### Exercise 1: Build Lazy Combinators

Create `iterators.go`:

```go
package iterators

import (
	"errors"
	"fmt"
	"iter"
)

var ErrNegativeLimit = errors.New("limit must not be negative")

func FromSlice[T any](values []T) iter.Seq[T] {
	return func(yield func(T) bool) {
		for _, value := range values {
			if !yield(value) {
				return
			}
		}
	}
}

func Map[T, U any](seq iter.Seq[T], fn func(T) U) iter.Seq[U] {
	return func(yield func(U) bool) {
		for value := range seq {
			if !yield(fn(value)) {
				return
			}
		}
	}
}

func Filter[T any](seq iter.Seq[T], keep func(T) bool) iter.Seq[T] {
	return func(yield func(T) bool) {
		for value := range seq {
			if keep(value) && !yield(value) {
				return
			}
		}
	}
}

func Take[T any](seq iter.Seq[T], limit int) (iter.Seq[T], error) {
	if limit < 0 {
		return nil, fmt.Errorf("take: %w", ErrNegativeLimit)
	}
	return func(yield func(T) bool) {
		count := 0
		for value := range seq {
			if count >= limit || !yield(value) {
				return
			}
			count++
		}
	}, nil
}

func Collect[T any](seq iter.Seq[T]) []T {
	var out []T
	for value := range seq {
		out = append(out, value)
	}
	return out
}
```

### Exercise 2: Add Tests And An Example

Create `iterators_test.go`:

```go
package iterators

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestPipeline(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   []int
		want []int
	}{
		{name: "empty", in: nil, want: []int{}},
		{name: "evens doubled", in: []int{1, 2, 3, 4, 5}, want: []int{4, 8}},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			seq := Filter(FromSlice(tt.in), func(n int) bool { return n%2 == 0 })
			mapped := Map(seq, func(n int) int { return n * 2 })
			got := Collect(mapped)
			if got == nil {
				got = []int{}
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Collect() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestTakeRejectsNegativeLimit(t *testing.T) {
	t.Parallel()

	_, err := Take(FromSlice([]int{1}), -1)
	if !errors.Is(err, ErrNegativeLimit) {
		t.Fatalf("err = %v, want ErrNegativeLimit", err)
	}
}

func TestTakeStopsEarly(t *testing.T) {
	t.Parallel()

	seq, err := Take(FromSlice([]int{1, 2, 3}), 2)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := Collect(seq), []int{1, 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Collect() = %#v, want %#v", got, want)
	}
}

func ExampleCollect() {
	seq, _ := Take(Map(FromSlice([]int{1, 2, 3}), func(n int) int { return n * n }), 2)
	fmt.Println(Collect(seq))
	// Output: [1 4]
}
```

### Exercise 3: Add A Runnable Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	iterators "example.com/verify"
)

func main() {
	seq, err := iterators.Take(iterators.Filter(iterators.FromSlice([]int{1, 2, 3, 4}), func(n int) bool { return n > 2 }), 2)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(iterators.Collect(seq))
}
```

## Common Mistakes

### Ignoring `yield` Returning False

Wrong: continuing to loop after `yield(value)` returns `false`.

Fix: return immediately so downstream consumers can stop early.

### Allocating In Every Combinator

Wrong: making `Map` build a full slice before returning.

Fix: return an `iter.Seq` and do work only when ranged over.

### Treating Negative Limits As Zero Silently

Wrong: `Take(seq, -1)` behaves like an empty sequence.

Fix: reject the invalid limit with `ErrNegativeLimit`.

## Verification

Run this from `~/go-exercises/iterators`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Add one more test that ranges over `Take(seq, 1)` manually and proves the second value is never requested.

## Summary

- `iter.Seq[T]` is the standard push-iterator form.
- Combinators should return lazy sequences.
- Every iterator must stop when `yield` returns `false`.
- Constructor-style combinators can validate arguments before returning a sequence.

## What's Next

Next: [Generic Repository Pattern](../10-generic-repository-pattern/10-generic-repository-pattern.md).

## Resources

- [iter package](https://pkg.go.dev/iter)
- [Go Blog: Range Over Function Types](https://go.dev/blog/range-functions)
- [Go Specification: For statements with range clause](https://go.dev/ref/spec#For_statements)
