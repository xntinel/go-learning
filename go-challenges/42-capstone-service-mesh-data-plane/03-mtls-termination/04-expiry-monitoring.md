# Exercise 4: Certificate Expiry Monitoring

A certificate that lapses without warning turns every new handshake into `x509: certificate has expired or is not yet valid` — a total outage with no notice. This exercise builds the `ExpiryWarner`: it parses the leaf out of the stored certificate, computes the time remaining, and fires a callback whenever that drops below a configurable threshold, giving operators a rotation window before the failure.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs — including a minimal certificate store — and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
expiry.go            CertStore, ExpiryWarner, NewExpiryWarner, Run, CheckNow
cmd/
  demo/
    main.go          store a soon-to-expire cert, run a check, observe the warning
expiry_test.go       fires for an expiring cert, silent for a long-lived one, fires for an already-expired one
```

- Files: `expiry.go`, `cmd/demo/main.go`, `expiry_test.go`.
- Implement: `ExpiryWarner` with `NewExpiryWarner`, `Run(ctx, interval)`, and `CheckNow()`, plus the minimal `CertStore` it reads the certificate from.
- Test: a certificate within the threshold fires the callback, a long-lived certificate does not, and an already-expired certificate (negative remaining) still fires.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p expiry-monitoring/cmd/demo && cd expiry-monitoring
go mod init example.com/expiry-monitoring
go mod edit -go=1.26
```

### Why parse from Certificate[0] and why a synchronous CheckNow

The expiry instant lives on the leaf's `NotAfter` field, and the only reliable way to read it from a hand-constructed `tls.Certificate` is to parse the raw DER. The tempting shortcut, `cert.Leaf.NotAfter`, is a panic waiting to happen: `tls.Certificate.Leaf` is nil unless the certificate was produced by `tls.LoadX509KeyPair` and the leaf was parsed by the caller. A certificate built by hand — by rotation code, by a test, by a mesh agent assembling DER from an issuance API — has a nil `Leaf`, so the warner always parses `cert.Certificate[0]`, which is the leaf DER and is always present when the certificate is constructed correctly:

```go
leaf, err := x509.ParseCertificate(cert.Certificate[0])
remaining := time.Until(leaf.NotAfter)
```

`time.Until` returns a signed duration. A certificate that is still valid yields a positive remaining; one that has already expired yields a negative remaining. The threshold comparison `remaining < threshold` therefore fires for both the soon-to-expire case and the already-expired case with no special-casing, which is the behavior you want — an expired certificate is the most urgent warning of all.

The warner runs on a ticker in its own goroutine via `Run(ctx, interval)`, checking on every tick until the context is cancelled. That is correct for production but awkward for a test, which would have to sleep for at least one interval to observe a single check. So the per-tick work lives in `CheckNow`, a synchronous method that runs exactly one check, and `Run` simply calls it on each tick. A test calls `CheckNow` directly and asserts on the callback with no sleeping and no flakiness; the ticker loop in `Run` is thin enough to read and trust by inspection.

Create `expiry.go`:

```go
package mtls

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"sync/atomic"
	"time"
)

// CertStore holds the certificate the warner inspects, behind an atomic pointer
// so a rotation is race-free.
type CertStore struct {
	cert atomic.Pointer[tls.Certificate]
}

// NewCertStore initialises a CertStore with cert as the current certificate.
func NewCertStore(cert *tls.Certificate) *CertStore {
	cs := &CertStore{}
	cs.cert.Store(cert)
	return cs
}

// Swap replaces the stored certificate; the next check inspects the new one.
func (cs *CertStore) Swap(cert *tls.Certificate) {
	cs.cert.Store(cert)
}

// Get returns the current certificate or nil if none has been stored.
func (cs *CertStore) Get() *tls.Certificate {
	return cs.cert.Load()
}

// ExpiryWarner checks certificate expiration at a fixed interval. It calls warn
// with the subject and remaining lifetime whenever a certificate will expire
// within threshold.
type ExpiryWarner struct {
	store     *CertStore
	threshold time.Duration
	warn      func(subject string, remaining time.Duration)
}

// NewExpiryWarner creates an ExpiryWarner for cs that fires warn whenever the
// stored certificate expires within threshold.
func NewExpiryWarner(cs *CertStore, threshold time.Duration, warn func(subject string, remaining time.Duration)) *ExpiryWarner {
	return &ExpiryWarner{store: cs, threshold: threshold, warn: warn}
}

// Run checks the certificate every interval until ctx is done. Call it in a
// dedicated goroutine: go warner.Run(ctx, time.Hour).
func (ew *ExpiryWarner) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ew.CheckNow()
		}
	}
}

// CheckNow runs a single expiration check synchronously. Exposed so tests can
// trigger a check without waiting for a tick.
func (ew *ExpiryWarner) CheckNow() {
	cert := ew.store.Get()
	if cert == nil || len(cert.Certificate) == 0 {
		return
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return
	}
	remaining := time.Until(leaf.NotAfter)
	if remaining < ew.threshold {
		ew.warn(leaf.Subject.CommonName, remaining)
	}
}
```

`CheckNow` is defensive on the read path: a nil certificate or one with no DER simply returns without firing, because "no certificate to check" is not the same as "certificate expiring". A parse error is treated the same way — there is nothing actionable to warn about — though in practice `Certificate[0]` of a correctly built `tls.Certificate` always parses. The warner reads the certificate fresh from the store on every check, so a rotation that swaps in a longer-lived certificate is picked up on the next tick with no extra wiring.

### The runnable demo

The demo stores a certificate that expires in an hour, sets the threshold to 48 hours so the check fires, and prints the warning with the remaining lifetime rounded to the minute so the output is stable.

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

	"example.com/expiry-monitoring"
)

func main() {
	expiring := newSelfSigned("expiring-proxy", time.Now().Add(time.Hour))
	cs := mtls.NewCertStore(expiring)

	warner := mtls.NewExpiryWarner(cs, 48*time.Hour, func(subject string, remaining time.Duration) {
		fmt.Printf("warning: %s expires in %s\n", subject, remaining.Round(time.Minute))
	})
	warner.CheckNow()

	longLived := newSelfSigned("long-lived-proxy", time.Now().Add(365*24*time.Hour))
	cs.Swap(longLived)
	fmt.Println("swapped to long-lived cert; checking again")
	warner.CheckNow()
	fmt.Println("no warning for long-lived cert")
}

func newSelfSigned(cn string, notAfter time.Time) *tls.Certificate {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
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
warning: expiring-proxy expires in 1h0m0s
swapped to long-lived cert; checking again
no warning for long-lived cert
```

### Tests

The tests cover the three regimes of the signed-duration comparison. `TestExpiryWarnerFiresForExpiringSoonCert` stores a certificate expiring in an hour against a 48-hour threshold and asserts the callback fires with a positive remaining inside the window. `TestExpiryWarnerSilentForLongLivedCert` stores a year-long certificate and asserts the callback never fires. `TestExpiryWarnerFiresForAlreadyExpiredCert` stores a certificate that expired an hour ago against a zero threshold and asserts the callback still fires with a negative remaining — the already-expired case the signed comparison handles for free.

Create `expiry_test.go`:

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

func newLeafCert(t *testing.T, cn string, notAfter time.Time) *tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func TestExpiryWarnerFiresForExpiringSoonCert(t *testing.T) {
	t.Parallel()

	// Certificate expires in 1 hour; threshold is 48 hours — warn should fire.
	expiring := newLeafCert(t, "expiring-proxy", time.Now().Add(time.Hour))
	cs := NewCertStore(expiring)

	var warned bool
	warner := NewExpiryWarner(cs, 48*time.Hour, func(subject string, remaining time.Duration) {
		if subject == "expiring-proxy" && remaining > 0 && remaining < 48*time.Hour {
			warned = true
		}
	})
	warner.CheckNow()

	if !warned {
		t.Fatal("ExpiryWarner should have fired for a certificate expiring within the threshold")
	}
}

func TestExpiryWarnerSilentForLongLivedCert(t *testing.T) {
	t.Parallel()

	longLived := newLeafCert(t, "long-lived", time.Now().Add(365*24*time.Hour))
	cs := NewCertStore(longLived)

	var warned bool
	warner := NewExpiryWarner(cs, 48*time.Hour, func(_ string, _ time.Duration) {
		warned = true
	})
	warner.CheckNow()

	if warned {
		t.Fatal("ExpiryWarner should not fire for a long-lived certificate")
	}
}

func TestExpiryWarnerFiresForAlreadyExpiredCert(t *testing.T) {
	t.Parallel()

	// Certificate expired 1 hour ago; remaining is negative, threshold is 0.
	expired := newLeafCert(t, "expired-proxy", time.Now().Add(-time.Hour))
	cs := NewCertStore(expired)

	var warned bool
	var got time.Duration
	warner := NewExpiryWarner(cs, 0, func(subject string, remaining time.Duration) {
		if subject == "expired-proxy" {
			warned = true
			got = remaining
		}
	})
	warner.CheckNow()

	if !warned {
		t.Fatal("ExpiryWarner should fire for an already-expired certificate")
	}
	if got >= 0 {
		t.Fatalf("remaining = %v, want negative for an expired certificate", got)
	}
}
```

## Review

The warner is sound when it parses the leaf from `cert.Certificate[0]` rather than `cert.Leaf` — the latter is nil on any hand-built certificate and dereferencing it panics, which is the single mistake this exercise exists to teach. The threshold test must use the signed comparison `remaining < threshold` so that an already-expired certificate, whose `time.Until` is negative, still fires; `TestExpiryWarnerFiresForAlreadyExpiredCert` is the guard that the negative case is not accidentally excluded. Confirm `CheckNow` returns quietly on a nil or empty certificate rather than firing or panicking, since "nothing to check" must not look like "expiring". Splitting the per-tick work into `CheckNow` is what makes the tests synchronous and flake-free; `Run` is just the ticker loop around it, and the whole thing passing under `go test -race ./...` confirms the store read and any concurrent swap stay race-clean.

## Resources

- [`crypto/x509.ParseCertificate`](https://pkg.go.dev/crypto/x509#ParseCertificate) — parsing the leaf DER to read `NotAfter`, the safe alternative to `tls.Certificate.Leaf`.
- [`time.Until`](https://pkg.go.dev/time#Until) — the signed remaining-duration computation that handles already-expired certificates for free.
- [`time.Ticker`](https://pkg.go.dev/time#Ticker) — the fixed-interval ticker driving the background check loop.

---

Back to [03-trust-isolation.md](03-trust-isolation.md) | Next: [../04-load-balancing/00-concepts.md](../04-load-balancing/00-concepts.md)
