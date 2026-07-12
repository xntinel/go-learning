# Exercise 3: Migrating a Legacy atomic.Value Store Without Breaking Callers

Codebases older than Go 1.19 — and several corners of the standard library —
publish config through the interface-typed `atomic.Value`, which trades the
type system for three runtime panics waiting to happen. This exercise builds
such a legacy store, pins its exact failure modes in tests, and then migrates
it to `atomic.Pointer[T]` behind an adapter so callers keep working while the
panics become unrepresentable.

This module is fully self-contained: its own `go mod init`, every type it
needs, its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
cfglegacy/                 independent module: example.com/cfglegacy
  go.mod
  config.go                type Config
  legacy.go                LegacyStore over atomic.Value (Store/Load/Swap, any-typed)
  typed.go                 TypedStore over atomic.Pointer[Config]; Migrate(legacy)
  cmd/
    demo/
      main.go              runnable demo: nil Load, a recovered type panic, the migration
  store_test.go            panic-pinning tests (defer/recover), migration table tests
```

- Files: `config.go`, `legacy.go`, `typed.go`, `cmd/demo/main.go`, `store_test.go`.
- Implement: `LegacyStore` exposing `atomic.Value` semantics verbatim; `TypedStore` on `atomic.Pointer[Config]`; `Migrate` converting one to the other with sentinel errors for an empty or wrongly-typed legacy store.
- Test: defer/recover tests asserting the exact panics (`Store(nil)`, inconsistently typed second `Store`), the nil-`any`-before-first-`Store` behavior, and `errors.Is` assertions on both migration sentinels.
- Verify: `go test -count=1 -race ./...`

### What atomic.Value actually promises, and where it bites

`atomic.Value` predates generics, so its API is `Store(val any)` and
`Load() any`. Because it is interface-typed, three contracts are enforced at
runtime instead of compile time:

1. `Store(nil)` panics with `sync/atomic: store of nil value into Value`.
   There is no way to represent "no config" except never storing.
2. Every `Store` must carry the same concrete type as the first one. Storing
   a `*ConfigV2` into a Value that previously held a `*Config` panics with
   `sync/atomic: store of inconsistently typed value into Value`. This is
   the classic hot-reload refactor landmine: someone widens the config struct
   under a new type name and the first reload after deploy panics the fleet.
3. `Load` returns a nil `any` before the first `Store`, so the idiomatic
   read `v.Load().(*Config)` panics at startup if any reader can run before
   initialization. Callers must write the two-result assertion
   `cfg, ok := v.Load().(*Config)` and handle `!ok` — and in legacy code,
   many do not.

Why does `Value` behave this way? It stores the interface's type word and
data word with two separate atomic operations and spins readers during the
first store; a nil value and a changing type word would make that protocol
unsound, hence the panics. `atomic.Pointer[T]` sidesteps the whole problem:
there is only a data word, the type is fixed at compile time, `Load` returns
a `*T` you can nil-check, and no operation panics — the failure modes are not
handled better, they are *unrepresentable*.

The migration itself is the realistic part. You rarely get to delete the
legacy store in one commit; you read whatever it currently holds, validate it
with an ok-checked assertion, seed the typed store, and return sentinel
errors (`ErrEmptyStore`, `ErrWrongType`) the caller can branch on with
`errors.Is` — because a migration running at process startup must distinguish
"nothing stored yet, seed a default" from "someone stored garbage, refuse to
proceed".

Create `config.go`:

```go
// Package cfglegacy demonstrates migrating an interface-typed atomic.Value
// config store to the compile-time-typed atomic.Pointer[Config], pinning
// the legacy API's runtime panics along the way.
package cfglegacy

// Config is one immutable configuration snapshot.
type Config struct {
	MaxConnections int
	TimeoutMillis  int
	Version        int
}
```

Create `legacy.go`:

```go
package cfglegacy

import "sync/atomic"

// LegacyStore is the pre-Go-1.19 shape: an interface-typed atomic.Value.
// It reproduces atomic.Value semantics exactly, including the panics:
// Store(nil) panics, storing a different concrete type than the first
// Store panics, and Load returns a nil any before the first Store.
type LegacyStore struct {
	v atomic.Value
}

// Store publishes cfg. Panics if cfg is nil or if its concrete type
// differs from a previously stored value's.
func (s *LegacyStore) Store(cfg any) {
	s.v.Store(cfg)
}

// Load returns the current value, or a nil any before the first Store.
// Callers must use the two-result type assertion.
func (s *LegacyStore) Load() any {
	return s.v.Load()
}

// Swap stores cfg and returns the previous value (nil any if none).
// Same panic rules as Store.
func (s *LegacyStore) Swap(cfg any) any {
	return s.v.Swap(cfg)
}
```

Create `typed.go`:

```go
package cfglegacy

import (
	"errors"
	"fmt"
	"sync/atomic"
)

var (
	// ErrEmptyStore reports a legacy store that has never been stored to.
	ErrEmptyStore = errors.New("legacy store is empty")
	// ErrWrongType reports a legacy store holding something other than *Config.
	ErrWrongType = errors.New("legacy store holds unexpected type")
)

// TypedStore is the migration target: compile-time typed, no runtime
// panics. Load returns a nil *Config before the first Store, which the
// type system forces callers to be able to handle.
type TypedStore struct {
	ptr atomic.Pointer[Config]
}

// Store publishes cfg. Unlike atomic.Value, storing a differently-shaped
// config is a compile error here, not a runtime panic.
func (s *TypedStore) Store(cfg *Config) {
	s.ptr.Store(cfg)
}

// Load returns the current snapshot, or nil before the first Store.
func (s *TypedStore) Load() *Config {
	return s.ptr.Load()
}

// Swap stores cfg and returns the previous snapshot (nil if none).
func (s *TypedStore) Swap(cfg *Config) *Config {
	return s.ptr.Swap(cfg)
}

// Migrate reads the legacy store's current value and seeds a TypedStore
// with it. It returns ErrEmptyStore if nothing was ever stored and
// ErrWrongType (wrapped with the offending concrete type) if the legacy
// store holds anything but a *Config.
func Migrate(l *LegacyStore) (*TypedStore, error) {
	cur := l.Load()
	if cur == nil {
		return nil, fmt.Errorf("migrate: %w", ErrEmptyStore)
	}
	cfg, ok := cur.(*Config)
	if !ok {
		return nil, fmt.Errorf("migrate: %w: %T", ErrWrongType, cur)
	}
	t := &TypedStore{}
	t.ptr.Store(cfg)
	return t, nil
}
```

### The runnable demo

The demo walks the three legacy behaviors in order — nil `Load` before the
first `Store`, a working store, and the inconsistent-type panic (recovered,
so you can read the exact message the fleet would have died with) — then
migrates to the typed store and swaps in a v2 with no panic possible.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/cfglegacy"
)

func main() {
	legacy := &cfglegacy.LegacyStore{}
	fmt.Printf("legacy Load before Store: %v\n", legacy.Load())

	legacy.Store(&cfglegacy.Config{MaxConnections: 100, Version: 1})
	if cfg, ok := legacy.Load().(*cfglegacy.Config); ok {
		fmt.Printf("legacy config: v=%d max=%d\n", cfg.Version, cfg.MaxConnections)
	}

	func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("recovered: %v\n", r)
			}
		}()
		legacy.Store("v2-as-string") // different concrete type: panics
	}()

	typed, err := cfglegacy.Migrate(legacy)
	if err != nil {
		fmt.Println("migrate failed:", err)
		return
	}
	typed.Store(&cfglegacy.Config{MaxConnections: 200, Version: 2})
	cfg := typed.Load()
	fmt.Printf("typed config: v=%d max=%d\n", cfg.Version, cfg.MaxConnections)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
legacy Load before Store: <nil>
legacy config: v=1 max=100
recovered: sync/atomic: store of inconsistently typed value into Value
typed config: v=2 max=200
```

### Tests

The panic tests use the defer/recover pattern with `t.Fatal` *after* the
panicking call — if execution reaches it, the expected panic never fired. The
recovered value is matched by substring rather than full equality so the
tests pin the documented failure mode, not one Go release's exact wording.
The migration tests are table-driven over the three legacy states and assert
sentinels with `errors.Is`, which works because `Migrate` wraps them with
`%w`.

Create `store_test.go`:

```go
package cfglegacy

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func mustPanicContaining(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected panic containing %q; no panic", want)
		}
		if got := fmt.Sprint(r); !strings.Contains(got, want) {
			t.Fatalf("panic = %q, want it to contain %q", got, want)
		}
	}()
	fn()
}

func TestLegacyLoadBeforeStoreIsNil(t *testing.T) {
	t.Parallel()

	s := &LegacyStore{}
	if got := s.Load(); got != nil {
		t.Fatalf("Load before Store = %v, want nil", got)
	}
	// The safe read pattern legacy callers must use:
	if cfg, ok := s.Load().(*Config); ok || cfg != nil {
		t.Fatalf("assertion on empty store = %v, %v; want nil, false", cfg, ok)
	}
}

func TestLegacyStoreNilPanics(t *testing.T) {
	t.Parallel()

	s := &LegacyStore{}
	mustPanicContaining(t, "store of nil value", func() {
		s.Store(nil)
	})
}

func TestLegacyInconsistentTypePanics(t *testing.T) {
	t.Parallel()

	s := &LegacyStore{}
	s.Store(&Config{Version: 1})
	mustPanicContaining(t, "inconsistently typed", func() {
		s.Store("not a *Config")
	})
}

func TestLegacySwapReturnsPrevious(t *testing.T) {
	t.Parallel()

	s := &LegacyStore{}
	s.Store(&Config{Version: 1})
	old := s.Swap(&Config{Version: 2})
	cfg, ok := old.(*Config)
	if !ok || cfg.Version != 1 {
		t.Fatalf("Swap returned %v, want *Config v1", old)
	}
}

func TestMigrate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		prime   func(*LegacyStore)
		wantErr error
		wantVer int
	}{
		{
			name:    "empty store",
			prime:   func(*LegacyStore) {},
			wantErr: ErrEmptyStore,
		},
		{
			name:    "wrong concrete type",
			prime:   func(s *LegacyStore) { s.Store(map[string]int{"max": 1}) },
			wantErr: ErrWrongType,
		},
		{
			name:    "current config carried over",
			prime:   func(s *LegacyStore) { s.Store(&Config{MaxConnections: 42, Version: 7}) },
			wantVer: 7,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			legacy := &LegacyStore{}
			tc.prime(legacy)

			typed, err := Migrate(legacy)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Migrate err = %v, want errors.Is(%v)", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Migrate: %v", err)
			}
			if got := typed.Load(); got.Version != tc.wantVer {
				t.Fatalf("migrated Version = %d, want %d", got.Version, tc.wantVer)
			}
		})
	}
}

func TestTypedStoreNoPanicPaths(t *testing.T) {
	t.Parallel()

	s := &TypedStore{}
	if got := s.Load(); got != nil {
		t.Fatalf("Load before Store = %v, want nil *Config", got)
	}
	s.Store(&Config{Version: 1})
	old := s.Swap(&Config{Version: 2})
	if old.Version != 1 || s.Load().Version != 2 {
		t.Fatalf("Swap: old=%+v now=%+v", old, s.Load())
	}
}

func TestTypedStoreConcurrent(t *testing.T) {
	t.Parallel()

	s := &TypedStore{}
	s.Store(&Config{Version: 1})

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				if c := s.Load(); c == nil {
					panic("nil snapshot after initial Store")
				}
			}
		}()
	}
	for v := 2; v <= 50; v++ {
		s.Store(&Config{Version: v})
	}
	wg.Wait()
}

func ExampleMigrate() {
	legacy := &LegacyStore{}
	legacy.Store(&Config{MaxConnections: 100, Version: 3})

	typed, _ := Migrate(legacy)
	cfg := typed.Load()
	fmt.Println(cfg.MaxConnections, cfg.Version)
	// Output: 100 3
}
```

## Review

The tests are the specification here: after this module you should be able to
state, without looking, the three ways `atomic.Value` fails at runtime and
why `atomic.Pointer[T]` cannot fail in any of them. The most common mistake
when writing panic tests is putting the assertion *before* the panicking call
or forgetting that a passing `recover()` swallows the panic — the
`mustPanicContaining` helper centralizes the pattern, and its `t.Fatalf` on a
nil recover is what catches "the panic stopped happening" regressions.

On the migration itself: notice `Migrate` refuses a wrongly-typed store
instead of silently seeding a default. That asymmetry is deliberate — an
empty store means "not initialized yet" and is often fine to seed, but a
store holding the wrong type means some code path you do not understand is
writing to it, and proceeding would split the config into two divergent
stores. Distinguishing the two is exactly why the sentinels exist and are
wrapped with `%w`. Verify with `go test -count=1 -race ./...`.

## Resources

- [sync/atomic: Value](https://pkg.go.dev/sync/atomic#Value) — the documented panic conditions on Store and Swap.
- [sync/atomic: Pointer](https://pkg.go.dev/sync/atomic#Pointer) — the typed replacement.
- [errors package](https://pkg.go.dev/errors) — sentinel errors, wrapping, and errors.Is.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-copy-on-write-flag-builder.md](02-copy-on-write-flag-builder.md) | Next: [04-file-poll-hot-reloader.md](04-file-poll-hot-reloader.md)
