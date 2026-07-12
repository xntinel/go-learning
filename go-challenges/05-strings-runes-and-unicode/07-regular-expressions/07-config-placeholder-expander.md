# Exercise 7: Config Placeholder Expander (${VAR} Interpolation)

Config files interpolate environment: `dsn: postgres://${DB_HOST}:${DB_PORT:-5432}/app`.
A correct expander substitutes present variables, falls back on the `:-default`
form, and — crucially — reports the *required* variables that are missing instead
of silently emitting empty strings that produce a broken connection string at
runtime. This module builds it with `ReplaceAllStringFunc`, whose closure records
the missing names as it goes.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
configtmpl/                 independent module: example.com/configtmpl
  go.mod                    go 1.26
  configtmpl.go             placeholderRe; Expand via ReplaceAllStringFunc; ErrUnresolved
  cmd/
    demo/
      main.go               runnable demo: expand a config template
  configtmpl_test.go        table-driven: present, default, missing accumulates, adjacent, $$ literal
```

- Files: `configtmpl.go`, `cmd/demo/main.go`, `configtmpl_test.go`.
- Implement: `Expand(tmpl string, vars map[string]string) (string, error)` expanding `${VAR}` and `${VAR:-default}` via `ReplaceAllStringFunc`, accumulating unresolved required names into a wrapped `ErrUnresolved`.
- Test: a present variable is substituted; the `:-default` form falls back; missing required variables accumulate into one error listing the names; adjacent `${A}${B}` both expand; a literal `$$` is left alone.
- Verify: `go test -count=1 -race ./...`

### Why the closure of ReplaceAllStringFunc is the right shape

Expansion is a rewrite where each replacement depends on the match *and* on
external state (the vars map), and where the process must accumulate a side effect
(the list of missing required variables). That is precisely what a
`ReplaceAllStringFunc(src, func(match string) string)` closure gives you: it is
called once per `${...}` occurrence, it can look each name up, and it can append to
a `missing` slice captured from the enclosing scope. After the single pass, if
`missing` is non-empty, `Expand` returns the rewritten string alongside an error
that names every unresolved variable — one error for the whole template, not a
failure on the first missing name, so an operator fixes all of them at once.

The placeholder regex is `\$\{([^}]+)\}`: a literal `${`, a captured body of
anything but `}`, and a closing `}`. Because it requires the braces, a bare `$$`
or a lone `$` is not matched and passes through untouched — the "leave escaped
sequences alone" behavior. Inside the closure the body is split on `:-` to detect
the default form: `${PORT:-5432}` yields name `PORT` and default `5432`, while
`${HOST}` yields name `HOST` and no default. Lookup order is: present in the map,
use it; absent but a default is given, use the default; absent and required,
record it in `missing` and substitute empty (the value is meaningless anyway, since
the call will return an error).

Create `configtmpl.go`:

```go
package configtmpl

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrUnresolved is returned when one or more required variables have no value.
var ErrUnresolved = errors.New("unresolved variables")

// placeholderRe matches ${NAME} and ${NAME:-default}. It requires braces, so a
// bare "$$" or "$" is left untouched.
var placeholderRe = regexp.MustCompile(`\$\{([^}]+)\}`)

// Expand substitutes ${VAR} and ${VAR:-default} placeholders in tmpl from vars.
// Missing required variables are accumulated and returned as one ErrUnresolved.
func Expand(tmpl string, vars map[string]string) (string, error) {
	var missing []string
	out := placeholderRe.ReplaceAllStringFunc(tmpl, func(match string) string {
		body := placeholderRe.FindStringSubmatch(match)[1]
		name, def, hasDef := strings.Cut(body, ":-")
		if v, ok := vars[name]; ok {
			return v
		}
		if hasDef {
			return def
		}
		missing = append(missing, name)
		return ""
	})
	if len(missing) > 0 {
		return out, fmt.Errorf("%w: %s", ErrUnresolved, strings.Join(missing, ", "))
	}
	return out, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/configtmpl"
)

func main() {
	vars := map[string]string{"DB_HOST": "db.internal"}

	out, err := configtmpl.Expand("dsn=postgres://${DB_HOST}:${DB_PORT:-5432}/app", vars)
	fmt.Printf("expanded: %s\n", out)
	fmt.Printf("err: %v\n", err)

	_, err = configtmpl.Expand("token=${API_KEY} region=${REGION}", vars)
	fmt.Printf("required missing? %v -> %v\n", errors.Is(err, configtmpl.ErrUnresolved), err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
expanded: dsn=postgres://db.internal:5432/app
err: <nil>
required missing? true -> unresolved variables: API_KEY, REGION
```

### Tests

Create `configtmpl_test.go`:

```go
package configtmpl

import (
	"errors"
	"strings"
	"testing"
)

func TestExpand(t *testing.T) {
	t.Parallel()
	vars := map[string]string{"A": "1", "B": "2", "HOST": "db"}
	tests := []struct {
		name string
		tmpl string
		want string
	}{
		{name: "present", tmpl: "h=${HOST}", want: "h=db"},
		{name: "default used", tmpl: "p=${PORT:-5432}", want: "p=5432"},
		{name: "default ignored when present", tmpl: "a=${A:-9}", want: "a=1"},
		{name: "adjacent", tmpl: "${A}${B}", want: "12"},
		{name: "dollar-dollar left alone", tmpl: "cost=$$5 and ${A}", want: "cost=$$5 and 1"},
		{name: "lone dollar left alone", tmpl: "100$ ${B}", want: "100$ 2"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Expand(tc.tmpl, vars)
			if err != nil {
				t.Fatalf("Expand(%q) error: %v", tc.tmpl, err)
			}
			if got != tc.want {
				t.Fatalf("Expand(%q) = %q, want %q", tc.tmpl, got, tc.want)
			}
		})
	}
}

func TestExpandAccumulatesMissing(t *testing.T) {
	t.Parallel()
	_, err := Expand("${X} ${Y} ${Z:-ok}", map[string]string{})
	if !errors.Is(err, ErrUnresolved) {
		t.Fatalf("err = %v, want ErrUnresolved", err)
	}
	// Both required names appear; the one with a default does not.
	for _, name := range []string{"X", "Y"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("error %q missing name %q", err, name)
		}
	}
	if strings.Contains(err.Error(), "Z") {
		t.Fatalf("error %q should not list Z (has default)", err)
	}
}

func TestExpandReturnsPartialOutput(t *testing.T) {
	t.Parallel()
	// Even on error, the resolved parts are expanded (the closure recorded the gap).
	out, err := Expand("a=${A} b=${MISSING}", map[string]string{"A": "1"})
	if !errors.Is(err, ErrUnresolved) {
		t.Fatalf("err = %v, want ErrUnresolved", err)
	}
	if out != "a=1 b=" {
		t.Fatalf("partial output = %q, want %q", out, "a=1 b=")
	}
}
```

## Review

The expander is correct when substitution, defaulting, and error accumulation all
happen in one pass. `ReplaceAllStringFunc`'s closure is what makes that possible:
it resolves each `${...}` against the map and records the required ones it cannot
resolve into a captured `missing` slice, so `TestExpandAccumulatesMissing` proves a
single error lists *every* missing required name — and omits the one with a
`:-default`. The `strings.Cut` split cleanly separates the name from its default.
Because the regex requires braces, `$$` and a lone `$` pass through, which the
table pins. The failure mode this design prevents is the silent one: substituting
an empty string for a missing required variable and shipping a broken config;
`TestExpandReturnsPartialOutput` shows the resolved parts still expand while the
error flags the gap. Run `go test -race` since the package-level regex is shared.

## Resources

- [`regexp` package](https://pkg.go.dev/regexp) — `ReplaceAllStringFunc` and its closure, plus `ExpandString` for the `$`-template alternative.
- [`strings.Cut`](https://pkg.go.dev/strings#Cut) — splitting the `${NAME:-default}` body once.
- [`os.Expand`](https://pkg.go.dev/os#Expand) — the stdlib's own `${var}` expander, worth comparing to this hand-built one.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-semver-tag-validator.md](06-semver-tag-validator.md) | Next: [08-streaming-log-grep.md](08-streaming-log-grep.md)
