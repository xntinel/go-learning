# Exercise 3: Verifying a Binary's FIPS Build Provenance in CI

"Was this artifact built FIPS-configured?" should be a fail-closed CI gate, not a
line in a runbook. This exercise builds a verifier that reads a Go binary's
embedded build settings — the `GOFIPS140` build setting and the `DefaultGODEBUG`
baked in from a `go.mod` `godebug` directive or a `//go:debug` line — and refuses
to let the artifact ship unless it was compiled FIPS-configured. It turns build
provenance from tribal knowledge into a policy check over data the toolchain
already records.

This module is fully self-contained: its own `go mod init`, all parsing inline,
its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
fipsprovenance/              independent module: example.com/fipsprovenance
  go.mod                     go 1.26
  fipsprovenance.go          Policy; checkProvenance (pure); parseGODEBUG; parseVersionM; Verify; FromBuildInfo
  cmd/
    demo/
      main.go                reads its own BuildInfo and prints the provenance verdict
  fipsprovenance_test.go     tables over checkProvenance and parseVersionM; Example
```

- Files: `fipsprovenance.go`, `cmd/demo/main.go`, `fipsprovenance_test.go`.
- Implement: `checkProvenance(settings, policy) error` (pure); `parseGODEBUG` and `parseVersionM` (pure parsers); `Verify(r io.Reader, policy)` that gates piped `go version -m` output; `FromBuildInfo()` for the self-check demo.
- Test: a table over `checkProvenance` (GOFIPS140 present + fips140=on passes; missing GOFIPS140 fails; fips140=off fails; empty/unknown fails closed) and a table over `parseVersionM` against real `go version -m` output.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/49-application-security-crypto-supplychain/08-fips-140-3-mode/03-fips-build-attestation/cmd/demo
cd go-solutions/49-application-security-crypto-supplychain/08-fips-140-3-mode/03-fips-build-attestation
go mod edit -go=1.26
```

### The three configuration surfaces, and which ones are inspectable

FIPS build configuration lives on three surfaces. `GOFIPS140` is an environment
variable at *build* time (`GOFIPS140=v1.0.0 go build ...`); the toolchain records
it as a `GOFIPS140` build setting in the binary. The `fips140` runtime mode can be
baked into the binary two ways that leave a trace: a `godebug fips140=on` line in
`go.mod`, or a `//go:debug fips140=on` directive in `package main`. Both are
recorded as a `DefaultGODEBUG` build setting. All three are readable *without
running the binary* — via `go version -m <binary>` on the command line, or via
`runtime/debug.ReadBuildInfo` from inside the program. That inspectability is the
entire basis for build attestation: CI reads the artifact, not a wiki.

The one surface that leaves *no* trace is the runtime `GODEBUG=fips140=on`
environment variable — it configures a process, not a binary, so it cannot be
attested from the artifact. A verifier that only accepted the runtime env var
would have nothing to check; that is why the gate reads `GOFIPS140` and
`DefaultGODEBUG`, the two surfaces the toolchain embeds.

### Fail-closed parsing and the policy check

`checkProvenance` is pure: it takes the settings slice and a policy and returns
nil or a joined error. It is deliberately fail-closed — an absent, empty, `off`,
or unrecognized value never reads as compliant. `RequireGOFIPS140` demands a
non-empty, non-`off` `GOFIPS140` setting; `RequireFIPSGODEBUG` parses the recorded
`DefaultGODEBUG` and demands `fips140=on` or `fips140=only`. Anything else — a
missing directive, `fips140=off`, or a typo'd value — is a failure. The same logic
serves both entry points: `FromBuildInfo` for a program checking itself, and
`Verify` for CI checking piped `go version -m` output from another artifact.

The `DefaultGODEBUG` value is a comma-separated `key=value` list (for example
`fips140=on,tlsmlkem=0`), so `parseGODEBUG` splits on commas and then on the first
`=` of each item — which is why a value like `DefaultGODEBUG=fips140=on` cuts
correctly into key `DefaultGODEBUG` at the outer layer and `fips140`/`on` at the
inner.

Create `fipsprovenance.go`:

```go
package fipsprovenance

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"runtime/debug"
	"strings"
)

// Sentinel errors so a CI gate can report exactly why an artifact was rejected.
var (
	ErrNoGOFIPS140      = errors.New("provenance: binary not built with GOFIPS140")
	ErrFIPSGODEBUGNotOn = errors.New("provenance: recorded GODEBUG does not enable fips140")
)

// Policy declares what the CI gate requires of an artifact's provenance.
type Policy struct {
	RequireGOFIPS140   bool // require a non-empty, non-"off" GOFIPS140 build setting
	RequireFIPSGODEBUG bool // require the recorded DefaultGODEBUG to set fips140=on or =only
}

// checkProvenance is pure: it maps embedded build settings and a policy to nil or
// a joined error. It fails closed: absent, empty, "off", or unrecognized values
// are never treated as compliant.
func checkProvenance(settings []debug.BuildSetting, policy Policy) error {
	m := make(map[string]string, len(settings))
	for _, s := range settings {
		m[s.Key] = s.Value
	}

	var errs []error
	if policy.RequireGOFIPS140 {
		if v := m["GOFIPS140"]; v == "" || v == "off" {
			errs = append(errs, fmt.Errorf("%w: GOFIPS140=%q", ErrNoGOFIPS140, v))
		}
	}
	if policy.RequireFIPSGODEBUG {
		god := parseGODEBUG(m["DefaultGODEBUG"])
		switch god["fips140"] {
		case "on", "only":
			// approved
		default:
			errs = append(errs, fmt.Errorf("%w: DefaultGODEBUG=%q",
				ErrFIPSGODEBUGNotOn, m["DefaultGODEBUG"]))
		}
	}
	return errors.Join(errs...)
}

// parseGODEBUG parses a comma-separated key=value GODEBUG string into a map.
func parseGODEBUG(v string) map[string]string {
	out := make(map[string]string)
	for _, item := range strings.Split(v, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if k, val, ok := strings.Cut(item, "="); ok {
			out[k] = val
		}
	}
	return out
}

// parseVersionM parses the output of `go version -m <binary>` and returns the
// embedded build settings. Each build line has the form "\tbuild\tKey=Value".
func parseVersionM(r io.Reader) []debug.BuildSetting {
	var settings []debug.BuildSetting
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.SplitN(strings.TrimSpace(sc.Text()), "\t", 2)
		if len(fields) != 2 || fields[0] != "build" {
			continue
		}
		if k, v, ok := strings.Cut(fields[1], "="); ok {
			settings = append(settings, debug.BuildSetting{Key: k, Value: v})
		}
	}
	return settings
}

// Verify gates an artifact from piped `go version -m` output:
//
//	go version -m ./artifact | fipsverify
//
// It returns nil only if the parsed settings satisfy policy.
func Verify(r io.Reader, policy Policy) error {
	return checkProvenance(parseVersionM(r), policy)
}

// FromBuildInfo returns the running binary's embedded build settings, for a
// program that checks its own provenance.
func FromBuildInfo() ([]debug.BuildSetting, bool) {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return nil, false
	}
	return bi.Settings, true
}
```

### The runnable demo

The demo reads its own build settings via `FromBuildInfo`, prints the two settings
the gate cares about, and applies a FIPS-required policy to itself. Because the
demo is built plainly (no `GOFIPS140`, no `godebug` directive), both settings are
absent and the verdict is a fail-closed rejection — exactly what CI should do to a
non-FIPS artifact.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/fipsprovenance"
)

func main() {
	settings, ok := fipsprovenance.FromBuildInfo()
	if !ok {
		fmt.Println("no build info available")
		return
	}

	get := func(key string) string {
		for _, s := range settings {
			if s.Key == key {
				return s.Value
			}
		}
		return "(absent)"
	}
	fmt.Printf("GOFIPS140      = %s\n", get("GOFIPS140"))
	fmt.Printf("DefaultGODEBUG = %s\n", get("DefaultGODEBUG"))

	policy := fipsprovenance.Policy{RequireGOFIPS140: true, RequireFIPSGODEBUG: true}
	if err := fipsprovenance.Verify(readerFor(settings), policy); err != nil {
		fmt.Println("verdict: REJECT (not a FIPS build)")
		return
	}
	fmt.Println("verdict: ACCEPT")
}
```

The demo needs a small adapter to feed the in-process settings through the same
`Verify` path CI uses; append it to the demo file. It renders the settings back
into the `go version -m` line format so a single code path handles both the
piped-CLI case and the self-check.

Create `cmd/demo/reader.go`:

```go
package main

import (
	"io"
	"runtime/debug"
	"strings"
)

func readerFor(settings []debug.BuildSetting) io.Reader {
	var b strings.Builder
	for _, s := range settings {
		b.WriteString("\tbuild\t")
		b.WriteString(s.Key)
		b.WriteString("=")
		b.WriteString(s.Value)
		b.WriteString("\n")
	}
	return strings.NewReader(b.String())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (a plain, non-FIPS build):

```
GOFIPS140      = (absent)
DefaultGODEBUG = (absent)
verdict: REJECT (not a FIPS build)
```

A FIPS-built artifact — `GOFIPS140=v1.0.0 go build ...` — would instead show
`GOFIPS140 = v1.0.0` and `DefaultGODEBUG = fips140=on,...`, and the verdict would
be `ACCEPT`. In CI you run the same check across the wire with
`go version -m ./artifact | fipsverify`.

### Tests

`TestCheckProvenance` is a pure table: a full FIPS build passes, a missing
`GOFIPS140` fails, `fips140=off` fails, and empty/unknown settings fail closed,
each asserting the sentinel with `errors.Is`. `TestParseVersionM` feeds real
`go version -m` output (tab-separated build lines) through `Verify` and confirms
the parser extracts `GOFIPS140` and `DefaultGODEBUG` and reaches the right verdict —
proving the CI wire format is handled, not just synthetic slices.

Create `fipsprovenance_test.go`:

```go
package fipsprovenance

import (
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"testing"
)

func TestCheckProvenance(t *testing.T) {
	t.Parallel()

	policy := Policy{RequireGOFIPS140: true, RequireFIPSGODEBUG: true}
	tests := []struct {
		name     string
		settings []debug.BuildSetting
		want     error
	}{
		{
			name: "full fips build passes",
			settings: []debug.BuildSetting{
				{Key: "GOFIPS140", Value: "v1.0.0"},
				{Key: "DefaultGODEBUG", Value: "fips140=on,tlsmlkem=0"},
			},
			want: nil,
		},
		{
			name:     "missing gofips140 fails",
			settings: []debug.BuildSetting{{Key: "DefaultGODEBUG", Value: "fips140=on"}},
			want:     ErrNoGOFIPS140,
		},
		{
			name: "fips140 off in godebug fails",
			settings: []debug.BuildSetting{
				{Key: "GOFIPS140", Value: "v1.0.0"},
				{Key: "DefaultGODEBUG", Value: "fips140=off"},
			},
			want: ErrFIPSGODEBUGNotOn,
		},
		{
			name:     "empty settings fail closed",
			settings: nil,
			want:     ErrNoGOFIPS140,
		},
		{
			name: "unknown gofips140 off value fails",
			settings: []debug.BuildSetting{
				{Key: "GOFIPS140", Value: "off"},
				{Key: "DefaultGODEBUG", Value: "fips140=on"},
			},
			want: ErrNoGOFIPS140,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := checkProvenance(tc.settings, policy)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("checkProvenance() = %v; want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("checkProvenance() = %v; want errors.Is(_, %v)", err, tc.want)
			}
		})
	}
}

func TestParseVersionM(t *testing.T) {
	t.Parallel()

	// Real `go version -m` output shape: a header line, then tab-indented
	// "build\tKey=Value" lines.
	const output = "/srv/app: go1.26.0\n" +
		"\tpath\texample.com/app\n" +
		"\tmod\texample.com/app\t(devel)\n" +
		"\tbuild\t-buildmode=exe\n" +
		"\tbuild\tGOARCH=amd64\n" +
		"\tbuild\tGOFIPS140=v1.0.0\n" +
		"\tbuild\tDefaultGODEBUG=fips140=on,tlsmlkem=0\n" +
		"\tbuild\tCGO_ENABLED=0\n"

	settings := parseVersionM(strings.NewReader(output))
	got := make(map[string]string)
	for _, s := range settings {
		got[s.Key] = s.Value
	}
	if got["GOFIPS140"] != "v1.0.0" {
		t.Fatalf("GOFIPS140 = %q; want v1.0.0", got["GOFIPS140"])
	}
	if got["DefaultGODEBUG"] != "fips140=on,tlsmlkem=0" {
		t.Fatalf("DefaultGODEBUG = %q; want fips140=on,tlsmlkem=0", got["DefaultGODEBUG"])
	}

	if err := Verify(strings.NewReader(output), Policy{RequireGOFIPS140: true, RequireFIPSGODEBUG: true}); err != nil {
		t.Fatalf("Verify() = %v; want nil for a FIPS-built artifact", err)
	}
}

func Example() {
	settings := []debug.BuildSetting{
		{Key: "GOFIPS140", Value: "v1.0.0"},
		{Key: "DefaultGODEBUG", Value: "fips140=on"},
	}
	err := checkProvenance(settings, Policy{RequireGOFIPS140: true, RequireFIPSGODEBUG: true})
	fmt.Println(err)
	// Output: <nil>
}
```

## Review

The gate is correct when it fails closed: a missing `GOFIPS140`, a `fips140=off`
default, an empty settings slice, or an unrecognized value must all be rejected,
and only a genuine `GOFIPS140` plus `fips140=on`/`only` passes. The parser must
handle the real `go version -m` shape — tab-separated `build` lines, values that
themselves contain `=` — which is why `parseVersionM` cuts on the first `=` and
`parseGODEBUG` cuts each comma-separated item the same way.

The mistakes to avoid: do not verify FIPS status with a comment or a runbook —
read the embedded `GOFIPS140` and `DefaultGODEBUG` from the artifact, which is the
only thing that survives a rebuild by someone who forgot the flag. Do not accept
the runtime `GODEBUG=fips140=on` env var as evidence; it configures a process, not
a binary, and leaves no trace to attest. And do not let an unknown value read as
compliant — a typo in a `//go:debug` directive should reject the artifact, not
wave it through. Confirm the wire path by piping `go version -m` from a real build
into `Verify`, and confirm the self-check path by running the demo, which rejects
its own non-FIPS build.

## Resources

- [`runtime/debug.ReadBuildInfo`](https://pkg.go.dev/runtime/debug#ReadBuildInfo) — the `BuildInfo` and `BuildSetting` types that carry `GOFIPS140` and `DefaultGODEBUG`.
- [FIPS 140-3 Compliance](https://go.dev/doc/security/fips140) — how `GOFIPS140`, the `go.mod` `godebug` directive, and `//go:debug` are recorded.
- [`go version` command](https://pkg.go.dev/cmd/go#hdr-Print_Go_version) — `go version -m` prints the embedded module and build settings of a binary.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../09-govulncheck-in-ci/00-concepts.md](../09-govulncheck-in-ci/00-concepts.md)
