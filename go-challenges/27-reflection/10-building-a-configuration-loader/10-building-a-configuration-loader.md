# 10. Building a Configuration Loader

Configuration libraries like `viper`, `envconfig`, and `kong` all converge on the same mechanism: reflection over struct tags. The struct defines the schema; the tags define the mapping from external sources (environment variables, files, flags) to Go fields; the library walks the struct and applies values in priority order. Building one yourself cements how `reflect.Value.Set`, recursive struct walking, string-to-type conversion, and multi-source merge work together.

```text
configloader/
  go.mod
  loader.go
  loader_test.go
  cmd/demo/main.go
```

## Concepts

### Priority-Ordered Providers

A well-designed configuration loader applies sources in a defined priority order. A common stack is:

```
defaults (lowest) < JSON file < environment variables < CLI flags (highest)
```

Each source is a `Provider` that answers "do you have a value for this key?" The loader iterates providers in order; later providers override earlier ones, but only for keys they actually define — a provider that returns nothing for a key leaves the value from an earlier source intact.

### Recursive Struct Walking

Nested structs require recursive field walking. The outer struct might have a `DBConfig` field; `DBConfig` has its own `env` and `default` tags. The caller does not want to register each nested struct separately — the loader should discover them automatically.

The one exception: `time.Time` is a struct but should be treated as a leaf (parsed from a string), not recursed into.

### String-to-Type Conversion

All external sources (env vars, config files loaded as strings, CLI flags) provide `string` values. The loader must convert them to the field's Go type:

| Field type | Conversion |
| --- | --- |
| `string` | identity |
| `int`, `int64` | `strconv.ParseInt` |
| `float64` | `strconv.ParseFloat` |
| `bool` | `strconv.ParseBool` |
| `time.Duration` | `time.ParseDuration` |
| `[]string` | `strings.Split(s, ",")` |

Duration is the most common surprise: `time.Duration` has kind `Int64` in reflect, so a naive kind-switch will try `strconv.ParseInt("30s")` and fail. The field type must be compared against `reflect.TypeOf(time.Duration(0))` before falling through to the integer case.

### Prefix Propagation for Nested Structs

A field of type `DBConfig` tagged `env:"DB" prefix:"DB_"` means all `env` tags inside `DBConfig` are prefixed with `DB_`. The prefix accumulates as you recurse:

```
Outer prefix: ""     + field prefix: "DB_"   → inner prefix: "DB_"
Inner field: env:"DSN" with prefix "DB_"     → env key: "DB_DSN"
```

### Pointer Fields as Optional Configuration

A `*string` field that receives no value from any source stays `nil`. The `required` validation rule does not apply to pointer fields by default — they are optional by definition.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/27-reflection/10-building-a-configuration-loader/10-building-a-configuration-loader/cmd/demo
cd go-solutions/27-reflection/10-building-a-configuration-loader/10-building-a-configuration-loader
```

### Exercise 1: Provider Interface and Built-in Providers

Create `loader.go`:

```go
package configloader

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// ErrRequired is returned when a required field has no value from any provider.
var ErrRequired = errors.New("configloader: required field not set")

// ErrParse is returned when a string value cannot be converted to the field type.
var ErrParse = errors.New("configloader: parse error")

// Provider is a source of configuration key-value pairs.
type Provider interface {
	// Name returns a human-readable label used in error messages.
	Name() string
	// Get returns the string value for key and whether the key was present.
	Get(key string) (string, bool)
}

// EnvProvider reads values from environment variables.
type EnvProvider struct{}

func (EnvProvider) Name() string { return "environment" }
func (EnvProvider) Get(key string) (string, bool) {
	return os.LookupEnv(key)
}

// MapProvider provides values from an in-memory map.
// Useful for testing and for loading a parsed config file.
type MapProvider struct {
	Label  string
	Values map[string]string
}

func (m MapProvider) Name() string { return m.Label }
func (m MapProvider) Get(key string) (string, bool) {
	v, ok := m.Values[key]
	return v, ok
}

// DefaultProvider reads values from `default` struct tags.
// It is constructed from the struct type itself, not from an external source.
type DefaultProvider struct {
	// defaults maps env key → default string value, derived from struct tags.
	defaults map[string]string
}

// BuildDefaultProvider builds a DefaultProvider from the default tags on cfg.
// cfg must be a pointer to a struct.
func BuildDefaultProvider(cfg any) (*DefaultProvider, error) {
	p := &DefaultProvider{defaults: make(map[string]string)}
	v := reflect.ValueOf(cfg)
	if v.Kind() != reflect.Ptr || v.Elem().Kind() != reflect.Struct {
		return nil, fmt.Errorf("configloader: cfg must be a pointer to struct")
	}
	collectDefaults(v.Elem().Type(), "", p.defaults)
	return p, nil
}

func (d *DefaultProvider) Name() string { return "defaults" }
func (d *DefaultProvider) Get(key string) (string, bool) {
	v, ok := d.defaults[key]
	return v, ok
}

func collectDefaults(t reflect.Type, prefix string, out map[string]string) {
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		ft := sf.Type
		for ft.Kind() == reflect.Ptr {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct && ft != reflect.TypeOf(time.Time{}) {
			fieldPrefix := prefix + sf.Tag.Get("prefix")
			collectDefaults(ft, fieldPrefix, out)
			continue
		}
		envKey := prefix + sf.Tag.Get("env")
		if envKey == prefix {
			continue // no env tag
		}
		if def := sf.Tag.Get("default"); def != "" {
			out[envKey] = def
		}
	}
}

// Loader applies providers in order to populate a configuration struct.
type Loader struct {
	providers []Provider
}

// New creates a Loader that applies providers in order (lowest to highest priority).
func New(providers ...Provider) *Loader {
	return &Loader{providers: providers}
}

// Load populates cfg from the loader's providers and validates the result.
// cfg must be a non-nil pointer to a struct.
func (l *Loader) Load(cfg any) error {
	v := reflect.ValueOf(cfg)
	if v.Kind() != reflect.Ptr || v.IsNil() || v.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("configloader: cfg must be a non-nil pointer to struct")
	}
	if err := l.loadStruct(v.Elem(), v.Elem().Type(), ""); err != nil {
		return err
	}
	return validate(v.Elem(), v.Elem().Type(), "")
}

func (l *Loader) loadStruct(v reflect.Value, t reflect.Type, prefix string) error {
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		fv := v.Field(i)

		ft := sf.Type
		isPtr := ft.Kind() == reflect.Ptr
		if isPtr {
			ft = ft.Elem()
		}

		// Recurse into nested structs (but not time.Time).
		if ft.Kind() == reflect.Struct && ft != reflect.TypeOf(time.Time{}) {
			nestedPrefix := prefix + sf.Tag.Get("prefix")
			nested := fv
			if isPtr {
				// Allocate the nested struct on first use.
				if nested.IsNil() {
					nested.Set(reflect.New(ft))
				}
				nested = nested.Elem()
			}
			if err := l.loadStruct(nested, ft, nestedPrefix); err != nil {
				return err
			}
			continue
		}

		envKey := prefix + sf.Tag.Get("env")
		if envKey == prefix {
			continue // no env tag
		}

		// Apply providers in order; later providers win.
		for _, p := range l.providers {
			raw, ok := p.Get(envKey)
			if !ok {
				continue
			}
			target := fv
			if isPtr {
				if target.IsNil() {
					target.Set(reflect.New(ft))
				}
				target = target.Elem()
			}
			if err := setFromString(target, raw); err != nil {
				return fmt.Errorf("%w: field %s (key %s from %s): %v",
					ErrParse, sf.Name, envKey, p.Name(), err)
			}
		}
	}
	return nil
}

func setFromString(fv reflect.Value, s string) error {
	// Special case: time.Duration has Kind == Int64 but parses differently.
	if fv.Type() == reflect.TypeOf(time.Duration(0)) {
		d, err := time.ParseDuration(s)
		if err != nil {
			return err
		}
		fv.SetInt(int64(d))
		return nil
	}
	// Special case: []string split by comma.
	if fv.Kind() == reflect.Slice && fv.Type().Elem().Kind() == reflect.String {
		parts := strings.Split(s, ",")
		fv.Set(reflect.ValueOf(parts))
		return nil
	}

	switch fv.Kind() {
	case reflect.String:
		fv.SetString(s)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return err
		}
		fv.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return err
		}
		fv.SetUint(n)
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return err
		}
		fv.SetFloat(f)
	case reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return err
		}
		fv.SetBool(b)
	default:
		return fmt.Errorf("unsupported kind %v", fv.Kind())
	}
	return nil
}

// validate checks required tags on the loaded struct.
func validate(v reflect.Value, t reflect.Type, path string) error {
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		fv := v.Field(i)
		name := sf.Name
		if path != "" {
			name = path + "." + name
		}

		ft := sf.Type
		if ft.Kind() == reflect.Ptr {
			if fv.IsNil() {
				continue // pointer fields are optional unless required tag is set
			}
			if err := validate(fv.Elem(), ft.Elem(), name); err != nil {
				return err
			}
			continue
		}
		if ft.Kind() == reflect.Struct && ft != reflect.TypeOf(time.Time{}) {
			if err := validate(fv, ft, name); err != nil {
				return err
			}
			continue
		}

		rules := sf.Tag.Get("validate")
		if strings.Contains(rules, "required") && fv.IsZero() {
			env := sf.Tag.Get("env")
			return fmt.Errorf("%w: %s (env: %s)", ErrRequired, name, env)
		}
	}
	return nil
}
```

### Exercise 2: Tests

Create `loader_test.go`:

```go
package configloader

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

type DBConfig struct {
	DSN      string        `env:"DSN" validate:"required"`
	MaxConns int           `env:"MAX_CONNS" default:"10"`
	Timeout  time.Duration `env:"TIMEOUT" default:"5s"`
}

type ServerConfig struct {
	Host     string   `env:"HOST" default:"localhost" validate:"required"`
	Port     int      `env:"PORT" default:"8080"`
	Debug    bool     `env:"DEBUG" default:"false"`
	Origins  []string `env:"ORIGINS"`
	Database DBConfig `prefix:"DB_"`
}

func newLoader(m map[string]string) *Loader {
	def, _ := BuildDefaultProvider(&ServerConfig{})
	return New(def, MapProvider{Label: "test", Values: m})
}

func TestDefaultsOnly(t *testing.T) {
	t.Parallel()

	var cfg ServerConfig
	// DSN has no default and is required — set it to avoid validation error.
	l := newLoader(map[string]string{"DB_DSN": "postgres://test"})
	if err := l.Load(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "localhost" {
		t.Errorf("Host = %q, want localhost", cfg.Host)
	}
	if cfg.Port != 8080 {
		t.Errorf("Port = %d, want 8080", cfg.Port)
	}
	if cfg.Database.MaxConns != 10 {
		t.Errorf("DB MaxConns = %d, want 10", cfg.Database.MaxConns)
	}
}

func TestEnvOverridesDefaults(t *testing.T) {
	t.Parallel()

	var cfg ServerConfig
	l := newLoader(map[string]string{
		"PORT":   "9090",
		"DB_DSN": "postgres://prod",
	})
	if err := l.Load(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 9090 {
		t.Errorf("Port = %d, want 9090", cfg.Port)
	}
}

func TestDurationParsing(t *testing.T) {
	t.Parallel()

	var cfg ServerConfig
	l := newLoader(map[string]string{
		"DB_DSN":     "postgres://test",
		"DB_TIMEOUT": "2m30s",
	})
	if err := l.Load(&cfg); err != nil {
		t.Fatal(err)
	}
	want := 2*time.Minute + 30*time.Second
	if cfg.Database.Timeout != want {
		t.Errorf("Timeout = %v, want %v", cfg.Database.Timeout, want)
	}
}

func TestSliceParsing(t *testing.T) {
	t.Parallel()

	var cfg ServerConfig
	l := newLoader(map[string]string{
		"DB_DSN":  "postgres://test",
		"ORIGINS": "http://a.com,http://b.com,http://c.com",
	})
	if err := l.Load(&cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Origins) != 3 {
		t.Errorf("Origins len = %d, want 3: %v", len(cfg.Origins), cfg.Origins)
	}
}

func TestRequiredFieldMissing(t *testing.T) {
	t.Parallel()

	var cfg ServerConfig
	// DB_DSN is required but no provider has it.
	def, _ := BuildDefaultProvider(&ServerConfig{})
	l := New(def)
	err := l.Load(&cfg)
	if !errors.Is(err, ErrRequired) {
		t.Fatalf("err = %v, want ErrRequired", err)
	}
}

func TestBadIntParseFails(t *testing.T) {
	t.Parallel()

	var cfg ServerConfig
	l := newLoader(map[string]string{
		"DB_DSN": "postgres://test",
		"PORT":   "not-a-number",
	})
	err := l.Load(&cfg)
	if !errors.Is(err, ErrParse) {
		t.Fatalf("err = %v, want ErrParse", err)
	}
}

func TestNestedPrefixPropagation(t *testing.T) {
	t.Parallel()

	var cfg ServerConfig
	l := newLoader(map[string]string{
		"DB_DSN":       "postgres://nested",
		"DB_MAX_CONNS": "25",
	})
	if err := l.Load(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Database.DSN != "postgres://nested" {
		t.Errorf("DB DSN = %q, want postgres://nested", cfg.Database.DSN)
	}
	if cfg.Database.MaxConns != 25 {
		t.Errorf("DB MaxConns = %d, want 25", cfg.Database.MaxConns)
	}
}

func TestBoolParsing(t *testing.T) {
	t.Parallel()

	var cfg ServerConfig
	l := newLoader(map[string]string{
		"DB_DSN": "postgres://test",
		"DEBUG":  "true",
	})
	if err := l.Load(&cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Debug {
		t.Error("Debug should be true")
	}
}

func ExampleLoader_Load() {
	type Cfg struct {
		Host string `env:"HOST" default:"localhost"`
		Port int    `env:"PORT" default:"8080"`
	}
	def, _ := BuildDefaultProvider(&Cfg{})
	l := New(def, MapProvider{Label: "env", Values: map[string]string{"PORT": "9090"}})
	var cfg Cfg
	l.Load(&cfg)
	fmt.Printf("host=%s port=%d\n", cfg.Host, cfg.Port)
	// Output: host=localhost port=9090
}
```

Add `"fmt"` to `loader_test.go`'s import block — the Example uses it.

**Your turn:** add `TestMultipleProvidersLastWins` that creates two `MapProvider` values with the same key but different values, passes them in order to `New`, and asserts the last provider's value wins.

### Exercise 3: Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"time"

	"example.com/configloader"
)

type DatabaseConfig struct {
	DSN      string        `env:"DSN" validate:"required"`
	MaxConns int           `env:"MAX_CONNS" default:"10"`
	Timeout  time.Duration `env:"TIMEOUT" default:"30s"`
}

type AppConfig struct {
	Host     string         `env:"HOST" default:"0.0.0.0" validate:"required"`
	Port     int            `env:"PORT" default:"8080"`
	Debug    bool           `env:"DEBUG" default:"false"`
	Database DatabaseConfig `prefix:"DB_"`
}

func main() {
	// Simulate environment variables.
	envVars := map[string]string{
		"PORT":         "3000",
		"DEBUG":        "true",
		"DB_DSN":       "postgres://localhost/myapp",
		"DB_MAX_CONNS": "20",
	}

	def, err := configloader.BuildDefaultProvider(&AppConfig{})
	if err != nil {
		log.Fatal(err)
	}
	l := configloader.New(def, configloader.MapProvider{Label: "env", Values: envVars})

	var cfg AppConfig
	if err := l.Load(&cfg); err != nil {
		log.Fatal("config error:", err)
	}

	fmt.Printf("host=%s port=%d debug=%v\n", cfg.Host, cfg.Port, cfg.Debug)
	fmt.Printf("db dsn=%s max_conns=%d timeout=%v\n",
		cfg.Database.DSN, cfg.Database.MaxConns, cfg.Database.Timeout)
}
```

Run with `go run ./cmd/demo`.

## Common Mistakes

### Handling time.Duration as a Plain Int

Wrong: falling through to the `reflect.Int64` case for `time.Duration` and calling `strconv.ParseInt("30s")`.

What happens: `strconv.ParseInt` fails on `"30s"` with a parse error.

Fix: compare `fv.Type() == reflect.TypeOf(time.Duration(0))` before the kind-switch and call `time.ParseDuration` for that case.

### Not Distinguishing "key absent" from "key present but empty"

Wrong: using `os.Getenv(key)` which returns `""` for both an unset variable and a variable explicitly set to `""`.

What happens: an explicit `PORT=` (empty string) is treated the same as an absent `PORT`, so the default is not overridden as the user intended.

Fix: use `os.LookupEnv(key)` which returns `(value, false)` for absent and `("", true)` for explicitly empty.

### Stopping Recursion at time.Time

Wrong: recursing into `time.Time` fields because they have kind `Struct`.

What happens: the loader tries to find `env` tags on `time.Time`'s internal fields (which have none), silently does nothing, and the field is never set from string input.

Fix: check `ft == reflect.TypeOf(time.Time{})` and treat it as a leaf (handled by `setFromString`).

## Verification

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

## Summary

- A `Provider` interface separates configuration sources; the `Loader` iterates them in priority order.
- `LoadStruct` recurses into nested struct fields, accumulating env-key prefixes from `prefix` struct tags.
- `setFromString` converts strings to Go types via `strconv`; handle `time.Duration` before the generic int case.
- Use `os.LookupEnv` (not `os.Getenv`) to distinguish absent from explicitly empty.
- Pointer fields are optional by design; the `required` validation check only fires for non-pointer, non-zero fields.

## What's Next

Next: [unsafe.Pointer and uintptr](../../28-unsafe-and-cgo/01-unsafe-pointer-and-uintptr/01-unsafe-pointer-and-uintptr.md).

## Resources

- [reflect.Value.Set](https://pkg.go.dev/reflect#Value.Set)
- [os.LookupEnv](https://pkg.go.dev/os#LookupEnv)
- [time.ParseDuration](https://pkg.go.dev/time#ParseDuration)
- [viper source](https://github.com/spf13/viper)
- [envconfig](https://github.com/kelseyhightower/envconfig)
