# Exercise 3: Per-Upstream Trust Isolation

The proxy verifies callers with one trust anchor and backends with another, and it must never let backend A's CA vouch for backend B. This exercise builds the trust machinery: `TrustBundle` turns DER-encoded CA certificates into an `x509.CertPool`, `DownstreamConfig` produces a server config that requires and verifies client certificates, and `UpstreamConfig` produces a client config with a per-backend `RootCAs` pool so trust is isolated one backend at a time.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs — including a minimal certificate store — and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
trust.go             CertStore, TrustBundle, DownstreamConfig, UpstreamConfig
cmd/
  demo/
    main.go          build a bundle, a downstream config, two isolated upstream configs
trust_test.go        downstream requires client cert, upstream sets RootCAs, per-backend isolation, bundle parse
```

- Files: `trust.go`, `cmd/demo/main.go`, `trust_test.go`.
- Implement: `TrustBundle(derCerts [][]byte) (*x509.CertPool, error)`, `DownstreamConfig(cs *CertStore, clientCAs *x509.CertPool) *tls.Config`, and `UpstreamConfig(cs *CertStore, rootCAs *x509.CertPool) *tls.Config`, plus the minimal `CertStore` the configs serve certificates from.
- Test: the downstream config requires and verifies client certificates and uses TLS 1.3; the upstream config sets `RootCAs` and TLS 1.3; two upstream configs for different backends have distinct pools; `TrustBundle` accepts valid DER and rejects garbage.
- Verify: `go test -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Why one config per upstream and an explicit CA pool

There are two trust decisions on every proxied request, and they use two different fields. On the downstream (server) side the proxy decides which callers it will accept, so it sets `ClientAuth` to `tls.RequireAndVerifyClientCert` and points `ClientCAs` at the pool of CAs allowed to issue caller certificates. On the upstream (client) side the proxy decides which backend it is willing to talk to, so it sets `RootCAs` to the pool of CAs allowed to issue that backend's certificate. The proxy's own certificate — the identity it presents in both roles — comes from the shared `CertStore` via the `GetCertificate` and `GetClientCertificate` callbacks, exactly as in exercise 1.

The isolation rule is the reason `UpstreamConfig` takes a pool per call rather than a single global pool. If every upstream shared one `RootCAs` containing every backend's CA, then a certificate signed by backend A's CA would verify successfully on a connection to backend B, and compromising A's CA would let an attacker impersonate B. Building one config per backend, each with a pool containing only that backend's CA, confines the blast radius of any single CA compromise to the one backend it signs for. The test `TestUpstreamConfigIsolatesTrustPerBackend` pins this by asserting the two configs hold distinct pool pointers.

`TrustBundle` is the small constructor that makes the explicit pool ergonomic. The system root pool is the wrong default for a mesh: a mesh CA is not a public CA and is not in the OS trust store, so verifying against system roots fails every handshake with `x509: certificate signed by unknown authority`. `TrustBundle` parses each DER block with `x509.ParseCertificate` and adds it to a fresh `x509.NewCertPool()`, returning an error on the first block that fails to parse so a malformed CA is caught at construction rather than at the first failed handshake.

Both configs set `MinVersion: tls.VersionTLS13`. Pinning the floor at 1.3 removes the legacy downgrade surface and is the right default for service-internal traffic where both ends are under your control.

Create `trust.go`:

```go
package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"sync/atomic"
)

// CertStore holds the proxy's certificate behind an atomic pointer and serves
// it to both the downstream and upstream TLS configs via the standard-library
// callbacks. It is the same identity in both roles.
type CertStore struct {
	cert atomic.Pointer[tls.Certificate]
}

// NewCertStore initialises a CertStore with cert as the current certificate.
func NewCertStore(cert *tls.Certificate) *CertStore {
	cs := &CertStore{}
	cs.cert.Store(cert)
	return cs
}

// Get returns the current certificate or nil if none has been stored.
func (cs *CertStore) Get() *tls.Certificate {
	return cs.cert.Load()
}

// GetCertificate implements tls.Config.GetCertificate for server-side use.
func (cs *CertStore) GetCertificate(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
	c := cs.cert.Load()
	if c == nil {
		return nil, fmt.Errorf("mtls: no server certificate loaded")
	}
	return c, nil
}

// GetClientCertificate implements tls.Config.GetClientCertificate for client-side use.
func (cs *CertStore) GetClientCertificate(_ *tls.CertificateRequestInfo) (*tls.Certificate, error) {
	c := cs.cert.Load()
	if c == nil {
		return nil, fmt.Errorf("mtls: no client certificate loaded")
	}
	return c, nil
}

// TrustBundle builds an *x509.CertPool from a set of DER-encoded CA
// certificates. It returns an error if any DER block fails to parse.
func TrustBundle(derCerts [][]byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	for i, der := range derCerts {
		ca, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, fmt.Errorf("mtls: trust bundle cert %d: %w", i, err)
		}
		pool.AddCert(ca)
	}
	return pool, nil
}

// DownstreamConfig returns a *tls.Config for the proxy's downstream
// (server-side) listener. It requires client certificates and verifies them
// against clientCAs. The server certificate is served from cs and
// hot-swappable at runtime.
func DownstreamConfig(cs *CertStore, clientCAs *x509.CertPool) *tls.Config {
	return &tls.Config{
		ClientAuth:     tls.RequireAndVerifyClientCert,
		ClientCAs:      clientCAs,
		GetCertificate: cs.GetCertificate,
		MinVersion:     tls.VersionTLS13,
	}
}

// UpstreamConfig returns a *tls.Config for the proxy's upstream (client-side)
// dialer. The proxy presents its own certificate (from cs) to the backend and
// verifies the backend's certificate against rootCAs. Use a separate config per
// upstream to isolate CA trust per backend.
func UpstreamConfig(cs *CertStore, rootCAs *x509.CertPool) *tls.Config {
	return &tls.Config{
		GetClientCertificate: cs.GetClientCertificate,
		RootCAs:              rootCAs,
		MinVersion:           tls.VersionTLS13,
	}
}
```

The `CertStore` here is the minimal slice of exercise 1 the configs need — `Get` plus the two callbacks — bundled inline so this module stands alone. Note what the configs do not set: `Certificates` is left empty, because the only path to a certificate is the callback, which keeps rotation race-free. `DownstreamConfig` uses `RequireAndVerifyClientCert`, the strictest `ClientAuth` mode, which both demands a client certificate and verifies it against `ClientCAs`; the weaker `RequireAnyClientCert` would accept an unverified certificate, which is not mTLS.

### The runnable demo

The demo builds a CA, derives a trust bundle from its DER, constructs a downstream config and two upstream configs for two different backends, and prints the security-relevant fields to show the downstream requires client auth, both ends pin TLS 1.3, and the two upstreams hold distinct pools.

Create `cmd/demo/main.go`:

```go
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"log"
	"math/big"
	"time"

	"example.com/trust-isolation"
)

func main() {
	caCert, caKey := newCA("demo-ca")
	caPool := x509.NewCertPool()
	caPool.AddCert(caCert)

	proxy := newLeaf(caCert, caKey, "proxy")
	cs := mtls.NewCertStore(proxy)

	bundle, err := mtls.TrustBundle([][]byte{caCert.Raw})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("trust bundle built from %d CA(s): %v\n", 1, bundle != nil)

	down := mtls.DownstreamConfig(cs, bundle)
	fmt.Printf("downstream requires+verifies client cert: %v\n", down.ClientAuth == tls.RequireAndVerifyClientCert)
	fmt.Printf("downstream min version is TLS 1.3: %v\n", down.MinVersion == tls.VersionTLS13)

	backendACA, _ := newCA("backend-a-ca")
	backendBCA, _ := newCA("backend-b-ca")
	poolA := x509.NewCertPool()
	poolA.AddCert(backendACA)
	poolB := x509.NewCertPool()
	poolB.AddCert(backendBCA)

	upA := mtls.UpstreamConfig(cs, poolA)
	upB := mtls.UpstreamConfig(cs, poolB)
	fmt.Printf("upstream min version is TLS 1.3: %v\n", upA.MinVersion == tls.VersionTLS13)
	fmt.Printf("backend A and B trust pools isolated: %v\n", upA.RootCAs != upB.RootCAs)
}

func newCA(cn string) (*x509.Certificate, *ecdsa.PrivateKey) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		log.Fatal(err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		log.Fatal(err)
	}
	return ca, key
}

func newLeaf(ca *x509.Certificate, caKey *ecdsa.PrivateKey, cn string) *tls.Certificate {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		log.Fatal(err)
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
trust bundle built from 1 CA(s): true
downstream requires+verifies client cert: true
downstream min version is TLS 1.3: true
upstream min version is TLS 1.3: true
backend A and B trust pools isolated: true
```

### Tests

The tests pin the security-relevant configuration. `TestDownstreamConfigRequiresClientCert` asserts `RequireAndVerifyClientCert`, the right `ClientCAs` pool, and the TLS 1.3 floor. `TestUpstreamConfigSetsRootCAs` asserts the upstream config's `RootCAs` and floor. `TestUpstreamConfigIsolatesTrustPerBackend` builds two configs from two CAs and asserts their pools are distinct. `TestTrustBundleAcceptsValidDER` and `TestTrustBundleRejectsInvalidDER` cover the bundle parser's happy and error paths.

Create `trust_test.go`:

```go
package mtls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"
)

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newTestCA(t *testing.T) testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return testCA{cert: ca, key: key, pool: pool}
}

func newLeafCert(t *testing.T, ca testCA, cn string) *tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func TestDownstreamConfigRequiresClientCert(t *testing.T) {
	t.Parallel()

	ca := newTestCA(t)
	cs := NewCertStore(newLeafCert(t, ca, "proxy"))

	cfg := DownstreamConfig(cs, ca.pool)
	if cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("ClientAuth = %v, want RequireAndVerifyClientCert", cfg.ClientAuth)
	}
	if cfg.ClientCAs != ca.pool {
		t.Fatal("ClientCAs should be the provided pool")
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Fatalf("MinVersion = %x, want TLS 1.3 (%x)", cfg.MinVersion, tls.VersionTLS13)
	}
}

func TestUpstreamConfigSetsRootCAs(t *testing.T) {
	t.Parallel()

	ca := newTestCA(t)
	cs := NewCertStore(newLeafCert(t, ca, "proxy"))

	cfg := UpstreamConfig(cs, ca.pool)
	if cfg.RootCAs != ca.pool {
		t.Fatal("RootCAs should be the provided pool")
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Fatalf("MinVersion = %x, want TLS 1.3 (%x)", cfg.MinVersion, tls.VersionTLS13)
	}
}

func TestUpstreamConfigIsolatesTrustPerBackend(t *testing.T) {
	t.Parallel()

	ca1 := newTestCA(t)
	ca2 := newTestCA(t)
	cs := NewCertStore(newLeafCert(t, ca1, "proxy"))

	cfg1 := UpstreamConfig(cs, ca1.pool)
	cfg2 := UpstreamConfig(cs, ca2.pool)

	if cfg1.RootCAs == cfg2.RootCAs {
		t.Fatal("configs for different backends must have distinct RootCAs pools")
	}
}

func TestTrustBundleAcceptsValidDER(t *testing.T) {
	t.Parallel()

	ca := newTestCA(t)
	pool, err := TrustBundle([][]byte{ca.cert.Raw})
	if err != nil {
		t.Fatalf("TrustBundle: %v", err)
	}
	if pool == nil {
		t.Fatal("TrustBundle returned nil pool")
	}
}

func TestTrustBundleRejectsInvalidDER(t *testing.T) {
	t.Parallel()

	if _, err := TrustBundle([][]byte{[]byte("not a certificate")}); err == nil {
		t.Fatal("TrustBundle should return error for invalid DER")
	}
}
```

## Review

The configs are sound when the two trust fields are set on the right sides: `ClientCAs` with `RequireAndVerifyClientCert` on the downstream server config, `RootCAs` on the upstream client config, both with `MinVersion: tls.VersionTLS13` and an empty `Certificates` slice so the callbacks remain the only certificate path. The mistake the isolation test exists to catch is sharing one `RootCAs` pool across every backend, which silently lets backend A's CA authenticate backend B; `UpstreamConfig` taking a pool per call and `TestUpstreamConfigIsolatesTrustPerBackend` asserting distinct pointers are the guard. The mistake `TrustBundle` exists to prevent is verifying against the system root pool, which never contains a mesh CA and fails every handshake with `unknown authority`; build the explicit pool and let `TestTrustBundleRejectsInvalidDER` confirm a malformed CA is rejected at construction instead of surfacing as a confusing handshake failure later.

## Resources

- [`crypto/tls.Config`](https://pkg.go.dev/crypto/tls#Config) — the `ClientAuth`, `ClientCAs`, `RootCAs`, and `MinVersion` fields these constructors set.
- [`crypto/x509.CertPool`](https://pkg.go.dev/crypto/x509#CertPool) — `NewCertPool` and `AddCert`, the basis of an explicit per-backend trust anchor.
- [`crypto/tls.ClientAuthType`](https://pkg.go.dev/crypto/tls#ClientAuthType) — the `RequireAndVerifyClientCert` mode and how it differs from the weaker client-auth modes.

---

Back to [02-spiffe-identity.md](02-spiffe-identity.md) | Next: [04-expiry-monitoring.md](04-expiry-monitoring.md)
