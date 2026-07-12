# Exercise 2: Extract a Bearer Token from the Authorization Header

Auth middleware runs on every authenticated request: it must pull the credential
out of the `Authorization` header before any token verification happens. RFC 9110
defines the auth-scheme as case-insensitive, so the parser has to fold the scheme
while leaving the token bytes exactly as sent.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
bearer/                         independent module: example.com/bearer
  go.mod                        go 1.26
  bearer.go                     ParseBearer + sentinel errors
  bearer_test.go                table test over valid, spoofed, and malformed headers
  cmd/
    demo/
      main.go                   runnable demo over a handful of headers
```

Files: `bearer.go`, `bearer_test.go`, `cmd/demo/main.go`.
Implement: `ParseBearer(header string) (token string, err error)`.
Test: `Bearer`/`bearer`/`BEARER` all accepted, extra space rejected, wrong scheme
rejected, empty and scheme-only rejected, a token with `=` and mixed case
returned verbatim; sentinels via `errors.Is`.
Verify: `go test -count=1 -race ./...`

### Why Cut and EqualFold, and why the token is untouched

The header is `scheme SP credentials`. `strings.Cut(header, " ")` splits on the
first space and returns `(scheme, credentials, found)` in one scan; `found ==
false` means there was no space at all (`"Bearer"` with no token), which is a
distinct failure from an empty token. Compare the scheme with
`strings.EqualFold(scheme, "Bearer")`: the scheme is case-insensitive by spec, so
`bearer` and `BEARER` are valid, and folding is a single allocation-free pass. A
case-sensitive `scheme == "Bearer"` is an interoperability bug that rejects
conforming clients.

The credential is the opposite: it is opaque and case-significant. A Bearer token
is a `b64token` per RFC 6750 and may contain `=` padding and mixed case; the
parser must return it byte-for-byte. So we fold only the scheme and never touch
the token.

The one subtlety is the space. RFC 9110 uses a single space between scheme and
credentials. `Bearer  abc` (two spaces) leaves the credentials as `" abc"` with a
leading space after the cut — malformed. We detect that by rejecting a credential
that starts with a space rather than silently trimming it, because trimming would
paper over a malformed header. An outer `TrimSpace` on the whole header tolerates
incidental leading/trailing whitespace from the transport, but the internal
structure must be exact.

Create `bearer.go`:

```go
package bearer

import (
	"fmt"
	"strings"
)

// ErrNotBearer means the auth-scheme is present but is not "Bearer".
var ErrNotBearer = fmt.Errorf("authorization: not a Bearer scheme")

// ErrEmptyToken means the scheme is Bearer but no credential follows.
var ErrEmptyToken = fmt.Errorf("authorization: empty bearer token")

// ErrMalformed means the header is empty or not "scheme SP credential".
var ErrMalformed = fmt.Errorf("authorization: malformed header")

// ParseBearer extracts the credential from an Authorization header. The scheme
// is compared case-insensitively per RFC 9110; the token is returned verbatim.
func ParseBearer(header string) (string, error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return "", ErrMalformed
	}
	scheme, token, found := strings.Cut(header, " ")
	if !found {
		return "", fmt.Errorf("no credential after scheme %q: %w", scheme, ErrMalformed)
	}
	if !strings.EqualFold(scheme, "Bearer") {
		return "", fmt.Errorf("scheme %q: %w", scheme, ErrNotBearer)
	}
	if strings.HasPrefix(token, " ") {
		return "", fmt.Errorf("extra whitespace before token: %w", ErrMalformed)
	}
	if token == "" {
		return "", ErrEmptyToken
	}
	return token, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/bearer"
)

func main() {
	headers := []string{
		"Bearer abc123",
		"bearer aB=cD",
		"Basic dXNlcjpwYXNz",
		"Bearer",
	}
	for _, h := range headers {
		token, err := bearer.ParseBearer(h)
		if err != nil {
			fmt.Printf("%-24q -> error: %v\n", h, err)
			continue
		}
		fmt.Printf("%-24q -> token %q\n", h, token)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
"Bearer abc123"          -> token "abc123"
"bearer aB=cD"           -> token "aB=cD"
"Basic dXNlcjpwYXNz"     -> error: scheme "Basic": authorization: not a Bearer scheme
"Bearer"                 -> error: no credential after scheme "Bearer": authorization: malformed header
```

### Tests

Create `bearer_test.go`:

```go
package bearer

import (
	"errors"
	"fmt"
	"testing"
)

func TestParseBearer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		header  string
		want    string
		wantErr error
	}{
		{name: "canonical", header: "Bearer abc123", want: "abc123"},
		{name: "lower scheme", header: "bearer abc123", want: "abc123"},
		{name: "upper scheme", header: "BEARER abc123", want: "abc123"},
		{name: "token verbatim", header: "Bearer aB=cD.eF", want: "aB=cD.eF"},
		{name: "extra space", header: "Bearer  abc", wantErr: ErrMalformed},
		{name: "wrong scheme", header: "Basic abc", wantErr: ErrNotBearer},
		{name: "empty header", header: "", wantErr: ErrMalformed},
		{name: "scheme only", header: "Bearer", wantErr: ErrMalformed},
		{name: "outer whitespace", header: "  Bearer abc  ", want: "abc"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseBearer(tc.header)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ParseBearer(%q) err = %v, want %v", tc.header, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseBearer(%q) unexpected err: %v", tc.header, err)
			}
			if got != tc.want {
				t.Fatalf("ParseBearer(%q) = %q, want %q", tc.header, got, tc.want)
			}
		})
	}
}

func ExampleParseBearer() {
	token, _ := ParseBearer("bearer aB=cD")
	fmt.Println(token)
	// Output: aB=cD
}
```

## Review

The parser is correct when the scheme comparison is case-insensitive and the
token survives byte-for-byte, including `=` and mixed case. The two traps this
lesson targets: comparing the scheme with `==` (which rejects `bearer` from a
conforming client) and lower-casing the credential along with the scheme (which
corrupts an opaque token). Note that `Cut`'s `found` bool is what separates
"scheme only, no token" (malformed) from "empty token", and that rejecting a
leading space in the credential — rather than trimming it — is what makes
`Bearer  abc` a hard error. Confirm with `go test -race`.

## Resources

- [RFC 9110 Authorization](https://www.rfc-editor.org/rfc/rfc9110#name-authorization) — the auth-scheme is case-insensitive.
- [RFC 6750 Bearer Token Usage](https://www.rfc-editor.org/rfc/rfc6750#section-2.1) — `Authorization: Bearer <b64token>`.
- [strings.Cut](https://pkg.go.dev/strings#Cut) and [strings.EqualFold](https://pkg.go.dev/strings#EqualFold).

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-username-normalizer.md](01-username-normalizer.md) | Next: [03-content-type-header-parser.md](03-content-type-header-parser.md)
