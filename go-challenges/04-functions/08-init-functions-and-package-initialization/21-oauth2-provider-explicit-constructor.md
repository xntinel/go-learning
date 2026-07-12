# Exercise 21: Killing Hidden Global State — OAuth2 Client Setup as a Constructor

**Nivel: Intermedio** — validacion rapida (un test corto).

OAuth2 client configuration — client ID, secret, redirect URL, scopes — is
one of the most common things a codebase stuffs into a package-level global
filled by `init()`. This exercise refactors that into an explicit
`New(...Option) (*Provider, error)` constructor with functional options and
an injected `getenv`, so building a `Provider` is deterministic, testable,
and produces no import-time side effect at all.

## What you'll build

```text
oauth2setup/                independent module: example.com/oauth2setup
  go.mod                     module example.com/oauth2setup
  provider.go                 Provider, Option, New(...Option) (*Provider, error); AuthCodeURL
  cmd/
    demo/
      main.go                 builds a Provider from the real process env and prints an auth URL
  provider_test.go             fake getenv table + joined missing-credential error + independence
```

Files: `provider.go`, `cmd/demo/main.go`, `provider_test.go`.
Implement: `New(...Option) (*Provider, error)` reading through an injected `getenv func(string) string`, functional options for redirect URL/scopes/endpoints, required-credential validation aggregated via `errors.Join`, and no `init()` and no package global.
Test: a fake `getenv` yields a populated `Provider` with no reliance on the process env; missing required credentials return a joined error each matchable with `errors.Is`; two `New` calls with different env produce independent `Provider`s; `AuthCodeURL` embeds client ID, redirect URI, scopes, and state.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/08-init-functions-and-package-initialization/21-oauth2-provider-explicit-constructor/cmd/demo
cd go-solutions/04-functions/08-init-functions-and-package-initialization/21-oauth2-provider-explicit-constructor
go mod edit -go=1.24
```

### The anti-pattern and the fix

The starting point (shown here only as an illustrative, non-compiled block)
is the code this exercise exists to eliminate:

```text
// WRONG: hidden global filled from the environment at import time.
var OAuthClientID string
var OAuthClientSecret string

func init() {
	OAuthClientID = os.Getenv("OAUTH_CLIENT_ID")         // runs before flags/config exist
	OAuthClientSecret = os.Getenv("OAUTH_CLIENT_SECRET")  // cannot be injected in a test
	// a missing secret has nowhere to report an error except a panic
}
```

Every problem this chapter has named converges here again, in a new domain.
The read happens at import time and freezes whatever the process environment
was; the values are package globals, so no two tests can each exercise a
different client registration; and validation has nowhere to send a failure
except a panic during package load, with no chance for the caller to decide
how to handle it.

`New(opts ...Option) (*Provider, error)` reads through an injected `getenv
func(string) string` instead of calling `os.Getenv` directly, so a test
passes a fake map-backed lookup and never touches the real environment.
Functional options (`WithRedirectURL`, `WithScopes`, `WithEndpoints`)
customize the provider without a forest of constructor parameters. Required
credentials are validated with `errors.Join`, so one `New` call reports every
missing value at once. There is no `init()` and no package global: each call
to `New` returns an independent `*Provider` the caller owns.

Create `provider.go`:

```go
// provider.go
// Package oauth2setup builds an OAuth2 client Provider through an explicit
// constructor rather than reading credentials from the environment inside
// an init(). There is no package-level global and no init in this file:
// every Provider is returned by New and owned by its caller.
package oauth2setup

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// Sentinel errors for each required credential, so a caller can match a
// specific missing value with errors.Is even though New joins them.
var (
	ErrMissingClientID     = errors.New("OAUTH_CLIENT_ID is required")
	ErrMissingClientSecret = errors.New("OAUTH_CLIENT_SECRET is required")
)

// Provider holds everything needed to build an authorization URL for one
// OAuth2 client registration.
type Provider struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
	AuthURL      string
	TokenURL     string
	Scopes       []string
}

// options carries the knobs New reads. getenv is injected so tests supply a
// fake lookup instead of mutating the process environment.
type options struct {
	getenv      func(string) string
	authURL     string
	tokenURL    string
	redirectURL string
	scopes      []string
}

// Option customizes a New call.
type Option func(*options)

// WithGetenv injects the environment lookup. Defaults to os.Getenv.
func WithGetenv(f func(string) string) Option {
	return func(o *options) { o.getenv = f }
}

// WithRedirectURL sets the callback URL registered with the provider.
func WithRedirectURL(u string) Option {
	return func(o *options) { o.redirectURL = u }
}

// WithScopes sets the OAuth2 scopes requested during authorization.
func WithScopes(scopes ...string) Option {
	return func(o *options) { o.scopes = scopes }
}

// WithEndpoints overrides the default authorization and token URLs, for
// providers other than the built-in default.
func WithEndpoints(authURL, tokenURL string) Option {
	return func(o *options) { o.authURL, o.tokenURL = authURL, tokenURL }
}

// New builds a Provider from the injected environment. It aggregates every
// validation failure with errors.Join instead of returning only the first,
// and has no import-time side effect: nothing reads the environment until
// you call New.
func New(opts ...Option) (*Provider, error) {
	o := options{
		getenv:   os.Getenv,
		authURL:  "https://provider.example.com/oauth2/authorize",
		tokenURL: "https://provider.example.com/oauth2/token",
	}
	for _, opt := range opts {
		opt(&o)
	}

	p := &Provider{
		ClientID:     o.getenv("OAUTH_CLIENT_ID"),
		ClientSecret: o.getenv("OAUTH_CLIENT_SECRET"),
		RedirectURL:  o.redirectURL,
		AuthURL:      o.authURL,
		TokenURL:     o.tokenURL,
		Scopes:       o.scopes,
	}

	var errs []error
	if p.ClientID == "" {
		errs = append(errs, ErrMissingClientID)
	}
	if p.ClientSecret == "" {
		errs = append(errs, ErrMissingClientSecret)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return p, nil
}

// AuthCodeURL builds the URL the user is redirected to in order to begin
// the authorization-code flow, embedding state for CSRF protection.
func (p *Provider) AuthCodeURL(state string) string {
	v := url.Values{}
	v.Set("client_id", p.ClientID)
	v.Set("redirect_uri", p.RedirectURL)
	v.Set("response_type", "code")
	v.Set("state", state)
	if len(p.Scopes) > 0 {
		v.Set("scope", strings.Join(p.Scopes, " "))
	}
	return fmt.Sprintf("%s?%s", p.AuthURL, v.Encode())
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"os"

	"example.com/oauth2setup"
)

func main() {
	os.Setenv("OAUTH_CLIENT_ID", "demo-client")
	os.Setenv("OAUTH_CLIENT_SECRET", "demo-secret")

	provider, err := oauth2setup.New(
		oauth2setup.WithRedirectURL("https://app.example.com/callback"),
		oauth2setup.WithScopes("profile", "email"),
	)
	if err != nil {
		fmt.Println("provider error:", err)
		return
	}
	fmt.Println(provider.AuthCodeURL("xyz123"))

	if _, err := oauth2setup.New(oauth2setup.WithGetenv(func(string) string { return "" })); err != nil {
		fmt.Println("empty env rejected:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
https://provider.example.com/oauth2/authorize?client_id=demo-client&redirect_uri=https%3A%2F%2Fapp.example.com%2Fcallback&response_type=code&scope=profile+email&state=xyz123
empty env rejected: OAUTH_CLIENT_ID is required
OAUTH_CLIENT_SECRET is required
```

### Tests

Create `provider_test.go`:

```go
// provider_test.go
package oauth2setup

import (
	"errors"
	"strings"
	"testing"
)

func fakeEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestNewFromFakeEnv(t *testing.T) {
	t.Parallel()

	p, err := New(
		WithGetenv(fakeEnv(map[string]string{
			"OAUTH_CLIENT_ID":     "abc",
			"OAUTH_CLIENT_SECRET": "shh",
		})),
		WithRedirectURL("https://app.example.com/callback"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if p.ClientID != "abc" || p.ClientSecret != "shh" || p.RedirectURL != "https://app.example.com/callback" {
		t.Fatalf("provider = %+v", p)
	}
}

func TestMissingCredentialsAreJoined(t *testing.T) {
	t.Parallel()

	_, err := New(WithGetenv(fakeEnv(map[string]string{})))
	if err == nil {
		t.Fatal("New with empty env returned nil error")
	}
	if !errors.Is(err, ErrMissingClientID) {
		t.Errorf("joined error missing ErrMissingClientID: %v", err)
	}
	if !errors.Is(err, ErrMissingClientSecret) {
		t.Errorf("joined error missing ErrMissingClientSecret: %v", err)
	}
}

func TestTwoProvidersAreIndependent(t *testing.T) {
	t.Parallel()

	a, err := New(WithGetenv(fakeEnv(map[string]string{"OAUTH_CLIENT_ID": "a", "OAUTH_CLIENT_SECRET": "sa"})))
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(WithGetenv(fakeEnv(map[string]string{"OAUTH_CLIENT_ID": "b", "OAUTH_CLIENT_SECRET": "sb"})))
	if err != nil {
		t.Fatal(err)
	}
	if a.ClientID == b.ClientID || a == b {
		t.Fatalf("providers not independent: a=%+v b=%+v", a, b)
	}
}

func TestAuthCodeURLIncludesScopesAndState(t *testing.T) {
	t.Parallel()

	p, err := New(
		WithGetenv(fakeEnv(map[string]string{"OAUTH_CLIENT_ID": "abc", "OAUTH_CLIENT_SECRET": "shh"})),
		WithRedirectURL("https://app.example.com/callback"),
		WithScopes("profile", "email"),
	)
	if err != nil {
		t.Fatal(err)
	}
	got := p.AuthCodeURL("xyz123")
	for _, want := range []string{"client_id=abc", "state=xyz123", "scope=profile", "redirect_uri="} {
		if !strings.Contains(got, want) {
			t.Errorf("AuthCodeURL() = %q, want it to contain %q", got, want)
		}
	}
}
```

## Review

`TestTwoProvidersAreIndependent` is the property a global would destroy:
building two `Provider`s with different client IDs in the same test is only
possible because `New` never touches shared state. `TestMissingCredentialsAreJoined`
proves the aggregation contract — an operator learns about a missing client
ID and a missing secret in a single failed `New` call, not one restart at a
time. `TestAuthCodeURLIncludesScopesAndState` guards the actual OAuth2
mechanics: a redirect URL missing the `state` parameter is a real CSRF
vulnerability, so the test checks it is present, not just that the URL looks
plausible.

The mistake to avoid is the one the anti-pattern block embodies: reading
`os.Getenv` at import time, storing credentials in a package global, and
panicking on the first missing value instead of reporting all of them.
Confirm no `init()` remains — the whole point is that nothing runs until a
caller calls `New`.

## Resources

- [errors.Join](https://pkg.go.dev/errors#Join) — aggregate multiple validation failures into one error that `errors.Is` can unwrap.
- [Functional options (Rob Pike)](https://commandcenter.blogspot.com/2014/01/self-referential-functions-and-design.html) — the self-referential option pattern used for `WithRedirectURL`, `WithScopes`, `WithEndpoints`.
- [RFC 6749 — The OAuth 2.0 Authorization Framework](https://www.rfc-editor.org/rfc/rfc6749) — defines the authorization-code flow `AuthCodeURL` builds a request for.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [20-tls-certificates-loader-and-validator.md](20-tls-certificates-loader-and-validator.md) | Next: [22-message-queue-codec-blank-import-registry.md](22-message-queue-codec-blank-import-registry.md)
