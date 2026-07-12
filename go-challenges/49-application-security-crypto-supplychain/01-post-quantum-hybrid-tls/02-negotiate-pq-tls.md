# Exercise 2: Negotiating X25519MLKEM768 End to End

Now let `crypto/tls` do the hybrid handshake for you, and prove which group was
actually negotiated. This exercise stands up a real TLS 1.3 endpoint on a
loopback listener with a certificate generated at runtime, dials it, and reads
`ConnectionState().CurveID` — the only correct way to observe the negotiated
key-exchange group.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
pqtls/                     independent module: example.com/pqtls
  go.mod                   go 1.26 (ConnectionState.CurveID needs 1.25+)
  pqtls.go                 Server, Dial, NegotiatedGroup, GroupName; runtime cert
  cmd/
    demo/
      main.go              runnable demo: full handshake, prints negotiated group
  pqtls_test.go            loopback handshake asserts CurveID + TLS 1.3, fallback
```

- Files: `pqtls.go`, `cmd/demo/main.go`, `pqtls_test.go`.
- Implement: `NewServer` (runtime self-signed cert, TLS 1.3 loopback listener), `Dial`, `NegotiatedGroup`, and `GroupName`.
- Test: a hybrid client negotiates `X25519MLKEM768` on TLS 1.3; a classical-only client falls back to `X25519`, proving the check reads the real handshake.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/49-application-security-crypto-supplychain/01-post-quantum-hybrid-tls/02-negotiate-pq-tls/cmd/demo
cd go-solutions/49-application-security-crypto-supplychain/01-post-quantum-hybrid-tls/02-negotiate-pq-tls
go mod edit -go=1.26
```

### The posture is three fields, and one accessor

A post-quantum TLS server is an ordinary TLS server with a specific `tls.Config`:

- `MinVersion: tls.VersionTLS13`. Post-quantum key exchange exists only on TLS
  1.3. If the config allows 1.2, any connection that negotiates 1.2 gets zero PQ
  protection, silently. `MinVersion` is part of the posture, not a separate knob.
- `CurvePreferences` containing `tls.X25519MLKEM768`. Remember this is a *set*,
  not an ordered list — Go applies its own preference internally and ignores your
  ordering. You express which groups are acceptable by membership.
- `Certificates`. TLS still needs a server certificate; this exercise generates a
  self-signed ECDSA P-256 certificate at runtime so the module is self-contained.

The one correct way to learn what was negotiated is `ConnectionState().CurveID`,
added in Go 1.25. Do not infer the group from the cipher suite (that is the
symmetric AEAD, orthogonal to the key exchange) and do not assume it from your
own `CurvePreferences` (the peer may not support your group). `NegotiatedGroup`
is a thin accessor over `ConnectionState().CurveID`, and `GroupName` maps the
numeric `CurveID` to a human string for logs and metrics.

### Why the fallback case matters

If the check simply hard-coded "the group is X25519MLKEM768", the test would pass
without proving anything. So the server offers *both* the hybrid group and plain
`X25519`, and a second client offers only `X25519`. That client negotiates plain
`X25519`, and the assertion confirms it. This proves two things at once: the
observation is reading the real handshake outcome, and a legacy client that does
not speak the hybrid group still interoperates by falling back — the migration is
gradual, not a hard cutover.

Note the difference from a server that pins *only* the hybrid group: if such a
server met a client that offered only `X25519`, there would be no group in common
and the handshake would *fail* rather than fall back. Offering both groups is what
enables graceful coexistence during a rollout.

### The kill switch

`GODEBUG=tlsmlkem=0` disables the ML-KEM hybrid group process-wide without a code
change or redeploy. It is the documented rollback lever for the operational risks
of a larger ClientHello (broken middleboxes, MTU-sensitive paths). You do not set
it in code; you keep it in your back pocket for the incident where a specific
network path mishandles the fragmented ClientHello.

Create `pqtls.go`:

```go
package pqtls

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
	"math/big"
	"net"
	"time"
)

// ErrUntrustedCert reports a certificate PEM that could not be added to the
// client's trust pool.
var ErrUntrustedCert = errors.New("pqtls: certificate PEM not accepted into pool")

// Server is a TLS 1.3 loopback endpoint that completes the handshake on each
// connection and writes back the negotiated group name, one line per connection.
type Server struct {
	ln      net.Listener
	certPEM []byte
}

// NewServer generates a self-signed certificate, binds a TLS 1.3 listener on a
// loopback port, and serves in the background. The groups become the server's
// CurvePreferences set. Callers must Close the returned server.
func NewServer(groups ...tls.CurveID) (*Server, error) {
	cert, certPEM, err := generateCert()
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{
		Certificates:     []tls.Certificate{cert},
		MinVersion:       tls.VersionTLS13,
		CurvePreferences: groups,
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		return nil, fmt.Errorf("pqtls: listen: %w", err)
	}
	s := &Server{ln: ln, certPEM: certPEM}
	go s.serve()
	return s, nil
}

func (s *Server) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return // listener closed
		}
		go func(c net.Conn) {
			defer c.Close()
			tc := c.(*tls.Conn)
			if err := tc.Handshake(); err != nil {
				return
			}
			fmt.Fprintln(tc, GroupName(tc.ConnectionState().CurveID))
		}(conn)
	}
}

// Addr is the loopback address the server is listening on.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// CertPEM is the server's self-signed certificate, for the client's trust pool.
func (s *Server) CertPEM() []byte { return s.certPEM }

// Close stops the listener.
func (s *Server) Close() error { return s.ln.Close() }

// Dial connects to addr, trusting certPEM, and offering the given key-exchange
// groups. It pins TLS 1.3 and verifies the "localhost" name on the certificate.
func Dial(addr string, certPEM []byte, groups ...tls.CurveID) (*tls.Conn, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		return nil, ErrUntrustedCert
	}
	cfg := &tls.Config{
		RootCAs:          pool,
		ServerName:       "localhost",
		MinVersion:       tls.VersionTLS13,
		CurvePreferences: groups,
	}
	conn, err := tls.Dial("tcp", addr, cfg)
	if err != nil {
		return nil, fmt.Errorf("pqtls: dial: %w", err)
	}
	return conn, nil
}

// NegotiatedGroup reports the key-exchange group the handshake actually
// negotiated. This is the only correct source of that fact.
func NegotiatedGroup(conn *tls.Conn) tls.CurveID {
	return conn.ConnectionState().CurveID
}

// GroupName maps a CurveID to a human-readable name for logs and metrics.
func GroupName(id tls.CurveID) string {
	switch id {
	case tls.X25519MLKEM768:
		return "X25519MLKEM768"
	case tls.SecP256r1MLKEM768:
		return "SecP256r1MLKEM768"
	case tls.SecP384r1MLKEM1024:
		return "SecP384r1MLKEM1024"
	case tls.X25519:
		return "X25519"
	case tls.CurveP256:
		return "P-256"
	case tls.CurveP384:
		return "P-384"
	default:
		return fmt.Sprintf("CurveID(%d)", uint16(id))
	}
}

// generateCert builds a self-signed ECDSA P-256 certificate for localhost.
func generateCert() (tls.Certificate, []byte, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("pqtls: generate key: %w", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("pqtls: create certificate: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("pqtls: marshal key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("pqtls: build keypair: %w", err)
	}
	return cert, certPEM, nil
}
```

### The runnable demo

The demo starts a server that offers both the hybrid group and classical X25519,
dials it preferring the hybrid group, and prints what was negotiated — plus the
line the server echoes back, confirming both ends agree.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"log"
	"strings"

	"example.com/pqtls"
)

func main() {
	srv, err := pqtls.NewServer(tls.X25519MLKEM768, tls.X25519)
	if err != nil {
		log.Fatal(err)
	}
	defer srv.Close()

	conn, err := pqtls.Dial(srv.Addr(), srv.CertPEM(), tls.X25519MLKEM768)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	state := conn.ConnectionState()
	group := pqtls.NegotiatedGroup(conn)
	fmt.Printf("negotiated group: %s (CurveID %d)\n", pqtls.GroupName(group), uint16(group))
	fmt.Printf("TLS 1.3: %v\n", state.Version == tls.VersionTLS13)

	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("server observed: %s\n", strings.TrimSpace(line))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
negotiated group: X25519MLKEM768 (CurveID 4588)
TLS 1.3: true
server observed: X25519MLKEM768
```

### Tests

The test runs entirely in-process against a loopback listener, so it stays in the
default test path with no external network. `TestNegotiation` table-drives the
client's offered groups against a server that offers both, asserting the
negotiated `CurveID` and that the version is TLS 1.3. The hybrid case proves PQ is
negotiated; the classical-only case proves the check reads the real handshake and
that fallback works. `t.Cleanup` closes the server.

Create `pqtls_test.go`:

```go
package pqtls

import (
	"crypto/tls"
	"errors"
	"fmt"
	"testing"
)

func TestNegotiation(t *testing.T) {
	t.Parallel()
	srv, err := NewServer(tls.X25519MLKEM768, tls.X25519)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { srv.Close() })

	tests := []struct {
		name  string
		offer []tls.CurveID
		want  tls.CurveID
	}{
		{"hybrid client negotiates PQ", []tls.CurveID{tls.X25519MLKEM768}, tls.X25519MLKEM768},
		{"classical client falls back", []tls.CurveID{tls.X25519}, tls.X25519},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn, err := Dial(srv.Addr(), srv.CertPEM(), tt.offer...)
			if err != nil {
				t.Fatalf("Dial: %v", err)
			}
			defer conn.Close()

			if got := NegotiatedGroup(conn); got != tt.want {
				t.Errorf("negotiated group = %s (%d); want %s (%d)",
					GroupName(got), uint16(got), GroupName(tt.want), uint16(tt.want))
			}
			if v := conn.ConnectionState().Version; v != tls.VersionTLS13 {
				t.Errorf("version = %#x; want TLS 1.3 (%#x)", v, tls.VersionTLS13)
			}
		})
	}
}

func TestDialUntrustedCert(t *testing.T) {
	t.Parallel()
	srv, err := NewServer(tls.X25519MLKEM768)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { srv.Close() })

	_, err = Dial(srv.Addr(), []byte("not a certificate"), tls.X25519MLKEM768)
	if !errors.Is(err, ErrUntrustedCert) {
		t.Fatalf("Dial with junk PEM: err = %v; want errors.Is ErrUntrustedCert", err)
	}
}

func TestGroupName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		id   tls.CurveID
		want string
	}{
		{tls.X25519MLKEM768, "X25519MLKEM768"},
		{tls.X25519, "X25519"},
		{tls.CurveP256, "P-256"},
	}
	for _, tt := range tests {
		if got := GroupName(tt.id); got != tt.want {
			t.Errorf("GroupName(%d) = %q; want %q", uint16(tt.id), got, tt.want)
		}
	}
}

func Example() {
	srv, err := NewServer(tls.X25519MLKEM768, tls.X25519)
	if err != nil {
		panic(err)
	}
	defer srv.Close()

	conn, err := Dial(srv.Addr(), srv.CertPEM(), tls.X25519MLKEM768)
	if err != nil {
		panic(err)
	}
	defer conn.Close()

	fmt.Println(GroupName(NegotiatedGroup(conn)))
	// Output: X25519MLKEM768
}
```

## Review

The endpoint is correct when a hybrid-capable client negotiates
`X25519MLKEM768` on TLS 1.3 and a classical-only client transparently falls back
to `X25519`, both confirmed by reading `ConnectionState().CurveID`. If the
fallback case ever reported the hybrid group, the observation would be wrong (or
hard-coded); if the hybrid case reported `X25519`, either the client did not offer
the group or `MinVersion` slipped below 1.3.

The mistakes to avoid: pinning `MinVersion` below TLS 1.3 and still expecting PQ
(there is none on 1.2); trying to make the hybrid group "win" by ordering
`CurvePreferences` (order is ignored — membership is what matters); and inferring
the negotiated group from the cipher suite instead of `CurveID`. When a real
network path misbehaves with the larger ClientHello, reach for the documented kill
switch `GODEBUG=tlsmlkem=0` rather than a code change. Run `go test -race` to
confirm the loopback handshake and the fallback assertion.

## Resources

- [`crypto/tls`](https://pkg.go.dev/crypto/tls) — `CurveID`, `Config.CurvePreferences`, `ConnectionState.CurveID`, and `X25519MLKEM768`.
- [Go 1.24 release notes](https://go.dev/doc/go1.24) — `X25519MLKEM768` as a default and the `tlsmlkem` GODEBUG.
- [Go 1.25 release notes](https://go.dev/doc/go1.25) — `ConnectionState.CurveID`.

---

Back to [01-hybrid-kem-handshake.md](01-hybrid-kem-handshake.md) | Next: [03-enforce-pq-posture.md](03-enforce-pq-posture.md)
