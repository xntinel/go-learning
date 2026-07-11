# Exercise 21: Redact PII from Nested Audit Logs Recursively

**Nivel: Intermedio** — validacion rapida (un test corto).

Audit logs decoded from JSON come back as `any` values built out of
`map[string]any`, `[]any`, and scalars, nested however deeply the original
payload happened to be — an actor object inside an event, a payment object
inside the actor, tags as a list of strings that might themselves embed an
email. Redacting PII before the log is shipped anywhere means walking that
whole shape and scrubbing every string, at every level, and a log payload
is exactly the kind of externally-influenced input that should never be
allowed to recurse without a depth limit.

This module is fully self-contained: its own `go mod init`, the redactor
inline, its own demo and tests.

## What you'll build

```text
piiredact/                    independent module: example.com/piiredact
  go.mod                        go 1.24
  piiredact.go                   func Redact(v any, maxDepth int) (any, error)
  piiredact_test.go               nested map, slice of maps, card number, non-PII untouched, depth guard, JSON round trip
  cmd/
    demo/
      main.go                     unmarshals a small JSON audit event, redacts it, prints it back as JSON
```

- Files: `piiredact.go`, `cmd/demo/main.go`, `piiredact_test.go`.
- Implement: `func Redact(v any, maxDepth int) (any, error)`, recursing
  through `map[string]any` and `[]any`, applying email and card-number
  regexps to every `string` leaf, and refusing to descend past `maxDepth`.
- Test: an email nested inside a map is redacted; PII inside a slice of maps
  is redacted per-element; a card-number-shaped string is redacted;
  non-string scalars (numbers, bools) and PII-free strings pass through
  unchanged; a structure nested past the depth limit is rejected; a real
  `encoding/json.Unmarshal` round trip redacts correctly.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/piiredact/cmd/demo
cd ~/go-exercises/piiredact
go mod init example.com/piiredact
go mod edit -go=1.24
```

### One recursive function, dispatching on what `any` actually holds

`encoding/json.Unmarshal` into an `any` produces a small, fixed set of
concrete types: `map[string]any` for a JSON object, `[]any` for an array,
and `string`, `float64`, `bool`, or `nil` for scalars. `redact` is a single
recursive function with a type switch over exactly those cases: an object
recurses into every value, an array recurses into every element, a string
gets scrubbed, and everything else (a number, a bool, `nil`) is returned
unchanged because there is nothing in it to redact and nothing to recurse
into. The recursion bottoms out naturally at the scalar cases — they are
the base case, reached once the structure has been unwrapped as far as it
goes.

The depth guard threads through both recursive cases (object and array)
identically, exactly like the mutual-recursion depth guard elsewhere in
this lesson needed the same check in both of its functions: whichever
container type is nested one level too many, the same `depth > maxDepth`
check catches it before that level's contents are ever inspected. This
matters because a log payload is attacker- or bug-influenced input by
definition — the entire point of scrubbing it before it goes anywhere — so
it is exactly the input a maliciously deep structure would target to
exhaust the stack of whatever process is doing the scrubbing.

Create `piiredact.go`:

```go
// Package piiredact walks an arbitrarily nested audit log structure
// (decoded JSON: maps, slices, strings, and other scalars) and redacts
// personally identifiable information found in string values, recursing
// into nested maps and slices. A depth guard rejects structures nested
// deeper than a configured limit rather than recursing into them, since a
// malformed or adversarial log is exactly the kind of input that must not
// be allowed to exhaust the goroutine stack.
package piiredact

import (
	"errors"
	"fmt"
	"regexp"
)

// ErrTooDeep is returned when a value nests deeper than maxDepth.
var ErrTooDeep = errors.New("piiredact: nesting depth exceeds limit")

const redacted = "[REDACTED]"

var (
	emailPattern = regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)
	// A simple 16-digit-with-optional-separators pattern, enough to catch
	// card-number-shaped strings in an audit log without pulling in a full
	// Luhn validator.
	cardPattern = regexp.MustCompile(`\b(?:\d[ -]?){13,16}\b`)
)

// Redact walks v (as produced by encoding/json.Unmarshal into an any: maps
// are map[string]any, arrays are []any, and scalars are string, float64,
// bool, or nil) and returns a copy with email- and card-number-shaped
// substrings replaced by a redaction marker. It refuses to descend past
// maxDepth levels of nesting, returning ErrTooDeep instead.
func Redact(v any, maxDepth int) (any, error) {
	return redact(v, 1, maxDepth)
}

func redact(v any, depth, maxDepth int) (any, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("%w: depth %d", ErrTooDeep, depth)
	}

	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, child := range t {
			rc, err := redact(child, depth+1, maxDepth)
			if err != nil {
				return nil, err
			}
			out[k] = rc
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, child := range t {
			rc, err := redact(child, depth+1, maxDepth)
			if err != nil {
				return nil, err
			}
			out[i] = rc
		}
		return out, nil
	case string:
		s := emailPattern.ReplaceAllString(t, redacted)
		s = cardPattern.ReplaceAllString(s, redacted)
		return s, nil
	default:
		return v, nil
	}
}
```

### The runnable demo

The demo unmarshals a small JSON audit event — a login with an actor email,
a nested payment card, and a tag that embeds an email — redacts it, and
prints the result back out as indented JSON.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"

	"example.com/piiredact"
)

func main() {
	raw := `{
		"event": "login",
		"actor": {
			"name": "Ada Lovelace",
			"email": "ada@example.com",
			"payment": {
				"card": "4111 1111 1111 1111"
			}
		},
		"tags": ["auth", "contact:ada@example.com"]
	}`

	var log any
	if err := json.Unmarshal([]byte(raw), &log); err != nil {
		fmt.Println("unmarshal error:", err)
		return
	}

	redacted, err := piiredact.Redact(log, 10)
	if err != nil {
		fmt.Println("redact error:", err)
		return
	}

	out, _ := json.MarshalIndent(redacted, "", "  ")
	fmt.Println(string(out))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{
  "actor": {
    "email": "[REDACTED]",
    "name": "Ada Lovelace",
    "payment": {
      "card": "[REDACTED]"
    }
  },
  "event": "login",
  "tags": [
    "auth",
    "contact:[REDACTED]"
  ]
}
```

### Tests

`TestRedactEmailInNestedMap` and `TestRedactEmailInsideSliceOfMaps` check
recursion into both container kinds. `TestRedactCardNumber` checks the
second pattern. `TestRedactLeavesNonPIIScalarsAlone` is the negative case
that matters just as much as the positive ones — numbers, bools, and
PII-free strings must survive unchanged. `TestRedactRejectsOverDepthStructure`
builds a structure 20 levels deep against a limit of 10 and expects
`ErrTooDeep`. `TestRedactRoundTripsThroughJSON` exercises the function
against a real `encoding/json.Unmarshal` result rather than a hand-built
`any`, confirming the type switch matches what the standard decoder
actually produces.

Create `piiredact_test.go`:

```go
package piiredact

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestRedactEmailInNestedMap(t *testing.T) {
	t.Parallel()

	v := map[string]any{
		"actor": map[string]any{
			"email": "ada@example.com",
		},
	}
	got, err := Redact(v, 10)
	if err != nil {
		t.Fatalf("Redact() error = %v", err)
	}

	actor := got.(map[string]any)["actor"].(map[string]any)
	if actor["email"] != redacted {
		t.Fatalf("email = %q, want %q", actor["email"], redacted)
	}
}

func TestRedactEmailInsideSliceOfMaps(t *testing.T) {
	t.Parallel()

	v := []any{
		map[string]any{"contact": "reach me at bob@example.org please"},
		map[string]any{"contact": "no pii here"},
	}
	got, err := Redact(v, 10)
	if err != nil {
		t.Fatalf("Redact() error = %v", err)
	}

	items := got.([]any)
	if items[0].(map[string]any)["contact"] != "reach me at "+redacted+" please" {
		t.Fatalf("contact[0] = %q", items[0].(map[string]any)["contact"])
	}
	if items[1].(map[string]any)["contact"] != "no pii here" {
		t.Fatalf("contact[1] = %q, want unchanged", items[1].(map[string]any)["contact"])
	}
}

func TestRedactCardNumber(t *testing.T) {
	t.Parallel()

	got, err := Redact("card on file: 4111 1111 1111 1111", 10)
	if err != nil {
		t.Fatalf("Redact() error = %v", err)
	}
	if got != "card on file: "+redacted {
		t.Fatalf("got = %q", got)
	}
}

func TestRedactLeavesNonPIIScalarsAlone(t *testing.T) {
	t.Parallel()

	v := map[string]any{"count": float64(3), "active": true, "note": "hello world"}
	got, err := Redact(v, 10)
	if err != nil {
		t.Fatalf("Redact() error = %v", err)
	}
	m := got.(map[string]any)
	if m["count"] != float64(3) || m["active"] != true || m["note"] != "hello world" {
		t.Fatalf("non-PII values changed: %v", m)
	}
}

func TestRedactRejectsOverDepthStructure(t *testing.T) {
	t.Parallel()

	// Build a structure nested 20 levels deep: {"child":{"child":...:"leaf"}}.
	var nested any = "leaf"
	for i := 0; i < 20; i++ {
		nested = map[string]any{"child": nested}
	}

	_, err := Redact(nested, 10)
	if !errors.Is(err, ErrTooDeep) {
		t.Fatalf("Redact() error = %v, want %v", err, ErrTooDeep)
	}
}

func TestRedactRoundTripsThroughJSON(t *testing.T) {
	t.Parallel()

	raw := `{"actor":{"email":"ada@example.com","name":"Ada"}}`
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	got, err := Redact(v, 10)
	if err != nil {
		t.Fatalf("Redact() error = %v", err)
	}

	actor := got.(map[string]any)["actor"].(map[string]any)
	if actor["email"] != redacted {
		t.Fatalf("email = %q, want %q", actor["email"], redacted)
	}
	if actor["name"] != "Ada" {
		t.Fatalf("name = %q, want unchanged", actor["name"])
	}
}
```

Run it: `go test -count=1 ./...`

## Review

`Redact` is correct when every string anywhere in the structure, at any
depth, has been checked against both patterns, and everything that is not a
string passes through byte-for-byte unchanged.
`TestRedactLeavesNonPIIScalarsAlone` is as important as the positive
redaction tests: an eager implementation that stringifies and re-scans
every value risks corrupting numbers or bools that were never strings to
begin with. The mistake this exercise targets is applying the depth guard
in only one of the two recursive branches (say, maps but not slices) — a
JSON array nested past the limit would then sail through unchecked, which
is exactly the gap `TestRedactRejectsOverDepthStructure` would need to be
paired with an array-shaped version of itself to catch, and is worth
noticing even though this exercise's structure happens to nest through
maps.

## Resources

- [encoding/json package (Unmarshal into any)](https://pkg.go.dev/encoding/json#Unmarshal)
- [regexp package](https://pkg.go.dev/regexp)
- [Go Specification: Type switches](https://go.dev/ref/spec#Type_switches)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [20-nested-cache-invalidation-memoized-cascade.md](20-nested-cache-invalidation-memoized-cascade.md) | Next: [22-raft-log-prefix-matching-memoized.md](22-raft-log-prefix-matching-memoized.md)
