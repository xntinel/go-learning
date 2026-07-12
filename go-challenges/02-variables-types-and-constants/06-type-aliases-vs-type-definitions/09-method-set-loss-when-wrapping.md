# Exercise 9: Defined Type vs Alias When Wrapping a Third-Party Struct

You import a config struct from a third-party library, it already has useful
methods, and you want to add your own `Validate()`. The obvious move —
`type LocalConfig ThirdParty` — silently drops every method the third-party type
had, because a defined type over a named type starts with an empty method set.
This exercise walks the trap and the two escape routes: an alias (keeps methods,
cannot add any) and embedding (keeps methods and lets you add your own).

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
wrapcfg/                  independent module: example.com/wrapcfg
  go.mod                  go 1.24
  vendor.go               a stand-in "third-party" ThirdParty type with Addr()
  config.go               NaiveConfig (defined, lost Addr); Config (embeds, keeps Addr)
  cmd/
    demo/
      main.go             shows the lost method and the embedding fix
  config_test.go          method-loss, conversion, embedding, Validate tests
```

- Files: `vendor.go`, `config.go`, `cmd/demo/main.go`, `config_test.go`.
- Implement: `ThirdParty` with an `Addr()` method; `NaiveConfig ThirdParty` (defined, loses `Addr`) with its own `Validate()`; `Config` embedding `ThirdParty` so `Addr` is promoted and `Validate` is added.
- Test: a `NaiveConfig` must be converted back to `ThirdParty` to reach `Addr`; `Config` exposes both `Addr` (promoted) and `Validate` (added).
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/02-variables-types-and-constants/06-type-aliases-vs-type-definitions/09-method-set-loss-when-wrapping/cmd/demo
cd go-solutions/02-variables-types-and-constants/06-type-aliases-vs-type-definitions/09-method-set-loss-when-wrapping
go mod edit -go=1.24
```

### The method-set rule, made concrete

Go's rule: a defined type whose underlying type is a *named* type does not inherit
that type's methods; the new method set starts empty. `ThirdParty` has an `Addr()`
method. Write `type NaiveConfig ThirdParty` and `NaiveConfig` has the same fields
but *no* `Addr()` — calling `nc.Addr()` does not compile. The methods did not move;
they simply are not part of the new type. This is surprising precisely because the
fields carry over, so the type looks the same until you reach for a method that
vanished. To reach `Addr` from a `NaiveConfig` you must convert back:
`ThirdParty(nc).Addr()`.

You do get one thing from the definition: you can add your own methods to
`NaiveConfig` (it is a local type), so `NaiveConfig` can have a `Validate()`. But
you have traded away the inherited behavior to get it.

An alias would keep the behavior: `type AliasConfig = ThirdParty` shares the
identical method set, so `Addr` works. But an alias *is* `ThirdParty`, owned by the
other package, so you cannot declare `Validate()` on it — the compiler rejects a
method whose receiver is a non-local type. Alias keeps methods, forbids adding
them; definition allows adding, drops inherited ones. Neither gives you both.

The tool that gives you both is *embedding*. `type Config struct { ThirdParty }`
embeds the third-party value; its exported methods (`Addr`) are promoted onto
`Config`, and because `Config` is your own local type you can also declare
`Validate()` on it. Embedding is the idiomatic answer to "keep the vendor's
behavior and add my own".

Create `vendor.go` (a stand-in for an imported third-party type):

```go
package wrapcfg

import "fmt"

// ThirdParty stands in for a struct imported from an external library. It already
// carries behavior: the Addr method.
type ThirdParty struct {
	Host string
	Port int
}

// Addr renders the host:port address. This is the method that a plain redefinition
// silently loses.
func (t ThirdParty) Addr() string {
	return fmt.Sprintf("%s:%d", t.Host, t.Port)
}
```

Create `config.go`:

```go
package wrapcfg

import (
	"errors"
	"fmt"
)

// ErrInvalidConfig is returned by Validate for a malformed config.
var ErrInvalidConfig = errors.New("invalid config")

// NaiveConfig is the trap: defining a new type over the named type ThirdParty
// drops ThirdParty's method set, so NaiveConfig has the fields but NOT Addr. You
// can add your own methods (Validate), but Addr is gone unless you convert back.
type NaiveConfig ThirdParty

// Validate can be declared because NaiveConfig is a local defined type.
func (c NaiveConfig) Validate() error {
	if c.Host == "" || c.Port <= 0 {
		return fmt.Errorf("host=%q port=%d: %w", c.Host, c.Port, ErrInvalidConfig)
	}
	return nil
}

// Addr on NaiveConfig must be reconstructed by converting back to ThirdParty,
// because the redefinition did not inherit the original method.
func (c NaiveConfig) Addr() string {
	return ThirdParty(c).Addr()
}

// Config is the correct wrapper: embedding ThirdParty promotes its methods
// (Addr) while letting us add our own (Validate) on the local type.
type Config struct {
	ThirdParty
	Timeout int
}

// Validate is a new method on the local Config type.
func (c Config) Validate() error {
	if c.Host == "" || c.Port <= 0 {
		return fmt.Errorf("host=%q port=%d: %w", c.Host, c.Port, ErrInvalidConfig)
	}
	if c.Timeout <= 0 {
		return fmt.Errorf("timeout=%d: %w", c.Timeout, ErrInvalidConfig)
	}
	return nil
}
```

### The method that is not there

The trap is a call that does not compile:

```text
nc := NaiveConfig{Host: "db", Port: 5432}
nc.Addr() // does NOT compile if NaiveConfig only redefines ThirdParty:
          // "nc.Addr undefined (type NaiveConfig has no field or method Addr)"
```

In this exercise `NaiveConfig` re-supplies `Addr` by converting back to
`ThirdParty` inside a method, which is the manual cost of the redefinition. The
`Config` wrapper avoids that cost entirely: `Addr` is promoted from the embedded
field.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/wrapcfg"
)

func main() {
	// The naive redefinition: Addr had to be re-supplied by converting back.
	naive := wrapcfg.NaiveConfig{Host: "db", Port: 5432}
	fmt.Println("naive addr:", naive.Addr())
	fmt.Println("naive valid:", naive.Validate() == nil)

	// The embedding wrapper: Addr is promoted for free, Validate is added.
	cfg := wrapcfg.Config{
		ThirdParty: wrapcfg.ThirdParty{Host: "cache", Port: 6379},
		Timeout:    30,
	}
	fmt.Println("config addr:", cfg.Addr()) // promoted from ThirdParty
	fmt.Println("config valid:", cfg.Validate() == nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
naive addr: db:5432
naive valid: true
config addr: cache:6379
config valid: true
```

### Tests

The tests show that `Config` exposes the promoted `Addr` and the added `Validate`,
that `Validate` rejects malformed input via the wrapped sentinel, and that a
`ThirdParty` value must be explicitly converted to reach the defined type — the
observable evidence of the method-set boundary.

Create `config_test.go`:

```go
package wrapcfg

import (
	"errors"
	"fmt"
	"testing"
)

func TestEmbeddingPromotesAddr(t *testing.T) {
	t.Parallel()

	cfg := Config{ThirdParty: ThirdParty{Host: "cache", Port: 6379}, Timeout: 30}

	if got := cfg.Addr(); got != "cache:6379" { // Addr promoted from ThirdParty
		t.Errorf("Addr = %q, want cache:6379", got)
	}
	if err := cfg.Validate(); err != nil { // Validate added on Config
		t.Errorf("Validate = %v, want nil", err)
	}
}

func TestConfigValidateRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  Config
	}{
		{"empty host", Config{ThirdParty: ThirdParty{Port: 1}, Timeout: 1}},
		{"bad port", Config{ThirdParty: ThirdParty{Host: "h"}, Timeout: 1}},
		{"bad timeout", Config{ThirdParty: ThirdParty{Host: "h", Port: 1}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := tc.cfg.Validate(); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("Validate() = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestDefinedTypeRequiresConversionForOriginalMethod(t *testing.T) {
	t.Parallel()

	tp := ThirdParty{Host: "db", Port: 5432}

	// A ThirdParty is NOT a NaiveConfig; converting is required (they are distinct
	// types, and the conversion is where the method-set boundary is visible).
	nc := NaiveConfig(tp)

	// NaiveConfig re-supplies Addr only by converting back to ThirdParty.
	if got := nc.Addr(); got != "db:5432" {
		t.Errorf("NaiveConfig.Addr = %q, want db:5432", got)
	}
	if got := ThirdParty(nc).Addr(); got != "db:5432" {
		t.Errorf("converted Addr = %q, want db:5432", got)
	}
}

func ExampleConfig_Addr() {
	cfg := Config{ThirdParty: ThirdParty{Host: "cache", Port: 6379}, Timeout: 30}
	fmt.Println(cfg.Addr())
	// Output: cache:6379
}
```

## Review

The wrapping is correct when `Config` (embedding) exposes both the promoted `Addr`
and the added `Validate`, and when reaching a third-party method from the
redefined `NaiveConfig` requires converting back to `ThirdParty`. The mistake this
exercise exists to prevent is `type Local ThirdParty` in the belief that it keeps
the vendor's methods — it does not; the method set is empty. Remember the three
options and their trade-offs: a definition lets you add methods but drops inherited
ones; an alias keeps the method set but forbids adding methods to a non-local type;
embedding keeps the inherited behavior and lets you add your own, which is why it is
the idiomatic wrapper. Assert `Validate`'s failures with `errors.Is` against the
sentinel.

## Resources

- [Go Language Spec: Method sets](https://go.dev/ref/spec#Method_sets) — why a defined type over a named type has an empty method set.
- [Effective Go: Embedding](https://go.dev/doc/effective_go#embedding) — promoting a wrapped type's methods.
- [Go Language Spec: Type definitions](https://go.dev/ref/spec#Type_definitions) — definitions do not carry the underlying named type's methods.

---

Prev: [08-json-marshal-recursion-guard.md](08-json-marshal-recursion-guard.md) | Next: [10-order-status-enum-defined-type.md](10-order-status-enum-defined-type.md)
