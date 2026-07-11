# Exercise 34: TLS Certificate Chain Verification

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye casos borde).

`certificate.Verify` in the standard library already does the hard
cryptographic work — walking the chain, checking signatures, checking
validity windows — but it hands back a slice of chains or an error, not
the two things an operator actually wants to log: did this succeed, and
whose certificate was it. This exercise builds `VerifyChain(leaf, roots,
at) (valid bool, subject string, error)`, walking the verified chain to
report a full trust path, and reporting the leaf's own subject even when
verification fails, so a failure log line names exactly what broke.

This module is fully self-contained: its own `go mod init`, all code
inline, its own demo and tests. All certificates are generated in-process
with `crypto/x509` — no network calls, no filesystem fixtures.

## What you'll build

```text
tlsverify/                 independent module: example.com/tls-cert-verify-with-subject
  go.mod                   go 1.24
  tlsverify.go             package tlsverify; VerifyChain(leaf,roots,at) (valid,subject,error); NewCA; NewLeaf test/demo helpers
  cmd/
    demo/
      main.go              trusted chain, expired leaf, untrusted root
  tlsverify_test.go         valid chain with trust-path subject; expired leaf; not-yet-valid leaf; untrusted root wraps x509.UnknownAuthorityError
```

- Files: `tlsverify.go`, `cmd/demo/main.go`, `tlsverify_test.go`.
- Implement: `VerifyChain(leaf *x509.Certificate, roots *x509.CertPool, at time.Time) (valid bool, subject string, err error)` calling `leaf.Verify` with an explicit `CurrentTime`, returning `(true, "leaf CN -> ... -> root CN", nil)` on success by walking `chains[0]`, and `(false, leaf's own CN, wrapped error)` on any verification failure.
- Test: a leaf signed by a trusted root verifies and reports the full trust path; a leaf whose validity window has already ended fails with a non-nil error while still reporting its own subject; a leaf that is not yet valid at `at` fails the same way; a leaf signed by a CA absent from the trusted pool fails with an error wrapping `x509.UnknownAuthorityError`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/tlsverify/cmd/demo
cd ~/go-exercises/tlsverify
go mod init example.com/tls-cert-verify-with-subject
go mod edit -go=1.24
```

### Why the subject survives a failed verification

`leaf.Verify` returning a bare `error` is already a well-designed API for
the crypto layer — it correctly refuses to hand back a "chain" when there
is no valid chain to hand back. But an operational log line built directly
from that error alone reads "certificate verification failed" with no way
to say *which* certificate, unless the caller separately remembers to log
`leaf.Subject.CommonName` next to the error. `VerifyChain` bakes that
"also report the subject" step into the failure path itself: `subject`
is populated from `leaf.Subject.CommonName` on the error branch, so a
caller cannot forget to log which certificate broke — it is already sitting
in the second return value.

On the success path, `subject` means something richer: not just the leaf's
own name, but the entire verified trust path, built by walking
`chains[0]` — the first of potentially several valid chains `Verify`
found — from leaf to root. That is "chain traversal" in the exercise's
title: the interesting information is not just "it validated" but *through
which intermediate and root* it validated, which matters when a service
trusts more than one CA and needs to know which one actually vouched for
a given connection.

Create `tlsverify.go`:

```go
package tlsverify

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"strings"
	"sync/atomic"
	"time"
)

// VerifyChain checks leaf against roots as of the given instant, walking
// the resulting verified chain to build a human-readable subject trail. It
// distinguishes two outcomes:
//   - invalid:  (false, leaf's own subject, wrapped verification error) --
//     the subject is still reported so an operator can tell which
//     certificate failed, even though the chain did not validate.
//   - valid:    (true, "leaf CN -> ... -> root CN", nil)
//
// at is accepted explicitly (instead of the function calling time.Now()
// internally) so tests can verify expiry and not-yet-valid behavior
// without depending on the real wall clock or a real certificate's actual
// validity window.
func VerifyChain(leaf *x509.Certificate, roots *x509.CertPool, at time.Time) (valid bool, subject string, err error) {
	chains, verifyErr := leaf.Verify(x509.VerifyOptions{
		Roots:       roots,
		CurrentTime: at,
	})
	if verifyErr != nil {
		return false, leaf.Subject.CommonName, fmt.Errorf("verify certificate %q: %w", leaf.Subject.CommonName, verifyErr)
	}

	// chains[0] is one complete, verified path from leaf to a trusted root.
	// Walk it to report the full trust path, not just the leaf's own name.
	names := make([]string, len(chains[0]))
	for i, cert := range chains[0] {
		names[i] = cert.Subject.CommonName
	}
	return true, strings.Join(names, " -> "), nil
}

var serialCounter atomic.Int64

func nextSerial() *big.Int {
	return big.NewInt(serialCounter.Add(1))
}

// NewCA generates a self-signed root CA certificate valid across
// [notBefore, notAfter], for use as a test/demo trust anchor. It is not
// intended for production certificate issuance.
func NewCA(commonName string, notBefore, notAfter time.Time) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate CA key: %w", err)
	}
	template := &x509.Certificate{
		SerialNumber:          nextSerial(),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA certificate: %w", err)
	}
	return cert, key, nil
}

// NewLeaf generates a leaf certificate signed by parent/parentKey, valid
// across [notBefore, notAfter].
func NewLeaf(commonName string, notBefore, notAfter time.Time, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (*x509.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate leaf key: %w", err)
	}
	template := &x509.Certificate{
		SerialNumber: nextSerial(),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, parent, &key.PublicKey, parentKey)
	if err != nil {
		return nil, fmt.Errorf("create leaf certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse leaf certificate: %w", err)
	}
	return cert, nil
}
```

`nextSerial` uses an atomic counter rather than a random serial number so
concurrent test cases generating certificates in parallel (`t.Parallel()`)
never collide or race on a shared source of randomness for this
bookkeeping value.

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"crypto/x509"
	"fmt"
	"time"

	tlsverify "example.com/tls-cert-verify-with-subject"
)

func main() {
	notBefore := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	notAfter := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	checkAt := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	root, rootKey, err := tlsverify.NewCA("Example Root CA", notBefore, notAfter)
	if err != nil {
		panic(err)
	}
	leaf, err := tlsverify.NewLeaf("api.example.com", notBefore, notAfter, root, rootKey)
	if err != nil {
		panic(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)

	valid, subject, err := tlsverify.VerifyChain(leaf, roots, checkAt)
	fmt.Printf("trusted chain:   valid=%t subject=%q err=%v\n", valid, subject, err)

	// A leaf that expired before checkAt.
	expiredLeaf, err := tlsverify.NewLeaf("old.example.com", notBefore, checkAt.Add(-24*time.Hour), root, rootKey)
	if err != nil {
		panic(err)
	}
	valid, subject, err = tlsverify.VerifyChain(expiredLeaf, roots, checkAt)
	fmt.Printf("expired leaf:    valid=%t subject=%q err!=nil=%t\n", valid, subject, err != nil)

	// A leaf signed by a CA that is not in the trusted pool.
	otherRoot, otherKey, err := tlsverify.NewCA("Untrusted Root CA", notBefore, notAfter)
	if err != nil {
		panic(err)
	}
	untrustedLeaf, err := tlsverify.NewLeaf("rogue.example.com", notBefore, notAfter, otherRoot, otherKey)
	if err != nil {
		panic(err)
	}
	valid, subject, err = tlsverify.VerifyChain(untrustedLeaf, roots, checkAt)
	fmt.Printf("untrusted chain: valid=%t subject=%q err!=nil=%t\n", valid, subject, err != nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
trusted chain:   valid=true subject="api.example.com -> Example Root CA" err=<nil>
expired leaf:    valid=false subject="old.example.com" err!=nil=true
untrusted chain: valid=false subject="rogue.example.com" err!=nil=true
```

### Tests

Create `tlsverify_test.go`:

```go
package tlsverify

import (
	"crypto/x509"
	"errors"
	"testing"
	"time"
)

var (
	notBefore = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	notAfter  = time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	checkAt   = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
)

func TestVerifyChainValid(t *testing.T) {
	t.Parallel()
	root, rootKey, err := NewCA("Example Root CA", notBefore, notAfter)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	leaf, err := NewLeaf("api.example.com", notBefore, notAfter, root, rootKey)
	if err != nil {
		t.Fatalf("NewLeaf: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)

	valid, subject, err := VerifyChain(leaf, roots, checkAt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !valid {
		t.Fatal("valid = false, want true")
	}
	want := "api.example.com -> Example Root CA"
	if subject != want {
		t.Fatalf("subject = %q, want %q", subject, want)
	}
}

func TestVerifyChainExpiredLeafReportsSubjectAndError(t *testing.T) {
	t.Parallel()
	root, rootKey, err := NewCA("Example Root CA", notBefore, notAfter)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	// Expired well before checkAt.
	leaf, err := NewLeaf("old.example.com", notBefore, checkAt.Add(-24*time.Hour), root, rootKey)
	if err != nil {
		t.Fatalf("NewLeaf: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)

	valid, subject, err := VerifyChain(leaf, roots, checkAt)
	if valid {
		t.Fatal("valid = true, want false for an expired leaf")
	}
	if err == nil {
		t.Fatal("want a non-nil error for an expired leaf")
	}
	if subject != "old.example.com" {
		t.Fatalf("subject = %q, want the leaf's own CN even on failure", subject)
	}
}

func TestVerifyChainNotYetValidLeaf(t *testing.T) {
	t.Parallel()
	root, rootKey, err := NewCA("Example Root CA", notBefore, notAfter)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	leaf, err := NewLeaf("future.example.com", checkAt.Add(24*time.Hour), notAfter, root, rootKey)
	if err != nil {
		t.Fatalf("NewLeaf: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)

	valid, _, err := VerifyChain(leaf, roots, checkAt)
	if valid {
		t.Fatal("valid = true, want false for a not-yet-valid leaf")
	}
	if err == nil {
		t.Fatal("want a non-nil error for a not-yet-valid leaf")
	}
}

func TestVerifyChainUntrustedRoot(t *testing.T) {
	t.Parallel()
	trustedRoot, _, err := NewCA("Trusted Root CA", notBefore, notAfter)
	if err != nil {
		t.Fatalf("NewCA (trusted): %v", err)
	}
	otherRoot, otherKey, err := NewCA("Untrusted Root CA", notBefore, notAfter)
	if err != nil {
		t.Fatalf("NewCA (untrusted): %v", err)
	}
	leaf, err := NewLeaf("rogue.example.com", notBefore, notAfter, otherRoot, otherKey)
	if err != nil {
		t.Fatalf("NewLeaf: %v", err)
	}

	roots := x509.NewCertPool()
	roots.AddCert(trustedRoot) // deliberately does not include otherRoot

	valid, subject, err := VerifyChain(leaf, roots, checkAt)
	if valid {
		t.Fatal("valid = true, want false for a chain signed by an untrusted root")
	}
	if err == nil {
		t.Fatal("want a non-nil error for an untrusted root")
	}
	if subject != "rogue.example.com" {
		t.Fatalf("subject = %q, want the leaf's own CN even on failure", subject)
	}
	var unknownAuthErr x509.UnknownAuthorityError
	if !errors.As(err, &unknownAuthErr) {
		t.Fatalf("err = %v (%T), want it to wrap x509.UnknownAuthorityError", err, err)
	}
}
```

## Review

`VerifyChain` is correct when `subject` is populated on *every* path, not
just success — a monitoring system scraping failure logs needs the
certificate's name precisely when verification fails, and a design that
only sets `subject` on the happy path forces every caller to duplicate
`leaf.Subject.CommonName` at the call site. `TestVerifyChainUntrustedRoot`
is the load-bearing test: it confirms the wrapped error still carries the
concrete `x509.UnknownAuthorityError` type via `errors.As`, so a caller can
distinguish "wrong CA" from "expired" from "not yet valid" programmatically,
not just by parsing an error string.

The mistake to avoid is calling `leaf.Verify` with a zero-value
`CurrentTime` (or omitting it, which makes the standard library default to
`time.Now()` internally) instead of threading through the explicit `at`
parameter. That single change is what makes
`TestVerifyChainExpiredLeafReportsSubjectAndError` and
`TestVerifyChainNotYetValidLeaf` deterministic — without it, both tests
would depend on the real clock relative to hardcoded certificate validity
windows, and would eventually start failing (or passing for the wrong
reason) as real time moves past those hardcoded dates.

## Resources

- [crypto/x509: Certificate.Verify](https://pkg.go.dev/crypto/x509#Certificate.Verify) — the chain-building verification this exercise wraps, including the `VerifyOptions.CurrentTime` field.
- [crypto/x509: UnknownAuthorityError](https://pkg.go.dev/crypto/x509#UnknownAuthorityError) — the concrete error type this exercise's untrusted-root test asserts against with `errors.As`.
- [RFC 5280: X.509 certificate and CRL profile](https://www.rfc-editor.org/rfc/rfc5280) — the validity-period and chain-of-trust model this exercise's generated certificates follow.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [33-binary-search-sorted-list.md](33-binary-search-sorted-list.md) | Next: [35-distributed-lock-acquire-lease.md](35-distributed-lock-acquire-lease.md)
