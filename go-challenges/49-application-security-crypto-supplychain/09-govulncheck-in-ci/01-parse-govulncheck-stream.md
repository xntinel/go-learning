# Exercise 1: Parse the govulncheck JSON stream into typed findings

govulncheck's `-json` output is the only stable machine contract it offers, and it
is a *stream* of single-key envelope objects, not one document. This exercise
builds a parser that consumes that stream, materializes it into typed Go structs,
and aggregates it into the numbers a gate cares about: the effective scan level,
how many advisories versus how many raw findings, and — critically — the
deduplicated per-OSV view. govulncheck emits several findings per vulnerability
(one module-level, one package-level, one or more symbol-level), so the parser
must group findings by OSV id and reduce each group to a single vulnerability at
the maximum granularity it reached before anything downstream counts or triages
it.

This module is fully self-contained: its own `go mod init`, all types inline, a
captured scan embedded as a Go constant so no external testdata file is needed,
and its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
vulnstream/                  independent module: example.com/vulnstream
  go.mod                     go 1.26
  vulnstream.go              Message/Config/Finding/Frame types; Parse; Report; Granularity; Vulnerability
  cmd/
    demo/
      main.go                parses an embedded captured scan and prints the summary
  vulnstream_test.go         golden-fixture decode + per-OSV dedup + granularity; Example
```

- Files: `vulnstream.go`, `cmd/demo/main.go`, `vulnstream_test.go`.
- Implement: `Parse(r io.Reader) (*Report, error)` that loops a `json.Decoder` until `io.EOF`; typed `Config`, `OSVEntry`, `Finding`, `Frame`; `Finding.Granularity()` and `Finding.Reachable()`; `Report.Vulnerabilities()` (group raw findings by OSV, take the max granularity) and `Report.Summary()`.
- Test: decode a captured stream and assert the parsed `scan_level`, the count of distinct OSV entries, the raw finding count, and that grouping by OSV collapses the three findings of one reachable advisory into a single symbol-level (reachable) vulnerability while a module-only advisory reduces to one module-level (unreachable) vulnerability.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/49-application-security-crypto-supplychain/09-govulncheck-in-ci/01-parse-govulncheck-stream/cmd/demo
cd go-solutions/49-application-security-crypto-supplychain/09-govulncheck-in-ci/01-parse-govulncheck-stream
go mod edit -go=1.26
```

### Why streaming, and why our own structs

Two design constraints drive this parser. First, the output is newline-delimited
envelopes, so a single `json.Unmarshal` over the whole buffer fails the moment it
finishes the first object and finds more data. The correct shape is a
`json.Decoder` in a loop: `Decode` reads exactly one JSON value per call and
advances, and it returns `io.EOF` when the stream is exhausted. You terminate on
`errors.Is(err, io.EOF)` and treat any other error as a real decode failure.

Second, govulncheck's message types live under `golang.org/x/vuln/internal` and
cannot be imported. So we define structs that match the documented JSON schema.
The envelope is a `Message` with one pointer field per possible key
(`config`, `progress`, `osv`, `finding`, and `SBOM`); because every field is a
pointer with `omitempty` on the wire, exactly one is non-nil per decoded message,
and a `switch` on which one is set routes it. Pinning `protocol_version` from the
config message is how you would detect a breaking schema change rather than
silently misparsing.

### What "granularity" means in the trace

A `finding` carries a `trace`: a slice of frames ordered from the vulnerable
symbol (frame 0) up to your entry point. The shape of frame 0 encodes the
granularity govulncheck achieved for that finding:

- frame 0 has a non-empty `function` -> **symbol-level**: your code calls the
  vulnerable function. This is the reachable, block-worthy case.
- frame 0 has a `package` but no `function` -> **package-level**: the vulnerable
  package is imported, but no specific call was resolved.
- frame 0 has only `module` (and maybe `version`) -> **module-level**: the
  vulnerable module is present but neither imported-package nor called-symbol was
  resolved. This is the imported-but-unreachable advisory a gate should report,
  not block on.

`Finding.Granularity()` inspects frame 0 and returns one of these; `Reachable()`
is true only at symbol granularity. Keeping this a pure method over the parsed
data is what lets the next exercise's gate stay a pure function.

### One OSV, many findings: the dedup step

govulncheck emits findings as it works, least-precise first, so a single
vulnerability normally produces several: a module-level finding when it sees the
vulnerable module is required, a package-level finding when it sees the package is
imported, and one or more symbol-level findings when it resolves a real call. A
single *reachable* advisory therefore appears as roughly three findings, only one
of which is symbol-level. Counting raw findings would triple-count it, and triaging
each finding independently would file the same OSV as both blocking (its symbol
finding) and informational (its module finding).

So `Report.Vulnerabilities()` groups the raw findings by OSV id and reduces each
group to a single `Vulnerability` carrying the *maximum* granularity any finding in
the group reached — symbol if any finding was symbol-level, else package, else
module. That deduplicated, one-per-OSV slice is what the demo prints and what a
gate triages; the raw `Findings` slice is kept only for the honest raw count and
for callers that want the full trace detail. `Summary()` reports both: the raw
`findings` count and the deduplicated `vulnerabilities` and `reachable` counts, so
the inflation is visible rather than hidden.

Create `vulnstream.go`:

```go
package vulnstream

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
)

// ErrNoConfig is returned when a stream contains no config envelope. govulncheck
// always emits config first, so its absence means the input is not a real scan.
var ErrNoConfig = errors.New("vulnstream: stream contained no config message")

// Message is one envelope in the govulncheck -json stream. Exactly one field is
// populated per decoded object; a switch on which is non-nil routes it. The types
// mirror the documented JSON schema because govulncheck's own structs live under
// golang.org/x/vuln/internal and cannot be imported.
type Message struct {
	Config   *Config         `json:"config,omitempty"`
	Progress *Progress       `json:"progress,omitempty"`
	OSV      *OSVEntry       `json:"osv,omitempty"`
	Finding  *Finding        `json:"finding,omitempty"`
	SBOM     json.RawMessage `json:"SBOM,omitempty"`
}

// Config is emitted first and records how the scan was performed.
type Config struct {
	ProtocolVersion string `json:"protocol_version"`
	ScannerName     string `json:"scanner_name"`
	ScannerVersion  string `json:"scanner_version"`
	DB              string `json:"db"`
	DBLastModified  string `json:"db_last_modified"`
	GoVersion       string `json:"go_version"`
	ScanLevel       string `json:"scan_level"`
	ScanMode        string `json:"scan_mode"`
}

// Progress is a human-readable status line; the parser records but ignores it.
type Progress struct {
	Message string `json:"message"`
}

// OSVEntry is an advisory the database considered applicable. It is emitted for
// every relevant advisory, whether or not a matching finding follows.
type OSVEntry struct {
	ID      string `json:"id"`
	Summary string `json:"summary"`
}

// Finding is an actual hit: an OSV id, the version that fixes it, and a trace of
// frames from the vulnerable symbol (frame 0) up to the program entry point.
type Finding struct {
	OSV          string  `json:"osv"`
	FixedVersion string  `json:"fixed_version"`
	Trace        []Frame `json:"trace"`
}

// Frame is one step in a finding's call trace. Its resolved depth (module only,
// or module+package, or module+package+function) encodes the achieved granularity.
type Frame struct {
	Module   string    `json:"module"`
	Version  string    `json:"version"`
	Package  string    `json:"package"`
	Function string    `json:"function"`
	Receiver string    `json:"receiver"`
	Position *Position `json:"position"`
}

// Position locates a frame in source.
type Position struct {
	Filename string `json:"filename"`
	Offset   int    `json:"offset"`
	Line     int    `json:"line"`
	Column   int    `json:"column"`
}

// Granularity is the precision ladder govulncheck resolves a finding to.
type Granularity int

const (
	GranularityModule Granularity = iota
	GranularityPackage
	GranularitySymbol
)

func (g Granularity) String() string {
	switch g {
	case GranularitySymbol:
		return "symbol"
	case GranularityPackage:
		return "package"
	default:
		return "module"
	}
}

// Granularity reports how precisely this finding was resolved, by inspecting the
// leaf frame (frame 0): a function means symbol-level, a package with no function
// means package-level, and module-only means module-level.
func (f *Finding) Granularity() Granularity {
	if len(f.Trace) == 0 {
		return GranularityModule
	}
	leaf := f.Trace[0]
	switch {
	case leaf.Function != "":
		return GranularitySymbol
	case leaf.Package != "":
		return GranularityPackage
	default:
		return GranularityModule
	}
}

// Reachable reports whether the finding is symbol-level: your code calls the
// vulnerable function. Only symbol-level findings are treated as block-worthy.
func (f *Finding) Reachable() bool {
	return f.Granularity() == GranularitySymbol
}

// Report is the parsed and aggregated result of a scan.
type Report struct {
	Config   Config
	OSVs     []OSVEntry
	Findings []Finding
}

// Parse decodes a govulncheck -json stream. The stream is a concatenation of
// single-key envelope objects, so it decodes one message at a time with a
// json.Decoder until io.EOF rather than a single Unmarshal.
func Parse(r io.Reader) (*Report, error) {
	dec := json.NewDecoder(r)
	var rep Report
	sawConfig := false
	for {
		var m Message
		if err := dec.Decode(&m); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("vulnstream: decode: %w", err)
		}
		switch {
		case m.Config != nil:
			rep.Config = *m.Config
			sawConfig = true
		case m.OSV != nil:
			rep.OSVs = append(rep.OSVs, *m.OSV)
		case m.Finding != nil:
			rep.Findings = append(rep.Findings, *m.Finding)
		}
	}
	if !sawConfig {
		return nil, ErrNoConfig
	}
	return &rep, nil
}

// Vulnerability is the deduplicated view of one advisory: every raw finding that
// shares an OSV id, collapsed into a single entry carrying the highest granularity
// any of them reached. govulncheck emits multiple findings per OSV (module,
// package, and symbol level), so a gate must group by OSV and take the maximum
// granularity before counting or triaging; skipping this triple-counts a reachable
// vulnerability and can place one OSV in two triage buckets at once.
type Vulnerability struct {
	OSV            string
	FixedVersion   string
	MaxGranularity Granularity
}

// Reachable reports whether any finding for this OSV reached symbol level.
func (v Vulnerability) Reachable() bool {
	return v.MaxGranularity == GranularitySymbol
}

// Vulnerabilities groups the raw findings by OSV id and keeps, per OSV, the
// highest granularity resolved and a fixed version. This one-per-OSV view is what
// a gate triages: an entry is reachable iff some finding for that OSV was
// symbol-level. Results are sorted by OSV id for deterministic output.
func (r *Report) Vulnerabilities() []Vulnerability {
	byOSV := make(map[string]Vulnerability, len(r.Findings))
	for _, f := range r.Findings {
		g := f.Granularity()
		v, ok := byOSV[f.OSV]
		if !ok {
			v = Vulnerability{OSV: f.OSV}
		}
		if !ok || g > v.MaxGranularity {
			v.MaxGranularity = g
		}
		if v.FixedVersion == "" && f.FixedVersion != "" {
			v.FixedVersion = f.FixedVersion
		}
		byOSV[f.OSV] = v
	}
	out := make([]Vulnerability, 0, len(byOSV))
	for _, v := range byOSV {
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b Vulnerability) int { return cmp.Compare(a.OSV, b.OSV) })
	return out
}

// Reachable returns the deduplicated vulnerabilities resolved to symbol level.
func (r *Report) Reachable() []Vulnerability {
	var out []Vulnerability
	for _, v := range r.Vulnerabilities() {
		if v.Reachable() {
			out = append(out, v)
		}
	}
	return out
}

// Summary renders the one-line aggregate a CI log would print. It reports both the
// raw finding count and the deduplicated per-OSV counts, so the multiplicity is
// visible rather than inflating the vulnerability total.
func (r *Report) Summary() string {
	vulns := r.Vulnerabilities()
	return fmt.Sprintf("scan_level=%s advisories=%d findings=%d vulnerabilities=%d reachable=%d",
		r.Config.ScanLevel, len(r.OSVs), len(r.Findings), len(vulns), len(r.Reachable()))
}
```

### The runnable demo

The demo parses a small captured scan embedded as a constant, so it runs with no
network and no toolchain. It prints the aggregate summary and then one line per
*deduplicated vulnerability* — OSV id, maximum resolved granularity, and whether it
is reachable — which is exactly the split a triage report is built from. Note that
the summary's raw `findings=4` collapses to `vulnerabilities=2`, `reachable=1`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"strings"

	"example.com/vulnstream"
)

// A trimmed but real-shaped govulncheck -json capture. It preserves the real
// multiplicity: the reachable advisory GO-2024-2687 carries three findings emitted
// least-precise first (module, then package, then symbol), exactly as govulncheck
// emits them as it works; the unreachable GO-2023-1988 carries a single
// module-level finding (the module is required but never imported).
const capture = `{"config":{"protocol_version":"v1.0.0","scanner_name":"govulncheck","scanner_version":"v1.1.4","db":"https://vuln.go.dev","db_last_modified":"2025-05-15T18:00:00Z","go_version":"go1.26.0","scan_level":"symbol","scan_mode":"source"}}
{"progress":{"message":"Scanning your code and 312 packages across 47 dependent modules for known vulnerabilities..."}}
{"osv":{"id":"GO-2024-2687","summary":"HTTP/2 CONTINUATION flood in golang.org/x/net"}}
{"osv":{"id":"GO-2023-1988","summary":"Denial of service in gopkg.in/yaml.v2"}}
{"finding":{"osv":"GO-2024-2687","fixed_version":"v0.23.0","trace":[{"module":"golang.org/x/net","version":"v0.17.0"}]}}
{"finding":{"osv":"GO-2024-2687","fixed_version":"v0.23.0","trace":[{"module":"golang.org/x/net","version":"v0.17.0","package":"golang.org/x/net/http2"}]}}
{"finding":{"osv":"GO-2024-2687","fixed_version":"v0.23.0","trace":[{"module":"golang.org/x/net","version":"v0.17.0","package":"golang.org/x/net/http2","function":"processHeaders","receiver":"*serverConn","position":{"filename":"server.go","offset":142,"line":2201,"column":21}},{"module":"example.com/app","package":"example.com/app","function":"main","position":{"filename":"main.go","offset":50,"line":18,"column":3}}]}}
{"finding":{"osv":"GO-2023-1988","fixed_version":"v2.4.0","trace":[{"module":"gopkg.in/yaml.v2","version":"v2.2.2"}]}}`

func main() {
	rep, err := vulnstream.Parse(strings.NewReader(capture))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(rep.Summary())
	for _, v := range rep.Vulnerabilities() {
		fmt.Printf("%s %s reachable=%t fix=%s\n", v.OSV, v.MaxGranularity, v.Reachable(), v.FixedVersion)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
scan_level=symbol advisories=2 findings=4 vulnerabilities=2 reachable=1
GO-2023-1988 module reachable=false fix=v2.4.0
GO-2024-2687 symbol reachable=true fix=v0.23.0
```

### Tests

The test decodes the same captured stream and asserts the load-bearing facts: the
parsed `scan_level`, the count of distinct OSV entries, the raw finding count, and
the dedup — the three findings of `GO-2024-2687` must collapse into one
symbol-level, reachable vulnerability, and the single `GO-2023-1988` finding into
one module-level, unreachable one. `TestVulnerabilitiesDedup` asserts the grouping
directly, which is the whole point of the exercise. `TestParseNoConfig` feeds a
stream with no config envelope and asserts the `ErrNoConfig` sentinel via
`errors.Is`, proving the parser rejects input that is not a real scan. The
`Example` pins the summary line.

Create `vulnstream_test.go`:

```go
package vulnstream

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

const sampleStream = `{"config":{"protocol_version":"v1.0.0","scanner_name":"govulncheck","scanner_version":"v1.1.4","db":"https://vuln.go.dev","db_last_modified":"2025-05-15T18:00:00Z","go_version":"go1.26.0","scan_level":"symbol","scan_mode":"source"}}
{"progress":{"message":"Scanning your code..."}}
{"osv":{"id":"GO-2024-2687","summary":"HTTP/2 CONTINUATION flood in golang.org/x/net"}}
{"osv":{"id":"GO-2023-1988","summary":"Denial of service in gopkg.in/yaml.v2"}}
{"finding":{"osv":"GO-2024-2687","fixed_version":"v0.23.0","trace":[{"module":"golang.org/x/net","version":"v0.17.0"}]}}
{"finding":{"osv":"GO-2024-2687","fixed_version":"v0.23.0","trace":[{"module":"golang.org/x/net","version":"v0.17.0","package":"golang.org/x/net/http2"}]}}
{"finding":{"osv":"GO-2024-2687","fixed_version":"v0.23.0","trace":[{"module":"golang.org/x/net","version":"v0.17.0","package":"golang.org/x/net/http2","function":"processHeaders","receiver":"*serverConn","position":{"filename":"server.go","offset":142,"line":2201,"column":21}},{"module":"example.com/app","package":"example.com/app","function":"main"}]}}
{"finding":{"osv":"GO-2023-1988","fixed_version":"v2.4.0","trace":[{"module":"gopkg.in/yaml.v2","version":"v2.2.2"}]}}`

func TestParseAggregates(t *testing.T) {
	t.Parallel()
	rep, err := Parse(strings.NewReader(sampleStream))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	if rep.Config.ScanLevel != "symbol" {
		t.Errorf("scan_level = %q; want symbol", rep.Config.ScanLevel)
	}
	if rep.Config.ProtocolVersion != "v1.0.0" {
		t.Errorf("protocol_version = %q; want v1.0.0", rep.Config.ProtocolVersion)
	}
	if got := len(rep.OSVs); got != 2 {
		t.Errorf("distinct OSV entries = %d; want 2", got)
	}
	if got := len(rep.Findings); got != 4 {
		t.Errorf("raw findings = %d; want 4", got)
	}
	if got := len(rep.Vulnerabilities()); got != 2 {
		t.Errorf("deduplicated vulnerabilities = %d; want 2", got)
	}
	if got := len(rep.Reachable()); got != 1 {
		t.Errorf("reachable vulnerabilities = %d; want 1", got)
	}
}

func TestVulnerabilitiesDedup(t *testing.T) {
	t.Parallel()
	rep, err := Parse(strings.NewReader(sampleStream))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	byOSV := make(map[string]Vulnerability, len(rep.Vulnerabilities()))
	for _, v := range rep.Vulnerabilities() {
		byOSV[v.OSV] = v
	}

	tests := []struct {
		osv       string
		want      Granularity
		reachable bool
	}{
		{osv: "GO-2024-2687", want: GranularitySymbol, reachable: true},
		{osv: "GO-2023-1988", want: GranularityModule, reachable: false},
	}
	for _, tc := range tests {
		t.Run(tc.osv, func(t *testing.T) {
			t.Parallel()
			v, ok := byOSV[tc.osv]
			if !ok {
				t.Fatalf("no vulnerability for %s", tc.osv)
			}
			if got := v.MaxGranularity; got != tc.want {
				t.Errorf("MaxGranularity = %s; want %s", got, tc.want)
			}
			if got := v.Reachable(); got != tc.reachable {
				t.Errorf("Reachable() = %v; want %v", got, tc.reachable)
			}
		})
	}
}

func TestParseNoConfig(t *testing.T) {
	t.Parallel()
	const noConfig = `{"osv":{"id":"GO-2024-2687"}}`
	if _, err := Parse(strings.NewReader(noConfig)); !errors.Is(err, ErrNoConfig) {
		t.Fatalf("Parse() error = %v; want errors.Is(_, ErrNoConfig)", err)
	}
}

func Example() {
	rep, _ := Parse(strings.NewReader(sampleStream))
	fmt.Println(rep.Summary())
	// Output: scan_level=symbol advisories=2 findings=4 vulnerabilities=2 reachable=1
}
```

## Review

The parser is correct when it decodes the stream one envelope at a time and never
tries a single `Unmarshal`: govulncheck concatenates objects, so anything but a
`json.Decoder` loop breaks after the first message. The `io.EOF` check is the loop
terminator, and any non-EOF error is a genuine failure to surface, not swallow.
Granularity is a pure function of frame 0: a function field means the vulnerable
symbol is called (reachable, block-worthy), a package-only frame means imported,
and a module-only frame means merely present — the exact distinction the gate in
Exercise 2 turns into a build decision.

The mistakes to avoid: do not import `golang.org/x/vuln/internal` types — define
structs against the documented schema and pin `protocol_version` so a schema break
is visible. Do not confuse the count of `osv` advisories (every applicable advisory
in the DB) with the count of raw `finding` hits, and do not confuse either with the
count of distinct vulnerabilities: govulncheck emits a module-, package-, and
symbol-level finding for the same reachable OSV, so `Vulnerabilities()` must group
by OSV and take the maximum granularity before anything downstream counts or
triages — otherwise one reachable CVE is triple-counted and can be classified both
reachable and unreachable at once. Confirm correctness by running the demo: the
summary must read `findings=4 vulnerabilities=2 reachable=1`, with `GO-2024-2687`
collapsed to a single `symbol` (reachable) entry and the yaml advisory classified
`module`, not `symbol`.

## Resources

- [govulncheck command (JSON output, flags, exit codes)](https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck) — the documented output schema and the exit-code contract.
- [`encoding/json.Decoder`](https://pkg.go.dev/encoding/json#Decoder) — `NewDecoder` and `Decode`, the streaming primitives for a concatenated-object stream.
- [Go vulnerability management](https://go.dev/security/vuln/) — the database, OSV format, and the reachability model behind findings.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-reachability-triage-gate.md](02-reachability-triage-gate.md)
