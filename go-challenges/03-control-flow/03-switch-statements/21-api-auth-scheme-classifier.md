# Exercise 21: Classify API Authentication Schemes With Normalization

**Nivel: Intermedio** — validacion rapida (un test corto).

An API gateway sitting in front of a dozen internal services sees every
shape of credential a client might send: a mutual TLS certificate
negotiated below the application layer, an `X-API-Key` header, or an
`Authorization` header carrying `Bearer` or `Basic`. Getting the precedence
and the parsing wrong in this one function means every downstream
validator inherits the bug. This module builds the classifier as a tagless
switch with an explicit precedence order, delegating the actual
credential-shape validation to each case. It is self-contained: its own
`go mod init`, code, demo, and test.

## What you'll build

```text
authscheme/                 independent module: example.com/api-auth-scheme-classifier
  go.mod                     go 1.24
  authscheme.go               package authscheme; ErrNoCredential, ErrMalformedCredential, ErrUnknownScheme; Credential; Classify(auth, apiKey, mtls) (Credential, error)
  cmd/demo/main.go            runnable demo over one request per scheme plus two failure shapes
  authscheme_test.go          table over every scheme, three malformed credentials, and a missing-credential case
```

- Implement: `Classify(authHeader, apiKeyHeader string, mTLSPresent bool) (Credential, error)` — split the Authorization header into a scheme word and the rest with `strings.Cut`, then a tagless switch in a fixed precedence order that validates each scheme's credential shape before returning.
- Test: a table covering mTLS precedence, an API key, a valid bearer and basic credential, three malformed shapes, an unrecognized scheme, and no credential at all.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why strings.Cut instead of slicing past a literal prefix

The first draft of this classifier sliced past a fixed-length literal —
`header[len("Bearer "):]` — after checking `strings.HasPrefix(lower,
"bearer ")`. It looked right and it wasn't: `strings.TrimSpace` on the whole
header collapses a bare `"Bearer "` (scheme word plus one trailing space,
no token) down to `"Bearer"`, which no longer has a trailing space at all,
so the `HasPrefix` check silently misses it and the request falls through
to `ErrUnknownScheme` instead of the more accurate "bearer scheme,
empty token" error. `strings.Cut(header, " ")` sidesteps the whole class of
bug: it splits the scheme word from whatever follows regardless of how much
whitespace was in between or trimmed away, so `scheme == "bearer"` with an
empty `rest` is recognized as *that scheme's* malformed credential, not a
scheme nobody's heard of.

Precedence is the other load-bearing decision, and it is why this is a
tagless switch instead of an expression switch on scheme name: mTLS and the
API key header are checked *before* the scheme word is even parsed, because
a client presenting a certificate has already authenticated at a layer the
Authorization header can't override or spoof, and an API key header is a
distinct signal from whatever garbage might be sitting in `Authorization`
that request also happened to include. Only once both of those are ruled
out does the switch look at the scheme word at all.

Create `authscheme.go`:

```go
// Package authscheme classifies the authentication signal an API gateway
// receives — a mutual TLS client certificate, an X-API-Key header, or an
// Authorization header carrying Bearer or Basic credentials — using a
// tagless switch so precedence between signals is explicit and ordered.
package authscheme

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// ErrNoCredential marks a request that carried no authentication signal at
// all.
var ErrNoCredential = errors.New("authscheme: no credential presented")

// ErrMalformedCredential marks a recognized scheme whose credential doesn't
// parse: not valid base64, missing the user:pass separator, or an empty
// bearer token.
var ErrMalformedCredential = errors.New("authscheme: malformed credential")

// ErrUnknownScheme marks an Authorization header whose scheme this gateway
// doesn't support.
var ErrUnknownScheme = errors.New("authscheme: unknown scheme")

// Credential is the normalized result of classifying a request's auth
// signal, ready to hand to a scheme-specific validator.
type Credential struct {
	Scheme string // "mtls", "apikey", "bearer", or "basic"
	Value  string // decoded token / "user:pass" / API key; empty for mtls
}

// Classify picks one authentication signal from the request, in a fixed
// precedence order: a presented client certificate wins outright (mTLS is
// negotiated below the application layer and isn't spoofable via headers),
// then an X-API-Key header, then the Authorization header's own scheme.
func Classify(authHeader, apiKeyHeader string, mTLSPresent bool) (Credential, error) {
	header := strings.TrimSpace(authHeader)

	// Split into the scheme word and whatever follows it, rather than
	// slicing by a fixed-length literal prefix: that keeps the parse
	// correct regardless of how much whitespace separates them, and lets
	// an empty rest (a bare "Bearer" with nothing after it) be recognized
	// as *that scheme's* malformed credential instead of an unknown scheme.
	scheme, rest, _ := strings.Cut(header, " ")
	scheme = strings.ToLower(scheme)
	rest = strings.TrimSpace(rest)

	switch {
	case mTLSPresent:
		return Credential{Scheme: "mtls"}, nil
	case apiKeyHeader != "":
		return Credential{Scheme: "apikey", Value: apiKeyHeader}, nil
	case scheme == "bearer":
		if rest == "" {
			return Credential{}, fmt.Errorf("%w: empty bearer token", ErrMalformedCredential)
		}
		return Credential{Scheme: "bearer", Value: rest}, nil
	case scheme == "basic":
		decoded, err := base64.StdEncoding.DecodeString(rest)
		if err != nil {
			return Credential{}, fmt.Errorf("%w: not valid base64: %v", ErrMalformedCredential, err)
		}
		if !strings.Contains(string(decoded), ":") {
			return Credential{}, fmt.Errorf("%w: missing user:pass separator", ErrMalformedCredential)
		}
		return Credential{Scheme: "basic", Value: string(decoded)}, nil
	case header == "":
		return Credential{}, ErrNoCredential
	default:
		return Credential{}, fmt.Errorf("%w: %q", ErrUnknownScheme, header)
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	authscheme "example.com/api-auth-scheme-classifier"
)

func main() {
	type req struct {
		auth, apiKey string
		mtls         bool
	}
	reqs := []req{
		{"", "", true},
		{"", "sk_live_abc123", false},
		{"Bearer eyJhbGciOi", "", false},
		{"Basic dXNlcjpwYXNz", "", false},
		{"Digest username=\"bob\"", "", false},
		{"", "", false},
	}

	for _, r := range reqs {
		cred, err := authscheme.Classify(r.auth, r.apiKey, r.mtls)
		if err != nil {
			fmt.Printf("auth=%-24q apikey=%-16q mtls=%-5v -> error: %v\n", r.auth, r.apiKey, r.mtls, err)
			continue
		}
		fmt.Printf("auth=%-24q apikey=%-16q mtls=%-5v -> %+v\n", r.auth, r.apiKey, r.mtls, cred)
	}
}
```

Run `go run ./cmd/demo`, expected output:

```
auth=""                       apikey=""               mtls=true  -> {Scheme:mtls Value:}
auth=""                       apikey="sk_live_abc123" mtls=false -> {Scheme:apikey Value:sk_live_abc123}
auth="Bearer eyJhbGciOi"      apikey=""               mtls=false -> {Scheme:bearer Value:eyJhbGciOi}
auth="Basic dXNlcjpwYXNz"     apikey=""               mtls=false -> {Scheme:basic Value:user:pass}
auth="Digest username=\"bob\"" apikey=""               mtls=false -> error: authscheme: unknown scheme: "Digest username=\"bob\""
auth=""                       apikey=""               mtls=false -> error: authscheme: no credential presented
```

### Tests

`TestClassify` runs a table over mTLS taking precedence even when other
signals are present, an API key, a valid bearer token, a valid basic
credential (base64 of `user:pass`), an empty bearer token, non-base64 basic
input, base64 input missing the `:` separator, an unrecognized scheme, and
no credential at all — the malformed and unknown cases checked with
`errors.Is` against their respective sentinels.

Create `authscheme_test.go`:

```go
package authscheme

import (
	"errors"
	"testing"
)

func TestClassify(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		auth, apiKey string
		mtls         bool
		want         Credential
		wantErr      error
	}{
		{"mtls wins over everything else", "Bearer xyz", "sk_live_abc", true, Credential{Scheme: "mtls"}, nil},
		{"api key header", "", "sk_live_abc123", false, Credential{Scheme: "apikey", Value: "sk_live_abc123"}, nil},
		{"bearer token", "Bearer eyJhbGciOi", "", false, Credential{Scheme: "bearer", Value: "eyJhbGciOi"}, nil},
		{"basic credential", "Basic dXNlcjpwYXNz", "", false, Credential{Scheme: "basic", Value: "user:pass"}, nil},
		{"empty bearer token", "Bearer ", "", false, Credential{}, ErrMalformedCredential},
		{"basic not base64", "Basic ###", "", false, Credential{}, ErrMalformedCredential},
		{"basic missing separator", "Basic anVzdHVzZXI=", "", false, Credential{}, ErrMalformedCredential},
		{"unknown scheme", "Digest username=\"bob\"", "", false, Credential{}, ErrUnknownScheme},
		{"no credential at all", "", "", false, Credential{}, ErrNoCredential},
	}

	for _, tc := range tests {
		got, err := Classify(tc.auth, tc.apiKey, tc.mtls)
		if tc.wantErr != nil {
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("%s: Classify(...) error = %v, want errors.Is match for %v", tc.name, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: Classify(...) unexpected error: %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: Classify(...) = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}
```

Verify with:

```bash
go test -count=1 ./...
```

## Review

The classifier is correct when mTLS and the API key header always win over
whatever happens to be in `Authorization`, when a malformed credential for
a *recognized* scheme is distinguishable from a genuinely unrecognized
scheme, and when trimming whitespace never accidentally turns a
recognizable-but-empty credential into an unknown one. Carry this forward:
when a header's shape is "keyword, then a separator, then a variable-length
rest," reach for `strings.Cut` over slicing past a literal prefix length —
it survives the whitespace edge cases that fixed-length slicing quietly
gets wrong.

## Resources

- [Go Specification: Switch statements](https://go.dev/ref/spec#Switch_statements) — the tagless (expressionless) switch form.
- [strings.Cut](https://pkg.go.dev/strings#Cut) — splitting a scheme word from its credential.
- [RFC 7617: The 'Basic' HTTP Authentication Scheme](https://www.rfc-editor.org/rfc/rfc7617) — the base64 `user:pass` format this exercise parses.
- [RFC 6750: Bearer Token Usage](https://www.rfc-editor.org/rfc/rfc6750) — the Bearer scheme this exercise parses.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [20-queue-priority-level-inheritance.md](20-queue-priority-level-inheritance.md) | Next: [22-replica-lag-threshold-alerter.md](22-replica-lag-threshold-alerter.md)
