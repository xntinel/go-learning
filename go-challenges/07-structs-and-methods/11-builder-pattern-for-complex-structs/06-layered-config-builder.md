# Exercise 6: Layered Config Builder: Defaults, then File, then Env

Service config is never one source. There are built-in defaults, a config file, and
environment overrides, and they compose in a fixed precedence: env beats file beats
default — but only when a source *actually set* the field. This module builds that
loader and gets the subtle part right: distinguishing "unset" from "set to zero",
so an explicit empty value is honored instead of silently replaced by a default.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
cfgbuild/                   independent module: example.com/cfgbuild
  go.mod                    go 1.26
  builder.go                package config: Config, FileConfig, Builder, New, Build
  cmd/
    demo/
      main.go               runnable demo: defaults, file override, env override
  builder_test.go           precedence + tri-state + parse-error tests
```

- Files: `builder.go`, `cmd/demo/main.go`, `builder_test.go`.
- Implement: a builder merging defaults `<` file `<` env with an injectable env lookup (`func(string) (string, bool)`), using `cmp.Or` for a field where empty means unset (`LogLevel`) and pointer/`ok`-bool tri-state for a field where empty is a real, intended value (`DBDSN`) and where zero is valid (`Port`). Parse and validate once in `Build`, aggregating errors with `errors.Join`.
- Test: defaults only; file overrides a subset; env overrides file; env set-but-empty resolved differently for the `cmp.Or` field versus the tri-state field; invalid port/duration returns an aggregated error; precedence is deterministic.
- Verify: `go test -count=1 -race ./...`

### Two correct ways to say "unset", and when each applies

The whole difficulty is telling "this source did not set the field" from "this
source set it to the zero value". Get it wrong and an operator who explicitly sets
`DB_DSN=` (empty, meaning "run without a database") gets the default DSN instead —
a silent, dangerous override.

There are two disciplined tools, and this module uses each where it fits:

`cmp.Or(a, b, c)` returns the first non-zero argument. It is perfect for `LogLevel`,
where an empty string genuinely means "not specified" — nobody sets a log level to
the empty string on purpose. `cmp.Or(envLog, fileLog, defaultLog)` reads exactly as
the precedence rule and treats empty at any layer as "skip me". But that same
behavior is *wrong* for `DBDSN`, where empty is a deliberate value. So `DBDSN` uses
the other tool: `os.LookupEnv`'s `ok` boolean (and a pointer field for the file
layer), which reports "the variable was set" independently of its value. If
`DB_DSN` is set at all — even to empty — it overrides. `Port` is the same story from
the numeric side: port 0 can legitimately mean "pick a free port", so zero is not a
safe "unset" signal and `Port` also uses the `ok`/pointer tri-state.

Parsing and validation happen once, in `Build`, over the fully-merged config, so an
invalid `PORT` or `REQUEST_TIMEOUT` string is reported (aggregated with
`errors.Join`) against the final resolved value, never a half-merged one. The env
lookup is an injectable `func(string) (string, bool)` so tests drive it without
touching the process environment; production passes `os.LookupEnv`.

Create `builder.go`:

```go
package config

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

// ErrInvalidConfig wraps every parse and validation failure so callers match it
// with errors.Is.
var ErrInvalidConfig = errors.New("invalid config")

// Config is the fully-merged, validated product.
type Config struct {
	Port           int
	LogLevel       string
	DBDSN          string
	RequestTimeout time.Duration
}

// FileConfig is the file layer. Pointer fields are tri-state: nil means "the
// file did not set this", so a non-nil pointer to the zero value is honored.
type FileConfig struct {
	Port           *int
	LogLevel       *string
	DBDSN          *string
	RequestTimeout *time.Duration
}

// Builder merges the three layers. env is injectable for testability.
type Builder struct {
	def  Config
	file FileConfig
	env  func(string) (string, bool)
}

// New seeds the built-in defaults.
func New() *Builder {
	return &Builder{
		def: Config{
			Port:           8080,
			LogLevel:       "info",
			DBDSN:          "postgres://localhost:5432/app",
			RequestTimeout: 30 * time.Second,
		},
	}
}

func (b *Builder) FromFile(f FileConfig) *Builder {
	b.file = f
	return b
}

func (b *Builder) FromEnv(lookup func(string) (string, bool)) *Builder {
	b.env = lookup
	return b
}

func (b *Builder) Build() (Config, error) {
	lookup := b.env
	if lookup == nil {
		lookup = os.LookupEnv
	}
	var errs []error
	cfg := b.def

	// Port: zero is a valid value, so use pointer/ok tri-state, never cmp.Or.
	if b.file.Port != nil {
		cfg.Port = *b.file.Port
	}
	if v, ok := lookup("PORT"); ok {
		if p, err := strconv.Atoi(v); err != nil {
			errs = append(errs, fmt.Errorf("%w: PORT=%q: %v", ErrInvalidConfig, v, err))
		} else {
			cfg.Port = p
		}
	}

	// LogLevel: empty means unset, so cmp.Or is exactly right. A set-but-empty
	// env value falls through to file then default.
	fileLog := ""
	if b.file.LogLevel != nil {
		fileLog = *b.file.LogLevel
	}
	envLog, _ := lookup("LOG_LEVEL")
	cfg.LogLevel = cmp.Or(envLog, fileLog, b.def.LogLevel)

	// DBDSN: empty is a deliberate value ("no database"), so use ok tri-state.
	// A set-but-empty env value overrides, unlike LogLevel.
	if b.file.DBDSN != nil {
		cfg.DBDSN = *b.file.DBDSN
	}
	if v, ok := lookup("DB_DSN"); ok {
		cfg.DBDSN = v
	}

	// RequestTimeout: tri-state plus a parse step.
	if b.file.RequestTimeout != nil {
		cfg.RequestTimeout = *b.file.RequestTimeout
	}
	if v, ok := lookup("REQUEST_TIMEOUT"); ok {
		if d, err := time.ParseDuration(v); err != nil {
			errs = append(errs, fmt.Errorf("%w: REQUEST_TIMEOUT=%q: %v", ErrInvalidConfig, v, err))
		} else {
			cfg.RequestTimeout = d
		}
	}

	if cfg.Port < 0 || cfg.Port > 65535 {
		errs = append(errs, fmt.Errorf("%w: port %d out of range", ErrInvalidConfig, cfg.Port))
	}
	if cfg.RequestTimeout <= 0 {
		errs = append(errs, fmt.Errorf("%w: request timeout %s must be positive", ErrInvalidConfig, cfg.RequestTimeout))
	}

	if err := errors.Join(errs...); err != nil {
		return Config{}, err
	}
	return cfg, nil
}
```

### The runnable demo

The demo shows the three layers stacking: defaults, then a file override of one
field, then env overriding file. The env lookup is a map so the demo is
self-contained.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/cfgbuild"
)

func main() {
	c, _ := config.New().Build()
	fmt.Printf("defaults: port=%d log=%s timeout=%s\n", c.Port, c.LogLevel, c.RequestTimeout)

	port := 9090
	c2, _ := config.New().FromFile(config.FileConfig{Port: &port}).Build()
	fmt.Printf("file:     port=%d log=%s\n", c2.Port, c2.LogLevel)

	env := map[string]string{"LOG_LEVEL": "debug", "REQUEST_TIMEOUT": "5s"}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	c3, _ := config.New().FromFile(config.FileConfig{Port: &port}).FromEnv(lookup).Build()
	fmt.Printf("env:      port=%d log=%s timeout=%s\n", c3.Port, c3.LogLevel, c3.RequestTimeout)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
defaults: port=8080 log=info timeout=30s
file:     port=9090 log=info
env:      port=9090 log=debug timeout=5s
```

The env run keeps the file's port (env set no `PORT`), takes the env log level and
timeout, and never disturbs the default DSN.

### Tests

The tests pin each precedence step and, crucially, the tri-state distinction: a
set-but-empty `LOG_LEVEL` falls through (`cmp.Or`), while a set-but-empty `DB_DSN`
overrides (`ok` bool). Parse failures aggregate through `ErrInvalidConfig`.

Create `builder_test.go`:

```go
package config

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func mapLookup(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestDefaultsOnly(t *testing.T) {
	t.Parallel()

	c, err := New().FromEnv(mapLookup(nil)).Build()
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != 8080 || c.LogLevel != "info" || c.RequestTimeout != 30*time.Second {
		t.Fatalf("defaults wrong: %+v", c)
	}
}

func TestFileOverridesSubset(t *testing.T) {
	t.Parallel()

	port := 9090
	c, err := New().FromFile(FileConfig{Port: &port}).FromEnv(mapLookup(nil)).Build()
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != 9090 {
		t.Fatalf("port = %d, want 9090 from file", c.Port)
	}
	if c.LogLevel != "info" {
		t.Fatalf("log = %q, want default info", c.LogLevel)
	}
}

func TestEnvOverridesFile(t *testing.T) {
	t.Parallel()

	port := 9090
	env := mapLookup(map[string]string{"PORT": "7000", "LOG_LEVEL": "warn"})
	c, err := New().FromFile(FileConfig{Port: &port}).FromEnv(env).Build()
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != 7000 {
		t.Fatalf("port = %d, want 7000 from env", c.Port)
	}
	if c.LogLevel != "warn" {
		t.Fatalf("log = %q, want warn from env", c.LogLevel)
	}
}

func TestSetButEmptyTriState(t *testing.T) {
	t.Parallel()

	// LOG_LEVEL set to empty: cmp.Or treats it as unset, falls through to default.
	// DB_DSN set to empty: ok-bool honors it as an explicit "no database".
	env := mapLookup(map[string]string{"LOG_LEVEL": "", "DB_DSN": ""})
	c, err := New().FromEnv(env).Build()
	if err != nil {
		t.Fatal(err)
	}
	if c.LogLevel != "info" {
		t.Fatalf("empty LOG_LEVEL should fall through to default, got %q", c.LogLevel)
	}
	if c.DBDSN != "" {
		t.Fatalf("empty DB_DSN should override to empty, got %q", c.DBDSN)
	}
}

func TestUnsetLeavesDefault(t *testing.T) {
	t.Parallel()

	// Neither var present: DB_DSN keeps its default.
	c, err := New().FromEnv(mapLookup(nil)).Build()
	if err != nil {
		t.Fatal(err)
	}
	if c.DBDSN != "postgres://localhost:5432/app" {
		t.Fatalf("unset DB_DSN should keep default, got %q", c.DBDSN)
	}
}

func TestInvalidValuesAggregate(t *testing.T) {
	t.Parallel()

	env := mapLookup(map[string]string{"PORT": "notanint", "REQUEST_TIMEOUT": "nope"})
	_, err := New().FromEnv(env).Build()
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("err = %v, want ErrInvalidConfig", err)
	}
}

func ExampleBuilder_Build() {
	c, _ := New().FromEnv(func(string) (string, bool) { return "", false }).Build()
	fmt.Println(c.Port, c.LogLevel)
	// Output: 8080 info
}
```

## Review

The loader is correct when precedence is deterministic and the "unset vs zero"
distinction is honored per field. `TestEnvOverridesFile` proves env wins over file
which wins over default. `TestSetButEmptyTriState` is the heart of the lesson: it
proves an empty `LOG_LEVEL` falls through (because `cmp.Or` treats empty as unset)
while an empty `DB_DSN` overrides (because the `ok` bool reports it was set). Getting
these two the same way — either both `cmp.Or` or both `ok`-bool — would be a bug for
one of them. Parsing and validation run once over the merged config and aggregate
through `ErrInvalidConfig`, so `TestInvalidValuesAggregate` matches it via
`errors.Is`. The trap to avoid: reaching for `cmp.Or` on a field where zero is a
real value, silently overriding an explicit setting. Run `go test -race` to confirm.

## Resources

- [cmp.Or](https://pkg.go.dev/cmp#Or) — first non-zero value; ideal for env-else-file-else-default when zero means unset.
- [os.LookupEnv](https://pkg.go.dev/os#LookupEnv) — the `ok` bool that distinguishes set-but-empty from unset.
- [time.ParseDuration](https://pkg.go.dev/time#ParseDuration) — parsing the timeout override.
- [errors.Join](https://pkg.go.dev/errors#Join) — aggregating parse and validation failures.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-generic-batch-builder.md](07-generic-batch-builder.md)
