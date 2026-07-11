# Exercise 10: Config Loader — Example with Setup That Stays Deterministic

An example is allowed to do real work: build input structs, seed a fake store,
call a multi-step function. What it may not do is produce non-deterministic
stdout, or it loses its `// Output:` comment. This exercise builds a config
loader whose example constructs its inputs in-line and pins the resolved config —
and draws the line between an `Example` and a `Test`.

## What you'll build

```text
configload/                 independent module: example.com/configload
  go.mod                    go 1.26
  config.go                 type Config; Load(overrides, defaults) Config; String()
  cmd/
    demo/
      main.go               runnable demo resolving overrides over defaults
  config_test.go            table-driven Test + ExampleLoad with deterministic // Output:
```

Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
Implement: `Config` with `Host`, `Port`, `TLS`; `Load(overrides, defaults Config) Config` using `cmp.Or` for fallback; a `String()` for stable formatting.
Test: a table-driven `Test`, plus `ExampleLoad` that builds inputs in-line and pins the resolved config.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/configload/cmd/demo
cd ~/go-exercises/configload
go mod init example.com/configload
```

## Setup is allowed; non-determinism is not

`ExampleLoad` does genuine setup — it constructs a `defaults` and an `overrides`
`Config`, calls `Load`, and prints the result. That is entirely legitimate: an
example may build structs, seed an in-memory store, or run a multi-step pipeline.
The single constraint is that everything it writes to stdout must be
reproducible, because the `// Output:` comment is an exact-match assertion. Here
the output is a pure function of the two input structs, so it is deterministic and
safe to pin. Add a `time.Now()`-derived field to the printed output and the
example would flake on every run — which is the boundary the exercise trains:
setup, yes; non-deterministic output, no.

`Load` resolves configuration the idiomatic way with `cmp.Or` (Go 1.22+), which
returns its first non-zero argument. A zero-valued override field — a `Port` of
`0`, an empty `Host` — falls back to the default, so the caller can supply a
partial override and let the rest come from defaults. `String()` gives a stable
one-line rendering so the example's output is fixed by the struct values alone.

This is also the place to name the `Example`-vs-`Test` boundary. Reach for an
`Example` when the documentation value *is* the input-to-stdout mapping: "give
`Load` these two configs, get this resolved config." Reach for a `Test` when you
need anything stdout cannot express — arbitrary assertions on fields, error
injection, `t.Cleanup` to tear down a seeded resource, or table-driven cases over
many inputs. The `Test` below does the latter (it asserts individual fields across
several override combinations); the `Example` does the former (it documents one
canonical resolution). Most packages ship both.

Create `config.go`:

```go
package configload

import (
	"cmp"
	"fmt"
)

// Config is a resolved service configuration.
type Config struct {
	Host string
	Port int
	TLS  bool
}

// Load resolves overrides on top of defaults. cmp.Or returns its first non-zero
// argument, so a zero-valued override field falls back to the default.
func Load(overrides, defaults Config) Config {
	return Config{
		Host: cmp.Or(overrides.Host, defaults.Host),
		Port: cmp.Or(overrides.Port, defaults.Port),
		TLS:  overrides.TLS || defaults.TLS,
	}
}

// String renders the config as a stable one-line summary.
func (c Config) String() string {
	return fmt.Sprintf("host=%s port=%d tls=%t", c.Host, c.Port, c.TLS)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/configload"
)

func main() {
	defaults := configload.Config{Host: "localhost", Port: 8080, TLS: false}
	overrides := configload.Config{Host: "api.example.com", TLS: true}
	fmt.Println(configload.Load(overrides, defaults))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
host=api.example.com port=8080 tls=true
```

### Tests and the example

The `Test` is table-driven over override combinations, asserting the resolved
fields — the kind of assertion an example cannot express. `ExampleLoad` documents
one canonical resolution with a deterministic `// Output:`.

Create `config_test.go`:

```go
package configload

import (
	"fmt"
	"testing"
)

func TestLoad(t *testing.T) {
	t.Parallel()
	defaults := Config{Host: "localhost", Port: 8080, TLS: false}
	tests := []struct {
		name      string
		overrides Config
		want      Config
	}{
		{"empty overrides use defaults", Config{}, Config{"localhost", 8080, false}},
		{"host override", Config{Host: "api.example.com"}, Config{"api.example.com", 8080, false}},
		{"port override", Config{Port: 9090}, Config{"localhost", 9090, false}},
		{"tls enabled", Config{TLS: true}, Config{"localhost", 8080, true}},
		{"full override", Config{"h", 1, true}, Config{"h", 1, true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Load(tt.overrides, defaults); got != tt.want {
				t.Errorf("Load = %v, want %v", got, tt.want)
			}
		})
	}
}

func ExampleLoad() {
	defaults := Config{Host: "localhost", Port: 8080, TLS: false}
	overrides := Config{Host: "api.example.com", TLS: true} // Port left zero -> default
	fmt.Println(Load(overrides, defaults))
	// Output: host=api.example.com port=8080 tls=true
}
```

## Review

`ExampleLoad` is correct because its stdout is a pure function of the two input
structs — real setup, deterministic output — so the `// Output:` comment holds.
The rule to carry away is the boundary: an example may do arbitrary setup but its
printed output must stay reproducible, so a `time.Now()`-stamped field would flake
and belongs in a `Test` instead. And the deeper judgment: use an `Example` when
the doc value is the input-to-stdout mapping, a `Test` when you need field-level
assertions, error injection, or `t.Cleanup` — which is why this package ships both
`TestLoad` and `ExampleLoad`. Keep `gofmt -l` empty and `go vet ./...` clean.

## Resources

- [cmp.Or](https://pkg.go.dev/cmp#Or) — returns the first non-zero argument, the idiom behind the fallback.
- [testing package — Examples](https://pkg.go.dev/testing#hdr-Examples) — the `// Output:` determinism requirement.
- [Effective Go — Testing](https://go.dev/doc/effective_go#testing) — examples as documentation alongside tests.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-multiple-scenario-suffix-examples.md](09-multiple-scenario-suffix-examples.md) | Next: [../16-testing-time-dependent-code/00-concepts.md](../16-testing-time-dependent-code/00-concepts.md)
