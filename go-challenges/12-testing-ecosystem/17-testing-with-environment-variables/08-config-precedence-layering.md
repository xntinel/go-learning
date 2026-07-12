# Exercise 8: Resolve Config With 12-Factor Precedence: Defaults < File < Env

Real services draw the same setting from three places: a compiled default, a
config file, and the environment. The twelve-factor rule is that the environment
wins. This exercise builds a `Resolve` that layers the sources in that order and
makes the precedence a tested contract rather than tribal knowledge.

## What you'll build

```text
precedence/                independent module: example.com/precedence
  go.mod                   go directive supplied by the gate
  resolve.go               LookupFunc; Resolve(defaults, fileValues, getenv)
  cmd/
    demo/
      main.go              runnable demo: show each layer overriding the last
  resolve_test.go          per-field: default / file-wins / env-wins, fully parallel
```

Files: `resolve.go`, `cmd/demo/main.go`, `resolve_test.go`.
Implement: `Resolve(defaults, fileValues map[string]string, getenv LookupFunc) map[string]string` layering defaults < file < env.
Test: for each key, three parallel subtests proving default-used, file-over-default, env-over-file; uses the injected `getenv` so the whole suite is pure.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/17-testing-with-environment-variables/08-config-precedence-layering/cmd/demo
cd go-solutions/12-testing-ecosystem/17-testing-with-environment-variables/08-config-precedence-layering
```

## Layering by priority

`Resolve` starts from a copy of the defaults and, for each key, replaces the value
with the highest-priority source that actually provides one. The priority order,
lowest to highest, is default < file < environment. `cmp.Or` expresses "first
non-empty wins" concisely: `cmp.Or(envVal, fileVal, defaultVal)` returns `envVal`
if it is non-empty, else `fileVal`, else the default. Feeding the values in
highest-to-lowest priority order makes the precedence fall out of a single call.

Two design choices keep this honest. First, `Resolve` iterates over the *keys of
the defaults*, so the set of configurable keys is fixed by the compiled defaults —
a stray environment variable cannot inject an unexpected key, and every known key
always has at least its default. Second, an *empty* value from a higher layer does
not override: an empty string in the file or a set-but-empty env var is treated as
"not provided" by `cmp.Or`, so it falls through to the next layer. That matches how
operators expect layering to behave (a blank line in a file should not blank out a
default).

Because the environment is read through the injected `getenv LookupFunc` — the
same inversion as Exercise 6 — `Resolve` is pure, and its whole precedence suite
runs in parallel with a map-backed reader. Cloning the defaults with `maps.Clone`
means `Resolve` never mutates its caller's map.

Create `resolve.go`:

```go
package precedence

import (
	"cmp"
	"maps"
)

// LookupFunc is the shape of os.LookupEnv, injected so Resolve stays pure.
type LookupFunc func(string) (string, bool)

// Resolve layers configuration sources by twelve-factor precedence:
// defaults < file < environment. It returns a new map keyed by the defaults'
// keys, with each key set to the highest-priority non-empty value available.
func Resolve(defaults, fileValues map[string]string, getenv LookupFunc) map[string]string {
	out := maps.Clone(defaults)
	if out == nil {
		out = make(map[string]string)
	}
	for key := range out {
		envVal, _ := getenv(key) // "" when unset, which cmp.Or skips
		fileVal := fileValues[key]
		out[key] = cmp.Or(envVal, fileVal, out[key])
	}
	return out
}
```

## The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/precedence"
)

func main() {
	defaults := map[string]string{
		"log_level": "info",
		"http_port": "8080",
		"region":    "us-east-1",
	}
	file := map[string]string{
		"log_level": "debug", // file overrides the default
		"http_port": "9090",
	}
	env := map[string]string{
		"http_port": "443", // env overrides the file
	}
	getenv := func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}

	resolved := precedence.Resolve(defaults, file, getenv)
	fmt.Printf("log_level=%s\n", resolved["log_level"]) // from file
	fmt.Printf("http_port=%s\n", resolved["http_port"]) // from env
	fmt.Printf("region=%s\n", resolved["region"])       // from default
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
log_level=debug
http_port=443
region=us-east-1
```

## Tests

The suite proves the precedence per layer. For a given key, three parallel
subtests assert: the default is used when neither file nor env provides a value;
the file value wins over the default; the env value wins over the file. Because
`Resolve` takes an injected `getenv`, every subtest builds its own map-backed
reader and calls `t.Parallel()` — the whole contract is verified with no process
state.

Create `resolve_test.go`:

```go
package precedence

import (
	"fmt"
	"testing"
)

func mapGetenv(m map[string]string) LookupFunc {
	return func(key string) (string, bool) {
		v, ok := m[key]
		return v, ok
	}
}

func TestResolvePrecedence(t *testing.T) {
	t.Parallel()

	defaults := map[string]string{"log_level": "info"}

	tests := []struct {
		name string
		file map[string]string
		env  map[string]string
		want string
	}{
		{
			name: "default used when nothing else set",
			file: nil,
			env:  nil,
			want: "info",
		},
		{
			name: "file overrides default",
			file: map[string]string{"log_level": "warn"},
			env:  nil,
			want: "warn",
		},
		{
			name: "env overrides file",
			file: map[string]string{"log_level": "warn"},
			env:  map[string]string{"log_level": "debug"},
			want: "debug",
		},
		{
			name: "empty env does not override file",
			file: map[string]string{"log_level": "warn"},
			env:  map[string]string{"log_level": ""},
			want: "warn",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel() // pure: Resolve reads only its arguments

			got := Resolve(defaults, tt.file, mapGetenv(tt.env))
			if got["log_level"] != tt.want {
				t.Fatalf("log_level = %q, want %q", got["log_level"], tt.want)
			}
		})
	}
}

func TestResolveDoesNotMutateDefaults(t *testing.T) {
	t.Parallel()

	defaults := map[string]string{"region": "us-east-1"}
	env := map[string]string{"region": "eu-west-1"}

	_ = Resolve(defaults, nil, mapGetenv(env))
	if defaults["region"] != "us-east-1" {
		t.Fatalf("Resolve mutated defaults: region = %q", defaults["region"])
	}
}

func ExampleResolve() {
	defaults := map[string]string{"port": "8080"}
	file := map[string]string{"port": "9090"}
	env := map[string]string{"port": "443"}

	got := Resolve(defaults, file, mapGetenv(env))
	fmt.Println(got["port"])
	// Output: 443
}
```

## Review

`Resolve` is correct when each layer overrides only the ones below it and an empty
higher-layer value falls through — the four table cases pin exactly that, and
`TestResolveDoesNotMutateDefaults` guards the `maps.Clone`. The elegance is that
`cmp.Or(envVal, fileVal, defaultVal)` encodes the whole precedence in one
expression, reading highest-to-lowest priority. And because the environment is
injected rather than read from `os`, the entire precedence contract is verified in
parallel — the same purity dividend from Exercise 6, now applied to a multi-source
resolver.

## Resources

- [cmp.Or](https://pkg.go.dev/cmp#Or) — returns the first non-zero argument, ideal for layered precedence.
- [maps.Clone](https://pkg.go.dev/maps#Clone) — copy the defaults so the resolver never mutates its input.
- [The Twelve-Factor App: Config](https://12factor.net/config) — the environment as the highest-priority override.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-env-var-expansion-dsn.md](07-env-var-expansion-dsn.md) | Next: [09-init-time-capture-pitfall.md](09-init-time-capture-pitfall.md)
