# Exercise 20: Fail-Fast init() — Parsing a TLS Certificate and Key Pair

**Nivel: Intermedio** — validacion rapida (un test corto).

A server that starts successfully but cannot actually terminate TLS is worse
than one that refuses to start at all — it accepts connections and then fails
the handshake, one confused client at a time. This exercise parses the
server's certificate and private key at `init()`, so a missing or malformed
pair panics before the process ever opens a listening socket.

## What you'll build

```text
tlscert/                   independent module: example.com/tlscert
  go.mod                    module example.com/tlscert
  tlscert.go                 embedded cert/key PEM, loadKeyPair, Cert, init()
  cmd/
    demo/
      main.go                prints facts about the loaded certificate
  tlscert_test.go             package-loaded proof + missing/malformed table
```

Files: `tlscert.go`, `cmd/demo/main.go`, `tlscert_test.go`.
Implement: `loadKeyPair(cert, key []byte) (tls.Certificate, error)` wrapping `tls.X509KeyPair` with a distinct `ErrEmptyPair` for missing content, and a package-level `Cert` loaded from it at `init()`.
Test: the package loads without panicking; empty cert or key content is rejected as `ErrEmptyPair`; malformed PEM, garbage key bytes, and a cert/key that don't match are each rejected with a non-`ErrEmptyPair` parse error.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/tlscert/cmd/demo
cd ~/go-exercises/tlscert
go mod init example.com/tlscert
go mod edit -go=1.24
```

### Why "empty" and "malformed" deserve different errors

`tls.X509KeyPair` alone reports every kind of bad input through the same
generic parse error, whether the byte slice was empty because a file failed
to load, or non-empty but corrupted. Distinguishing the two at the call site
matters operationally: an empty cert or key almost always means a deployment
problem — a file that was never mounted, an environment variable that was
never set — while a non-empty-but-malformed pair usually means the content
itself is wrong — a truncated download, a key that doesn't match the cert it
was paired with. `loadKeyPair` checks for empty input explicitly and returns
the distinct `ErrEmptyPair` sentinel for it, before ever calling
`tls.X509KeyPair`, so the panic message at `init()` points an operator toward
the right kind of investigation immediately.

The certificate and key are embedded here as PEM constants — standing in for
`os.ReadFile(certPath)` and `os.ReadFile(keyPath)` in a real deployment — so
this module has no external files to keep in sync and `loadKeyPair` works
identically whether its input came from a file, an environment variable, or
a secrets manager: it only ever sees bytes.

Create `tlscert.go`:

```go
// tlscert.go
// Package tlscert loads and parses this service's TLS certificate and key
// pair at init, panicking before the server ever starts if either is
// missing (empty) or fails to parse, rather than discovering the problem
// on the first incoming TLS handshake.
package tlscert

import (
	"crypto/tls"
	"errors"
	"fmt"
)

// certPEM and keyPEM stand in for the contents of a deployed cert.pem and
// key.pem file, embedded as constants so this module stays self-contained.
// In production these come from os.ReadFile(certPath) and
// os.ReadFile(keyPath); loadKeyPair below works identically either way,
// since tls.X509KeyPair only ever sees bytes.
const certPEM = `-----BEGIN CERTIFICATE-----
MIIBhTCCASugAwIBAgIUVyXrwnYRW5TTdi66VVl+sXBigJswCgYIKoZIzj0EAwIw
GDEWMBQGA1UEAwwNZXhhbXBsZS5sb2NhbDAeFw0yNjA3MDUyMDI1MDJaFw0zNjA3
MDIyMDI1MDJaMBgxFjAUBgNVBAMMDWV4YW1wbGUubG9jYWwwWTATBgcqhkjOPQIB
BggqhkjOPQMBBwNCAAQsEWY62zo8qEEB2wIjhwveEVmPBUpGXZy3lbZOh4bWrBlU
1fyhZxiyMGl0wiAHWOTg3PbrldBcQtzwRJ1EQfEYo1MwUTAdBgNVHQ4EFgQUuWpB
TQab2hmi0dQl/zbv5DXq8/AwHwYDVR0jBBgwFoAUuWpBTQab2hmi0dQl/zbv5DXq
8/AwDwYDVR0TAQH/BAUwAwEB/zAKBggqhkjOPQQDAgNIADBFAiEAkZD90j/eGCfT
WWpRdrf6VqSN4Q+zueYmCONrzdnfww4CIH3uJryZgnFnkM83Ea36V/p1BQucp8xt
FISedepe3P5t
-----END CERTIFICATE-----
`

const keyPEM = `-----BEGIN PRIVATE KEY-----
MIGHAgEAMBMGByqGSM49AgEGCCqGSM49AwEHBG0wawIBAQQgkKSzRqmSGV9aYJ1Z
gGbwU37lDcm0rJcEVfV5cjR4X6KhRANCAAQsEWY62zo8qEEB2wIjhwveEVmPBUpG
XZy3lbZOh4bWrBlU1fyhZxiyMGl0wiAHWOTg3PbrldBcQtzwRJ1EQfEY
-----END PRIVATE KEY-----
`

// ErrEmptyPair is returned when either the certificate or key content is
// empty, which is treated the same as a missing file.
var ErrEmptyPair = errors.New("certificate or key content is empty")

// Cert is the parsed certificate and key this server presents on every TLS
// connection, loaded once at init.
var Cert tls.Certificate

func init() {
	c, err := loadKeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		panic(fmt.Errorf("tlscert: failed to load TLS certificate: %w", err))
	}
	Cert = c
}

// loadKeyPair parses a certificate and key pair from PEM bytes, treating
// empty input as a distinct, clearer error than what tls.X509KeyPair
// reports for it. Extracted from init so a test can pass deliberately
// missing or malformed content without crashing the test binary.
func loadKeyPair(cert, key []byte) (tls.Certificate, error) {
	if len(cert) == 0 || len(key) == 0 {
		return tls.Certificate{}, ErrEmptyPair
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("parsing certificate/key: %w", err)
	}
	return pair, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/tlscert"
)

func main() {
	fmt.Println("leaf certificates loaded:", len(tlscert.Cert.Certificate))
	fmt.Println("private key present:", tlscert.Cert.PrivateKey != nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
leaf certificates loaded: 1
private key present: true
```

### Tests

Create `tlscert_test.go`:

```go
// tlscert_test.go
package tlscert

import (
	"errors"
	"testing"
)

// TestPackageLoaded proves init did not panic: if the shipped cert/key were
// missing or malformed, this test binary would never reach a test body.
func TestPackageLoaded(t *testing.T) {
	if len(Cert.Certificate) == 0 {
		t.Fatal("Cert has no leaf certificate bytes")
	}
	if Cert.PrivateKey == nil {
		t.Fatal("Cert has no private key")
	}
}

func TestLoadKeyPairValid(t *testing.T) {
	c, err := loadKeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		t.Fatalf("loadKeyPair error = %v", err)
	}
	if len(c.Certificate) == 0 {
		t.Fatal("loaded certificate has no leaf bytes")
	}
}

func TestLoadKeyPairRejectsMissing(t *testing.T) {
	tests := []struct {
		name string
		cert []byte
		key  []byte
	}{
		{"empty cert", nil, []byte(keyPEM)},
		{"empty key", []byte(certPEM), nil},
		{"both empty", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadKeyPair(tt.cert, tt.key)
			if !errors.Is(err, ErrEmptyPair) {
				t.Fatalf("err = %v, want ErrEmptyPair", err)
			}
		})
	}
}

func TestLoadKeyPairRejectsMalformed(t *testing.T) {
	tests := []struct {
		name string
		cert string
		key  string
	}{
		{"garbage cert", "not a certificate", keyPEM},
		{"garbage key", certPEM, "not a key"},
		{"mismatched key", certPEM, otherKeyPEM},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadKeyPair([]byte(tt.cert), []byte(tt.key))
			if err == nil {
				t.Fatal("expected a parse error, got nil")
			}
			if errors.Is(err, ErrEmptyPair) {
				t.Fatal("malformed content should not be reported as empty")
			}
		})
	}
}

// otherKeyPEM is a syntactically valid PEM key block that does not match
// certPEM's public key, so tls.X509KeyPair must reject the pairing.
const otherKeyPEM = `-----BEGIN PRIVATE KEY-----
MIGHAgEAMBMGByqGSM49AgEGCCqGSM49AwEHBG0wawIBAQQg4z2Y1s1c4z2Y1s1c
4z2Y1s1c4z2Y1s1c4z2Y1s1coAoGCCqGSM49AwEHoUQDQgAEXwZ2rC3z6L2c0mP0
c4z2Y1s1c4z2Y1s1c4z2Y1s1c4z2Y1s1c4z2Y1s1c4z2Y1s1c4z2Y1s1c4z2Y1w==
-----END PRIVATE KEY-----
`
```

## Review

`TestLoadKeyPairRejectsMissing` and `TestLoadKeyPairRejectsMalformed` split
the failure space exactly along the line the code draws: empty input is
always `ErrEmptyPair`, and any non-empty-but-bad input is always some other
error, checked here by asserting it is specifically *not* `ErrEmptyPair`. The
`mismatched key` case is the subtlest one — both PEM blocks are individually
well-formed, but `tls.X509KeyPair` cross-checks that the private key
actually corresponds to the certificate's public key, and rejects the pair
if it does not, catching a class of misconfiguration a syntax-only check
would miss entirely.

The mistake to avoid is calling `tls.X509KeyPair` from deep inside request
handling and discovering a bad pair only when the first client connects. Load
and validate once, at `init()`, and let the panic happen where an operator
watching the process start will actually see it.

## Resources

- [tls.X509KeyPair](https://pkg.go.dev/crypto/tls#X509KeyPair) — parses a certificate/key pair from PEM-encoded data, cross-checking that they match.
- [tls.Certificate](https://pkg.go.dev/crypto/tls#Certificate) — the parsed structure a `tls.Config`'s `Certificates` field expects.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [19-schema-migration-ordering-dag-validator.md](19-schema-migration-ordering-dag-validator.md) | Next: [21-oauth2-provider-explicit-constructor.md](21-oauth2-provider-explicit-constructor.md)
