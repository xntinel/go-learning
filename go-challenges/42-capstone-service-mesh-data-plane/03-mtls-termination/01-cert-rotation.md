# Exercise 1: Hot Certificate Rotation

A mesh proxy is handed a new certificate every hour or so and must start serving it without a restart and without a single failed handshake. This exercise builds the object that makes that possible: a `CertStore` that holds the current `tls.Certificate` behind an `atomic.Pointer` and exposes the two `crypto/tls` callbacks — `GetCertificate` for the server side and `GetClientCertificate` for the client side — so a swap is wait-free and never races a handshake.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
rotation.go          CertStore, NewCertStore, Swap, Get, GetCertificate, GetClientCertificate
cmd/
  demo/
    main.go          store v1, swap to v2, prove the store now serves v2
rotation_test.go     initial read, atomic swap visibility, both callbacks, empty-store error
```

- Files: `rotation.go`, `cmd/demo/main.go`, `rotation_test.go`.
- Implement: `CertStore` with `NewCertStore`, `Swap`, `Get`, `GetCertificate`, and `GetClientCertificate`.
- Test: the store returns the initial certificate, a `Swap` is atomically visible to both callbacks, and an empty store returns an error instead of a nil certificate.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p cert-rotation/cmd/demo && cd cert-rotation
go mod init example.com/cert-rotation
go mod edit -go=1.26
```

### Why an atomic pointer and two callbacks

A `tls.Config` is built once and shared by every goroutine that performs a handshake. The handshake reads the server certificate from the config at the moment a connection arrives. If rotation works by mutating the config's `Certificates` slice in place, that write races the handshake's read: the race detector fires and, worse, the bytes a peer receives can be a torn mix of old and new. The standard library anticipates this and offers two callback fields that it invokes fresh on every handshake instead of reading a fixed slice:

- `GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error)` — the server side calls it once per incoming handshake to choose the certificate to present.
- `GetClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error)` — the client side calls it once per outgoing handshake when the server requests a client certificate.

Because both callbacks run per connection, the store only has to answer "what is the current certificate?" and the freshest value is automatically served to every new connection. Connections already in progress keep the certificate they handshook with, which is exactly the behavior you want: rotation affects the future, not the present.

The store itself is one `atomic.Pointer[tls.Certificate]`. `Store` publishes a new value and `Load` reads the latest, both wait-free and with no lock on the hot path. A rotation is a single `Store`; a handshake is a single `Load`. There is no window in which a reader sees a half-updated value, because the pointer is swapped atomically — the reader sees either the whole old certificate or the whole new one. That is why the demo and tests can run under `-race` with concurrent swaps and reads and stay clean.

Create `rotation.go`:

```go
package mtls

import (
	"crypto/tls"
	"fmt"
	"sync/atomic"
)

// CertStore holds a TLS certificate behind an atomic pointer so that swapping
// the certificate is wait-free and safe under the race detector. New
// connections always see the latest certificate; connections already in
// progress keep their original certificate for the lifetime of that connection.
type CertStore struct {
	cert atomic.Pointer[tls.Certificate]
}

// NewCertStore initialises a CertStore with cert as the initial certificate.
func NewCertStore(cert *tls.Certificate) *CertStore {
	cs := &CertStore{}
	cs.cert.Store(cert)
	return cs
}

// Swap replaces the stored certificate. All new connections after this call
// receive the new certificate. In-flight connections are unaffected.
func (cs *CertStore) Swap(cert *tls.Certificate) {
	cs.cert.Store(cert)
}

// Get returns the current certificate or nil if none has been stored.
func (cs *CertStore) Get() *tls.Certificate {
	return cs.cert.Load()
}

// GetCertificate implements tls.Config.GetCertificate for server-side use. It
// is called once per incoming TLS handshake and returns the latest certificate.
func (cs *CertStore) GetCertificate(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
	c := cs.cert.Load()
	if c == nil {
		return nil, fmt.Errorf("mtls: no server certificate loaded")
	}
	return c, nil
}

// GetClientCertificate implements tls.Config.GetClientCertificate for
// client-side use. It is called once per outgoing TLS handshake when the server
// requests a certificate.
func (cs *CertStore) GetClientCertificate(_ *tls.CertificateRequestInfo) (*tls.Certificate, error) {
	c := cs.cert.Load()
	if c == nil {
		return nil, fmt.Errorf("mtls: no client certificate loaded")
	}
	return c, nil
}
```

The two callbacks are deliberately identical except for their signature: `crypto/tls` hands the server callback a `*tls.ClientHelloInfo` and the client callback a `*tls.CertificateRequestInfo`, neither of which this store consults — a single proxy identity is presented in both roles. Wire the store into a config by assigning the callbacks and leaving `Certificates` empty; that empty slice is the whole point, because the only path to a certificate is the callback, and the callback is the only thing that ever touches the atomic pointer.

### The runnable demo

The demo makes the swap concrete. It generates two self-signed certificates with distinct CommonNames, stores the first, swaps in the second, then asks the store's server callback what it would serve and prints the CommonName to prove the new certificate won.

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

	"example.com/cert-rotation"
)

func main() {
	v1 := newSelfSigned("proxy-v1")
	v2 := newSelfSigned("proxy-v2")

	cs := mtls.NewCertStore(v1)
	fmt.Println("stored proxy-v1")

	cs.Swap(v2)
	fmt.Println("swapped to proxy-v2")

	served, err := cs.GetCertificate(nil)
	if err != nil {
		log.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(served.Certificate[0])
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("server callback now serves: %s\n", leaf.Subject.CommonName)

	client, err := cs.GetClientCertificate(nil)
	if err != nil {
		log.Fatal(err)
	}
	clientLeaf, err := x509.ParseCertificate(client.Certificate[0])
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("client callback now serves: %s\n", clientLeaf.Subject.CommonName)
}

func newSelfSigned(cn string) *tls.Certificate {
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
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
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
stored proxy-v1
swapped to proxy-v2
server callback now serves: proxy-v2
client callback now serves: proxy-v2
```

### Tests

The tests pin three properties. The store returns the exact pointer it was given (`Get` and both callbacks). A `Swap` is immediately visible through every read path, which is the rotation guarantee. An empty store — one constructed without a certificate — returns an error from the callbacks rather than a nil certificate that would later panic the TLS stack.

Create `rotation_test.go`:

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

func newCert(t *testing.T, cn string) *tls.Certificate {
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
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func TestCertStoreGetReturnsInitialCert(t *testing.T) {
	t.Parallel()

	cert := newCert(t, "proxy")
	cs := NewCertStore(cert)

	if cs.Get() != cert {
		t.Fatal("Get() should return the initial certificate")
	}
}

func TestCertStoreSwapIsAtomicallyVisible(t *testing.T) {
	t.Parallel()

	first := newCert(t, "proxy-v1")
	second := newCert(t, "proxy-v2")

	cs := NewCertStore(first)
	cs.Swap(second)

	if cs.Get() != second {
		t.Fatal("Get() should return the swapped certificate")
	}
}

func TestGetCertificateReturnsCurrentCert(t *testing.T) {
	t.Parallel()

	cert := newCert(t, "proxy")
	cs := NewCertStore(cert)

	got, err := cs.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if got != cert {
		t.Fatal("GetCertificate should return the stored certificate")
	}
}

func TestGetCertificateReturnsSwappedCert(t *testing.T) {
	t.Parallel()

	first := newCert(t, "proxy-v1")
	second := newCert(t, "proxy-v2")

	cs := NewCertStore(first)
	cs.Swap(second)

	got, err := cs.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if got != second {
		t.Fatal("GetCertificate should return the swapped certificate after Swap")
	}
}

func TestGetClientCertificateReturnsCurrentCert(t *testing.T) {
	t.Parallel()

	cert := newCert(t, "proxy")
	cs := NewCertStore(cert)

	got, err := cs.GetClientCertificate(nil)
	if err != nil {
		t.Fatalf("GetClientCertificate: %v", err)
	}
	if got != cert {
		t.Fatal("GetClientCertificate should return the stored certificate")
	}
}

func TestGetCertificateErrorsWithNoCert(t *testing.T) {
	t.Parallel()

	cs := &CertStore{}
	if _, err := cs.GetCertificate(nil); err == nil {
		t.Fatal("expected error when no certificate is stored")
	}
	if _, err := cs.GetClientCertificate(nil); err == nil {
		t.Fatal("expected error from GetClientCertificate when no certificate is stored")
	}
}
```

## Review

The store is sound when every read path returns the exact pointer last stored and a swap is visible the instant it completes. The regression to guard against is rotation by slice mutation: the moment `cfg.Certificates[0]` is written while a handshake reads it, `-race` fires, which is why the whole store is one `atomic.Pointer` and the callbacks only `Load`. Confirm the empty-store case returns an error from both callbacks rather than a nil certificate, because the TLS stack will dereference whatever the callback returns and a nil there panics the handshake goroutine. The two callbacks differ only in the unused `*tls.ClientHelloInfo` versus `*tls.CertificateRequestInfo` argument; both must consult the same pointer so the proxy presents one identity in both its server and client roles. All of the tests passing under `go test -race ./...` is the real bar, because the race detector is what proves the swap path and the read path never touch the pointer non-atomically.

## Resources

- [`crypto/tls` GetCertificate](https://pkg.go.dev/crypto/tls#Config) — the `Config` callback fields `GetCertificate` and `GetClientCertificate` invoked per handshake.
- [`sync/atomic.Pointer`](https://pkg.go.dev/sync/atomic#Pointer) — the typed atomic pointer and its sequential-consistency guarantees for safe concurrent access.
- [Go Blog: TLS cipher suites](https://go.dev/blog/tls-cipher-suites) — how the standard library handles TLS configuration and defaults.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-spiffe-identity.md](02-spiffe-identity.md)
