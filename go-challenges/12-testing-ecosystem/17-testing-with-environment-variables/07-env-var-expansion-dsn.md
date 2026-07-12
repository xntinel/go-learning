# Exercise 7: Expand ${VAR} References To Build A Database DSN Template

Composing a connection string from parts is a twelve-factor staple. This exercise
builds a `BuildDSN` that expands a `${VAR}` template with `os.Expand` and a custom
mapping that treats every undefined variable as an error — instead of
`os.ExpandEnv`'s silent empty-string substitution, which yields a valid-looking
but broken DSN.

## What you'll build

```text
dsnbuild/                  independent module: example.com/dsnbuild
  go.mod                   go directive supplied by the gate
  dsn.go                   BuildDSN(template); ErrUndefinedVar; the custom mapping
  cmd/
    demo/
      main.go              runnable demo: full expansion, then a missing var
  dsn_test.go              full-DSN assertion; missing-var naming; ExpandEnv trap contrast
```

Files: `dsn.go`, `cmd/demo/main.go`, `dsn_test.go`.
Implement: `BuildDSN(template string) (string, error)` using `os.Expand` with a mapping that records undefined variables and returns them joined as `ErrUndefinedVar`.
Test: set the `DB_*` vars and assert the expanded DSN; unset one and assert `BuildDSN` names it; contrast `os.ExpandEnv`'s silent empty substitution.
Verify: `go test -count=1 -race ./...`

## `os.Expand` with a mapping that fails loudly

`os.Expand(s, mapping)` walks `s`, and for each `${name}` (or `$name`) reference
calls `mapping(name)` and substitutes the result. `os.ExpandEnv(s)` is the
convenience form that hardcodes `os.Getenv` as the mapping — and `os.Getenv`
returns `""` for an undefined variable, so `os.ExpandEnv` silently drops a missing
`${DB_HOST}` and produces `postgres://user:pass@:5432/app`. That string parses; it
just connects to the wrong place, and the failure surfaces far from its cause.

`BuildDSN` supplies its own mapping. From inside it, call `os.LookupEnv(name)`;
if `ok` is false, record the name in a `missing` slice and return `""` (the
substitution value is irrelevant on the error path). After `os.Expand` returns,
if `missing` is non-empty, build one wrapped error per missing name and combine
them with `errors.Join`, so the caller sees every undefined variable at once and
can match `ErrUndefinedVar` with `errors.Is`. Sorting the names first makes the
message deterministic, which matters for a test that asserts on it.

A detail worth noting: the mapping is called once per *reference*, so a template
that mentions `${DB_HOST}` twice would record it twice; deduplicating with a small
`seen` set keeps the reported list clean. This is a pure-ish function — it reads
the environment through `os.LookupEnv`, so its tests use `t.Setenv` and stay
serial. (You could inject the lookup exactly as in Exercise 6; here we keep it on
`os` to focus on the expansion mechanics.)

Create `dsn.go`:

```go
package dsnbuild

import (
	"errors"
	"fmt"
	"os"
	"slices"
)

// ErrUndefinedVar is returned when a template references a variable that is not
// set in the environment.
var ErrUndefinedVar = errors.New("undefined variable")

// BuildDSN expands ${VAR} references in template using the environment, and
// reports every undefined variable instead of silently substituting "".
func BuildDSN(template string) (string, error) {
	var missing []string
	seen := make(map[string]bool)

	expanded := os.Expand(template, func(name string) string {
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		if !seen[name] {
			seen[name] = true
			missing = append(missing, name)
		}
		return ""
	})

	if len(missing) > 0 {
		slices.Sort(missing)
		errs := make([]error, 0, len(missing))
		for _, name := range missing {
			errs = append(errs, fmt.Errorf("%s: %w", name, ErrUndefinedVar))
		}
		return "", errors.Join(errs...)
	}
	return expanded, nil
}

// ExpandSilently mirrors os.ExpandEnv: it substitutes "" for undefined variables
// with no error. Kept to demonstrate the trap BuildDSN avoids.
func ExpandSilently(template string) string {
	return os.ExpandEnv(template)
}
```

## The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"

	"example.com/dsnbuild"
)

const template = "postgres://${DB_USER}:${DB_PASS}@${DB_HOST}:${DB_PORT}/${DB_NAME}?sslmode=${DB_SSL}"

func main() {
	os.Setenv("DB_USER", "app")
	os.Setenv("DB_PASS", "s3cr3t")
	os.Setenv("DB_HOST", "db.internal")
	os.Setenv("DB_PORT", "5432")
	os.Setenv("DB_NAME", "orders")
	os.Setenv("DB_SSL", "require")

	dsn, err := dsnbuild.BuildDSN(template)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(dsn)

	os.Unsetenv("DB_HOST")
	if _, err := dsnbuild.BuildDSN(template); err != nil {
		fmt.Println("error:", err)
	}
	// The silent variant hides the missing host in a broken DSN:
	fmt.Println("silent:", dsnbuild.ExpandSilently(template))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
postgres://app:s3cr3t@db.internal:5432/orders?sslmode=require
error: DB_HOST: undefined variable
silent: postgres://app:s3cr3t@:5432/orders?sslmode=require
```

## Tests

`TestBuildDSNFull` sets all six `DB_*` variables and asserts the exact expanded
string. `TestBuildDSNMissing` unsets one and asserts `BuildDSN` returns
`ErrUndefinedVar` naming that variable, reporting every missing name when several
are absent. `TestExpandEnvTrap` is the contrast: with a variable unset,
`os.ExpandEnv` produces a DSN with an empty segment and no error — the exact
silent failure `BuildDSN` exists to prevent.

Create `dsn_test.go`:

```go
package dsnbuild

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

const tmpl = "postgres://${DB_USER}:${DB_PASS}@${DB_HOST}:${DB_PORT}/${DB_NAME}"

func setDBVars(t *testing.T) {
	t.Helper()
	t.Setenv("DB_USER", "app")
	t.Setenv("DB_PASS", "pw")
	t.Setenv("DB_HOST", "db.internal")
	t.Setenv("DB_PORT", "5432")
	t.Setenv("DB_NAME", "orders")
}

func TestBuildDSNFull(t *testing.T) {
	setDBVars(t)

	got, err := BuildDSN(tmpl)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "postgres://app:pw@db.internal:5432/orders"
	if got != want {
		t.Fatalf("BuildDSN() = %q, want %q", got, want)
	}
}

func TestBuildDSNMissing(t *testing.T) {
	setDBVars(t)
	// Remove one variable for the duration of this test.
	orig, had := os.LookupEnv("DB_HOST")
	os.Unsetenv("DB_HOST")
	t.Cleanup(func() {
		if had {
			os.Setenv("DB_HOST", orig)
		} else {
			os.Unsetenv("DB_HOST")
		}
	})

	_, err := BuildDSN(tmpl)
	if !errors.Is(err, ErrUndefinedVar) {
		t.Fatalf("BuildDSN() err = %v, want ErrUndefinedVar", err)
	}
	if !strings.Contains(err.Error(), "DB_HOST") {
		t.Fatalf("error %q does not name the missing variable", err)
	}
}

func TestBuildDSNReportsAllMissing(t *testing.T) {
	// None of the DB_* variables are set; expect every reference reported.
	for _, k := range []string{"DB_USER", "DB_PASS", "DB_HOST", "DB_PORT", "DB_NAME"} {
		orig, had := os.LookupEnv(k)
		os.Unsetenv(k)
		t.Cleanup(func() {
			if had {
				os.Setenv(k, orig)
			} else {
				os.Unsetenv(k)
			}
		})
	}

	_, err := BuildDSN(tmpl)
	for _, k := range []string{"DB_USER", "DB_PASS", "DB_HOST", "DB_PORT", "DB_NAME"} {
		if !strings.Contains(err.Error(), k) {
			t.Errorf("error %q missing %s", err.Error(), k)
		}
	}
}

func TestExpandEnvTrap(t *testing.T) {
	setDBVars(t)
	orig, had := os.LookupEnv("DB_HOST")
	os.Unsetenv("DB_HOST")
	t.Cleanup(func() {
		if had {
			os.Setenv("DB_HOST", orig)
		} else {
			os.Unsetenv("DB_HOST")
		}
	})

	// os.ExpandEnv silently drops the missing host: a broken but valid-looking DSN.
	got := ExpandSilently(tmpl)
	want := "postgres://app:pw@:5432/orders"
	if got != want {
		t.Fatalf("ExpandSilently() = %q, want %q (silent empty host)", got, want)
	}
}

func ExampleBuildDSN() {
	os.Setenv("DB_USER", "app")
	os.Setenv("DB_PASS", "pw")
	os.Setenv("DB_HOST", "db")
	os.Setenv("DB_PORT", "5432")
	os.Setenv("DB_NAME", "orders")

	dsn, _ := BuildDSN(tmpl)
	fmt.Println(dsn)
	// Output: postgres://app:pw@db:5432/orders
}
```

## Review

`BuildDSN` is correct when a fully-populated environment expands to the exact
target string and any missing reference produces `ErrUndefinedVar` naming every
absent variable — the `TestExpandEnvTrap` case standing as the concrete reason not
to use `os.ExpandEnv` for anything where a missing value must fail loudly. The
mechanism to internalize is that `os.Expand` lets you own the mapping, so
"undefined" becomes a recorded error rather than a silent `""`. Because the
mapping reads `os.LookupEnv`, these tests mutate the environment and stay serial;
the `os.Unsetenv` plus `(orig, had)` restore is the same hermetic-unset pattern
from Exercise 3.

## Resources

- [os.Expand](https://pkg.go.dev/os#Expand) — expansion with a caller-supplied mapping.
- [os.ExpandEnv](https://pkg.go.dev/os#ExpandEnv) — the convenience form that substitutes `""` for undefined variables.
- [errors.Join](https://pkg.go.dev/errors#Join) — reporting every missing variable in one error.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-getenv-injection-parallel.md](06-getenv-injection-parallel.md) | Next: [08-config-precedence-layering.md](08-config-precedence-layering.md)
