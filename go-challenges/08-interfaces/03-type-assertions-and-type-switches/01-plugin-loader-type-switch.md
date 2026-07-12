# Exercise 1: Dispatch Config Values With A Type Switch (Plugin Loader)

A plugin loader receives configuration as `map[string]any` — the shape you get
from decoded JSON or YAML — and must turn each raw value into a typed plugin. This
is the workhorse example for all three assertion forms in one place: a type switch
for dispatch, comma-ok for a single expected type, and the panic form behind a
`Must*` name.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
pluginload/                 independent module: example.com/pluginload
  go.mod                    module path
  pluginload.go             Load (type switch), LoadString (comma-ok), MustLoadString (panic form)
  cmd/
    demo/
      main.go               runnable demo over a mixed config map
  pluginload_test.go        table-driven dispatch + errors.Is + nil-value contract
```

Files: `pluginload.go`, `cmd/demo/main.go`, `pluginload_test.go`.
Implement: `Load(name, map[string]any) (Plugin, error)` dispatching by type, `LoadString` via comma-ok, `MustLoadString` via the panic form.
Test: table-driven cases for string/number/bool/config, `errors.Is` for `ErrUnknownType` and `ErrMissingKey`, and a `nil` value handled by `case nil` rather than as an error.
Verify: `go test -count=1 -race ./...`

### Why a type switch here

The loader's job is dispatch: given an `any`, decide which of several concrete
shapes it holds and route accordingly. A chain of comma-ok assertions would work
but reads poorly; a `switch v := raw.(type)` says the intent directly and binds a
typed `v` in each arm. The design points worth pinning down:

`case int, int64, float64:` groups the numeric shapes. Note that inside this
multi-type arm `x` keeps the interface type `any`, not a numeric type — which is
exactly why the arm stores `x` as `Data any` rather than trying to use it as a
number. If you needed the number itself you would nest a further switch.

`case nil` is a real arm, not the same as "missing". A key present in the map with
value `nil` (a JSON `null`) is a value the loader recognizes and turns into a
`"null"` plugin; a key absent from the map is `ErrMissingKey`. Distinguishing the
two is the "nil is a value, not an error" contract, and it is easy to get wrong by
letting `nil` fall through to `default`.

`default` returns `ErrUnknownType` wrapped with `%w` and the offending dynamic
type via `%T`, so an unexpected shape is observable, not silently dropped.
`LoadString` shows the comma-ok form for the common "I expect exactly a string"
path, returning a typed error on mismatch instead of panicking. `MustLoadString`
is the one place the panic form is licensed, and its `Must` name says so.

Create `pluginload.go`:

```go
package pluginload

import (
	"errors"
	"fmt"
)

// Sentinel errors, wrapped with %w so callers match them with errors.Is.
var (
	ErrUnknownType = errors.New("unknown plugin type")
	ErrMissingKey  = errors.New("missing plugin key")
)

// Plugin is a typed view of one configuration value.
type Plugin struct {
	Name string
	Kind string
	Data any
}

// Load dispatches raw[name] by its dynamic type. A missing key is an error; a
// present nil value is the "null" plugin, not an error.
func Load(name string, raw map[string]any) (Plugin, error) {
	v, ok := raw[name]
	if !ok {
		return Plugin{}, fmt.Errorf("%w: %s", ErrMissingKey, name)
	}
	switch x := v.(type) {
	case nil:
		return Plugin{Name: name, Kind: "null", Data: nil}, nil
	case string:
		return Plugin{Name: name, Kind: "string", Data: x}, nil
	case int, int64, float64:
		return Plugin{Name: name, Kind: "number", Data: x}, nil
	case bool:
		return Plugin{Name: name, Kind: "bool", Data: x}, nil
	case map[string]any:
		return Plugin{Name: name, Kind: "config", Data: x}, nil
	default:
		return Plugin{}, fmt.Errorf("%w: %T", ErrUnknownType, v)
	}
}

// LoadString returns raw[name] only if it is a string, via the safe comma-ok
// form. A non-string value is ErrUnknownType, never a panic.
func LoadString(name string, raw map[string]any) (string, error) {
	v, ok := raw[name]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrMissingKey, name)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%w: %T is not string", ErrUnknownType, v)
	}
	return s, nil
}

// MustLoadString uses the panic form and is licensed only by its Must name: the
// caller asserts the key is a present string. Any other shape panics.
func MustLoadString(name string, raw map[string]any) string {
	return raw[name].(string)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/pluginload"
)

func main() {
	cfg := map[string]any{
		"service": "checkout",
		"port":    8080,
		"debug":   true,
		"limits":  map[string]any{"rps": 100},
		"note":    nil,
	}

	for _, key := range []string{"service", "port", "debug", "limits", "note"} {
		p, err := pluginload.Load(key, cfg)
		if err != nil {
			fmt.Printf("%s: error %v\n", key, err)
			continue
		}
		fmt.Printf("%s: kind=%s\n", p.Name, p.Kind)
	}

	if _, err := pluginload.Load("absent", cfg); errors.Is(err, pluginload.ErrMissingKey) {
		fmt.Println("absent: missing key")
	}

	fmt.Println("service string:", pluginload.MustLoadString("service", cfg))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
service: kind=string
port: kind=number
debug: kind=bool
limits: kind=config
note: kind=null
absent: missing key
service string: checkout
```

### Tests

The table drives the dispatch arms; the error cases assert the sentinels with
`errors.Is`; `TestLoadHandlesNilValue` pins the `case nil` contract so that a
present `nil` is the null plugin and not `ErrUnknownType`.

Create `pluginload_test.go`:

```go
package pluginload

import (
	"errors"
	"fmt"
	"testing"
)

func TestLoadDispatchesByType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		value    any
		wantKind string
	}{
		{"string", "alice", "string"},
		{"int", int(42), "number"},
		{"int64", int64(42), "number"},
		{"float64", float64(3.14), "number"},
		{"bool", true, "bool"},
		{"config", map[string]any{"host": "localhost"}, "config"},
		{"nil", nil, "null"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, err := Load("k", map[string]any{"k": tc.value})
			if err != nil {
				t.Fatalf("Load(%s) error: %v", tc.name, err)
			}
			if p.Kind != tc.wantKind {
				t.Fatalf("Kind = %q, want %q", p.Kind, tc.wantKind)
			}
		})
	}
}

func TestLoadHandlesNilValue(t *testing.T) {
	t.Parallel()
	// A present nil value is the null plugin, not an error.
	p, err := Load("note", map[string]any{"note": nil})
	if err != nil {
		t.Fatalf("nil value returned error: %v", err)
	}
	if p.Kind != "null" || p.Data != nil {
		t.Fatalf("nil value = %+v, want kind=null data=nil", p)
	}
}

func TestLoadRejectsUnknownType(t *testing.T) {
	t.Parallel()
	_, err := Load("weird", map[string]any{"weird": []int{1, 2, 3}})
	if !errors.Is(err, ErrUnknownType) {
		t.Fatalf("err = %v, want ErrUnknownType", err)
	}
}

func TestLoadRejectsMissingKey(t *testing.T) {
	t.Parallel()
	_, err := Load("missing", map[string]any{"other": "value"})
	if !errors.Is(err, ErrMissingKey) {
		t.Fatalf("err = %v, want ErrMissingKey", err)
	}
}

func TestLoadStringAcceptsStringOnly(t *testing.T) {
	t.Parallel()
	if got, err := LoadString("s", map[string]any{"s": "ok"}); err != nil || got != "ok" {
		t.Fatalf("LoadString = %q,%v; want ok,nil", got, err)
	}
	if _, err := LoadString("port", map[string]any{"port": 8080}); !errors.Is(err, ErrUnknownType) {
		t.Fatalf("err = %v, want ErrUnknownType", err)
	}
}

func TestMustLoadStringPanicsOnMismatch(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("MustLoadString did not panic on a non-string value")
		}
	}()
	_ = MustLoadString("port", map[string]any{"port": 8080})
}

func ExampleLoad() {
	p, _ := Load("port", map[string]any{"port": 8080})
	fmt.Println(p.Kind)
	// Output: number
}
```

## Review

The loader is correct when dispatch is a total function of the value's dynamic
type: every recognized shape lands in its arm, a present `nil` lands in `case nil`,
an unrecognized shape lands in `default` with `ErrUnknownType`, and an absent key
is `ErrMissingKey` before the switch even runs. The three assertion forms each
appear where they belong — the type switch for multi-way dispatch, comma-ok in
`LoadString` for one expected type, and the panic form only inside `MustLoadString`
whose name announces it. The traps this exercise guards against are letting `nil`
fall through to `default` (breaking the null-is-a-value contract), and reaching for
the panic form in `Load` where a hostile config value would crash the process. Run
`go test -race` to confirm the dispatch and the panic contract together.

## Resources

- [Go Specification: Type assertions](https://go.dev/ref/spec#Type_assertions)
- [Go Specification: Type switches](https://go.dev/ref/spec#Type_switches)
- [errors.Is](https://pkg.go.dev/errors#Is)

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-retry-classifier-net-error.md](02-retry-classifier-net-error.md)
