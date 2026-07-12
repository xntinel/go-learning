# Exercise 2: Build a 12-Factor Env Config Loader with Defaults and Aggregated Validation

The real config path of a deployed service reads its settings from the
environment: required keys that must be present, optional keys with documented
defaults, and typed coercion from strings into ints, bools, and durations. This
exercise builds that loader — with the environment lookup injected as a function
so every branch is unit-testable without mutating global state — and aggregates
every problem with `errors.Join`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
envconfig/                   independent module: example.com/env-config-loader-defaults
  go.mod
  envconfig.go               Config, LoadFromEnv(lookup), typed coercion helpers, sentinels
  cmd/
    demo/
      main.go                loads from a map-backed lookup, prints the typed Config
  envconfig_test.go          full env, minimal env + defaults, each bad type, aggregated, zero not masked
```

- Files: `envconfig.go`, `cmd/demo/main.go`, `envconfig_test.go`.
- Implement: `LoadFromEnv(lookup func(string) (string, bool)) (Config, error)` that requires the mandatory keys, applies defaults for optional ones, coerces `int`/`bool`/`time.Duration`, and joins every failure.
- Test: fully-populated env yields the typed Config; minimal env fills defaults; missing required and unparseable int/bool/duration each surface their sentinel via `errors.Is`; a maximally-broken env joins all failures; and an explicit `MAX_RETRIES=0` is not masked by the non-zero default.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/07-structs-and-methods/06-constructor-functions-and-validation/02-env-config-loader-defaults/cmd/demo
cd go-solutions/07-structs-and-methods/06-constructor-functions-and-validation/02-env-config-loader-defaults
```

### Inject the lookup; distinguish unset from zero

The single most important design decision here is not calling `os.Getenv`
directly. `os.Getenv` returns `""` both when a variable is unset and when it is
set to the empty string, and it forces every test to mutate process-global
environment state, which fights `t.Parallel`. Instead the loader takes a
`lookup func(string) (string, bool)` — exactly the shape of `os.LookupEnv` — whose
second return distinguishes "present" from "absent". Production passes
`os.LookupEnv`; a test passes a closure over a `map[string]string`. Now every
branch — required-but-missing, present-but-malformed, absent-so-defaulted — is
reachable deterministically with no global state.

The two-return form is also what lets a default coexist with an explicit zero. If
the loader read `MAX_RETRIES` with a single-return getter, an operator who sets
`MAX_RETRIES=0` to disable retries would be silently overridden by the default of
`3`, because `0` is indistinguishable from unset. With the `ok` form the loader
asks "was the key present?" first: present means parse and honor the value even if
it is zero; absent means apply the default. The helper functions encode this: they
call `lookup`, return the default when `ok` is false, and otherwise parse.

Every coercion failure is a wrapped sentinel, and the loader accumulates them so a
badly-configured deploy reports every malformed variable in one pass rather than
one per restart.

Create `envconfig.go`:

```go
package envconfig

import (
	"errors"
	"fmt"
	"strconv"
	"time"
)

var (
	ErrMissingRequired = errors.New("required environment variable is missing")
	ErrInvalidInt      = errors.New("value must be an integer")
	ErrInvalidBool     = errors.New("value must be a boolean")
	ErrInvalidDuration = errors.New("value must be a duration")
)

// Config is a validated service configuration built from environment variables.
type Config struct {
	ServiceName    string        // required: SERVICE_NAME
	Port           int           // required: PORT
	LogLevel       string        // optional: LOG_LEVEL   (default "info")
	RequestTimeout time.Duration // optional: REQUEST_TIMEOUT (default 30s)
	MaxRetries     int           // optional: MAX_RETRIES (default 3)
	Debug          bool          // optional: DEBUG       (default false)
}

// LoadFromEnv reads configuration through the injected lookup (os.LookupEnv in
// production, a map-backed closure in tests) and returns either a valid Config
// or errors.Join of every problem.
func LoadFromEnv(lookup func(string) (string, bool)) (Config, error) {
	var errs []error
	var c Config

	c.ServiceName = required(lookup, "SERVICE_NAME", &errs)

	if v, ok := lookup("PORT"); !ok {
		errs = append(errs, fmt.Errorf("%w: PORT", ErrMissingRequired))
	} else if n, err := parseInt("PORT", v); err != nil {
		errs = append(errs, err)
	} else {
		c.Port = n
	}

	c.LogLevel = optionalString(lookup, "LOG_LEVEL", "info")
	c.RequestTimeout = optionalDuration(lookup, "REQUEST_TIMEOUT", 30*time.Second, &errs)
	c.MaxRetries = optionalInt(lookup, "MAX_RETRIES", 3, &errs)
	c.Debug = optionalBool(lookup, "DEBUG", false, &errs)

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return c, nil
}

func required(lookup func(string) (string, bool), key string, errs *[]error) string {
	v, ok := lookup(key)
	if !ok || v == "" {
		*errs = append(*errs, fmt.Errorf("%w: %s", ErrMissingRequired, key))
		return ""
	}
	return v
}

func optionalString(lookup func(string) (string, bool), key, def string) string {
	if v, ok := lookup(key); ok {
		return v
	}
	return def
}

func optionalInt(lookup func(string) (string, bool), key string, def int, errs *[]error) int {
	v, ok := lookup(key)
	if !ok {
		return def
	}
	n, err := parseInt(key, v)
	if err != nil {
		*errs = append(*errs, err)
		return def
	}
	return n
}

func optionalBool(lookup func(string) (string, bool), key string, def bool, errs *[]error) bool {
	v, ok := lookup(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%w: %s=%q", ErrInvalidBool, key, v))
		return def
	}
	return b
}

func optionalDuration(lookup func(string) (string, bool), key string, def time.Duration, errs *[]error) time.Duration {
	v, ok := lookup(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%w: %s=%q", ErrInvalidDuration, key, v))
		return def
	}
	return d
}

func parseInt(key, v string) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%w: %s=%q", ErrInvalidInt, key, v)
	}
	return n, nil
}
```

### The runnable demo

The demo builds a map-backed lookup — the same shape the tests use — populates a
minimal environment, and prints the typed Config so you can see the defaults fill
in for the keys that were omitted.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/env-config-loader-defaults"
)

func main() {
	env := map[string]string{
		"SERVICE_NAME": "checkout",
		"PORT":         "8443",
	}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }

	c, err := envconfig.LoadFromEnv(lookup)
	if err != nil {
		fmt.Println("config error:", err)
		return
	}
	fmt.Printf("service=%s port=%d log=%s timeout=%s retries=%d debug=%t\n",
		c.ServiceName, c.Port, c.LogLevel, c.RequestTimeout, c.MaxRetries, c.Debug)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
service=checkout port=8443 log=info timeout=30s retries=3 debug=false
```

### Tests

`mapLookup` builds the injected function from a map, so each test is a pure
input-to-output assertion. `TestExplicitZeroNotMasked` is the subtle one: it sets
`MAX_RETRIES=0` and proves the loader honors the explicit zero instead of
substituting the default of 3, which only works because the loader uses the `ok`
form.

Create `envconfig_test.go`:

```go
package envconfig

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func mapLookup(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestLoadFullEnv(t *testing.T) {
	t.Parallel()
	c, err := LoadFromEnv(mapLookup(map[string]string{
		"SERVICE_NAME":    "checkout",
		"PORT":            "8443",
		"LOG_LEVEL":       "debug",
		"REQUEST_TIMEOUT": "5s",
		"MAX_RETRIES":     "7",
		"DEBUG":           "true",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		ServiceName:    "checkout",
		Port:           8443,
		LogLevel:       "debug",
		RequestTimeout: 5 * time.Second,
		MaxRetries:     7,
		Debug:          true,
	}
	if c != want {
		t.Fatalf("Config = %+v, want %+v", c, want)
	}
}

func TestLoadMinimalEnvFillsDefaults(t *testing.T) {
	t.Parallel()
	c, err := LoadFromEnv(mapLookup(map[string]string{
		"SERVICE_NAME": "checkout",
		"PORT":         "8443",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.LogLevel != "info" || c.RequestTimeout != 30*time.Second || c.MaxRetries != 3 || c.Debug {
		t.Fatalf("defaults not applied: %+v", c)
	}
}

func TestMissingRequired(t *testing.T) {
	t.Parallel()
	_, err := LoadFromEnv(mapLookup(map[string]string{"PORT": "8443"}))
	if !errors.Is(err, ErrMissingRequired) {
		t.Fatalf("err = %v, want ErrMissingRequired", err)
	}
}

func TestBadTypes(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		env  map[string]string
		want error
	}{
		"bad port": {map[string]string{"SERVICE_NAME": "s", "PORT": "notint"}, ErrInvalidInt},
		"bad retries": {map[string]string{
			"SERVICE_NAME": "s", "PORT": "1", "MAX_RETRIES": "x"}, ErrInvalidInt},
		"bad timeout": {map[string]string{
			"SERVICE_NAME": "s", "PORT": "1", "REQUEST_TIMEOUT": "10"}, ErrInvalidDuration},
		"bad debug": {map[string]string{
			"SERVICE_NAME": "s", "PORT": "1", "DEBUG": "maybe"}, ErrInvalidBool},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := LoadFromEnv(mapLookup(tc.env))
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestMaximallyBrokenJoinsAll(t *testing.T) {
	t.Parallel()
	_, err := LoadFromEnv(mapLookup(map[string]string{
		"PORT":            "notint",
		"REQUEST_TIMEOUT": "nope",
		"DEBUG":           "maybe",
	}))
	for _, want := range []error{ErrMissingRequired, ErrInvalidInt, ErrInvalidDuration, ErrInvalidBool} {
		if !errors.Is(err, want) {
			t.Fatalf("joined err %v should include %v", err, want)
		}
	}
}

func TestExplicitZeroNotMasked(t *testing.T) {
	t.Parallel()
	c, err := LoadFromEnv(mapLookup(map[string]string{
		"SERVICE_NAME": "s",
		"PORT":         "1",
		"MAX_RETRIES":  "0",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxRetries != 0 {
		t.Fatalf("MaxRetries = %d, default masked the explicit 0", c.MaxRetries)
	}
}

func ExampleLoadFromEnv() {
	env := map[string]string{"SERVICE_NAME": "api", "PORT": "80"}
	c, _ := LoadFromEnv(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	fmt.Printf("%s:%d retries=%d\n", c.ServiceName, c.Port, c.MaxRetries)
	// Output: api:80 retries=3
}
```

## Review

The loader is correct when a full environment produces the exact typed Config, a
minimal one fills every documented default, and a broken one reports every problem
through `errors.Join`. The trap this exercise targets is a default masking an
explicit zero: with a single-return getter, `MAX_RETRIES=0` is indistinguishable
from unset and the default silently wins, a bug that only appears when an operator
deliberately sets a zero. The two-return `ok` form is what makes "was it present?"
answerable. The second trap is calling globals: injecting `lookup` keeps every
branch testable in parallel with no environment mutation.

## Resources

- [os.LookupEnv](https://pkg.go.dev/os#LookupEnv) — the two-return presence check the loader's `lookup` mirrors.
- [strconv package](https://pkg.go.dev/strconv) — `Atoi` and `ParseBool`.
- [time.ParseDuration](https://pkg.go.dev/time#ParseDuration) — parsing `5s`, `100ms`, `1h30m`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-config-load-errors-join.md](01-config-load-errors-join.md) | Next: [03-functional-options-http-client.md](03-functional-options-http-client.md)
