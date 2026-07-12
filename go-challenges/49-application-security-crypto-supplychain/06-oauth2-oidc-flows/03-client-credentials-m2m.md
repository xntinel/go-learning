# Exercise 3: Service-to-Service Auth with the Client Credentials Grant

Most backend OAuth2 code is not a login page — it is one service authenticating to
another. This exercise builds that machine-to-machine flow: a component that
obtains an access token with the `client_credentials` grant and hands callers an
`http.Client` that attaches and refreshes the bearer token transparently, so no
handler ever touches token expiry.

This module is self-contained: an `httptest` token endpoint and an `httptest`
protected resource stand in for the IdP and the upstream API, so the flow is
tested with no network.

## What you'll build

```text
m2mauth/                      independent module: example.com/m2mauth
  go.mod                      go 1.26; requires golang.org/x/oauth2
  m2mauth.go                  Client{Token, TokenSource, HTTPClient}; AudienceParams
  cmd/
    demo/
      main.go                 fetch once, call an API three times, show token reuse
  m2mauth_test.go             hit-counting token server; reuse vs refetch; AuthStyle; bearer attach
```

- Files: `m2mauth.go`, `cmd/demo/main.go`, `m2mauth_test.go`.
- Implement: a `Client` wrapping `clientcredentials.Config` that exposes the auto-refreshing `TokenSource` and an `HTTPClient` that attaches the bearer token; plus `AudienceParams` for the `audience` endpoint parameter.
- Test: assert the token is fetched once and reused while valid, refetched after expiry, that `AuthStyleInHeader` uses HTTP Basic while `AuthStyleInParams` posts the secret, that `EndpointParams` reach the endpoint, and that `HTTPClient` attaches the bearer automatically.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/49-application-security-crypto-supplychain/06-oauth2-oidc-flows/03-client-credentials-m2m/cmd/demo
cd go-solutions/49-application-security-crypto-supplychain/06-oauth2-oidc-flows/03-client-credentials-m2m
go get golang.org/x/oauth2@latest
```

### Let the token source own the lifecycle

The `client_credentials` grant is the simplest OAuth2 flow: no user, no browser,
no ID token. The client POSTs its id and secret (plus scope and any endpoint
params) to the token endpoint and gets back a short-lived access token
representing the *service*. The interesting engineering is not fetching the token
once — it is never having to think about expiry again.

`clientcredentials.Config.TokenSource(ctx)` returns an `oauth2.TokenSource` that
memoizes the token and refreshes it only when it has expired. `Token.Valid()`
already subtracts a small early-expiry buffer (about ten seconds), so a token
that is nominally valid but about to expire is refreshed proactively rather than
used and rejected. `Config.Client(ctx)` goes one step further: it returns an
`*http.Client` whose transport calls that same token source before each request
and sets the `Authorization: Bearer` header for you. Downstream code just makes
ordinary HTTP calls; the bearer token is attached and refreshed underneath. This
is why the correct answer is almost never a hand-written `if token.Expiry.Before(now)`
check scattered across handlers — that pattern races under concurrency and a
single shared token source does not.

`AuthStyle` decides how the client presents its credentials. `AuthStyleInHeader`
sends them as HTTP Basic; `AuthStyleInParams` puts `client_id` and `client_secret`
in the POST body. The zero value auto-detects by trying one and, on failure,
retrying the other — correct but costing an extra round trip and a puzzling 401 in
the IdP logs the first time. Pin it to what your IdP documents. `EndpointParams`
carries extra fields many IdPs require to scope the token, most commonly
`audience`; `AudienceParams` is a tiny helper that builds it.

Create `m2mauth.go`:

```go
package m2mauth

import (
	"context"
	"net/http"
	"net/url"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

// AudienceParams builds the EndpointParams many IdPs require to scope a
// client-credentials token to a specific upstream API.
func AudienceParams(audience string) url.Values {
	return url.Values{"audience": {audience}}
}

// Client authenticates a service to an upstream API with the client_credentials
// grant. Its token source auto-refreshes, so callers never handle expiry.
type Client struct {
	cfg *clientcredentials.Config
}

// New builds a Client. style should match what the IdP expects
// (oauth2.AuthStyleInHeader or oauth2.AuthStyleInParams); params may be nil.
func New(clientID, clientSecret, tokenURL string, scopes []string, params url.Values, style oauth2.AuthStyle) *Client {
	return &Client{cfg: &clientcredentials.Config{
		ClientID:       clientID,
		ClientSecret:   clientSecret,
		TokenURL:       tokenURL,
		Scopes:         scopes,
		EndpointParams: params,
		AuthStyle:      style,
	}}
}

// Token fetches or reuses an access token.
func (c *Client) Token(ctx context.Context) (*oauth2.Token, error) {
	return c.cfg.Token(ctx)
}

// TokenSource returns the auto-refreshing token source: it reuses the token
// until it expires, then fetches a new one, concurrency-safe.
func (c *Client) TokenSource(ctx context.Context) oauth2.TokenSource {
	return c.cfg.TokenSource(ctx)
}

// HTTPClient returns an *http.Client that attaches and refreshes the bearer
// token on every request transparently.
func (c *Client) HTTPClient(ctx context.Context) *http.Client {
	return c.cfg.Client(ctx)
}
```

### The runnable demo

The demo stands up a token endpoint and a protected API, both in-process. It calls
the API three times through the auto-refreshing client and prints the hit count on
the token endpoint — which is one, because the first token is reused for all three
calls.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"

	"golang.org/x/oauth2"

	"example.com/m2mauth"
)

func main() {
	var hits int32
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"access_token":"at-m2m-123","token_type":"Bearer","expires_in":3600}`)
	}))
	defer idp.Close()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "authorized (token=%s)", strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	}))
	defer api.Close()

	svc := m2mauth.New("svc-a", "s3cret", idp.URL+"/token",
		[]string{"orders.read"}, m2mauth.AudienceParams("https://api.example.com"),
		oauth2.AuthStyleInHeader)
	client := svc.HTTPClient(context.Background())

	var body string
	for range 3 {
		resp, err := client.Get(api.URL)
		if err != nil {
			panic(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		body = string(b)
	}
	fmt.Println("resource response:", body)
	fmt.Println("token endpoint hits:", atomic.LoadInt32(&hits))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
resource response: authorized (token=at-m2m-123)
token endpoint hits: 1
```

### Tests

The tests use an `httptest` token endpoint that counts hits. With a long TTL, two
`Token()` calls hit it once (reuse); with a one-second TTL they hit it twice,
because the ten-second early-expiry buffer treats a one-second token as already
expired and forces a refetch — a deterministic way to prove refresh without
sleeping. Two more tests capture the token request to assert `AuthStyleInHeader`
sends HTTP Basic while `AuthStyleInParams` posts the secret and the `audience`
and `scope` params. A final test points `HTTPClient` at a protected resource that
demands the exact bearer token.

Create `m2mauth_test.go`:

```go
package m2mauth

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"golang.org/x/oauth2"
)

// countingTokenServer returns a token with the given TTL and counts requests.
func countingTokenServer(expiresIn int, hits *int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"at-xyz","token_type":"Bearer","expires_in":%d}`, expiresIn)
	}))
}

func TestReuseWhileValid(t *testing.T) {
	t.Parallel()
	var hits int32
	ts := countingTokenServer(3600, &hits)
	defer ts.Close()

	c := New("svc", "secret", ts.URL, []string{"orders.read"}, nil, oauth2.AuthStyleInHeader)
	src := c.TokenSource(context.Background())
	for range 3 {
		if _, err := src.Token(); err != nil {
			t.Fatal(err)
		}
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("token endpoint hits = %d, want 1 (reused while valid)", got)
	}
}

func TestRefetchAfterExpiry(t *testing.T) {
	t.Parallel()
	var hits int32
	ts := countingTokenServer(1, &hits) // 1s TTL < 10s early-expiry buffer -> always stale
	defer ts.Close()

	c := New("svc", "secret", ts.URL, nil, nil, oauth2.AuthStyleInHeader)
	src := c.TokenSource(context.Background())
	for range 2 {
		if _, err := src.Token(); err != nil {
			t.Fatal(err)
		}
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("token endpoint hits = %d, want 2 (short TTL forces refetch)", got)
	}
}

// capture records what the token endpoint saw, delivered over a channel so the
// race detector sees a proper happens-before with the test goroutine.
type capture struct {
	basic      bool
	id, secret string
	form       url.Values
}

func capturingTokenServer(ch chan<- capture) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		id, secret, ok := r.BasicAuth()
		ch <- capture{basic: ok, id: id, secret: secret, form: r.PostForm}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"at-xyz","token_type":"Bearer","expires_in":3600}`)
	}))
}

func TestAuthStyleInHeader(t *testing.T) {
	t.Parallel()
	ch := make(chan capture, 1)
	ts := capturingTokenServer(ch)
	defer ts.Close()

	c := New("svc-a", "s3cret", ts.URL, nil, nil, oauth2.AuthStyleInHeader)
	if _, err := c.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := <-ch
	if !got.basic {
		t.Fatal("AuthStyleInHeader: expected HTTP Basic auth")
	}
	if got.id != "svc-a" || got.secret != "s3cret" {
		t.Errorf("basic auth = %q/%q, want svc-a/s3cret", got.id, got.secret)
	}
}

func TestAuthStyleInParams(t *testing.T) {
	t.Parallel()
	ch := make(chan capture, 1)
	ts := capturingTokenServer(ch)
	defer ts.Close()

	c := New("svc-a", "s3cret", ts.URL, []string{"orders.read"},
		AudienceParams("https://api.example.com"), oauth2.AuthStyleInParams)
	if _, err := c.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := <-ch
	if got.basic {
		t.Error("AuthStyleInParams: did not expect HTTP Basic auth")
	}
	if got.form.Get("client_id") != "svc-a" || got.form.Get("client_secret") != "s3cret" {
		t.Errorf("post body creds = %q/%q, want svc-a/s3cret",
			got.form.Get("client_id"), got.form.Get("client_secret"))
	}
	if got.form.Get("audience") != "https://api.example.com" {
		t.Errorf("audience = %q, want https://api.example.com", got.form.Get("audience"))
	}
	if got.form.Get("scope") != "orders.read" {
		t.Errorf("scope = %q, want orders.read", got.form.Get("scope"))
	}
}

func TestHTTPClientAttachesBearer(t *testing.T) {
	t.Parallel()
	var hits int32
	idp := countingTokenServer(3600, &hits)
	defer idp.Close()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer at-xyz" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		io.WriteString(w, "ok")
	}))
	defer api.Close()

	c := New("svc", "secret", idp.URL, nil, nil, oauth2.AuthStyleInHeader)
	resp, err := c.HTTPClient(context.Background()).Get(api.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resource status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Errorf("body = %q, want ok", body)
	}
}

func ExampleAudienceParams() {
	fmt.Println(AudienceParams("https://api.example.com").Encode())
	// Output: audience=https%3A%2F%2Fapi.example.com
}
```

## Review

The client is correct when the token lifecycle is entirely the token source's job.
`TestReuseWhileValid` proves the token is fetched once and reused; if you replaced
the shared source with a fresh `Token()` call per request, the hit count would
climb and the test would fail. `TestRefetchAfterExpiry` proves the buffer forces a
refetch when the token is (near) expired — the exact behavior a hand-rolled expiry
check tends to get wrong. `TestHTTPClientAttachesBearer` proves `Config.Client`
attaches `Authorization: Bearer` with no caller involvement.

The mistakes to avoid: do not check `token.Expiry` and refresh by hand — build one
`TokenSource` (or `HTTPClient`) and share it, so refresh happens exactly once and
concurrency-safely. Match `AuthStyle` to the IdP: the two style tests show the wire
difference, and leaving it as the auto-detect zero value costs a failed round trip
on first use. Remember the client-credentials grant has no user and no ID token —
it is service identity only; do not look for a `sub` claim or run the ID-token
checklist against it. Run `go test -race` to confirm the shared token source is
safe under concurrent callers.

## Resources

- [`golang.org/x/oauth2/clientcredentials`](https://pkg.go.dev/golang.org/x/oauth2/clientcredentials) — `Config`, `Token`, `TokenSource`, `Client`, `EndpointParams`, `AuthStyle`.
- [`golang.org/x/oauth2`](https://pkg.go.dev/golang.org/x/oauth2) — `TokenSource`, `ReuseTokenSource`, `Token.Valid`, `AuthStyleInHeader`/`AuthStyleInParams`.
- [RFC 6749 §4.4 — Client Credentials Grant](https://datatracker.ietf.org/doc/html/rfc6749#section-4.4) — the grant this exercise implements.

---

Back to [02-oidc-idtoken-verify.md](02-oidc-idtoken-verify.md) | Next: [../07-secrets-management-vault/00-concepts.md](../07-secrets-management-vault/00-concepts.md)
