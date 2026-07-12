# Exercise 25: Parse Nested JWT Claims with Nesting Depth Validation

**Nivel: Intermedio** — validacion rapida (un test corto).

A JWT's claims are just a JSON object, and nothing stops an attacker from
crafting a token whose claims value nests thousands of objects deep. If the
verifier's parsing path is `json.Unmarshal(payload, &map[string]any{})`
followed by a recursive walk to extract nested claims, that walk either
blows the goroutine stack or burns an unreasonable amount of CPU and memory
materializing garbage the caller was never going to use — a classic
token-bomb denial-of-service. The fix is not to skip recursion; nested
claims are a real, useful shape (SAML-style claim namespaces, role trees).
The fix is to make the recursive descent itself enforce a depth limit,
rejecting the offending nesting level the moment it is seen, before
reading — let alone materializing — anything beneath it.

This module is fully self-contained: its own `go mod init`, the decoder
inline, its own demo and tests.

## What you'll build

```text
claimdepth/                   independent module: example.com/claimdepth
  go.mod                        go 1.24
  claimdepth.go                  ParseClaims; decodeObject/decodeArray/decodeValue (recursive descent)
  claimdepth_test.go             flat, nested-ok, token-bomb, bad maxDepth, non-object root, arrays count
  cmd/
    demo/
      main.go                     parses realistic nested claims, then a 5000-deep bomb
```

- Files: `claimdepth.go`, `cmd/demo/main.go`, `claimdepth_test.go`.
- Implement: `ParseClaims(r io.Reader, maxDepth int) (map[string]any, error)` using a recursive-descent trio — `decodeObject`, `decodeArray`, `decodeValue` — built on `encoding/json`'s streaming `Token()` API, so nesting depth is checked before each object or array is entered.
- Test: a flat claim set; nesting within the limit; a 5000-level token bomb rejected by `errors.Is(err, ErrMaxDepthExceeded)`; an invalid `maxDepth < 1`; a non-object root; arrays counting toward depth the same as objects.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Checking depth before recursing, not after

The naive defense checks depth *after* calling `json.Unmarshal` into a
`map[string]any` and then walking the result — but by then the damage is
done: `Unmarshal` already built every nested map, however deep, using its
own internal recursion over the full token stream. The depth check in that
design only decides whether to report an error; it does nothing to bound
the work already performed to reach that point.

This module instead drives `encoding/json`'s lower-level `Decoder.Token()`
API directly, one token at a time, and recurses through three mutually
supporting functions: `decodeObject` for `{...}`, `decodeArray` for
`[...]`, and `decodeValue` for whatever comes next (delegating to one of
the other two, or returning a scalar). Each of `decodeObject` and
`decodeArray` checks `depth > maxDepth` as its very first action — before
reading a single key or element. When a payload nests one level past the
limit, the check fires the instant that level's opening delimiter is
consumed, and the function returns immediately. Every level *beyond* that
one is never read from the decoder at all: the attacker's remaining 4,995
nesting levels sit unread in the input, and the recursive call stack never
grows past `maxDepth` frames regardless of how deep the malicious payload
claims to go.

Depth counts objects and arrays identically: the root object is depth 1,
and each object or array reached by descending into a value is one level
deeper than its parent, whether that parent was a map key or an array
index. A claims payload that nests an array of role objects therefore
spends depth on both the array and the objects inside it, which matches
how an attacker would actually use either container to build a bomb.

Create `claimdepth.go`:

```go
// Package claimdepth decodes JWT/SAML claim payloads with a streaming,
// recursive-descent JSON reader that enforces a maximum nesting depth. Real
// tokens are attacker-controlled: a forged or replayed JWT can encode a claim
// value nested thousands of objects deep purely to exhaust the verifier's
// stack or memory once it is naively unmarshaled into map[string]any. Because
// this decoder walks the token stream itself, it rejects an over-nested
// payload the moment the offending opening brace is seen -- the remaining
// nesting is never read, let alone materialized into Go values.
package claimdepth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ErrMaxDepthExceeded is returned when a claims payload nests deeper than the
// configured maximum.
var ErrMaxDepthExceeded = errors.New("claimdepth: nesting depth exceeds maximum")

// ParseClaims decodes a JSON object of JWT/SAML claims from r, recursively
// rebuilding nested objects and arrays as map[string]any and []any, while
// enforcing maxDepth (the root object is depth 1). Decoding a value one level
// past maxDepth fails immediately, before any of its children are read.
func ParseClaims(r io.Reader, maxDepth int) (map[string]any, error) {
	if maxDepth < 1 {
		return nil, fmt.Errorf("claimdepth: maxDepth must be >= 1, got %d", maxDepth)
	}
	dec := json.NewDecoder(r)
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("claimdepth: %w", err)
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return nil, fmt.Errorf("claimdepth: root claims value must be a JSON object")
	}
	return decodeObject(dec, 1, maxDepth)
}

// decodeObject reads key/value pairs up to the closing '}', recursing into
// decodeValue for each value. depth is this object's own nesting level.
func decodeObject(dec *json.Decoder, depth, maxDepth int) (map[string]any, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("%w: object at depth %d", ErrMaxDepthExceeded, depth)
	}
	out := make(map[string]any)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("claimdepth: %w", err)
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, fmt.Errorf("claimdepth: object key is not a string: %v", keyTok)
		}
		val, err := decodeValue(dec, depth+1, maxDepth)
		if err != nil {
			return nil, err
		}
		out[key] = val
	}
	if _, err := dec.Token(); err != nil { // consume closing '}'
		return nil, fmt.Errorf("claimdepth: %w", err)
	}
	return out, nil
}

// decodeArray mirrors decodeObject for JSON arrays. Array elements count
// toward nesting depth the same as object values do.
func decodeArray(dec *json.Decoder, depth, maxDepth int) ([]any, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("%w: array at depth %d", ErrMaxDepthExceeded, depth)
	}
	out := []any{}
	for dec.More() {
		val, err := decodeValue(dec, depth+1, maxDepth)
		if err != nil {
			return nil, err
		}
		out = append(out, val)
	}
	if _, err := dec.Token(); err != nil { // consume closing ']'
		return nil, fmt.Errorf("claimdepth: %w", err)
	}
	return out, nil
}

// decodeValue reads one JSON value and, for objects and arrays, recurses at
// depth. Scalars (string, float64, bool, nil) are returned as-is.
func decodeValue(dec *json.Decoder, depth, maxDepth int) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("claimdepth: %w", err)
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			return decodeObject(dec, depth, maxDepth)
		case '[':
			return decodeArray(dec, depth, maxDepth)
		default:
			return nil, fmt.Errorf("claimdepth: unexpected closing delimiter %v", t)
		}
	default:
		return t, nil
	}
}
```

### The runnable demo

The demo parses a realistic claims payload with one level of nested role
metadata, then attempts to parse a 5000-level-deep token bomb at the same
`maxDepth` and prints the rejection.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"example.com/claimdepth"
)

func main() {
	good := `{
		"sub": "user-42",
		"iss": "https://auth.example.com",
		"https://schemas.example.com/roles": {
			"org": "acme",
			"permissions": {"read": true, "write": false}
		}
	}`

	claims, err := claimdepth.ParseClaims(strings.NewReader(good), 4)
	if err != nil {
		panic(err)
	}
	out, _ := json.MarshalIndent(claims, "", "  ")
	fmt.Println(string(out))

	bomb := strings.Repeat(`{"a":`, 5000) + "1" + strings.Repeat("}", 5000)
	_, err = claimdepth.ParseClaims(strings.NewReader(bomb), 4)
	fmt.Println("bomb result:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{
  "https://schemas.example.com/roles": {
    "org": "acme",
    "permissions": {
      "read": true,
      "write": false
    }
  },
  "iss": "https://auth.example.com",
  "sub": "user-42"
}
bomb result: claimdepth: nesting depth exceeds maximum: object at depth 5
```

### Tests

`TestParseClaimsFlat` and `TestParseClaimsNestedWithinLimit` check ordinary,
well-formed claims at increasing nesting. `TestParseClaimsRejectsTokenBomb`
is the test this exercise exists for: a 5000-level nested payload must
still return `ErrMaxDepthExceeded` (via `errors.Is`), not hang or overflow.
`TestParseClaimsRejectsInvalidMaxDepth` and
`TestParseClaimsRejectsNonObjectRoot` cover input validation.
`TestParseClaimsArraysCountTowardDepth` proves an array level is not a free
pass around the limit.

Create `claimdepth_test.go`:

```go
package claimdepth

import (
	"errors"
	"strings"
	"testing"
)

func TestParseClaimsFlat(t *testing.T) {
	t.Parallel()

	got, err := ParseClaims(strings.NewReader(`{"sub":"u1","admin":true}`), 2)
	if err != nil {
		t.Fatalf("ParseClaims() error = %v", err)
	}
	if got["sub"] != "u1" || got["admin"] != true {
		t.Fatalf("got = %+v", got)
	}
}

func TestParseClaimsNestedWithinLimit(t *testing.T) {
	t.Parallel()

	raw := `{"sub":"u1","roles":{"org":"acme","perms":{"read":true}}}`
	got, err := ParseClaims(strings.NewReader(raw), 3)
	if err != nil {
		t.Fatalf("ParseClaims() error = %v", err)
	}
	roles, ok := got["roles"].(map[string]any)
	if !ok {
		t.Fatalf("roles is %T, want map[string]any", got["roles"])
	}
	perms, ok := roles["perms"].(map[string]any)
	if !ok {
		t.Fatalf("perms is %T, want map[string]any", roles["perms"])
	}
	if perms["read"] != true {
		t.Fatalf("perms[read] = %v, want true", perms["read"])
	}
}

func TestParseClaimsRejectsTokenBomb(t *testing.T) {
	t.Parallel()

	bomb := strings.Repeat(`{"a":`, 5000) + "1" + strings.Repeat("}", 5000)
	_, err := ParseClaims(strings.NewReader(bomb), 4)
	if !errors.Is(err, ErrMaxDepthExceeded) {
		t.Fatalf("ParseClaims() error = %v, want %v", err, ErrMaxDepthExceeded)
	}
}

func TestParseClaimsRejectsInvalidMaxDepth(t *testing.T) {
	t.Parallel()

	_, err := ParseClaims(strings.NewReader(`{}`), 0)
	if err == nil {
		t.Fatal("expected error for maxDepth < 1")
	}
}

func TestParseClaimsRejectsNonObjectRoot(t *testing.T) {
	t.Parallel()

	_, err := ParseClaims(strings.NewReader(`[1,2,3]`), 3)
	if err == nil {
		t.Fatal("expected error for non-object root")
	}
}

func TestParseClaimsArraysCountTowardDepth(t *testing.T) {
	t.Parallel()

	// root(1) -> "groups" array(2) -> element object(3): fits at maxDepth 3.
	raw := `{"groups":[{"name":"eng"}]}`
	if _, err := ParseClaims(strings.NewReader(raw), 3); err != nil {
		t.Fatalf("ParseClaims() error = %v, want nil at maxDepth 3", err)
	}
	// Same shape at maxDepth 2 must fail: the element object sits at depth 3.
	_, err := ParseClaims(strings.NewReader(raw), 2)
	if !errors.Is(err, ErrMaxDepthExceeded) {
		t.Fatalf("ParseClaims() error = %v, want %v", err, ErrMaxDepthExceeded)
	}
}
```

## Review

`ParseClaims` is correct when it accepts any claims shape within the depth
budget and rejects anything deeper at the exact level the budget was
crossed, without reading further into the offending branch.
`TestParseClaimsRejectsTokenBomb` is the test that would fail (by hanging
or by a stack-overflow panic instead of a clean error) on a version of this
exercise that unmarshals into `map[string]any` first and checks depth
second — the mistake this exercise targets is doing the depth check as a
post-hoc validation pass over already-materialized data rather than as a
guard woven into the recursive descent itself. `TestParseClaimsArraysCountTowardDepth`
guards the second-most-common mistake: forgetting that an attacker can
build the same bomb out of nested arrays instead of nested objects, so both
container kinds must charge the same depth.

## Resources

- [encoding/json package (Decoder, Token, Delim)](https://pkg.go.dev/encoding/json)
- [RFC 7519: JSON Web Token (JWT)](https://www.rfc-editor.org/rfc/rfc7519)
- [OWASP: Denial of Service via unbounded resource consumption](https://owasp.org/www-community/attacks/Denial_of_Service)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [24-semver-dependency-resolution-backtracking.md](24-semver-dependency-resolution-backtracking.md) | Next: [26-state-machine-reachability-analysis.md](26-state-machine-reachability-analysis.md)
