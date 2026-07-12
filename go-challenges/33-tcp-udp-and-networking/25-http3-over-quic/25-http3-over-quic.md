# 25. HTTP/3 over QUIC

HTTP/3 replaces TCP with QUIC as the transport, eliminating head-of-line blocking at the transport layer, halving connection establishment to a single round trip, and enabling connection migration when the client's IP changes. The central challenge in a production deployment is that clients discover HTTP/3 only after talking to the server over HTTP/2 first: the server injects an `Alt-Svc` header into every HTTP/2 response advertising its QUIC endpoint, and capable clients upgrade automatically on the next request. This lesson builds a dual-stack server (HTTP/2 over TCP and HTTP/3 over QUIC on the same address), a matching HTTP/3 client, and wires `Alt-Svc` discovery via a middleware, using the `quic-go/http3` library.

```text
h3server/
  go.mod
  tls.go           self-signed TLS cert (crypto stdlib only)
  altsvc.go        Alt-Svc middleware (net/http stdlib only)
  server.go        DualStack server (requires github.com/quic-go/quic-go)
  client.go        HTTP/3 client    (requires github.com/quic-go/quic-go)
  server_test.go   hermetic tests for tls.go and altsvc.go
  cmd/demo/main.go runnable dual-stack demo
```

## Concepts

### Why QUIC eliminates transport-layer head-of-line blocking

HTTP/2 multiplexes many logical streams over a single TCP connection. TCP delivers bytes in order: a single lost packet stalls every stream until the retransmission arrives. This is head-of-line blocking at the transport layer, not the application layer—and HTTP/2 cannot fix it because it sits above TCP. QUIC multiplexes streams at the transport: a lost packet blocks only the stream that carries its data; other streams continue uninterrupted. For a page that loads 30 resources over one connection, a 2% packet loss that under HTTP/2 would stall all 30 resources will under HTTP/3 stall only the affected ones.

### Connection establishment and 0-RTT

TCP requires a SYN/ACK exchange before TLS begins, costing two round trips before any application data flows. QUIC fuses the transport and TLS 1.3 handshakes: a new connection takes one round trip. A client with a session ticket from a prior connection can send application data in the first flight (0-RTT), before receiving the server's handshake response. The tradeoff: 0-RTT data can be replayed by a network attacker, so only idempotent requests (GET, HEAD) should use 0-RTT mode.

### Connection migration

A TCP connection is bound to a four-tuple (client IP, client port, server IP, server port). When a mobile client switches from Wi-Fi to cellular, the IP changes and the TCP connection breaks. QUIC identifies connections by a connection ID, independent of the four-tuple. When the network path changes, the client presents its connection ID on the new path and the QUIC connection survives.

### Alt-Svc and protocol discovery

A server cannot know in advance whether a client supports QUIC. The standard discovery path is:

1. Client connects over HTTP/2 (TCP).
2. Server includes `Alt-Svc: h3=":443"; ma=86400` in the HTTP/2 response.
3. Client caches the advertisement for `ma` seconds (86400 = 24 hours).
4. On the next request to the same origin, the client opens a QUIC connection and speaks HTTP/3.

The value `h3` is the ALPN token for HTTP/3 (RFC 9114, §7.2). `ma` is max-age in seconds. The `quic-go/http3` package provides `Server.SetQUICHeaders(hdr http.Header) error` to write this value without constructing the string manually.

### ALPN negotiation

Both sides agree on the application protocol during the TLS handshake via the Application-Layer Protocol Negotiation extension (RFC 7301). For HTTP/3 the negotiated name is `"h3"` (the exported constant `http3.NextProtoH3`). The `*tls.Config` passed to the quic-go server must include `"h3"` in `NextProtos`; calling `http3.ConfigureTLSConfig` on an existing config sets this without overwriting other values.

### QPACK: header compression adapted for out-of-order delivery

HTTP/2 uses HPACK (RFC 7541), which encodes header changes against a shared dynamic table built from a strictly ordered stream of updates. HPACK depends on in-order delivery: a receiver cannot decode an entry until every earlier entry has been applied, creating head-of-line blocking inside the compressor. HTTP/3 replaces HPACK with QPACK (RFC 9204). Updates travel on dedicated unidirectional control streams separate from the header blocks; header blocks may reference entries without waiting for the control stream update. The receiver can decode header blocks out of order, preserving QUIC's independence-of-streams property.

### The quic-go/http3 API surface

`http3.Server` mirrors `net/http.Server`. The `Handler` field accepts any `http.Handler`—the same mux used for HTTP/2. `ListenAndServe` opens a UDP socket and speaks HTTP/3 over QUIC. `Shutdown(ctx)` drains in-flight QUIC streams before closing.

`http3.Transport` implements `http.RoundTripper`. Set it as the `Transport` field of an `http.Client` to send HTTP/3 requests. The transport maintains a pool of QUIC connections to the same origin and multiplexes concurrent requests as independent QUIC streams.

## Exercises

Set up the module:

```bash
go get github.com/quic-go/quic-go@latest
```

This lesson requires the external module `github.com/quic-go/quic-go`. The hermetic tests (Alt-Svc middleware and TLS helper) compile and run with no network access. The integration demo requires the module to be fetched once.

### Exercise 1: Self-signed TLS certificate helper

Create `tls.go`. This file uses only the standard library and is offline-compilable:

```go
package h3server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"time"
)

// SelfSignedTLS generates an ECDSA P-256 self-signed certificate valid for
// the given host names and returns a *tls.Config containing it. It is
// suitable for local testing and integration tests only.
func SelfSignedTLS(hosts ...string) (*tls.Config, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     hosts,
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, err
	}
	privDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return nil, err
	}
	cert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privDER}),
	)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	}, nil
}
```

`tls.VersionTLS13` is required because QUIC mandates TLS 1.3. ECDSA P-256 is preferred over RSA for QUIC because it produces shorter handshake messages, which matters when fitting into QUIC initial packets.

### Exercise 2: Alt-Svc middleware

Create `altsvc.go`. This file also uses only the standard library:

```go
package h3server

import (
	"fmt"
	"net/http"
)

// AltSvcMiddleware wraps next and adds an Alt-Svc response header advertising
// HTTP/3 on the given UDP port. The max-age is set to 86400 seconds (24 h),
// matching RFC 7838's recommended default.
//
// The resulting header value follows the format required by RFC 7838 §3:
//
//	Alt-Svc: h3=":<port>"; ma=86400
//
// Wire the middleware on the HTTP/2 listener so clients discover the QUIC
// endpoint and upgrade on subsequent requests.
func AltSvcMiddleware(port int, next http.Handler) http.Handler {
	val := fmt.Sprintf(`h3=":%d"; ma=86400`, port)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Alt-Svc", val)
		next.ServeHTTP(w, r)
	})
}
```

The header value is pre-computed once at construction time; the closure captures the string so no allocation occurs per request. Using `fmt.Sprintf` once at startup is preferable to string concatenation inside the handler.

### Exercise 3: Dual-stack server

Create `server.go`. This file imports `github.com/quic-go/quic-go/http3`:

```go
package h3server

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strconv"

	"github.com/quic-go/quic-go/http3"
)

// DualStack serves HTTP/2 over TCP and HTTP/3 over QUIC on the same address.
// Every HTTP/2 response carries an Alt-Svc header so QUIC-capable clients
// discover the QUIC endpoint and upgrade on the next request.
type DualStack struct {
	addr string
	h2   *http.Server
	h3   *http3.Server
}

// New creates a DualStack server. addr is the listen address (e.g. ":8443").
// tlsCfg must supply at least one certificate; use SelfSignedTLS for testing.
// The HTTP/3 server uses tlsCfg directly; the HTTP/2 server uses a clone so
// the two listeners do not share mutable state.
func New(addr string, handler http.Handler, tlsCfg *tls.Config) (*DualStack, error) {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("h3server: %w", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("h3server: non-numeric port in %q: %w", addr, err)
	}
	return &DualStack{
		addr: addr,
		h3: &http3.Server{
			Addr:      addr,
			TLSConfig: tlsCfg,
			Handler:   handler,
		},
		// The HTTP/2 listener wraps every response with AltSvcMiddleware so
		// clients learn about the QUIC endpoint on their first HTTP/2 request.
		h2: &http.Server{
			Addr:      addr,
			Handler:   AltSvcMiddleware(port, handler),
			TLSConfig: tlsCfg.Clone(),
		},
	}, nil
}

// Addr returns the configured listen address.
func (s *DualStack) Addr() string { return s.addr }

// ListenAndServe starts both the HTTP/2 (TCP) and HTTP/3 (QUIC) listeners.
// It blocks until the first listener exits and returns its error.
// Call Shutdown to stop both listeners gracefully before this returns.
//
// Note: ListenAndServeTLS("", "") works here because h2.TLSConfig already
// contains the certificate; the stdlib skips file loading when the config
// has certificates and both file paths are empty.
func (s *DualStack) ListenAndServe() error {
	errCh := make(chan error, 2)
	go func() { errCh <- s.h3.ListenAndServe() }()
	go func() { errCh <- s.h2.ListenAndServeTLS("", "") }()
	return <-errCh
}

// Shutdown gracefully drains both listeners. HTTP/3 shutdown drains in-flight
// QUIC streams; HTTP/2 shutdown drains in-flight HTTP requests. Both errors
// are inspected; the first non-nil error is returned.
func (s *DualStack) Shutdown(ctx context.Context) error {
	h3Err := s.h3.Shutdown(ctx)
	h2Err := s.h2.Shutdown(ctx)
	if h3Err != nil {
		return h3Err
	}
	return h2Err
}
```

### Exercise 4: HTTP/3 client

Create `client.go`. `http3.Transport` implements `http.RoundTripper`, so it plugs directly into `http.Client`:

```go
package h3server

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"

	"github.com/quic-go/quic-go/http3"
)

// H3Client sends HTTP requests over QUIC using HTTP/3. It wraps an
// http.Client whose transport is http3.Transport.
type H3Client struct {
	hc *http.Client
	tr *http3.Transport
}

// NewH3Client creates an HTTP/3 client. tlsCfg configures the TLS layer
// for the QUIC handshake. Set tlsCfg.InsecureSkipVerify = true when
// connecting to a server with a self-signed certificate.
func NewH3Client(tlsCfg *tls.Config) *H3Client {
	tr := &http3.Transport{TLSClientConfig: tlsCfg}
	return &H3Client{
		hc: &http.Client{Transport: tr},
		tr: tr,
	}
}

// Get performs an HTTP GET over HTTP/3 and returns the response.
// The caller is responsible for closing resp.Body.
func (c *H3Client) Get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("h3client: %w", err)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("h3client: %w", err)
	}
	return resp, nil
}

// Close releases all QUIC connections held by the transport.
func (c *H3Client) Close() error { return c.tr.Close() }
```

`http3.Transport` maintains a pool of QUIC connections per origin and multiplexes concurrent `Get` calls as independent QUIC streams over the same connection.

### Exercise 5: Hermetic tests

Create `server_test.go`. These tests cover only the stdlib-only functions (`AltSvcMiddleware` and `SelfSignedTLS`) and run offline with no quic-go dependency at test time:

```go
package h3server

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAltSvcMiddlewareAddsHeader(t *testing.T) {
	t.Parallel()

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	tests := []struct {
		name string
		port int
		want string
	}{
		{"https-port", 443, `h3=":443"; ma=86400`},
		{"dev-port", 8443, `h3=":8443"; ma=86400`},
		{"zero-port", 0, `h3=":0"; ma=86400`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := AltSvcMiddleware(tc.port, inner)
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/health", nil)
			h.ServeHTTP(w, r)
			if got := w.Header().Get("Alt-Svc"); got != tc.want {
				t.Errorf("Alt-Svc = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAltSvcMiddlewareCallsInnerHandler(t *testing.T) {
	t.Parallel()

	var called bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusTeapot)
	})

	h := AltSvcMiddleware(443, inner)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(w, r)

	if !called {
		t.Error("inner handler was not called")
	}
	if w.Code != http.StatusTeapot {
		t.Errorf("status = %d, want %d", w.Code, http.StatusTeapot)
	}
}

func TestSelfSignedTLSProducesCert(t *testing.T) {
	t.Parallel()

	cfg, err := SelfSignedTLS("localhost", "127.0.0.1")
	if err != nil {
		t.Fatalf("SelfSignedTLS: %v", err)
	}
	if len(cfg.Certificates) == 0 {
		t.Fatal("TLS config has no certificates")
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion = %d, want TLS 1.3 (%d)", cfg.MinVersion, tls.VersionTLS13)
	}
}

func TestSelfSignedTLSDifferentCallsProduceDifferentCerts(t *testing.T) {
	t.Parallel()

	cfg1, err := SelfSignedTLS("localhost")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	cfg2, err := SelfSignedTLS("localhost")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	// Each call generates a fresh key pair; the raw certificates must differ.
	raw1 := cfg1.Certificates[0].Certificate[0]
	raw2 := cfg2.Certificates[0].Certificate[0]
	if string(raw1) == string(raw2) {
		t.Error("two SelfSignedTLS calls produced identical certificates")
	}
}

func ExampleAltSvcMiddleware() {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := AltSvcMiddleware(8443, inner)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(w, r)
	fmt.Println(w.Header().Get("Alt-Svc"))
	// Output: h3=":8443"; ma=86400
}
```

Your turn: add `TestAltSvcMiddlewarePrecomputesValue` — call `AltSvcMiddleware(9000, inner)` twice with different inner handlers and assert both produce `h3=":9000"; ma=86400`. This pins the contract that the port is captured at construction, not per-call.

### Exercise 6: Runnable demo

Create `cmd/demo/main.go`. It exercises only exported API:

```go
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"example.com/h3server"
)

func main() {
	addr := flag.String("addr", ":8443", "listen address (TCP + UDP)")
	flag.Parse()

	tlsCfg, err := h3server.SelfSignedTLS("localhost")
	if err != nil {
		log.Fatalf("TLS: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "proto=%s host=%s path=%s\n", r.Proto, r.Host, r.URL.Path)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "ok\n")
	})

	srv, err := h3server.New(*addr, mux, tlsCfg)
	if err != nil {
		log.Fatalf("New: %v", err)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Printf("listening on %s  (HTTP/2 TCP + HTTP/3 QUIC)", srv.Addr())
		if err := srv.ListenAndServe(); err != nil {
			log.Printf("ListenAndServe: %v", err)
		}
	}()

	<-stop
	log.Println("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("Shutdown: %v", err)
	}
}
```

Run with:

```bash
go run ./cmd/demo -addr :8443
# in another terminal:
curl -k --http2 https://localhost:8443/health        # HTTP/2; check Alt-Svc in response headers
curl -k --http3 https://localhost:8443/health        # HTTP/3 (curl must be built with QUIC support)
```

## Common Mistakes

### Sharing a `*tls.Config` between the HTTP/2 and HTTP/3 servers

Wrong: both `h2.TLSConfig` and `h3.TLSConfig` point to the same `*tls.Config`. The HTTP/2 server's `ListenAndServeTLS` mutates the config when it sets up the ALPN negotiation for `"h2"`. The HTTP/3 server also mutates it for `"h3"`. The two servers race on the same pointer and can corrupt each other's ALPN list.

Fix: call `tlsCfg.Clone()` for one of the two servers so each holds an independent copy. `DualStack.New` does this: `h3` receives the original, `h2` receives `tlsCfg.Clone()`.

### Forgetting that QUIC uses UDP, not TCP

Wrong: a firewall rule or cloud security group that opens port 443 TCP but not 443 UDP. HTTP/3 traffic is silently dropped and clients never upgrade, even though `Alt-Svc` headers are present.

Fix: open both TCP 443 and UDP 443. In cloud environments (AWS security groups, GCP firewall rules), UDP rules are separate from TCP rules and must be added explicitly.

### Injecting `Alt-Svc` on the HTTP/3 listener instead of the HTTP/2 listener

Wrong: setting `Alt-Svc` on the HTTP/3 handler. Clients that have already upgraded to HTTP/3 do not need the discovery header; it is only useful to the HTTP/2 path.

Fix: apply `AltSvcMiddleware` only on the HTTP/2 server's handler. The `DualStack` struct wires this correctly: `h2.Handler = AltSvcMiddleware(port, handler)` while `h3.Handler = handler`.

### Using TLS 1.2 with QUIC

Wrong: setting `MinVersion: tls.VersionTLS12` in the TLS config. QUIC mandates TLS 1.3 (RFC 9001). quic-go will reject the config and fail to start.

Fix: set `MinVersion: tls.VersionTLS13`. `SelfSignedTLS` does this by default.

### Calling `Close` instead of `Shutdown` on the HTTP/3 server

Wrong: `h3.Close()` immediately terminates all QUIC connections, including those with in-flight streams. Clients experience abrupt stream resets.

Fix: call `h3.Shutdown(ctx)` with a deadline context. The server sends QUIC GOAWAY frames, stops accepting new connections, and waits for existing streams to finish or the context to expire.

## Verification

From `~/go-exercises/h3server`, run the hermetic tests (these pass offline, before `go get`):

```bash
# Extract tls.go + altsvc.go only for offline verification:
test -z "$(gofmt -l tls.go altsvc.go server_test.go)"
go vet ./...
go test -count=1 -race -run 'TestAltSvc|TestSelfSigned|Example' ./...
```

After fetching the module (`go get github.com/quic-go/quic-go@latest`):

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Confirm in the demo:
- HTTP/2 responses include `Alt-Svc: h3=":8443"; ma=86400`.
- `curl -k --http3 https://localhost:8443/` prints `proto=HTTP/3.0 ...`.
- Shutdown completes within 5 s with no abrupt stream resets.

## Summary

- QUIC eliminates transport-layer head-of-line blocking: a lost packet stalls only its own stream, not every concurrent stream.
- QUIC fuses the transport and TLS 1.3 handshakes into one round trip; repeat clients can use 0-RTT for idempotent requests.
- `Alt-Svc: h3=":<port>"; ma=<seconds>` is the discovery mechanism: a client sees it in an HTTP/2 response and upgrades to HTTP/3 on the next request.
- `http3.Server` accepts any `http.Handler`; it shares the same mux as the HTTP/2 server.
- `http3.Transport` implements `http.RoundTripper` and multiplexes requests as QUIC streams over a shared connection.
- QUIC mandates TLS 1.3; sharing a `*tls.Config` between the HTTP/2 and HTTP/3 servers causes a race on ALPN mutation — always clone it.

## What's Next

Next: [VPN Tunnel Implementation](../26-vpn-tunnel-implementation/26-vpn-tunnel-implementation.md).

## Resources

- [RFC 9114 — HTTP/3](https://datatracker.ietf.org/doc/html/rfc9114): the protocol specification, including ALPN token `h3` (§7.2) and Alt-Svc usage.
- [RFC 9001 — Using TLS to Secure QUIC](https://datatracker.ietf.org/doc/html/rfc9001): TLS 1.3 is mandatory for QUIC; explains why TLS 1.2 is not permitted.
- [RFC 7838 — HTTP Alternative Services](https://datatracker.ietf.org/doc/html/rfc7838): `Alt-Svc` header format, max-age semantics, and the upgrade flow.
- [pkg.go.dev/github.com/quic-go/quic-go/http3](https://pkg.go.dev/github.com/quic-go/quic-go/http3): full API reference for `http3.Server`, `http3.Transport`, and helper functions.
- [RFC 9204 — QPACK: Field Compression for HTTP/3](https://datatracker.ietf.org/doc/html/rfc9204): how QPACK separates dynamic table updates from header blocks to avoid HPACK's HOL blocking.
