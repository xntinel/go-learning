# Exercise 14: Accept Header Negotiation: Absent vs Unsupported

**Nivel: Intermedio** — validacion rapida (un test corto).

An API that can serve more than one representation of a resource has to
decide, per request, what to actually send back. This module builds that
decision as a small chain of guard clauses over the `Accept` header value,
distinguishing "the client has no preference" from "the client asked for
something this server cannot produce" — two cases that call for different
fallback behavior.

## What you'll build

```text
negotiate/                  independent module: example.com/accept-header-negotiation
  go.mod                    go 1.24
  negotiate.go              Negotiate(accept, supported, fallback) (string, error)
  negotiate_test.go         table: empty, wildcard, supported, unsupported, no fallback
```

- Files: `negotiate.go`, `negotiate_test.go`.
- Implement: `Negotiate(accept string, supported map[string]bool, fallback string) (string, error)`, guarding an empty or `*/*` header first, then a comma-ok check `if _, ok := supported[trimmed]; ok { ... }` against the supported set, then a fallback guard, ending in `ErrUnacceptable`.
- Test: a table over an empty header, a wildcard header, a supported type, an unsupported type with a fallback configured, and two error cases with no fallback.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/negotiate
cd ~/go-exercises/negotiate
go mod init example.com/accept-header-negotiation
go mod edit -go=1.24
```

### Why "no preference" and "unsupported" are not the same guard

An empty `Accept` header or the wildcard `*/*` both mean the client did
not ask for anything specific — the server is free to pick, so it falls
back silently, no error possible if a fallback is configured. A header
naming a real content type the server cannot produce is a different
situation: the client had a preference and it cannot be honored, which is
closer to an error than a default. Collapsing both into one `if
!supported[accept]` guard would make an honestly indifferent client and a
client asking for something impossible look identical; keeping them as
separate guards preserves that distinction all the way to the return
value.

Create `negotiate.go`:

```go
// Package negotiate picks a response content type from an incoming Accept
// header against a fixed set the server can actually produce.
package negotiate

import (
	"errors"
	"strings"
)

// ErrUnacceptable means the client named a content type the server cannot
// produce and no fallback was configured.
var ErrUnacceptable = errors.New("no acceptable content type")

// Negotiate picks the response content type for accept, the raw value of an
// incoming Accept header. An empty header and the wildcard "*/*" both mean
// "client has no preference," which is a different case from a header naming
// a type the server does not support — the first falls back silently, the
// second only falls back if fallback is set. supported is a set: presence of
// a key is what matters, not its value.
func Negotiate(accept string, supported map[string]bool, fallback string) (string, error) {
	trimmed := strings.TrimSpace(accept)

	if trimmed == "" || trimmed == "*/*" {
		if fallback == "" {
			return "", ErrUnacceptable
		}
		return fallback, nil
	}

	if _, ok := supported[trimmed]; ok {
		return trimmed, nil
	}

	if fallback != "" {
		return fallback, nil
	}
	return "", ErrUnacceptable
}
```

### Tests

The table covers both "no preference" cases (empty and wildcard), a
directly supported type, an unsupported type that still resolves via
fallback, and the two genuinely unacceptable cases where no fallback is
configured, asserting the error with `errors.Is` rather than a string
comparison.

Create `negotiate_test.go`:

```go
package negotiate

import (
	"errors"
	"testing"
)

func TestNegotiate(t *testing.T) {
	t.Parallel()

	supported := map[string]bool{
		"application/json": true,
		"application/xml":  true,
	}

	tests := []struct {
		name     string
		accept   string
		fallback string
		want     string
		wantErr  error
	}{
		{
			name:     "empty header falls back silently",
			accept:   "",
			fallback: "application/json",
			want:     "application/json",
		},
		{
			name:     "wildcard header falls back silently",
			accept:   "*/*",
			fallback: "application/json",
			want:     "application/json",
		},
		{
			name:     "a supported type is echoed back as-is",
			accept:   "application/xml",
			fallback: "application/json",
			want:     "application/xml",
		},
		{
			name:     "an unsupported type falls back when one is configured",
			accept:   "application/yaml",
			fallback: "application/json",
			want:     "application/json",
		},
		{
			name:     "an unsupported type with no fallback is an error",
			accept:   "application/yaml",
			fallback: "",
			wantErr:  ErrUnacceptable,
		},
		{
			name:     "an empty header with no fallback is an error",
			accept:   "  ",
			fallback: "",
			wantErr:  ErrUnacceptable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Negotiate(tc.accept, supported, tc.fallback)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Negotiate(%q) error = %v, want %v", tc.accept, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Negotiate(%q) unexpected error: %v", tc.accept, err)
			}
			if got != tc.want {
				t.Errorf("Negotiate(%q) = %q, want %q", tc.accept, got, tc.want)
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

The two error cases prove the guard chain reaches `ErrUnacceptable` from
two different starting points — an empty header and a named-but-unsupported
type — which only happens because the fallback check is one shared guard
placed after both. Carry this forward: when a decision has multiple ways to
reach the same terminal outcome, test each path independently rather than
assuming one passing case covers the others.

## Resources

- [MDN: HTTP content negotiation](https://developer.mozilla.org/en-US/docs/Web/HTTP/Content_negotiation) — the protocol-level version of this decision.
- [RFC 9110 section 12.5.1: Accept](https://www.rfc-editor.org/rfc/rfc9110#section-12.5.1) — the header this exercise parses.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-monthly-quota-gate.md](13-monthly-quota-gate.md) | Next: [15-circuit-breaker-fallback-state-machine.md](15-circuit-breaker-fallback-state-machine.md)
