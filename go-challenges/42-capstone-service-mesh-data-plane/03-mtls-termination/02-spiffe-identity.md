# Exercise 2: SPIFFE Identity Extraction

Once a client certificate is verified, the proxy still has to answer "who is this caller?" In a mesh the answer is a SPIFFE ID — a `spiffe://trust-domain/path` URI carried in the certificate's SAN. This exercise builds the extractor: pull the peer's identity out of a `tls.ConnectionState`, prefer the SPIFFE URI over a DNS name over the Subject, and parse the trust domain out of a SPIFFE ID.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
identity.go          PeerIdentity, ExtractPeerIdentity, SPIFFETrustDomain, ErrNoPeerCert
cmd/
  demo/
    main.go          build a cert with a SPIFFE SAN, extract its identity and trust domain
identity_test.go     no-cert error, SPIFFE SAN, DNS-only fallback, String precedence, trust domain
```

- Files: `identity.go`, `cmd/demo/main.go`, `identity_test.go`.
- Implement: `PeerIdentity` with `String()`, the function `ExtractPeerIdentity(tls.ConnectionState) (PeerIdentity, error)`, and `SPIFFETrustDomain(id string) string`.
- Test: a connection with no peer certificate returns `ErrNoPeerCert`; a SPIFFE SAN is extracted; a DNS-only certificate falls back to the DNS name; `String` prefers SPIFFE over DNS over Subject; `SPIFFETrustDomain` returns the host for valid SPIFFE IDs and empty otherwise.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/42-capstone-service-mesh-data-plane/03-mtls-termination/02-spiffe-identity/cmd/demo && cd go-solutions/42-capstone-service-mesh-data-plane/03-mtls-termination/02-spiffe-identity
go mod edit -go=1.26
```

### Why a precedence ladder and where the SPIFFE ID lives

After a successful mTLS handshake, `crypto/tls` exposes the peer's verified certificate chain on `tls.ConnectionState.PeerCertificates`, with the leaf at index zero. Everything this extractor needs is on that leaf. A SPIFFE ID is not a special X.509 field — it is an ordinary URI SAN, surfaced by Go as `cert.URIs []*url.URL`. Extraction is a scan for the first URI whose `Scheme` is `"spiffe"`; the trust domain is that URI's `Host` and the workload path is its `Path`.

Not every certificate carries a SPIFFE URI. A backend with a conventional leaf may have only DNS SANs, and a hand-rolled certificate may have only a Subject CommonName. So identity extraction is a precedence ladder, not a single lookup: SPIFFE ID first, then the first DNS SAN, then the Subject CommonName. `PeerIdentity` keeps all three so a caller can use whichever it needs, and `String` collapses them to the single most specific value. A caller that requires a SPIFFE ID specifically must still check `SPIFFEID != ""` rather than trust `String`, because `String` will happily return a DNS name when no SPIFFE URI is present.

The one error case worth a sentinel is "no peer certificate at all", which happens when the connection was not mutually authenticated. Returning a typed `ErrNoPeerCert` lets a caller distinguish "anonymous peer" from "peer with an unusual but present identity" with `errors.Is`, instead of guessing from an empty struct.

Create `identity.go`:

```go
package mtls

import (
	"crypto/tls"
	"errors"
	"net/url"
)

// ErrNoPeerCert is returned by ExtractPeerIdentity when the TLS connection
// carries no peer certificate.
var ErrNoPeerCert = errors.New("mtls: no peer certificate presented")

// PeerIdentity is the service identity extracted from an mTLS peer certificate.
type PeerIdentity struct {
	// SPIFFEID is the SPIFFE URI SAN, e.g. "spiffe://cluster.local/ns/default/sa/frontend".
	// Empty if the certificate carries no URI SAN with scheme "spiffe".
	SPIFFEID string
	// DNSNames are the DNS SANs from the peer certificate.
	DNSNames []string
	// Subject is the certificate Subject CommonName.
	Subject string
}

// String returns the most-specific identity string: SPIFFE ID, then first DNS
// SAN, then Subject.
func (p PeerIdentity) String() string {
	if p.SPIFFEID != "" {
		return p.SPIFFEID
	}
	if len(p.DNSNames) > 0 {
		return p.DNSNames[0]
	}
	return p.Subject
}

// ExtractPeerIdentity extracts the service identity from the first peer
// certificate in the TLS connection state. It prefers a SPIFFE URI SAN (scheme
// "spiffe") over DNS SANs. Returns ErrNoPeerCert if the peer did not present a
// certificate.
func ExtractPeerIdentity(state tls.ConnectionState) (PeerIdentity, error) {
	if len(state.PeerCertificates) == 0 {
		return PeerIdentity{}, ErrNoPeerCert
	}
	cert := state.PeerCertificates[0]
	id := PeerIdentity{
		DNSNames: cert.DNSNames,
		Subject:  cert.Subject.CommonName,
	}
	for _, u := range cert.URIs {
		if u.Scheme == "spiffe" {
			id.SPIFFEID = u.String()
			break
		}
	}
	return id, nil
}

// SPIFFETrustDomain returns the trust domain from a SPIFFE ID URI string, e.g.
// "spiffe://cluster.local/ns/a/sa/b" -> "cluster.local". Returns an empty
// string if id is not a valid SPIFFE URI.
func SPIFFETrustDomain(id string) string {
	u, err := url.Parse(id)
	if err != nil || u.Scheme != "spiffe" {
		return ""
	}
	return u.Host
}
```

`ExtractPeerIdentity` reads only index zero of the chain — the leaf — because that is the certificate the peer proved possession of; the rest of the chain is intermediates and the CA. The SPIFFE scan `break`s on the first match, so a certificate with multiple URI SANs yields the first SPIFFE one deterministically. `SPIFFETrustDomain` is intentionally strict: it rejects anything whose scheme is not exactly `"spiffe"`, so an `https://` URI or a bare string returns empty rather than a misleading host.

### The runnable demo

The demo builds a leaf certificate with both a DNS SAN and a SPIFFE URI SAN, parses it the way the TLS stack would, runs it through `ExtractPeerIdentity`, and prints the resulting identity and trust domain.

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
	"net/url"
	"time"

	"example.com/spiffe-identity"
)

func main() {
	spiffeURI, _ := url.Parse("spiffe://cluster.local/ns/default/sa/frontend")
	leaf := newLeaf("frontend", []string{"frontend.svc"}, []*url.URL{spiffeURI})

	id, err := mtls.ExtractPeerIdentity(tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{leaf},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("identity: %s\n", id)
	fmt.Printf("subject: %s\n", id.Subject)
	fmt.Printf("dns san: %s\n", id.DNSNames[0])
	fmt.Printf("trust domain: %s\n", mtls.SPIFFETrustDomain(id.SPIFFEID))

	anon, err := mtls.ExtractPeerIdentity(tls.ConnectionState{})
	fmt.Printf("anonymous peer: %v (%v)\n", anon, err)
}

func newLeaf(cn string, dnsNames []string, uris []*url.URL) *x509.Certificate {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		DNSNames:     dnsNames,
		URIs:         uris,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		log.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		log.Fatal(err)
	}
	return leaf
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
identity: spiffe://cluster.local/ns/default/sa/frontend
subject: frontend
dns san: frontend.svc
trust domain: cluster.local
anonymous peer:  (mtls: no peer certificate presented)
```

### Tests

The tests cover the whole ladder. `TestExtractPeerIdentityNoCert` asserts the empty connection state yields `ErrNoPeerCert` via `errors.Is`. `TestExtractPeerIdentityWithSPIFFE` builds a certificate with a SPIFFE SAN and checks both the SPIFFE ID and the Subject survive. `TestExtractPeerIdentityWithDNSOnly` builds a certificate with only DNS SANs and asserts the SPIFFE ID is empty. `TestPeerIdentityStringPrefersSPIFFE` drives the precedence directly with table cases. `TestSPIFFETrustDomain` checks the parser accepts SPIFFE URIs and rejects everything else.

Create `identity_test.go`:

```go
package mtls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"testing"
	"time"
)

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", raw, err)
	}
	return u
}

func newLeaf(t *testing.T, cn string, dnsNames []string, uris []*url.URL) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		DNSNames:     dnsNames,
		URIs:         uris,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return leaf
}

func TestExtractPeerIdentityNoCert(t *testing.T) {
	t.Parallel()

	_, err := ExtractPeerIdentity(tls.ConnectionState{})
	if !errors.Is(err, ErrNoPeerCert) {
		t.Fatalf("err = %v, want ErrNoPeerCert", err)
	}
}

func TestExtractPeerIdentityWithSPIFFE(t *testing.T) {
	t.Parallel()

	spiffeURI := mustParseURL(t, "spiffe://cluster.local/ns/default/sa/frontend")
	leaf := newLeaf(t, "frontend", nil, []*url.URL{spiffeURI})

	id, err := ExtractPeerIdentity(tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{leaf},
	})
	if err != nil {
		t.Fatalf("ExtractPeerIdentity: %v", err)
	}
	if id.SPIFFEID != spiffeURI.String() {
		t.Fatalf("SPIFFEID = %q, want %q", id.SPIFFEID, spiffeURI.String())
	}
	if id.Subject != "frontend" {
		t.Fatalf("Subject = %q, want frontend", id.Subject)
	}
}

func TestExtractPeerIdentityWithDNSOnly(t *testing.T) {
	t.Parallel()

	leaf := newLeaf(t, "backend", []string{"backend.svc", "backend.svc.cluster.local"}, nil)

	id, err := ExtractPeerIdentity(tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{leaf},
	})
	if err != nil {
		t.Fatalf("ExtractPeerIdentity: %v", err)
	}
	if id.SPIFFEID != "" {
		t.Fatalf("SPIFFEID = %q, want empty", id.SPIFFEID)
	}
	if len(id.DNSNames) != 2 || id.DNSNames[0] != "backend.svc" {
		t.Fatalf("DNSNames = %v", id.DNSNames)
	}
}

func TestPeerIdentityStringPrefersSPIFFE(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		id   PeerIdentity
		want string
	}{
		{
			name: "spiffe preferred over dns",
			id: PeerIdentity{
				SPIFFEID: "spiffe://cluster.local/ns/a/sa/b",
				DNSNames: []string{"b.svc"},
				Subject:  "b",
			},
			want: "spiffe://cluster.local/ns/a/sa/b",
		},
		{
			name: "dns preferred over subject when no spiffe",
			id: PeerIdentity{
				DNSNames: []string{"b.svc"},
				Subject:  "b",
			},
			want: "b.svc",
		},
		{
			name: "subject as fallback",
			id: PeerIdentity{
				Subject: "b",
			},
			want: "b",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.id.String(); got != tc.want {
				t.Fatalf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSPIFFETrustDomain(t *testing.T) {
	t.Parallel()

	cases := []struct {
		id   string
		want string
	}{
		{"spiffe://cluster.local/ns/default/sa/frontend", "cluster.local"},
		{"spiffe://prod.example.com/svc/payment", "prod.example.com"},
		{"https://not-spiffe.example.com/path", ""},
		{"not-a-uri", ""},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			t.Parallel()
			if got := SPIFFETrustDomain(tc.id); got != tc.want {
				t.Fatalf("SPIFFETrustDomain(%q) = %q, want %q", tc.id, got, tc.want)
			}
		})
	}
}

func ExamplePeerIdentity_String() {
	id := PeerIdentity{
		SPIFFEID: "spiffe://cluster.local/ns/default/sa/frontend",
		DNSNames: []string{"frontend.svc"},
		Subject:  "frontend",
	}
	fmt.Println(id.String())
	// Output: spiffe://cluster.local/ns/default/sa/frontend
}
```

## Review

The extractor is sound when the precedence ladder is exact: SPIFFE ID over the first DNS SAN over the Subject, with `String` collapsing the three and the struct keeping all three for callers that need a specific one. The trap is treating `String` as a SPIFFE accessor — it returns a DNS name or Subject when no SPIFFE URI is present, so an authorization check that needs a real SPIFFE ID must test `SPIFFEID != ""` itself; `TestExtractPeerIdentityWithDNSOnly` is the guard that the SPIFFE field stays empty rather than borrowing the DNS name. `ExtractPeerIdentity` must read only the leaf at index zero and must return `ErrNoPeerCert` for an unauthenticated connection so a caller can tell an anonymous peer apart from one with an unusual identity with `errors.Is`. `SPIFFETrustDomain` has to reject any non-`spiffe` scheme, which `TestSPIFFETrustDomain` pins with the `https://` and bare-string cases. The example test additionally compiles into the package's documentation and is run by `go test`, so the `// Output:` line is verified, not decorative.

## Resources

- [SPIFFE ID specification](https://github.com/spiffe/spiffe/blob/main/standards/SPIFFE-ID.md) — the `spiffe://trust-domain/path` URI format and its encoding as a URI SAN.
- [`crypto/x509.Certificate`](https://pkg.go.dev/crypto/x509#Certificate) — the `URIs`, `DNSNames`, and `Subject` fields this extractor reads from the leaf.
- [`net/url.Parse`](https://pkg.go.dev/net/url#Parse) — parsing the SPIFFE URI and reading its `Scheme` and `Host`.

---

Back to [01-cert-rotation.md](01-cert-rotation.md) | Next: [03-trust-isolation.md](03-trust-isolation.md)
