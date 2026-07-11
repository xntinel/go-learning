# 9. Mutual TLS Authentication

Standard TLS is one-sided: the client verifies the server's certificate, but the server accepts any caller. Mutual TLS (mTLS) adds the reverse step: the server sends a `CertificateRequest`, the client must present a certificate signed by a CA the server trusts, and the server rejects anyone else. The hard parts are generating the PKI material entirely in memory (no `openssl`, no files on disk), wiring `tls.Config.ClientAuth` correctly on both ends, and testing the rejection scenarios — not just the happy path.

```text
mtls/
  go.mod
  mtls.go
  mtls_test.go
  cmd/demo/main.go
```

## Concepts

### Standard TLS vs. Mutual TLS

In a standard TLS handshake (RFC 8446 §4.4) the server presents a `Certificate` message; the client verifies it. The server never asks the client for a certificate. In mutual TLS the server also sends `CertificateRequest` (§4.3.2). The client must respond with a `Certificate` and a `CertificateVerify` message. If the client sends an empty certificate list, the server sends a `certificate_required` alert (TLS 1.3) and the handshake fails. If the client sends a certificate from a CA the server does not trust, the server sends `unknown_ca` and the handshake fails.

mTLS is the standard authentication mechanism for service-to-service communication in zero-trust architectures. Kubernetes authenticates kubelets to the API server this way. Service meshes (Istio, Linkerd) inject mTLS transparently between pods.

### The ClientAuth Policies

`tls.Config.ClientAuth` is a `tls.ClientAuthType` integer (iota 0-4):

| Value | Name | Behavior |
|---|---|---|
| 0 | `NoClientCert` | no request, no requirement (default) |
| 1 | `RequestClientCert` | server requests; client may decline |
| 2 | `RequireAnyClientCert` | client must send a cert; cert is not verified |
| 3 | `VerifyClientCertIfGiven` | verified only when provided |
| 4 | `RequireAndVerifyClientCert` | cert required and verified against `ClientCAs` |

mTLS means `RequireAndVerifyClientCert`. The server also needs `ClientCAs` — an `*x509.CertPool` containing the CA certificates it trusts for client verification.

### Building PKI Material in Memory

The stdlib packages `crypto/ecdsa`, `crypto/elliptic`, `crypto/rand`, `crypto/x509`, `crypto/x509/pkix`, `encoding/pem`, and `math/big` are sufficient to build a full CA-and-leaf PKI:

```go
// Create a self-signed CA.
key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
tmpl := &x509.Certificate{
	SerialNumber:          big.NewInt(1),
	Subject:               pkix.Name{CommonName: "Test CA"},
	IsCA:                  true,
	BasicConstraintsValid: true,
	KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
}
// template == parent -> self-signed
der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
```

A leaf certificate is signed by the CA: pass the CA template as parent and the CA key as signer:

```go
der, _ := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
```

P-256 ECDSA key generation is an order of magnitude faster than RSA 2048 and produces smaller certificates — important when tests generate fresh PKI material on every run.

### ClientCAs vs. RootCAs

| Field | Used by | Purpose |
|---|---|---|
| `tls.Config.ClientCAs` | server | CAs whose client certificates the server trusts |
| `tls.Config.RootCAs` | client | CAs whose server certificates the client trusts |

A common mistake is setting `RootCAs` on the server or `ClientCAs` on the client. Each field is only consulted by the side that owns it.

### Identity Extraction

After a successful handshake `(*tls.Conn).ConnectionState()` returns a `tls.ConnectionState`. Its `PeerCertificates` slice holds the verified chain presented by the peer. Index 0 is the leaf certificate. `Subject.CommonName` is the conventional field for service identity in mTLS deployments:

```go
state := conn.ConnectionState()
if len(state.PeerCertificates) > 0 {
	cn := state.PeerCertificates[0].Subject.CommonName
}
```

### Failure Modes

**Empty `ClientCAs` with `RequireAndVerifyClientCert`**: the server rejects every client certificate because the trust pool is empty.

**`ExtKeyUsage` mismatch**: a server certificate must carry `ExtKeyUsageServerAuth`; a client certificate must carry `ExtKeyUsageClientAuth`. Swapping them produces a handshake error: `tls: failed to verify certificate: x509: certificate specifies an incompatible key usage`.

**Clock skew**: certificates carry `NotBefore`/`NotAfter` bounds. Set `NotBefore` to `time.Now().Add(-time.Minute)` to absorb small clock differences between machines.

**Forgetting IP SANs for loopback tests**: when the client dials `127.0.0.1`, TLS verifies the server's IP SANs. Include `net.ParseIP("127.0.0.1")` in `IPAddresses` or the handshake fails with `x509: cannot validate certificate for 127.0.0.1 because it doesn't contain any IP SANs`.

**TLS 1.3 rejection is deferred**: in TLS 1.3 the client completes its side of the handshake (sends Certificate + Finished) before the server has finished verifying the client certificate. `tls.Dial` therefore returns `nil` even when the server will reject the client. The `certificate_required` or `unknown_ca` alert arrives as the server closes the connection. It only surfaces when the client attempts its first `Read`. Tests that assert `tls.Dial` returns an error will fail silently (the dial "succeeds") unless they also perform a read.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/mtls/cmd/demo
cd ~/go-exercises/mtls
go mod init example.com/mtls
```

This is a library verified by `go test`, not by running a program.

### Exercise 1: PKI Helpers and TLS Config Builders

Create `mtls.go`:

```go
package mtls

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"time"
)

// Sentinel errors for invalid arguments.
var (
	ErrEmptyCN = errors.New("mtls: common name must not be empty")
	ErrNilCA   = errors.New("mtls: CA certificate and key must not be nil")
)

// GenerateCA creates an in-memory self-signed CA certificate and private key.
// The returned *x509.Certificate and *ecdsa.PrivateKey are ready to sign leaf
// certificates via GenerateCert.
func GenerateCA(cn string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	if cn == "" {
		return nil, nil, ErrEmptyCN
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("mtls: generate CA key: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("mtls: create CA cert: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, fmt.Errorf("mtls: parse CA cert: %w", err)
	}
	return cert, key, nil
}

// GenerateCert signs a leaf certificate with the given CA.
// Set isServer to true for ExtKeyUsageServerAuth, false for ExtKeyUsageClientAuth.
// The certificate includes 127.0.0.1 and localhost SANs for loopback tests.
func GenerateCert(ca *x509.Certificate, caKey *ecdsa.PrivateKey, cn string, isServer bool) (tls.Certificate, error) {
	if ca == nil || caKey == nil {
		return tls.Certificate{}, ErrNilCA
	}
	if cn == "" {
		return tls.Certificate{}, ErrEmptyCN
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("mtls: generate leaf key: %w", err)
	}
	extKeyUsage := x509.ExtKeyUsageClientAuth
	if isServer {
		extKeyUsage = x509.ExtKeyUsageServerAuth
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{extKeyUsage},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("mtls: create leaf cert: %w", err)
	}
	certBuf := new(bytes.Buffer)
	if err := pem.Encode(certBuf, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		return tls.Certificate{}, fmt.Errorf("mtls: encode cert PEM: %w", err)
	}
	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("mtls: marshal EC key: %w", err)
	}
	keyBuf := new(bytes.Buffer)
	if err := pem.Encode(keyBuf, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}); err != nil {
		return tls.Certificate{}, fmt.Errorf("mtls: encode key PEM: %w", err)
	}
	return tls.X509KeyPair(certBuf.Bytes(), keyBuf.Bytes())
}

// CertPool builds an *x509.CertPool containing a single CA certificate.
func CertPool(ca *x509.Certificate) *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return pool
}

// ServerConfig returns a *tls.Config that requires and verifies a client certificate.
// clientCAs is the pool of CAs whose client certificates the server will trust.
func ServerConfig(serverCert tls.Certificate, clientCAs *x509.CertPool) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
		MinVersion:   tls.VersionTLS13,
	}
}

// ClientConfig returns a *tls.Config that presents a client certificate and
// trusts rootCAs for server certificate verification.
func ClientConfig(clientCert tls.Certificate, rootCAs *x509.CertPool) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{clientCert},
		RootCAs:      rootCAs,
		MinVersion:   tls.VersionTLS13,
	}
}

// PolicyName returns the human-readable label for a tls.ClientAuthType value.
func PolicyName(auth tls.ClientAuthType) string {
	switch auth {
	case tls.NoClientCert:
		return "NoClientCert"
	case tls.RequestClientCert:
		return "RequestClientCert"
	case tls.RequireAnyClientCert:
		return "RequireAnyClientCert"
	case tls.VerifyClientCertIfGiven:
		return "VerifyClientCertIfGiven"
	case tls.RequireAndVerifyClientCert:
		return "RequireAndVerifyClientCert"
	default:
		return fmt.Sprintf("ClientAuthType(%d)", int(auth))
	}
}
```

`GenerateCert` returns a ready-to-use `tls.Certificate`. The `ExtKeyUsage` field is what distinguishes a server certificate from a client certificate at the protocol level — swapping them causes a handshake failure even when the CA chain is valid.

### Exercise 2: Tests

Create `mtls_test.go`:

```go
package mtls

import (
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"testing"
	"time"
)

// mustPKI generates a CA certificate and key for use in tests.
func mustPKI(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	ca, caKey, err := GenerateCA("Test CA")
	if err != nil {
		t.Fatal(err)
	}
	return ca, caKey
}

// startMTLSServer starts a single-connection TLS listener and returns its
// address and a channel that receives the peer's Common Name after a successful
// handshake, or an empty string if the handshake fails.
func startMTLSServer(t *testing.T, cfg *tls.Config) (addr string, cnCh <-chan string) {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	ch := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			ch <- ""
			return
		}
		tlsConn := conn.(*tls.Conn)
		defer tlsConn.Close()
		if err := tlsConn.Handshake(); err != nil {
			ch <- ""
			return
		}
		state := tlsConn.ConnectionState()
		if len(state.PeerCertificates) > 0 {
			ch <- state.PeerCertificates[0].Subject.CommonName
		} else {
			ch <- ""
		}
	}()
	return ln.Addr().String(), ch
}

// dialAndRead dials addr with cfg, then reads one byte. It returns the
// first non-nil error from either step. In TLS 1.3, the server rejection
// alert arrives after the handshake completes on the client side, so the
// rejection is only observable on the first read, not on Dial.
func dialAndRead(addr string, cfg *tls.Config) error {
	conn, err := tls.Dial("tcp", addr, cfg)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, err = conn.Read(make([]byte, 1))
	return err
}

func TestGenerateCARejectsEmptyCN(t *testing.T) {
	t.Parallel()
	_, _, err := GenerateCA("")
	if !errors.Is(err, ErrEmptyCN) {
		t.Fatalf("err = %v, want ErrEmptyCN", err)
	}
}

func TestGenerateCertRejectsNilCA(t *testing.T) {
	t.Parallel()
	_, err := GenerateCert(nil, nil, "svc", false)
	if !errors.Is(err, ErrNilCA) {
		t.Fatalf("err = %v, want ErrNilCA", err)
	}
}

func TestGenerateCertRejectsEmptyCN(t *testing.T) {
	t.Parallel()
	ca, caKey := mustPKI(t)
	_, err := GenerateCert(ca, caKey, "", false)
	if !errors.Is(err, ErrEmptyCN) {
		t.Fatalf("err = %v, want ErrEmptyCN", err)
	}
}

func TestMTLSHandshake(t *testing.T) {
	t.Parallel()

	ca, caKey := mustPKI(t)
	caPool := CertPool(ca)

	serverCert, err := GenerateCert(ca, caKey, "server", true)
	if err != nil {
		t.Fatalf("GenerateCert(server): %v", err)
	}
	clientCert, err := GenerateCert(ca, caKey, "client-svc", false)
	if err != nil {
		t.Fatalf("GenerateCert(client): %v", err)
	}

	addr, cnCh := startMTLSServer(t, ServerConfig(serverCert, caPool))

	conn, err := tls.Dial("tcp", addr, ClientConfig(clientCert, caPool))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	conn.Close()

	// Block until the server goroutine reads the peer certificate.
	cn := <-cnCh
	if cn != "client-svc" {
		t.Fatalf("PeerCertificates[0].Subject.CommonName = %q, want client-svc", cn)
	}
}

func TestNoClientCertRejected(t *testing.T) {
	t.Parallel()

	ca, caKey := mustPKI(t)
	caPool := CertPool(ca)

	serverCert, err := GenerateCert(ca, caKey, "server", true)
	if err != nil {
		t.Fatalf("GenerateCert(server): %v", err)
	}

	addr, _ := startMTLSServer(t, ServerConfig(serverCert, caPool))

	// Client presents no certificate.
	// In TLS 1.3 the rejection alert arrives after the handshake completes
	// on the client side, so it surfaces on the first Read, not on Dial.
	cfg := &tls.Config{
		RootCAs:    caPool,
		MinVersion: tls.VersionTLS13,
	}
	if err := dialAndRead(addr, cfg); err == nil {
		t.Fatal("expected rejection when client presents no certificate")
	}
}

func TestWrongCARejected(t *testing.T) {
	t.Parallel()

	ca1, caKey1 := mustPKI(t)
	ca2, caKey2 := mustPKI(t)

	ca1Pool := CertPool(ca1)

	serverCert, err := GenerateCert(ca1, caKey1, "server", true)
	if err != nil {
		t.Fatalf("GenerateCert(server): %v", err)
	}
	// Client cert is signed by CA2; server only trusts CA1.
	wrongClientCert, err := GenerateCert(ca2, caKey2, "intruder", false)
	if err != nil {
		t.Fatalf("GenerateCert(client, CA2): %v", err)
	}

	addr, _ := startMTLSServer(t, ServerConfig(serverCert, ca1Pool))

	// Client trusts CA1 for the server cert but presents a cert from CA2.
	// The server rejection alert arrives after the client-side handshake
	// completes (TLS 1.3), so it is only visible on the first Read.
	cfg := &tls.Config{
		Certificates: []tls.Certificate{wrongClientCert},
		RootCAs:      ca1Pool,
		MinVersion:   tls.VersionTLS13,
	}
	if err := dialAndRead(addr, cfg); err == nil {
		t.Fatal("expected rejection when client cert is signed by an untrusted CA")
	}
}

func TestPolicyName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		auth tls.ClientAuthType
		want string
	}{
		{tls.NoClientCert, "NoClientCert"},
		{tls.RequestClientCert, "RequestClientCert"},
		{tls.RequireAnyClientCert, "RequireAnyClientCert"},
		{tls.VerifyClientCertIfGiven, "VerifyClientCertIfGiven"},
		{tls.RequireAndVerifyClientCert, "RequireAndVerifyClientCert"},
		{tls.ClientAuthType(99), "ClientAuthType(99)"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			if got := PolicyName(tc.auth); got != tc.want {
				t.Fatalf("PolicyName(%d) = %q, want %q", int(tc.auth), got, tc.want)
			}
		})
	}
}

func ExamplePolicyName() {
	fmt.Println(PolicyName(tls.RequireAndVerifyClientCert))
	// Output: RequireAndVerifyClientCert
}
```

Your turn: add `TestServerCertNotTrustedByClient`. Call `tls.Dial` with a `*tls.Config` whose `RootCAs` is a fresh pool that does NOT contain the server's CA. Assert that `err != nil` and that the error wraps an `x509.UnknownAuthorityError`.

### Exercise 3: Runnable Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"crypto/tls"
	"fmt"
	"log"

	"example.com/mtls"
)

func main() {
	ca, caKey, err := mtls.GenerateCA("Demo CA")
	if err != nil {
		log.Fatalf("GenerateCA: %v", err)
	}

	serverCert, err := mtls.GenerateCert(ca, caKey, "demo-server", true)
	if err != nil {
		log.Fatalf("GenerateCert(server): %v", err)
	}
	clientCert, err := mtls.GenerateCert(ca, caKey, "demo-client", false)
	if err != nil {
		log.Fatalf("GenerateCert(client): %v", err)
	}

	caPool := mtls.CertPool(ca)

	ln, err := tls.Listen("tcp", "127.0.0.1:0", mtls.ServerConfig(serverCert, caPool))
	if err != nil {
		log.Fatalf("Listen: %v", err)
	}

	doneCh := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			doneCh <- "accept error: " + err.Error()
			return
		}
		tlsConn := conn.(*tls.Conn)
		defer tlsConn.Close()
		if err := tlsConn.Handshake(); err != nil {
			doneCh <- "handshake error: " + err.Error()
			return
		}
		state := tlsConn.ConnectionState()
		if len(state.PeerCertificates) > 0 {
			doneCh <- state.PeerCertificates[0].Subject.CommonName
		} else {
			doneCh <- "(no peer cert)"
		}
	}()

	conn, err := tls.Dial("tcp", ln.Addr().String(), mtls.ClientConfig(clientCert, caPool))
	if err != nil {
		log.Fatalf("Dial: %v", err)
	}
	conn.Close()
	ln.Close()

	cn := <-doneCh
	fmt.Printf("auth policy  : %s\n", mtls.PolicyName(tls.RequireAndVerifyClientCert))
	fmt.Printf("client CN    : %s\n", cn)
}
```

Run it with:

```bash
go run ./cmd/demo
```

## Common Mistakes

### Setting `RootCAs` on the Server Instead of `ClientCAs`

Wrong: `serverCfg.RootCAs = caPool`. `RootCAs` is only consulted by the client when verifying the server's certificate. Setting it on the server has no effect on client authentication.

Fix: use `serverCfg.ClientCAs = caPool`. The server verifies client certs against `ClientCAs`.

### Missing `ExtKeyUsage` on Leaf Certificates

Wrong: a certificate created without `ExtKeyUsage` set. By default the field is empty, which means the certificate has no extended usage constraints. Some TLS implementations accept it; Go's does not for `RequireAndVerifyClientCert`.

Fix: always set `ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}` on client certificates and `ExtKeyUsageServerAuth` on server certificates. The handshake will fail with `incompatible key usage` if the wrong value is used.

### Testing Only the Happy Path

Wrong: a test that dials successfully, prints the CN, and calls it done. If `ClientAuth` is accidentally set to `NoClientCert`, the test still passes.

Fix: add explicit rejection tests. `TestNoClientCertRejected` asserts that a client without a certificate cannot connect. `TestWrongCARejected` asserts that a client with an untrusted CA cert cannot connect. If either test fails (dial unexpectedly succeeds), mTLS is broken.

### Checking Only `tls.Dial` for Rejection in TLS 1.3

Wrong: asserting `tls.Dial(…) != nil` as the rejection check. In TLS 1.3 the client completes its handshake and `tls.Dial` returns `nil` before the server has verified the client certificate. The rejection alert only arrives on the first `Read` after the server closes the connection.

Fix: after a successful dial, perform a `Read` and check that for the error:

```go
conn, err := tls.Dial("tcp", addr, cfg)
if err == nil {
    conn.SetDeadline(time.Now().Add(5 * time.Second))
    _, err = conn.Read(make([]byte, 1))
    conn.Close()
}
if err == nil {
    t.Fatal("expected rejection")
}
```

### Not Calling `Handshake()` Explicitly on the Server

Wrong: server goroutine reads from the connection without calling `Handshake()` first, then calls `ConnectionState()`. `PeerCertificates` is only populated after the handshake.

Fix: call `tlsConn.Handshake()` explicitly before reading `ConnectionState()`. `Handshake()` is idempotent if the handshake already completed; calling it explicitly makes the error explicit and `ConnectionState()` safe to use.

## Verification

From `~/go-exercises/mtls`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four commands must exit 0. `go test -race` is the verification — do not rely on `go run ./cmd/demo` as a substitute because a runnable demo does not fail on regression.

Add the "Your turn" test (`TestServerCertNotTrustedByClient`) and confirm it passes.

## Summary

- mTLS authenticates both sides: client verifies server, server verifies client.
- `tls.Config.ClientAuth = tls.RequireAndVerifyClientCert` enforces client certificate presentation and verification.
- Server uses `ClientCAs` (not `RootCAs`) to verify client certificates.
- Leaf certificates require matching `ExtKeyUsage`: `ServerAuth` for servers, `ClientAuth` for clients.
- Extract client identity from `ConnectionState().PeerCertificates[0].Subject.CommonName` after an explicit `Handshake()` call.
- Test both the happy path and at least two rejection scenarios (no cert, wrong CA).

## What's Next

Next: [DNS Resolver and Custom Dialer](../10-dns-resolver-and-custom-dialer/10-dns-resolver-and-custom-dialer.md).

## Resources

- [crypto/tls — pkg.go.dev](https://pkg.go.dev/crypto/tls)
- [crypto/x509 — pkg.go.dev](https://pkg.go.dev/crypto/x509)
- [RFC 8446 §4.3.2 Certificate Request](https://www.rfc-editor.org/rfc/rfc8446#section-4.3.2)
- [tls.Config.ClientAuth — pkg.go.dev](https://pkg.go.dev/crypto/tls#ClientAuthType)
- [x509.Certificate — pkg.go.dev](https://pkg.go.dev/crypto/x509#Certificate)
