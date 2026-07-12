# Exercise 8: Layered Config Loader: Defaults, Environment, Then Options

Real services layer their configuration: hard-coded defaults are the floor,
environment variables override them in a container, and explicit code overrides
everything for tests and special cases. This module builds a loader with exactly
that precedence — defaults < environment < functional options — by seeding
defaults, reading the environment, and applying options *last* so explicit code
always wins.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
appconfig/                       independent module: example.com/appconfig
  go.mod                         go 1.26
  config.go                      Config, Option, NewConfig, WithPort, WithLogLevel,
                                 WithFeatureFlag, WithReadTimeout
  cmd/
    demo/
      main.go                    sets env, overrides one field via option, prints precedence
  config_test.go                 t.Setenv proves env beats default and option beats env
```

- Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
- Implement: `NewConfig(opts...) (Config, error)` that seeds defaults, reads typed values from the environment, then applies validating options last, and finally validates the whole result.
- Test: `t.Setenv` PORT/LOG_LEVEL; with no options assert env won over defaults; with `WithPort` assert the option beat the env; assert malformed env is a wrapped error and an out-of-range `WithPort` is rejected.
- Verify: `go test -count=1 ./...`

### Ordering is the whole design

The precedence is enforced by the *order of operations* inside `NewConfig`, not by
any cleverness in the options:

1. Seed defaults into the `Config`. This is the floor — a field nobody sets still
   has a sane value.
2. Read the environment. Each recognized variable, if present, overwrites the
   default. Parsing is typed: `strconv.Atoi` for the port, `strconv.ParseBool` for
   a flag, `time.ParseDuration` for a timeout, and a malformed value is a
   construction error, not a silent fallback.
3. Apply the options. Because they run *after* the environment is read, an
   explicit `WithPort(3000)` overwrites whatever `PORT` was — options are the top
   of the stack.
4. Validate the final result. The port must be in range whether it came from the
   environment or an option, so the check lives once, at the end, after every
   layer has contributed.

Put the option loop before the environment read and you invert the precedence:
env would clobber explicit code, which is exactly backwards. The ordering *is* the
contract, and it is worth a comment in the code and a test that pins it.

Create `config.go`:

```go
package appconfig

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config is a service configuration assembled from defaults, environment, and
// explicit options in increasing precedence.
type Config struct {
	Port        int
	LogLevel    string
	FeatureFlag bool
	ReadTimeout time.Duration
}

// Option overrides a Config field and may reject invalid input.
type Option func(*Config) error

func validLogLevel(level string) bool {
	switch level {
	case "debug", "info", "warn", "error":
		return true
	default:
		return false
	}
}

// NewConfig loads configuration with precedence defaults < environment < options.
func NewConfig(opts ...Option) (Config, error) {
	c := Config{
		Port:        8080,
		LogLevel:    "info",
		FeatureFlag: false,
		ReadTimeout: 5 * time.Second,
	}

	// Environment overrides defaults.
	if v, ok := os.LookupEnv("PORT"); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("env PORT %q: %w", v, err)
		}
		c.Port = n
	}
	if v, ok := os.LookupEnv("LOG_LEVEL"); ok {
		c.LogLevel = v
	}
	if v, ok := os.LookupEnv("FEATURE_FLAG"); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("env FEATURE_FLAG %q: %w", v, err)
		}
		c.FeatureFlag = b
	}
	if v, ok := os.LookupEnv("READ_TIMEOUT"); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, fmt.Errorf("env READ_TIMEOUT %q: %w", v, err)
		}
		c.ReadTimeout = d
	}

	// Options override the environment.
	for _, opt := range opts {
		if err := opt(&c); err != nil {
			return Config{}, err
		}
	}

	// Validate the fully assembled result.
	if c.Port < 1 || c.Port > 65535 {
		return Config{}, fmt.Errorf("port %d out of range 1-65535", c.Port)
	}
	if !validLogLevel(c.LogLevel) {
		return Config{}, fmt.Errorf("invalid log level %q", c.LogLevel)
	}
	if c.ReadTimeout <= 0 {
		return Config{}, fmt.Errorf("read timeout must be positive, got %s", c.ReadTimeout)
	}
	return c, nil
}

// WithPort overrides the port (1-65535).
func WithPort(port int) Option {
	return func(c *Config) error {
		if port < 1 || port > 65535 {
			return fmt.Errorf("port %d out of range 1-65535", port)
		}
		c.Port = port
		return nil
	}
}

// WithLogLevel overrides the log level.
func WithLogLevel(level string) Option {
	return func(c *Config) error {
		if !validLogLevel(level) {
			return fmt.Errorf("invalid log level %q", level)
		}
		c.LogLevel = level
		return nil
	}
}

// WithFeatureFlag overrides the feature flag.
func WithFeatureFlag(on bool) Option {
	return func(c *Config) error {
		c.FeatureFlag = on
		return nil
	}
}

// WithReadTimeout overrides the read timeout (> 0).
func WithReadTimeout(d time.Duration) Option {
	return func(c *Config) error {
		if d <= 0 {
			return fmt.Errorf("read timeout must be positive, got %s", d)
		}
		c.ReadTimeout = d
		return nil
	}
}
```

### The runnable demo

The demo sets `PORT` and `LOG_LEVEL` in the environment, then overrides only the
port with an option, so the output shows all three layers at once: the port comes
from the option (beating the env), the log level from the env (beating the
default), and the timeout from the default.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"

	"example.com/appconfig"
)

func main() {
	os.Setenv("PORT", "9090")
	os.Setenv("LOG_LEVEL", "warn")

	cfg, err := appconfig.NewConfig(appconfig.WithPort(3000))
	if err != nil {
		panic(err)
	}

	fmt.Printf("port: %d (option overrode env)\n", cfg.Port)
	fmt.Printf("log level: %s (env overrode default)\n", cfg.LogLevel)
	fmt.Printf("feature flag: %t (default)\n", cfg.FeatureFlag)
	fmt.Printf("read timeout: %s (default)\n", cfg.ReadTimeout)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
port: 3000 (option overrode env)
log level: warn (env overrode default)
feature flag: false (default)
read timeout: 5s (default)
```

### Tests

`TestEnvBeatsDefault` sets `PORT`/`LOG_LEVEL` and constructs with no options,
asserting the env values won. `TestOptionBeatsEnv` sets `PORT` and passes
`WithPort`, asserting the option won. `TestMalformedEnvError` sets a non-numeric
`PORT` and asserts a wrapped `strconv` error. `TestOptionOutOfRangeRejected`
proves the option's own range check. These use `t.Setenv`, so they must not call
`t.Parallel`.

Create `config_test.go`:

```go
package appconfig

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"testing"
)

func TestEnvBeatsDefault(t *testing.T) {
	t.Setenv("PORT", "9090")
	t.Setenv("LOG_LEVEL", "debug")

	cfg, err := NewConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 9090 {
		t.Errorf("Port = %d, want 9090 (env)", cfg.Port)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug (env)", cfg.LogLevel)
	}
}

func TestOptionBeatsEnv(t *testing.T) {
	t.Setenv("PORT", "9090")

	cfg, err := NewConfig(WithPort(3000))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 3000 {
		t.Errorf("Port = %d, want 3000 (option beats env)", cfg.Port)
	}
}

func TestMalformedEnvError(t *testing.T) {
	t.Setenv("PORT", "not-a-number")

	_, err := NewConfig()
	if err == nil {
		t.Fatal("expected error for malformed PORT, got nil")
	}
	if !errors.Is(err, strconv.ErrSyntax) {
		t.Fatalf("error = %v, want wrapped strconv.ErrSyntax", err)
	}
}

func TestOptionOutOfRangeRejected(t *testing.T) {
	_, err := NewConfig(WithPort(70000))
	if err == nil {
		t.Fatal("expected error for out-of-range port, got nil")
	}
}

func ExampleNewConfig() {
	os.Unsetenv("PORT")
	os.Unsetenv("LOG_LEVEL")

	cfg, _ := NewConfig(WithPort(443))
	// Port from the option; log level falls back to the default.
	fmt.Printf("%d %s\n", cfg.Port, cfg.LogLevel)
	// Output: 443 info
}
```

## Review

The loader is correct when precedence follows the order of operations exactly:
defaults seeded first, environment read second, options applied third, validation
last. `TestEnvBeatsDefault` and `TestOptionBeatsEnv` together pin the full stack,
and swapping the option loop above the environment read would flip
`TestOptionBeatsEnv` to failure. Parsing the environment with typed conversions
and returning a wrapped error — rather than silently falling back — is what lets
`TestMalformedEnvError` assert `errors.Is(err, strconv.ErrSyntax)`. Because these
tests mutate process environment through `t.Setenv`, none may run in parallel; the
Go runtime enforces that with a panic if you try.

## Resources

- [os.LookupEnv](https://pkg.go.dev/os#LookupEnv)
- [strconv.Atoi and ParseBool](https://pkg.go.dev/strconv)
- [time.ParseDuration](https://pkg.go.dev/time#ParseDuration)
- [testing.T.Setenv](https://pkg.go.dev/testing#T.Setenv)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-generic-options-errors-join.md](07-generic-options-errors-join.md) | Next: [09-preset-and-ordering-options.md](09-preset-and-ordering-options.md)
