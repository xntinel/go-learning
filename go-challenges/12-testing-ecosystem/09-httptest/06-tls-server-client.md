# Exercise 6: Test HTTPS clients with httptest.NewTLSServer

A client that talks to a downstream service over TLS has to trust that service's
certificate — and getting trust wrong is a real production incident, not a
theoretical one. `httptest.NewTLSServer` gives you an HTTPS server with a
self-signed cert so you can test the happy path, prove that an untrusting client
is correctly rejected, and build a cert-pinning client from the server's
certificate. This module does all three, and never reaches for
`InsecureSkipVerify`.

## What you'll build

```text
tlsclient/                      independent module: example.com/tls-server-client
  go.mod                        go 1.26
  tlsclient.go                  Fetch(client, url); PinnedClient(cert) building a RootCAs pool
  cmd/
    demo/
      main.go                   trusted vs default vs pinned client against a TLS server
  tlsclient_test.go             srv.Client() succeeds; DefaultClient rejected; pinned succeeds; HTTP/2
```

- Files: `tlsclient.go`, `cmd/demo/main.go`, `tlsclient_test.go`.
- Implement: `Fetch(ctx, *http.Client, url)` and `PinnedClient(cert *x509.Certificate)` which builds an `x509.CertPool` and sets it as `RootCAs` in a `tls.Config`.
- Test: `srv.Client().Get(srv.URL)` succeeds; `http.DefaultClient` fails with a certificate-verification error (assert it, do not skip verification); a pinned client built from `srv.Certificate()` succeeds; an `EnableHTTP2` server negotiates HTTP/2.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/tlsclient/cmd/demo
cd ~/go-exercises/tlsclient
go mod init example.com/tls-server-client
```

### Self-signed by design: trust is the thing under test

`httptest.NewTLSServer` starts an HTTPS server whose certificate is freshly
generated and self-signed — no public CA vouches for it. That is not a limitation;
it is the point. It forces you to be explicit about trust, exactly as production
does. Three clients behave three different ways against it:

- `srv.Client()` is preconfigured to trust *this server's* certificate, so its
  `Get(srv.URL)` succeeds. This is your happy path.
- `http.DefaultClient` trusts only the system root CAs, none of which signed the
  test cert, so its `Get(srv.URL)` fails TLS verification with an "unknown
  authority" error. The correct assertion is to *verify that error* — a client
  that rejects an untrusted cert is behaving correctly. The wrong move, and a
  frequent one, is to reach for `InsecureSkipVerify: true`, which trains you to
  disable the very check that protects production.
- A *pinned* client trusts exactly one certificate: you take `srv.Certificate()`
  (the server's `*x509.Certificate`), add it to an `x509.CertPool`, and set that
  pool as `RootCAs` in a `tls.Config` wired into an `http.Transport`. That client
  succeeds against this server and would reject any other — the real cert-pinning
  pattern.

For HTTP/2, `httptest`'s TLS server speaks HTTP/1.1 by default. To get HTTP/2 you
must set `srv.EnableHTTP2 = true` on an *unstarted* server (`NewUnstartedServer`)
*before* calling `StartTLS` — once started, the field is read and cannot change.
The test below verifies the negotiated protocol via `resp.ProtoMajor == 2`.

Create `tlsclient.go`:

```go
package tlsclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
)

// Fetch GETs url with the given client and returns the body as a string.
func Fetch(ctx context.Context, c *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("tlsclient: new request: %w", err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("tlsclient: do: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("tlsclient: read: %w", err)
	}
	return string(body), nil
}

// PinnedClient returns an http.Client that trusts exactly the given certificate
// and no other root — the cert-pinning pattern.
func PinnedClient(cert *x509.Certificate) *http.Client {
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    pool,
				MinVersion: tls.VersionTLS12,
			},
		},
	}
}
```

### The demo

The demo starts a TLS server and fetches with all three clients, showing the
trusted and pinned clients succeed while the default client is rejected.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"

	"example.com/tls-server-client"
)

func main() {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("secure ok"))
	}))
	defer srv.Close()

	ctx := context.Background()

	if body, err := tlsclient.Fetch(ctx, srv.Client(), srv.URL); err == nil {
		fmt.Printf("trusted client: %s\n", body)
	} else {
		log.Fatal(err)
	}

	if _, err := tlsclient.Fetch(ctx, http.DefaultClient, srv.URL); err != nil {
		fmt.Println("default client: rejected untrusted certificate")
	} else {
		log.Fatal("default client unexpectedly trusted the test cert")
	}

	pinned := tlsclient.PinnedClient(srv.Certificate())
	if body, err := tlsclient.Fetch(ctx, pinned, srv.URL); err == nil {
		fmt.Printf("pinned client: %s\n", body)
	} else {
		log.Fatal(err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
trusted client: secure ok
default client: rejected untrusted certificate
pinned client: secure ok
```

### Tests

The tests assert all three trust behaviors and HTTP/2 negotiation. The rejection
test verifies the error is a certificate-verification failure via `errors.As`
against `x509.UnknownAuthorityError` or `*tls.CertificateVerificationError` — a
real assertion, not a shrug.

Create `tlsclient_test.go`:

```go
package tlsclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func secureHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("secure ok"))
	})
}

func TestTrustedClientSucceeds(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(secureHandler())
	t.Cleanup(srv.Close)

	body, err := Fetch(t.Context(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("Fetch with srv.Client(): %v", err)
	}
	if body != "secure ok" {
		t.Fatalf("body = %q, want secure ok", body)
	}
}

func TestDefaultClientRejectsUntrustedCert(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(secureHandler())
	t.Cleanup(srv.Close)

	_, err := Fetch(t.Context(), http.DefaultClient, srv.URL)
	if err == nil {
		t.Fatal("DefaultClient unexpectedly trusted the self-signed test cert")
	}
	var uae x509.UnknownAuthorityError
	var cve *tls.CertificateVerificationError
	if !errors.As(err, &uae) && !errors.As(err, &cve) {
		t.Fatalf("err = %v, want a certificate-verification error", err)
	}
}

func TestPinnedClientSucceeds(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(secureHandler())
	t.Cleanup(srv.Close)

	c := PinnedClient(srv.Certificate())
	body, err := Fetch(t.Context(), c, srv.URL)
	if err != nil {
		t.Fatalf("Fetch with pinned client: %v", err)
	}
	if body != "secure ok" {
		t.Fatalf("body = %q, want secure ok", body)
	}
}

func TestEnableHTTP2(t *testing.T) {
	t.Parallel()

	srv := httptest.NewUnstartedServer(secureHandler())
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)

	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })

	if resp.ProtoMajor != 2 {
		t.Fatalf("ProtoMajor = %d, want 2 (HTTP/2)", resp.ProtoMajor)
	}
}
```

As a "your turn" addition, start a *second* TLS server and confirm a client pinned
to the first server's cert is rejected when it talks to the second — proving the
pin is exclusive.

## Review

The three-client contrast is the whole lesson: trust is explicit, and the test
proves each policy. The rejection test is the one people get wrong — the reflex on
an x509 error is `InsecureSkipVerify: true`, which is exactly backwards; the right
move is to assert the rejection happened (`errors.As`) and, when you legitimately
need to trust a private cert, build a `RootCAs` pool from it. `PinnedClient`
mirrors what a real service does when it pins a downstream's certificate. The
HTTP/2 test pins the ordering rule that trips people up: `EnableHTTP2` only takes
effect on an unstarted server set before `StartTLS`. Always close the response
body, even here.

## Resources

- [httptest `NewTLSServer`](https://pkg.go.dev/net/http/httptest#NewTLSServer) — the self-signed server and `Server.Certificate`/`Server.Client`/`EnableHTTP2`.
- [crypto/x509 `CertPool`](https://pkg.go.dev/crypto/x509#CertPool) — building a custom root pool for pinning.
- [crypto/tls `Config`](https://pkg.go.dev/crypto/tls#Config) — `RootCAs`, `MinVersion`, and why `InsecureSkipVerify` is the wrong fix.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-roundtripper-stub.md](05-roundtripper-stub.md) | Next: [07-middleware-chain.md](07-middleware-chain.md)
