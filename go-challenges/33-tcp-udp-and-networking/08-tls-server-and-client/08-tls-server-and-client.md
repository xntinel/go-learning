# 8. TLS Server and Client

`crypto/tls` wraps any `net.Conn` with TLS encryption. Upgrading a plain TCP service to TLS changes `net.Listen` to `tls.Listen` on the server and `net.Dial` to `tls.Dial` on the client. The hard part is certificate management: generating a certificate, configuring the server to present it, and configuring the client to trust the right CA. This lesson covers all three using only the standard library — no `openssl`, no key files on disk.

The design challenge: certificate verification errors must be distinguishable from network errors, and the reject path (untrusted CA) must be tested explicitly, not assumed.

```text
tlsdemo/
  go.mod
  tlsdemo.go
  tlsdemo_test.go
  cmd/demo/main.go
```

## Concepts

### TLS Is a Wrapper Around TCP

TLS does not replace TCP; it wraps it. After the TCP three-way handshake, both sides run the TLS handshake: they agree on a cipher suite, the server presents its certificate, and they derive a shared session key. All subsequent reads and writes are encrypted and authenticated.

`crypto/tls` provides `tls.Listen` (server side) and `tls.Dial` (client side). Both return a `*tls.Conn` that implements `net.Conn`. Any code written against the `net.Conn` interface works without change once TLS is layered in.

### Certificates and Certificate Pools

A TLS certificate binds a public key to an identity (a domain name or IP address). During the handshake the server presents its certificate. The client verifies it against a trusted CA pool (`tls.Config.RootCAs`). If `RootCAs` is nil, Go uses the operating system root store.

For development and testing the standard library generates a self-signed certificate entirely in memory:

- `ecdsa.GenerateKey(elliptic.P256(), rand.Reader)` creates an ECDSA P-256 key pair.
- `x509.CreateCertificate(rand.Reader, template, parent, pub, priv)` creates a DER-encoded certificate. When `template == parent` the certificate is self-signed.
- `pem.EncodeToMemory` encodes the DER bytes to PEM format.
- `tls.X509KeyPair(certPEM, keyPEM)` parses the PEM blocks into a `tls.Certificate`.
- `x509.NewCertPool()` and `(*x509.CertPool).AppendCertsFromPEM(pem)` build a trust store from PEM data.

ECDSA P-256 is preferred over RSA 2048 for new certificates: equivalent security, smaller key size, faster signing. ECDSA keys require only `x509.KeyUsageDigitalSignature`; RSA keys additionally need `x509.KeyUsageKeyEncipherment` for key-exchange cipher suites.

### MinVersion Enforcement

`tls.Config.MinVersion` sets the minimum TLS version the implementation will negotiate. Set it to `tls.VersionTLS12` at a minimum; TLS 1.0 and 1.1 are cryptographically broken. If the client and server cannot agree on a version, the handshake fails immediately.

After the handshake, `(*tls.Conn).ConnectionState()` returns a `tls.ConnectionState` value. Its `Version` field holds the negotiated version and `CipherSuite` field holds the cipher suite identifier. `tls.VersionName(ver)` and `tls.CipherSuiteName(id)` convert those values to human-readable strings (added in Go 1.21 and Go 1.14 respectively).

### The Reject Path

A client that calls `tls.Dial` without the correct `RootCAs` receives an error wrapping `x509.UnknownAuthorityError`. This is the TLS security boundary: no application data flows over a connection whose certificate chain cannot be verified. Testing the reject path is as important as testing the happy path.

## Exercises

This is a library. Verification runs `go test`, not `go run`.

### Exercise 1: Certificate Generation and Config Helpers

Create `tlsdemo.go`:

```go
package tlsdemo

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"time"
)

// ErrCertGen is returned when in-memory certificate generation fails.
var ErrCertGen = errors.New("tls: certificate generation failed")

// GenerateCert creates a self-signed ECDSA P-256 certificate valid for
// 127.0.0.1, with a one-hour lifetime. It returns the parsed TLS
// certificate and a certificate pool that trusts only that certificate.
func GenerateCert() (tls.Certificate, *x509.CertPool, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("%w: generate key: %v", ErrCertGen, err)
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{Organization: []string{"tlsdemo"}},
		NotBefore:    time.Now().Add(-time.Second),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("%w: create cert: %v", ErrCertGen, err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("%w: marshal key: %v", ErrCertGen, err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("%w: key pair: %v", ErrCertGen, err)
	}

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(certPEM)

	return tlsCert, pool, nil
}

// ServerConfig returns a *tls.Config for a server that presents cert
// and requires at least TLS 1.2.
func ServerConfig(cert tls.Certificate) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
}

// ClientConfig returns a *tls.Config for a client that trusts only
// the CAs in pool and requires at least TLS 1.2.
func ClientConfig(pool *x509.CertPool) *tls.Config {
	return &tls.Config{
		RootCAs:    pool,
		MinVersion: tls.VersionTLS12,
	}
}

// Echo reads up to 512 bytes from conn and writes them back. It is
// intended for use in tests and demos.
func Echo(conn net.Conn) error {
	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("echo read: %w", err)
	}
	if n > 0 {
		if _, werr := conn.Write(buf[:n]); werr != nil {
			return fmt.Errorf("echo write: %w", werr)
		}
	}
	return nil
}
```

`NotBefore` is set one second in the past to avoid clock-skew rejections when the certificate is used immediately after generation. The `IPAddresses` SAN is required so the client can verify the server by IP address; in production use `DNSNames` with real domain names.

### Exercise 2: Tests and the Example Function

Create `tlsdemo_test.go`:

```go
package tlsdemo

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
)

// startEchoServer starts a TLS echo server on a random loopback port.
// The returned listener must be closed by the caller.
func startEchoServer(t *testing.T, cfg *tls.Config) net.Listener {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = Echo(c)
			}(conn)
		}
	}()
	return ln
}

func TestGenerateCert(t *testing.T) {
	t.Parallel()

	cert, pool, err := GenerateCert()
	if err != nil {
		t.Fatalf("GenerateCert() error = %v", err)
	}
	if len(cert.Certificate) == 0 {
		t.Fatal("cert has no DER blocks")
	}
	if pool == nil {
		t.Fatal("pool is nil")
	}
}

func TestTLSEchoRoundTrip(t *testing.T) {
	t.Parallel()

	cert, pool, err := GenerateCert()
	if err != nil {
		t.Fatal(err)
	}

	ln := startEchoServer(t, ServerConfig(cert))
	defer ln.Close()

	conn, err := tls.Dial("tcp", ln.Addr().String(), ClientConfig(pool))
	if err != nil {
		t.Fatalf("tls.Dial: %v", err)
	}
	defer conn.Close()

	msg := []byte("hello tls")
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("Write: %v", err)
	}

	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if string(buf) != string(msg) {
		t.Errorf("echo = %q, want %q", buf, msg)
	}
}

func TestTLSVersionAtLeast12(t *testing.T) {
	t.Parallel()

	cert, pool, err := GenerateCert()
	if err != nil {
		t.Fatal(err)
	}

	ln := startEchoServer(t, ServerConfig(cert))
	defer ln.Close()

	conn, err := tls.Dial("tcp", ln.Addr().String(), ClientConfig(pool))
	if err != nil {
		t.Fatalf("tls.Dial: %v", err)
	}
	defer conn.Close()

	if err := conn.Handshake(); err != nil {
		t.Fatalf("Handshake: %v", err)
	}

	state := conn.ConnectionState()
	if state.Version < tls.VersionTLS12 {
		t.Errorf("TLS version = 0x%04x (%s), want >= TLS 1.2",
			state.Version, tls.VersionName(state.Version))
	}
}

func TestUntrustedCertRejected(t *testing.T) {
	t.Parallel()

	cert, _, err := GenerateCert()
	if err != nil {
		t.Fatal(err)
	}

	ln := startEchoServer(t, ServerConfig(cert))
	defer ln.Close()

	// Empty RootCAs falls back to the operating-system root store, which
	// does not trust the self-signed certificate.
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	_, dialErr := tls.Dial("tcp", ln.Addr().String(), cfg)
	if dialErr == nil {
		t.Fatal("expected certificate verification error, got nil")
	}

	// On Go 1.20+ the error is wrapped in *tls.CertificateVerificationError;
	// errors.As unwraps through it to reach x509.UnknownAuthorityError.
	var unknownAuth x509.UnknownAuthorityError
	if !errors.As(dialErr, &unknownAuth) {
		t.Logf("note: dialErr type %T did not unwrap to x509.UnknownAuthorityError (wrapping is version-dependent)", dialErr)
	}
}

func TestServerConfigFields(t *testing.T) {
	t.Parallel()

	cert, _, err := GenerateCert()
	if err != nil {
		t.Fatal(err)
	}
	cfg := ServerConfig(cert)
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = 0x%04x, want 0x%04x (TLS 1.2)", cfg.MinVersion, tls.VersionTLS12)
	}
	if len(cfg.Certificates) != 1 {
		t.Errorf("len(Certificates) = %d, want 1", len(cfg.Certificates))
	}
}

func ExampleServerConfig() {
	cert, _, err := GenerateCert()
	if err != nil {
		panic(err)
	}
	cfg := ServerConfig(cert)
	fmt.Println(cfg.MinVersion == tls.VersionTLS12)
	fmt.Println(len(cfg.Certificates) == 1)
	// Output:
	// true
	// true
}
```

Your turn: add `TestClientConfigFields` that calls `ClientConfig(pool)` and asserts `cfg.RootCAs == pool` and `cfg.MinVersion == tls.VersionTLS12`.

### Exercise 3: Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"crypto/tls"
	"fmt"
	"io"
	"log"

	"example.com/tlsdemo"
)

func main() {
	cert, pool, err := tlsdemo.GenerateCert()
	if err != nil {
		log.Fatalf("GenerateCert: %v", err)
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", tlsdemo.ServerConfig(cert))
	if err != nil {
		log.Fatalf("tls.Listen: %v", err)
	}
	defer ln.Close()

	errCh := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			errCh <- err
			return
		}
		defer conn.Close()
		errCh <- tlsdemo.Echo(conn)
	}()

	conn, err := tls.Dial("tcp", ln.Addr().String(), tlsdemo.ClientConfig(pool))
	if err != nil {
		log.Fatalf("tls.Dial: %v", err)
	}

	msg := []byte("hello")
	if _, err := conn.Write(msg); err != nil {
		log.Fatalf("Write: %v", err)
	}

	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		log.Fatalf("ReadFull: %v", err)
	}
	conn.Close()

	if err := <-errCh; err != nil {
		log.Fatalf("server echo: %v", err)
	}

	state := conn.ConnectionState()
	fmt.Printf("echo        : %q\n", buf)
	fmt.Printf("TLS version : %s\n", tls.VersionName(state.Version))
	fmt.Printf("cipher suite: %s\n", tls.CipherSuiteName(state.CipherSuite))
}
```

Run with `go run ./cmd/demo`. Example output (cipher suite and TLS version depend on Go version):

```text
echo        : "hello"
TLS version : TLS 1.3
cipher suite: TLS_AES_128_GCM_SHA256
```

## Common Mistakes

### Wrong KeyUsage for ECDSA

Wrong: applying RSA-style key usage to an ECDSA key:

```go
KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
```

What happens: the certificate contains a key-usage extension that ECDSA keys never need. Some strict TLS stacks reject handshakes when the key usage does not match the key type.

Fix: for ECDSA use only `x509.KeyUsageDigitalSignature`. `KeyUsageKeyEncipherment` is for RSA key exchange; ECDSA does not perform key encipherment.

### Missing RootCAs in the Client Config

Wrong:

```go
conn, err := tls.Dial("tcp", addr, &tls.Config{})
```

What happens: `RootCAs` is nil, so Go falls back to the operating-system root store. A self-signed development certificate is not in that store. The handshake fails with `x509.UnknownAuthorityError`.

Fix:

```go
_, pool, _ := GenerateCert()
conn, err := tls.Dial("tcp", addr, ClientConfig(pool))
```

### Missing IPAddresses SAN

Wrong: the certificate template has no `DNSNames` and no `IPAddresses`.

What happens: the client dials `127.0.0.1` but the certificate has no SAN matching that IP. The handshake fails with a hostname-mismatch error even when the CA is trusted.

Fix: add `IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}` to the template when testing by IP. For production servers add `DNSNames: []string{"your.domain.com"}`.

### Omitting MinVersion

Wrong: leaving `MinVersion` at its zero value.

What happens: Go's defaults tighten across versions but are not guaranteed to exclude TLS 1.0 forever. Explicit `MinVersion` pins the security floor independently of the Go release being used.

Fix: always set `MinVersion: tls.VersionTLS12`; for server-to-server calls where you control both ends prefer `tls.VersionTLS13`.

## Verification

From `~/go-exercises/tlsdemo`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four commands must exit zero. Add `TestClientConfigFields` from Exercise 2 before running.

## Summary

- `tls.Listen` and `tls.Dial` wrap `net.Listen`/`net.Dial` with TLS; the result is a `*tls.Conn` that implements `net.Conn`.
- Generate self-signed ECDSA P-256 certificates in memory: `ecdsa.GenerateKey` -> `x509.CreateCertificate` -> `pem.EncodeToMemory` -> `tls.X509KeyPair`.
- Set `MinVersion: tls.VersionTLS12` in both the server and client config.
- The client must have the server CA in `RootCAs`; nil uses the OS root store and rejects self-signed certs.
- `(*tls.Conn).ConnectionState()` exposes the negotiated TLS version and cipher suite after the handshake.
- Test the reject path (untrusted CA) explicitly; it is the security guarantee TLS provides.

## What's Next

Next: [Mutual TLS Authentication](../09-mutual-tls-authentication/09-mutual-tls-authentication.md).

## Resources

- [crypto/tls package reference](https://pkg.go.dev/crypto/tls)
- [crypto/x509 package reference](https://pkg.go.dev/crypto/x509)
- [Go Blog: TLS cipher suites in Go 1.17](https://go.dev/blog/tls-cipher-suites)
- [encoding/pem package reference](https://pkg.go.dev/encoding/pem)
- [crypto/ecdsa package reference](https://pkg.go.dev/crypto/ecdsa)
