# 12. Type Constraint Composition

Constraint composition lets a generic function ask for several capabilities at once. This lesson builds a keyed registry whose keys must be comparable for map lookup and stringable for display.

## Concepts

### Embedded Interfaces Compose Requirements

A constraint can embed `comparable` and also require `String() string`. A type argument must satisfy both requirements.

### Compose Small Constraints Into Domain Constraints

Small constraints are easier to reuse and test. A domain-specific name such as `Key` communicates why the combined requirements exist.

### Do Not Over-Constrain Values

Only keys need comparability in this registry. Values are `any` because the registry stores and returns them without comparing or ordering them.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/20-generics/12-type-constraint-composition/12-type-constraint-composition/cmd/demo
cd go-solutions/20-generics/12-type-constraint-composition/12-type-constraint-composition
```

### Exercise 1: Build The Composed Constraint

Create `registry.go`:

```go
package constraintcomposition

import (
	"errors"
	"fmt"
)

var ErrEmptyKey = errors.New("key string must not be empty")

type Stringer interface {
	String() string
}

type Key interface {
	comparable
	Stringer
}

type Registry[K Key, V any] struct {
	items map[K]V
}

func NewRegistry[K Key, V any]() *Registry[K, V] {
	return &Registry[K, V]{items: make(map[K]V)}
}

func (r *Registry[K, V]) Put(key K, value V) error {
	if key.String() == "" {
		return fmt.Errorf("put: %w", ErrEmptyKey)
	}
	r.items[key] = value
	return nil
}

func (r *Registry[K, V]) Get(key K) (V, bool) {
	value, ok := r.items[key]
	return value, ok
}

func (r *Registry[K, V]) Labels() []string {
	out := make([]string, 0, len(r.items))
	for key := range r.items {
		out = append(out, key.String())
	}
	return out
}
```

### Exercise 2: Add Tests And An Example

Create `registry_test.go`:

```go
package constraintcomposition

import (
	"errors"
	"fmt"
	"testing"
)

type UserID string

func (id UserID) String() string { return string(id) }

func TestRegistryPutGet(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  UserID
		err  error
	}{
		{name: "valid", key: "u1"},
		{name: "empty", key: "", err: ErrEmptyKey},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			registry := NewRegistry[UserID, string]()
			err := registry.Put(tt.key, "Alice")
			if !errors.Is(err, tt.err) {
				t.Fatalf("err = %v, want %v", err, tt.err)
			}
			if tt.err == nil {
				got, ok := registry.Get(tt.key)
				if !ok || got != "Alice" {
					t.Fatalf("Get() = %q, %v", got, ok)
				}
			}
		})
	}
}

func TestLabels(t *testing.T) {
	t.Parallel()

	registry := NewRegistry[UserID, int]()
	if err := registry.Put("u1", 1); err != nil {
		t.Fatal(err)
	}
	if got := len(registry.Labels()); got != 1 {
		t.Fatalf("Labels length = %d, want 1", got)
	}
}

func ExampleRegistry_Get() {
	registry := NewRegistry[UserID, string]()
	_ = registry.Put("u1", "Alice")
	name, _ := registry.Get("u1")
	fmt.Println(name)
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

	constraintcomposition "example.com/verify"
)

type Code string

func (c Code) String() string { return string(c) }

func main() {
	registry := constraintcomposition.NewRegistry[Code, int]()
	if err := registry.Put("A", 10); err != nil {
		log.Fatal(err)
	}
	value, _ := registry.Get("A")
	fmt.Println(value)
}
```

## Common Mistakes

### Forgetting `comparable`

Wrong: a key constraint with only `String() string`, then using `map[K]V`.

Fix: embed `comparable` because map keys must be comparable.

### Over-Constraining Values

Wrong: requiring values to implement `String()` even though the registry never calls it.

Fix: keep `V any` until the code needs more.

### Using A Runtime Check For A Compile-Time Property

Wrong: using reflection to see whether a key is comparable.

Fix: put `comparable` in the constraint and let invalid code fail at compile time.

## Verification

Run this from `~/go-exercises/constraintcomposition`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Add one more test using a second key type to prove the registry is not coupled to `UserID`.

## Summary

- Constraints can combine method requirements and predeclared constraints.
- `comparable` is required for generic map keys.
- Domain constraints should state exactly what the implementation needs.
- Values should remain unconstrained unless the code uses extra capabilities.

## What's Next

Next: [Generic Middleware and Decorator](../13-generic-middleware-and-decorator/13-generic-middleware-and-decorator.md).

## Resources

- [Go Specification: General interfaces](https://go.dev/ref/spec#General_interfaces)
- [Go Specification: Map types](https://go.dev/ref/spec#Map_types)
- [Go Blog: When To Use Generics](https://go.dev/blog/when-generics)
