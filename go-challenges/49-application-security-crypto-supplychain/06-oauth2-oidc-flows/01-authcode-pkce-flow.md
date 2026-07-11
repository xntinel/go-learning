# Exercise 1: Authorization Code Flow with PKCE and a Login-Session Ledger

This is the browser-facing half of an OIDC login: a `/login` handler that starts
the authorization-code-with-PKCE flow and a `/callback` handler that finishes it.
The security lives in a server-side session ledger that binds `state`, `nonce`,
and the PKCE verifier to one browser, single-use and TTL-bounded, so a stolen code
or a forged callback is useless.

This module is fully self-contained: its own `go mod init`, its own demo, and its
own tests that stand up a fake identity provider with `httptest` so nothing
touches a live IdP.

## What you'll build

```text
authpkce/                     independent module: example.com/authpkce
  go.mod                      go 1.26; requires golang.org/x/oauth2
  login.go                    Authenticator{Login, Callback}; ledger (single-use + TTL)
  cmd/
    demo/
      main.go                 prints the PKCE authorize URL the handler builds
  login_test.go               httptest fake IdP; PKCE end-to-end; state/single-use/expiry rejections
```

- Files: `login.go`, `cmd/demo/main.go`, `login_test.go`.
- Implement: `Authenticator` with `Login` (mints state/nonce/PKCE verifier, stores them keyed to a cookie session id, redirects to the IdP with the S256 challenge) and `Callback` (looks up the session, rejects state mismatch / reuse / expiry, exchanges the code with the verifier, extracts the raw `id_token`).
- Test: a fake IdP that asserts `code_challenge_method=S256` and that the posted `code_verifier` is the S256 pre-image of the challenge; plus rejection of tampered state, reused session, and expired session.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/authpkce/cmd/demo
cd ~/go-exercises/authpkce
go mod init example.com/authpkce
go get golang.org/x/oauth2@latest
```

### The ledger is the whole security story

`AuthCodeURL` and `Exchange` are the easy part — `golang.org/x/oauth2` builds the
URL and posts the token request for you. What the library cannot do for you is
bind the flow to *this* browser and make it single-use. That is the ledger.

At `/login` the handler generates three independent random values: a `state`
(CSRF token), an OIDC `nonce` (ID-token replay defense), and a PKCE
`code_verifier` (via `oauth2.GenerateVerifier`, which returns a 43-character
high-entropy string). It stores all three in the ledger under a fresh random
session id, and writes that id into an `HttpOnly` cookie. The browser now carries
an opaque handle; the secrets stay server-side. The redirect to the IdP includes
`oauth2.S256ChallengeOption(verifier)` — which sends `code_challenge` and
`code_challenge_method=S256`, never the raw verifier — plus the `nonce` as an
extra URL parameter via `oauth2.SetAuthURLParam`.

At `/callback` the handler reads the cookie, and calls `ledger.take` — which
**deletes** the entry as it reads it. That single line enforces single-use: a
replayed callback finds nothing and is rejected. `take` also enforces the TTL, so
a callback that arrives after the login window has closed is rejected too. Only
then does it compare `state` in constant time (`subtle.ConstantTimeCompare`), and
only then exchange the code with `oauth2.VerifierOption(verifier)` — the option
that carries the raw verifier so the IdP can prove the client that started the
flow is the one finishing it.

Note the two PKCE options are not interchangeable: `S256ChallengeOption` goes on
`AuthCodeURL` (send the hash), `VerifierOption` goes on `Exchange` (send the
pre-image). Swap them and PKCE silently does nothing.

Create `login.go`:

```go
package login

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

const sessionCookie = "oidc_session"

// sess holds the per-browser secrets for one in-flight login.
type sess struct {
	state    string
	nonce    string
	verifier string
	created  time.Time
}

// ledger stores login sessions single-use and TTL-bounded, keyed by a random id
// carried in a cookie. now is a clock seam so tests can force expiry.
type ledger struct {
	ttl time.Duration
	now func() time.Time

	mu sync.Mutex
	m  map[string]sess
}

func newLedger(ttl time.Duration) *ledger {
	return &ledger{ttl: ttl, now: time.Now, m: make(map[string]sess)}
}

func (l *ledger) put(id string, s sess) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.m[id] = s
}

// take returns the session and deletes it (single-use). It reports false when
// the id is unknown or the entry is older than the TTL.
func (l *ledger) take(id string) (sess, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.m[id]
	if !ok {
		return sess{}, false
	}
	delete(l.m, id)
	if l.now().Sub(s.created) > l.ttl {
		return sess{}, false
	}
	return s, true
}

// Authenticator wires the browser-facing side of authorization-code-with-PKCE.
// Secure controls the cookie Secure flag; keep it true in production (tests over
// plain HTTP set it false).
type Authenticator struct {
	cfg    *oauth2.Config
	ledger *ledger
	Secure bool
}

func New(cfg *oauth2.Config, ttl time.Duration) *Authenticator {
	return &Authenticator{cfg: cfg, ledger: newLedger(ttl), Secure: true}
}

// randString returns n bytes of crypto-random data, base64url encoded.
func randString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Login starts the flow: mint state/nonce/verifier, store them, and redirect to
// the IdP authorize endpoint with the S256 challenge.
func (a *Authenticator) Login(w http.ResponseWriter, r *http.Request) {
	state, err := randString(32)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	nonce, err := randString(32)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	sid, err := randString(32)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	verifier := oauth2.GenerateVerifier()

	a.ledger.put(sid, sess{state: state, nonce: nonce, verifier: verifier, created: a.ledger.now()})

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    sid,
		Path:     "/",
		HttpOnly: true,
		Secure:   a.Secure,
		SameSite: http.SameSiteLaxMode,
	})

	authURL := a.cfg.AuthCodeURL(state,
		oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("nonce", nonce),
		oauth2.AccessTypeOffline,
	)
	http.Redirect(w, r, authURL, http.StatusFound)
}

// Result is what a successful callback produces: the raw ID token to verify next
// (Exercise 2) and the nonce to compare it against.
type Result struct {
	RawIDToken  string
	AccessToken string
	Nonce       string
}

// Callback finishes the flow: consume the session (single-use, TTL-checked),
// verify state, exchange the code with the PKCE verifier, and pull out id_token.
func (a *Authenticator) Callback(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		http.Error(w, "missing session cookie", http.StatusBadRequest)
		return
	}
	s, ok := a.ledger.take(c.Value)
	if !ok {
		http.Error(w, "unknown or expired session", http.StatusForbidden)
		return
	}
	got := r.URL.Query().Get("state")
	if subtle.ConstantTimeCompare([]byte(got), []byte(s.state)) != 1 {
		http.Error(w, "state mismatch", http.StatusForbidden)
		return
	}
	code := r.URL.Query().Get("code")
	tok, err := a.cfg.Exchange(r.Context(), code, oauth2.VerifierOption(s.verifier))
	if err != nil {
		http.Error(w, "code exchange failed", http.StatusBadGateway)
		return
	}
	rawID, ok := tok.Extra("id_token").(string)
	if !ok || rawID == "" {
		http.Error(w, "no id_token in token response", http.StatusBadGateway)
		return
	}
	// A real handler verifies rawID and compares res.Nonce here, then sets its
	// own application session. We echo the values so the demo/test can see them.
	res := Result{RawIDToken: rawID, AccessToken: tok.AccessToken, Nonce: s.nonce}
	fmt.Fprintf(w, "id_token=%s\nnonce=%s\n", res.RawIDToken, res.Nonce)
}
```

### The runnable demo

The demo does not need a network round trip to show the important artifact: the
authorize URL the `/login` handler builds. It uses fixed values (rather than the
random ones `Login` generates) so the output is deterministic, and prints the
PKCE challenge derived from the RFC 7636 example verifier.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/url"

	"golang.org/x/oauth2"
)

func main() {
	cfg := &oauth2.Config{
		ClientID: "web-app",
		Endpoint: oauth2.Endpoint{
			AuthURL:  "https://idp.example.com/authorize",
			TokenURL: "https://idp.example.com/token",
		},
		RedirectURL: "https://app.example.com/callback",
		Scopes:      []string{"openid", "email"},
	}

	// Fixed values so the URL is reproducible. The RFC 7636 example verifier
	// hashes to the challenge printed below.
	verifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	raw := cfg.AuthCodeURL("login-state-123",
		oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("nonce", "login-nonce-abc"),
		oauth2.AccessTypeOffline,
	)

	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	q := u.Query()
	fmt.Printf("authorize endpoint: %s://%s%s\n", u.Scheme, u.Host, u.Path)
	for _, k := range []string{
		"client_id", "response_type", "scope", "redirect_uri",
		"state", "nonce", "code_challenge", "code_challenge_method", "access_type",
	} {
		fmt.Printf("%s=%s\n", k, q.Get(k))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
authorize endpoint: https://idp.example.com/authorize
client_id=web-app
response_type=code
scope=openid email
redirect_uri=https://app.example.com/callback
state=login-state-123
nonce=login-nonce-abc
code_challenge=E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM
code_challenge_method=S256
access_type=offline
```

### Tests

The tests stand up a fake IdP with two handlers. The `authorize` handler asserts
`code_challenge_method=S256`, records the `code -> code_challenge` mapping (as a
real IdP would), and redirects back to the callback with a code and the echoed
state. The `token` handler looks up the stored challenge and verifies the posted
`code_verifier` is its S256 pre-image using `oauth2.S256ChallengeFromVerifier` —
proving PKCE end to end — then returns a JSON token with a canned `id_token`.
`TestFullFlow` drives `/login` through the IdP to `/callback` with a real cookie
jar. The handler-level tests seed the ledger directly (same-package access) to
prove state-mismatch, single-use, and expiry each reject.

Create `login_test.go`:

```go
package login

import (
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// fakeIDP impersonates an authorization server's authorize and token endpoints.
type fakeIDP struct {
	idToken string

	mu    sync.Mutex
	n     int
	codes map[string]string // code -> code_challenge
}

func (f *fakeIDP) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("code_challenge_method") != "S256" {
		http.Error(w, "expected S256", http.StatusBadRequest)
		return
	}
	chal := q.Get("code_challenge")
	if chal == "" {
		http.Error(w, "missing code_challenge", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.n++
	code := "auth-code-" + strconv.Itoa(f.n)
	f.codes[code] = chal
	f.mu.Unlock()

	u, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	rq := u.Query()
	rq.Set("code", code)
	rq.Set("state", q.Get("state"))
	u.RawQuery = rq.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (f *fakeIDP) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	code := r.PostFormValue("code")
	verifier := r.PostFormValue("code_verifier")

	f.mu.Lock()
	chal, ok := f.codes[code]
	delete(f.codes, code)
	f.mu.Unlock()
	if !ok {
		http.Error(w, "unknown or reused code", http.StatusBadRequest)
		return
	}
	if oauth2.S256ChallengeFromVerifier(verifier) != chal {
		http.Error(w, "PKCE verifier mismatch", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"access_token":"at-1","token_type":"Bearer","expires_in":3600,"id_token":%q}`, f.idToken)
}

func TestFullFlow(t *testing.T) {
	t.Parallel()
	idp := &fakeIDP{idToken: "eyJ.canned.jwt", codes: make(map[string]string)}
	idpMux := http.NewServeMux()
	idpMux.HandleFunc("/authorize", idp.authorize)
	idpMux.HandleFunc("/token", idp.token)
	idpSrv := httptest.NewServer(idpMux)
	defer idpSrv.Close()

	a := New(&oauth2.Config{
		ClientID:     "web-app",
		ClientSecret: "s3cret",
		Endpoint: oauth2.Endpoint{
			AuthURL:   idpSrv.URL + "/authorize",
			TokenURL:  idpSrv.URL + "/token",
			AuthStyle: oauth2.AuthStyleInParams,
		},
		Scopes: []string{"openid", "email"},
	}, time.Minute)
	a.Secure = false // httptest serves plain HTTP

	appMux := http.NewServeMux()
	appMux.HandleFunc("/login", a.Login)
	appMux.HandleFunc("/callback", a.Callback)
	appSrv := httptest.NewServer(appMux)
	defer appSrv.Close()
	a.cfg.RedirectURL = appSrv.URL + "/callback"

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}

	resp, err := client.Get(appSrv.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("final status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "id_token=eyJ.canned.jwt") {
		t.Errorf("callback body missing id_token: %s", body)
	}
	if !strings.Contains(string(body), "nonce=") {
		t.Errorf("callback body missing nonce: %s", body)
	}
}

func TestLoginBuildsPKCEChallenge(t *testing.T) {
	t.Parallel()
	a := New(&oauth2.Config{
		ClientID: "web-app",
		Endpoint: oauth2.Endpoint{
			AuthURL:  "https://idp.example.com/authorize",
			TokenURL: "https://idp.example.com/token",
		},
		RedirectURL: "https://app.example.com/callback",
		Scopes:      []string{"openid", "email"},
	}, time.Minute)
	a.Secure = false

	w := httptest.NewRecorder()
	a.Login(w, httptest.NewRequest(http.MethodGet, "/login", nil))
	if w.Code != http.StatusFound {
		t.Fatalf("Login status = %d, want 302", w.Code)
	}
	u, err := url.Parse(w.Result().Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" {
		t.Errorf("code_challenge_method = %q, want S256", q.Get("code_challenge_method"))
	}
	for _, k := range []string{"code_challenge", "state", "nonce"} {
		if q.Get(k) == "" {
			t.Errorf("authorize URL missing %s", k)
		}
	}
	if q.Get("response_type") != "code" {
		t.Errorf("response_type = %q, want code", q.Get("response_type"))
	}
	if len(w.Result().Cookies()) == 0 {
		t.Error("no session cookie set")
	}
}

// tokenServer is a minimal token endpoint for the handler-level tests: it only
// checks that a code_verifier was posted, then returns a canned token.
func tokenServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		if r.PostFormValue("code_verifier") == "" {
			t.Error("token request missing code_verifier")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"at-9","token_type":"Bearer","expires_in":3600,"id_token":"header.payload.sig"}`)
	}))
}

func newAuthenticator(t *testing.T, ttl time.Duration) *Authenticator {
	t.Helper()
	ts := tokenServer(t)
	t.Cleanup(ts.Close)
	a := New(&oauth2.Config{
		ClientID:    "web",
		Endpoint:    oauth2.Endpoint{TokenURL: ts.URL, AuthStyle: oauth2.AuthStyleInParams},
		RedirectURL: "http://app.test/callback",
	}, ttl)
	a.Secure = false
	return a
}

func callbackReq(sid, state string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/callback?code=abc&state="+url.QueryEscape(state), nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	return r
}

func TestCallbackRejections(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(a *Authenticator) *http.Request
		want  int
	}{
		{
			name: "happy path",
			setup: func(a *Authenticator) *http.Request {
				a.ledger.put("sid-ok", sess{state: "st", verifier: oauth2.GenerateVerifier(), nonce: "n", created: a.ledger.now()})
				return callbackReq("sid-ok", "st")
			},
			want: http.StatusOK,
		},
		{
			name: "state mismatch",
			setup: func(a *Authenticator) *http.Request {
				a.ledger.put("sid-bad", sess{state: "correct", verifier: oauth2.GenerateVerifier(), created: a.ledger.now()})
				return callbackReq("sid-bad", "WRONG")
			},
			want: http.StatusForbidden,
		},
		{
			name: "unknown session",
			setup: func(a *Authenticator) *http.Request {
				return callbackReq("does-not-exist", "st")
			},
			want: http.StatusForbidden,
		},
		{
			name: "expired session",
			setup: func(a *Authenticator) *http.Request {
				base := time.Now()
				a.ledger.now = func() time.Time { return base }
				a.ledger.put("sid-exp", sess{state: "st", verifier: oauth2.GenerateVerifier(), created: base})
				a.ledger.now = func() time.Time { return base.Add(2 * a.ledger.ttl) }
				return callbackReq("sid-exp", "st")
			},
			want: http.StatusForbidden,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := newAuthenticator(t, time.Minute)
			req := tc.setup(a)
			w := httptest.NewRecorder()
			a.Callback(w, req)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

func TestCallbackSingleUse(t *testing.T) {
	t.Parallel()
	a := newAuthenticator(t, time.Minute)
	a.ledger.put("sid-1", sess{state: "st", verifier: oauth2.GenerateVerifier(), nonce: "n", created: a.ledger.now()})

	w1 := httptest.NewRecorder()
	a.Callback(w1, callbackReq("sid-1", "st"))
	if w1.Code != http.StatusOK {
		t.Fatalf("first callback = %d, want 200; body=%s", w1.Code, w1.Body.String())
	}
	w2 := httptest.NewRecorder()
	a.Callback(w2, callbackReq("sid-1", "st"))
	if w2.Code != http.StatusForbidden {
		t.Fatalf("reused callback = %d, want 403", w2.Code)
	}
}

func Example_pkceChallenge() {
	// The RFC 7636 Appendix B example verifier and its S256 challenge.
	verifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	fmt.Println(oauth2.S256ChallengeFromVerifier(verifier))
	// Output: E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM
}
```

## Review

The flow is correct when three invariants hold. First, the browser only ever
carries the opaque session id and the authorization code — never the verifier,
tokens, or nonce; the verifier is proven at the token endpoint via
`VerifierOption`, and the fake IdP's `S256ChallengeFromVerifier` check is what
demonstrates that. Second, the callback is single-use: `ledger.take` deletes the
entry as it reads it, so `TestCallbackSingleUse` sees the second attempt rejected;
if you accidentally read without deleting, that test fails. Third, `state` is
compared in constant time and a mismatch is a hard 403, which is login-CSRF
defense.

The mistakes to avoid: do not store `state`/`nonce`/`verifier` in a global or an
unauthenticated cookie — the ledger keyed by a random cookie id is what binds them
to one browser. Do not skip the `state` check or use `==` on it. Do not swap the
PKCE options: `S256ChallengeOption` belongs on `AuthCodeURL`, `VerifierOption` on
`Exchange`; the fake token handler would reject a swapped verifier, but in
production the swap fails silently against a real IdP. Finally, keep the cookie
`Secure` in production — the test flips it off only because `httptest` serves
plain HTTP. Run `go test -race` to confirm the ledger's mutex holds under
concurrent logins.

## Resources

- [`golang.org/x/oauth2`](https://pkg.go.dev/golang.org/x/oauth2) — `Config.AuthCodeURL`, `Exchange`, `GenerateVerifier`, `S256ChallengeOption`, `VerifierOption`.
- [RFC 7636 — Proof Key for Code Exchange](https://datatracker.ietf.org/doc/html/rfc7636) — the S256 challenge/verifier construction and Appendix B test vector.
- [RFC 9700 — OAuth 2.0 Security Best Current Practice](https://datatracker.ietf.org/doc/html/rfc9700) — why PKCE and exact redirect-URI matching are required for all clients.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-oidc-idtoken-verify.md](02-oidc-idtoken-verify.md)
