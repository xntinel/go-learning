# Exercise 20: Environment Variable Lookup With Fallback Tracking

**Nivel: Intermedio** — validacion rapida (un test corto).

"Why is this setting the value it is" is one of the most common support
questions in any service with layered config — an operator sets an env
var, nothing changes, and nobody can tell whether the env var was even
read. This exercise builds `Lookup(key) (value string, found bool, source
string)`, checking the process environment before a defaults map and
reporting which layer actually answered, with the environment access
itself injected so tests never touch (or race on) the real process
environment.

This module is fully self-contained: its own `go mod init`, all code
inline, one quick test file.

## What you'll build

```text
envlookup/                   independent module: example.com/env-var-lookup-with-source
  go.mod                     go 1.24
  envlookup.go                package envlookup; Resolver, envKeyFor, Lookup(key) (value,found,source)
  cmd/
    demo/
      main.go                 real os.LookupEnv wrapped through NewResolver, one var set, one defaulted, one missing
  envlookup_test.go           table test over env/default/missing, backed by a fake lookupEnv; a naming-convention test
```

- Files: `envlookup.go`, `cmd/demo/main.go`, `envlookup_test.go`.
- Implement: `(*Resolver).Lookup(key string) (value string, found bool, source string)`, deriving the environment variable name from a dotted config key (`database.host` -> `APP_DATABASE_HOST`) and checking it via an injected `lookupEnv func(string) (string, bool)` before falling back to a defaults map.
- Test: a key present in the (faked) environment reports `source == "env"`; a key present only in defaults reports `source == "default"`; a key present nowhere reports `found == false, source == ""`.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/envlookup/cmd/demo
cd ~/go-exercises/envlookup
go mod init example.com/env-var-lookup-with-source
go mod edit -go=1.24
```

### Inject the environment the same way you'd inject a clock

`os.LookupEnv` reads real, global, process-wide state — exactly the kind
of dependency this curriculum has been teaching you to inject rather than
call directly, the same reasoning behind passing a clock instead of
calling `time.Now()`. A test that calls `os.Setenv` before asserting on
`Lookup` works, but it mutates real process state that leaks across
parallel tests (`t.Parallel()` and `os.Setenv` do not mix safely) and it
cannot represent "this variable is unset" without first making sure
nothing else set it. Injecting `lookupEnv func(string) (string, bool)` —
the exact signature of `os.LookupEnv` — sidesteps all of that: production
code passes `os.LookupEnv` (or `nil`, which `NewResolver` treats as
"use the real one"), tests pass a small map-backed fake, and neither one
touches the other's state:

```go
func NewResolver(prefix string, defaults map[string]string, lookupEnv func(string) (string, bool)) *Resolver {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	return &Resolver{prefix: prefix, defaults: defaults, lookupEnv: lookupEnv}
}
```

The three-value return then does the same audit-trail job `source` does
throughout this lesson: `found` says whether a value exists at all,
`source` says which layer produced it, distinguishing "the operator's env
var won" from "nobody set anything, this is just the default".

Create `envlookup.go`:

```go
package envlookup

import (
	"os"
	"strings"
)

// Resolver looks up config keys against the process environment first,
// falling back to a defaults map, while recording which layer answered.
type Resolver struct {
	prefix    string
	defaults  map[string]string
	lookupEnv func(string) (string, bool)
}

// NewResolver builds a Resolver. prefix is prepended to the derived
// environment variable name (e.g. "APP_"). lookupEnv is the injectable
// dependency: pass os.LookupEnv in production, a fake map-backed function
// in tests, so a test never depends on (or mutates) the real process
// environment.
func NewResolver(prefix string, defaults map[string]string, lookupEnv func(string) (string, bool)) *Resolver {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	return &Resolver{prefix: prefix, defaults: defaults, lookupEnv: lookupEnv}
}

// envKeyFor converts a dotted config key like "database.host" into the
// environment variable name convention: uppercased, dots replaced with
// underscores, prefixed — "database.host" becomes "APP_DATABASE_HOST".
func envKeyFor(prefix, key string) string {
	return prefix + strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
}

// Lookup resolves key, checking the environment before the defaults map,
// and reports which layer supplied the value: "env", "default", or "" when
// neither has it. This lets an operator debugging "why is this setting
// what it is" see the answer without grepping through both layers by hand.
func (r *Resolver) Lookup(key string) (value string, found bool, source string) {
	envKey := envKeyFor(r.prefix, key)
	if v, ok := r.lookupEnv(envKey); ok {
		return v, true, "env"
	}
	if v, ok := r.defaults[key]; ok {
		return v, true, "default"
	}
	return "", false, ""
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"

	"example.com/env-var-lookup-with-source"
)

func main() {
	os.Setenv("APP_DATABASE_HOST", "10.0.0.5")
	defer os.Unsetenv("APP_DATABASE_HOST")

	defaults := map[string]string{
		"database.host": "localhost",
		"database.port": "5432",
	}
	resolver := envlookup.NewResolver("APP_", defaults, nil) // nil -> real os.LookupEnv

	for _, key := range []string{"database.host", "database.port", "database.name"} {
		value, found, source := resolver.Lookup(key)
		fmt.Printf("%-14s value=%-10q found=%-5t source=%q\n", key, value, found, source)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
database.host  value="10.0.0.5" found=true  source="env"
database.port  value="5432"     found=true  source="default"
database.name  value=""         found=false source=""
```

### Test

Create `envlookup_test.go`:

```go
package envlookup

import "testing"

// fakeEnv builds a lookupEnv function backed by a plain map, so tests never
// touch the real process environment.
func fakeEnv(vars map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	}
}

func TestLookup(t *testing.T) {
	t.Parallel()

	env := fakeEnv(map[string]string{"APP_DATABASE_HOST": "10.0.0.5"})
	defaults := map[string]string{
		"database.host": "localhost",
		"database.port": "5432",
	}
	resolver := NewResolver("APP_", defaults, env)

	tests := []struct {
		name       string
		key        string
		wantValue  string
		wantFound  bool
		wantSource string
	}{
		{"env wins over default", "database.host", "10.0.0.5", true, "env"},
		{"falls back to default", "database.port", "5432", true, "default"},
		{"missing everywhere", "database.name", "", false, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			value, found, source := resolver.Lookup(tc.key)
			if value != tc.wantValue || found != tc.wantFound || source != tc.wantSource {
				t.Fatalf("Lookup(%q) = (%q, %t, %q), want (%q, %t, %q)",
					tc.key, value, found, source, tc.wantValue, tc.wantFound, tc.wantSource)
			}
		})
	}
}

func TestEnvKeyForConvention(t *testing.T) {
	t.Parallel()
	got := envKeyFor("APP_", "database.host")
	want := "APP_DATABASE_HOST"
	if got != want {
		t.Fatalf("envKeyFor = %q, want %q", got, want)
	}
}
```

## Review

`Lookup` is correct when the environment always wins over the defaults map
whenever the (faked) environment has the key, and when `source` names
exactly which layer answered rather than just reporting `found`.
`TestLookup`'s three cases are deliberately independent keys rather than
one key checked three ways, so the test also demonstrates the env-vs-default
priority without needing to unset anything between cases — a benefit of
never touching real process state in the first place.

The mistake to avoid is calling `os.LookupEnv` directly inside `Lookup`
instead of through the injected field — that one line change is the
difference between a test suite that can run `t.Parallel()` freely and one
that silently corrupts itself the moment two tests both try to set the
same environment variable.

## Resources

- [os.LookupEnv](https://pkg.go.dev/os#LookupEnv) — the exact `(string, bool)` signature this resolver's injected dependency mirrors.
- [Testing: avoid os.Setenv in parallel tests](https://pkg.go.dev/testing#T.Setenv) — `T.Setenv` exists precisely because `os.Setenv` and `t.Parallel()` do not mix; injecting the lookup avoids needing either.
- [The Twelve-Factor App: Config](https://12factor.net/config) — the environment-variables-as-config convention this resolver's naming scheme follows.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [19-dns-resolver-with-ttl.md](19-dns-resolver-with-ttl.md) | Next: [21-protobuf-field-extract-typed.md](21-protobuf-field-extract-typed.md)
