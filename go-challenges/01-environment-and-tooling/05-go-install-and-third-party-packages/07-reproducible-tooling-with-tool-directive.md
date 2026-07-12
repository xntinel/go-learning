# Exercise 7: Per-Repo Reproducible Tooling with the tool Directive

Globally installed dev tools drift: every laptop's `~/go/bin` ends up on a
different version of `goimports`, `stringer`, or a codegen, and one day the
generated output disagrees across the team. Go 1.24's `tool` directive fixes this
by pinning tools in `go.mod` and running them via `go tool`. This exercise builds
a real formatting-gate tool (the kind you would pin), then walks the directive
commands that make its version deterministic per checkout.

This module is self-contained and uses only the standard library.

## What you'll build

```text
fmtguard/                       independent module: example.com/fmtguard
  go.mod                        will also carry a tool directive (shown below)
  fmtguard.go                   Check(src) (formatted, want, err) over go/format
  fmtguard_test.go              table test + Example
  cmd/demo/
    main.go                     a local dev tool: fails if a file is not gofmt-clean
```

Files: `fmtguard.go`, `fmtguard_test.go`, `cmd/demo/main.go`.
Implement: `Check(src []byte) (formatted bool, want []byte, err error)` using `go/format`.
Test: unformatted source is reported unformatted with a corrected `want`; clean source is reported formatted.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/01-environment-and-tooling/05-go-install-and-third-party-packages/07-reproducible-tooling-with-tool-directive/cmd/demo
cd go-solutions/01-environment-and-tooling/05-go-install-and-third-party-packages/07-reproducible-tooling-with-tool-directive
```

### The directive, and what it replaces

Before Go 1.24 the trick for pinning tools was a `tools.go` file with blank
imports so the tools rode along as ordinary dependencies:

```go
//go:build tools

package tools

import _ "golang.org/x/tools/cmd/stringer"
```

It worked but abused the dependency graph and needed a manual `go install` step.
Go 1.24 replaced it with a first-class `tool` directive. `go get -tool` pins a
tool and records it:

```bash
go get -tool golang.org/x/tools/cmd/goimports
go get -tool golang.org/x/tools/cmd/stringer
```

Your `go.mod` now contains, alongside the usual `require` lines that pin the
tools' versions:

```text
tool (
	golang.org/x/tools/cmd/goimports
	golang.org/x/tools/cmd/stringer
)
```

From here everything runs off the pin, identically on every checkout:

```bash
go tool                       # lists the pinned tools
go tool goimports -l .        # runs the PINNED goimports, built from the cache
go install tool               # materializes all pinned tools into GOBIN (for CI)
go get tool@upgrade           # bumps every pinned tool together
go get golang.org/x/tools/cmd/stringer@none   # removes that tool from the directive
```

The version a teammate runs is the one in `go.mod`, not whatever their global
`~/go/bin` drifted to — and `go version -m "$(go env GOPATH)/bin/goimports"` after
`go install tool` shows the same version `go.mod` pins. That is the whole payoff:
tooling is now part of the reproducible, checked-in state of the repo.

### The tool you would pin

`cmd/demo` is exactly the kind of local tool a team pins with the directive:
a CI gate that fails when any Go file is not gofmt-clean. Its core is `Check`,
built on `go/format.Source`, which formats source the way `gofmt` does. `Check`
returns whether the input already equals its formatted form and, when it does
not, the corrected bytes — so the tool can both fail the build and show the fix.

Create `fmtguard.go`:

```go
// fmtguard.go
package fmtguard

import (
	"bytes"
	"go/format"
)

// Check reports whether src is already gofmt-formatted. When it is not,
// formatted is false and want holds the corrected source. A syntax error in src
// is returned as err.
func Check(src []byte) (formatted bool, want []byte, err error) {
	want, err = format.Source(src)
	if err != nil {
		return false, nil, err
	}
	return bytes.Equal(want, src), want, nil
}
```

### The demo tool

`cmd/demo` runs `Check` over an inline sample and reports the verdict with an
exit code — non-zero when the source needs formatting, the convention a CI step
relies on. A real version would walk `os.Args` files; the inline sample keeps the
output deterministic.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"os"

	"example.com/fmtguard"
)

func main() {
	src := []byte("package p\nvar x=1\n") // deliberately unformatted
	formatted, want, err := fmtguard.Check(src)
	if err != nil {
		fmt.Fprintln(os.Stderr, "parse error:", err)
		os.Exit(2)
	}
	if formatted {
		fmt.Println("ok: gofmt-clean")
		return
	}
	fmt.Print("needs formatting; want:\n", string(want))
	os.Exit(1)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
needs formatting; want:
package p

var x = 1
```

(The tool exits with status 1 because the sample was not gofmt-clean.)

### The test

The table drives both branches: an unformatted source is reported unformatted
with a corrected `want`, and an already-clean source is reported formatted and
byte-identical. The `Example` pins the corrected output.

Create `fmtguard_test.go`:

```go
// fmtguard_test.go
package fmtguard

import (
	"fmt"
	"testing"
)

func TestCheck(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		src           string
		wantFormatted bool
		wantOut       string
	}{
		{
			name:          "unformatted",
			src:           "package p\nvar x=1\n",
			wantFormatted: false,
			wantOut:       "package p\n\nvar x = 1\n",
		},
		{
			name:          "already clean",
			src:           "package p\n\nvar x = 1\n",
			wantFormatted: true,
			wantOut:       "package p\n\nvar x = 1\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			formatted, want, err := Check([]byte(tc.src))
			if err != nil {
				t.Fatalf("Check() unexpected err = %v", err)
			}
			if formatted != tc.wantFormatted {
				t.Fatalf("Check() formatted = %v, want %v", formatted, tc.wantFormatted)
			}
			if string(want) != tc.wantOut {
				t.Fatalf("Check() want = %q, want %q", want, tc.wantOut)
			}
		})
	}
}

func ExampleCheck() {
	formatted, want, _ := Check([]byte("package p\nvar x=1\n"))
	fmt.Printf("formatted=%v\n%s", formatted, want)
	// Output:
	// formatted=false
	// package p
	//
	// var x = 1
}
```

## Review

The tool is correct when `Check` reports `formatted=true` exactly for input that
already equals `format.Source(input)`, and otherwise returns the corrected bytes;
the demo's non-zero exit is what lets CI fail on unformatted code. The directive
is used correctly when tools live in the `tool` block of `go.mod` (added by
`go get -tool`, removed by `@none`) and are run with `go tool`, never installed
globally per developer. The trap is keeping a `tools.go` blank-import file *and* a
`tool` directive, or expecting `go install pkg@version` to update the directive —
only `go get -tool`/`go get tool@upgrade` edit `go.mod`. Confirm with
`go test -race ./...` and `go tool` (which lists the pinned set).

## Resources

- [Managing tool dependencies (`go get -tool`, `go tool`)](https://go.dev/doc/modules/managing-dependencies#tools) — the Go 1.24 directive workflow.
- [Go 1.24 release notes: tool directive](https://go.dev/doc/go1.24#tools) — what the directive is and how it replaces `tools.go`.
- [`go/format`](https://pkg.go.dev/go/format) — `format.Source`, the gofmt engine behind the check tool.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-read-binary-build-metadata.md](06-read-binary-build-metadata.md) | Next: [08-installing-from-private-module-servers.md](08-installing-from-private-module-servers.md)
