# Exercise 6: Prevent Secret Leakage in Logs and Responses With a Redacting Type

Secrets leak because a field is exported and nobody thought about it: a `Password`
or `APIKey` gets swept into a JSON response, or a struct is dumped with
`log.Printf("%+v", req)` and the credential lands in your log aggregator forever.
This module builds a generic `Secret[T]` wrapper that carries a real value but
refuses to reveal it through any default path — JSON, `fmt`, `String()` — while a
deliberate `Reveal()` call still returns the real thing.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
secretbox/                     independent module: example.com/secretbox
  go.mod                       go 1.24
  secret/
    secret.go                  Secret[T]: New, Reveal, MarshalJSON, UnmarshalJSON, String
  cmd/
    demo/
      main.go                  marshal credentials; show redaction vs json:"-"
  secret/secret_test.go        redacted JSON, Reveal works, fmt redacted, inbound round-trip
```

Files: `secret/secret.go`, `cmd/demo/main.go`, `secret/secret_test.go`.
Implement: `Secret[T any]` with `New`, `Reveal() T`, `MarshalJSON` emitting `"[REDACTED]"`, `UnmarshalJSON` accepting the real value, and `String()`/`GoString()`.
Test: marshaled output contains `[REDACTED]` and never the real value; `Reveal()` returns it; `fmt` `%v`/`%s`/`%#v` are redacted; an inbound payload populates the secret yet re-marshals redacted.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

## Redact, do not drop

There are two ways to keep a secret out of output, and they are not equivalent.
`json:"-"` **drops** the field: it disappears entirely, so a consumer of the JSON
cannot even tell the field exists. That is right for a purely internal value. But
for a credential that is part of the API's shape, you often want the field to be
*visible as redacted* — `"password":"[REDACTED]"` — so that logs and responses show
the field was present without exposing the value, and so a naive `%+v` dump of the
struct is safe. That is what a redacting `MarshalJSON` buys you that `json:"-"`
cannot.

`Secret[T]` is a generic wrapper (`T` can be a `string` API key, a struct of
credentials, anything). It stores the real value unexported so no code outside the
package can read the field directly. Every default output path is overridden to
redact:

- `MarshalJSON` returns the constant bytes `"[REDACTED]"` — the value never reaches
  the encoder.
- `String()` (`fmt.Stringer`) and `GoString()` (`fmt.GoStringer`) return
  `[REDACTED]`, so `%s`, `%v`, and `%#v` all print the placeholder. This is the
  path that matters for logging, where `%+v` on a request struct is the most common
  accidental-leak vector.
- `UnmarshalJSON` is the one direction that *does* accept the real value: an
  inbound request legitimately carries the password, so decoding populates the
  wrapped value. The asymmetry is the whole point — real in, redacted out.

Retrieving the value is possible only through an explicit `Reveal()` method. That
makes every access to the plaintext greppable and intentional: `grep Reveal` shows
you every place a secret is actually used, which is exactly the audit surface you
want.

Create `secret/secret.go`:

```go
// secret/secret.go
package secret

import "encoding/json"

// placeholder is what a Secret shows anywhere it would otherwise leak.
const placeholder = "[REDACTED]"

// Secret wraps a value that must never appear in JSON, logs, or fmt output. The
// real value is reachable only through Reveal.
type Secret[T any] struct {
	value T
}

// New wraps v as a Secret.
func New[T any](v T) Secret[T] { return Secret[T]{value: v} }

// Reveal returns the underlying value. Every call is a deliberate, greppable
// access to the plaintext.
func (s Secret[T]) Reveal() T { return s.value }

// MarshalJSON always emits the redaction placeholder; the value never reaches
// the encoder.
func (s Secret[T]) MarshalJSON() ([]byte, error) {
	return json.Marshal(placeholder)
}

// UnmarshalJSON accepts the real inbound value into the wrapper.
func (s *Secret[T]) UnmarshalJSON(b []byte) error {
	return json.Unmarshal(b, &s.value)
}

// String redacts for %s and %v.
func (s Secret[T]) String() string { return placeholder }

// GoString redacts for %#v.
func (s Secret[T]) GoString() string { return placeholder }
```

## The runnable demo

The demo marshals a credentials struct that mixes a redacted `Secret` password
with an `APIKey` dropped via `json:"-"`, then prints a `%v` dump and a `Reveal()`,
so you can see redaction on every default path and the value only on purpose.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"encoding/json"
	"fmt"

	"example.com/secretbox/secret"
)

type Credentials struct {
	User     string                `json:"user"`
	Password secret.Secret[string] `json:"password"`
	APIKey   string                `json:"-"`
}

func main() {
	c := Credentials{
		User:     "alice",
		Password: secret.New("hunter2"),
		APIKey:   "sk-live-do-not-log",
	}

	out, _ := json.Marshal(c)
	fmt.Printf("json:   %s\n", out)
	fmt.Printf("log:    %v\n", c)
	fmt.Printf("reveal: %s\n", c.Password.Reveal())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
json:   {"user":"alice","password":"[REDACTED]"}
log:    {alice [REDACTED] sk-live-do-not-log}
reveal: hunter2
```

The JSON drops `APIKey` entirely (`json:"-"`) and redacts the password. The `%v`
log line still shows `APIKey` (it is a plain field), which is exactly why a real
key belongs in a `Secret`, not behind `json:"-"` alone — the tests pin that
distinction.

## Tests

`TestJSONIsRedacted` marshals a struct with a `Secret` and asserts the output
contains `[REDACTED]` and never the plaintext. `TestRevealReturnsValue` asserts the
real value is still retrievable. `TestFmtIsRedacted` asserts `%v`, `%s`, and `%#v`
all redact. `TestInboundThenRedacted` unmarshals a real payload, asserts the secret
was populated (via `Reveal`), and asserts it re-marshals redacted.

Create `secret/secret_test.go`:

```go
// secret/secret_test.go
package secret

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestJSONIsRedacted(t *testing.T) {
	t.Parallel()
	s := New("hunter2")
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `"[REDACTED]"` {
		t.Fatalf("got %s, want redacted string", data)
	}
	if strings.Contains(string(data), "hunter2") {
		t.Fatalf("plaintext leaked: %s", data)
	}
}

func TestRevealReturnsValue(t *testing.T) {
	t.Parallel()
	s := New("hunter2")
	if s.Reveal() != "hunter2" {
		t.Fatalf("Reveal = %q, want hunter2", s.Reveal())
	}
}

func TestFmtIsRedacted(t *testing.T) {
	t.Parallel()
	s := New("hunter2")
	for _, verb := range []string{"%v", "%s", "%#v", "%+v"} {
		got := fmt.Sprintf(verb, s)
		if strings.Contains(got, "hunter2") {
			t.Fatalf("verb %s leaked plaintext: %s", verb, got)
		}
		if !strings.Contains(got, "[REDACTED]") {
			t.Fatalf("verb %s not redacted: %s", verb, got)
		}
	}
}

func TestInboundThenRedacted(t *testing.T) {
	t.Parallel()
	var s Secret[string]
	if err := json.Unmarshal([]byte(`"inbound-secret"`), &s); err != nil {
		t.Fatal(err)
	}
	if s.Reveal() != "inbound-secret" {
		t.Fatalf("inbound value not stored: %q", s.Reveal())
	}
	out, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "inbound-secret") {
		t.Fatalf("re-marshal leaked inbound secret: %s", out)
	}
}

func ExampleSecret() {
	s := New("hunter2")
	b, _ := json.Marshal(s)
	fmt.Printf("%s json=%s reveal=%s\n", s, b, s.Reveal())
	// Output: [REDACTED] json="[REDACTED]" reveal=hunter2
}
```

## Review

The wrapper is correct when the plaintext appears on exactly one path — `Reveal()`
— and is redacted on every other: JSON, `%v`, `%s`, `%#v`, and `%+v`. The design
lesson is that redaction is a property of the *type*, not of a tag you might forget:
once a value is a `Secret[T]`, there is no default code path that leaks it, so a new
handler or a hasty `log.Printf("%+v", req)` is safe by construction. Use `json:"-"`
for values that should simply not exist on the wire, and a redacting `Secret` for
credentials that should be visibly-present-but-scrubbed — and never leave a raw
`Password string` field exported, which is how the incident starts.

## Resources

- [`encoding/json.Marshaler`](https://pkg.go.dev/encoding/json#Marshaler) — overriding a type's JSON output.
- [`fmt` package: Stringer and GoStringer](https://pkg.go.dev/fmt#Stringer) — controlling `%v`/`%s` and `%#v` output.
- [Go generics: type parameters](https://go.dev/blog/intro-generics) — writing `Secret[T]` for any wrapped value type.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-custom-marshaler-money-type.md](05-custom-marshaler-money-type.md) | Next: [07-rawmessage-polymorphic-envelope.md](07-rawmessage-polymorphic-envelope.md)
