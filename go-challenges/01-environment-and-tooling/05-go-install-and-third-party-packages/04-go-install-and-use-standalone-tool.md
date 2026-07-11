# Exercise 4: go install — A Standalone Developer Tool

`go install pkg@version` compiles a Go-authored command and drops the binary in
`GOBIN`, without touching your module's `go.mod`. The canonical example is
`goimports`, the import-fixing formatter from `golang.org/x/tools`. This exercise
installs it, proves it resolves on `PATH`, and then builds a small library around
the *same engine* — `golang.org/x/tools/imports` — so you can see, and test, what
`goimports` does under the hood.

This module is self-contained and links `golang.org/x/tools`.

## What you'll build

```text
importfix/                      independent module: example.com/importfix
  go.mod                        require golang.org/x/tools
  go.sum
  importfix.go                  Fix(filename, src) (out, changed, err) over imports.Process
  importfix_test.go             adds missing import; leaves clean source unchanged; Example
  cmd/demo/
    main.go                     runs Fix on an inline source missing "fmt"
```

Files: `importfix.go`, `importfix_test.go`, `cmd/demo/main.go`.
Implement: `Fix(filename string, src []byte) (out []byte, changed bool, err error)` wrapping `imports.Process`.
Test: table cases (missing import gets added; already-clean source is unchanged) plus an `Example`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/importfix/cmd/demo
cd ~/go-exercises/importfix
go mod init example.com/importfix
```

### Installing the tool, and where it goes

Install `goimports` as a runnable command:

```bash
go install golang.org/x/tools/cmd/goimports@latest
```

This compiles the `cmd/goimports` `main` package and writes the binary to
`GOBIN` (or `$(go env GOPATH)/bin` if `GOBIN` is empty). It does *not* add
anything to this module's `go.mod` — `go install pkg@version` is module-neutral by
design, because a developer tool has no business appearing in your service's
dependency list. Confirm it resolves:

```bash
which goimports          # -> $(go env GOPATH)/bin/goimports
goimports --help 2>&1 | head -1
```

If `which` prints nothing, `$(go env GOPATH)/bin` is not on `PATH`; add it. And
because `go install` is module-neutral, `go.mod` is byte-identical before and
after the install — a property you can check with `git diff go.mod` (empty).

### Building a tester's `goimports`

`goimports` the command is a thin wrapper over `golang.org/x/tools/imports`, and
its core is one function: `imports.Process(filename string, src []byte, opt *imports.Options) ([]byte, error)`.
It parses the source, adds missing imports, removes unused ones, and gofmt-formats
the result. Wrapping it in a small library gives you an import-check you can run in
CI and, more to the point here, one you can *test* deterministically without
shelling out to an installed binary. `Fix` returns the processed bytes plus a
`changed` flag (`out != src`) so a CI step can fail when a file was not already
import-clean — the same "is this formatted?" gate `gofmt -l` provides, extended to
imports.

Create `importfix.go`:

```go
// importfix.go
package importfix

import (
	"bytes"

	"golang.org/x/tools/imports"
)

// Fix runs the goimports engine over src: it adds missing imports, removes
// unused ones, and gofmt-formats the result. filename is used only to resolve
// imports relative to a package; it need not exist on disk. changed reports
// whether the output differs from the input, which a CI gate can treat as a
// failure ("this file was not import-clean").
func Fix(filename string, src []byte) (out []byte, changed bool, err error) {
	out, err = imports.Process(filename, src, nil)
	if err != nil {
		return nil, false, err
	}
	return out, !bytes.Equal(out, src), nil
}
```

### The demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"os"

	"example.com/importfix"
)

func main() {
	src := []byte("package main\n\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n")
	out, changed, err := importfix.Fix("main.go", src)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Printf("changed=%v\n", changed)
	fmt.Print(string(out))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
changed=true
package main

import "fmt"

func main() {
	fmt.Println("hi")
}
```

(The input referenced `fmt` without importing it; `Fix` added the
`import "fmt"` and reported `changed=true`.)

### The test

The test drives `Fix` directly. The missing-import case asserts the output
contains the added import and that `changed` is true; the already-clean case
asserts `changed` is false and the output is byte-identical. The `Example` pins
the exact formatted output through its `// Output:` comment.

Create `importfix_test.go`:

```go
// importfix_test.go
package importfix

import (
	"fmt"
	"strings"
	"testing"
)

func TestFix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		src         string
		wantChanged bool
		wantSubstr  string
	}{
		{
			name:        "adds missing import",
			src:         "package p\n\nvar _ = fmt.Println\n",
			wantChanged: true,
			wantSubstr:  "import \"fmt\"",
		},
		{
			name:        "clean source unchanged",
			src:         "package p\n",
			wantChanged: false,
			wantSubstr:  "package p",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			out, changed, err := Fix("p.go", []byte(tc.src))
			if err != nil {
				t.Fatalf("Fix() unexpected err = %v", err)
			}
			if changed != tc.wantChanged {
				t.Fatalf("Fix() changed = %v, want %v (out=%q)", changed, tc.wantChanged, out)
			}
			if !strings.Contains(string(out), tc.wantSubstr) {
				t.Fatalf("Fix() out = %q, want substring %q", out, tc.wantSubstr)
			}
		})
	}
}

func ExampleFix() {
	out, changed, _ := Fix("p.go", []byte("package p\n\nvar _ = fmt.Println\n"))
	fmt.Printf("changed=%v\n%s", changed, out)
	// Output:
	// changed=true
	// package p
	//
	// import "fmt"
	//
	// var _ = fmt.Println
}
```

## Review

The install is correct when `which goimports` resolves under
`$(go env GOPATH)/bin` and `git diff go.mod` is empty afterward — proof that
`go install pkg@version` produced a binary without editing the module. The
library is correct when `Fix` adds a missing import and reports `changed=true`,
and leaves an already-clean file byte-identical with `changed=false`. The trap is
reaching for `go get golang.org/x/tools/cmd/goimports` expecting a binary — that
only edits `go.mod`; `go install ...@version` is the command that produces the
tool. Confirm with `go test -race ./...` and by reading the tool's provenance
with `go version -m "$(which goimports)"`.

## Resources

- [`golang.org/x/tools/imports`](https://pkg.go.dev/golang.org/x/tools/imports) — `Process` and `Options`, the engine behind `goimports`.
- [`cmd/go` install reference](https://pkg.go.dev/cmd/go#hdr-Compile_and_install_packages_and_dependencies) — `go install pkg@version` semantics.
- [Deprecation of `go get` for installing binaries](https://go.dev/doc/go-get-install-deprecation) — why install, not get, produces a binary.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-go-get-third-party-dependency.md](03-go-get-third-party-dependency.md) | Next: [05-indirect-dependency-marker.md](05-indirect-dependency-marker.md)
