# Exercise 3: Keyless-sign a release artifact via Fulcio, Rekor and a TSA

This exercise mirrors a CI release step: generate an ephemeral keypair, exchange
an OIDC identity token for a short-lived Fulcio certificate, sign the artifact,
obtain a Rekor transparency-log entry and an RFC 3161 TSA timestamp, and emit a
self-contained `.sigstore.json` bundle that round-trips against the verifier from
exercise 1.

The interesting engineering here is splitting the surface. Everything that does
*not* touch the network — building the ephemeral keypair, choosing the content
type, assembling `sign.BundleOptions` — is ordinary, deterministic, unit-testable
code. Only the actual `sign.Bundle` call reaches Fulcio, Rekor, and a TSA, so it
lives behind a `//go:build online` tag and is exercised by a smoke test that skips
without an OIDC token. This module is self-contained: its own `go mod init`, its
own demo, its own tests.

## What you'll build

```text
keyless/                     independent module: example.com/keyless
  go.mod                     requires sigstore/sigstore-go + protobuf-specs + protobuf
  keyless.go                 SigningConfig, sentinels; BuildContent, AssembleOptions
  sign_online.go            //go:build online: SignRelease (Fulcio/Rekor/TSA + write bundle)
  cmd/
    demo/
      main.go                offline: ephemeral keypair properties + option assembly
  keyless_test.go            offline unit tests: keypair, content PAE, assembly, Example
  sign_online_test.go        //go:build online: end-to-end sign then verify round trip
```

- Files: `keyless.go`, `sign_online.go`, `cmd/demo/main.go`, `keyless_test.go`, `sign_online_test.go`.
- Implement: `BuildContent(data, dsse)` returning `sign.PlainData` or `sign.DSSEData`; `AssembleOptions(cfg, idToken, trustedRoot)` wiring Fulcio + Rekor + TSA and threading the ID token; `SignRelease` (online) producing and writing the bundle.
- Test (offline): the ephemeral keypair reports ECDSA P-256 / SHA-256 and a non-empty PEM; `PlainData`/`DSSEData` pre-auth encodings are stable; `AssembleOptions` wires all three services and the token.
- Test (online): behind `//go:build online`, sign a blob against the public-good instances with an ambient OIDC token and verify the produced bundle end to end.
- Verify: `go test -count=1 ./...` (offline); `go test -tags online ./...` with an OIDC token for the round trip.

Set up the module:

```bash
mkdir -p ~/go-exercises/keyless/cmd/demo
cd ~/go-exercises/keyless
go mod init example.com/keyless
go mod edit -go=1.25
go get github.com/sigstore/sigstore-go@v1.2.1
```

### Why the network path is tagged out of the default gate

`sign.Bundle` calls Fulcio to mint a certificate (which requires a real OIDC
token), Rekor to log the entry, and a TSA to countersign — three network round
trips against public-good infrastructure that has rate limits and no offline mode.
That cannot be part of a hermetic `go test ./...` run, so it is compiled only with
`-tags online` and the online test *skips* unless an OIDC token is present in the
environment. What remains in the default path is everything that determines the
bundle's *shape*: the keypair, the content type, and the option assembly. Those
are pure and are where the real correctness bugs live, so those are what the
offline tests pin down.

### Content type decides the Rekor entry and the verification path

`BuildContent` returns a `sign.Content`. `sign.PlainData` signs raw bytes and
yields a `hashedrekord` entry, verified later by binding the artifact digest.
`sign.DSSEData` wraps an in-toto statement with the payload type
`application/vnd.in-toto+json`, yields a `dsse` entry, and on verification produces
a parsed `Statement`. This is the same fork as exercise 1's verifier: sign a plain
blob and the verifier's `result.Statement` is nil; sign a DSSE statement and it is
populated. `DSSEData.PreAuthEncoding` produces the DSSE v1 pre-auth encoding
(`DSSEv1 <len> <payloadType> <len> <body>`), which is what actually gets signed;
`PlainData.PreAuthEncoding` returns the raw bytes unchanged.

### Assembling the options without touching the network

`sign.NewFulcio`, `sign.NewRekor`, and `sign.NewTimestampAuthority` are plain
constructors — they build option-holding structs and make no network call, which
is why `AssembleOptions` is unit-testable. `sign.Bundle` is where the calls
actually happen. Fulcio is wired as the `CertificateProvider`, and the OIDC token
is threaded through `CertificateProviderOptions.IDToken` — Fulcio *requires* it,
and this is the single most common wiring mistake. Rekor instances go in
`TransparencyLogs`, timestamp authorities in `TimestampAuthorities`. In a real
pipeline these service URLs come from the Sigstore `SigningConfig` fetched over
TUF; here they are passed in as configuration so the assembly stays testable.

Create `keyless.go`:

```go
package keyless

import (
	"errors"
	"time"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/sign"
)

// InTotoPayloadType is the DSSE payload type for an in-toto statement (SLSA
// provenance, SBOM attestation, and so on).
const InTotoPayloadType = "application/vnd.in-toto+json"

// Sentinel errors for classifying signing failures with errors.Is.
var (
	ErrKeypair     = errors.New("keyless: generate ephemeral keypair")
	ErrTrustedRoot = errors.New("keyless: fetch trusted root")
	ErrSign        = errors.New("keyless: sign and bundle")
)

// SigningConfig holds the service endpoints. In production these are sourced from
// the Sigstore SigningConfig over TUF; here they are explicit so assembly is
// testable. An empty field disables that service.
type SigningConfig struct {
	FulcioURL string // e.g. https://fulcio.sigstore.dev
	RekorURL  string // e.g. https://rekor.sigstore.dev
	TSAURL    string // e.g. https://timestamp.sigstore.dev/api/v1/timestamp
}

// BuildContent picks the content type: a DSSE-wrapped in-toto statement when dsse
// is true, otherwise a plain blob. The choice drives the Rekor entry type and the
// verification path.
func BuildContent(data []byte, dsse bool) sign.Content {
	if dsse {
		return &sign.DSSEData{Data: data, PayloadType: InTotoPayloadType}
	}
	return &sign.PlainData{Data: data}
}

// AssembleOptions wires Fulcio, Rekor, and the TSA into BundleOptions and threads
// the OIDC token to the certificate provider. It performs no network I/O.
func AssembleOptions(cfg SigningConfig, idToken string, trustedRoot root.TrustedMaterial) sign.BundleOptions {
	opts := sign.BundleOptions{TrustedRoot: trustedRoot}

	if cfg.FulcioURL != "" {
		opts.CertificateProvider = sign.NewFulcio(&sign.FulcioOptions{
			BaseURL: cfg.FulcioURL,
			Timeout: 30 * time.Second,
			Retries: 1,
		})
		opts.CertificateProviderOptions = &sign.CertificateProviderOptions{IDToken: idToken}
	}
	if cfg.RekorURL != "" {
		opts.TransparencyLogs = append(opts.TransparencyLogs, sign.NewRekor(&sign.RekorOptions{
			BaseURL: cfg.RekorURL,
			Timeout: 90 * time.Second,
			Retries: 1,
		}))
	}
	if cfg.TSAURL != "" {
		opts.TimestampAuthorities = append(opts.TimestampAuthorities, sign.NewTimestampAuthority(&sign.TimestampAuthorityOptions{
			URL:     cfg.TSAURL,
			Timeout: 30 * time.Second,
			Retries: 1,
		}))
	}
	return opts
}
```

### The online signing path

`SignRelease` is the network step: an ephemeral keypair, a fetched trusted root
(to verify the bundle Sigstore returns), the assembled options with the caller's
context, the `sign.Bundle` call, and `protojson.Marshal` to serialize the result
to the `.sigstore.json` on-disk form that exercise 1 reads back.

Create `sign_online.go`:

```go
//go:build online

package keyless

import (
	"context"
	"fmt"
	"os"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/sign"
	"google.golang.org/protobuf/encoding/protojson"
)

// SignRelease keyless-signs artifact and writes a self-contained .sigstore.json
// bundle to outPath. When dsse is true, artifact must be an in-toto statement.
func SignRelease(ctx context.Context, artifact []byte, dsse bool, cfg SigningConfig, idToken, outPath string) error {
	keypair, err := sign.NewEphemeralKeypair(nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrKeypair, err)
	}

	trustedRoot, err := root.FetchTrustedRoot()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrTrustedRoot, err)
	}

	opts := AssembleOptions(cfg, idToken, trustedRoot)
	opts.Context = ctx

	b, err := sign.Bundle(BuildContent(artifact, dsse), keypair, opts)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSign, err)
	}

	data, err := protojson.Marshal(b)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSign, err)
	}
	if err := os.WriteFile(outPath, data, 0o600); err != nil {
		return fmt.Errorf("write bundle: %w", err)
	}
	return nil
}
```

### The runnable demo

The demo stays offline: it generates an ephemeral keypair and prints its
algorithm properties (proving the default is ECDSA P-256 with SHA-256), then
assembles the options against the public-good endpoints and reports what got
wired — without contacting any of them.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"strings"

	"github.com/sigstore/sigstore-go/pkg/sign"

	"example.com/keyless"
)

func main() {
	kp, err := sign.NewEphemeralKeypair(nil)
	if err != nil {
		log.Fatal(err)
	}
	pem, err := kp.GetPublicKeyPem()
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("key algorithm: %s\n", kp.GetKeyAlgorithm())
	fmt.Printf("signing algorithm: %s\n", kp.GetSigningAlgorithm())
	fmt.Printf("hash algorithm: %s\n", kp.GetHashAlgorithm())
	fmt.Printf("public key: %s\n", pemStatus(pem))

	content := keyless.BuildContent([]byte(`{"_type":"https://in-toto.io/Statement/v1"}`), true)
	if _, ok := content.(*sign.DSSEData); ok {
		fmt.Printf("content: DSSE %s\n", keyless.InTotoPayloadType)
	}

	opts := keyless.AssembleOptions(keyless.SigningConfig{
		FulcioURL: "https://fulcio.sigstore.dev",
		RekorURL:  "https://rekor.sigstore.dev",
		TSAURL:    "https://timestamp.sigstore.dev/api/v1/timestamp",
	}, "ambient-oidc-token", nil)

	fmt.Printf("fulcio configured: %v\n", opts.CertificateProvider != nil)
	fmt.Printf("rekor configured: %v\n", len(opts.TransparencyLogs) > 0)
	fmt.Printf("tsa configured: %v\n", len(opts.TimestampAuthorities) > 0)
	fmt.Printf("id token threaded: %v\n", opts.CertificateProviderOptions != nil && opts.CertificateProviderOptions.IDToken != "")
}

func pemStatus(pem string) string {
	if strings.HasPrefix(pem, "-----BEGIN PUBLIC KEY-----") {
		return "PEM present"
	}
	return "missing"
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
key algorithm: ECDSA
signing algorithm: PKIX_ECDSA_P256_SHA_256
hash algorithm: SHA2_256
public key: PEM present
content: DSSE application/vnd.in-toto+json
fulcio configured: true
rekor configured: true
tsa configured: true
id token threaded: true
```

### Tests

The offline tests pin the deterministic surface. The keypair test asserts the
defaults (ECDSA, `PKIX_ECDSA_P256_SHA_256`, `SHA2_256`) through the `Keypair`
interface and that the PEM is well-formed. The content test asserts `PlainData`
returns its bytes unchanged and `DSSEData` produces a `DSSEv1` pre-auth encoding
carrying the payload type. The assembly test asserts all three services and the
token are wired. The online round-trip test signs a blob and verifies the produced
bundle against the pinned identity supplied via the environment; it skips unless a
token and expected identity are set.

Create `keyless_test.go`:

```go
package keyless

import (
	"fmt"
	"strings"
	"testing"

	protocommon "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	"github.com/sigstore/sigstore-go/pkg/sign"
)

func TestEphemeralKeypairDefaults(t *testing.T) {
	t.Parallel()
	kp, err := sign.NewEphemeralKeypair(nil)
	if err != nil {
		t.Fatalf("NewEphemeralKeypair: %v", err)
	}
	if got := kp.GetKeyAlgorithm(); got != "ECDSA" {
		t.Fatalf("GetKeyAlgorithm = %q, want ECDSA", got)
	}
	if got := kp.GetSigningAlgorithm(); got != protocommon.PublicKeyDetails_PKIX_ECDSA_P256_SHA_256 {
		t.Fatalf("GetSigningAlgorithm = %v, want PKIX_ECDSA_P256_SHA_256", got)
	}
	if got := kp.GetHashAlgorithm(); got != protocommon.HashAlgorithm_SHA2_256 {
		t.Fatalf("GetHashAlgorithm = %v, want SHA2_256", got)
	}
	pem, err := kp.GetPublicKeyPem()
	if err != nil {
		t.Fatalf("GetPublicKeyPem: %v", err)
	}
	if !strings.HasPrefix(pem, "-----BEGIN PUBLIC KEY-----") {
		t.Fatalf("public key PEM = %q, want a PKIX PEM block", pem)
	}
}

func TestBuildContent(t *testing.T) {
	t.Parallel()

	plain := BuildContent([]byte("release-binary"), false)
	if _, ok := plain.(*sign.PlainData); !ok {
		t.Fatalf("BuildContent(dsse=false) = %T, want *sign.PlainData", plain)
	}
	if got := string(plain.PreAuthEncoding()); got != "release-binary" {
		t.Fatalf("PlainData PAE = %q, want the raw bytes", got)
	}

	dsse := BuildContent([]byte(`{"_type":"x"}`), true)
	if _, ok := dsse.(*sign.DSSEData); !ok {
		t.Fatalf("BuildContent(dsse=true) = %T, want *sign.DSSEData", dsse)
	}
	pae := string(dsse.PreAuthEncoding())
	if !strings.HasPrefix(pae, "DSSEv1 ") {
		t.Fatalf("DSSE PAE = %q, want a DSSEv1 pre-auth encoding", pae)
	}
	if !strings.Contains(pae, InTotoPayloadType) {
		t.Fatalf("DSSE PAE = %q, want it to carry the payload type", pae)
	}
}

func TestAssembleOptions(t *testing.T) {
	t.Parallel()
	opts := AssembleOptions(SigningConfig{
		FulcioURL: "https://fulcio.sigstore.dev",
		RekorURL:  "https://rekor.sigstore.dev",
		TSAURL:    "https://timestamp.sigstore.dev/api/v1/timestamp",
	}, "the-oidc-token", nil)

	if opts.CertificateProvider == nil {
		t.Fatal("Fulcio certificate provider not wired")
	}
	if opts.CertificateProviderOptions == nil || opts.CertificateProviderOptions.IDToken != "the-oidc-token" {
		t.Fatalf("ID token not threaded to the certificate provider")
	}
	if len(opts.TransparencyLogs) != 1 {
		t.Fatalf("TransparencyLogs = %d, want 1 (Rekor)", len(opts.TransparencyLogs))
	}
	if len(opts.TimestampAuthorities) != 1 {
		t.Fatalf("TimestampAuthorities = %d, want 1 (TSA)", len(opts.TimestampAuthorities))
	}
}

func Example() {
	content := BuildContent([]byte("v1.4.2-linux-amd64"), false)
	fmt.Printf("%T\n", content)
	fmt.Println(string(content.PreAuthEncoding()))
	// Output:
	// *sign.PlainData
	// v1.4.2-linux-amd64
}
```

The online round-trip test proves the bundle this module produces verifies with
the same threshold-and-identity policy exercise 1 uses. It needs an ambient OIDC
token (the GitHub Actions token in CI, or an interactive token locally) and the
identity that token maps to, so it skips unless `SIGSTORE_ID_TOKEN`,
`EXPECT_ISSUER`, and `EXPECT_SAN` are set.

Create `sign_online_test.go`:

```go
//go:build online

package keyless

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

func TestSignReleaseRoundTrip(t *testing.T) {
	token := os.Getenv("SIGSTORE_ID_TOKEN")
	issuer := os.Getenv("EXPECT_ISSUER")
	san := os.Getenv("EXPECT_SAN")
	if token == "" || issuer == "" || san == "" {
		t.Skip("set SIGSTORE_ID_TOKEN, EXPECT_ISSUER, EXPECT_SAN to run the online round trip")
	}

	artifact := []byte("integration-release-artifact")
	out := filepath.Join(t.TempDir(), "artifact.sigstore.json")

	cfg := SigningConfig{
		FulcioURL: "https://fulcio.sigstore.dev",
		RekorURL:  "https://rekor.sigstore.dev",
		TSAURL:    "https://timestamp.sigstore.dev/api/v1/timestamp",
	}
	if err := SignRelease(t.Context(), artifact, false, cfg, token, out); err != nil {
		t.Fatalf("SignRelease: %v", err)
	}

	trustedRoot, err := root.FetchTrustedRoot()
	if err != nil {
		t.Fatalf("FetchTrustedRoot: %v", err)
	}
	v, err := verify.NewVerifier(trustedRoot,
		verify.WithSignedCertificateTimestamps(1),
		verify.WithTransparencyLog(1),
		verify.WithObserverTimestamps(1),
	)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	b, err := bundle.LoadJSONFromPath(out)
	if err != nil {
		t.Fatalf("LoadJSONFromPath: %v", err)
	}
	certID, err := verify.NewShortCertificateIdentity(issuer, "", san, "")
	if err != nil {
		t.Fatalf("NewShortCertificateIdentity: %v", err)
	}
	digest := sha256.Sum256(artifact)
	if _, err := v.Verify(b, verify.NewPolicy(
		verify.WithArtifactDigest("sha256", digest[:]),
		verify.WithCertificateIdentity(certID),
	)); err != nil {
		t.Fatalf("round-trip verify: %v", err)
	}
}
```

## Review

The signer is correct when the offline surface holds and the online round trip
closes. Offline: the ephemeral keypair defaults to ECDSA P-256 / SHA-256, so the
Fulcio certificate binds that public key; `BuildContent` returns the content type
you asked for and its pre-auth encoding is the bytes actually signed; and
`AssembleOptions` wires Fulcio, Rekor, and the TSA and threads the OIDC token —
the token is what people forget, and Fulcio rejects the request without it.
Online: the bundle this module writes verifies under the same SCT, transparency-log
and observer-timestamp thresholds, and the same pinned identity, that exercise 1
enforces — which is the whole point, since a bundle that cannot be verified by the
gate is not worth producing.

The mistakes to avoid are the wiring ones. Forgetting `CertificateProviderOptions.IDToken`
makes Fulcio refuse to issue a certificate. Signing a plain blob when downstream
policy needs a statement (or the reverse) leaves the verifier's `Statement` empty
or its digest binding unusable — choose `BuildContent(_, true)` only when the
payload is an in-toto statement. Shelling out to the `cosign` CLI from a service to
do this on a request path is the anti-pattern; embed the library as here. Because
this module imports `sigstore-go`, it cannot build offline without that dependency
in the module cache, and the `online` tag keeps the network path out of the
default `go test ./...`; run `go test -tags online` with an OIDC token for the
end-to-end proof.

## Resources

- [`sigstore-go/pkg/sign`](https://pkg.go.dev/github.com/sigstore/sigstore-go/pkg/sign) — `NewEphemeralKeypair`, `PlainData`, `DSSEData`, `NewFulcio`, `NewRekor`, `NewTimestampAuthority`, `Bundle`, and `BundleOptions`.
- [sigstore-go signing guide](https://github.com/sigstore/sigstore-go/blob/main/docs/signing.md) — the `Content`, `Keypair`, `CertificateProvider`, and `Transparency` abstractions.
- [`protojson`](https://pkg.go.dev/google.golang.org/protobuf/encoding/protojson) — serializing the protobuf bundle to the `.sigstore.json` on-disk form.
- [cosign sign-blob](https://docs.sigstore.dev/cosign/signing/signing_with_blobs/) — the operator-facing CLI that produces the same bundle format.

---

Back to [02-identity-policy-as-code.md](02-identity-policy-as-code.md) | Next: [../../50-messaging-and-event-driven/01-kafka-with-franz-go/00-concepts.md](../../50-messaging-and-event-driven/01-kafka-with-franz-go/00-concepts.md)
