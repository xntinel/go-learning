# Exercise 4: Reflect-based typed-nil guard for a DI/plugin registry

A dependency-injection or plugin registry receives dependencies as `any`, so a
plain `dep == nil` check misses a typed nil — a `(*sqlDB)(nil)` passed as a
`Store` sails through and panics in production on first use. This module builds
`isNilValue(any) bool` with `reflect` and a `Register` that rejects both nil
forms.

## What you'll build

```text
diregistry/                independent module: example.com/diregistry
  go.mod                   go 1.26
  registry.go              isNilValue; Registry; Register; Get; ErrNilDependency
  cmd/
    demo/
      main.go              register a live dep, a nil, and a typed nil
  registry_test.go         table over every Kind; Register accept/reject
```

- Files: `registry.go`, `cmd/demo/main.go`, `registry_test.go`.
- Implement: `isNilValue(v any) bool` that guards `reflect.Value.IsNil` by Kind, and `Register(name string, dep any) error` that rejects both a nil interface and a typed nil.
- Test: a table over nil interface, typed-nil pointer, live pointer, nil/live map, nil slice, nil func, and non-nilable kinds (int, struct) — `isNilValue` returns the right answer and never panics.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/08-interfaces/07-nil-interface-values/04-reflect-nil-guard-di-container/cmd/demo
cd go-solutions/08-interfaces/07-nil-interface-values/04-reflect-nil-guard-di-container
```

### Why a plain nil check is not enough

`Register(name string, dep any)` accepts `any`, so the dependency is already
boxed in an interface by the time the function sees it. If a caller writes
`var db *sqlDB; reg.Register("store", db)`, then inside `Register` the parameter
`dep` is an `any` with dynamic type `*sqlDB` and a nil dynamic value: a typed
nil. `dep == nil` is false, so a naive guard admits it, and the first time
someone pulls `"store"` out of the registry and calls a method that dereferences
the nil pointer, the service panics — far from the registration site, hard to
trace.

`reflect` is the only way to detect this generically. `isNilValue` first handles
the genuine nil interface with `v == nil` (a real nil interface never reaches
reflect, and `reflect.ValueOf(nil)` would yield the zero Value with Kind
`Invalid`). Then it takes `reflect.ValueOf(v)` and switches on the Kind. Only
the nilable kinds — Chan, Func, Interface, Map, Pointer, Slice — support
`IsNil`; calling `IsNil` on an int, struct, or string panics. So the switch is
not decoration, it is the guard that keeps `isNilValue` itself from panicking on
a non-nilable value. For any non-nilable kind, the value cannot be nil, so the
default returns false.

Create `registry.go`:

```go
package diregistry

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
)

// ErrNilDependency is returned when a registration is a nil interface or a typed
// nil (e.g. a (*sqlDB)(nil) passed as an interface).
var ErrNilDependency = errors.New("registry: nil dependency")

// isNilValue reports whether v is a nil interface or a typed nil, without
// panicking on non-nilable kinds.
func isNilValue(v any) bool {
	if v == nil {
		return true // genuine nil interface: both words nil
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface,
		reflect.Map, reflect.Pointer, reflect.Slice:
		return rv.IsNil() // IsNil is only valid for these kinds
	default:
		return false // int, struct, string, ... cannot be nil
	}
}

// Registry is a name-keyed dependency container.
type Registry struct {
	mu   sync.RWMutex
	deps map[string]any
}

func New() *Registry {
	return &Registry{deps: make(map[string]any)}
}

// Register stores dep under name, rejecting both nil forms so a dead dependency
// never reaches a consumer.
func (r *Registry) Register(name string, dep any) error {
	if isNilValue(dep) {
		return fmt.Errorf("register %q: %w", name, ErrNilDependency)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deps[name] = dep
	return nil
}

// Get returns the dependency registered under name.
func (r *Registry) Get(name string) (any, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	dep, ok := r.deps[name]
	return dep, ok
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/diregistry"
)

// sqlDB stands in for a real dependency type.
type sqlDB struct{ dsn string }

func main() {
	reg := diregistry.New()

	// A live dependency: accepted.
	live := &sqlDB{dsn: "postgres://localhost"}
	fmt.Println("live pointer:", reg.Register("store", live))

	// A nil interface: rejected.
	err := reg.Register("logger", nil)
	fmt.Println("nil interface rejected:", errors.Is(err, diregistry.ErrNilDependency))

	// A typed nil: also rejected, even though dep == nil would be false.
	var dead *sqlDB
	err = reg.Register("cache", dead)
	fmt.Println("typed nil rejected:", errors.Is(err, diregistry.ErrNilDependency))

	_, ok := reg.Get("store")
	fmt.Println("store present:", ok)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
live pointer: <nil>
nil interface rejected: true
typed nil rejected: true
store present: true
```

### Tests

Create `registry_test.go`:

```go
package diregistry

import (
	"errors"
	"testing"
)

type store struct{ dsn string }

func TestIsNilValue(t *testing.T) {
	t.Parallel()

	var typedNilPtr *store
	livePtr := &store{}
	var nilMap map[string]int
	liveMap := map[string]int{"a": 1}
	var nilSlice []int
	var nilFunc func()

	tests := []struct {
		name string
		v    any
		want bool
	}{
		{"nil interface", nil, true},
		{"typed nil pointer", typedNilPtr, true},
		{"live pointer", livePtr, false},
		{"nil map", nilMap, true},
		{"live map", liveMap, false},
		{"nil slice", nilSlice, true},
		{"nil func", nilFunc, true},
		{"non-nilable int", 42, false},
		{"non-nilable struct", store{}, false},
		{"non-nilable string", "x", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isNilValue(tc.v); got != tc.want {
				t.Fatalf("isNilValue(%s) = %v; want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestRegisterRejectsNilForms(t *testing.T) {
	t.Parallel()

	reg := New()

	var typedNil *store
	if err := reg.Register("a", typedNil); !errors.Is(err, ErrNilDependency) {
		t.Fatalf("typed nil: got %v; want ErrNilDependency", err)
	}
	if err := reg.Register("b", nil); !errors.Is(err, ErrNilDependency) {
		t.Fatalf("nil interface: got %v; want ErrNilDependency", err)
	}
}

func TestRegisterAcceptsLiveDependency(t *testing.T) {
	t.Parallel()

	reg := New()
	if err := reg.Register("store", &store{dsn: "x"}); err != nil {
		t.Fatalf("Register live dep: %v", err)
	}
	if _, ok := reg.Get("store"); !ok {
		t.Fatal("store should be present after Register")
	}
}
```

## Review

The guard is correct when it distinguishes a nil interface, a typed nil, and a
live value, and never panics on a non-nilable kind. `isNilValue` handles the nil
interface with `v == nil`, then switches on `reflect.Value.Kind` so `IsNil` is
only ever called on Chan/Func/Interface/Map/Pointer/Slice; the table test drives
an int, struct, and string through it to prove the non-nilable path returns
false without panicking. `Register` rejects both nil forms with a wrapped
`ErrNilDependency` so a caller can match it with `errors.Is`. The mistake this
prevents is the one a plain `dep == nil` allows: a typed-nil dependency admitted
into the container and dereferenced far away, in production.

## Resources

- [`reflect.Value.IsNil`](https://pkg.go.dev/reflect#Value.IsNil) — the valid kinds and the panic contract.
- [`reflect.Value.Kind`](https://pkg.go.dev/reflect#Value.Kind) — the kind enumeration the switch uses.
- [The Laws of Reflection](https://go.dev/blog/laws-of-reflection) — how `reflect.ValueOf` maps an interface to a Value.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-slog-discardhandler-adapter.md](05-slog-discardhandler-adapter.md)
