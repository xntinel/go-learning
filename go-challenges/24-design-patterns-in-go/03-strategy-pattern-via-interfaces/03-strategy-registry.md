# Exercise 3: A Strategy Registry Chosen by Key

A strategy is often selected not by a Go expression but by a string that arrives at runtime — a config field, a CLI flag, an HTTP parameter, a database column. This exercise builds a registry that maps string keys to strategies, so the algorithm is chosen at request time, new strategies join by registration rather than by editing a dispatcher, and an unknown key fails loudly with a diagnosable error instead of silently defaulting.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
registry.go          PricingStrategy interface, StrategyFunc adapter,
                     FlatDiscount/PercentageDiscount, Registry, DefaultRegistry
cmd/
  demo/
    main.go          register a strategy at runtime, then apply keys to a cart
registry_test.go     known keys, unknown-key error, overwrite, extension, sort
```

- Files: `registry.go`, `cmd/demo/main.go`, `registry_test.go`.
- Implement: `PricingStrategy`, the `StrategyFunc` adapter, two concrete strategies, and a `Registry` with `Register`, `Get`, `Keys`, and `Apply`, plus a `DefaultRegistry` preloaded with built-ins.
- Test: known keys resolve and compute, an unknown key errors without mutating the subtotal, re-registration overwrites, a runtime-added strategy works, and `Keys` is sorted.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/24-design-patterns-in-go/03-strategy-pattern-via-interfaces/03-strategy-registry/cmd/demo && cd go-solutions/24-design-patterns-in-go/03-strategy-pattern-via-interfaces/03-strategy-registry
```

### Open extension, closed dispatch

A naive runtime selector is a `switch key` that constructs a strategy per case, and it has the defect the strategy pattern exists to remove: every new algorithm edits the switch. A registry inverts that. The dispatch logic is one map lookup that never changes; extension happens by calling `Register("key", impl)`. The standard library uses exactly this shape — `sql.Register` for database drivers, `image.RegisterFormat` for decoders — so that a driver in a package the core has never imported can plug in by registering a key.

The contract is a one-method interface, `PricingStrategy`, with just `Calculate`. Keeping it to one method is what lets the registry accept both struct strategies and plain functions: `StrategyFunc` is the adapter (the same `http.HandlerFunc` move) that gives a function a `Calculate` method, so a closure can be registered next to a `FlatDiscount` value. The registry stores `map[string]PricingStrategy`, and because the value type is the interface, it does not care whether a given entry is a struct or a wrapped function.

`Register` binds a key, overwriting any prior binding so a later registration wins — the property that lets a configuration override a built-in. `Get` is where the safety lives: a missing key returns an error that names the key and lists the available ones, so a typo like `"percetage"` produces a message a human can act on rather than a zero-value strategy that silently charges full price. `Keys` returns the registered keys sorted, both for stable output and so callers can validate input or render a menu. `Apply` is the convenience path that looks up and runs in one call, and on a miss it returns the subtotal unchanged alongside the error, so a caller that ignores the error still does not corrupt the amount.

`DefaultRegistry` preloads the built-ins. It registers a `none` identity (as a `StrategyFunc` closure, to show a function entry living beside the struct entries), a `welcome` flat discount, and a `loyalty` percentage. A program builds on it by registering more keys at startup; nothing in the dispatch path changes.

Create `registry.go`:

```go
package pricing

import (
	"fmt"
	"sort"
)

// PricingStrategy is the family contract: turn a subtotal into a discount and
// the resulting total.
type PricingStrategy interface {
	Calculate(subtotal float64) (discount float64, total float64)
}

// StrategyFunc adapts a plain function to PricingStrategy so closures can be
// registered alongside struct strategies.
type StrategyFunc func(subtotal float64) (float64, float64)

func (f StrategyFunc) Calculate(subtotal float64) (float64, float64) { return f(subtotal) }

// FlatDiscount and PercentageDiscount are two concrete strategies the default
// registry ships with.
type FlatDiscount struct{ Amount float64 }

func (d FlatDiscount) Calculate(subtotal float64) (float64, float64) {
	if subtotal <= 0 {
		return 0, subtotal
	}
	disc := d.Amount
	if disc > subtotal {
		disc = subtotal
	}
	return disc, subtotal - disc
}

type PercentageDiscount struct{ Percent float64 }

func (d PercentageDiscount) Calculate(subtotal float64) (float64, float64) {
	if subtotal <= 0 {
		return 0, subtotal
	}
	disc := subtotal * (d.Percent / 100)
	return disc, subtotal - disc
}

// Registry maps a string key to a strategy, so the choice of algorithm can be
// made at runtime from configuration or user input rather than hard-coded.
type Registry struct {
	strategies map[string]PricingStrategy
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{strategies: make(map[string]PricingStrategy)}
}

// Register binds key to s, overwriting any existing binding for that key.
func (r *Registry) Register(key string, s PricingStrategy) {
	r.strategies[key] = s
}

// Get returns the strategy bound to key. The error names the missing key and
// lists the keys that are available, so a typo in a config file is diagnosable.
func (r *Registry) Get(key string) (PricingStrategy, error) {
	s, ok := r.strategies[key]
	if !ok {
		return nil, fmt.Errorf("pricing: no strategy registered for %q (have %v)", key, r.Keys())
	}
	return s, nil
}

// Keys returns the registered keys in sorted order for stable output.
func (r *Registry) Keys() []string {
	keys := make([]string, 0, len(r.strategies))
	for k := range r.strategies {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Apply looks up key and runs the strategy in one call.
func (r *Registry) Apply(key string, subtotal float64) (discount, total float64, err error) {
	s, err := r.Get(key)
	if err != nil {
		return 0, subtotal, err
	}
	d, t := s.Calculate(subtotal)
	return d, t, nil
}

// DefaultRegistry returns a registry preloaded with the built-in strategies.
func DefaultRegistry() *Registry {
	r := NewRegistry()
	r.Register("none", StrategyFunc(func(s float64) (float64, float64) { return 0, s }))
	r.Register("welcome", FlatDiscount{Amount: 10})
	r.Register("loyalty", PercentageDiscount{Percent: 15})
	return r
}
```

### The runnable demo

The demo starts from the default registry, adds a `blackfriday` strategy at runtime to show extension, prints the sorted keys, then applies a list of keys — the kind of list that would arrive from config or a request — including a bogus one to show the error path. On a `$200` subtotal, `loyalty` (15%) removes `$30`, `blackfriday` (40%) removes `$80`, and `bogus` returns the subtotal unchanged with a diagnostic error.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/pricing-registry"
)

func main() {
	reg := pricing.DefaultRegistry()

	// A new strategy joins the system by registering a key; no dispatcher edit.
	reg.Register("blackfriday", pricing.PercentageDiscount{Percent: 40})

	fmt.Printf("registered keys: %v\n\n", reg.Keys())

	subtotal := 200.0
	// The keys could come from a config file, a CLI flag, or an HTTP request.
	for _, key := range []string{"none", "welcome", "loyalty", "blackfriday", "bogus"} {
		d, total, err := reg.Apply(key, subtotal)
		if err != nil {
			fmt.Printf("%-12s error: %v\n", key, err)
			continue
		}
		fmt.Printf("%-12s discount=$%6.2f  total=$%7.2f\n", key, d, total)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
registered keys: [blackfriday loyalty none welcome]

none         discount=$  0.00  total=$ 200.00
welcome      discount=$ 10.00  total=$ 190.00
loyalty      discount=$ 30.00  total=$ 170.00
blackfriday  discount=$ 80.00  total=$ 120.00
bogus        error: pricing: no strategy registered for "bogus" (have [blackfriday loyalty none welcome])
```

### Tests

The tests pin the registry's contract: every default key resolves and computes correctly, an unknown key returns an error whose message names the bad key and lists the valid ones while leaving the subtotal untouched, re-registration overwrites, a strategy added at runtime is immediately usable, and `Keys` comes back sorted. `TestStrategyFunc_SatisfiesInterface` proves the function adapter is a full `PricingStrategy`.

Create `registry_test.go`:

```go
package pricing

import (
	"reflect"
	"strings"
	"testing"
)

func TestDefaultRegistry_KnownKeys(t *testing.T) {
	t.Parallel()

	reg := DefaultRegistry()
	cases := []struct {
		key          string
		sub          float64
		wantD, wantT float64
	}{
		{"none", 200, 0, 200},
		{"welcome", 200, 10, 190},
		{"loyalty", 200, 30, 170},
	}
	for _, tc := range cases {
		d, total, err := reg.Apply(tc.key, tc.sub)
		if err != nil {
			t.Fatalf("Apply(%q): unexpected error %v", tc.key, err)
		}
		if d != tc.wantD || total != tc.wantT {
			t.Errorf("Apply(%q, %v) = (%v, %v), want (%v, %v)", tc.key, tc.sub, d, total, tc.wantD, tc.wantT)
		}
	}
}

func TestRegistry_UnknownKeyErrors(t *testing.T) {
	t.Parallel()

	reg := DefaultRegistry()
	_, total, err := reg.Apply("nope", 200)
	if err == nil {
		t.Fatal("Apply with unknown key: want error, got nil")
	}
	if total != 200 {
		t.Errorf("total on error = %v, want subtotal 200 unchanged", total)
	}
	// The error names the missing key and lists what is available.
	if !strings.Contains(err.Error(), "nope") || !strings.Contains(err.Error(), "loyalty") {
		t.Errorf("error message %q lacks key or available list", err.Error())
	}
}

func TestRegistry_RegisterOverwrites(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	reg.Register("x", FlatDiscount{Amount: 5})
	reg.Register("x", FlatDiscount{Amount: 50})
	d, _, err := reg.Apply("x", 100)
	if err != nil {
		t.Fatal(err)
	}
	if d != 50 {
		t.Errorf("overwritten strategy discount = %v, want 50", d)
	}
}

func TestRegistry_RuntimeExtension(t *testing.T) {
	t.Parallel()

	// A new strategy is added without touching any existing dispatch code.
	reg := DefaultRegistry()
	reg.Register("clearance", StrategyFunc(func(s float64) (float64, float64) {
		return s * 0.5, s * 0.5
	}))
	d, total, err := reg.Apply("clearance", 80)
	if err != nil {
		t.Fatal(err)
	}
	if d != 40 || total != 40 {
		t.Errorf("clearance = (%v, %v), want (40, 40)", d, total)
	}
}

func TestRegistry_KeysSorted(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	reg.Register("zeta", FlatDiscount{})
	reg.Register("alpha", FlatDiscount{})
	reg.Register("mid", FlatDiscount{})
	if got, want := reg.Keys(), []string{"alpha", "mid", "zeta"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Keys() = %v, want %v", got, want)
	}
}

func TestStrategyFunc_SatisfiesInterface(t *testing.T) {
	t.Parallel()

	var s PricingStrategy = StrategyFunc(func(sub float64) (float64, float64) {
		return sub * 0.2, sub * 0.8
	})
	if d, total := s.Calculate(100); d != 20 || total != 80 {
		t.Errorf("StrategyFunc.Calculate(100) = (%v, %v), want (20, 80)", d, total)
	}
}
```

## Review

The registry is correct when extension never touches dispatch and a miss never passes silently. Confirm `Get` returns an error rather than a nil-but-usable strategy for an unknown key, and that the message both names the key and enumerates the valid ones — that is what turns a config typo into a fixable report instead of a wrong charge. Confirm `Apply` returns the subtotal unchanged on the error path so a caller that forgets to check `err` still does not mutate the amount into a half-computed value. The `StrategyFunc` adapter is what keeps the contract one method wide; verify a registered closure and a registered struct are indistinguishable to `Apply`, which is the whole point of storing the interface as the map value.

Common mistakes for this feature. The first is a silent default: returning a no-op or zero-value strategy for an unknown key, which converts a misconfiguration into a wrong number with no signal. The second is reintroducing a `switch` somewhere — a `switch key` that still constructs strategies defeats the registry; all selection must be the single map lookup. The third is an unstable `Keys`: ranging a map yields a random order, so callers that diff or display the keys see flapping output unless the slice is sorted. Note that this registry is not safe for concurrent registration and lookup; a server that registers at startup and only reads afterward is fine, but one that registers during request handling needs a mutex or a copy-on-write map.

## Resources

- [`database/sql.Register`](https://pkg.go.dev/database/sql#Register) — the standard library's driver registry, the canonical "register by key, dispatch by lookup" design this exercise mirrors.
- [Strategy pattern](https://refactoring.guru/design-patterns/strategy) — the pattern whose runtime-selection variant a keyed registry implements.
- [`sort.Strings`](https://pkg.go.dev/sort#Strings) — sorting the keys so a map's random iteration order does not leak into output.

---

Back to [02-strategy-via-function-types.md](02-strategy-via-function-types.md) | Next: [04-rate-limiter-strategies.md](04-rate-limiter-strategies.md)
