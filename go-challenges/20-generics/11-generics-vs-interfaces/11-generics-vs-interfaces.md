# 11. Generics vs Interfaces

Generics and interfaces both express polymorphism, but they are not substitutes. This lesson compares a behavior-based interface cache with a type-safe generic cache and uses tests to pin the trade-off.

## Concepts

### Interfaces Abstract Behavior

An interface is the right tool when the caller needs behavior, such as `Read`, `Write`, or `ServeHTTP`, and the concrete type is intentionally hidden.

### Generics Preserve A Concrete Type Relationship

A generic cache can state that every value in the cache is a `User`. A non-generic cache can store mixed values, but retrieval requires a type assertion and can fail at runtime.

### The Boundary Is A Design Decision

Use interfaces at package boundaries for behavior and dependency injection. Use generics for containers and algorithms where the same type flows through inputs and outputs.

## Exercises

### Exercise 1: Implement Both Cache Styles

Create `cache.go`:

```go
package choice

import (
	"errors"
	"fmt"
)

var (
	ErrMissing   = errors.New("key is missing")
	ErrWrongType = errors.New("value has wrong type")
)

type AnyCache struct {
	items map[string]any
}

func NewAnyCache() *AnyCache {
	return &AnyCache{items: make(map[string]any)}
}

func (c *AnyCache) Set(key string, value any) {
	c.items[key] = value
}

func GetAs[T any](c *AnyCache, key string) (T, error) {
	var zero T
	value, ok := c.items[key]
	if !ok {
		return zero, fmt.Errorf("get %q: %w", key, ErrMissing)
	}
	typed, ok := value.(T)
	if !ok {
		return zero, fmt.Errorf("get %q: %w", key, ErrWrongType)
	}
	return typed, nil
}

type Cache[T any] struct {
	items map[string]T
}

func NewCache[T any]() *Cache[T] {
	return &Cache[T]{items: make(map[string]T)}
}

func (c *Cache[T]) Set(key string, value T) {
	c.items[key] = value
}

func (c *Cache[T]) Get(key string) (T, error) {
	value, ok := c.items[key]
	if !ok {
		var zero T
		return zero, fmt.Errorf("get %q: %w", key, ErrMissing)
	}
	return value, nil
}
```

### Exercise 2: Add Tests And An Example

Create `cache_test.go`:

```go
package choice

import (
	"errors"
	"fmt"
	"testing"
)

type User struct {
	Name string
}

func TestGenericCache(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  string
		want error
	}{
		{name: "hit", key: "u1"},
		{name: "miss", key: "u2", want: ErrMissing},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cache := NewCache[User]()
			cache.Set("u1", User{Name: "Alice"})
			_, err := cache.Get(tt.key)
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestAnyCacheCanFailAtRuntime(t *testing.T) {
	t.Parallel()

	cache := NewAnyCache()
	cache.Set("answer", 42)
	if _, err := GetAs[string](cache, "answer"); !errors.Is(err, ErrWrongType) {
		t.Fatalf("err = %v, want ErrWrongType", err)
	}
}

func TestAnyCacheMissing(t *testing.T) {
	t.Parallel()

	cache := NewAnyCache()
	if _, err := GetAs[int](cache, "missing"); !errors.Is(err, ErrMissing) {
		t.Fatalf("err = %v, want ErrMissing", err)
	}
}

func ExampleCache_Get() {
	cache := NewCache[User]()
	cache.Set("u1", User{Name: "Alice"})
	user, _ := cache.Get("u1")
	fmt.Println(user.Name)
	// Output: Alice
}
```

### Exercise 3: Add A Runnable Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	choice "example.com/verify"
)

func main() {
	cache := choice.NewCache[string]()
	cache.Set("language", "go")
	value, err := cache.Get("language")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(value)
}
```

## Common Mistakes

### Using Generics For Behavior-Only APIs

Wrong: replacing a simple `io.Reader` parameter with a type parameter that adds no relationship between inputs and outputs.

Fix: keep interfaces for behavior abstraction.

### Using `any` For Homogeneous Containers

Wrong: storing all cache values as `any` when a cache instance should hold one type.

Fix: use `Cache[T]` so wrong value types fail at compile time.

### Writing Generic Methods

Wrong: `func (c *AnyCache) GetAs[T any](key string) (T, error)`.

Fix: Go methods cannot declare their own type parameters; use a generic function such as `GetAs[T](c, key)`.

## Verification

Run this from `~/go-exercises/choice`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Add one more test showing that `Cache[int]` returns `ErrMissing` for an absent key.

## Summary

- Interfaces abstract behavior; generics preserve type relationships.
- Generic containers avoid retrieval assertions.
- Interface-based containers can intentionally store mixed values but need runtime checks.
- Go supports generic functions and generic types, not methods with their own type parameters.

## What's Next

Next: [Type Constraint Composition](../12-type-constraint-composition/12-type-constraint-composition.md).

## Resources

- [Go Blog: When To Use Generics](https://go.dev/blog/when-generics)
- [Go Specification: Method declarations](https://go.dev/ref/spec#Method_declarations)
- [Go Blog: An Introduction To Generics](https://go.dev/blog/intro-generics)
