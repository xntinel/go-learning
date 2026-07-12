# Exercise 3: Generic Options

Every config type in a codebase otherwise repeats the same boilerplate: its own `type Option func(*X) error` and its own constructor loop. Go generics collapse that to a single reusable `Option[T]` type and a single `New[T]` constructor that serve every config type. This exercise builds that toolkit and demonstrates it on two unrelated types — an `HTTPConfig` and a `CacheConfig` — configured by the same option type and the same constructor.

This module is fully self-contained. It starts with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
config.go              Option[T], New[T], HTTPConfig + its options, CacheConfig + its options, sentinels
cmd/
  demo/
    main.go            build an HTTPConfig and a CacheConfig through the one generic New
config_test.go         defaults, per-type validation, reuse across both types, order + short-circuit
```

- Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
- Implement: `type Option[T any] func(*T) error`, `New[T any](defaults T, opts ...Option[T]) (T, error)`, and per-type `WithX` builders for both `HTTPConfig` and `CacheConfig`.
- Test: `config_test.go` proves both types reuse the same `New`, every per-type validator fires, the last option wins, and a failure short-circuits the remaining options.
- Verify: `go test -race ./...` then `go run ./cmd/demo`.

Set up the module:

```bash
mkdir -p go-solutions/24-design-patterns-in-go/01-functional-options-deep-dive/03-generic-options/cmd/demo && cd go-solutions/24-design-patterns-in-go/01-functional-options-deep-dive/03-generic-options
```

### Why generics here, and the trade-off it makes

In the previous exercises, `Option` and `New` were tied to one concrete type. Add a second config type and you copy the whole machinery: a new option type, a new loop, a new short-circuit. The generic form parameterizes both over `T`, so the option type and the constructor are written once. `New[T]` takes a `defaults` value of type `T` and applies each `Option[T]` to a pointer to it; `T` is inferred from the `defaults` argument, so callers write `config.New(config.HTTPDefaults(), ...)` with no explicit type argument. The body is the same defaults-then-options-in-order loop as before, including the short-circuit on the first error and the `fmt.Errorf("config: %w", err)` wrap that keeps each sentinel matchable.

There are two deliberate design shifts from the concrete form, and both are trade-offs worth naming. First, defaults move out of the constructor and into a caller-supplied value (`HTTPDefaults()`, `CacheDefaults()`), because a single generic `New` cannot know the per-type defaults; passing them in is what keeps it generic. Second, the config fields here are exported. The generic form pairs naturally with exported fields — the demo and tests read `h.Addr` directly — which gives up the after-construction immutability that Exercise 1's unexported-field design provides. That is not a contradiction of the earlier lesson but a different point on the same spectrum: choose the concrete, unexported-field form when one type wants the tightest encapsulation; choose this generic, exported-field form when several config types would otherwise duplicate the same option machinery and you value the de-duplication more than sealing the fields.

The payoff is visible in the file: `HTTPConfig` and `CacheConfig` share `Option[T]` and `New[T]` and differ only in their `WithX` builders. Adding a third config type would add only its struct, its defaults function, and its options — never another option type or another constructor.

Create `config.go`:

```go
package config

import (
	"errors"
	"fmt"
	"time"
)

// Option is a generic functional option: one type parameterized by the value it
// configures. Defining it once means HTTPConfig, CacheConfig, and any future
// config type all reuse the same option type and the same constructor, instead
// of each redeclaring its own `type Option func(*X) error` and its own loop.
type Option[T any] func(*T) error

// New starts from a caller-supplied defaults value and applies each option in
// order, stopping at the first failure. The same generic constructor serves
// every config type; T is inferred from the defaults argument.
func New[T any](defaults T, opts ...Option[T]) (T, error) {
	for _, opt := range opts {
		if err := opt(&defaults); err != nil {
			return defaults, fmt.Errorf("config: %w", err)
		}
	}
	return defaults, nil
}

// HTTPConfig is configured with Option[HTTPConfig] values.
type HTTPConfig struct {
	Addr       string
	Timeout    time.Duration
	MaxRetries int
}

// HTTPDefaults is the base value New starts from for HTTPConfig.
func HTTPDefaults() HTTPConfig {
	return HTTPConfig{Addr: ":8080", Timeout: 5 * time.Second, MaxRetries: 3}
}

var (
	ErrEmptyAddr       = errors.New("addr must not be empty")
	ErrBadTimeout      = errors.New("timeout must be positive")
	ErrNegativeRetries = errors.New("max retries must not be negative")
)

func WithAddr(addr string) Option[HTTPConfig] {
	return func(c *HTTPConfig) error {
		if addr == "" {
			return ErrEmptyAddr
		}
		c.Addr = addr
		return nil
	}
}

func WithTimeout(d time.Duration) Option[HTTPConfig] {
	return func(c *HTTPConfig) error {
		if d <= 0 {
			return fmt.Errorf("%w: got %s", ErrBadTimeout, d)
		}
		c.Timeout = d
		return nil
	}
}

func WithMaxRetries(n int) Option[HTTPConfig] {
	return func(c *HTTPConfig) error {
		if n < 0 {
			return fmt.Errorf("%w: got %d", ErrNegativeRetries, n)
		}
		c.MaxRetries = n
		return nil
	}
}

// CacheConfig is a second, unrelated type configured by the SAME Option[T] and
// the SAME New constructor, which is the entire point of making them generic.
type CacheConfig struct {
	MaxEntries int
	TTL        time.Duration
}

func CacheDefaults() CacheConfig {
	return CacheConfig{MaxEntries: 1024, TTL: time.Minute}
}

var (
	ErrBadCapacity = errors.New("max entries must be at least 1")
	ErrBadTTL      = errors.New("ttl must be positive")
)

func WithMaxEntries(n int) Option[CacheConfig] {
	return func(c *CacheConfig) error {
		if n < 1 {
			return fmt.Errorf("%w: got %d", ErrBadCapacity, n)
		}
		c.MaxEntries = n
		return nil
	}
}

func WithTTL(d time.Duration) Option[CacheConfig] {
	return func(c *CacheConfig) error {
		if d <= 0 {
			return fmt.Errorf("%w: got %s", ErrBadTTL, d)
		}
		c.TTL = d
		return nil
	}
}
```

### The runnable demo

The demo builds an `HTTPConfig` and then a `CacheConfig` through the exact same `config.New`, which is the whole point — one constructor, two unrelated types, type inferred from the defaults argument. It closes by passing an invalid capacity to show that validation still flows through the generic constructor and the sentinel is still matchable with `errors.Is`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"time"

	"example.com/genoptions"
)

func main() {
	// One generic New builds an HTTPConfig...
	h, err := config.New(config.HTTPDefaults(),
		config.WithAddr(":9000"),
		config.WithTimeout(15*time.Second),
	)
	if err != nil {
		fmt.Println("http error:", err)
		return
	}
	fmt.Printf("http: addr=%s timeout=%s retries=%d\n", h.Addr, h.Timeout, h.MaxRetries)

	// ...and the same New builds an unrelated CacheConfig.
	c, err := config.New(config.CacheDefaults(),
		config.WithMaxEntries(4096),
	)
	if err != nil {
		fmt.Println("cache error:", err)
		return
	}
	fmt.Printf("cache: entries=%d ttl=%s\n", c.MaxEntries, c.TTL)

	// Validation still flows through the generic constructor.
	_, err = config.New(config.CacheDefaults(), config.WithMaxEntries(0))
	fmt.Printf("bad capacity rejected: %v (is ErrBadCapacity=%t)\n",
		err, errors.Is(err, config.ErrBadCapacity))
}
```

The import path is the module path `example.com/genoptions`; the package it names is `config`. Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
http: addr=:9000 timeout=15s retries=3
cache: entries=4096 ttl=1m0s
bad capacity rejected: config: max entries must be at least 1: got 0 (is ErrBadCapacity=true)
```

### Tests

The tests prove the generic constructor behaves for both types: defaults survive, each per-type validator fires through `errors.Is`, and the order/short-circuit invariants hold exactly as in the concrete form. `TestCacheReusesSameGenericConstructor` exists specifically to show the second type goes through the identical `New`.

Create `config_test.go`:

```go
package config

import (
	"errors"
	"testing"
	"time"
)

func TestHTTPDefaults(t *testing.T) {
	t.Parallel()

	c, err := New(HTTPDefaults())
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != ":8080" || c.Timeout != 5*time.Second || c.MaxRetries != 3 {
		t.Fatalf("defaults wrong: %+v", c)
	}
}

func TestHTTPOptionsApply(t *testing.T) {
	t.Parallel()

	c, err := New(HTTPDefaults(),
		WithAddr(":9000"),
		WithTimeout(20*time.Second),
		WithMaxRetries(0),
	)
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != ":9000" || c.Timeout != 20*time.Second || c.MaxRetries != 0 {
		t.Fatalf("got %+v", c)
	}
}

func TestHTTPValidation(t *testing.T) {
	t.Parallel()

	if _, err := New(HTTPDefaults(), WithAddr("")); !errors.Is(err, ErrEmptyAddr) {
		t.Errorf("WithAddr(\"\"): err = %v, want ErrEmptyAddr", err)
	}
	if _, err := New(HTTPDefaults(), WithTimeout(0)); !errors.Is(err, ErrBadTimeout) {
		t.Errorf("WithTimeout(0): err = %v, want ErrBadTimeout", err)
	}
	if _, err := New(HTTPDefaults(), WithMaxRetries(-1)); !errors.Is(err, ErrNegativeRetries) {
		t.Errorf("WithMaxRetries(-1): err = %v, want ErrNegativeRetries", err)
	}
}

func TestCacheReusesSameGenericConstructor(t *testing.T) {
	t.Parallel()

	c, err := New(CacheDefaults(), WithMaxEntries(2048), WithTTL(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxEntries != 2048 || c.TTL != 2*time.Minute {
		t.Fatalf("got %+v", c)
	}
}

func TestCacheValidation(t *testing.T) {
	t.Parallel()

	if _, err := New(CacheDefaults(), WithMaxEntries(0)); !errors.Is(err, ErrBadCapacity) {
		t.Errorf("WithMaxEntries(0): err = %v, want ErrBadCapacity", err)
	}
	if _, err := New(CacheDefaults(), WithTTL(-time.Second)); !errors.Is(err, ErrBadTTL) {
		t.Errorf("WithTTL(-1s): err = %v, want ErrBadTTL", err)
	}
}

func TestNewStopsAtFirstError(t *testing.T) {
	t.Parallel()

	// The bad timeout fails before WithMaxRetries(5) runs, so retries stays at
	// the default 3 in the returned (partial) value.
	c, err := New(HTTPDefaults(), WithTimeout(-1), WithMaxRetries(5))
	if !errors.Is(err, ErrBadTimeout) {
		t.Fatalf("err = %v, want ErrBadTimeout", err)
	}
	if c.MaxRetries != 3 {
		t.Fatalf("MaxRetries = %d, want 3 (option after the failure must not run)", c.MaxRetries)
	}
}

func TestLaterOptionWins(t *testing.T) {
	t.Parallel()

	c, err := New(HTTPDefaults(), WithAddr(":1"), WithAddr(":2"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != ":2" {
		t.Fatalf("Addr = %q, want :2 (last option wins)", c.Addr)
	}
}
```

## Review

The generic constructor is correct when `T` is inferred from `defaults` and the loop applies each `Option[T]` to `&defaults` in order, short-circuiting on the first error — the same contract as the concrete form, now written once. The reuse claim is only real if both `HTTPConfig` and `CacheConfig` go through the identical `New` with no per-type constructor; `TestCacheReusesSameGenericConstructor` is the proof, and if you find yourself writing a second `New` you have lost the benefit generics bought. Watch the two trade-offs the form makes: defaults must be supplied by the caller (a generic constructor cannot hold per-type defaults), and the exported fields mean a built value is mutable afterward — acceptable for a config type, but the reason Exercise 1 kept its fields sealed. `TestNewStopsAtFirstError` confirms the short-circuit by checking that an option after the failing one never ran, and `TestLaterOptionWins` confirms ordering. All passing under `go test -race ./...` establishes the toolkit.

## Resources

- [Go: Tutorial on generics](https://go.dev/doc/tutorial/generics) — type parameters and inference, the mechanism `Option[T]` and `New[T]` rely on.
- [Go blog: An Introduction to Generics](https://go.dev/blog/intro-generics) — when parameterizing over a type pays off versus when it does not.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — the sentinel matcher the per-type validators preserve through the generic constructor.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-required-and-aggregated-options.md](02-required-and-aggregated-options.md) | Next: [04-production-api-client.md](04-production-api-client.md)
