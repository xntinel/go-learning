# Exercise 7: Reproduce The CI Build By Disabling The Workspace

The classic multi-module bug: a service imports a symbol that exists only in your
uncommitted local copy of the library. The workspace resolves the library from
disk, so it compiles for you; CI resolves the committed, published version, which
lacks the symbol, so it fails. `GOWORK=off` drops the overlay and forces every
module to resolve through its own `go.mod` — the exact resolution CI performs —
so you catch the failure before you push. This exercise builds the committed,
`GOWORK=off`-clean state and the Makefile-style parity target that keeps it clean.

## What you'll build

```text
platform/                      gated module: example.com/platform
  go.mod                       go 1.26
  health/
    health.go                  package health; Line (committed API only)
  probe/
    probe.go                   package probe; Report uses only committed health symbols
    probe_test.go              asserts the committed build is green
  cmd/
    demo/
      main.go                  prints a health report
```

- Files: `health/health.go`, `probe/probe.go`, `probe/probe_test.go`, `cmd/demo/main.go`.
- Implement: `health.Line(service string, ok bool) string` (committed) and `probe.Report` using only it.
- Test: the committed module builds and passes — the state that survives `GOWORK=off`.
- Verify: a `GOWORK=off go build ./...` parity target passes; the uncommitted-symbol variant fails it exactly as CI would.

Set up the gated module:

```bash
mkdir -p ~/platform/health ~/platform/probe ~/platform/cmd/demo
cd ~/platform
go mod init example.com/platform
go mod edit -go=1.26
```

### The bug, and GOWORK=off as the reproducer

Suppose you add a helper `LineWithLatency` to the local library and call it from
the probe, but you have not committed or tagged the library. With the workspace
active the probe resolves the library from disk and builds. Push only the probe,
and CI resolves the library at its committed version — which has no
`LineWithLatency` — and the build breaks:

```text
# with the workspace active (your machine): builds
$ go build ./...

# the way CI resolves it (no go.work): fails on the uncommitted symbol
$ GOWORK=off go build ./...
probe/probe.go:9:20: undefined: health.LineWithLatency
```

`GOWORK=off` disables the workspace entirely, so each module resolves through its
own `go.mod` at the committed versions — byte-for-byte the resolution CI uses. Run
it before every push and the "works on my machine" gap closes. Make it a target so
the parity check is one command and CI can run the identical thing:

```makefile
# Makefile
.PHONY: ci-parity
ci-parity:
	GOWORK=off go build ./...
	GOWORK=off go vet ./...
	GOWORK=off go test -count=1 ./...
```

The passing state — and the gated artifact below — is the committed one: the
probe uses only symbols that exist in the committed library, so `GOWORK=off`
builds green. `LineWithLatency` is described but never added, mirroring a symbol
that lives only in your working tree.

Create `health/health.go` — the committed API:

```go
// health/health.go
package health

// Line renders a one-line health status for a service. This is the committed,
// published API; a probe may rely only on symbols that exist here.
func Line(service string, ok bool) string {
	status := "FAIL"
	if ok {
		status = "OK"
	}
	return service + ": " + status
}
```

Create `probe/probe.go` — using only committed symbols:

```go
// probe/probe.go
package probe

import (
	"sort"
	"strings"

	"example.com/platform/health"
)

// Report renders a sorted, newline-joined health report for the given services.
// It uses only health.Line, so it builds identically with or without the workspace.
func Report(results map[string]bool) string {
	names := make([]string, 0, len(results))
	for name := range results {
		names = append(names, name)
	}
	sort.Strings(names)

	lines := make([]string, len(names))
	for i, name := range names {
		lines[i] = health.Line(name, results[name])
	}
	return strings.Join(lines, "\n")
}
```

### The demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/platform/probe"
)

func main() {
	fmt.Println(probe.Report(map[string]bool{
		"greeter": true,
		"billing": false,
	}))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
billing: FAIL
greeter: OK
```

### Tests

The test asserts the committed build's behavior — the state that must survive
`GOWORK=off`. Deterministic ordering (the report sorts names) lets the assertion
be exact.

Create `probe/probe_test.go`:

```go
// probe/probe_test.go
package probe

import (
	"fmt"
	"testing"
)

func TestReport(t *testing.T) {
	t.Parallel()

	got := Report(map[string]bool{
		"greeter": true,
		"billing": false,
		"auth":    true,
	})
	want := "auth: OK\nbilling: FAIL\ngreeter: OK"
	if got != want {
		t.Fatalf("Report =\n%q\nwant\n%q", got, want)
	}
}

func TestReportEmpty(t *testing.T) {
	t.Parallel()
	if got := Report(nil); got != "" {
		t.Fatalf("Report(nil) = %q, want empty", got)
	}
}

func ExampleReport() {
	fmt.Println(Report(map[string]bool{"api": true}))
	// Output: api: OK
}
```

## Review

The gap this closes is that the workspace can resolve an uncommitted symbol or an
unpublished version, so a green local build says nothing about CI, which sees only
committed `go.mod` versions and no `go.work`. `GOWORK=off` reproduces exactly that
resolution locally: run it before pushing and the failure surfaces on your machine
instead of in the pipeline. The gated artifact is the committed state — the probe
depends only on symbols in the published library — so `GOWORK=off go build` is
green; the moment it reaches for an uncommitted `LineWithLatency`, the parity
target fails with `undefined:` just as CI would. Wire the `GOWORK=off` build, vet,
and test into a Makefile target so the check is one reproducible command shared by
you and CI.

## Resources

- [Go Modules Reference — Workspaces and GOWORK](https://go.dev/ref/mod#workspaces) — `GOWORK=off` and single-module resolution.
- [`go help environment`](https://pkg.go.dev/cmd/go#hdr-Environment_variables) — the `GOWORK` variable and its `off` value.
- [`go build`](https://pkg.go.dev/cmd/go#hdr-Compile_packages_and_dependencies) — how a build resolves packages from the active module graph.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-godebug-directive-workspace-migration.md](08-godebug-directive-workspace-migration.md)
