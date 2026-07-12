# Exercise 9: Lazy TLS materialization: parse client cert and CA pool exactly once

An outbound mTLS client needs a `*tls.Config` built from PEM bytes: parse the
cert/key pair, build the root CA pool. That work is expensive, fallible, and —
critically — must produce one immutable config shared by every caller, because
mutating a `tls.Config` after other goroutines hold it is a data race. This
exercise materializes the config lazily under a `sync.Once`: build fully inside
the closure, publish once, hand every caller the identical pointer.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
lazy-tls-config/              module: example.com/lazy-tls-config
  go.mod
  tlsconfig.go                ErrTLSConfig; type Provider; NewProvider, Get, Builds
  cmd/
    demo/
      main.go                 runnable demo: in-memory self-signed cert, one build
  tlsconfig_test.go           in-memory cert helper (no fixtures), concurrent
                              pointer identity, cached parse error, Example
```

- Files: `tlsconfig.go`, `cmd/demo/main.go`, `tlsconfig_test.go`.
- Implement: a `Provider` holding raw PEM bytes; `Get()` runs the build closure once — `tls.X509KeyPair` for the client cert, `x509.NewCertPool` + `AppendCertsFromPEM` for the roots — capturing either the finished `*tls.Config` or an `ErrTLSConfig`-wrapped error; `Builds()` counts constructions.
- Test: a helper generates a self-signed ed25519 certificate entirely in memory (`x509.CreateCertificate` + `pem.EncodeToMemory`, no fixture files); 100 concurrent `Get` calls observe one build and pointer-identical configs; corrupt PEM yields the same cached `ErrTLSConfig` on every call.
- Verify: `go test -count=1 -race ./...`

### Publish-then-freeze: why the config is built fully inside the closure

The `Once` here buys two things beyond deduplicating an expensive parse. First,
the error channel: PEM that fails to parse is a *configuration* error — the
operator mounted the wrong secret — and the right behavior is a stable, cached,
sentinel-wrapped error surfaced to every caller through `errors.Is`, not a
panic and not a silent nil config that fails later inside the TLS handshake
with a much more confusing message.

Second, and easier to get wrong: immutability by construction. A `*tls.Config`
is read by every connection that uses it, potentially from many goroutines. The
happens-before edge of `Once.Do` makes the *pointer read* safe for every caller
— everything written inside the closure is visible to everyone who returns from
`Do`. But that edge covers only writes that happen *before* the closure
returns. If any code mutates the shared config after publication — appending
another root to the pool, toggling `InsecureSkipVerify` for a debug session —
that write races with every reader, and no `Once` protects it. So the
discipline is: build the config *completely* inside the closure (keypair
parsed, pool populated, `MinVersion` set), publish it by returning, and treat
it as frozen from then on. If runtime rotation is a requirement, that is a
different primitive — an `atomic.Pointer[tls.Config]` that swaps in a *new*
fully-built config — not post-hoc mutation of the published one.

The build itself uses two verifiable stdlib calls. `tls.X509KeyPair(certPEM,
keyPEM)` parses a PEM-encoded certificate/key pair into a `tls.Certificate`,
checking that the private key matches the leaf certificate.
`x509.NewCertPool()` plus `pool.AppendCertsFromPEM(caPEM)` builds the root set;
`AppendCertsFromPEM` returns `false` if it found no parseable certificate,
which must be treated as a hard error — an empty root pool does not fail here,
it fails later as `x509: certificate signed by unknown authority` on every
dial.

Create `tlsconfig.go`:

```go
// Package tlsconfig materializes an outbound client's *tls.Config from PEM
// bytes exactly once: built fully inside a sync.Once closure, then published
// frozen, with parse errors cached and sentinel-wrapped.
package tlsconfig

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// ErrTLSConfig wraps any failure to materialize the TLS configuration. It is
// a configuration error: stable, cached, and the same for every caller.
var ErrTLSConfig = errors.New("tlsconfig: cannot build TLS configuration")

// Provider lazily builds one immutable *tls.Config from raw PEM bytes.
type Provider struct {
	certPEM []byte
	keyPEM  []byte
	caPEM   []byte

	once   sync.Once
	cfg    *tls.Config
	err    error
	builds atomic.Int64
}

// NewProvider returns a Provider over the given PEM-encoded client
// certificate, private key, and CA bundle. Nothing is parsed until Get.
func NewProvider(certPEM, keyPEM, caPEM []byte) *Provider {
	return &Provider{certPEM: certPEM, keyPEM: keyPEM, caPEM: caPEM}
}

// Get returns the shared TLS config, building it on first use. Every caller
// receives the identical pointer (or the identical cached error); the config
// is fully constructed before publication and must not be mutated afterward.
func (p *Provider) Get() (*tls.Config, error) {
	p.once.Do(func() {
		p.builds.Add(1)

		cert, err := tls.X509KeyPair(p.certPEM, p.keyPEM)
		if err != nil {
			p.err = fmt.Errorf("%w: parse client keypair: %w", ErrTLSConfig, err)
			return
		}

		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(p.caPEM) {
			p.err = fmt.Errorf("%w: no CA certificates found in bundle", ErrTLSConfig)
			return
		}

		p.cfg = &tls.Config{
			Certificates: []tls.Certificate{cert},
			RootCAs:      pool,
			MinVersion:   tls.VersionTLS12,
		}
	})
	return p.cfg, p.err
}

// Builds reports how many times the build closure ran; it must be 0 or 1.
func (p *Provider) Builds() int64 { return p.builds.Load() }
```

### The runnable demo

The demo generates a self-signed ed25519 certificate entirely in memory — the
same technique the tests use, so nothing touches disk — then shows one build
shared across calls, and a corrupt-PEM provider caching its configuration
error.

Create `cmd/demo/main.go`:

```go
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"math/big"
	"time"

	tlsconfig "example.com/lazy-tls-config"
)

func selfSignedPEM() (certPEM, keyPEM []byte, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "demo-client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

func main() {
	certPEM, keyPEM, err := selfSignedPEM()
	if err != nil {
		log.Fatal(err)
	}

	p := tlsconfig.NewProvider(certPEM, keyPEM, certPEM)
	cfg1, err := p.Get()
	if err != nil {
		log.Fatal(err)
	}
	cfg2, _ := p.Get()

	fmt.Println("build calls:", p.Builds())
	fmt.Println("same pointer:", cfg1 == cfg2)
	fmt.Println("client certificates:", len(cfg1.Certificates))
	fmt.Println("root CAs populated:", cfg1.RootCAs != nil)

	bad := tlsconfig.NewProvider([]byte("not pem"), []byte("not pem"), []byte("not pem"))
	_, err1 := bad.Get()
	_, err2 := bad.Get()
	fmt.Println("bad PEM is config error:", errors.Is(err1, tlsconfig.ErrTLSConfig))
	fmt.Println("bad PEM error cached:", err1.Error() == err2.Error())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
build calls: 1
same pointer: true
client certificates: 1
root CAs populated: true
bad PEM is config error: true
bad PEM error cached: true
```

### Tests

The test helper mints a fresh in-memory certificate per test, so tests are
hermetic and parallel-safe with no fixtures to rot on disk.
`TestConcurrentGetBuildsOnce` fans 100 goroutines at `Get` and asserts one
build and pointer-identical configs. `TestCorruptPEM` covers both corrupt
shapes (bad keypair, empty CA bundle) table-driven, asserting the sentinel and
that the failure is cached with exactly one build. `TestConfigShape` pins what
the closure must have finished before publishing.

Create `tlsconfig_test.go`:

```go
package tlsconfig

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"testing"
	"time"
)

// selfSignedPEM generates a self-signed ed25519 certificate and PKCS #8 key,
// both PEM-encoded, entirely in memory.
func selfSignedPEM(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

func TestConcurrentGetBuildsOnce(t *testing.T) {
	t.Parallel()

	certPEM, keyPEM := selfSignedPEM(t)
	p := NewProvider(certPEM, keyPEM, certPEM)

	const goroutines = 100
	cfgs := make(chan any, goroutines) // *tls.Config or error
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			cfg, err := p.Get()
			if err != nil {
				cfgs <- err
				return
			}
			cfgs <- cfg
		}()
	}
	wg.Wait()
	close(cfgs)

	first, _ := p.Get()
	for got := range cfgs {
		if got != any(first) {
			t.Fatalf("Get returned %v, want the identical *tls.Config %p", got, first)
		}
	}
	if got := p.Builds(); got != 1 {
		t.Fatalf("Builds() = %d, want exactly 1", got)
	}
}

func TestCorruptPEM(t *testing.T) {
	t.Parallel()

	certPEM, keyPEM := selfSignedPEM(t)

	tests := []struct {
		name          string
		cert, key, ca []byte
	}{
		{name: "garbage keypair", cert: []byte("not pem"), key: []byte("not pem"), ca: certPEM},
		{name: "empty CA bundle", cert: certPEM, key: keyPEM, ca: []byte("-- no certs here --")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p := NewProvider(tt.cert, tt.key, tt.ca)
			cfg, err1 := p.Get()
			if cfg != nil {
				t.Fatalf("Get returned a config alongside a failure")
			}
			if !errors.Is(err1, ErrTLSConfig) {
				t.Fatalf("err = %v, want ErrTLSConfig", err1)
			}
			_, err2 := p.Get()
			if err1.Error() != err2.Error() {
				t.Fatalf("error not cached: %v vs %v", err1, err2)
			}
			if got := p.Builds(); got != 1 {
				t.Fatalf("Builds() = %d, want 1 (failure cached, no rebuild)", got)
			}
		})
	}
}

func TestConfigShape(t *testing.T) {
	t.Parallel()

	certPEM, keyPEM := selfSignedPEM(t)
	p := NewProvider(certPEM, keyPEM, certPEM)

	cfg, err := p.Get()
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(cfg.Certificates) != 1 {
		t.Errorf("Certificates = %d, want 1", len(cfg.Certificates))
	}
	if cfg.RootCAs == nil {
		t.Error("RootCAs is nil; AppendCertsFromPEM result was not checked")
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %#x, want TLS 1.2", cfg.MinVersion)
	}
}

func Example() {
	p := NewProvider([]byte("not pem"), []byte("not pem"), []byte("not pem"))
	_, err := p.Get()
	fmt.Println(errors.Is(err, ErrTLSConfig))
	// Output: true
}
```

## Review

The provider is correct when every caller shares one fully-built, never-mutated
config: `Builds() == 1` under 100-way concurrency, pointer identity across
every `Get`, and both corrupt-input shapes cached as the same
`ErrTLSConfig`-wrapped error. The subtle review points: the
`AppendCertsFromPEM` boolean must be checked (an empty pool is a deferred
outage, not a build error, if you skip it); the config must be complete before
the closure returns, because the `Do` happens-before edge covers only
pre-publication writes; and rotation must never be implemented by mutating this
config — swap a freshly built one through an `atomic.Pointer` instead. The
in-memory certificate helper is worth stealing for any TLS test: hermetic,
parallel-safe, no fixture files to expire. Run `go test -count=1 -race`.

## Resources

- [tls.X509KeyPair — pkg.go.dev](https://pkg.go.dev/crypto/tls#X509KeyPair)
- [x509.CertPool.AppendCertsFromPEM — pkg.go.dev](https://pkg.go.dev/crypto/x509#CertPool.AppendCertsFromPEM)
- [x509.CreateCertificate — pkg.go.dev](https://pkg.go.dev/crypto/x509#CreateCertificate)
- [sync.Once — pkg.go.dev](https://pkg.go.dev/sync#Once)

---

Prev: [08-per-key-migration-once.md](08-per-key-migration-once.md) | Back to [00-concepts.md](00-concepts.md) | Next: [10-context-aware-once-getter.md](10-context-aware-once-getter.md)
