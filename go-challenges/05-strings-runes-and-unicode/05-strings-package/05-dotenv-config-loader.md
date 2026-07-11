# Exercise 5: Load KEY=VALUE Configuration From a .env-Style File

Twelve-factor config lands in the process as `KEY=VALUE` lines, often from a
`.env` file in development. The loader must skip comments and blank lines, honor
an optional `export` prefix, split on the *first* `=` so a value can contain `=`
(a database URL with a query string), and reject a line that has no `=` or an
empty key.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
dotenv/                         independent module: example.com/dotenv
  go.mod                        go 1.26
  dotenv.go                     ParseEnv(io.Reader) -> (map, err)
  dotenv_test.go                golden test over a multi-line fixture
  cmd/
    demo/
      main.go                   runnable demo over an embedded fixture
```

Files: `dotenv.go`, `dotenv_test.go`, `cmd/demo/main.go`.
Implement: `ParseEnv(r io.Reader) (map[string]string, error)`.
Test: comments, blank lines, plain assignment, a value containing `=`, a quoted
value, an `export`-prefixed line, a malformed line with no `=` (error), and
duplicate keys (last wins).
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/dotenv/cmd/demo
cd ~/go-exercises/dotenv
go mod init example.com/dotenv
```

### Reading line by line, splitting on the first =

`bufio.Scanner` over the `io.Reader` gives one line at a time (accepting a
`strings.NewReader` in tests and an `*os.File` in production without changing the
signature). Each line is `TrimSpace`d; a blank line or one that now starts with
`#` is a comment and is skipped. `strings.CutPrefix(line, "export ")` strips the
optional shell `export` keyword and its bool tells you whether it was there —
cleaner than `HasPrefix` followed by `TrimPrefix`.

The assignment itself is `strings.Cut(line, "=")`: split on the *first* `=`. This
is the whole reason to prefer `Cut` over `strings.Split(line, "=")`.
`DATABASE_URL=postgres://u:p@h/db?x=1` contains two `=`; `Split` would return
three pieces and lose the query string, while `Cut` returns the key and the
entire rest of the line as the value. `Cut`'s `found` bool distinguishes a line
with no `=` at all (malformed) from `KEY=` (present, empty value, which is
legal). An empty key after trimming is rejected. One optional layer of single or
double quotes is stripped from the value. Duplicate keys resolve last-wins, the
same way a later `export` overrides an earlier one in a shell.

Create `dotenv.go`:

```go
package dotenv

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// ErrNoEquals means a non-comment line had no '=' separator.
var ErrNoEquals = fmt.Errorf("dotenv: line has no '='")

// ErrEmptyKey means a line assigned to an empty key.
var ErrEmptyKey = fmt.Errorf("dotenv: empty key")

// ParseEnv reads KEY=VALUE lines from r, skipping blanks and '#' comments, and
// returns the resulting environment. Later duplicate keys overwrite earlier.
func ParseEnv(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "export "); ok {
			line = strings.TrimSpace(rest)
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			return nil, fmt.Errorf("line %d %q: %w", lineNo, line, ErrNoEquals)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("line %d: %w", lineNo, ErrEmptyKey)
		}
		value = strings.TrimSpace(value)
		value = unquote(value)
		out[key] = value
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// unquote strips one matching layer of single or double quotes.
func unquote(s string) string {
	if len(s) < 2 {
		return s
	}
	if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
		return s[1 : len(s)-1]
	}
	return s
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sort"
	"strings"

	"example.com/dotenv"
)

const fixture = `# database
export DATABASE_URL=postgres://u:p@h/db?x=1
PORT=8080
GREETING="hello world"
`

func main() {
	env, err := dotenv.ParseEnv(strings.NewReader(fixture))
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("%s=%q\n", k, env[k])
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
DATABASE_URL="postgres://u:p@h/db?x=1"
GREETING="hello world"
PORT="8080"
```

### Tests

Create `dotenv_test.go`:

```go
package dotenv

import (
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"
)

func TestParseEnvGolden(t *testing.T) {
	t.Parallel()

	fixture := strings.Join([]string{
		"# a comment",
		"",
		"   ",
		"PORT=8080",
		"DATABASE_URL=postgres://u:p@h/db?x=1",
		`GREETING="hello world"`,
		"export TOKEN=abc",
		"PORT=9090",
	}, "\n")

	want := map[string]string{
		"PORT":         "9090", // last wins
		"DATABASE_URL": "postgres://u:p@h/db?x=1",
		"GREETING":     "hello world",
		"TOKEN":        "abc",
	}

	got, err := ParseEnv(strings.NewReader(fixture))
	if err != nil {
		t.Fatalf("ParseEnv: %v", err)
	}
	if !maps.Equal(got, want) {
		t.Fatalf("ParseEnv = %v, want %v", got, want)
	}
}

func TestParseEnvErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		wantErr error
	}{
		{name: "no equals", in: "no_equals_here", wantErr: ErrNoEquals},
		{name: "empty key", in: "=value", wantErr: ErrEmptyKey},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseEnv(strings.NewReader(tc.in))
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ParseEnv(%q) err = %v, want %v", tc.in, err, tc.wantErr)
			}
		})
	}
}

func TestParseEnvEmptyValueIsLegal(t *testing.T) {
	t.Parallel()

	got, err := ParseEnv(strings.NewReader("EMPTY="))
	if err != nil {
		t.Fatalf("ParseEnv: %v", err)
	}
	if v, ok := got["EMPTY"]; !ok || v != "" {
		t.Fatalf("EMPTY = (%q, %v), want (\"\", true)", v, ok)
	}
}

func ExampleParseEnv() {
	env, _ := ParseEnv(strings.NewReader("export DB_URL=postgres://h/db?x=1"))
	fmt.Println(env["DB_URL"])
	// Output: postgres://h/db?x=1
}
```

## Review

The loader is correct when a value that contains `=` survives (proving the split
is on the first `=`, not every `=`), when `KEY=` yields an empty string rather
than an error, and when a line with no `=` is a hard error. The two structural
traps: using `strings.Split(line, "=")` (which loses everything after the second
`=`) and using `HasPrefix`+`TrimPrefix` for the `export` keyword where
`CutPrefix` says in one call whether it was there. Confirm with `go test -race`;
production loaders add variable interpolation (`${OTHER}`) and multi-line values,
which build on exactly this skeleton.

## Resources

- [strings.Cut](https://pkg.go.dev/strings#Cut) and [strings.CutPrefix](https://pkg.go.dev/strings#CutPrefix).
- [bufio.Scanner](https://pkg.go.dev/bufio#Scanner) — line-oriented reading over any io.Reader.
- [The Twelve-Factor App: Config](https://12factor.net/config) — why config is environment, not code.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-logfmt-line-parser.md](04-logfmt-line-parser.md) | Next: [06-url-path-prefix-router.md](06-url-path-prefix-router.md)
