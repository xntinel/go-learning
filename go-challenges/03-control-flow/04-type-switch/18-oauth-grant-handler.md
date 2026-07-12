# Exercise 18: Dispatch OAuth 2.0 Grant Type Handlers by Request Payload

**Nivel: Intermedio** — validacion rapida (un test corto).

An authorization server supports three OAuth 2.0 grant types defined by RFC
6749: `authorization_code` for a browser-based login flow, `client_credentials`
for machine-to-machine calls with no end user, and `refresh_token` for
renewing an existing session. Each grant carries entirely different required
parameters and a different issuance rule, so once the request body is
decoded into one of three Go types, a type switch is the natural way to
route it to the handler that actually knows how to validate and issue a
token for that grant.

## What you'll build

```text
oauth-grant-handler/        independent module: example.com/oauth-grant-handler
  go.mod                     go 1.24
  oauthgrant.go              IssueToken(grant any) (Token, error)
  cmd/
    demo/
      main.go                issues tokens for two grant kinds, rejects a third
  oauthgrant_test.go          table test over every grant kind plus invalid requests
```

- Files: `oauthgrant.go`, `cmd/demo/main.go`, `oauthgrant_test.go`.
- Implement: `IssueToken(grant any) (Token, error)`, type-switching on
  `AuthorizationCodeGrant`, `ClientCredentialsGrant`, and
  `RefreshTokenGrant`, each with an injected issuance function.
- Test: each grant kind succeeding, each grant kind missing a required
  field, and an unsupported grant type.

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/04-type-switch/18-oauth-grant-handler/cmd/demo
cd go-solutions/03-control-flow/04-type-switch/18-oauth-grant-handler
go mod edit -go=1.24
```

A shared `GrantHandler` interface for all three grants would either force
irrelevant fields onto every grant (a `RefreshTokenGrant` has no
`RedirectURI`, an `AuthorizationCodeGrant` has no `RefreshToken`) or hide
which fields a given grant actually requires behind a generic
`map[string]string`. The type switch keeps each grant's contract explicit
in its own struct and its own validation branch: `authorization_code`
requires a `Code`, a `RedirectURI`, and a `ClientID` because the
authorization server must confirm the code was issued for that exact
redirect URI and client, closing the door on a code intercepted in transit
being replayed against a different client. `client_credentials` skips the
user entirely and validates a client secret instead. `refresh_token` has no
code or redirect at all — it trusts the refresh token itself as the
credential. Each grant's issuance function (`Exchange`, `Authenticate`,
`Renew`) is injected so the router's own dispatch and validation logic is
what gets tested, not a token store.

Create `oauthgrant.go`:

```go
package oauthgrant

import (
	"errors"
	"fmt"
)

// ErrInvalidGrant is the sentinel for a grant request that fails its
// type-specific validation before a token is ever issued.
var ErrInvalidGrant = errors.New("invalid grant request")

// Token is the normalized result of any successful grant.
type Token struct {
	AccessToken string
	ExpiresIn   int
}

// AuthorizationCodeGrant exchanges a one-time code from the authorization
// step for a token. Exchange is injected so tests never call a real code
// store.
type AuthorizationCodeGrant struct {
	Code        string
	RedirectURI string
	ClientID    string
	Exchange    func(code, redirectURI, clientID string) (Token, error)
}

// ClientCredentialsGrant issues a token directly to a confidential client
// with no end user involved.
type ClientCredentialsGrant struct {
	ClientID     string
	ClientSecret string
	Authenticate func(clientID, clientSecret string) (Token, error)
}

// RefreshTokenGrant exchanges a previously issued refresh token for a new
// access token.
type RefreshTokenGrant struct {
	RefreshToken string
	ClientID     string
	Renew        func(refreshToken, clientID string) (Token, error)
}

// IssueToken dispatches a decoded grant request to its handler by concrete
// type. RFC 6749 defines each grant type with its own required parameters
// and its own validation and issuance logic (an authorization code is
// single-use and bound to a redirect URI; client credentials skip the user
// entirely; a refresh token has no code or redirect at all), so a shared
// interface would either force irrelevant fields onto every grant or hide
// which fields a given grant actually requires. The type switch keeps each
// grant's contract explicit.
func IssueToken(grant any) (Token, error) {
	switch g := grant.(type) {
	case AuthorizationCodeGrant:
		if g.Code == "" || g.RedirectURI == "" || g.ClientID == "" {
			return Token{}, fmt.Errorf("%w: authorization_code requires code, redirect_uri, and client_id", ErrInvalidGrant)
		}
		return g.Exchange(g.Code, g.RedirectURI, g.ClientID)
	case ClientCredentialsGrant:
		if g.ClientID == "" || g.ClientSecret == "" {
			return Token{}, fmt.Errorf("%w: client_credentials requires client_id and client_secret", ErrInvalidGrant)
		}
		return g.Authenticate(g.ClientID, g.ClientSecret)
	case RefreshTokenGrant:
		if g.RefreshToken == "" || g.ClientID == "" {
			return Token{}, fmt.Errorf("%w: refresh_token requires refresh_token and client_id", ErrInvalidGrant)
		}
		return g.Renew(g.RefreshToken, g.ClientID)
	default:
		return Token{}, fmt.Errorf("%w: unsupported grant type %T", ErrInvalidGrant, grant)
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/oauth-grant-handler"
)

func main() {
	grants := []any{
		oauthgrant.AuthorizationCodeGrant{
			Code: "abc", RedirectURI: "https://app/callback", ClientID: "web",
			Exchange: func(code, redirectURI, clientID string) (oauthgrant.Token, error) {
				return oauthgrant.Token{AccessToken: "at-1", ExpiresIn: 3600}, nil
			},
		},
		oauthgrant.ClientCredentialsGrant{
			ClientID: "svc", ClientSecret: "shh",
			Authenticate: func(clientID, clientSecret string) (oauthgrant.Token, error) {
				return oauthgrant.Token{AccessToken: "at-2", ExpiresIn: 600}, nil
			},
		},
		"password",
	}
	for _, g := range grants {
		token, err := oauthgrant.IssueToken(g)
		if err != nil {
			fmt.Printf("rejected: %v\n", err)
			continue
		}
		fmt.Printf("issued: %s (expires in %ds)\n", token.AccessToken, token.ExpiresIn)
	}
}
```

```bash
go run ./cmd/demo
```

Expected output:

```text
issued: at-1 (expires in 3600s)
issued: at-2 (expires in 600s)
rejected: invalid grant request: unsupported grant type string
```

### Tests

Create `oauthgrant_test.go`:

```go
package oauthgrant

import (
	"errors"
	"testing"
)

func TestIssueToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		grant   any
		want    Token
		wantErr bool
	}{
		{
			name: "authorization code succeeds",
			grant: AuthorizationCodeGrant{
				Code: "abc", RedirectURI: "https://app/callback", ClientID: "web",
				Exchange: func(code, redirectURI, clientID string) (Token, error) {
					return Token{AccessToken: "at-1", ExpiresIn: 3600}, nil
				},
			},
			want: Token{AccessToken: "at-1", ExpiresIn: 3600},
		},
		{
			name: "authorization code missing redirect uri is invalid",
			grant: AuthorizationCodeGrant{
				Code: "abc", ClientID: "web",
				Exchange: func(string, string, string) (Token, error) { return Token{}, nil },
			},
			wantErr: true,
		},
		{
			name: "client credentials succeeds",
			grant: ClientCredentialsGrant{
				ClientID: "svc", ClientSecret: "shh",
				Authenticate: func(clientID, clientSecret string) (Token, error) {
					return Token{AccessToken: "at-2", ExpiresIn: 600}, nil
				},
			},
			want: Token{AccessToken: "at-2", ExpiresIn: 600},
		},
		{
			name: "client credentials missing secret is invalid",
			grant: ClientCredentialsGrant{
				ClientID:     "svc",
				Authenticate: func(string, string) (Token, error) { return Token{}, nil },
			},
			wantErr: true,
		},
		{
			name: "refresh token succeeds",
			grant: RefreshTokenGrant{
				RefreshToken: "rt-1", ClientID: "web",
				Renew: func(refreshToken, clientID string) (Token, error) {
					return Token{AccessToken: "at-3", ExpiresIn: 3600}, nil
				},
			},
			want: Token{AccessToken: "at-3", ExpiresIn: 3600},
		},
		{
			name: "refresh token missing client id is invalid",
			grant: RefreshTokenGrant{
				RefreshToken: "rt-1",
				Renew:        func(string, string) (Token, error) { return Token{}, nil },
			},
			wantErr: true,
		},
		{
			name:    "unsupported grant type",
			grant:   "password",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := IssueToken(tt.grant)
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidGrant) {
					t.Fatalf("IssueToken err = %v, want ErrInvalidGrant", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("IssueToken unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("IssueToken = %+v, want %+v", got, tt.want)
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

Each grant validates its own required fields before ever calling its
injected issuance function, so a malformed request never reaches a code
exchange, a client authentication check, or a token renewal — the
validation is cheap and the issuance functions are what would talk to a
database or another service in a real server. The most consequential design
choice here is what is *not* in this exercise: a `password` grant (Resource
Owner Password Credentials) is deliberately unsupported, since RFC 6749
grants that hand a client the user's actual password are considered a
legacy anti-pattern the OAuth working group has since recommended against.
The `default` branch treating it as `ErrUnsupportedGrant` rather than a
silent fallback to some other grant's logic is the switch doing exactly
its job: an unrecognized grant type must fail loudly, not be guessed at.

## Resources

- [RFC 6749: The OAuth 2.0 Authorization Framework](https://www.rfc-editor.org/rfc/rfc6749)
- [RFC 6749 Section 4: Obtaining Authorization](https://www.rfc-editor.org/rfc/rfc6749#section-4)
- [Go Specification: Type switches](https://go.dev/ref/spec#Type_switches)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [17-health-check-router.md](17-health-check-router.md) | Next: [19-retry-backoff-picker.md](19-retry-backoff-picker.md)
