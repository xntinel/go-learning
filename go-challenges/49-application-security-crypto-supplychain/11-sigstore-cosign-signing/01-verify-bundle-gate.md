# Exercise 1: An offline verification gate for Sigstore bundles

This is the artifact-trust boundary of a deploy pipeline expressed as code: a gate
that loads a Sigstore bundle, verifies it against a *pinned* trust root with no
network access, and enforces a policy that binds both the artifact digest and the
signer identity before returning a structured result. It is the shape an
admission controller or a release check would actually ship.

Because keyless bundles carry a fixed Rekor integrated time and SCT, and the trust
root is loaded from disk, verification of the vendored fixture is fully
reproducible offline. This module is self-contained: its own `go mod init`, its
own demo, its own tests.

## What you'll build

```text
bundleverify/                 independent module: example.com/bundleverify
  go.mod                      requires sigstore/sigstore-go + in-toto/attestation
  bundleverify.go             Gate, Policy, Result, sentinels; NewGate, VerifyBundle
  testdata/
    bundle.json               vendored sigstore-go example bundle (DSSE provenance)
    trusted_root.json         vendored pinned trust root (offline verification)
  cmd/
    demo/
      main.go                 runnable gate against the vendored fixture
  bundleverify_test.go        table tests (identity + digest), Example, threshold test
```

- Files: `bundleverify.go`, `cmd/demo/main.go`, `bundleverify_test.go`, plus the two vendored `testdata` fixtures.
- Implement: `NewGate(trustedRootPath)` building a `verify.Verifier` that requires an SCT, a Rekor inclusion proof, and an observer timestamp; `VerifyBundle(path, Policy)` binding the artifact digest AND the signer identity, returning a typed `Result`.
- Test: correct issuer + anchored SAN regex + correct sha512 digest succeeds; a wrong SAN regex, a wrong issuer, and a mutated digest each fail with the wrapped `ErrVerify`; a raised observer-timestamp threshold is rejected.
- Verify: `go test -count=1 ./...` (the fixtures make it deterministic and offline).

Set up the module. The `sigstore-go` library and its protobuf/attestation
dependencies are external modules, so fetch them, then vendor the two fixtures:

```bash
mkdir -p ~/go-exercises/bundleverify/cmd/demo ~/go-exercises/bundleverify/testdata
cd ~/go-exercises/bundleverify
go mod init example.com/bundleverify
go mod edit -go=1.25
go get github.com/sigstore/sigstore-go@v1.2.1

# Vendor the reproducible fixtures from the sigstore-go examples.
base=https://raw.githubusercontent.com/sigstore/sigstore-go/main/examples
curl -sSL "$base/bundle-provenance.json"        -o testdata/bundle.json
curl -sSL "$base/trusted-root-public-good.json" -o testdata/trusted_root.json
```

### Why the trust root is loaded from a file

The single most important design decision here is that `NewGate` calls
`root.NewTrustedRootFromPath`, not `root.FetchTrustedRoot`. Fetching pulls the
Fulcio CA roots and the Rekor/CT/TSA public keys over TUF on every call — network,
rate limits, and an outright failure on an air-gapped deploy host. A gate on the
hot path pins the trust material to a file and refreshes it out of band. The same
choice is what makes this exercise reproducible: the vendored `trusted_root.json`
and the vendored bundle together verify with no network at all.

### Why the verifier requires three thresholds

A Fulcio certificate lives about ten minutes, so the gate cannot check it against
the current wall clock — by verification time it has long expired. Instead the
verifier is built to *require a proof that the signature existed while the
certificate was valid*. `WithSignedCertificateTimestamps(1)` requires the SCT
proving the certificate was logged; `WithTransparencyLog(1)` requires a Rekor
inclusion proof; `WithObserverTimestamps(1)` requires at least one trusted
observer timestamp (the Rekor integrated time or a TSA countersignature). All
three at threshold 1 is the correct posture for public-good keyless bundles.

### Why the policy binds both digest and identity

Verification answers two independent questions. `WithArtifactDigest("sha512",
digest)` binds the signature to *this* artifact — the bundle's payload subject
must carry a matching SHA-512 digest. `WithCertificateIdentity` binds it to *this
signer* — the certificate's OIDC issuer and SAN must match the policy. Both are
mandatory. A matching digest with an unpinned identity trusts any signer on earth
(anyone can get a valid Fulcio certificate); a pinned identity with no digest
binding trusts a signature over some *other* artifact. `NewShortCertificateIdentity`
takes four positional strings — `issuer, issuerRegex, sanValue, sanRegex` — and
the gate threads a `Policy` struct through them so the roles stay named at the call
site instead of at a bare four-argument call.

Create `bundleverify.go`:

```go
package bundleverify

import (
	"errors"
	"fmt"
	"time"

	in_toto "github.com/in-toto/attestation/go/v1"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// Sentinel errors so callers classify failures with errors.Is without importing
// the sigstore-go packages.
var (
	ErrLoadTrustedRoot = errors.New("bundleverify: load trusted root")
	ErrBuildVerifier   = errors.New("bundleverify: build verifier")
	ErrLoadBundle      = errors.New("bundleverify: load bundle")
	ErrBuildIdentity   = errors.New("bundleverify: build certificate identity")
	ErrVerify          = errors.New("bundleverify: verification failed")
)

// Policy is the trust decision the gate enforces: which artifact digest and which
// signer identity are acceptable. Exactly one of SANValue/SANRegex and one of
// Issuer/IssuerRegex should be set, mirroring NewShortCertificateIdentity.
type Policy struct {
	Issuer          string // exact OIDC issuer, e.g. https://token.actions.githubusercontent.com
	IssuerRegex     string // anchored regex alternative to Issuer
	SANValue        string // exact Subject Alternative Name
	SANRegex        string // anchored regex alternative to SANValue
	DigestAlgorithm string // "sha256" or "sha512", matching how the bundle was signed
	Digest          []byte // raw digest bytes of the artifact
}

// Result is the structured verdict a deploy gate acts on.
type Result struct {
	Statement    *in_toto.Statement // the in-toto statement for a DSSE bundle; nil for a plain blob
	SignerSAN    string             // the actual Subject Alternative Name in the signing certificate
	SignerIssuer string             // the actual OIDC issuer in the signing certificate
	Timestamps   []time.Time        // the verified observer timestamps
}

// Gate holds a verifier built once from a pinned trust root and reused per
// verification, so the hot path never touches the network.
type Gate struct {
	verifier *verify.Verifier
}

// NewGate loads a pinned trusted root from disk and builds a verifier that
// requires an SCT, a Rekor inclusion proof, and an observer timestamp.
func NewGate(trustedRootPath string) (*Gate, error) {
	return NewGateWithThresholds(trustedRootPath, 1, 1, 1)
}

// NewGateWithThresholds is NewGate with explicit thresholds, so a caller can
// require more than one observer timestamp for a stricter posture.
func NewGateWithThresholds(trustedRootPath string, sct, tlog, observer int) (*Gate, error) {
	tr, err := root.NewTrustedRootFromPath(trustedRootPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrLoadTrustedRoot, err)
	}
	v, err := verify.NewVerifier(tr,
		verify.WithSignedCertificateTimestamps(sct),
		verify.WithTransparencyLog(tlog),
		verify.WithObserverTimestamps(observer),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBuildVerifier, err)
	}
	return &Gate{verifier: v}, nil
}

// VerifyBundle loads the bundle at path and verifies it against the policy,
// binding both the artifact digest and the signer identity.
func (g *Gate) VerifyBundle(path string, pol Policy) (*Result, error) {
	b, err := bundle.LoadJSONFromPath(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrLoadBundle, err)
	}

	certID, err := verify.NewShortCertificateIdentity(pol.Issuer, pol.IssuerRegex, pol.SANValue, pol.SANRegex)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBuildIdentity, err)
	}

	pb := verify.NewPolicy(
		verify.WithArtifactDigest(pol.DigestAlgorithm, pol.Digest),
		verify.WithCertificateIdentity(certID),
	)

	res, err := g.verifier.Verify(b, pb)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrVerify, err)
	}

	out := &Result{Statement: res.Statement}
	if res.Signature != nil && res.Signature.Certificate != nil {
		out.SignerSAN = res.Signature.Certificate.SubjectAlternativeName
		out.SignerIssuer = res.Signature.Certificate.Issuer
	}
	for _, ts := range res.VerifiedTimestamps {
		out.Timestamps = append(out.Timestamps, ts.Timestamp)
	}
	return out, nil
}
```

### The runnable demo

The demo verifies the vendored provenance bundle: the SLSA provenance for the npm
package `sigstore@1.3.0`, signed by the `sigstore-js` release workflow. It pins the
GitHub Actions OIDC issuer, an anchored SAN prefix, and the exact SHA-512 digest
recorded in the statement's subject. Run it from the module root so the
`testdata/` paths resolve.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/hex"
	"fmt"
	"log"

	"example.com/bundleverify"
)

// sha512 of the artifact recorded in the vendored provenance bundle's subject.
const artifactSHA512 = "76176ffa33808b54602c7c35de5c6e9a4deb96066dba6533f50ac234f4f1f4c6b3527515dc17c06fbe2860030f410eee69ea20079bd3a2c6f3dcf3b329b10751"

func main() {
	gate, err := bundleverify.NewGate("testdata/trusted_root.json")
	if err != nil {
		log.Fatal(err)
	}

	digest, err := hex.DecodeString(artifactSHA512)
	if err != nil {
		log.Fatal(err)
	}

	res, err := gate.VerifyBundle("testdata/bundle.json", bundleverify.Policy{
		Issuer:          "https://token.actions.githubusercontent.com",
		SANRegex:        "^https://github.com/sigstore/sigstore-js/",
		DigestAlgorithm: "sha512",
		Digest:          digest,
	})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("predicate type: %s\n", res.Statement.GetPredicateType())
	fmt.Printf("signer SAN: %s\n", res.SignerSAN)
	fmt.Printf("signer issuer: %s\n", res.SignerIssuer)
	fmt.Printf("timestamps verified: %v\n", len(res.Timestamps) > 0)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
predicate type: https://slsa.dev/provenance/v0.2
signer SAN: https://github.com/sigstore/sigstore-js/.github/workflows/release.yml@refs/heads/main
signer issuer: https://token.actions.githubusercontent.com
timestamps verified: true
```

### Tests

The table test drives the gate against the vendored bundle. The success case pins
the correct issuer, an anchored SAN regex, and the correct digest, and asserts the
statement and signer identity come back populated. Three failure cases each flip
one input — a SAN regex for a different org, a different issuer, and a single
mutated digest byte — and assert the wrapped `ErrVerify` surfaces via `errors.Is`.
A separate test raises the observer-timestamp threshold beyond what the bundle
carries and asserts it is rejected: this is the "your turn" extension made real,
proving the timestamp requirement is enforced, not decorative.

Create `bundleverify_test.go`:

```go
package bundleverify

import (
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
)

const (
	bundlePath = "testdata/bundle.json"
	trustPath  = "testdata/trusted_root.json"
	sha512hex  = "76176ffa33808b54602c7c35de5c6e9a4deb96066dba6533f50ac234f4f1f4c6b3527515dc17c06fbe2860030f410eee69ea20079bd3a2c6f3dcf3b329b10751"

	wantSAN    = "https://github.com/sigstore/sigstore-js/.github/workflows/release.yml@refs/heads/main"
	wantIssuer = "https://token.actions.githubusercontent.com"
)

func mustDigest(t *testing.T) []byte {
	t.Helper()
	d, err := hex.DecodeString(sha512hex)
	if err != nil {
		t.Fatalf("decode digest: %v", err)
	}
	return d
}

func TestVerifyBundle(t *testing.T) {
	t.Parallel()

	good := mustDigest(t)
	mutated := mustDigest(t)
	mutated[0] ^= 0xff // flip one byte so the digest no longer matches

	tests := []struct {
		name    string
		pol     Policy
		wantErr error
	}{
		{
			name: "correct identity and digest",
			pol: Policy{
				Issuer:          wantIssuer,
				SANRegex:        "^https://github.com/sigstore/sigstore-js/",
				DigestAlgorithm: "sha512",
				Digest:          good,
			},
		},
		{
			name: "wrong SAN regex",
			pol: Policy{
				Issuer:          wantIssuer,
				SANRegex:        "^https://github.com/attacker/",
				DigestAlgorithm: "sha512",
				Digest:          good,
			},
			wantErr: ErrVerify,
		},
		{
			name: "wrong issuer",
			pol: Policy{
				Issuer:          "https://accounts.google.com",
				SANRegex:        "^https://github.com/sigstore/sigstore-js/",
				DigestAlgorithm: "sha512",
				Digest:          good,
			},
			wantErr: ErrVerify,
		},
		{
			name: "mutated digest",
			pol: Policy{
				Issuer:          wantIssuer,
				SANRegex:        "^https://github.com/sigstore/sigstore-js/",
				DigestAlgorithm: "sha512",
				Digest:          mutated,
			},
			wantErr: ErrVerify,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gate, err := NewGate(trustPath)
			if err != nil {
				t.Fatalf("NewGate: %v", err)
			}
			res, err := gate.VerifyBundle(bundlePath, tc.pol)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("VerifyBundle err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("VerifyBundle: %v", err)
			}
			if res.Statement == nil {
				t.Fatal("Statement is nil on a DSSE bundle")
			}
			if res.SignerSAN != wantSAN {
				t.Fatalf("SignerSAN = %q, want %q", res.SignerSAN, wantSAN)
			}
			if res.SignerIssuer != wantIssuer {
				t.Fatalf("SignerIssuer = %q, want %q", res.SignerIssuer, wantIssuer)
			}
			if len(res.Timestamps) == 0 {
				t.Fatal("no verified timestamps")
			}
		})
	}
}

// TestObserverThresholdNotMet is the "your turn" case: raise the observer
// threshold above what the bundle carries and confirm the gate rejects it.
func TestObserverThresholdNotMet(t *testing.T) {
	t.Parallel()
	gate, err := NewGateWithThresholds(trustPath, 1, 1, 5)
	if err != nil {
		t.Fatalf("NewGateWithThresholds: %v", err)
	}
	_, err = gate.VerifyBundle(bundlePath, Policy{
		Issuer:          wantIssuer,
		SANRegex:        "^https://github.com/sigstore/sigstore-js/",
		DigestAlgorithm: "sha512",
		Digest:          mustDigest(t),
	})
	if !errors.Is(err, ErrVerify) {
		t.Fatalf("err = %v, want ErrVerify for unmet observer threshold", err)
	}
}

func Example() {
	gate, err := NewGate("testdata/trusted_root.json")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	digest, _ := hex.DecodeString(sha512hex)
	res, err := gate.VerifyBundle("testdata/bundle.json", Policy{
		Issuer:          "https://token.actions.githubusercontent.com",
		SANRegex:        "^https://github.com/sigstore/sigstore-js/",
		DigestAlgorithm: "sha512",
		Digest:          digest,
	})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(res.Statement.GetPredicateType())
	fmt.Println(res.SignerIssuer)
	// Output:
	// https://slsa.dev/provenance/v0.2
	// https://token.actions.githubusercontent.com
}
```

## Review

The gate is correct when a bundle passes only if three independent conditions all
hold: the signature chains to the pinned trust root with an SCT, a Rekor inclusion
proof, and an observer timestamp; the artifact digest matches under the declared
algorithm; and the signer's issuer and SAN match the policy. The four table cases
prove the AND — flipping any single input turns success into a wrapped `ErrVerify`
— and the threshold test proves the timestamp requirement is load-bearing.

The mistakes to avoid are the ones that make a gate silently permissive. Do not
verify without an identity: a matching digest alone trusts any signer, because
Fulcio issues valid certificates to anyone. Do not use an unanchored SAN regex:
`^https://github.com/sigstore/sigstore-js/` here is anchored at the start on
purpose; a bare substring would also match an attacker's fork. Do not fetch the
trust root on the hot path — pin it and load from disk, which is also what makes
this test offline and deterministic. And keep the digest algorithm string in step
with how the bundle was signed; this fixture is SHA-512, so `"sha256"` would fail.
Offline, the module cannot build without the sigstore-go dependency in the module
cache and the two vendored fixtures present; with both, `go test -count=1 ./...`
verifies end to end with no network.

## Resources

- [`sigstore-go/pkg/verify`](https://pkg.go.dev/github.com/sigstore/sigstore-go/pkg/verify) — `NewVerifier`, the threshold options, `NewPolicy`, `WithArtifactDigest`, `WithCertificateIdentity`, and `VerificationResult`.
- [`sigstore-go/pkg/root`](https://pkg.go.dev/github.com/sigstore/sigstore-go/pkg/root) — `NewTrustedRootFromPath`, `FetchTrustedRoot`, and `TrustedMaterial`.
- [sigstore-go verification guide](https://github.com/sigstore/sigstore-go/blob/main/docs/verification.md) — the full offline verify example with an identity policy.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-identity-policy-as-code.md](02-identity-policy-as-code.md)
