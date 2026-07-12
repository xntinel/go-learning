# Exercise 1: Fail-Closed FIPS Startup Posture Gate

A regulated service must refuse to serve non-compliant cryptography. This
exercise builds a reusable startup guard that reads the live cryptographic
posture — `crypto/fips140`'s `Enabled`/`Enforced`/`Version` plus the binary's
embedded `GOFIPS140` build setting — and validates it against a declared
requirement, returning a structured error (and a machine-readable report for a
`/readyz` probe) when the process is not running in the mode its deployment
target mandates. This mirrors the real on-the-job task of making a service fail
closed instead of silently degrading.

This module is fully self-contained: its own `go mod init`, all types inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
fipsposture/                 independent module: example.com/fipsposture
  go.mod                     go 1.26 (crypto/fips140 Enforced/Version need it)
  fipsposture.go             Posture, Requirement, Report; Observe, evaluate (pure), Check
  cmd/
    demo/
      main.go                prints the live posture as JSON; exits non-zero if non-compliant
  fipsposture_test.go        table tests over the pure evaluate; Example
```

- Files: `fipsposture.go`, `cmd/demo/main.go`, `fipsposture_test.go`.
- Implement: `Observe()` reads the live posture; the pure `evaluate(observed, req) error` decides compliance; `Check(req)` ties them together; `Posture.Report(req)` renders a `/readyz` JSON body; `Posture.LogValue()` renders a `slog` group.
- Test: a table over `evaluate` covering enabled+enforced+version pass, disabled fail, version mismatch fail, and enforced-required-but-only-enabled fail, asserting sentinel errors with `errors.Is`.
- Verify: `go test -count=1 -race ./...`

Set up the module. `crypto/fips140.Enforced` and `Version` were added in Go 1.26,
so pin the language version:

```bash
go mod edit -go=1.26
```

### Why a pure decision function is the only testable seam

The `fips140` mode is read once at process start and cannot change afterward, and
you cannot flip it from within a running test (mutating `GODEBUG` after startup
does nothing). So the observation and the decision must be *separated*. `Observe()`
does the impure part — it calls `fips140.Enabled()`, `fips140.Enforced()`,
`fips140.Version()`, and walks `runtime/debug.ReadBuildInfo().Settings` for the
embedded `GOFIPS140` build setting — and returns a plain `Posture` value.
`evaluate(observed Posture, req Requirement) error` is pure: given a posture and a
requirement it returns nil or a joined error, with no globals and no I/O. That
purity is what makes the compliance logic exhaustively table-testable: you
construct synthetic `Posture` values for the disabled case, the version-mismatch
case, and the enforced-but-only-enabled case, none of which you could reproduce by
actually restarting the process under a different GODEBUG.

`evaluate` returns sentinel errors wrapped and joined with `errors.Join`, so a
caller can pick out *which* requirements failed with `errors.Is` — a
`/readyz` handler can surface every failed check at once rather than only the
first. This is deliberately fail-closed: an empty or unexpected value never reads
as "compliant".

### The observed posture and the declared requirement

`Posture` is what is true of the running process. `Requirement` is what the
deployment target mandates — a service in a FedRAMP boundary sets
`RequireEnabled` and often `RequireModuleVersion` to a specific certified version;
a service that must run strict sets `RequireEnforced` too. Keeping requirement and
posture as separate types means the same guard code serves a dev build (no
requirement, everything passes) and a production build (strict requirement) with
only configuration changing.

Create `fipsposture.go`:

```go
package fipsposture

import (
	"crypto/fips140"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
)

// Sentinel errors, wrapped and joined by evaluate so callers can pick out which
// requirement failed with errors.Is.
var (
	ErrNotEnabled      = errors.New("fips140: module not running in FIPS mode")
	ErrNotEnforced     = errors.New("fips140: strict enforcement (fips140=only) not active")
	ErrVersionMismatch = errors.New("fips140: module version mismatch")
	ErrNotFIPSBuild    = errors.New("fips140: binary not built with GOFIPS140")
)

// Posture is the observed cryptographic posture of the running process.
type Posture struct {
	Enabled   bool   // fips140.Enabled(): module operating in FIPS mode
	Enforced  bool   // fips140.Enforced(): strict fips140=only enforcement active
	Version   string // fips140.Version(): "latest" when not built against a frozen module
	BuildFIPS string // GOFIPS140 build setting, "" when the binary was not built with it
}

// Requirement declares what the deployment target mandates.
type Requirement struct {
	RequireEnabled       bool
	RequireEnforced      bool
	RequireModuleVersion string // "" means any version is acceptable
	RequireFIPSBuild     bool   // require an embedded GOFIPS140 build setting
}

// Observe reads the live posture. This is the only impure part; the decision
// logic lives in the pure evaluate.
func Observe() Posture {
	p := Posture{
		Enabled:  fips140.Enabled(),
		Enforced: fips140.Enforced(),
		Version:  fips140.Version(),
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "GOFIPS140" {
				p.BuildFIPS = s.Value
			}
		}
	}
	return p
}

// evaluate is pure: it maps an observed posture and a requirement to nil or a
// joined error. It never reads globals or does I/O, so it is exhaustively
// table-testable.
func evaluate(observed Posture, req Requirement) error {
	var errs []error
	if req.RequireEnabled && !observed.Enabled {
		errs = append(errs, ErrNotEnabled)
	}
	if req.RequireEnforced && !observed.Enforced {
		errs = append(errs, ErrNotEnforced)
	}
	if req.RequireModuleVersion != "" && observed.Version != req.RequireModuleVersion {
		errs = append(errs, fmt.Errorf("%w: have %q, want %q",
			ErrVersionMismatch, observed.Version, req.RequireModuleVersion))
	}
	if req.RequireFIPSBuild && observed.BuildFIPS == "" {
		errs = append(errs, ErrNotFIPSBuild)
	}
	return errors.Join(errs...)
}

// Check observes the live posture and evaluates it against req. A nil result
// means the process may serve; a non-nil result should stop startup or mark the
// service not-ready.
func Check(req Requirement) error {
	return evaluate(Observe(), req)
}

// Report is a machine-readable posture suitable for a /readyz JSON body.
type Report struct {
	Enabled   bool   `json:"fips_enabled"`
	Enforced  bool   `json:"fips_enforced"`
	Version   string `json:"module_version"`
	BuildFIPS string `json:"gofips140_build,omitempty"`
	Compliant bool   `json:"compliant"`
	Reason    string `json:"reason,omitempty"`
}

// Report renders the posture and its compliance verdict against req.
func (p Posture) Report(req Requirement) Report {
	r := Report{
		Enabled:   p.Enabled,
		Enforced:  p.Enforced,
		Version:   p.Version,
		BuildFIPS: p.BuildFIPS,
	}
	if err := evaluate(p, req); err != nil {
		r.Reason = err.Error()
	} else {
		r.Compliant = true
	}
	return r
}

// LogValue lets a Posture be logged as a structured group:
// slog.Info("fips posture", "posture", p).
func (p Posture) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Bool("enabled", p.Enabled),
		slog.Bool("enforced", p.Enforced),
		slog.String("version", p.Version),
		slog.String("gofips140_build", p.BuildFIPS),
	)
}
```

### The runnable demo

The demo declares a requirement (FIPS must be enabled), observes the live posture,
prints it as the JSON a `/readyz` probe would return, logs a structured line, and
exits non-zero if the process is not compliant — the fail-closed behavior in one
place. Run it plainly and it reports a non-FIPS build; run it a second time with
`GODEBUG=fips140=on` and `enabled` flips to `true`, which is the whole point of
observing the live process rather than trusting the build command.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"example.com/fipsposture"
)

func main() {
	req := fipsposture.Requirement{RequireEnabled: true}

	p := fipsposture.Observe()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	slog.Info("fips startup posture", "posture", p)

	body, err := json.MarshalIndent(p.Report(req), "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "marshal report:", err)
		os.Exit(1)
	}
	fmt.Println(string(body))

	if err := fipsposture.Check(req); err != nil {
		fmt.Fprintln(os.Stderr, "startup refused:", err)
		os.Exit(1)
	}
	fmt.Println("startup allowed")
}
```

Run it (plain build, so FIPS is off):

```bash
go run ./cmd/demo
```

Expected output (the JSON on stdout; the `slog` line and refusal go to stderr):

```
{
  "fips_enabled": false,
  "fips_enforced": false,
  "module_version": "latest",
  "compliant": false,
  "reason": "fips140: module not running in FIPS mode"
}
```

Running it again with `GODEBUG=fips140=on go run ./cmd/demo` flips
`fips_enabled` to `true` and `compliant` to `true`, and the process prints
`startup allowed` — proof that the guard reads the live process, not the build.

### Tests

The tests exercise the pure `evaluate` across the compliance matrix, which is
exactly the logic an auditor cares about and which cannot be reached by restarting
the process. Each failing case asserts the specific sentinel with `errors.Is`, so
a regression that returns the wrong reason is caught, not just "some error". A
final subtest confirms `Report` mirrors `evaluate` (compliant flag set, reason
empty on success).

Create `fipsposture_test.go`:

```go
package fipsposture

import (
	"errors"
	"fmt"
	"testing"
)

func TestEvaluate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		observed Posture
		req      Requirement
		want     error // sentinel to assert with errors.Is; nil means compliant
	}{
		{
			name:     "enabled enforced correct version passes",
			observed: Posture{Enabled: true, Enforced: true, Version: "v1.0.0", BuildFIPS: "v1.0.0"},
			req:      Requirement{RequireEnabled: true, RequireEnforced: true, RequireModuleVersion: "v1.0.0", RequireFIPSBuild: true},
			want:     nil,
		},
		{
			name:     "disabled fails",
			observed: Posture{Enabled: false, Version: "latest"},
			req:      Requirement{RequireEnabled: true},
			want:     ErrNotEnabled,
		},
		{
			name:     "version mismatch fails",
			observed: Posture{Enabled: true, Version: "latest"},
			req:      Requirement{RequireEnabled: true, RequireModuleVersion: "v1.0.0"},
			want:     ErrVersionMismatch,
		},
		{
			name:     "enforced required but only enabled fails",
			observed: Posture{Enabled: true, Enforced: false, Version: "v1.0.0"},
			req:      Requirement{RequireEnabled: true, RequireEnforced: true},
			want:     ErrNotEnforced,
		},
		{
			name:     "fips build required but absent fails",
			observed: Posture{Enabled: true, Version: "latest", BuildFIPS: ""},
			req:      Requirement{RequireEnabled: true, RequireFIPSBuild: true},
			want:     ErrNotFIPSBuild,
		},
		{
			name:     "empty requirement passes any posture",
			observed: Posture{},
			req:      Requirement{},
			want:     nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := evaluate(tc.observed, tc.req)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("evaluate() = %v; want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("evaluate() = %v; want errors.Is(_, %v)", err, tc.want)
			}
		})
	}
}

func TestReportMirrorsEvaluate(t *testing.T) {
	t.Parallel()

	p := Posture{Enabled: true, Enforced: true, Version: "v1.0.0", BuildFIPS: "v1.0.0"}
	req := Requirement{RequireEnabled: true, RequireEnforced: true, RequireModuleVersion: "v1.0.0"}
	if r := p.Report(req); !r.Compliant || r.Reason != "" {
		t.Fatalf("Report() = %+v; want compliant with empty reason", r)
	}

	bad := Posture{Enabled: false, Version: "latest"}
	r := bad.Report(Requirement{RequireEnabled: true})
	if r.Compliant {
		t.Fatalf("Report() compliant = true; want false")
	}
	if r.Reason == "" {
		t.Fatal("Report() reason is empty; want a failure reason")
	}
}

func Example() {
	// A dev posture (FIPS off) against a production requirement (FIPS on):
	// the reason is deterministic and machine-readable.
	dev := Posture{Enabled: false, Version: "latest"}
	fmt.Println(evaluate(dev, Requirement{RequireEnabled: true}))
	// Output: fips140: module not running in FIPS mode
}
```

## Review

The guard is correct when the decision is a pure function of the observed posture
and the requirement, and when it fails closed: an empty or unexpected value must
never read as compliant, which is why every check is opt-in via the requirement
and the joined error defaults to a real failure. The tests prove each failure
mode independently and pin the exact sentinel with `errors.Is`, so a change that
returns "not enabled" when it should return "version mismatch" is caught.

The mistakes to avoid are structural. Do not try to toggle FIPS mode inside a test
to reach the enabled cases — the `fips140` GODEBUG is fixed at startup; construct
synthetic `Posture` values instead, which is why `Observe` and `evaluate` are
split. Do not conflate `Version()` reporting a frozen version with `Enabled()`
being true: a binary can report `v1.0.0` while not actually running in FIPS mode
this process, which is precisely why the requirement can demand both. And keep the
fail-closed default: when in doubt the process should refuse to serve, because a
mis-set GODEBUG is invisible until an auditor finds it. Confirm the report path by
running the demo twice — once plain, once with `GODEBUG=fips140=on` — and watching
`fips_enabled` flip.

## Resources

- [`crypto/fips140`](https://pkg.go.dev/crypto/fips140) — `Enabled`, `Enforced`, `Version`, and `WithoutEnforcement`, with the Go versions each was added.
- [FIPS 140-3 Compliance](https://go.dev/doc/security/fips140) — the official Go security doc on `GOFIPS140`, the `fips140` GODEBUG, and mode semantics.
- [`runtime/debug.ReadBuildInfo`](https://pkg.go.dev/runtime/debug#ReadBuildInfo) — the `BuildInfo` and `BuildSetting` types that carry the embedded `GOFIPS140` setting.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-fips-tls-conformance.md](02-fips-tls-conformance.md)
