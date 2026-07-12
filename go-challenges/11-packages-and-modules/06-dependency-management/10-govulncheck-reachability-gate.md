# Exercise 10: Security gate — fail CI only on reachable vulnerabilities

`govulncheck` does not just list vulnerabilities you import — it performs
reachability analysis and tells you which vulnerable *symbols* your code actually
calls. A gate that fails on every import-level finding becomes noise and gets muted.
This exercise builds the mature version: consume `govulncheck -json` and fail the
build only on a *reachable* finding, offline against a captured JSON stream.

This module is fully self-contained. It has its own `go mod init`, its own demo,
and its own tests, and imports nothing from the other exercises.

## What you'll build

```text
vulngate/                  independent module: example.com/vulngate
  go.mod                   go 1.26 (standard library only)
  vulngate.go              Gate(r io.Reader) ([]Reachable, error) over govulncheck -json
  cmd/
    demo/
      main.go              runs the gate over an embedded JSON stream fixture
  vulngate_test.go         ignores an unreachable OSV; fails naming the reachable one
```

- Files: `vulngate.go`, `cmd/demo/main.go`, `vulngate_test.go`.
- Implement: `Gate` that streams the govulncheck JSON with a `json.Decoder`, collects OSV summaries, and returns only findings whose top trace frame names a called function (reachable).
- Test: a stream with an imported-but-unreachable OSV and a reachable finding with a call trace; assert the gate ignores the former and returns the latter with its OSV id and package.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/11-packages-and-modules/06-dependency-management/10-govulncheck-reachability-gate/cmd/demo
cd go-solutions/11-packages-and-modules/06-dependency-management/10-govulncheck-reachability-gate
```

### The govulncheck -json stream and reachability

`govulncheck -json` emits a *stream* of JSON objects, one per line-ish message, each
an object with exactly one of these keys populated: `config`, `progress`, `osv`, or
`finding`. An `osv` message carries the vulnerability record (its `id` and
`summary`). A `finding` message reports one way a vulnerability touches your build,
and govulncheck emits findings at escalating precision for the same OSV: a
module-level finding, a package-level finding, and — only if the vulnerable code is
actually called — a symbol-level finding. The distinguishing field is the trace: a
`finding`'s `trace` array is ordered from the vulnerable symbol outward, and the top
frame (`trace[0]`) has a non-empty `function` *only* for a reachable, called
finding. An import-level finding's top frame names a module and package but no
function.

So the reachability rule is exactly: a finding is reachable when
`len(trace) > 0 && trace[0].function != ""`. The gate decodes the stream, remembers
each OSV's summary, and fails only on OSVs that have at least one reachable finding —
reporting the OSV id and the affected package so the on-call engineer can act
(upgrade to a fixed version, `exclude` the bad version, or vendor a patch).

Stream the objects with `json.Decoder` and its `More`/`Decode` loop rather than
unmarshalling the whole input at once (a real scan can be large). Decode each
message into a struct with pointer fields so an absent key stays `nil`, and keep the
raw OSV payload as `json.RawMessage` to unmarshal its id and summary lazily.

Create `vulngate.go`:

```go
package vulngate

import (
	"encoding/json"
	"fmt"
	"io"
)

// message is one object in the govulncheck -json stream. Exactly one field is set.
type message struct {
	OSV     json.RawMessage `json:"osv"`
	Finding *finding        `json:"finding"`
}

// osvRecord is the part of an OSV entry the gate needs.
type osvRecord struct {
	ID      string `json:"id"`
	Summary string `json:"summary"`
}

// finding is one govulncheck finding: a path by which an OSV reaches the build.
type finding struct {
	OSV   string  `json:"osv"`
	Trace []frame `json:"trace"`
}

// frame is one entry in a finding's call trace, ordered from the vulnerable symbol
// outward. Function is non-empty only for a reachable (called) finding.
type frame struct {
	Module   string `json:"module"`
	Package  string `json:"package"`
	Function string `json:"function"`
}

// Reachable is a vulnerability whose vulnerable symbol is called by the code.
type Reachable struct {
	OSV     string
	Package string
	Summary string
}

// Gate reads a govulncheck -json stream and returns the reachable vulnerabilities:
// those with at least one finding whose top trace frame names a called function.
// Imported-but-unreachable vulnerabilities are ignored. A non-empty result is a
// CI failure.
func Gate(r io.Reader) ([]Reachable, error) {
	dec := json.NewDecoder(r)
	summaries := make(map[string]string) // OSV id -> summary
	seen := make(map[string]bool)        // OSV id already reported reachable
	var out []Reachable

	for dec.More() {
		var m message
		if err := dec.Decode(&m); err != nil {
			return nil, fmt.Errorf("decode govulncheck stream: %w", err)
		}
		if len(m.OSV) > 0 {
			var rec osvRecord
			if err := json.Unmarshal(m.OSV, &rec); err != nil {
				return nil, fmt.Errorf("decode osv: %w", err)
			}
			if rec.ID != "" {
				summaries[rec.ID] = rec.Summary
			}
		}
		if m.Finding != nil && isReachable(m.Finding) && !seen[m.Finding.OSV] {
			seen[m.Finding.OSV] = true
			out = append(out, Reachable{
				OSV:     m.Finding.OSV,
				Package: m.Finding.Trace[0].Package,
				Summary: summaries[m.Finding.OSV],
			})
		}
	}
	return out, nil
}

// isReachable reports whether a finding's top trace frame names a called function.
func isReachable(f *finding) bool {
	return len(f.Trace) > 0 && f.Trace[0].Function != ""
}
```

### The runnable demo

The embedded fixture is a trimmed but shape-accurate `govulncheck -json` stream: two
OSV records, an import-level finding for the first (no function in its top frame), and
a symbol-level finding for the second (with a called function). Only the second is
reachable.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"

	"example.com/vulngate"
)

const stream = `{"osv":{"id":"GO-2024-0001","summary":"DoS in example/parser"}}
{"osv":{"id":"GO-2024-0002","summary":"Auth bypass in example/jwt"}}
{"finding":{"osv":"GO-2024-0001","trace":[{"module":"example.com/parser","package":"example.com/parser"}]}}
{"finding":{"osv":"GO-2024-0002","trace":[{"module":"example.com/jwt","package":"example.com/jwt","function":"Verify"},{"module":"example.com/app","package":"example.com/app","function":"main"}]}}
`

func main() {
	reachable, err := vulngate.Gate(strings.NewReader(stream))
	if err != nil {
		panic(err)
	}
	fmt.Printf("reachable vulnerabilities: %d\n", len(reachable))
	for _, r := range reachable {
		fmt.Printf("  %s in %s: %s\n", r.OSV, r.Package, r.Summary)
	}
	if len(reachable) > 0 {
		fmt.Println("gate: FAIL")
	} else {
		fmt.Println("gate: PASS")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
reachable vulnerabilities: 1
  GO-2024-0002 in example.com/jwt: Auth bypass in example/jwt
gate: FAIL
```

### Tests

Create `vulngate_test.go`:

```go
package vulngate

import (
	"fmt"
	"strings"
	"testing"
)

const stream = `{"config":{"scanner":"govulncheck"}}
{"osv":{"id":"GO-2024-0001","summary":"DoS in example/parser"}}
{"osv":{"id":"GO-2024-0002","summary":"Auth bypass in example/jwt"}}
{"progress":{"message":"scanning"}}
{"finding":{"osv":"GO-2024-0001","trace":[{"module":"example.com/parser","package":"example.com/parser"}]}}
{"finding":{"osv":"GO-2024-0002","trace":[{"module":"example.com/jwt","package":"example.com/jwt"}]}}
{"finding":{"osv":"GO-2024-0002","trace":[{"module":"example.com/jwt","package":"example.com/jwt","function":"Verify"},{"module":"example.com/app","package":"example.com/app","function":"main"}]}}
`

func TestGateFailsOnReachableOnly(t *testing.T) {
	t.Parallel()
	reachable, err := Gate(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("Gate: %v", err)
	}
	if len(reachable) != 1 {
		t.Fatalf("reachable = %d, want 1 (unreachable GO-2024-0001 must be ignored)", len(reachable))
	}
	got := reachable[0]
	if got.OSV != "GO-2024-0002" {
		t.Errorf("OSV = %q, want GO-2024-0002", got.OSV)
	}
	if got.Package != "example.com/jwt" {
		t.Errorf("Package = %q, want example.com/jwt", got.Package)
	}
	if !strings.Contains(got.Summary, "Auth bypass") {
		t.Errorf("Summary = %q, want the OSV summary attached", got.Summary)
	}
}

func TestGateCleanStream(t *testing.T) {
	t.Parallel()
	// Only an import-level (unreachable) finding: the gate passes.
	clean := `{"osv":{"id":"GO-2024-0001","summary":"DoS"}}
{"finding":{"osv":"GO-2024-0001","trace":[{"module":"example.com/parser","package":"example.com/parser"}]}}
`
	reachable, err := Gate(strings.NewReader(clean))
	if err != nil {
		t.Fatalf("Gate: %v", err)
	}
	if len(reachable) != 0 {
		t.Fatalf("reachable = %v, want none", reachable)
	}
}

func TestGateDeduplicatesOSV(t *testing.T) {
	t.Parallel()
	// Two reachable findings for the same OSV report it once.
	dup := `{"osv":{"id":"GO-2024-0002","summary":"Auth bypass"}}
{"finding":{"osv":"GO-2024-0002","trace":[{"package":"example.com/jwt","function":"Verify"}]}}
{"finding":{"osv":"GO-2024-0002","trace":[{"package":"example.com/jwt","function":"Sign"}]}}
`
	reachable, err := Gate(strings.NewReader(dup))
	if err != nil {
		t.Fatalf("Gate: %v", err)
	}
	if len(reachable) != 1 {
		t.Fatalf("reachable = %d, want 1 (deduplicated)", len(reachable))
	}
}

func ExampleGate() {
	stream := `{"osv":{"id":"GO-2024-0002","summary":"Auth bypass"}}
{"finding":{"osv":"GO-2024-0002","trace":[{"package":"example.com/jwt","function":"Verify"}]}}
`
	reachable, _ := Gate(strings.NewReader(stream))
	fmt.Printf("%d %s\n", len(reachable), reachable[0].OSV)
	// Output: 1 GO-2024-0002
}
```

## Review

The gate is correct when it fails on exactly the vulnerabilities whose vulnerable
symbol your code calls and ignores those merely present in an imported package: the
main test proves it drops the import-level `GO-2024-0001` and reports the reachable
`GO-2024-0002` with its package and summary, and the dedup test proves multiple
symbol findings for one OSV surface once. The load-bearing detail is reading
`trace[0].function` — that top frame is govulncheck's own signal for "this symbol is
actually reachable from your entry points", and keying on it is what separates a
signal-rich gate from an alert-fatigue generator. Streaming with `json.Decoder`
(rather than one big `Unmarshal`) is how this scales to a real scan's output. Run
`go test -race`.

## Resources

- [Go vulnerability management](https://go.dev/security/vuln/) — govulncheck, the vuln database, and reachability analysis.
- [`govulncheck` output](https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck) — the `-json` message stream and the finding/trace shape.
- [`encoding/json.Decoder`](https://pkg.go.dev/encoding/json#Decoder) — streaming `Decode`/`More` and `json.RawMessage`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-tool-directive-pinning.md](09-tool-directive-pinning.md) | Next: [../07-module-proxies-and-goproxy/00-concepts.md](../07-module-proxies-and-goproxy/00-concepts.md)
