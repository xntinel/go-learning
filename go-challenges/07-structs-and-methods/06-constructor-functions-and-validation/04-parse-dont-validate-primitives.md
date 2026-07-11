# Exercise 4: Make Illegal States Unrepresentable with Parsed Domain Primitives

The strongest form of the constructor discipline is parsing a raw string into a
distinct type whose underlying value is unexported, so the only way to hold the
type is to have passed its validation. A function that accepts an `Email` can
never receive an unvalidated string, because there is no other door in. This
exercise builds three such primitives — `Email`, `HostPort`, `NonEmptyString` —
using the stdlib parsers that already implement the real format rules.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
primitives/                  independent module: example.com/parse-dont-validate-primitives
  go.mod
  primitives.go              Email, HostPort, NonEmptyString + New* constructors, accessors, sentinels
  cmd/
    demo/
      main.go                parses one of each, prints the canonical values
  primitives_test.go         valid canonicalization, each malformed input, zero value unusable
```

- Files: `primitives.go`, `cmd/demo/main.go`, `primitives_test.go`.
- Implement: `NewEmail`, `NewHostPort`, `NewNonEmptyString`, each wrapping an unexported field and validating via `net/mail`, `net/netip`, and a trim check.
- Test: valid inputs construct and their accessor returns the canonical value; malformed email (no `@`, display-name form), non-numeric or out-of-range port, and empty string each return the sentinel; and the zero value of each type is inert without the constructor.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/primitives/cmd/demo
cd ~/go-exercises/primitives
go mod init example.com/parse-dont-validate-primitives
```

### Parsing that returns a type discharges the obligation

The difference between validating and parsing is where the proof of validity
lives. A `func IsValidEmail(string) bool` returns a boolean and immediately throws
away everything it learned; the raw string travels on, and each consumer must
either re-check or trust that a check happened somewhere. A
`func NewEmail(string) (Email, error)` returns a value of a distinct type whose
field is unexported, so possession of an `Email` is itself the proof that it was
parsed — the type system carries the obligation instead of a convention. Downstream
code writes `func Send(to Email)` and cannot be handed a raw string by mistake.

Each primitive delegates to the parser the stdlib already ships, because these
formats have real rules that a hand-rolled regex gets subtly wrong.
`net/mail.ParseAddress` implements RFC 5322 addressing; the one wrinkle is that it
also accepts the display-name form `Alice <alice@example.com>`, so `NewEmail`
rejects anything with a non-empty `Name` to require a bare address.
`net/netip.ParseAddrPort` parses `host:port` into a typed `AddrPort` with range
checks on the port for free, replacing a fragile split-on-colon.
`NonEmptyString` is the trivial case that still deserves a type: trim, reject
empty, and hand back a value that provably carries content.

Create `primitives.go`:

```go
package primitives

import (
	"errors"
	"fmt"
	"net/mail"
	"net/netip"
	"strings"
)

var (
	ErrInvalidEmail    = errors.New("invalid email address")
	ErrInvalidHostPort = errors.New("invalid host:port")
	ErrEmptyString     = errors.New("string must not be empty")
)

// Email is a validated bare email address. Its zero value is the empty string,
// which no constructor will ever produce; hold one only via NewEmail.
type Email struct {
	addr string
}

// NewEmail parses raw as a bare RFC 5322 address, rejecting the display-name
// form ("Alice <a@b.com>") and anything net/mail cannot parse.
func NewEmail(raw string) (Email, error) {
	raw = strings.TrimSpace(raw)
	a, err := mail.ParseAddress(raw)
	if err != nil {
		return Email{}, fmt.Errorf("%w: %v", ErrInvalidEmail, err)
	}
	if a.Name != "" || a.Address != raw {
		return Email{}, fmt.Errorf("%w: want a bare address, got %q", ErrInvalidEmail, raw)
	}
	return Email{addr: a.Address}, nil
}

// String returns the canonical address.
func (e Email) String() string { return e.addr }

// HostPort is a validated network endpoint.
type HostPort struct {
	ap netip.AddrPort
}

// NewHostPort parses raw ("10.0.0.1:8080") into a HostPort, validating the
// address and port range in one step.
func NewHostPort(raw string) (HostPort, error) {
	raw = strings.TrimSpace(raw)
	ap, err := netip.ParseAddrPort(raw)
	if err != nil {
		return HostPort{}, fmt.Errorf("%w: %v", ErrInvalidHostPort, err)
	}
	return HostPort{ap: ap}, nil
}

// Addr returns the parsed address as text.
func (h HostPort) Addr() string { return h.ap.Addr().String() }

// Port returns the parsed port.
func (h HostPort) Port() uint16 { return h.ap.Port() }

// String returns the canonical host:port.
func (h HostPort) String() string { return h.ap.String() }

// NonEmptyString is a string proven to carry content after trimming.
type NonEmptyString struct {
	value string
}

// NewNonEmptyString trims raw and rejects it if nothing remains.
func NewNonEmptyString(raw string) (NonEmptyString, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return NonEmptyString{}, ErrEmptyString
	}
	return NonEmptyString{value: trimmed}, nil
}

// Value returns the trimmed, non-empty string.
func (s NonEmptyString) Value() string { return s.value }
```

### The runnable demo

The demo parses one of each primitive and prints the canonical value each
accessor returns.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/parse-dont-validate-primitives"
)

func main() {
	e, _ := primitives.NewEmail("alice@example.com")
	hp, _ := primitives.NewHostPort("10.0.0.1:8080")
	name, _ := primitives.NewNonEmptyString("  checkout  ")

	fmt.Printf("email=%s host=%s port=%d name=%q\n",
		e, hp.Addr(), hp.Port(), name.Value())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
email=alice@example.com host=10.0.0.1 port=8080 name="checkout"
```

### Tests

`TestZeroValueInert` documents the payoff of the unexported field: a zero-value
`Email{}` carries an empty address and there is no exported way to give it a valid
one, so a caller cannot manufacture a fake `Email` — the constructor is the only
source. The malformed cases pin each parser's rejection, including the
display-name form that `net/mail` would otherwise accept.

Create `primitives_test.go`:

```go
package primitives

import (
	"errors"
	"fmt"
	"testing"
)

func TestValidConstruction(t *testing.T) {
	t.Parallel()
	e, err := NewEmail("alice@example.com")
	if err != nil || e.String() != "alice@example.com" {
		t.Fatalf("NewEmail = %q, %v", e.String(), err)
	}
	hp, err := NewHostPort("10.0.0.1:8080")
	if err != nil || hp.Addr() != "10.0.0.1" || hp.Port() != 8080 {
		t.Fatalf("NewHostPort = %v, %v", hp, err)
	}
	s, err := NewNonEmptyString("  hi  ")
	if err != nil || s.Value() != "hi" {
		t.Fatalf("NewNonEmptyString = %q, %v", s.Value(), err)
	}
}

func TestInvalidEmail(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"", "notanemail", "no-at-sign", "Alice <alice@example.com>"} {
		if _, err := NewEmail(raw); !errors.Is(err, ErrInvalidEmail) {
			t.Fatalf("NewEmail(%q) err = %v, want ErrInvalidEmail", raw, err)
		}
	}
}

func TestInvalidHostPort(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"", "10.0.0.1", "10.0.0.1:notaport", "10.0.0.1:70000", "nohost:80"} {
		if _, err := NewHostPort(raw); !errors.Is(err, ErrInvalidHostPort) {
			t.Fatalf("NewHostPort(%q) err = %v, want ErrInvalidHostPort", raw, err)
		}
	}
}

func TestEmptyString(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"", "   ", "\t\n"} {
		if _, err := NewNonEmptyString(raw); !errors.Is(err, ErrEmptyString) {
			t.Fatalf("NewNonEmptyString(%q) err = %v, want ErrEmptyString", raw, err)
		}
	}
}

func TestZeroValueInert(t *testing.T) {
	t.Parallel()
	var e Email
	if e.String() != "" {
		t.Fatalf("zero Email should be empty, got %q", e.String())
	}
	var s NonEmptyString
	if s.Value() != "" {
		t.Fatalf("zero NonEmptyString should be empty, got %q", s.Value())
	}
	// The unexported fields mean no package outside this one can populate a
	// primitive without its constructor; the zero value is the only value a
	// caller can obtain without validating, and it is inert.
}

func ExampleNewEmail() {
	e, _ := NewEmail("alice@example.com")
	fmt.Println(e)
	// Output: alice@example.com
}
```

## Review

The primitives are correct when a valid input round-trips through the accessor and
every malformed input returns its sentinel, including the display-name email that
`net/mail` accepts but a bare-address field must reject. The design point is the
unexported field: it is what makes the zero value the only unvalidated value a
caller can hold, and that value is inert, so "parse, don't validate" is enforced
by the compiler rather than by discipline. The mistake to avoid is hand-rolling a
regex for these formats; `net/mail` and `net/netip` implement the actual rules,
including the port range check you would otherwise forget.

## Resources

- [net/mail.ParseAddress](https://pkg.go.dev/net/mail#ParseAddress) — RFC 5322 address parsing, including the display-name form.
- [net/netip.ParseAddrPort](https://pkg.go.dev/net/netip#ParseAddrPort) — typed `host:port` with a free port-range check.
- [Parse, don't validate](https://lexi-lambda.github.io/blog/2019/11/05/parse-don-t-validate/) — the design principle this exercise applies.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-functional-options-http-client.md](03-functional-options-http-client.md) | Next: [05-validate-interface-pipeline.md](05-validate-interface-pipeline.md)
