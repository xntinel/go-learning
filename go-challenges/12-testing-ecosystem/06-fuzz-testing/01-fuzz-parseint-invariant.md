# Exercise 1: Fuzz An Integer Parser For A Config Loader

A config loader turns strings from the environment into typed values, and the
integer parser at its center is a small perimeter: it sees whatever an operator
or an orchestrator puts in a variable. This module builds that `ParseInt`, keeps
the deterministic table tests that pin its contract, and adds `FuzzParseInt`,
which encodes the invariant that a well-formed numeric string never comes back
as an error.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
configint/                 independent module: example.com/configint
  go.mod                   module path
  parse.go                 ErrInvalid; ParseInt(string) (int, error); LoadPort(map)
  cmd/
    demo/
      main.go              loads a port from an env-like map, prints the result
  parse_test.go            TestParseIntSuccess, TestParseIntRejectsInvalid, FuzzParseInt, Example
```

Files: `parse.go`, `cmd/demo/main.go`, `parse_test.go`.
Implement: `ParseInt(s string) (int, error)` returning `ErrInvalid` (wrapped with
`%w`) for empty or non-numeric input, and `LoadPort` using it.
Test: table tests for accept and reject; `FuzzParseInt` asserting the numeric
invariant; an `Example`.
Verify: `go test -race ./...`, then a bounded `go test -fuzz=FuzzParseInt
-fuzztime=2s`.

Set up the module:

```bash
mkdir -p ~/go-exercises/configint/cmd/demo
cd ~/go-exercises/configint
go mod init example.com/configint
```

### The parser and the invariant it must satisfy

`ParseInt` wraps `strconv.Atoi`: an empty string or anything `Atoi` rejects comes
back as `ErrInvalid`, wrapped with `%w` so a caller can match it with
`errors.Is`. The config loader on top (`LoadPort`) reads a variable, parses it,
and range-checks the port. That is the real shape: a thin typed accessor over an
untrusted string.

The fuzz invariant needs care, and the care is the lesson. The tempting property
is "any string of digits parses without error" — but that is *false*, and a
fuzzer finds the counterexample in milliseconds: a string of forty `9`s looks
numeric yet overflows a 64-bit `int`, so `Atoi` correctly returns a range error.
An overflowing numeric literal *should* be rejected. So the honest, fuzzer-proof
invariant is narrower: a string that is an optional single leading `-` followed
by one to fifteen ASCII digits (fifteen digits max is `999999999999999`, safely
inside `int64`) must parse without error. `FuzzParseInt` computes that predicate
on the generated string and only then asserts `err == nil`. This is an invariant
property with a deliberately-bounded precondition — the discipline that keeps a
fuzz assertion true for *every* input, not just the ones you pictured.

Create `parse.go`:

```go
package configint

import (
	"errors"
	"fmt"
	"strconv"
)

// ErrInvalid is returned (wrapped) when a string is not a valid integer.
var ErrInvalid = errors.New("invalid integer")

// ParseInt parses s as a base-10 integer. Empty or non-numeric input yields a
// value wrapping ErrInvalid, so callers match it with errors.Is.
func ParseInt(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty string: %w", ErrInvalid)
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("parse %q: %w", s, ErrInvalid)
	}
	return n, nil
}

// LoadPort reads key from env and parses it as a TCP port in [1, 65535].
func LoadPort(env map[string]string, key string) (int, error) {
	raw, ok := env[key]
	if !ok {
		return 0, fmt.Errorf("missing %s: %w", key, ErrInvalid)
	}
	n, err := ParseInt(raw)
	if err != nil {
		return 0, err
	}
	if n < 1 || n > 65535 {
		return 0, fmt.Errorf("port %d out of range: %w", n, ErrInvalid)
	}
	return n, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/configint"
)

func main() {
	env := map[string]string{"PORT": "8443", "WORKERS": "twelve"}

	port, err := configint.LoadPort(env, "PORT")
	fmt.Printf("PORT -> %d (err=%v)\n", port, err)

	_, err = configint.LoadPort(env, "MISSING")
	fmt.Printf("MISSING -> err is ErrInvalid: %v\n", errors.Is(err, configint.ErrInvalid))

	_, err = configint.ParseInt(env["WORKERS"])
	fmt.Printf("WORKERS=%q -> err=%v\n", env["WORKERS"], err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
PORT -> 8443 (err=<nil>)
MISSING -> err is ErrInvalid: true
WORKERS="twelve" -> err=parse "twelve": invalid integer
```

### Tests

The table tests are the deterministic contract: known-good strings parse to known
values, and known-bad strings all wrap `ErrInvalid`. `FuzzParseInt` seeds the
corpus with representative strings, then, for each generated input, decides
whether the string is safely numeric and asserts the parser agrees.

Create `parse_test.go`:

```go
package configint

import (
	"errors"
	"fmt"
	"testing"
)

func TestParseIntSuccess(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want int
	}{
		{"0", 0},
		{"42", 42},
		{"-1", -1},
		{"100000", 100000},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := ParseInt(tc.in)
			if err != nil {
				t.Fatalf("ParseInt(%q) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ParseInt(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseIntRejectsInvalid(t *testing.T) {
	t.Parallel()
	cases := []string{"", "abc", "12.5", "1a", "--3", "0x10"}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			t.Parallel()
			_, err := ParseInt(c)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("ParseInt(%q) err = %v, want ErrInvalid", c, err)
			}
		})
	}
}

// safelyNumeric reports whether s is an optional single leading '-' followed by
// one to fifteen ASCII digits: a form guaranteed to fit in int64, so ParseInt
// must accept it. Anything longer may legitimately overflow and be rejected.
func safelyNumeric(s string) bool {
	if s == "" {
		return false
	}
	if s[0] == '-' {
		s = s[1:]
	}
	if len(s) == 0 || len(s) > 15 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func FuzzParseInt(f *testing.F) {
	f.Add("42")
	f.Add("-1")
	f.Add("0")
	f.Add("999999999")
	f.Add("abc")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		_, err := ParseInt(s)
		if safelyNumeric(s) && err != nil {
			t.Fatalf("ParseInt(%q) = error %v for a safely-numeric string", s, err)
		}
		if err != nil && !errors.Is(err, ErrInvalid) {
			t.Fatalf("ParseInt(%q) returned %v, not wrapping ErrInvalid", s, err)
		}
	})
}

func Example() {
	n, _ := ParseInt("8443")
	_, err := ParseInt("nope")
	fmt.Println(n, errors.Is(err, ErrInvalid))
	// Output: 8443 true
}
```

## Review

The parser is correct when its accept/reject decision is exactly `strconv.Atoi`'s
plus the empty-string case, and when every rejection wraps `ErrInvalid` so
callers match with `errors.Is` rather than string comparison. The fuzz proof is
that `safelyNumeric(s) ⟹ err == nil` holds for every generated `s`; the reason
that predicate is bounded to fifteen digits is the whole point — a wider claim
would be false for overflowing literals, and the fuzzer would find that in one
run. The most common beginner mistake here is exactly that over-broad invariant.
Run `go test -race ./...` for the deterministic contract, then
`go test -fuzz=FuzzParseInt -fuzztime=2s`; it must reach `PASS` with no failing
input written under `testdata/fuzz`.

## Resources

- [`testing.F`](https://pkg.go.dev/testing#F) — `Add`, `Fuzz`, and the allowed argument types.
- [Go Tutorial: Getting started with fuzzing](https://go.dev/doc/tutorial/fuzz) — the parser-fuzzing walkthrough this exercise generalizes.
- [`strconv.Atoi`](https://pkg.go.dev/strconv#Atoi) — the accept/reject and range behavior the parser wraps.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-roundtrip-uvarint-codec.md](02-roundtrip-uvarint-codec.md)
