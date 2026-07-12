# Exercise 24: OAuth Token Scope Claim Verifier

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

An OAuth-protected endpoint declares the scopes it needs — `read`, `write`,
`admin` — and a request is authorized only if the presented token's granted
scopes are a superset of that requirement. This is the AND counterpart to
the RBAC "any one predicate grants" chain from exercise 23: here *every*
required scope must be present, and the check fails fast on the first
missing one rather than reporting every gap, because scope enforcement runs
on every single request and the caller only ever needs to know "denied,
here's why" to return a `403`.

## What you'll build

```text
oauthscope/                 independent module: example.com/oauthscope
  go.mod                    go 1.24
  oauthscope.go             package oauthscope; func RequireScopes(tokenScopes []string, required ...string) error
  cmd/
    demo/
      main.go               runnable demo: a read-only endpoint granted, an admin endpoint denied
  oauthscope_test.go        table tests: full grant, missing scope, zero requirement, empty token, fail-fast ordering
```

- Files: `oauthscope.go`, `cmd/demo/main.go`, `oauthscope_test.go`.
- Implement: `RequireScopes(tokenScopes []string, required ...string) error`, building a set from `tokenScopes` and checking every element of `required` is present, returning on the first miss.
- Test: a token with every required scope passes; a token missing one required scope fails; zero required scopes always passes, even against an empty token; an empty token against any non-empty requirement fails; when multiple required scopes are missing, the error names only the first one in `required`'s order, never a later one.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/03-variadic-functions/24-oauth-scope-claim-verifier/cmd/demo
cd go-solutions/04-functions/03-variadic-functions/24-oauth-scope-claim-verifier
go mod edit -go=1.24
```

### AND semantics and fail-fast, in contrast with the RBAC OR chain

`RequireScopes(tokenScopes []string, required ...string) error` looks
structurally similar to exercise 23's `Allow(userRoles []string, predicates
...Predicate) bool`, but the logic is deliberately inverted. `Allow` grants
on the *first* predicate that says yes (an OR of alternatives); `RequireScopes`
denies on the *first* required scope that is missing (an AND of
requirements) — every element of `required` must independently be satisfied,
so one miss is enough to deny the whole request, and there is no reason to
keep checking the rest once that has happened. That is exactly what "fail
fast" means here: the function returns from inside the loop the instant it
finds a gap, rather than collecting every missing scope into one report.

The reason fail-fast is the right choice for *this* function — as opposed to
the WHERE-predicate combinator in exercise 20, which deliberately aggregates
every failure — is the audience and frequency of the check. A form
validator runs once per user submission and benefits from reporting every
problem at once; a scope check runs on every single authorized request in a
hot path, and the caller (usually middleware returning a `403`) only needs
one reason, not an exhaustive audit. Building `granted` as a
`map[string]struct{}` once up front turns each membership test into O(1),
so checking `required` (however long it is) against `tokenScopes` (however
long *that* is) costs one pass over each, not a nested loop.

Create `oauthscope.go`:

```go
// oauthscope.go
package oauthscope

import "fmt"

// RequireScopes checks that tokenScopes contains every one of required. It
// fails fast: the moment a required scope is missing, it returns naming that
// scope, without checking whether any later required scope is also missing.
// Zero required scopes always succeeds — an endpoint with no scope
// requirement grants access regardless of what the token carries.
func RequireScopes(tokenScopes []string, required ...string) error {
	if len(required) == 0 {
		return nil
	}

	granted := make(map[string]struct{}, len(tokenScopes))
	for _, s := range tokenScopes {
		granted[s] = struct{}{}
	}

	for _, req := range required {
		if _, ok := granted[req]; !ok {
			return fmt.Errorf("oauthscope: missing required scope %q", req)
		}
	}
	return nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/oauthscope"
)

func main() {
	tokenScopes := []string{"read", "write"}

	if err := oauthscope.RequireScopes(tokenScopes, "read"); err != nil {
		fmt.Println("error:", err)
	} else {
		fmt.Println("read-only endpoint: granted")
	}

	if err := oauthscope.RequireScopes(tokenScopes, "read", "admin"); err != nil {
		fmt.Println("error:", err)
	} else {
		fmt.Println("admin endpoint: granted")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
read-only endpoint: granted
error: oauthscope: missing required scope "admin"
```

### Tests

`TestRequireScopesFailsFastOnFirstMissingScope` is the one that pins the
"fail fast" half of the contract: given a token with only `read` and a
requirement of `read, write, admin` (two scopes missing), the error must name
`write` — the first missing scope in `required`'s order — and must *not*
mention `admin`, proving the function stopped rather than continuing to
check.

Create `oauthscope_test.go`:

```go
// oauthscope_test.go
package oauthscope

import (
	"strings"
	"testing"
)

func TestRequireScopes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		tokenScopes []string
		required    []string
		wantErr     bool
	}{
		{"all required scopes granted", []string{"read", "write", "admin"}, []string{"read", "write"}, false},
		{"zero required scopes always passes", nil, nil, false},
		{"zero required scopes passes even with an empty token", []string{}, nil, false},
		{"missing scope fails", []string{"read"}, []string{"read", "write"}, true},
		{"empty token with a requirement fails", nil, []string{"read"}, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := RequireScopes(tc.tokenScopes, tc.required...)
			if tc.wantErr && err == nil {
				t.Fatalf("RequireScopes(%v, %v) error = nil, want an error", tc.tokenScopes, tc.required)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("RequireScopes(%v, %v) unexpected error: %v", tc.tokenScopes, tc.required, err)
			}
		})
	}
}

func TestRequireScopesFailsFastOnFirstMissingScope(t *testing.T) {
	t.Parallel()

	err := RequireScopes([]string{"read"}, "read", "write", "admin")
	if err == nil {
		t.Fatalf("expected an error")
	}
	if !strings.Contains(err.Error(), `"write"`) {
		t.Fatalf("error %q should name the first missing scope, %q", err.Error(), "write")
	}
	if strings.Contains(err.Error(), `"admin"`) {
		t.Fatalf("error %q should not mention %q: RequireScopes must fail fast on the first miss", err.Error(), "admin")
	}
}
```

## Review

`RequireScopes` is correct when it grants exactly when every element of
`required` is present in `tokenScopes`, denies with an error naming the
first missing scope in `required`'s order the moment one is absent, and
treats zero required scopes as an automatic pass regardless of the token.
The senior point is recognizing which of two equally valid error-reporting
strategies fits a given call frequency: fail-fast for a per-request hot-path
check that only needs one reason to deny, versus aggregate-all-failures for
a form validated once per human submission (exercise 20). The mistake to
avoid is testing membership with a linear scan of `tokenScopes` inside the
loop over `required` — that is O(len(required) × len(tokenScopes)), and it
silently gets worse as either list grows, whereas the map-based set turns
the whole check into O(len(required) + len(tokenScopes)).

## Resources

- [RFC 6749: The OAuth 2.0 Authorization Framework](https://www.rfc-editor.org/rfc/rfc6749) — scope semantics (`scope` parameter, section 3.3).
- [RFC 8693: OAuth 2.0 Token Exchange](https://www.rfc-editor.org/rfc/rfc8693) — scope narrowing across token exchange, for further reading.
- [Go Spec: Passing arguments to `...` parameters](https://go.dev/ref/spec#Passing_arguments_to_..._parameters)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [23-rbac-permission-validator-roles.md](23-rbac-permission-validator-roles.md) | Next: [25-transaction-fee-accumulator-rules.md](25-transaction-fee-accumulator-rules.md)
