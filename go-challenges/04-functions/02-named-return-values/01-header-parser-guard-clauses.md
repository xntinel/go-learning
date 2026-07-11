# Exercise 1: Auth Header Parser with Guard-Clause Named Returns

Parsing an `Authorization` header line is a task every backend touches: split
`Bearer abc123` into a scheme and a token, reject the malformed shapes, and do it
in code a reviewer can scan at a glance. This exercise builds that parser and uses
named returns for exactly one reason — to make the guard clauses read
top-to-bottom — while deliberately using explicit returns in the batch helper to
show the contrast.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
headparse/                        independent module: example.com/headparse
  go.mod
  internal/hparse/
    parser.go                     AuthHeader; Parse (named-return guards); ParseMany (explicit returns)
  cmd/demo/
    main.go                       runnable demo: parse one, batch, stop-on-first-error
  internal/hparse/parser_test.go  table-driven accept/reject, tab separator, ParseMany contract
```

- Files: `internal/hparse/parser.go`, `cmd/demo/main.go`, `internal/hparse/parser_test.go`.
- Implement: `Parse(raw string) (auth AuthHeader, err error)` with named-return guard clauses, and `ParseMany([]string) ([]AuthHeader, error)` with explicit returns and stop-on-first-error.
- Test: valid header, token-with-interior-spaces, tab separator, a rejection table asserted with `errors.Is`, plus `ParseMany` aggregation and stop-on-first-error.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/headparse/internal/hparse ~/go-exercises/headparse/cmd/demo
cd ~/go-exercises/headparse
go mod init example.com/headparse
```

### Why named returns here, and why not in ParseMany

`Parse` has three exit shapes: empty input, malformed input, and success. Naming
the results `(auth AuthHeader, err error)` lets each guard clause read as a plain
sentence in order — "if empty, that error; if it does not split, that error; if a
half is missing, that error; otherwise the parsed value." The function is short
enough that the names fully describe the result, which is the one situation where
naming buys readability. It is not using a deferred closure; it is using named
returns purely for the guard-clause shape, which is the mildest and most common
justification.

`ParseMany` deliberately does *not* name its returns. It is a short loop whose
`return nil, err` and `return out, nil` are clearer stated explicitly than a naked
return would be. Putting the two side by side is the lesson: named returns are a
tool for a shape, not a mandate.

A real-world subtlety drives the split logic. A credential can contain interior
spaces, and RFC 7230 allows a tab as the separator between the scheme and the
credential. So the parser splits on the *first* run of a space or tab and keeps
the entire remainder as the token — `strings.IndexAny(raw, " \t")` finds that
boundary, and `raw[i+1:]` preserves everything after it. A naive
`strings.SplitN(raw, " ", 2)` would reject a tab-separated header outright; the
`IndexAny` form is the production-correct choice. Both the scheme and the token
must be non-empty, which rejects `" abc"` (missing scheme), `"Bearer "` (missing
token), and `"   "` (whitespace only).

Create `internal/hparse/parser.go`:

```go
package hparse

import (
	"errors"
	"strings"
)

// Sentinel errors let callers branch on the failure kind with errors.Is.
var (
	ErrEmpty     = errors.New("empty header")
	ErrMalformed = errors.New("malformed header")
)

// AuthHeader is a parsed Authorization header line.
type AuthHeader struct {
	Scheme string
	Token  string
}

// Parse splits an Authorization header line ("Bearer abc123") into its scheme and
// token. It splits on the first space or tab and keeps the remainder as the
// token, so a credential with interior spaces and a tab-separated header both
// parse. The named results let the guard clauses read top-to-bottom.
func Parse(raw string) (auth AuthHeader, err error) {
	if raw == "" {
		return AuthHeader{}, ErrEmpty
	}
	i := strings.IndexAny(raw, " \t")
	if i < 0 {
		return AuthHeader{}, ErrMalformed
	}
	scheme, token := raw[:i], raw[i+1:]
	if scheme == "" || token == "" {
		return AuthHeader{}, ErrMalformed
	}
	return AuthHeader{Scheme: scheme, Token: token}, nil
}

// ParseMany parses a batch, stopping on the first error. It uses explicit returns
// on purpose: the function is short and "return nil, err" is clearer here than a
// naked return would be.
func ParseMany(raws []string) ([]AuthHeader, error) {
	out := make([]AuthHeader, 0, len(raws))
	for _, raw := range raws {
		auth, err := Parse(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, auth)
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

	"example.com/headparse/internal/hparse"
)

func main() {
	h, err := hparse.Parse("Bearer abc123")
	if err != nil {
		fmt.Println("parse error:", err)
		return
	}
	fmt.Printf("parsed: %+v\n", h)

	batch, err := hparse.ParseMany([]string{"Bearer abc", "Token def"})
	if err != nil {
		fmt.Println("batch error:", err)
		return
	}
	fmt.Printf("batch of %d: %+v\n", len(batch), batch)

	_, err = hparse.ParseMany([]string{"Bearer abc", "", "Token def"})
	fmt.Println("stops on first error:", errors.Is(err, hparse.ErrEmpty))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
parsed: {Scheme:Bearer Token:abc123}
batch of 2: [{Scheme:Bearer Token:abc} {Scheme:Token Token:def}]
stops on first error: true
```

### Tests

The table asserts both halves of the contract: what parses and what is rejected.
`TestParseKeepsRemainder` proves interior spaces survive; `TestParseTabSeparator`
proves the tab-separated form is accepted (the reason for `IndexAny` over a
space-only `SplitN`); the rejection table asserts each failure with `errors.Is`
against the sentinels; and the `ParseMany` tests pin aggregation and the
stop-on-first-error contract.

Create `internal/hparse/parser_test.go`:

```go
package hparse

import (
	"errors"
	"fmt"
	"testing"
)

func TestParseValid(t *testing.T) {
	t.Parallel()

	got, err := Parse("Bearer abc123")
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	if got.Scheme != "Bearer" || got.Token != "abc123" {
		t.Fatalf("Parse = %+v, want {Bearer abc123}", got)
	}
}

func TestParseKeepsRemainder(t *testing.T) {
	t.Parallel()

	got, err := Parse("Token hello world here")
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	if got.Scheme != "Token" || got.Token != "hello world here" {
		t.Fatalf("Parse = %+v, want token with interior spaces", got)
	}
}

func TestParseTabSeparator(t *testing.T) {
	t.Parallel()

	got, err := Parse("Bearer\tabc123")
	if err != nil {
		t.Fatalf("Parse: unexpected error on tab separator: %v", err)
	}
	if got.Scheme != "Bearer" || got.Token != "abc123" {
		t.Fatalf("Parse = %+v, want {Bearer abc123}", got)
	}
}

func TestParseRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		wantErr error
	}{
		{name: "empty", input: "", wantErr: ErrEmpty},
		{name: "no separator", input: "Bearer", wantErr: ErrMalformed},
		{name: "missing token", input: "Bearer ", wantErr: ErrMalformed},
		{name: "missing scheme", input: " abc123", wantErr: ErrMalformed},
		{name: "whitespace only", input: "   ", wantErr: ErrMalformed},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := Parse(tc.input)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Parse(%q) err = %v, want %v", tc.input, err, tc.wantErr)
			}
		})
	}
}

func TestParseManyAggregates(t *testing.T) {
	t.Parallel()

	got, err := ParseMany([]string{"Bearer abc", "Token def"})
	if err != nil {
		t.Fatalf("ParseMany: unexpected error: %v", err)
	}
	if len(got) != 2 || got[0].Scheme != "Bearer" || got[1].Scheme != "Token" {
		t.Fatalf("ParseMany = %+v, want two headers", got)
	}
}

func TestParseManyStopsOnFirstError(t *testing.T) {
	t.Parallel()

	_, err := ParseMany([]string{"Bearer abc", "", "Token def"})
	if !errors.Is(err, ErrEmpty) {
		t.Fatalf("ParseMany err = %v, want ErrEmpty", err)
	}
}

func ExampleParse() {
	h, _ := Parse("Bearer abc123")
	fmt.Printf("%s / %s\n", h.Scheme, h.Token)
	// Output: Bearer / abc123
}
```

## Review

The parser is correct when it is a pure function of its input: a scheme and token
split on the first space-or-tab, both non-empty, remainder preserved, and each
rejection mapped to the right sentinel so `errors.Is` sees through it. The named
results earn their place only by making the guard clauses legible; the moment the
function grew long enough that a naked return became unclear, explicit returns
would be the better call — which is precisely why `ParseMany` uses them. The
mistake to avoid is naming a result for "documentation" when the body sets it once
from a single expression: that adds nothing an explicit `return value, nil` would
not. Run `go test -race` to confirm the whole thing under the race detector, even
though this parser holds no shared state.

## Resources

- [Go Spec: Function declarations](https://go.dev/ref/spec#Function_declarations)
- [`strings.IndexAny`](https://pkg.go.dev/strings#IndexAny)
- [Effective Go: Named result parameters](https://go.dev/doc/effective_go#named-results)

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-defer-error-wrapping-repository.md](02-defer-error-wrapping-repository.md)
