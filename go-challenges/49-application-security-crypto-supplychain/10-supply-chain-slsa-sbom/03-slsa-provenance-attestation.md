# Exercise 3: Building and Verifying an in-toto SLSA Provenance Attestation

Provenance answers "who built this, from what source, with what parameters" — and
it is worthless unless a consumer verifies it. This exercise builds both halves: a
producer that assembles an in-toto Statement wrapping a SLSA Provenance v1
predicate, and a verifier that gates a deploy on the subject digest and a pinned
builder identity, classifying the result against SLSA build levels.

The in-toto bindings and protobuf are external modules, so the code and tests sit
behind a `//go:build slsa` tag; the offline gate cannot fetch them, so build
verification is deferred to a networked run. This module is self-contained.

## What you'll build

```text
attest/                     independent module: example.com/attest
  go.mod                    requires in-toto/attestation + protobuf
  attest.go                 //go:build slsa: BuildStatement, Verifier, Level, marshal/parse
  cmd/
    demo/
      main.go               //go:build slsa: build, gate, and classify an attestation
  attest_test.go            //go:build slsa: round-trip, Validate, gate, Example
```

- Files: `attest.go`, `cmd/demo/main.go`, `attest_test.go`.
- Implement: `BuildStatement(Artifact)` assembling a Statement (subject digest + SLSA Provenance predicate), JSON marshal/parse via `protojson`, a `Verifier.Verify` gating on digest + pinned `builder.id`, and `Level` classifying L0/L1/L2/L3.
- Test: a complete predicate `Validate()`s nil; dropping `builder.id` yields `ErrBuilderIdRequired`; the gate passes on a matching digest + trusted builder and fails on a mismatch or an untrusted builder; round-trip the Statement through JSON.
- Verify: `go test -tags slsa ./...` (needs the modules fetched).

Set up the module:

```bash
mkdir -p go-solutions/49-application-security-crypto-supplychain/10-supply-chain-slsa-sbom/03-slsa-provenance-attestation/cmd/demo
cd go-solutions/49-application-security-crypto-supplychain/10-supply-chain-slsa-sbom/03-slsa-provenance-attestation
go mod edit -go=1.26
go get github.com/in-toto/attestation/go/v1
go get github.com/in-toto/attestation/go/predicates/provenance/v1
go get google.golang.org/protobuf
```

### Envelope and predicate, kept separate

The in-toto framework splits the claim into two layers. The `Statement` is the
envelope: it binds a *subject* (the artifact and its digest) to a `predicateType`
URI and a `predicate` payload. SLSA provenance is one predicate type among many —
an SBOM attestation or a scan result would use different ones — and keeping the
envelope generic is what lets a single verification pipeline dispatch on
`predicateType` and handle every kind of claim uniformly. In the Go bindings the
`Statement` is a protobuf message whose `Predicate` field is a `structpb.Struct`,
so you build the strongly typed SLSA `Provenance` message, then convert it into
that struct.

The SLSA `Provenance` predicate has two halves. `BuildDefinition` is the *what*:
the `buildType` (a URI naming the build recipe), `externalParameters` (the inputs
a user controlled — here the source repository and ref), and
`resolvedDependencies` (the inputs that were actually resolved, such as the git
commit). `RunDetails` is the *who and when*: the `Builder.Id` identity and
`BuildMetadata` with an invocation id and start/finish timestamps.

### Two protojson conversions you cannot skip

Because these are protobuf messages, JSON goes through
`google.golang.org/protobuf/encoding/protojson`, not `encoding/json` — the field
names (for example the subject's `_type`) depend on it. There are two conversions.
To place the typed `Provenance` into the Statement's `structpb.Struct` predicate,
you marshal the predicate to JSON and unmarshal it back into a `structpb.Struct`.
To read it out in the verifier, you marshal the struct and unmarshal into a fresh
`Provenance`. This round-trip through JSON is the idiomatic way to move between a
typed predicate and the generic envelope.

A second constraint: `structpb.NewStruct` accepts only JSON-compatible values
(`nil`, `bool`, numbers as `float64`, `string`, `[]any`, `map[string]any`). Build
`externalParameters` from a `map[string]any` of strings; do not hand it an
`int64`, a `time.Time`, or a struct.

### Validate is a cascade; dropping builder.id proves L1's minimum

The predicate's `Validate()` cascades: `Provenance` checks `BuildDefinition`
(requiring `buildType` and a non-empty `externalParameters`), then `RunDetails`,
then `Builder`, whose empty `Id` returns `ErrBuilderIdRequired`. Because every
layer wraps the inner error with `%w`, `errors.Is(prov.Validate(), ErrBuilderIdRequired)`
holds even though the message is nested under `provenance.runDetails error: ...`.
That specific error is the concrete SLSA Build L1 minimum: L1 requires *complete*
provenance, and a builder with no identity is incomplete. (Note a subtlety the
test respects: an entirely empty `Builder` triggers `ErrBuilderRequired` first, so
to reach the id check the builder must be non-empty with an empty id.)

### The verifier is the security; pin the builder

A verifier that accepts any attestation is theater. `Verify` does two checks that
matter. First, the subject digest must equal the digest of the artifact you
actually hold — otherwise the provenance describes some other artifact. Second,
the `builder.id` must be in an allowlist you control; an attacker's own build
platform emits perfectly valid provenance naming itself, so without the allowlist
the whole exercise proves nothing. Both failures are `%w`-wrapped sentinels.

`Level` classifies the result. L0 is a predicate that does not validate. L1 is
complete-but-unsigned provenance. L2 requires a verified signature from a hosted
builder, and L3 additionally requires an isolated, non-forgeable builder — signing
is the next lesson, so here `signed` and `isolated` are modeled as inputs
representing signals a full pipeline would establish.

Create `attest.go`:

```go
//go:build slsa

package attest

import (
	"errors"
	"fmt"

	prov "github.com/in-toto/attestation/go/predicates/provenance/v1"
	v1 "github.com/in-toto/attestation/go/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// PredicateTypeSLSAProvenance is the SLSA Provenance v1 predicate type URI. The
// in-toto Go module does not export it, so we pin it here.
const PredicateTypeSLSAProvenance = "https://slsa.dev/provenance/v1"

var (
	ErrDigestMismatch   = errors.New("verify: subject digest does not match expected artifact digest")
	ErrUntrustedBuilder = errors.New("verify: builder.id is not in the trusted allowlist")
)

// Artifact identifies the thing built.
type Artifact struct {
	Name         string
	SHA256Hex    string
	SourceRepo   string
	SourceRef    string
	SourceCommit string
	BuilderID    string
	InvocationID string
	StartedOn    *timestamppb.Timestamp
	FinishedOn   *timestamppb.Timestamp
}

// BuildStatement assembles a signed-ready in-toto Statement whose predicate is a
// SLSA Provenance v1 record for the given artifact.
func BuildStatement(a Artifact) (*v1.Statement, error) {
	ext, err := structpb.NewStruct(map[string]any{
		"sourceRepository": a.SourceRepo,
		"ref":              a.SourceRef,
	})
	if err != nil {
		return nil, fmt.Errorf("external parameters: %w", err)
	}

	predicate := &prov.Provenance{
		BuildDefinition: &prov.BuildDefinition{
			BuildType:          "https://slsa.dev/container-based-build/v0.1",
			ExternalParameters: ext,
			ResolvedDependencies: []*v1.ResourceDescriptor{
				{Uri: "git+" + a.SourceRepo, Digest: map[string]string{"gitCommit": a.SourceCommit}},
			},
		},
		RunDetails: &prov.RunDetails{
			Builder: &prov.Builder{Id: a.BuilderID},
			Metadata: &prov.BuildMetadata{
				InvocationId: a.InvocationID,
				StartedOn:    a.StartedOn,
				FinishedOn:   a.FinishedOn,
			},
		},
	}
	if err := predicate.Validate(); err != nil {
		return nil, fmt.Errorf("invalid provenance predicate: %w", err)
	}

	predStruct, err := predicateToStruct(predicate)
	if err != nil {
		return nil, err
	}

	stmt := &v1.Statement{
		Type: v1.StatementTypeUri,
		Subject: []*v1.ResourceDescriptor{
			{Name: a.Name, Digest: map[string]string{string(v1.AlgorithmSHA256): a.SHA256Hex}},
		},
		PredicateType: PredicateTypeSLSAProvenance,
		Predicate:     predStruct,
	}
	if err := stmt.Validate(); err != nil {
		return nil, fmt.Errorf("invalid statement: %w", err)
	}
	return stmt, nil
}

func predicateToStruct(p *prov.Provenance) (*structpb.Struct, error) {
	b, err := protojson.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("marshal predicate: %w", err)
	}
	s := &structpb.Struct{}
	if err := protojson.Unmarshal(b, s); err != nil {
		return nil, fmt.Errorf("predicate to struct: %w", err)
	}
	return s, nil
}

// MarshalStatement serializes a Statement to JSON via protojson.
func MarshalStatement(s *v1.Statement) ([]byte, error) {
	return protojson.Marshal(s)
}

// ParseStatement decodes JSON back into a Statement.
func ParseStatement(data []byte) (*v1.Statement, error) {
	s := &v1.Statement{}
	if err := protojson.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("parse statement: %w", err)
	}
	return s, nil
}

// PredicateOf extracts the SLSA provenance predicate from a statement.
func PredicateOf(s *v1.Statement) (*prov.Provenance, error) {
	b, err := protojson.Marshal(s.Predicate)
	if err != nil {
		return nil, fmt.Errorf("marshal predicate struct: %w", err)
	}
	p := &prov.Provenance{}
	if err := protojson.Unmarshal(b, p); err != nil {
		return nil, fmt.Errorf("decode provenance: %w", err)
	}
	return p, nil
}

// Verifier gates a deploy on an attestation.
type Verifier struct {
	ExpectedDigestHex string
	TrustedBuilders   map[string]bool
}

// Verify checks the statement's subject digest and builder identity.
func (v Verifier) Verify(s *v1.Statement) error {
	if err := s.Validate(); err != nil {
		return fmt.Errorf("verify: %w", err)
	}
	matched := false
	for _, sub := range s.Subject {
		if sub.Digest[string(subjectAlg)] == v.ExpectedDigestHex && v.ExpectedDigestHex != "" {
			matched = true
		}
	}
	if !matched {
		return fmt.Errorf("verify: %w", ErrDigestMismatch)
	}
	p, err := PredicateOf(s)
	if err != nil {
		return err
	}
	if err := p.Validate(); err != nil {
		return fmt.Errorf("verify: %w", err)
	}
	id := p.GetRunDetails().GetBuilder().GetId()
	if !v.TrustedBuilders[id] {
		return fmt.Errorf("verify: builder %q: %w", id, ErrUntrustedBuilder)
	}
	return nil
}

const subjectAlg = v1.AlgorithmSHA256

// Level classifies an attestation into a SLSA build-level label. signed models
// whether a DSSE/Sigstore signature over the statement was verified (the next
// lesson); isolated models a non-forgeable, isolated builder (SLSA L3).
func Level(p *prov.Provenance, signed, isolated bool) string {
	if p == nil || p.Validate() != nil {
		return "L0"
	}
	switch {
	case signed && isolated:
		return "L3"
	case signed:
		return "L2"
	default:
		return "L1"
	}
}
```

### The runnable demo

The demo hashes some bytes to stand in for a built image, produces an attestation,
runs the gate with a pinned builder, and prints the level under each signal.

Create `cmd/demo/main.go`:

```go
//go:build slsa

package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"time"

	"example.com/attest"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func main() {
	digest := sha256.Sum256([]byte("the built container image bytes"))
	digestHex := fmt.Sprintf("%x", digest)

	start := timestamppb.New(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	finish := timestamppb.New(time.Date(2026, 6, 1, 12, 3, 0, 0, time.UTC))

	art := attest.Artifact{
		Name:         "ghcr.io/acme/checkout:v1.4.0",
		SHA256Hex:    digestHex,
		SourceRepo:   "https://github.com/acme/checkout",
		SourceRef:    "refs/tags/v1.4.0",
		SourceCommit: "9c1f2b3a4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f90",
		BuilderID:    "https://github.com/acme/.github/workflows/build.yml@refs/heads/main",
		InvocationID: "run-1234567890-1",
		StartedOn:    start,
		FinishedOn:   finish,
	}

	stmt, err := attest.BuildStatement(art)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	v := attest.Verifier{
		ExpectedDigestHex: digestHex,
		TrustedBuilders: map[string]bool{
			"https://github.com/acme/.github/workflows/build.yml@refs/heads/main": true,
		},
	}
	if err := v.Verify(stmt); err != nil {
		fmt.Printf("gate: REJECT (%v)\n", err)
		os.Exit(1)
	}
	fmt.Println("gate: ACCEPT")

	pred, _ := attest.PredicateOf(stmt)
	fmt.Printf("subject:  %s\n", stmt.Subject[0].Name)
	fmt.Printf("builder:  %s\n", pred.GetRunDetails().GetBuilder().GetId())
	fmt.Printf("level (unsigned): %s\n", attest.Level(pred, false, false))
	fmt.Printf("level (signed):   %s\n", attest.Level(pred, true, false))
	fmt.Printf("level (isolated): %s\n", attest.Level(pred, true, true))
}
```

Run it:

```bash
go run -tags slsa ./cmd/demo
```

Expected output:

```
gate: ACCEPT
subject:  ghcr.io/acme/checkout:v1.4.0
builder:  https://github.com/acme/.github/workflows/build.yml@refs/heads/main
level (unsigned): L1
level (signed):   L2
level (isolated): L3
```

### Tests

`TestBuildAndVerifyRoundTrip` marshals the Statement to JSON, reparses it, and
verifies it — proving the envelope survives serialization.
`TestPredicateValidateBuilderID` shows a complete predicate validating nil and a
builder with a cleared id yielding `ErrBuilderIdRequired` via `errors.Is`.
`TestVerifierGate` is table-driven over the trusted path, a digest mismatch, and
an untrusted builder. The `Example` renders the three level strings.

Create `attest_test.go`:

```go
//go:build slsa

package attest

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"

	prov "github.com/in-toto/attestation/go/predicates/provenance/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func sampleArtifact() Artifact {
	d := sha256.Sum256([]byte("image bytes"))
	return Artifact{
		Name:         "ghcr.io/acme/checkout:v1.4.0",
		SHA256Hex:    fmt.Sprintf("%x", d),
		SourceRepo:   "https://github.com/acme/checkout",
		SourceRef:    "refs/tags/v1.4.0",
		SourceCommit: "9c1f2b3a4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f90",
		BuilderID:    "https://github.com/acme/.github/workflows/build.yml@refs/heads/main",
		InvocationID: "run-1",
		StartedOn:    timestamppb.New(time.Unix(1000, 0)),
		FinishedOn:   timestamppb.New(time.Unix(1180, 0)),
	}
}

func trustedVerifier(a Artifact) Verifier {
	return Verifier{
		ExpectedDigestHex: a.SHA256Hex,
		TrustedBuilders:   map[string]bool{a.BuilderID: true},
	}
}

func TestBuildAndVerifyRoundTrip(t *testing.T) {
	t.Parallel()
	a := sampleArtifact()
	stmt, err := BuildStatement(a)
	if err != nil {
		t.Fatalf("BuildStatement: %v", err)
	}
	data, err := MarshalStatement(stmt)
	if err != nil {
		t.Fatalf("MarshalStatement: %v", err)
	}
	reparsed, err := ParseStatement(data)
	if err != nil {
		t.Fatalf("ParseStatement: %v", err)
	}
	if err := trustedVerifier(a).Verify(reparsed); err != nil {
		t.Fatalf("Verify after round-trip: %v", err)
	}
}

func TestPredicateValidateBuilderID(t *testing.T) {
	t.Parallel()
	p := &prov.Provenance{
		BuildDefinition: mustBuildDef(t),
		RunDetails: &prov.RunDetails{
			Builder: &prov.Builder{Id: "https://ci/build"},
		},
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("complete predicate should validate: %v", err)
	}
	// Clear the id but keep the builder otherwise non-empty, so validation
	// reaches the id check (an entirely empty builder yields ErrBuilderRequired).
	p.RunDetails.Builder = &prov.Builder{Id: "", Version: map[string]string{"host": "ci"}}
	if err := p.Validate(); !errors.Is(err, prov.ErrBuilderIdRequired) {
		t.Fatalf("Validate = %v, want ErrBuilderIdRequired", err)
	}
}

func TestVerifierGate(t *testing.T) {
	t.Parallel()
	a := sampleArtifact()
	stmt, err := BuildStatement(a)
	if err != nil {
		t.Fatalf("BuildStatement: %v", err)
	}

	tests := []struct {
		name    string
		v       Verifier
		wantErr error
	}{
		{"trusted", trustedVerifier(a), nil},
		{
			name:    "wrong digest",
			v:       Verifier{ExpectedDigestHex: "00", TrustedBuilders: map[string]bool{a.BuilderID: true}},
			wantErr: ErrDigestMismatch,
		},
		{
			name:    "untrusted builder",
			v:       Verifier{ExpectedDigestHex: a.SHA256Hex, TrustedBuilders: map[string]bool{"other": true}},
			wantErr: ErrUntrustedBuilder,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.v.Verify(stmt)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Verify = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Verify = %v, want errors.Is %v", err, tc.wantErr)
			}
		})
	}
}

func mustBuildDef(t *testing.T) *prov.BuildDefinition {
	t.Helper()
	stmt, err := BuildStatement(sampleArtifact())
	if err != nil {
		t.Fatalf("BuildStatement: %v", err)
	}
	p, err := PredicateOf(stmt)
	if err != nil {
		t.Fatalf("PredicateOf: %v", err)
	}
	return p.BuildDefinition
}

func ExampleLevel() {
	stmt, _ := BuildStatement(sampleArtifact())
	p, _ := PredicateOf(stmt)
	fmt.Println(Level(p, false, false))
	fmt.Println(Level(p, true, false))
	fmt.Println(Level(p, true, true))
	// Output:
	// L1
	// L2
	// L3
}
```

## Review

The attestation is correct when a complete predicate validates, the Statement
round-trips through `protojson` and re-verifies, and the gate rejects on exactly
two conditions: a subject digest that does not match the artifact in hand, and a
`builder.id` outside the allowlist. Because `Validate` wraps its inner errors with
`%w`, `errors.Is(prov.Validate(), ErrBuilderIdRequired)` holds for a builder with
an empty id — the concrete L1-minimum failure.

The mistakes to avoid: using `encoding/json` on these protobuf messages instead of
`protojson` (the field names go wrong); passing non-JSON values into
`structpb.NewStruct`; putting a non-hex value in a `gitCommit` digest (the
resource-descriptor validator rejects it); and, above all, writing a verifier that
does not pin `builder.id`, which accepts provenance from any builder and secures
nothing. Signing the statement — the DSSE envelope, cosign, Fulcio, and Rekor that
turn this L1 payload into L2 — is the next lesson; the two compose, with this
statement as the payload. Offline this module cannot build without the in-toto and
protobuf modules fetched; run `go test -tags slsa ./...` against a populated cache.

## Resources

- [in-toto attestation Go bindings (`go/v1`)](https://pkg.go.dev/github.com/in-toto/attestation/go/v1) — `Statement`, `ResourceDescriptor`, `StatementTypeUri`, `AlgorithmSHA256`.
- [SLSA Provenance v1 predicate Go types](https://pkg.go.dev/github.com/in-toto/attestation/go/predicates/provenance/v1) — `Provenance`, `BuildDefinition`, `RunDetails`, `Validate`.
- [SLSA v1.0 build levels](https://slsa.dev/spec/v1.0/levels) — what L1, L2, and L3 require of the builder and the provenance.
- [in-toto Attestation Framework](https://github.com/in-toto/attestation/blob/main/spec/v1/README.md) — the envelope, subject, and predicate model.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../11-sigstore-cosign-signing/00-concepts.md](../11-sigstore-cosign-signing/00-concepts.md)
