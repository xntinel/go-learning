# 2. Layered Configuration

Most services must accept configuration from several sources simultaneously: a
developer runs locally with a config file, ops deploys with environment variables,
and an operator overrides a single value with a CLI flag. The hard part is not
reading each source — it is merging them with a deterministic precedence order and
stopping early with all validation errors visible at once.

This lesson builds a `config` package that applies four layers in sequence (lowest
to highest priority): compiled defaults, JSON file, environment variables, CLI
flags. Reflection over struct tags drives all four layers generically, so adding a
new field never requires touching the loader code.

```text
config/
  go.mod
  config.go
  config_test.go
  cmd/demo/main.go
```

## Concepts

### The Precedence Stack

A layered loader applies sources in order from lowest to highest priority. Each
source overwrites only the fields it explicitly provides; it does not blank out
fields set by a lower layer. The standard production order is:

```
defaults < file < env < flags
```

Defaults ship with the binary (safe, always present). A config file customizes the
service for an environment. Environment variables are injected by the container
orchestrator without redeploying the binary. CLI flags allow per-invocation
overrides (useful in tests and one-off debugging runs).

The key invariant: a higher-priority source must win over a lower one only when it
is explicitly present. An environment variable that is not set must not blank out a
value that came from the config file. This is why `os.LookupEnv` (returns a
`bool`) is used instead of `os.Getenv` (returns an empty string for both "not set"
and "set to empty").

The same rule applies to CLI flags: `flag.Visit` iterates only the flags that were
explicitly provided on the command line, so a default registered with
`flag.FlagSet` does not silently overwrite earlier layers.

### Reflection Over Struct Tags

Each field in `Config` carries four struct tags:

```
json:"port"    -- key in the JSON config file
env:"APP_PORT" -- name of the environment variable
flag:"port"    -- name of the CLI flag
default:"8080" -- raw string for the compiled default
```

A single `setField(reflect.Value, string)` function converts a raw string to the
field's kind (`string`, `int`, `bool`, `time.Duration`). All four layer functions
call it. The only per-layer difference is where the raw string comes from:

- `applyDefaults` reads the `default` tag.
- `applyJSON` unmarshals a file into the struct directly (the `json` tag handles
  it).
- `applyEnv` calls `os.LookupEnv` per the `env` tag.
- `applyFlags` calls `fs.Visit` per the `flag` tag.

This pattern means you can add a `MaxConns int` field, give it four tags, and the
loader picks it up automatically — zero changes to the layer functions.

### Validation: Report All Errors at Once

A config struct that validation walks field by field, stopping at the first error,
is frustrating: the operator fixes it, reruns, and gets the next error. Collect all
errors into a slice and return them in a single message. Wrap each in a sentinel so
callers can test with `errors.Is` instead of string matching.

### Time.Duration Fields

`time.Duration` is `int64` under the hood, so `reflect.Value.Kind()` returns
`reflect.Int64`. The `setField` helper must check the concrete type before calling
`time.ParseDuration`. A plain `reflect.Int64` branch that calls `strconv.ParseInt`
would misparse `"5s"` silently.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/30-production-patterns/02-configuration-layered/02-configuration-layered/cmd/demo
cd go-solutions/30-production-patterns/02-configuration-layered/02-configuration-layered
```

This is a library. Verification is `go test`, not `go run`.

### Exercise 1: The Config Struct and Defaults

Create `config.go`:

```go
package config

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// Config holds the service configuration. Each field carries four struct tags
// that drive the four loader layers. Adding a field here is sufficient; no
// loader code changes are needed.
type Config struct {
	Host         string        `json:"host"          env:"APP_HOST"          flag:"host"          default:"localhost"`
	Port         int           `json:"port"          env:"APP_PORT"          flag:"port"          default:"8080"`
	DatabaseURL  string        `json:"database_url"  env:"APP_DATABASE_URL"  flag:"database-url"  default:""`
	ReadTimeout  time.Duration `json:"read_timeout"  env:"APP_READ_TIMEOUT"  flag:"read-timeout"  default:"5s"`
	WriteTimeout time.Duration `json:"write_timeout" env:"APP_WRITE_TIMEOUT" flag:"write-timeout" default:"10s"`
	LogLevel     string        `json:"log_level"     env:"APP_LOG_LEVEL"     flag:"log-level"     default:"info"`
	MaxConns     int           `json:"max_conns"     env:"APP_MAX_CONNS"     flag:"max-conns"     default:"25"`
}

// Sentinel errors returned by Validate. Callers test with errors.Is.
var (
	ErrInvalidPort     = errors.New("port must be 1-65535")
	ErrMissingDB       = errors.New("database_url is required")
	ErrBadReadTimeout  = errors.New("read_timeout must be positive")
	ErrBadWriteTimeout = errors.New("write_timeout must be positive")
	ErrBadLogLevel     = errors.New("log_level must be debug|info|warn|error")
	ErrBadMaxConns     = errors.New("max_conns must be >= 1")
)

// Load applies four layers in order: defaults, JSON file, env, flags.
// args is typically os.Args[1:]; pass a custom slice in tests.
// configPath may be "" to skip the file layer.
func Load(configPath string, args []string) (*Config, error) {
	cfg := &Config{}
	if err := applyDefaults(cfg); err != nil {
		return nil, fmt.Errorf("config defaults: %w", err)
	}
	if configPath != "" {
		if err := applyJSON(cfg, configPath); err != nil {
			return nil, fmt.Errorf("config file: %w", err)
		}
	}
	applyEnv(cfg)
	if err := applyFlags(cfg, args); err != nil {
		return nil, fmt.Errorf("config flags: %w", err)
	}
	return cfg, nil
}

// Validate returns a non-nil error listing all invalid fields.
// The returned error wraps each sentinel so callers can use errors.Is.
func (c *Config) Validate() error {
	var errs []error
	if c.Port < 1 || c.Port > 65535 {
		errs = append(errs, fmt.Errorf("%w: got %d", ErrInvalidPort, c.Port))
	}
	if c.DatabaseURL == "" {
		errs = append(errs, ErrMissingDB)
	}
	if c.ReadTimeout <= 0 {
		errs = append(errs, fmt.Errorf("%w: got %s", ErrBadReadTimeout, c.ReadTimeout))
	}
	if c.WriteTimeout <= 0 {
		errs = append(errs, fmt.Errorf("%w: got %s", ErrBadWriteTimeout, c.WriteTimeout))
	}
	valid := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	if !valid[c.LogLevel] {
		errs = append(errs, fmt.Errorf("%w: got %q", ErrBadLogLevel, c.LogLevel))
	}
	if c.MaxConns < 1 {
		errs = append(errs, fmt.Errorf("%w: got %d", ErrBadMaxConns, c.MaxConns))
	}
	if len(errs) > 0 {
		msgs := make([]string, len(errs))
		for i, e := range errs {
			msgs[i] = e.Error()
		}
		return fmt.Errorf("config validation:\n  - %s", strings.Join(msgs, "\n  - "))
	}
	return nil
}

// ValidateErrors returns the individual sentinel errors that Validate found.
// This lets tests call errors.Is on each sentinel independently.
func (c *Config) ValidateErrors() []error {
	var errs []error
	if c.Port < 1 || c.Port > 65535 {
		errs = append(errs, fmt.Errorf("%w: got %d", ErrInvalidPort, c.Port))
	}
	if c.DatabaseURL == "" {
		errs = append(errs, ErrMissingDB)
	}
	if c.ReadTimeout <= 0 {
		errs = append(errs, fmt.Errorf("%w: got %s", ErrBadReadTimeout, c.ReadTimeout))
	}
	if c.WriteTimeout <= 0 {
		errs = append(errs, fmt.Errorf("%w: got %s", ErrBadWriteTimeout, c.WriteTimeout))
	}
	valid := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	if !valid[c.LogLevel] {
		errs = append(errs, fmt.Errorf("%w: got %q", ErrBadLogLevel, c.LogLevel))
	}
	if c.MaxConns < 1 {
		errs = append(errs, fmt.Errorf("%w: got %d", ErrBadMaxConns, c.MaxConns))
	}
	return errs
}

// applyDefaults reads the "default" struct tag and sets each field.
func applyDefaults(cfg *Config) error {
	v := reflect.ValueOf(cfg).Elem()
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		def := t.Field(i).Tag.Get("default")
		if def == "" {
			continue
		}
		if err := setField(v.Field(i), def); err != nil {
			return fmt.Errorf("field %s: %w", t.Field(i).Name, err)
		}
	}
	return nil
}

// applyJSON unmarshals a JSON file on top of cfg (json struct tags drive
// field mapping). Only fields present in the JSON override earlier layers.
func applyJSON(cfg *Config, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, cfg)
}

// applyEnv reads each field's "env" tag and calls os.LookupEnv. Fields whose
// environment variable is not set are left unchanged.
func applyEnv(cfg *Config) {
	v := reflect.ValueOf(cfg).Elem()
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		key := t.Field(i).Tag.Get("env")
		if key == "" {
			continue
		}
		val, ok := os.LookupEnv(key)
		if !ok {
			continue
		}
		_ = setField(v.Field(i), val)
	}
}

// applyFlags registers one flag per "flag" struct tag and calls fs.Visit so
// only explicitly provided flags override earlier layers.
func applyFlags(cfg *Config, args []string) error {
	v := reflect.ValueOf(cfg).Elem()
	t := v.Type()

	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	for i := 0; i < t.NumField(); i++ {
		name := t.Field(i).Tag.Get("flag")
		if name == "" {
			continue
		}
		cur := fmt.Sprintf("%v", v.Field(i).Interface())
		fs.String(name, cur, t.Field(i).Name)
	}

	if err := fs.Parse(args); err != nil {
		return err
	}

	fs.Visit(func(f *flag.Flag) {
		for i := 0; i < t.NumField(); i++ {
			if t.Field(i).Tag.Get("flag") == f.Name {
				_ = setField(v.Field(i), f.Value.String())
			}
		}
	})
	return nil
}

// setField parses val (a raw string) into the kind of field.
func setField(field reflect.Value, val string) error {
	switch field.Kind() {
	case reflect.String:
		field.SetString(val)
	case reflect.Int:
		n, err := strconv.Atoi(val)
		if err != nil {
			return err
		}
		field.SetInt(int64(n))
	case reflect.Bool:
		b, err := strconv.ParseBool(val)
		if err != nil {
			return err
		}
		field.SetBool(b)
	case reflect.Int64:
		// time.Duration is int64; check the concrete type first.
		if field.Type() == reflect.TypeOf(time.Duration(0)) {
			d, err := time.ParseDuration(val)
			if err != nil {
				return err
			}
			field.SetInt(int64(d))
			return nil
		}
		n, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			return err
		}
		field.SetInt(n)
	default:
		return fmt.Errorf("unsupported kind: %s", field.Kind())
	}
	return nil
}
```

### Exercise 2: Test the Loader Contract

Create `config_test.go`:

```go
package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeJSON writes cfg as JSON to a temp file and returns the path.
func writeJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	f, err := os.CreateTemp(t.TempDir(), "*.json")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatalf("Write: %v", err)
	}
	f.Close()
	return f.Name()
}

func TestLoadAppliesDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := Load("", nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Host != "localhost" {
		t.Errorf("Host = %q, want localhost", cfg.Host)
	}
	if cfg.Port != 8080 {
		t.Errorf("Port = %d, want 8080", cfg.Port)
	}
	if cfg.ReadTimeout != 5*time.Second {
		t.Errorf("ReadTimeout = %v, want 5s", cfg.ReadTimeout)
	}
	if cfg.MaxConns != 25 {
		t.Errorf("MaxConns = %d, want 25", cfg.MaxConns)
	}
}

func TestLoadFileOverridesDefaults(t *testing.T) {
	t.Parallel()

	path := writeJSON(t, map[string]any{
		"host": "0.0.0.0",
		"port": 3000,
	})
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Host != "0.0.0.0" {
		t.Errorf("Host = %q, want 0.0.0.0", cfg.Host)
	}
	if cfg.Port != 3000 {
		t.Errorf("Port = %d, want 3000", cfg.Port)
	}
	// Fields not in the file keep their defaults.
	if cfg.MaxConns != 25 {
		t.Errorf("MaxConns = %d, want 25 (default)", cfg.MaxConns)
	}
}

func TestLoadEnvOverridesFile(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	path := writeJSON(t, map[string]any{"port": 3000})
	t.Setenv("APP_PORT", "9090")

	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != 9090 {
		t.Errorf("Port = %d, want 9090 (env wins over file)", cfg.Port)
	}
}

func TestLoadFlagOverridesEnv(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	t.Setenv("APP_PORT", "9090")
	cfg, err := Load("", []string{"--port=4000"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != 4000 {
		t.Errorf("Port = %d, want 4000 (flag wins over env)", cfg.Port)
	}
}

func TestLoadUnsetEnvDoesNotBlankFile(t *testing.T) {
	// Sequential: relies on APP_LOG_LEVEL being absent from the environment.
	// APP_LOG_LEVEL is not set; the value from the file must survive.
	path := writeJSON(t, map[string]any{"log_level": "warn"})
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LogLevel != "warn" {
		t.Errorf("LogLevel = %q, want warn (env not set, file value must persist)", cfg.LogLevel)
	}
}

func TestLoadMissingFileFails(t *testing.T) {
	t.Parallel()

	_, err := Load(filepath.Join(t.TempDir(), "missing.json"), nil)
	if err == nil {
		t.Fatal("expected error for missing config file")
	}
}

func TestValidateReportsAllErrors(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Port:         0,  // invalid
		DatabaseURL:  "", // missing
		ReadTimeout:  -1, // invalid
		WriteTimeout: 10 * time.Second,
		LogLevel:     "verbose", // invalid
		MaxConns:     25,
	}
	errs := cfg.ValidateErrors()
	wants := []error{ErrInvalidPort, ErrMissingDB, ErrBadReadTimeout, ErrBadLogLevel}
	for _, want := range wants {
		found := false
		for _, e := range errs {
			if errors.Is(e, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("ValidateErrors missing %v", want)
		}
	}
}

func TestValidatePassesForValidConfig(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Host:         "localhost",
		Port:         8080,
		DatabaseURL:  "postgres://localhost/db",
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		LogLevel:     "info",
		MaxConns:     10,
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestLoadDurationFlag(t *testing.T) {
	t.Parallel()

	cfg, err := Load("", []string{"--read-timeout=30s"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ReadTimeout != 30*time.Second {
		t.Errorf("ReadTimeout = %v, want 30s", cfg.ReadTimeout)
	}
}

// ExampleLoad shows the happy-path: defaults only, then check a field.
func ExampleLoad() {
	cfg, _ := Load("", nil)
	_ = cfg.Port // 8080 by default
	// Output:
}
```

Your turn: add `TestLoadFlagOverridesFile` that writes a JSON file setting
`port: 3000`, calls `Load` with flag `--port=4000`, and asserts `cfg.Port == 4000`.

### Exercise 3: The Demo Program

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"log"
	"os"

	"example.com/config"
)

func main() {
	// Accepts --port, --host, --database-url, --log-level, --read-timeout,
	// --write-timeout, --max-conns from the command line.
	// Pass a JSON config file path as the first non-flag argument if desired.
	configPath := ""
	if len(os.Args) > 1 && os.Args[1][0] != '-' {
		configPath = os.Args[1]
		os.Args = append(os.Args[:1], os.Args[2:]...)
	}

	cfg, err := config.Load(configPath, os.Args[1:])
	if err != nil {
		log.Fatalf("load: %v", err)
	}

	fmt.Printf("host:          %s\n", cfg.Host)
	fmt.Printf("port:          %d\n", cfg.Port)
	fmt.Printf("database_url:  %s\n", cfg.DatabaseURL)
	fmt.Printf("read_timeout:  %v\n", cfg.ReadTimeout)
	fmt.Printf("write_timeout: %v\n", cfg.WriteTimeout)
	fmt.Printf("log_level:     %s\n", cfg.LogLevel)
	fmt.Printf("max_conns:     %d\n", cfg.MaxConns)
}
```

Run it:

```bash
go run ./cmd/demo
APP_PORT=9090 go run ./cmd/demo --log-level=warn
```

## Common Mistakes

### Using `os.Getenv` Instead of `os.LookupEnv`

Wrong: `val := os.Getenv("APP_PORT"); if val != "" { setField(...) }`. When the
variable is deliberately set to the empty string `APP_PORT=""`, `os.Getenv`
returns `""` and the condition is false, so the empty string silently loses to a
lower-priority layer. More commonly, an unset variable also returns `""`, so the
code works accidentally until someone sets an explicit empty value.

Fix: use `os.LookupEnv`. It returns `(value, ok)` where `ok` is `false` only when
the variable is not present in the environment.

### Parsing Duration Fields as Plain `int64`

Wrong:

```go
case reflect.Int64:
	n, err := strconv.ParseInt(val, 10, 64)
	...
	field.SetInt(n)
```

What happens: `"5s"` passed as a flag value is parsed by `flag` as the string
`"5s"`. `strconv.ParseInt("5s", 10, 64)` fails, the error is silently ignored (if
the caller discards it), and the field stays at its previous value — silent
misconfiguration.

Fix: check `field.Type() == reflect.TypeOf(time.Duration(0))` before the
`reflect.Int64` branch and call `time.ParseDuration`.

### Registering Flags With The Current Value as The Default

The `applyFlags` function registers each flag with the current field value as its
default: `fs.String(name, currentValue, ...)`. After `fs.Parse`, every registered
flag has a value — either what was on the command line or the default that was
registered. If you call `setField` for all registered flags instead of only the
explicitly provided ones (via `fs.Visit`), you write the current value back over
itself, which is harmless but also silently overwrites values from higher-priority
sources that were applied between `applyEnv` and the flag loop.

Fix: use `fs.Visit` (not `fs.VisitAll`) to iterate only explicitly provided flags.

### Stopping Validation at the First Error

Wrong: return the first error encountered in `Validate`. The operator restarts the
process for each broken field.

Fix: collect all validation errors into a slice, format them into one message, and
return them together. Wrap each with `fmt.Errorf("%w: ...", ErrSentinel)` so
callers can `errors.Is` each one without parsing strings.

## Verification

From `~/go-exercises/config`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. The test suite is the verification — there is no program to
eyeball.

## Summary

- Apply layers in order: defaults < file < env < flags; each layer overwrites only
  the fields it explicitly provides.
- Use `os.LookupEnv` (not `os.Getenv`) to distinguish "not set" from "set to
  empty"; use `fs.Visit` (not `fs.VisitAll`) to distinguish "not given" from
  "defaulted".
- Drive all four layers generically from struct tags; adding a field requires only
  a new struct line, not a loader change.
- Check `field.Type()` before `field.Kind()` for `time.Duration` to avoid
  misrouting `int64` fields.
- Validate all fields at once and wrap each error with a sentinel for `errors.Is`.

## What's Next

Next: [Feature Flags](../03-feature-flags/03-feature-flags.md).

## Resources

- [flag package](https://pkg.go.dev/flag) — `FlagSet.Visit` vs `FlagSet.VisitAll`
- [os.LookupEnv](https://pkg.go.dev/os#LookupEnv) — distinguishes unset from empty
- [reflect package](https://pkg.go.dev/reflect) — `Value.Kind`, `Value.Type`, struct tag iteration
- [encoding/json](https://pkg.go.dev/encoding/json) — `json.Unmarshal` with struct tags
- [time.ParseDuration](https://pkg.go.dev/time#ParseDuration) — parses "5s", "30ms", "1h"
