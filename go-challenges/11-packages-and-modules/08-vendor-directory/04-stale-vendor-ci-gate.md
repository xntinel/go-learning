# Exercise 4: A pre-merge CI gate that fails on a stale vendor/

The consistency check from the previous exercise becomes a real CI gate: a CLI
that reads `go.mod` and `vendor/modules.txt`, renders a human-readable drift
table, and exits non-zero when the vendored tree is stale — a lightweight,
network-free stand-in for `go mod vendor --diff` in a hermetic pipeline that must
not touch a proxy.

This module is fully self-contained: its own `go mod init`, a bundled drift
checker, a flag-driven CLI, and both unit and exec-based tests. Nothing here
imports another exercise.

## What you'll build

```text
vendorgate/                  independent module: example.com/vendorgate
  go.mod                     go 1.26 (requires golang.org/x/mod)
  vendorgate.go              Gate([]byte,[]byte,io.Writer) int; Main([]string,io.Writer,io.Writer) int
  cmd/
    demo/
      main.go                the gate CLI, seeded with a deliberately-stale sample
  vendorgate_test.go         Gate table + Main flag path + exec smoke on the built binary
```

- Files: `vendorgate.go`, `cmd/demo/main.go`, `vendorgate_test.go`.
- Implement: `Gate`, which renders drift with `text/tabwriter` and returns an exit code, and `Main`, which parses `-gomod`/`-modules-txt` flags with `flag.NewFlagSet`, reads the files with `os.ReadFile`, and calls `Gate`.
- Test: a table driving `Gate` with a `bytes.Buffer` for in-sync (exit 0) and drifted (exit 1) inputs asserting the rendered table; a `Main` test over temp files; and an exec smoke test that builds the CLI and runs it against a stale vendor, asserting a non-zero exit.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/11-packages-and-modules/08-vendor-directory/04-stale-vendor-ci-gate/cmd/demo
cd go-solutions/11-packages-and-modules/08-vendor-directory/04-stale-vendor-ci-gate
go get golang.org/x/mod
```

### Why exit codes, and why render with tabwriter

A CI gate communicates two things: a machine-readable pass/fail via the process
exit code, and a human-readable explanation on stdout for the engineer reading
the failed job. Those are separate concerns, so the core is a function that
*returns* an exit code and *writes* to an `io.Writer` — never one that calls
`os.Exit` or writes to `os.Stdout` directly, because that shape cannot be tested.
Only the outermost `main` translates the returned code into `os.Exit`.

The drift table is columnar, and columns that do not line up are unreadable in a
CI log. `text/tabwriter` turns tab-separated cells into aligned columns: you write
`"MODULE\tDRIFT\tGO.MOD\tVENDOR\n"` rows through it, call `Flush`, and it pads each
column to the width of its widest cell. This is the standard way Go tooling
(including `go` subcommands) formats aligned terminal output.

### Why a separate Gate and Main

`Gate([]byte, []byte, io.Writer) int` is the pure decision: given the two file
contents, decide and render. `Main([]string, io.Writer, io.Writer) int` is the
I/O shell: parse flags with a private `flag.NewFlagSet` (never the global
`flag.CommandLine`, so tests do not collide), read the files with `os.ReadFile`,
and delegate to `Gate`. Splitting them means the decision logic is tested with
in-memory bytes and no filesystem, while the flag-and-file plumbing is tested
separately over a `t.TempDir()`.

Create `vendorgate.go`:

```go
package vendorgate

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"text/tabwriter"

	"golang.org/x/mod/modfile"
)

type driftKind int

const (
	missing driftKind = iota
	extra
	mismatch
)

func (k driftKind) String() string {
	switch k {
	case missing:
		return "missing-in-vendor"
	case extra:
		return "extra-in-vendor"
	case mismatch:
		return "version-mismatch"
	default:
		return "unknown"
	}
}

type drift struct {
	modPath   string
	kind      driftKind
	goModVer  string
	vendorVer string
}

// Gate compares go.mod against vendor/modules.txt, renders any drift to out,
// and returns an exit code: 0 = up to date, 1 = stale, 2 = internal error.
func Gate(goMod, modulesTxt []byte, out io.Writer) int {
	drifts, err := check(goMod, modulesTxt)
	if err != nil {
		fmt.Fprintln(out, "gate error:", err)
		return 2
	}
	if len(drifts) == 0 {
		fmt.Fprintln(out, "vendor/ is up to date with go.mod")
		return 0
	}
	fmt.Fprintf(out, "vendor/ is STALE: %d finding(s)\n", len(drifts))
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "MODULE\tDRIFT\tGO.MOD\tVENDOR")
	for _, d := range drifts {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", d.modPath, d.kind, dash(d.goModVer), dash(d.vendorVer))
	}
	tw.Flush()
	fmt.Fprintln(out, "fix: run `go mod vendor` and commit vendor/")
	return 1
}

// Main is the CLI entry point: it parses flags, reads the files, and gates.
func Main(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("vendorgate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	goModPath := fs.String("gomod", "go.mod", "path to go.mod")
	modulesPath := fs.String("modules-txt", "vendor/modules.txt", "path to vendor/modules.txt")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	goMod, err := os.ReadFile(*goModPath)
	if err != nil {
		fmt.Fprintln(stderr, "read go.mod:", err)
		return 2
	}
	modulesTxt, err := os.ReadFile(*modulesPath)
	if err != nil {
		fmt.Fprintln(stderr, "read modules.txt:", err)
		return 2
	}
	return Gate(goMod, modulesTxt, stdout)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func check(goMod, modulesTxt []byte) ([]drift, error) {
	mf, err := modfile.Parse("go.mod", goMod, nil)
	if err != nil {
		return nil, fmt.Errorf("parse go.mod: %w", err)
	}
	direct := map[string]string{}
	for _, r := range mf.Require {
		if !r.Indirect {
			direct[r.Mod.Path] = r.Mod.Version
		}
	}
	explicit, err := explicitModules(bytes.NewReader(modulesTxt))
	if err != nil {
		return nil, err
	}
	var drifts []drift
	for _, path := range slices.Sorted(maps.Keys(direct)) {
		vv, ok := explicit[path]
		switch {
		case !ok:
			drifts = append(drifts, drift{modPath: path, kind: missing, goModVer: direct[path]})
		case vv != direct[path]:
			drifts = append(drifts, drift{modPath: path, kind: mismatch, goModVer: direct[path], vendorVer: vv})
		}
	}
	for _, path := range slices.Sorted(maps.Keys(explicit)) {
		if _, ok := direct[path]; !ok {
			drifts = append(drifts, drift{modPath: path, kind: extra, vendorVer: explicit[path]})
		}
	}
	slices.SortFunc(drifts, func(a, b drift) int {
		if a.modPath != b.modPath {
			return strings.Compare(a.modPath, b.modPath)
		}
		return int(a.kind) - int(b.kind)
	})
	return drifts, nil
}

func explicitModules(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	var curPath, curVer string
	for sc.Scan() {
		line := sc.Text()
		if rest, ok := strings.CutPrefix(line, "# "); ok {
			if before, _, found := strings.Cut(rest, " => "); found {
				rest = before
			}
			fields := strings.Fields(rest)
			curPath, curVer = "", ""
			if len(fields) > 0 {
				curPath = fields[0]
			}
			if len(fields) > 1 {
				curVer = fields[1]
			}
			continue
		}
		if rest, ok := strings.CutPrefix(line, "## "); ok {
			for _, tok := range strings.Split(rest, ";") {
				if strings.TrimSpace(tok) == "explicit" && curPath != "" {
					out[curPath] = curVer
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read modules.txt: %w", err)
	}
	return out, nil
}
```

### The runnable demo (the gate binary)

The demo is the gate CLI, seeded with a deliberately-stale sample so that running
it is a self-contained demonstration of a failing gate: `go.mod` requires
`golang.org/x/mod v0.37.0`, but the vendored manifest still records `v0.36.0`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"os"

	"example.com/vendorgate"
)

const goMod = `module example.com/service

go 1.26

require golang.org/x/mod v0.37.0
`

const modulesTxt = `# golang.org/x/mod v0.36.0
## explicit; go 1.23
golang.org/x/mod/modfile
`

func main() {
	os.Exit(vendorgate.Gate([]byte(goMod), []byte(modulesTxt), os.Stdout))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
vendor/ is STALE: 1 finding(s)
MODULE            DRIFT             GO.MOD   VENDOR
golang.org/x/mod  version-mismatch  v0.37.0  v0.36.0
fix: run `go mod vendor` and commit vendor/
```

### Tests

`TestGate` drives the pure decision with a `bytes.Buffer`, asserting both the
exit code and the rendered table for in-sync and drifted inputs. `TestMainFlags`
writes fixture files into `t.TempDir()`, runs `Main` with `-gomod`/`-modules-txt`
pointing at them, and checks the exit code. `TestGateBinaryExits` builds
`./cmd/demo` and runs it, asserting a non-zero exit — the exec smoke that proves
the built binary fails a stale vendor. It skips when the toolchain is absent.

Create `vendorgate_test.go`:

```go
package vendorgate

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGate(t *testing.T) {
	t.Parallel()
	inSyncMod := "module m\n\ngo 1.26\n\nrequire golang.org/x/mod v0.37.0\n"
	inSyncTxt := "# golang.org/x/mod v0.37.0\n## explicit; go 1.23\ngolang.org/x/mod/modfile\n"
	staleTxt := "# golang.org/x/mod v0.36.0\n## explicit; go 1.23\ngolang.org/x/mod/modfile\n"

	t.Run("in sync exits zero", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		if code := Gate([]byte(inSyncMod), []byte(inSyncTxt), &buf); code != 0 {
			t.Fatalf("exit = %d; want 0\n%s", code, buf.String())
		}
		if !strings.Contains(buf.String(), "up to date") {
			t.Fatalf("missing up-to-date message: %q", buf.String())
		}
	})

	t.Run("stale exits one", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		code := Gate([]byte(inSyncMod), []byte(staleTxt), &buf)
		if code != 1 {
			t.Fatalf("exit = %d; want 1", code)
		}
		out := buf.String()
		if !strings.Contains(out, "version-mismatch") ||
			!strings.Contains(out, "v0.37.0") || !strings.Contains(out, "v0.36.0") {
			t.Fatalf("drift table missing expected cells:\n%s", out)
		}
	})
}

func TestMainFlags(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	modPath := filepath.Join(dir, "go.mod")
	txtPath := filepath.Join(dir, "modules.txt")
	if err := os.WriteFile(modPath, []byte("module m\n\ngo 1.26\n\nrequire golang.org/x/mod v0.37.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(txtPath, []byte("# golang.org/x/mod v0.36.0\n## explicit\ngolang.org/x/mod/modfile\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := Main([]string{"-gomod", modPath, "-modules-txt", txtPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("Main exit = %d; want 1 (stale)\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
}

func TestMainMissingFile(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := Main([]string{"-gomod", filepath.Join(t.TempDir(), "nope.mod")}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("Main exit = %d; want 2 for missing file", code)
	}
}

func TestGateBinaryExits(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable; skipping exec smoke")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "gate.bin")
	build := exec.Command("go", "build", "-o", bin, "./cmd/demo")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build gate: %v\n%s", err, out)
	}
	err := exec.Command(bin).Run()
	if err == nil {
		t.Fatal("gate binary exited 0 on a stale vendor; want non-zero")
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 {
		t.Fatalf("gate exit = %v; want exit code 1", err)
	}
}
```

## Review

The gate is correct when its two responsibilities stay separate: `Gate` decides
and renders to an `io.Writer`, returning a code, while only `main` calls
`os.Exit`. That separation is what makes `TestGate` able to assert the rendered
table from a buffer and `TestGateBinaryExits` able to confirm the real binary
propagates a non-zero code. Using a private `flag.NewFlagSet` rather than the
global `flag.CommandLine` is not cosmetic: parallel tests that each parse flags
would corrupt shared global state otherwise. The `tabwriter` columns must be
flushed — a forgotten `Flush` silently drops the buffered rows, which is a classic
"the table is empty but no error" bug.

## Resources

- [`text/tabwriter`](https://pkg.go.dev/text/tabwriter) — aligned columnar output, `NewWriter` and `Flush`.
- [`flag.NewFlagSet`](https://pkg.go.dev/flag#NewFlagSet) — a private flag set with `ContinueOnError` for testable CLIs.
- [`go mod vendor`](https://go.dev/ref/mod#go-mod-vendor) — the command whose `--diff`-style check this gate approximates offline.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-vendor-license-scanner.md](05-vendor-license-scanner.md)
