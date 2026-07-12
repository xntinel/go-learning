# Exercise 7: Resolve the effective -mod value the way the go command does

"It built fine locally, but CI used the module cache instead of vendor/" is a
recurring class of bug, and it comes from misunderstanding exactly when the go
command auto-selects `-mod=vendor`. This exercise encodes the real resolution rule
as a pure function: given the `go` directive, the presence of `vendor/`, `GOFLAGS`,
an explicit `-mod` flag, and workspace mode, return the effective mode and the
reason for it.

This module is fully self-contained: its own `go mod init`, its own demo and
exhaustive table tests. Nothing here imports another exercise.

## What you'll build

```text
modresolver/                 independent module: example.com/modresolver
  go.mod                     go 1.26 (requires golang.org/x/mod)
  modresolver.go             type Inputs, Result; Resolve; ErrVendorMissing
  cmd/
    demo/
      main.go                resolves several representative configurations
  modresolver_test.go        exhaustive rule table
```

- Files: `modresolver.go`, `cmd/demo/main.go`, `modresolver_test.go`.
- Implement: `Resolve(Inputs) (Result, error)`, returning the effective mode (`mod`, `vendor`, or `readonly`) and a human reason, encoding the documented precedence: explicit flag, then `GOFLAGS`, then workspace mode, then the auto-vendor rule, then the default.
- Test: an exhaustive table crossing go `1.13` vs `1.14+` with `vendor/` present/absent, a `GOFLAGS=-mod=mod` override, an explicit `-mod=vendor` with no vendor directory (error), and workspace mode disabling auto-vendor.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/11-packages-and-modules/08-vendor-directory/07-build-mod-resolver/cmd/demo
cd go-solutions/11-packages-and-modules/08-vendor-directory/07-build-mod-resolver
go get golang.org/x/mod
```

### The rule, in precedence order

The go command decides the module mode by the first rule that applies:

1. An explicit `-mod` flag on the command line wins outright. If it is
   `-mod=vendor` but no `vendor/` directory exists, that is an error — the go
   command refuses rather than silently falling back.
2. Otherwise a `-mod=...` inside `GOFLAGS` applies, with the same vendor-presence
   check. This is the silent override that causes the "differs in CI" surprise: a
   `GOFLAGS=-mod=mod` in one environment defeats the auto rule.
3. Otherwise, in workspace mode (a `go.work` covering multiple modules), the
   top-level `vendor/` is ignored and the auto-vendor rule does not fire; the mode
   is `readonly`.
4. Otherwise the auto-vendor rule: if the `go` directive is `1.14` or higher AND a
   `vendor/` directory is present, the mode is `vendor`.
5. Otherwise the default is `readonly` (the build-command default since Go 1.16):
   the cache is used but `go.mod` is not modified.

Encoding this as a pure function makes it testable exhaustively, and makes the
reason auditable — a CI diagnostic can print *why* a given build resolved to a
given mode.

### Comparing go directives with semver

The `go` directive (`1.14`, `1.21.5`) is not a semver on its own, but prefixing
`v` makes it one (`v1.14`, `v1.21.5`), and `semver.Compare` then orders it
correctly — including `v1.21.5` above `v1.14`. `semver.IsValid` guards a malformed
directive; a directive that does not parse simply fails the `>= 1.14` test rather
than crashing, which is the conservative behavior (no auto-vendor on a version we
cannot understand).

Create `modresolver.go`:

```go
package modresolver

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/mod/semver"
)

// ErrVendorMissing is returned when -mod=vendor is requested but no vendor/
// directory is present.
var ErrVendorMissing = errors.New("modresolver: -mod=vendor requested but vendor/ is absent")

// Inputs are the signals the go command uses to pick a module mode.
type Inputs struct {
	GoVersion     string // the go directive, e.g. "1.14" or "1.21.5"
	VendorPresent bool   // a vendor/ directory exists at the module root
	GOFLAGS       string // the GOFLAGS environment value, may contain -mod=...
	ExplicitMod   string // an explicit -mod flag value, "" if none
	Workspace     bool   // running under a go.work (workspace mode)
}

// Result is the resolved mode and the reason it was chosen.
type Result struct {
	Mode   string // "mod", "vendor", or "readonly"
	Reason string
}

// Resolve returns the effective module mode for the given inputs, mirroring the
// go command's documented precedence.
func Resolve(in Inputs) (Result, error) {
	// 1. Explicit -mod flag wins.
	if in.ExplicitMod != "" {
		return fromRequest(in.ExplicitMod, in.VendorPresent, "explicit -mod flag")
	}
	// 2. GOFLAGS -mod=... applies next.
	if m, ok := modFromGOFLAGS(in.GOFLAGS); ok {
		return fromRequest(m, in.VendorPresent, "GOFLAGS -mod")
	}
	// 3. Workspace mode ignores the top-level vendor/.
	if in.Workspace {
		return Result{Mode: "readonly", Reason: "workspace mode ignores the top-level vendor/"}, nil
	}
	// 4. Auto-vendor: go >= 1.14 AND vendor/ present.
	if in.VendorPresent && goAtLeast(in.GoVersion, "1.14") {
		return Result{Mode: "vendor", Reason: "auto: go >= 1.14 and vendor/ present"}, nil
	}
	// 5. Default.
	return Result{Mode: "readonly", Reason: "default: module cache, read-only"}, nil
}

// fromRequest validates a requested mode and enforces the vendor-presence rule.
func fromRequest(mode string, vendorPresent bool, reason string) (Result, error) {
	switch mode {
	case "mod", "readonly":
		return Result{Mode: mode, Reason: reason}, nil
	case "vendor":
		if !vendorPresent {
			return Result{}, fmt.Errorf("%w (via %s)", ErrVendorMissing, reason)
		}
		return Result{Mode: "vendor", Reason: reason}, nil
	default:
		return Result{}, fmt.Errorf("modresolver: unknown -mod value %q", mode)
	}
}

// modFromGOFLAGS extracts a -mod=... value from a GOFLAGS string, if present.
func modFromGOFLAGS(goflags string) (string, bool) {
	for _, f := range strings.Fields(goflags) {
		if v, ok := strings.CutPrefix(f, "-mod="); ok {
			return v, true
		}
	}
	return "", false
}

// goAtLeast reports whether go directive gv is >= want (both as bare "1.14"
// style strings). An unparseable gv returns false.
func goAtLeast(gv, want string) bool {
	a, b := "v"+gv, "v"+want
	if !semver.IsValid(a) || !semver.IsValid(b) {
		return false
	}
	return semver.Compare(a, b) >= 0
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/modresolver"
)

func main() {
	configs := []struct {
		name string
		in   modresolver.Inputs
	}{
		{"modern + vendor", modresolver.Inputs{GoVersion: "1.21", VendorPresent: true}},
		{"modern + no vendor", modresolver.Inputs{GoVersion: "1.21", VendorPresent: false}},
		{"old go + vendor", modresolver.Inputs{GoVersion: "1.13", VendorPresent: true}},
		{"GOFLAGS override", modresolver.Inputs{GoVersion: "1.21", VendorPresent: true, GOFLAGS: "-mod=mod"}},
		{"workspace + vendor", modresolver.Inputs{GoVersion: "1.21", VendorPresent: true, Workspace: true}},
	}
	for _, c := range configs {
		r, err := modresolver.Resolve(c.in)
		if err != nil {
			fmt.Printf("%-20s ERROR %v\n", c.name, err)
			continue
		}
		fmt.Printf("%-20s %-9s (%s)\n", c.name, r.Mode, r.Reason)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
modern + vendor      vendor    (auto: go >= 1.14 and vendor/ present)
modern + no vendor   readonly  (default: module cache, read-only)
old go + vendor      readonly  (default: module cache, read-only)
GOFLAGS override     mod       (GOFLAGS -mod)
workspace + vendor   readonly  (workspace mode ignores the top-level vendor/)
```

### Tests

The table is exhaustive over the rule's inputs: the go-version boundary (1.13 vs
1.14), vendor present/absent, the `GOFLAGS` override, the explicit-flag win, the
`-mod=vendor`-without-vendor error, and workspace mode disabling auto-vendor.

Create `modresolver_test.go`:

```go
package modresolver

import (
	"errors"
	"fmt"
	"testing"
)

func TestResolve(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		in       Inputs
		wantMode string
		wantErr  bool
	}{
		{"auto vendor on modern go", Inputs{GoVersion: "1.14", VendorPresent: true}, "vendor", false},
		{"newer go still auto", Inputs{GoVersion: "1.21.5", VendorPresent: true}, "vendor", false},
		{"pre 1.14 no auto", Inputs{GoVersion: "1.13", VendorPresent: true}, "readonly", false},
		{"modern no vendor", Inputs{GoVersion: "1.21", VendorPresent: false}, "readonly", false},
		{"GOFLAGS mod overrides auto", Inputs{GoVersion: "1.21", VendorPresent: true, GOFLAGS: "-mod=mod"}, "mod", false},
		{"explicit vendor wins", Inputs{GoVersion: "1.21", VendorPresent: true, ExplicitMod: "vendor"}, "vendor", false},
		{"explicit vendor without dir errors", Inputs{GoVersion: "1.21", VendorPresent: false, ExplicitMod: "vendor"}, "", true},
		{"GOFLAGS vendor without dir errors", Inputs{GoVersion: "1.21", VendorPresent: false, GOFLAGS: "-mod=vendor"}, "", true},
		{"workspace disables auto vendor", Inputs{GoVersion: "1.21", VendorPresent: true, Workspace: true}, "readonly", false},
		{"explicit beats GOFLAGS", Inputs{GoVersion: "1.21", VendorPresent: true, ExplicitMod: "readonly", GOFLAGS: "-mod=mod"}, "readonly", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Resolve(tc.in)
			if tc.wantErr {
				if !errors.Is(err, ErrVendorMissing) {
					t.Fatalf("err = %v; want ErrVendorMissing", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error %v", err)
			}
			if got.Mode != tc.wantMode {
				t.Fatalf("mode = %q (%s); want %q", got.Mode, got.Reason, tc.wantMode)
			}
		})
	}
}

func Example() {
	r, _ := Resolve(Inputs{GoVersion: "1.21", VendorPresent: true})
	fmt.Println(r.Mode, "-", r.Reason)
	// Output: vendor - auto: go >= 1.14 and vendor/ present
}
```

## Review

The resolver is correct when it applies the five rules in exactly the documented
order: an explicit flag and then `GOFLAGS` both win over the auto rule, which is
precisely why a stray `GOFLAGS=-mod=mod` produces a different build than a clean
environment. Workspace mode short-circuits auto-vendor because a top-level
`vendor/` is not consulted for a multi-module build. The `-mod=vendor`-without
-vendor error is the honest behavior: the go command refuses rather than silently
using the cache, and so does this function via `ErrVendorMissing`. The go-version
comparison must go through `semver` on the `v`-prefixed string so that `1.21.5`
sorts above `1.14`; a naive string compare would misorder multi-digit minors.

## Resources

- [Vendoring and the `-mod` flag](https://go.dev/ref/mod#vendoring) — the auto-enable rule and the mode values.
- [Build commands and `-mod`](https://go.dev/ref/mod#build-commands) — how `-mod`, `GOFLAGS`, and the default interact.
- [`golang.org/x/mod/semver`](https://pkg.go.dev/golang.org/x/mod/semver) — `Compare` and `IsValid` for the go-directive comparison.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-reproducible-vendor-attestation.md](08-reproducible-vendor-attestation.md)
