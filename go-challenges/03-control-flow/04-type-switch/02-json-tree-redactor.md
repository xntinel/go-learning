# Exercise 2: Redact Sensitive Fields While Walking an Arbitrary JSON Tree

Before a request or response payload is logged, its secret fields — passwords,
tokens, card numbers — must be masked. The payload was decoded into `any`, so
scrubbing it means walking a tree of `map[string]any`, `[]any`, and scalars,
recursing on the containers and masking the leaves under sensitive keys. The type
switch drives the recursion.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
redact/                      independent module: example.com/redact
  go.mod                     go 1.26
  redact.go                  Redact(v any, secretKeys map[string]bool) any (recursive type switch)
  cmd/
    demo/
      main.go                decodes a payload, redacts it, re-marshals for logging
  redact_test.go             deep nesting, arrays of objects, scalar pass-through, golden JSON
```

- Files: `redact.go`, `cmd/demo/main.go`, `redact_test.go`.
- Implement: `Redact(v any, secretKeys map[string]bool) any` that returns a copy of
  the tree with values under sensitive keys replaced by a mask.
- Test: deep object+array nesting masks secrets at every depth while leaving
  non-secret scalars untouched; an array of objects is fully traversed; unknown
  scalar types pass through and the walker never panics; a golden test compares
  re-marshaled output to expected JSON.
- Verify: `go test -count=1 -race ./...`

## How the type switch drives a recursive walk

A decoded JSON tree has exactly three shapes at every node: an object
(`map[string]any`), an array (`[]any`), or a scalar (`string`, `json.Number`,
`bool`, `nil`, or anything else the boundary produced). `Redact` type-switches on
the node:

- `case map[string]any`: build a fresh map. For each key, if it is sensitive,
  store the mask; otherwise recurse into the value. Keys are compared
  case-insensitively via `strings.ToLower`, because a real payload has `Password`,
  `password`, and `PASSWORD` and you want all of them.
- `case []any`: build a fresh slice and recurse into each element, so an array of
  objects is fully scrubbed.
- `default`: a scalar (or any type the switch does not name). Return it unchanged.
  This is the branch that makes the walker total: whatever odd value slipped in,
  it passes through instead of panicking.

Two design points matter for a pre-logging scrubber. First, `Redact` returns a
*new* tree and never mutates the input — you must not corrupt the live payload
you are still processing. Second, the mask is applied per key regardless of the
value's shape: masking `"password"` masks whatever it holds, whether a string or
a nested object, so a secret cannot hide inside a structure under a sensitive
key. That is why the sensitive-key check happens before the recursion, not after.

Create `redact.go`:

```go
package redact

import "strings"

// Mask is the placeholder substituted for every sensitive value.
const Mask = "[REDACTED]"

// Redact returns a deep copy of v with any value stored under a key in
// secretKeys (matched case-insensitively) replaced by Mask. Objects and arrays
// are recursed; scalars are returned unchanged. The input is never mutated.
func Redact(v any, secretKeys map[string]bool) any {
	switch node := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(node))
		for k, val := range node {
			if secretKeys[strings.ToLower(k)] {
				out[k] = Mask
				continue
			}
			out[k] = Redact(val, secretKeys)
		}
		return out
	case []any:
		out := make([]any, len(node))
		for i, elem := range node {
			out[i] = Redact(elem, secretKeys)
		}
		return out
	default:
		return node
	}
}

// SecretSet builds a case-insensitive lookup set from a list of key names.
func SecretSet(keys ...string) map[string]bool {
	set := make(map[string]bool, len(keys))
	for _, k := range keys {
		set[strings.ToLower(k)] = true
	}
	return set
}
```

## The runnable demo

The demo decodes a login payload with a nested credentials object and an array
of tokens, redacts it, and re-marshals with `json.MarshalIndent` for logging.
Because `json.Marshal` sorts object keys, the output is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"

	"example.com/redact"
)

func main() {
	raw := `{
		"user": "alice",
		"password": "hunter2",
		"session": {"token": "abc123", "ttl": 30},
		"cards": [{"pan": "4111111111111111", "brand": "visa"}]
	}`

	var payload any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		panic(err)
	}

	secrets := redact.SecretSet("password", "token", "pan")
	clean := redact.Redact(payload, secrets)

	out, err := json.MarshalIndent(clean, "", "  ")
	if err != nil {
		panic(err)
	}
	fmt.Println(string(out))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
{
  "cards": [
    {
      "brand": "visa",
      "pan": "[REDACTED]"
    }
  ],
  "password": "[REDACTED]",
  "session": {
    "token": "[REDACTED]",
    "ttl": 30
  },
  "user": "alice"
}
```

## Tests

The deep-nesting test masks secrets at every depth and asserts the non-secret
scalars are untouched. The array test proves each object element is scrubbed. The
pass-through test hands `Redact` a bare unusual scalar and asserts it returns
unchanged with no panic. The golden test re-marshals the redacted tree and
compares the exact JSON, which also proves the input was not mutated (it is
marshaled again afterward).

Create `redact_test.go`:

```go
package redact

import (
	"encoding/json"
	"testing"
)

func decode(t *testing.T, raw string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return v
}

func TestRedactDeepNesting(t *testing.T) {
	t.Parallel()
	in := decode(t, `{"a":{"b":{"password":"x","keep":"y"}},"token":"t"}`)
	got := Redact(in, SecretSet("password", "token")).(map[string]any)

	inner := got["a"].(map[string]any)["b"].(map[string]any)
	if inner["password"] != Mask {
		t.Errorf("deep password = %v, want %s", inner["password"], Mask)
	}
	if inner["keep"] != "y" {
		t.Errorf("deep keep = %v, want y", inner["keep"])
	}
	if got["token"] != Mask {
		t.Errorf("top token = %v, want %s", got["token"], Mask)
	}
}

func TestRedactArrayOfObjects(t *testing.T) {
	t.Parallel()
	in := decode(t, `{"cards":[{"pan":"1","brand":"visa"},{"pan":"2","brand":"mc"}]}`)
	got := Redact(in, SecretSet("pan")).(map[string]any)
	cards := got["cards"].([]any)
	if len(cards) != 2 {
		t.Fatalf("cards len = %d, want 2", len(cards))
	}
	for i, c := range cards {
		obj := c.(map[string]any)
		if obj["pan"] != Mask {
			t.Errorf("cards[%d].pan = %v, want %s", i, obj["pan"], Mask)
		}
		if obj["brand"] == Mask {
			t.Errorf("cards[%d].brand was masked", i)
		}
	}
}

func TestRedactScalarPassThrough(t *testing.T) {
	t.Parallel()
	// A bare non-container value: the walker must return it untouched.
	type weird struct{ N int }
	got := Redact(weird{N: 7}, SecretSet("password"))
	if got != (weird{N: 7}) {
		t.Fatalf("scalar pass-through = %v, want {7}", got)
	}
}

func TestRedactGolden(t *testing.T) {
	t.Parallel()
	in := decode(t, `{"user":"bob","password":"s3cr3t","meta":{"token":"tok","n":1}}`)
	got := Redact(in, SecretSet("password", "token"))
	out, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"meta":{"n":1,"token":"[REDACTED]"},"password":"[REDACTED]","user":"bob"}`
	if string(out) != want {
		t.Fatalf("golden:\n got %s\nwant %s", out, want)
	}
}
```

## Review

The redactor is correct when a secret key is masked no matter how deep it sits or
whether it holds a scalar or a whole subtree, when non-secret scalars survive
byte-for-byte, and when an unexpected value type passes through the `default`
branch instead of panicking. The classic bug is masking *after* recursing, which
lets a secret leak from inside a nested object under a sensitive key; check the
key first, and only recurse when the key is not sensitive. The second bug is
mutating the input map in place — never do that in a pre-logging path, because
the same payload is often still being handled downstream. Building a fresh map
and slice keeps the walk pure.

## Resources

- [Go Specification: Type switches](https://go.dev/ref/spec#Type_switches)
- [encoding/json Marshal (object keys are sorted)](https://pkg.go.dev/encoding/json#Marshal)
- [strings.ToLower](https://pkg.go.dev/strings#ToLower)

---

Prev: [01-polymorphic-json-event-decoder.md](01-polymorphic-json-event-decoder.md) | Up: [00-concepts.md](00-concepts.md) | Next: [03-sql-scanner-type-switch.md](03-sql-scanner-type-switch.md)
