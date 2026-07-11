# Exercise 2: Generating a CycloneDX SBOM from the Module Graph

An SBOM is the inventory a container image ships for scanners and procurement.
This exercise turns a build's module graph into a valid CycloneDX BOM in JSON: the
main module as the application component, each dependency as a library component
with a Package URL, and each `go.sum` checksum rendered honestly. This is the SBOM
you would attach to a release artifact.

Because CycloneDX is an external module, the generator and its test sit behind a
`//go:build sbom` tag; the offline gate cannot fetch the dependency, so build
verification is deferred to a networked run. This module is self-contained: its
own `go mod init`, demo, and tests.

## What you'll build

```text
sbom/                       independent module: example.com/sbom
  go.mod                    requires github.com/CycloneDX/cyclonedx-go
  sbom.go                   //go:build sbom: Generate, Encode, Decode, goSumHash, purl
  cmd/
    demo/
      main.go               //go:build sbom: emit a pretty CycloneDX BOM to stdout
  sbom_test.go              //go:build sbom: round-trip, byte-stability, hash tests
```

- Files: `sbom.go`, `cmd/demo/main.go`, `sbom_test.go`.
- Implement: `Generate(*debug.BuildInfo, serial, timestamp)` producing a `*cdx.BOM` (application metadata component, one library component per dep with a `pkg:golang/` purl, the `go.sum` hash), plus `Encode`/`Decode` round-trip.
- Test: decode the emitted JSON and assert `bomFormat == "CycloneDX"`, component count equals deps, purls match `pkg:golang/`, each hash is SHA-256; assert byte-stable output when serial and timestamp are pinned.
- Verify: `go test -tags sbom ./...` (needs the module fetched).

Set up the module:

```bash
mkdir -p ~/go-exercises/sbom/cmd/demo
cd ~/go-exercises/sbom
go mod init example.com/sbom
go mod edit -go=1.26
go get github.com/CycloneDX/cyclonedx-go
```

### The mapping: module graph to BOM

CycloneDX models a build as a metadata component (the thing the BOM describes)
plus a flat list of components (its dependencies) plus a dependency graph relating
them. The mapping from Go's `debug.BuildInfo` is direct: `bi.Main` becomes the
metadata component with `Type: cdx.ComponentTypeApplication`, and each entry in
`bi.Deps` becomes a `cdx.ComponentTypeLibrary`. The glue that makes an SBOM useful
to a scanner is the Package URL: `pkg:golang/<module-path>@<version>`. That purl is
the interoperable identity a vulnerability scanner uses to join your component
against CVE feeds, so it goes in both `BOMRef` (the internal reference used by the
dependency graph) and `PackageURL`. The `Dependencies` slice then roots the
application on its direct dependencies by `BOMRef`.

### Rendering the go.sum hash honestly

This is the detail that separates a real SBOM from a decorative one. A dependency's
`Sum` is a `go.sum` line like `h1:GokP8Fi...=`. It is not a raw SHA-256 of a
downloadable archive — it is a *module dirhash*: SHA-256 of a synthetic listing of
the module's files, base64-encoded, with the `h1:` prefix naming the algorithm
version. So the 32 bytes underneath really are a SHA-256 digest, but of the
manifest, not of any tarball.

The honest rendering — the one the real `cyclonedx-gomod` tool uses — is to strip
`h1:`, base64-decode to the 32 raw bytes, and hex-encode those into a standard
`cdx.HashAlgoSHA256` hash. Because CycloneDX's hash field has no place to record
*what* was hashed, this exercise additionally preserves the original `h1:` token
verbatim in a namespaced `go:mod:h1` property, so a verifier can recompute it the
correct way (via `golang.org/x/mod/sumdb/dirhash`) rather than being misled into
running `sha256sum` on an archive. If the decode fails or the length is not 32
bytes, the entry carries no hash rather than a fabricated one.

### Reproducibility: pin the non-deterministic fields

An SBOM you regenerate on every build should be byte-identical when nothing
changed, or diffing it is worthless. Two fields sabotage that by default: the BOM
serial number (normally random) and the metadata timestamp. `Generate` takes both
as parameters so a caller can pin them; with them fixed and components emitted in
graph order (a slice, not a map), the encoder's output is byte-stable. The test
asserts this directly.

Create `sbom.go`:

```go
//go:build sbom

package sbom

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"runtime/debug"

	cdx "github.com/CycloneDX/cyclonedx-go"
)

// purl builds a Package URL for a Go module: pkg:golang/<path>@<version>.
func purl(path, version string) string {
	return "pkg:golang/" + path + "@" + version
}

// Generate turns a build's module graph into a CycloneDX BOM. serial and
// timestamp are injected so the output is byte-stable for diffing.
func Generate(bi *debug.BuildInfo, serial, timestamp string) (*cdx.BOM, error) {
	mainRef := purl(bi.Main.Path, bi.Main.Version)

	metaComp := &cdx.Component{
		BOMRef:     mainRef,
		Type:       cdx.ComponentTypeApplication,
		Name:       bi.Main.Path,
		Version:    bi.Main.Version,
		PackageURL: mainRef,
	}

	comps := make([]cdx.Component, 0, len(bi.Deps))
	directRefs := make([]string, 0, len(bi.Deps))
	for _, d := range bi.Deps {
		ref := purl(d.Path, d.Version)
		c := cdx.Component{
			BOMRef:     ref,
			Type:       cdx.ComponentTypeLibrary,
			Name:       d.Path,
			Version:    d.Version,
			PackageURL: ref,
		}
		if h, ok := goSumHash(d.Sum); ok {
			c.Hashes = &[]cdx.Hash{h}
			c.Properties = &[]cdx.Property{{Name: "go:mod:h1", Value: d.Sum}}
		}
		comps = append(comps, c)
		directRefs = append(directRefs, ref)
	}

	deps := []cdx.Dependency{{Ref: mainRef, Dependencies: &directRefs}}

	bom := cdx.NewBOM()
	bom.SerialNumber = serial
	bom.Version = 1
	bom.Metadata = &cdx.Metadata{
		Timestamp: timestamp,
		Component: metaComp,
	}
	bom.Components = &comps
	bom.Dependencies = &deps
	return bom, nil
}

// goSumHash converts a go.sum h1: value into a CycloneDX Hash. The h1 scheme is
// base64(SHA-256(dirhash file list)); we decode and hex-encode the 32 raw bytes.
// The digest is a real SHA-256, but of Go's module dirhash manifest, not of any
// downloadable archive, so a verifier must recompute it with the dirhash package.
func goSumHash(sum string) (cdx.Hash, bool) {
	const prefix = "h1:"
	if len(sum) <= len(prefix) || sum[:len(prefix)] != prefix {
		return cdx.Hash{}, false
	}
	raw, err := base64.StdEncoding.DecodeString(sum[len(prefix):])
	if err != nil || len(raw) != 32 {
		return cdx.Hash{}, false
	}
	return cdx.Hash{Algorithm: cdx.HashAlgoSHA256, Value: fmt.Sprintf("%x", raw)}, true
}

// Encode renders the BOM as pretty JSON.
func Encode(bom *cdx.BOM) ([]byte, error) {
	var buf bytes.Buffer
	enc := cdx.NewBOMEncoder(&buf, cdx.BOMFileFormatJSON)
	enc.SetPretty(true)
	if err := enc.Encode(bom); err != nil {
		return nil, fmt.Errorf("encode bom: %w", err)
	}
	return buf.Bytes(), nil
}

// Decode parses CycloneDX JSON back into a BOM (used to prove round-trip).
func Decode(data []byte) (*cdx.BOM, error) {
	bom := &cdx.BOM{}
	dec := cdx.NewBOMDecoder(bytes.NewReader(data), cdx.BOMFileFormatJSON)
	if err := dec.Decode(bom); err != nil {
		return nil, fmt.Errorf("decode bom: %w", err)
	}
	return bom, nil
}
```

### The runnable demo

The demo constructs a small `debug.BuildInfo` by hand (in a real pipeline you feed
it the output of Exercise 1's `FromRunning` or `Parse`) and prints a pretty BOM.
The dependency's `Sum` is a real `go.sum` `h1:` value, so the emitted hash is the
genuine hex of its 32 decoded bytes.

Create `cmd/demo/main.go`:

```go
//go:build sbom

package main

import (
	"fmt"
	"os"
	"runtime/debug"

	"example.com/sbom"
)

func main() {
	bi := &debug.BuildInfo{
		GoVersion: "go1.26.0",
		Path:      "example.com/checkout",
		Main:      debug.Module{Path: "example.com/checkout", Version: "v1.4.0"},
		Deps: []*debug.Module{
			{Path: "github.com/google/uuid", Version: "v1.6.0", Sum: "h1:NIvaJDMOsjHA8n1jAhLSgzrAzy1Hgr+hNrb57e+94F0="},
		},
	}

	bom, err := sbom.Generate(bi, "urn:uuid:00000000-0000-0000-0000-000000000001", "2026-06-01T12:00:00Z")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	out, err := sbom.Encode(bom)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Stdout.Write(out)
}
```

Run it:

```bash
go run -tags sbom ./cmd/demo
```

Expected output:

```
{
  "$schema": "http://cyclonedx.org/schema/bom-1.7.schema.json",
  "bomFormat": "CycloneDX",
  "specVersion": "1.7",
  "serialNumber": "urn:uuid:00000000-0000-0000-0000-000000000001",
  "version": 1,
  "metadata": {
    "timestamp": "2026-06-01T12:00:00Z",
    "component": {
      "bom-ref": "pkg:golang/example.com/checkout@v1.4.0",
      "type": "application",
      "name": "example.com/checkout",
      "version": "v1.4.0",
      "purl": "pkg:golang/example.com/checkout@v1.4.0"
    }
  },
  "components": [
    {
      "bom-ref": "pkg:golang/github.com/google/uuid@v1.6.0",
      "type": "library",
      "name": "github.com/google/uuid",
      "version": "v1.6.0",
      "hashes": [
        {
          "alg": "SHA-256",
          "content": "348bda24330eb231c0f27d630212d2833ac0cf2d4782bfa136b6f9edefbde05d"
        }
      ],
      "purl": "pkg:golang/github.com/google/uuid@v1.6.0",
      "properties": [
        {
          "name": "go:mod:h1",
          "value": "h1:NIvaJDMOsjHA8n1jAhLSgzrAzy1Hgr+hNrb57e+94F0="
        }
      ]
    }
  ],
  "dependencies": [
    {
      "ref": "pkg:golang/example.com/checkout@v1.4.0",
      "dependsOn": [
        "pkg:golang/github.com/google/uuid@v1.6.0"
      ]
    }
  ]
}
```

### Tests

`TestGenerateRoundTrip` encodes the BOM and decodes it back, asserting the
CycloneDX format marker, that the component count equals the dependency count,
that every purl uses the `pkg:golang/` scheme and every library carries a SHA-256
hash, and that the metadata component is the application. `TestGenerateByteStable`
generates twice with pinned serial and timestamp and requires identical bytes.
`TestGoSumHash` checks the decode-and-hex conversion and the rejection of a
non-`h1:` value.

Create `sbom_test.go`:

```go
//go:build sbom

package sbom

import (
	"bytes"
	"runtime/debug"
	"strings"
	"testing"

	cdx "github.com/CycloneDX/cyclonedx-go"
)

func fixtureBuildInfo() *debug.BuildInfo {
	return &debug.BuildInfo{
		GoVersion: "go1.26.0",
		Path:      "example.com/checkout",
		Main:      debug.Module{Path: "example.com/checkout", Version: "v1.4.0"},
		Deps: []*debug.Module{
			{Path: "github.com/google/uuid", Version: "v1.6.0", Sum: "h1:GokP8FiRC+foiuwWhSSLpSD5H4hSWtGnR3wo7apkBFI="},
			{Path: "golang.org/x/crypto", Version: "v0.31.0", Sum: "h1:GokP8FiRC+foiuwWhSSLpSD5H4hSWtGnR3wo7apkBFI="},
		},
	}
}

func TestGenerateRoundTrip(t *testing.T) {
	t.Parallel()
	bi := fixtureBuildInfo()
	bom, err := Generate(bi, "urn:uuid:0", "2026-06-01T12:00:00Z")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	out, err := Encode(bom)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := Decode(out)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.BOMFormat != "CycloneDX" {
		t.Errorf("bomFormat = %q, want CycloneDX", got.BOMFormat)
	}
	if got.Components == nil || len(*got.Components) != len(bi.Deps) {
		t.Fatalf("components = %v, want %d", got.Components, len(bi.Deps))
	}
	for _, c := range *got.Components {
		if !strings.HasPrefix(c.PackageURL, "pkg:golang/") {
			t.Errorf("purl %q not pkg:golang/ scheme", c.PackageURL)
		}
		if c.Type != cdx.ComponentTypeLibrary {
			t.Errorf("component type = %q, want library", c.Type)
		}
		if c.Hashes == nil || (*c.Hashes)[0].Algorithm != cdx.HashAlgoSHA256 {
			t.Errorf("component %s missing SHA-256 hash", c.Name)
		}
	}
	if got.Metadata == nil || got.Metadata.Component == nil ||
		got.Metadata.Component.Type != cdx.ComponentTypeApplication {
		t.Fatalf("metadata component must be the application")
	}
}

func TestGenerateByteStable(t *testing.T) {
	t.Parallel()
	bi := fixtureBuildInfo()
	a, _ := Generate(bi, "urn:uuid:fixed", "2026-06-01T12:00:00Z")
	b, _ := Generate(bi, "urn:uuid:fixed", "2026-06-01T12:00:00Z")
	oa, _ := Encode(a)
	ob, _ := Encode(b)
	if !bytes.Equal(oa, ob) {
		t.Fatal("pinned serial+timestamp did not produce byte-stable output")
	}
}

func TestGoSumHash(t *testing.T) {
	t.Parallel()
	h, ok := goSumHash("h1:GokP8FiRC+foiuwWhSSLpSD5H4hSWtGnR3wo7apkBFI=")
	if !ok {
		t.Fatal("valid h1 sum rejected")
	}
	if h.Algorithm != cdx.HashAlgoSHA256 {
		t.Errorf("alg = %q, want SHA-256", h.Algorithm)
	}
	if len(h.Value) != 64 {
		t.Errorf("hex value len = %d, want 64", len(h.Value))
	}
	if _, ok := goSumHash(""); ok {
		t.Error("empty sum accepted")
	}
}
```

## Review

The generator is correct when the BOM round-trips through the CycloneDX decoder
with `bomFormat` set, one library component per dependency each carrying a
`pkg:golang/` purl, the application as the metadata component, and every hash a
real SHA-256 derived by decoding and hex-encoding the `h1:` value rather than
copying the base64 token. The byte-stability test proves you pinned the serial
number and timestamp; if it flakes, some non-deterministic field leaked in.

The mistakes to avoid: hand-writing the JSON instead of using the typed encoder
(which is how `specVersion`, `bomFormat`, and purl shapes get subtly wrong);
mislabeling the dirhash as an artifact SHA-256, or worse, copying the base64 token
into a hash field where a hex value belongs; and forgetting the `go:mod:h1`
property, which is the only place the exact `go.sum` token survives for a verifier
to recompute correctly. Offline this module cannot build without the CycloneDX
module fetched; run `go test -tags sbom ./...` against a populated cache.

## Resources

- [`cyclonedx-go` package reference](https://pkg.go.dev/github.com/CycloneDX/cyclonedx-go) — `BOM`, `Component`, `Hash`, `Dependency`, the encoder and decoder.
- [CycloneDX specification](https://cyclonedx.org/specification/overview/) — the BOM model, components, dependencies, and hashes.
- [Package URL (purl) specification](https://github.com/package-url/purl-spec) — the `pkg:golang/` identity scheme scanners join on.
- [`cyclonedx-gomod`](https://github.com/CycloneDX/cyclonedx-gomod) — the reference tool; see how it decodes the `h1:` hash.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-slsa-provenance-attestation.md](03-slsa-provenance-attestation.md)
