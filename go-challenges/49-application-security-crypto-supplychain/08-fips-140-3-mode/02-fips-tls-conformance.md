# Exercise 2: FIPS-Approved TLS Configuration and Handshake Conformance

A `tls.Config` with the right fields is a claim; a conformance test that inspects
the *negotiated* connection is proof. This exercise builds a hardened
`tls.Config` (TLS 1.2 floor, explicit approved cipher-suite and curve allowlists)
plus a loopback handshake and a conformance checker that rejects any run whose
protocol version, cipher suite, or curve falls outside the FIPS-approved set — and
a negative path proving a TLS 1.1 peer is refused, so the config is validated by
behavior rather than by comment.

This module is fully self-contained: its own `go mod init`, cert generation
inline, its own demo and tests. The handshake runs over `net.Pipe`, so no network
and no FIPS build are needed to test it.

## What you'll build

```text
fipstls/                     independent module: example.com/fipstls
  go.mod                     go 1.26
  fipstls.go                 approved allowlists; ServerConfig/ClientConfig; Handshake; VerifyConformance
  cmd/
    demo/
      main.go                loopback TLS 1.3 handshake + a refused TLS 1.1 client
  fipstls_test.go            pure VerifyConformance table; real handshake; negative path; Example
```

- Files: `fipstls.go`, `cmd/demo/main.go`, `fipstls_test.go`.
- Implement: `ServerConfig`/`ClientConfig` with `MinVersion` TLS 1.2, an approved `CipherSuites` list, and an approved `CurvePreferences` list; `Handshake` over `net.Pipe`; `VerifyConformance(cs tls.ConnectionState) error`.
- Test: a pure table over `VerifyConformance` (approved 1.3 and 1.2 suites pass; ChaCha20, a CBC suite, and TLS 1.1 fail); a real handshake asserting conformance; a negative test where a TLS 1.1 client is refused.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/49-application-security-crypto-supplychain/08-fips-140-3-mode/02-fips-tls-conformance/cmd/demo
cd go-solutions/49-application-security-crypto-supplychain/08-fips-140-3-mode/02-fips-tls-conformance
go mod edit -go=1.26
```

### Why the config is not enough, and what the checker adds

`tls.Config.CipherSuites` only constrains TLS 1.2; the TLS 1.3 suites are chosen
by the stack and are *not* configurable through that field. So a config that looks
locked down can still negotiate a TLS 1.3 suite you did not list, and — outside
FIPS mode — a server without hardware AES can pick `TLS_CHACHA20_POLY1305_SHA256`,
which is *not* FIPS-approved. The config expresses a preference; only the
negotiated `tls.ConnectionState` tells you what actually happened. `VerifyConformance`
inspects that state after the handshake and rejects anything outside the approved
set: version below TLS 1.2, or a cipher suite not on the allowlist. This is the
difference between "we configured FIPS-approved parameters" and "we proved the
connection used them".

Under a real `GODEBUG=fips140=on` build the story is stronger still: `crypto/tls`
itself refuses to negotiate non-approved versions, suites, and curves even if the
config lists them, so the handshake fails rather than completing insecurely. The
conformance checker is the belt to that suspenders — it makes the guarantee
explicit and testable on any build, and it catches the case where you run without
FIPS mode but still require FIPS-approved parameters. (One caveat worth knowing:
the post-quantum `X25519MLKEM768` key exchange has interacted awkwardly with
`fips140=only` in some toolchain versions; if you require the hybrid group, test
it against your exact toolchain.)

### The approved sets and the config

The approved cipher suites are the AES-GCM AEAD suites: the two TLS 1.3 suites
`TLS_AES_128_GCM_SHA256` and `TLS_AES_256_GCM_SHA384`, and the ECDHE AES-GCM
suites for TLS 1.2. ChaCha20-Poly1305 is deliberately excluded because it is not
FIPS-approved. The approved curves are the NIST P-curves; `X25519` alone is not
approved. Note that `CurvePreferences` uses `tls.CurveID` values and constrains
key-exchange groups.

Create `fipstls.go`:

```go
package fipstls

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

// Sentinel errors so a caller can distinguish which conformance rule failed.
var (
	ErrVersionTooLow    = errors.New("tls: negotiated version below TLS 1.2")
	ErrSuiteNotApproved = errors.New("tls: negotiated cipher suite is not FIPS-approved")
)

// approvedSuites is the FIPS-approved AES-GCM AEAD allowlist. ChaCha20-Poly1305
// and CBC suites are intentionally excluded.
var approvedSuites = map[uint16]bool{
	tls.TLS_AES_128_GCM_SHA256:                  true, // TLS 1.3
	tls.TLS_AES_256_GCM_SHA384:                  true, // TLS 1.3
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256: true, // TLS 1.2
	tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256:   true,
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384: true,
	tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384:   true,
}

// approvedCurves are the NIST P-curves. X25519 is not FIPS-approved.
var approvedCurves = []tls.CurveID{tls.CurveP256, tls.CurveP384}

// ServerConfig returns a hardened server config: TLS 1.2 floor, approved TLS 1.2
// cipher suites, and approved curves. TLS 1.3 suites are not configurable here
// and are constrained instead by VerifyConformance on the negotiated state.
func ServerConfig(cert tls.Certificate) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		MaxVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		CipherSuites: []uint16{
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		},
		CurvePreferences: approvedCurves,
	}
}

// ClientConfig returns a client config with the same floor and approved groups.
// InsecureSkipVerify is set because the loopback peer uses a self-signed cert;
// this lesson validates negotiated parameters, not certificate chains.
func ClientConfig() *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		MaxVersion:         tls.VersionTLS13,
		CurvePreferences:   approvedCurves,
		InsecureSkipVerify: true,
	}
}

// VerifyConformance rejects a negotiated connection whose version or cipher suite
// is outside the FIPS-approved set. Its errors are wrapped so callers can match
// them with errors.Is.
func VerifyConformance(cs tls.ConnectionState) error {
	if cs.Version < tls.VersionTLS12 {
		return fmt.Errorf("%w: got %s", ErrVersionTooLow, tls.VersionName(cs.Version))
	}
	if !approvedSuites[cs.CipherSuite] {
		return fmt.Errorf("%w: got %s", ErrSuiteNotApproved, tls.CipherSuiteName(cs.CipherSuite))
	}
	return nil
}

// Handshake performs a TLS handshake over an in-memory net.Pipe and returns the
// client's negotiated ConnectionState. It never touches the network.
func Handshake(serverCfg, clientCfg *tls.Config) (tls.ConnectionState, error) {
	c1, c2 := net.Pipe()
	server := tls.Server(c1, serverCfg)
	client := tls.Client(c2, clientCfg)

	errc := make(chan error, 1)
	go func() {
		errc <- server.Handshake()
		c1.Close()
	}()

	cerr := client.Handshake()
	var cs tls.ConnectionState
	if cerr == nil {
		cs = client.ConnectionState()
	}
	c2.Close() // unblock the server goroutine on the client-error path
	serr := <-errc

	if cerr != nil {
		return tls.ConnectionState{}, cerr
	}
	if serr != nil {
		return tls.ConnectionState{}, serr
	}
	return cs, nil
}

// SelfSignedCert generates an ECDSA P-256 self-signed certificate for loopback
// tests. P-256 works for both the TLS 1.2 ECDSA suites and TLS 1.3.
func SelfSignedCert() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "fips-loopback"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return tls.X509KeyPair(certPEM, keyPEM)
}
```

### The runnable demo

The demo runs a full TLS 1.3 handshake over the loopback pipe and prints the
negotiated version, the cipher-suite name, and the conformance verdict, then runs
a second handshake with a client pinned to TLS 1.0-1.1 and shows the server
refusing it. The cipher-suite name in the first line depends on hardware AES
support (AES-128 vs AES-256 GCM); both are FIPS-approved, so the verdict is
`conformant` either way.

Create `cmd/demo/main.go`:

```go
package main

import (
	"crypto/tls"
	"fmt"

	"example.com/fipstls"
)

func main() {
	cert, err := fipstls.SelfSignedCert()
	if err != nil {
		panic(err)
	}

	cs, err := fipstls.Handshake(fipstls.ServerConfig(cert), fipstls.ClientConfig())
	if err != nil {
		panic(err)
	}
	verdict := "conformant"
	if err := fipstls.VerifyConformance(cs); err != nil {
		verdict = err.Error()
	}
	fmt.Printf("negotiated %s with %s: %s\n",
		tls.VersionName(cs.Version), tls.CipherSuiteName(cs.CipherSuite), verdict)

	// A client that only offers TLS 1.0-1.1 must be refused by the TLS 1.2 floor.
	old := fipstls.ClientConfig()
	old.MinVersion = tls.VersionTLS10
	old.MaxVersion = tls.VersionTLS11
	if _, err := fipstls.Handshake(fipstls.ServerConfig(cert), old); err != nil {
		fmt.Println("legacy client refused")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (the negotiated suite may be AES-256-GCM on some hardware; both
are approved):

```
negotiated TLS 1.3 with TLS_AES_128_GCM_SHA256: conformant
legacy client refused
```

### Tests

`TestVerifyConformance` is a pure table over synthetic `tls.ConnectionState`
values — it is deterministic and needs no handshake, so it can enumerate the
approved 1.3 and 1.2 suites, ChaCha20, a CBC suite, and a TLS 1.1 version without
hardware or toolchain variance. `TestHandshakeConformant` runs a real handshake
pinned to TLS 1.2 with an approved suite so the negotiated suite is deterministic,
then asserts conformance. `TestRejectsLegacyVersion` proves the negative path: a
TLS 1.0-1.1 client cannot complete the handshake against the TLS 1.2 floor. The
`Example` runs a TLS 1.3 handshake and asserts only the version (deterministic
across hardware), avoiding the suite-selection variance.

Create `fipstls_test.go`:

```go
package fipstls

import (
	"crypto/tls"
	"errors"
	"fmt"
	"testing"
)

func TestVerifyConformance(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state tls.ConnectionState
		want  error
	}{
		{"tls1.3 aes128 approved", tls.ConnectionState{Version: tls.VersionTLS13, CipherSuite: tls.TLS_AES_128_GCM_SHA256}, nil},
		{"tls1.3 aes256 approved", tls.ConnectionState{Version: tls.VersionTLS13, CipherSuite: tls.TLS_AES_256_GCM_SHA384}, nil},
		{"tls1.2 ecdhe aes128 approved", tls.ConnectionState{Version: tls.VersionTLS12, CipherSuite: tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256}, nil},
		{"tls1.3 chacha20 not approved", tls.ConnectionState{Version: tls.VersionTLS13, CipherSuite: tls.TLS_CHACHA20_POLY1305_SHA256}, ErrSuiteNotApproved},
		{"tls1.2 cbc not approved", tls.ConnectionState{Version: tls.VersionTLS12, CipherSuite: tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA}, ErrSuiteNotApproved},
		{"tls1.1 version too low", tls.ConnectionState{Version: tls.VersionTLS11, CipherSuite: tls.TLS_AES_128_GCM_SHA256}, ErrVersionTooLow},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := VerifyConformance(tc.state)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("VerifyConformance() = %v; want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("VerifyConformance() = %v; want errors.Is(_, %v)", err, tc.want)
			}
		})
	}
}

func TestHandshakeConformant(t *testing.T) {
	t.Parallel()

	cert, err := SelfSignedCert()
	if err != nil {
		t.Fatalf("SelfSignedCert() = %v", err)
	}
	// Pin TLS 1.2 with a single approved suite so the negotiated suite is
	// deterministic across hardware.
	sc := ServerConfig(cert)
	sc.MaxVersion = tls.VersionTLS12
	sc.CipherSuites = []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256}
	cc := ClientConfig()
	cc.MaxVersion = tls.VersionTLS12

	cs, err := Handshake(sc, cc)
	if err != nil {
		t.Fatalf("Handshake() = %v", err)
	}
	if cs.Version != tls.VersionTLS12 {
		t.Fatalf("negotiated %s; want TLS 1.2", tls.VersionName(cs.Version))
	}
	if err := VerifyConformance(cs); err != nil {
		t.Fatalf("VerifyConformance() = %v; want nil", err)
	}
}

func TestRejectsLegacyVersion(t *testing.T) {
	t.Parallel()

	cert, err := SelfSignedCert()
	if err != nil {
		t.Fatalf("SelfSignedCert() = %v", err)
	}
	old := ClientConfig()
	old.MinVersion = tls.VersionTLS10
	old.MaxVersion = tls.VersionTLS11

	if _, err := Handshake(ServerConfig(cert), old); err == nil {
		t.Fatal("Handshake() succeeded with a TLS 1.1 client; want refusal")
	}
}

func Example() {
	cert, _ := SelfSignedCert()
	cs, err := Handshake(ServerConfig(cert), ClientConfig())
	fmt.Println(err == nil, tls.VersionName(cs.Version))
	// Output: true TLS 1.3
}
```

## Review

The config is correct only when the negotiated state proves it: `VerifyConformance`
must reject any version below TLS 1.2 and any suite off the AES-GCM allowlist,
including ChaCha20-Poly1305 and CBC suites, and the real handshake must land on an
approved suite. The negative test is what makes the floor real — without it, a
config that silently permitted TLS 1.1 would pass a comment review but fail an
audit.

The mistakes to avoid: do not assume `CipherSuites` controls TLS 1.3 — it does
not, which is why the checker inspects the negotiated suite rather than trusting
the config. Do not leave `InsecureSkipVerify` in production code; it is here only
because the loopback peer is self-signed and this lesson checks negotiated
parameters, not chains. And do not read a leaked `net.Pipe` goroutine as harmless:
the `Handshake` helper closes the client side on the error path so the server
goroutine always unblocks, which `go test -race` confirms. Remember that under a
real `fips140=on` build `crypto/tls` enforces the approved set itself, so the
checker's job on that build is to catch the run that was *not* in FIPS mode but
still required FIPS parameters.

## Resources

- [`crypto/tls`](https://pkg.go.dev/crypto/tls) — `Config`, `ConnectionState`, `CipherSuiteName`, `VersionName`, and the cipher-suite and curve constants.
- [FIPS 140-3 Compliance](https://go.dev/doc/security/fips140) — which TLS versions, suites, and curves `crypto/tls` allows in FIPS mode.
- [The FIPS 140-3 Go Cryptographic Module](https://go.dev/blog/fips140) — the Go blog on how FIPS mode narrows TLS negotiation.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-fips-build-attestation.md](03-fips-build-attestation.md)
