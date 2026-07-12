# Exercise 17: JWT Token Decoding With Typed Claims

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A JWT is three base64url segments joined by dots: header, payload, and a
signature this exercise deliberately does not check. Decoding the payload
into usable claims still has to answer three separate questions at once —
what did the token say, when does it expire, and is any of that even
trustworthy shape-wise — which is exactly what a three-value return is for.
This exercise builds `DecodeClaims(token) (claims map[string]any,
expiresAt time.Time, valid bool)`, using the comma-ok form on every step
of the decode so a malformed token degrades to `valid == false` instead of
a panic.

This module is fully self-contained: its own `go mod init`, all code
inline, its own demo and tests.

## What you'll build

```text
tokenclaims/                independent module: example.com/oauth-token-decode-claims
  go.mod                    go 1.24
  tokenclaims.go            package tokenclaims; BuildToken(claims) (string,error); DecodeClaims(token) (claims,expiresAt,valid)
  cmd/
    demo/
      main.go               builds a token, decodes it, then decodes a malformed one
  tokenclaims_test.go       valid token; missing exp; wrong-typed exp; malformed shapes
```

- Files: `tokenclaims.go`, `cmd/demo/main.go`, `tokenclaims_test.go`.
- Implement: `DecodeClaims(token string) (claims map[string]any, expiresAt time.Time, valid bool)` splitting on `.`, base64url-decoding the payload, JSON-unmarshalling it, and comma-ok asserting the `exp` claim to `float64` before converting to `time.Time`.
- Test: a well-formed token decodes with `valid == true` and the right `expiresAt`; a token missing `exp`, one with a non-numeric `exp`, and tokens with the wrong segment count all give `valid == false` without panicking.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Decode, not verify — and the assertions that make that safe

This package never checks a signature — it is a claims *decoder*, not an
auth library, and the doc comment on `DecodeClaims` says so explicitly so
nobody wires it in expecting cryptographic trust. What it does have to get
right is that a byte string from the network can claim to be anything:
wrong number of segments, non-base64 payload, payload that is valid base64
but not JSON, JSON that is valid but has no `exp`, or an `exp` that is a
string instead of a number. Every one of those has to degrade to
`valid == false`, never a panic, because `encoding/json` decodes *every*
JSON number into `float64` regardless of what the schema expects:

```go
expRaw, ok := raw["exp"]
if !ok {
    return raw, time.Time{}, false
}
expNum, ok := expRaw.(float64)
if !ok {
    return raw, time.Time{}, false
}
```

The comma-ok assertion `expRaw.(float64)` does two jobs in one line: it
confirms `exp` is numeric at all, and it performs the conversion, without a
plain type assertion's panic on a token where `exp` is missing, a string,
or a nested object. `claims` itself is still returned even when `valid` is
false, so a caller debugging a rejected token can inspect what *was*
present.

Create `tokenclaims.go`:

```go
package tokenclaims

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// BuildToken assembles a JWT-shaped string (header.payload.signature) for
// demos and tests. The signature segment is left empty — this package only
// decodes claims, it never verifies a signature, so it must not be mistaken
// for an auth library.
func BuildToken(claims map[string]any) (string, error) {
	header := map[string]string{"alg": "none", "typ": "JWT"}
	h, err := encodeSegment(header)
	if err != nil {
		return "", err
	}
	p, err := encodeSegment(claims)
	if err != nil {
		return "", err
	}
	return h + "." + p + ".", nil
}

func encodeSegment(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// DecodeClaims decodes the payload segment of a JWT-shaped token. It never
// checks a signature — the "valid" result means only "this string has the
// three-segment shape, its payload is a JSON object, and it carries a
// numeric exp claim". Callers still needing signature verification must do
// that separately before trusting the claims.
func DecodeClaims(token string) (claims map[string]any, expiresAt time.Time, valid bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, time.Time{}, false
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, time.Time{}, false
	}

	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, time.Time{}, false
	}

	expRaw, ok := raw["exp"]
	if !ok {
		return raw, time.Time{}, false
	}
	// encoding/json decodes all JSON numbers into float64 by default; the
	// comma-ok assertion both confirms the claim is numeric and performs
	// the conversion in one step, without a panic on a malformed token
	// where "exp" is a string or an object.
	expNum, ok := expRaw.(float64)
	if !ok {
		return raw, time.Time{}, false
	}

	return raw, time.Unix(int64(expNum), 0), true
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/oauth-token-decode-claims"
)

func main() {
	exp := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	token, err := tokenclaims.BuildToken(map[string]any{
		"sub":  "user-42",
		"role": "admin",
		"exp":  float64(exp.Unix()),
	})
	if err != nil {
		fmt.Println("build error:", err)
		return
	}

	claims, expiresAt, valid := tokenclaims.DecodeClaims(token)
	fmt.Printf("valid=%t sub=%v role=%v expiresAt=%s\n",
		valid, claims["sub"], claims["role"], expiresAt.UTC().Format(time.RFC3339))

	_, _, valid = tokenclaims.DecodeClaims("not-a-jwt")
	fmt.Printf("malformed token valid=%t\n", valid)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
valid=true sub=user-42 role=admin expiresAt=2030-01-01T00:00:00Z
malformed token valid=false
```

### Tests

Create `tokenclaims_test.go`:

```go
package tokenclaims

import (
	"testing"
	"time"
)

func TestDecodeClaimsValidToken(t *testing.T) {
	t.Parallel()
	exp := time.Date(2030, 6, 15, 12, 0, 0, 0, time.UTC)
	token, err := BuildToken(map[string]any{
		"sub": "user-1",
		"exp": float64(exp.Unix()),
	})
	if err != nil {
		t.Fatalf("BuildToken: %v", err)
	}

	claims, expiresAt, valid := DecodeClaims(token)
	if !valid {
		t.Fatal("valid = false, want true")
	}
	if claims["sub"] != "user-1" {
		t.Fatalf("sub = %v, want user-1", claims["sub"])
	}
	if !expiresAt.Equal(exp) {
		t.Fatalf("expiresAt = %s, want %s", expiresAt, exp)
	}
}

func TestDecodeClaimsMissingExp(t *testing.T) {
	t.Parallel()
	token, err := BuildToken(map[string]any{"sub": "user-1"})
	if err != nil {
		t.Fatalf("BuildToken: %v", err)
	}

	claims, _, valid := DecodeClaims(token)
	if valid {
		t.Fatal("valid = true, want false when exp is absent")
	}
	if claims["sub"] != "user-1" {
		t.Fatalf("claims should still be returned for inspection, got %v", claims)
	}
}

func TestDecodeClaimsExpWrongType(t *testing.T) {
	t.Parallel()
	token, err := BuildToken(map[string]any{"sub": "user-1", "exp": "soon"})
	if err != nil {
		t.Fatalf("BuildToken: %v", err)
	}

	_, _, valid := DecodeClaims(token)
	if valid {
		t.Fatal("valid = true, want false when exp is not numeric")
	}
}

func TestDecodeClaimsMalformedShape(t *testing.T) {
	t.Parallel()
	cases := []string{
		"",
		"only-one-segment",
		"two.segments",
		"three.segments.but-not-base64!!!",
	}
	for _, tok := range cases {
		_, _, valid := DecodeClaims(tok)
		if valid {
			t.Fatalf("DecodeClaims(%q) valid = true, want false", tok)
		}
	}
}
```

## Review

`DecodeClaims` is correct when every shape failure — segment count, base64,
JSON, and the `exp` type — degrades to `valid == false` with no panic, and
when a valid token's `expiresAt` matches `time.Unix` applied to the exact
`exp` value that went in. `TestDecodeClaimsMalformedShape` is the
load-bearing test: it runs four different kinds of garbage through the
same function and asserts none of them crash it, which is the whole point
of building this on comma-ok assertions instead of a direct
`raw["exp"].(float64)` that would panic on the very first malformed input.

The mistake to avoid is trusting `valid == true` as a proxy for
"authenticated" — this decoder never touches the signature segment, so a
caller that needs real auth must verify the signature (with a real JWT
library and the right key) before treating any of these claims as fact.

## Resources

- [JSON Web Token (RFC 7519)](https://www.rfc-editor.org/rfc/rfc7519) — the three-segment structure and the standard `exp` claim this exercise decodes.
- [encoding/json package docs](https://pkg.go.dev/encoding/json) — why JSON numbers decode into `float64` by default, the reason `exp` needs a comma-ok assertion rather than a direct type conversion.
- [encoding/base64.RawURLEncoding](https://pkg.go.dev/encoding/base64#pkg-variables) — the unpadded, URL-safe alphabet JWT segments use.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [16-database-cursor-position-pagination.md](16-database-cursor-position-pagination.md) | Next: [18-schema-migration-compatibility-check.md](18-schema-migration-compatibility-check.md)
