# Exercise 8: Debug Why json.Marshaler Is Ignored For A Value-Receiver Type

You add `MarshalJSON` to redact a secret in logs and API responses, ship it, and
the secret leaks anyway — but only sometimes. The cause is a method-set rule:
`encoding/json` checks for `json.Marshaler` at runtime, and a pointer-receiver
method is not in the method set of a non-addressable value. This module reproduces
the miss, explains it, and fixes it.

This module is fully self-contained: its own module, code, demo, and tests.

## What you'll build

```text
secret/                     independent module: example.com/secret
  go.mod                    go 1.26
  secret.go                 Secret (value receiver, correct) vs PtrSecret (pointer receiver, buggy)
  cmd/
    demo/
      main.go               marshals each type as a value and as a map element
  secret_test.go            value-receiver always redacts; pointer-receiver misses on values; round trip
```

- Files: `secret.go`, `cmd/demo/main.go`, `secret_test.go`.
- Implement: `Secret` with a value-receiver `MarshalJSON` (redacts everywhere) and pointer-receiver `UnmarshalJSON`; `PtrSecret` with a pointer-receiver `MarshalJSON` (the bug).
- Test: marshaling a `Secret` value and a `Secret` map element is redacted; marshaling a `PtrSecret` value is NOT redacted while `&PtrSecret` is; a Marshal/Unmarshal round trip keeps the secret out of the JSON.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/secret/cmd/demo
cd ~/go-exercises/secret
go mod init example.com/secret
go mod edit -go=1.26
```

### The method-set rule, and how json checks it

The method set of a value type `T` contains only its value-receiver methods; the
method set of `*T` contains both value- and pointer-receiver methods. `encoding/json`
marshals a value by asking, at runtime, whether it implements `json.Marshaler`
(`MarshalJSON() ([]byte, error)`) — an interface-satisfaction check against the
value's method set. If the method is on a pointer receiver and json holds a
non-addressable value, the check fails, json never calls your method, and it falls
back to default encoding.

Non-addressable is the trap word. When you call `json.Marshal(x)`, the argument is
copied into an `any` and is not addressable, so json cannot take its address to
reach a pointer method. Map elements are also non-addressable (`m[k]` cannot have
its address taken). So a pointer-receiver `MarshalJSON`:

- is *not* called for `json.Marshal(PtrSecret("x"))` — the value is not
  addressable, only `*PtrSecret` has the method, so json encodes the underlying
  string and the secret leaks;
- *is* called for `json.Marshal(&PtrSecret("x"))` — the pointer already satisfies
  `json.Marshaler`;
- is *not* called for a `PtrSecret` map value — map elements are not addressable.

The struct-field case is the one that fools people: for a *struct* value that json
is already encoding through a pointer, its fields are addressable, so a
pointer-receiver method on a field type *does* fire. That inconsistency — works as
a struct field, misses as a map value or a bare value — is exactly why the bug is
intermittent.

The fix is a *value* receiver. `Secret` implements `MarshalJSON` on `Secret`
(value), so the method is in the method set of both `Secret` and `*Secret`, and it
fires for values, map elements, and struct fields alike. Redaction that must hold
everywhere belongs on a value receiver. (`UnmarshalJSON` must stay on a pointer
receiver — it mutates the receiver — but unmarshaling always targets an addressable
`&v`, so that is never a problem.)

Create `secret.go`:

```go
package secret

import (
	"encoding/json"
	"fmt"
)

// Secret is a redacted string. MarshalJSON is on a VALUE receiver, so it is in
// the method set of both Secret and *Secret and fires everywhere: bare values,
// map elements, and struct fields.
type Secret string

// MarshalJSON renders the secret as a fixed placeholder, never its content.
func (s Secret) MarshalJSON() ([]byte, error) {
	return []byte(`"[REDACTED]"`), nil
}

// UnmarshalJSON reads a plain JSON string into the secret. It must be on a
// pointer receiver to mutate; unmarshaling always targets an addressable value.
func (s *Secret) UnmarshalJSON(b []byte) error {
	var raw string
	if err := json.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf("secret: %w", err)
	}
	*s = Secret(raw)
	return nil
}

// Reveal returns the underlying secret. It is the only way to read the content;
// JSON encoding never exposes it.
func (s Secret) Reveal() string {
	return string(s)
}

// PtrSecret reproduces the bug: MarshalJSON is on a POINTER receiver, so it is
// only in the method set of *PtrSecret. A non-addressable PtrSecret value (a bare
// value or a map element) skips it and leaks the content.
type PtrSecret string

// MarshalJSON is on a pointer receiver -- the source of the intermittent leak.
func (s *PtrSecret) MarshalJSON() ([]byte, error) {
	return []byte(`"[REDACTED]"`), nil
}
```

### The runnable demo

The demo marshals each type as a bare value and as a map element, printing the
JSON so the leak is visible: `Secret` is redacted in every position, while
`PtrSecret` is redacted only when marshaled through a pointer.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"

	"example.com/secret"
)

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func main() {
	s := secret.Secret("hunter2")
	p := secret.PtrSecret("hunter2")

	fmt.Println("Secret value:      ", mustJSON(s))
	fmt.Println("Secret map elem:   ", mustJSON(map[string]secret.Secret{"k": s}))
	fmt.Println("PtrSecret value:   ", mustJSON(p))
	fmt.Println("PtrSecret pointer: ", mustJSON(&p))
	fmt.Println("PtrSecret map elem:", mustJSON(map[string]secret.PtrSecret{"k": p}))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Secret value:       "[REDACTED]"
Secret map elem:    {"k":"[REDACTED]"}
PtrSecret value:    "hunter2"
PtrSecret pointer:  "[REDACTED]"
PtrSecret map elem: {"k":"hunter2"}
```

The two `hunter2` lines are the leak: the pointer-receiver marshaler is skipped
for the non-addressable value and map element.

### Tests

`TestSecretRedactsEverywhere` asserts the value-receiver `Secret` is redacted as a
value, a map element, and a struct field. `TestPtrSecretLeaksOnValues` documents
the bug: a `PtrSecret` value and map element are NOT redacted, while `&PtrSecret`
is. `TestRoundTrip` unmarshals a plain string into a `Secret` (recovering the
content for use) and re-marshals it (which redacts), proving the content can be
loaded but never re-serialized.

Create `secret_test.go`:

```go
package secret

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal(%T) error = %v", v, err)
	}
	return string(b)
}

func TestSecretRedactsEverywhere(t *testing.T) {
	t.Parallel()

	s := Secret("hunter2")
	type wrapper struct {
		Password Secret `json:"password"`
	}

	cases := map[string]any{
		"value":        s,
		"map element":  map[string]Secret{"k": s},
		"struct field": wrapper{Password: s},
	}
	for name, v := range cases {
		out := mustMarshal(t, v)
		if strings.Contains(out, "hunter2") {
			t.Fatalf("%s leaked the secret: %s", name, out)
		}
		if !strings.Contains(out, "[REDACTED]") {
			t.Fatalf("%s not redacted: %s", name, out)
		}
	}
}

func TestPtrSecretLeaksOnValues(t *testing.T) {
	t.Parallel()

	p := PtrSecret("hunter2")

	// Bug: a non-addressable value is not redacted.
	if out := mustMarshal(t, p); !strings.Contains(out, "hunter2") {
		t.Fatalf("expected pointer-receiver value to LEAK, got %s", out)
	}
	// Bug: a map element is not addressable either.
	if out := mustMarshal(t, map[string]PtrSecret{"k": p}); !strings.Contains(out, "hunter2") {
		t.Fatalf("expected pointer-receiver map element to LEAK, got %s", out)
	}
	// Correct: a pointer satisfies json.Marshaler, so it is redacted.
	if out := mustMarshal(t, &p); strings.Contains(out, "hunter2") {
		t.Fatalf("expected &PtrSecret to redact, got %s", out)
	}
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()

	var s Secret
	if err := json.Unmarshal([]byte(`"topsecret"`), &s); err != nil {
		t.Fatalf("Unmarshal error = %v", err)
	}
	if s.Reveal() != "topsecret" {
		t.Fatalf("Reveal() = %q, want topsecret", s.Reveal())
	}
	// Re-marshaling never emits the content.
	if out := mustMarshal(t, s); strings.Contains(out, "topsecret") {
		t.Fatalf("re-marshal leaked the secret: %s", out)
	}
}
```

## Review

The fix is a one-word change — value receiver instead of pointer receiver — but the
reason is the whole lesson: `encoding/json` performs a runtime `json.Marshaler`
check against the value's method set, and a pointer-receiver method is absent from
the method set of a non-addressable value, so the redaction silently does not fire
for bare values and map elements. The tell in production is intermittency: the
same type redacts as an addressable struct field but leaks as a map value. The tests
pin both the correct type (redacted in every position) and the buggy one (leaks on
values and map elements, redacts only through a pointer), so a future refactor that
"tidies" the receiver back to a pointer fails loudly. When a method must fire for
every value of a type, put it on a value receiver.

## Resources

- [encoding/json Marshaler](https://pkg.go.dev/encoding/json#Marshaler) — the interface json checks at runtime.
- [Go Specification: Method sets](https://go.dev/ref/spec#Method_sets) — the T-vs-*T rule that decides whether the method is found.
- [Go blog: JSON and Go](https://go.dev/blog/json) — how the encoder discovers and calls custom marshalers.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-reflect-vs-typeswitch-validator.md](07-reflect-vs-typeswitch-validator.md) | Next: [09-stringer-formatter-precedence.md](09-stringer-formatter-precedence.md)
