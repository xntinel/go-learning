# Exercise 3: Invoke govulncheck from Go and interpret its exit code

A CI gate usually drives govulncheck rather than being driven by it. This exercise
builds a runner on `golang.org/x/vuln/scan` that runs govulncheck in-process,
streams its `-json` output into a buffer, and — separately — a pure exit-code
classifier that turns a process error into a build outcome. The two are split on
purpose: the classifier is deterministic and unit-tested offline, while the
network- and toolchain-dependent invocation lives behind an `integration` build
tag.

This module is fully self-contained: its own `go mod init`, its own demo and
tests. Nothing here imports another exercise.

## What you'll build

```text
vulnrunner/                  independent module: example.com/vulnrunner
  go.mod                     go 1.26 (+ golang.org/x/vuln for the integration build)
  exitcode.go                Outcome; Classify (pure); Outcome.ExitCode
  runner.go                  //go:build integration: RunJSON over scan.Command
  cmd/
    demo/
      main.go                classifies clean / vulns / tool-failure error cases
  exitcode_test.go           table over Classify; Example
  runner_integration_test.go //go:build integration: proves the -json exit-0 contract
```

- Files: `exitcode.go`, `runner.go`, `cmd/demo/main.go`, `exitcode_test.go`, `runner_integration_test.go`.
- Implement: `Classify(err error) Outcome` via `errors.As` against `interface{ ExitCode() int }`; `Outcome.ExitCode()`; `RunJSON(ctx, args...)` over `scan.Command` (integration-tagged).
- Test: feed `Classify` nil (clean), a fake `ExitCode() int` returning 3 (vulnerable), and other errors (scan-failed); an integration test that runs a real scan and proves `-json` exits 0 even with a finding.
- Verify: `go test -count=1 ./...` (unit); `go test -tags=integration ./...` needs the govulncheck toolchain and DB access.

Set up the module:

```bash
mkdir -p ~/go-exercises/vulnrunner/cmd/demo
cd ~/go-exercises/vulnrunner
go mod init example.com/vulnrunner
go mod edit -go=1.26
go get golang.org/x/vuln@latest   # only the integration build imports it
```

### Why the classifier and the runner are separate

The runner talks to the network and needs the govulncheck toolchain and a live
vulnerability database; it cannot run in an offline unit test. The exit-code
interpretation, on the other hand, is pure logic and is exactly the part most
likely to be wrong in a real pipeline. Splitting them means the risky logic —
"does this error mean vulnerable, or does it mean the scan itself broke?" — is
covered by fast, deterministic tests, and only the genuinely I/O-bound part sits
behind `//go:build integration`. Files carrying that tag are excluded from the
default build, so `go test ./...` runs offline and green; the integration test
runs only when you opt in with `-tags=integration`.

### The exit-code contract, and why Classify never conflates the three cases

govulncheck's text mode exits `3` when it finds reachable vulnerabilities and `0`
when clean; any *other* nonzero code means the scan itself failed — a build error,
a network failure, a missing database. A naive gate collapses this into "nonzero =
bad", which is wrong twice over: it reports infrastructure failures as
vulnerabilities, and (worse) if it only checks `== 0` it treats a `3` and a
`clean` differently but a `3` and a `tool crash` the same. `Classify` keeps three
outcomes distinct: `OutcomeClean` (nil error), `OutcomeVulnerable` (exit code
exactly `3`), and `OutcomeScanFailed` (any other exit code, or a non-exit error).
A scan failure must page whoever owns the pipeline, not silently pass and not file
a false CVE.

The exit code is retrieved exactly as `os/exec` intends: `scan.Cmd.Wait` returns
an error that wraps something implementing `interface{ ExitCode() int }`, so
`errors.As` extracts it through any wrapping. `Classify` uses that path, which is
why it works whether the error is the raw exit error or one wrapped with `%w`.

### The -json exit-0 trap the runner must respect

`RunJSON` requests `-json`, and under `-json` govulncheck **always exits 0**,
regardless of findings. So `Wait` returns nil even when the scan found a reachable
vulnerability. This is the single most important thing about the runner: the
verdict does not come from the error — it comes from parsing the bytes the runner
captured (with the parser from Exercise 1). The classifier is for the text-mode
exit code; the `-json` runner deliberately ignores exit status for the vuln
verdict and only treats a genuine start/Wait failure as an error. The integration
test pins this contract directly: it runs a scan against a module with a known
advisory and asserts `Wait` returned nil *and* the JSON buffer is non-empty.

Create `exitcode.go`:

```go
package vulnrunner

import "errors"

// Outcome is the build-relevant interpretation of a govulncheck run.
type Outcome int

const (
	OutcomeClean      Outcome = iota // no vulnerabilities (exit 0 in text mode)
	OutcomeVulnerable                // reachable vulnerabilities (exit 3 in text mode)
	OutcomeScanFailed                // the scan itself failed (any other error)
)

func (o Outcome) String() string {
	switch o {
	case OutcomeClean:
		return "clean"
	case OutcomeVulnerable:
		return "vulnerable"
	default:
		return "scan-failed"
	}
}

// ExitCode maps an outcome to a CI exit code. It mirrors govulncheck text mode
// for vulnerable (3) and clean (0), and uses a distinct code (2) for a scan
// failure so a pipeline can tell an infrastructure error from a finding.
func (o Outcome) ExitCode() int {
	switch o {
	case OutcomeClean:
		return 0
	case OutcomeVulnerable:
		return 3
	default:
		return 2
	}
}

// Classify interprets the error returned by a text-mode govulncheck run. It maps
// nil to clean, an exit code of exactly 3 to vulnerable, and every other error
// (a different exit code, or a non-exit error such as a network failure) to
// scan-failed, so a tool failure is never mistaken for a clean pass or a finding.
// The exit code is retrieved with errors.As against interface{ ExitCode() int },
// so it works through %w wrapping.
func Classify(err error) Outcome {
	if err == nil {
		return OutcomeClean
	}
	var ec interface{ ExitCode() int }
	if errors.As(err, &ec) {
		if ec.ExitCode() == 3 {
			return OutcomeVulnerable
		}
		return OutcomeScanFailed
	}
	return OutcomeScanFailed
}
```

Create `runner.go`. The build tag keeps this file — and its dependency on
`golang.org/x/vuln/scan` — out of the default offline build:

```go
//go:build integration

package vulnrunner

import (
	"bytes"
	"context"
	"fmt"

	"golang.org/x/vuln/scan"
)

// RunJSON runs govulncheck in-process with -json and returns its captured stdout.
// scan.Command mirrors os/exec.Cmd: set Stdout/Stderr/Env, then Start and Wait.
//
// Because -json makes govulncheck exit 0 regardless of findings, Wait returns nil
// even when a reachable vulnerability exists. The caller MUST compute the verdict
// by parsing the returned bytes, not from the (absent) error. A non-nil error
// therefore means the scan itself failed to run, not that vulnerabilities exist.
func RunJSON(ctx context.Context, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := scan.Command(ctx, append([]string{"-json"}, args...)...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("vulnrunner: start govulncheck: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		return stdout.Bytes(), fmt.Errorf("vulnrunner: govulncheck failed: %w: %s", err, stderr.String())
	}
	return stdout.Bytes(), nil
}
```

### The runnable demo

The demo exercises the classifier against the three error shapes a gate must
distinguish, using a small local error type that implements `ExitCode() int`
exactly as govulncheck's process error does. It needs no network, so it runs from
the default build.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/vulnrunner"
)

// exitErr mimics the error shape scan.Cmd.Wait wraps: it carries a process exit
// code retrievable via ExitCode().
type exitErr struct{ code int }

func (e exitErr) Error() string { return fmt.Sprintf("exit status %d", e.code) }
func (e exitErr) ExitCode() int { return e.code }

func main() {
	cases := []struct {
		label string
		err   error
	}{
		{"clean scan (nil)", nil},
		{"text-mode vulns (exit 3)", exitErr{3}},
		{"tool failure (exit 1)", exitErr{1}},
	}
	for _, c := range cases {
		o := vulnrunner.Classify(c.err)
		fmt.Printf("%s => %s (exit %d)\n", c.label, o, o.ExitCode())
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
clean scan (nil) => clean (exit 0)
text-mode vulns (exit 3) => vulnerable (exit 3)
tool failure (exit 1) => scan-failed (exit 2)
```

### Tests

`TestClassify` is a pure table: nil is clean, an exit code of 3 is vulnerable, an
exit code of 1 is scan-failed, a `%w`-wrapped exit-3 error is still vulnerable
(proving `errors.As` sees through wrapping), and a plain error is scan-failed. The
`Example` pins the string forms. The integration test — behind `//go:build
integration` so it never runs in the default offline build — runs a real scan and
asserts the `-json` exit-0 contract: `RunJSON` returns nil error and a non-empty
buffer even against a module with a known advisory. It needs the govulncheck
toolchain and network access to `vuln.go.dev`, and a fixture module under
`testdata/vulnmod` that pins a vulnerable dependency.

Create `exitcode_test.go`:

```go
package vulnrunner

import (
	"errors"
	"fmt"
	"testing"
)

// fakeExitErr implements the ExitCode() interface that scan.Cmd.Wait's error
// satisfies, so Classify can be tested without running the tool.
type fakeExitErr struct{ code int }

func (e fakeExitErr) Error() string { return fmt.Sprintf("exit status %d", e.code) }
func (e fakeExitErr) ExitCode() int { return e.code }

func TestClassify(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want Outcome
	}{
		{"nil is clean", nil, OutcomeClean},
		{"exit 3 is vulnerable", fakeExitErr{3}, OutcomeVulnerable},
		{"exit 1 is scan failed", fakeExitErr{1}, OutcomeScanFailed},
		{"wrapped exit 3 is vulnerable", fmt.Errorf("wait: %w", fakeExitErr{3}), OutcomeVulnerable},
		{"plain error is scan failed", errors.New("network down"), OutcomeScanFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Classify(tc.err); got != tc.want {
				t.Errorf("Classify(%v) = %s; want %s", tc.err, got, tc.want)
			}
		})
	}
}

func ExampleClassify() {
	fmt.Println(Classify(nil))
	fmt.Println(Classify(fakeExitErr{3}))
	fmt.Println(Classify(fakeExitErr{1}))
	// Output:
	// clean
	// vulnerable
	// scan-failed
}
```

Create `runner_integration_test.go`:

```go
//go:build integration

package vulnrunner

import (
	"bytes"
	"testing"
)

// TestRunJSONExitZeroContract proves that under -json govulncheck exits 0 even
// when a reachable vulnerability is present: RunJSON returns a nil error and a
// non-empty JSON stream. Run with:
//
//	go test -tags=integration ./...
//
// It requires the govulncheck toolchain and network access to vuln.go.dev, plus a
// fixture module at testdata/vulnmod that imports a known-vulnerable dependency.
func TestRunJSONExitZeroContract(t *testing.T) {
	t.Chdir("testdata/vulnmod")

	out, err := RunJSON(t.Context(), "-mode=source", "./...")
	if err != nil {
		t.Fatalf("RunJSON returned an error despite the -json exit-0 contract: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("RunJSON produced no JSON output")
	}
	if !bytes.Contains(out, []byte(`"config"`)) {
		t.Fatalf("output did not begin with a config envelope: %s", out)
	}
}
```

## Review

The classifier is correct when it keeps three outcomes apart: nil is clean, exit
code exactly `3` is vulnerable, and everything else — a different exit code or a
non-exit error — is scan-failed, so an infrastructure failure never masquerades as
a clean pass or a finding. The `errors.As` against `interface{ ExitCode() int }`
is what makes it robust to `%w` wrapping, which the wrapped-exit-3 test asserts
directly. The runner is correct when it treats a `-json` run's nil error as "the
scan ran", not "no vulnerabilities": the verdict is computed from the captured
bytes, because under `-json` the exit code is always 0.

The mistakes to avoid: do not write `govulncheck -json ./... && deploy` or its
programmatic equivalent that trusts the exit status of a `-json` run — it is always
0 and ships vulnerable code. Do not collapse every nonzero exit into
"vulnerabilities found"; reserve that for `3` and route the rest to a scan-failure
alert. Confirm the offline path with the demo and `go test ./...`; confirm the
exit-0 contract with `go test -tags=integration ./...` against a fixture that pins
a vulnerable dependency, which returns a nil error and a populated JSON buffer.

## Resources

- [`golang.org/x/vuln/scan`](https://pkg.go.dev/golang.org/x/vuln/scan) — `Command`, the `Cmd` fields, `Start`/`Wait`, and the `ExitCode()` error contract.
- [govulncheck command](https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck) — the exit codes and the exit-0 behavior of `-json`/`-sarif`/`-openvex`.
- [`errors.As`](https://pkg.go.dev/errors#As) — extracting the exit-code interface through wrapped errors.
- [govulncheck user guide](https://go.dev/security/vuln/govulncheck) — running modes, `-mode=source` vs binary, and CI integration.

---

Back to [02-reachability-triage-gate.md](02-reachability-triage-gate.md) | Next: [../10-supply-chain-slsa-sbom/00-concepts.md](../10-supply-chain-slsa-sbom/00-concepts.md)
