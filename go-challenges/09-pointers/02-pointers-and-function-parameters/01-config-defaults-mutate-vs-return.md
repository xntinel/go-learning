# Exercise 1: Config Loader — Mutate via Pointer Parameter vs Return a New Value

Every backend starts by loading configuration and filling in defaults. This
exercise builds that config package three ways so the difference is unmissable:
`ApplyDefaults(*Config)` mutates the caller in place, `WithDefaults(Config) Config`
returns a fresh value, and `WithHostname(Config, string) Config` transforms without
touching the caller. The through-line is that the signature *is* the contract about
who sees the change.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
config/                     independent module: example.com/config
  go.mod                    go directive supplied by the module
  config.go                 Config; Load; ApplyDefaults(*Config); WithDefaults; WithHostname; Validate
  cmd/
    demo/
      main.go               shows mutate-in-place vs return-a-new-value side by side
  config_test.go            mutation contract, equivalence contract, sentinel errors, env load
```

- Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
- Implement: a `Config`, an env `Load`, a pointer-parameter `ApplyDefaults`, a value-in/value-out `WithDefaults` and `WithHostname`, and a `Validate` returning sentinel errors.
- Test: prove `ApplyDefaults` mutates the caller, the value forms do not, the pointer and value forms produce `==` results, and `Validate` returns wrapped sentinels asserted with `errors.Is`.
- Verify: `go test -count=1 -race ./...`

### Why one package needs all three forms

`ApplyDefaults(c *Config)` copies the *pointer*, so writes through it land on the
caller's `Config`; after the call the caller's struct has the defaults filled in,
with no new value returned and no allocation. That is the right tool when the caller
explicitly wants its own value completed in place — the common shape of "finish
initializing this thing I already own."

`WithDefaults(c Config) Config` copies the whole `Config` into the parameter, fills
defaults on the copy, and returns it. The caller's original is provably untouched
(it was never shared), and the caller opts in by assigning the result. Prefer this
when the caller might *not* want the mutation, or when immutability makes the code
easier to reason about — the input is a pure function of the output.

`WithHostname(c Config, host string) Config` is the same value-in/value-out shape
specialized to one field: it returns a copy with `Host` replaced and never aliases
the caller. The pinned property that ties the two default forms together is
equivalence: running `ApplyDefaults(&a)` and `b = WithDefaults(b)` on equal starting
configs must yield `a == b`. If they ever diverge, one of them has a bug — the
contract is that pointer-mutation and value-return are two encodings of the *same*
transformation, differing only in who owns the result.

Create `config.go`:

```go
package config

import (
	"errors"
	"os"
	"strconv"
)

// Sentinel errors, wrapped by callers and asserted with errors.Is.
var (
	ErrMissingHost = errors.New("missing host")
	ErrInvalidPort = errors.New("invalid port")
)

const (
	DefaultHost = "localhost"
	DefaultPort = 8080
)

// Config is a server's runtime configuration.
type Config struct {
	Host string
	Port int
	TLS  bool
}

// Load reads configuration from the environment. APP_PORT must be a positive
// integer; APP_HOST and APP_TLS are optional. A bad port is reported as a
// wrapped ErrInvalidPort so callers can match it with errors.Is.
func Load() (Config, error) {
	host := os.Getenv("APP_HOST")
	tls := os.Getenv("APP_TLS") == "true"

	port, err := strconv.Atoi(os.Getenv("APP_PORT"))
	if err != nil || port <= 0 {
		return Config{}, ErrInvalidPort
	}
	return Config{Host: host, Port: port, TLS: tls}, nil
}

// ApplyDefaults fills empty fields in place through the pointer parameter. The
// caller observes the change: after this returns, c.Host and c.Port are set.
func ApplyDefaults(c *Config) {
	if c.Host == "" {
		c.Host = DefaultHost
	}
	if c.Port == 0 {
		c.Port = DefaultPort
	}
}

// WithDefaults returns a NEW Config with defaults applied. The argument is a
// copy; the caller's original is untouched.
func WithDefaults(c Config) Config {
	if c.Host == "" {
		c.Host = DefaultHost
	}
	if c.Port == 0 {
		c.Port = DefaultPort
	}
	return c
}

// WithHostname returns a copy of c with Host replaced. It does not alias or
// mutate the caller's Config.
func WithHostname(c Config, host string) Config {
	c.Host = host
	return c
}

// Validate reports the first structural problem with c, or nil if it is usable.
func (c *Config) Validate() error {
	if c.Host == "" {
		return ErrMissingHost
	}
	if c.Port <= 0 {
		return ErrInvalidPort
	}
	return nil
}
```

### The runnable demo

The demo constructs a zero `Config`, applies defaults by pointer (watch the same
variable change), then shows the value form leaving its input alone.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/config"
)

func main() {
	// Pointer parameter: the caller's value is mutated in place.
	c := config.Config{}
	config.ApplyDefaults(&c)
	fmt.Printf("after ApplyDefaults(&c): %+v\n", c)

	// Value in / value out: the input is untouched; the caller takes the result.
	base := config.Config{}
	filled := config.WithDefaults(base)
	fmt.Printf("base stays zero:          %+v\n", base)
	fmt.Printf("WithDefaults returns new: %+v\n", filled)

	// Transform without mutation.
	moved := config.WithHostname(filled, "db.internal")
	fmt.Printf("WithHostname copy:        %+v\n", moved)
	fmt.Printf("original still localhost: %+v\n", filled)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
after ApplyDefaults(&c): {Host:localhost Port:8080 TLS:false}
base stays zero:          {Host: Port:0 TLS:false}
WithDefaults returns new: {Host:localhost Port:8080 TLS:false}
WithHostname copy:        {Host:db.internal Port:8080 TLS:false}
original still localhost: {Host:localhost Port:8080 TLS:false}
```

### Tests

The tests pin the contract. `TestApplyDefaultsMutatesReceiver` proves the pointer
form changes the caller; `TestWithDefaultsDoesNotMutateCaller` and
`TestWithHostnameDoesNotAlias` prove the value forms do not. `TestValidate` asserts
the sentinels with `errors.Is`. `TestApplyDefaultsSkipsNonZeroFields` proves
defaults never clobber real values. `TestPointerAndValueFormsAgree` is the pinned
equivalence contract. `TestLoadFromEnv` uses `t.Setenv` (so it is not parallel).

Create `config_test.go`:

```go
package config

import (
	"errors"
	"fmt"
	"testing"
)

func TestApplyDefaultsMutatesReceiver(t *testing.T) {
	t.Parallel()
	c := &Config{}
	ApplyDefaults(c)
	if c.Host != DefaultHost || c.Port != DefaultPort {
		t.Fatalf("caller not mutated: got %+v", *c)
	}
}

func TestWithDefaultsDoesNotMutateCaller(t *testing.T) {
	t.Parallel()
	c := Config{}
	got := WithDefaults(c)
	if got.Host != DefaultHost || got.Port != DefaultPort {
		t.Fatalf("result missing defaults: got %+v", got)
	}
	if c.Host != "" || c.Port != 0 {
		t.Fatalf("caller was mutated: got %+v", c)
	}
}

func TestWithHostnameDoesNotAlias(t *testing.T) {
	t.Parallel()
	c := Config{Host: "old", Port: 9090}
	got := WithHostname(c, "new")
	if got.Host != "new" {
		t.Fatalf("Host = %q, want new", got.Host)
	}
	if c.Host != "old" {
		t.Fatalf("caller mutated: Host = %q, want old", c.Host)
	}
}

func TestApplyDefaultsSkipsNonZeroFields(t *testing.T) {
	t.Parallel()
	c := &Config{Host: "db", Port: 5432}
	ApplyDefaults(c)
	if c.Host != "db" || c.Port != 5432 {
		t.Fatalf("defaults clobbered real values: got %+v", *c)
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		c    Config
		want error
	}{
		"empty":    {Config{}, ErrMissingHost},
		"bad port": {Config{Host: "h"}, ErrInvalidPort},
		"valid":    {Config{Host: "h", Port: 80}, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := tc.c.Validate()
			if !errors.Is(got, tc.want) {
				t.Fatalf("Validate() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPointerAndValueFormsAgree pins the equivalence contract: mutating in place
// and returning a new value are two encodings of the same transformation.
func TestPointerAndValueFormsAgree(t *testing.T) {
	t.Parallel()
	starts := []Config{
		{},
		{Host: "h"},
		{Port: 443},
		{Host: "h", Port: 443, TLS: true},
	}
	for _, start := range starts {
		byPtr := start
		ApplyDefaults(&byPtr)
		byVal := WithDefaults(start)
		if byPtr != byVal {
			t.Fatalf("forms diverged for %+v: ptr=%+v val=%+v", start, byPtr, byVal)
		}
	}
}

func TestLoadFromEnv(t *testing.T) {
	// t.Setenv forbids t.Parallel.
	t.Setenv("APP_HOST", "api.internal")
	t.Setenv("APP_PORT", "9443")
	t.Setenv("APP_TLS", "true")
	got, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	want := Config{Host: "api.internal", Port: 9443, TLS: true}
	if got != want {
		t.Fatalf("Load() = %+v, want %+v", got, want)
	}

	t.Setenv("APP_PORT", "not-a-number")
	if _, err := Load(); !errors.Is(err, ErrInvalidPort) {
		t.Fatalf("Load() bad port err = %v, want ErrInvalidPort", err)
	}
}

func ExampleWithDefaults() {
	c := WithDefaults(Config{})
	fmt.Printf("%s:%d\n", c.Host, c.Port)
	// Output: localhost:8080
}
```

## Review

The package is correct when each function honors its signature's promise. The
pointer form and the value forms are not two styles of the same thing that you pick
by taste; they encode different ownership. `ApplyDefaults` earns its `*Config`
because the caller wants its own value completed — the mutation is the feature, not
an optimization. `WithDefaults`/`WithHostname` earn their value-in/value-out shape
because the caller wants a transformation without a side effect. The equivalence
test is the guard rail: it fails the moment one form drifts from the other. Keep the
env-loading test non-parallel because `t.Setenv` and `t.Parallel` are mutually
exclusive. Run `go test -race` to confirm nothing shares state it should not.

## Resources

- [Go Spec: Pointer types](https://go.dev/ref/spec#Pointer_types) — the language rule that a parameter is always a copy and a pointer copies the address.
- [Effective Go: Pointers vs. Values](https://go.dev/doc/effective_go#pointers_vs_values) — when to choose each for receivers and parameters.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — matching wrapped sentinel errors, as `TestValidate` does.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-patch-semantics-optional-pointer-fields.md](02-patch-semantics-optional-pointer-fields.md)
