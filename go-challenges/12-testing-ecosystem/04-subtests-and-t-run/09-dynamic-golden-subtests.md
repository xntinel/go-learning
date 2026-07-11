# Exercise 9: Generating Subtests from testdata Golden Files

The last pattern makes coverage grow without editing code: discover every input
file under `testdata/`, run one `t.Run` per file, and compare the rendered output
against a checked-in `.golden` sibling. Adding a case is `git add
testdata/new.json`. This exercise builds a JSON report renderer and tests it with
dynamically generated subtests and a `-update` flag to regenerate goldens.

This module is fully self-contained: its own `go mod init`, renderer, demo, and
tests. Nothing here imports any other exercise.

## What you'll build

```text
jsonreport/                 independent module: example.com/jsonreport
  go.mod                    go 1.26
  render.go                 func Render([]byte) ([]byte, error) — stable sorted report
  cmd/
    demo/
      main.go               runnable demo: render a config object
  render_test.go            discover inputs, one t.Run per file, golden compare, -update
```

- Files: `render.go`, `cmd/demo/main.go`, `render_test.go`.
- Implement: `Render(input []byte) ([]byte, error)` producing a deterministic,
  sorted `key = value` report from a JSON object.
- Test: discover input files, `t.Run(filepath.Base(f))` per file, compare to a
  `.golden` sibling; a package-level `-update` flag rewrites goldens.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/jsonreport/cmd/demo
cd ~/go-exercises/jsonreport
go mod init example.com/jsonreport
```

### The golden-file workflow and dynamic subtests

The test does not hard-code a table of cases. It reads a directory, filters for
`.json` inputs, and calls `t.Run(name, …)` once per file — so the set of subtests
is whatever is on disk. Each subtest renders its input and compares the bytes to a
sibling `name.golden`. A mismatch reports the *file name*, so a failure is
self-locating: you know exactly which fixture broke. The `-update` flag
(`var update = flag.Bool("update", …)`) is the idiomatic regeneration switch:
`go test -update` rewrites every golden from the current output, which you review
in the diff and commit. This is how renderers, formatters, and serializers get
exhaustive coverage cheaply — a new edge case is a new file, not new code.

Two conventions matter. In a real repository the inputs and goldens live in a
`testdata/` directory, which the Go tool treats specially (it is ignored by the
build, so arbitrary fixtures do not break compilation). And the renderer's output
must be *deterministic* for golden comparison to be stable: this `Render` sorts
keys and decodes numbers with `json.Decoder.UseNumber` so `42` renders as `42`,
not `42.000000`, and map iteration order never leaks into the output.

Because this module must gate as a standalone unit with no checked-in fixtures,
the test *seeds* its input and golden files into a `t.TempDir()` before
discovering them. That is the only difference from a production suite, where those
files are committed under `testdata/`; the discovery loop — `os.ReadDir`, filter,
`t.Run` per file, golden compare — is identical.

Create `render.go`:

```go
package jsonreport

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// Render parses a JSON object and renders it as a stable, key-sorted
// "key = value" report — deterministic output suitable for golden-file testing.
// Numbers are decoded with UseNumber so they render in their original form.
func Render(input []byte) ([]byte, error) {
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("decode json: %w", err)
	}

	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var buf bytes.Buffer
	for _, k := range keys {
		fmt.Fprintf(&buf, "%s = %v\n", k, m[k])
	}
	return buf.Bytes(), nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/jsonreport"
)

func main() {
	out, err := jsonreport.Render([]byte(`{"service":"api","port":8080,"tls":true}`))
	if err != nil {
		panic(err)
	}
	fmt.Print(string(out))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
port = 8080
service = api
tls = true
```

### Tests

`TestGolden` seeds a temp directory with input/golden pairs (the stand-in for a
committed `testdata/`), discovers the `.json` inputs with `os.ReadDir`, and runs
one subtest per file named by `filepath.Base`. Each subtest renders its input,
honors `-update` by rewriting the golden, then compares bytes and reports the file
name on mismatch. The `ran == 0` guard fails loudly if discovery found nothing —
a golden suite that silently tests zero files is a common, dangerous mistake.

Create `render_test.go`:

```go
package jsonreport

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "regenerate .golden files from current output")

// seedTestdata writes input/golden pairs into a temp dir and returns it. In a
// real suite these files are committed under testdata/; seeding keeps this module
// self-contained so it gates alone.
func seedTestdata(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	fixtures := map[string]struct{ input, golden string }{
		"person": {`{"name":"ada","age":37}`, "age = 37\nname = ada\n"},
		"flags":  {`{"z":true,"a":"x"}`, "a = x\nz = true\n"},
	}
	for name, f := range fixtures {
		if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(f.input), 0o644); err != nil {
			t.Fatalf("seed input %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name+".golden"), []byte(f.golden), 0o644); err != nil {
			t.Fatalf("seed golden %s: %v", name, err)
		}
	}
	return dir
}

func TestGolden(t *testing.T) {
	dir := seedTestdata(t)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}

	ran := 0
	for _, e := range entries {
		name := e.Name()
		if filepath.Ext(name) != ".json" {
			continue
		}
		ran++
		t.Run(name, func(t *testing.T) {
			input, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatalf("read input: %v", err)
			}
			got, err := Render(input)
			if err != nil {
				t.Fatalf("Render(%s): %v", name, err)
			}

			goldenPath := filepath.Join(dir, strings.TrimSuffix(name, ".json")+".golden")
			if *update {
				if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
					t.Fatalf("update golden: %v", err)
				}
			}
			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("%s: output does not match golden\n got: %q\nwant: %q", name, got, want)
			}
		})
	}
	if ran == 0 {
		t.Fatal("no .json testdata cases discovered")
	}
}

func ExampleRender() {
	out, _ := Render([]byte(`{"b":2,"a":1}`))
	os.Stdout.Write(out)
	// Output:
	// a = 1
	// b = 2
}
```

## Running -update

To regenerate the goldens after an intentional change to `Render`'s output format:

```bash
go test -run TestGolden -update
```

Review the resulting diff in the `.golden` files and commit it. Never run
`-update` blind — it makes any current output "correct" and can bless a regression.

## Review

Dynamic subtests turn the file system into the test table: `os.ReadDir` plus
`t.Run(filepath.Base(f))` means coverage grows by dropping a fixture under
`testdata/`, and a mismatch names the exact file so failures are self-locating.
The renderer must be deterministic — sorted keys, `UseNumber` so numbers keep
their form — or the golden comparison flakes on nothing. Two guards earn their
keep: the `-update` flag makes regeneration a reviewed diff rather than a manual
edit, and the `ran == 0` check fails loudly so a mis-globbed suite that tests zero
files cannot masquerade as green. This module seeds its fixtures into a temp dir to
stay self-contained; a production suite commits them under `testdata/` and the
discovery loop is otherwise identical.

## Resources

- [testing.T.Run — pkg.go.dev](https://pkg.go.dev/testing#T.Run)
- [os.ReadDir — pkg.go.dev](https://pkg.go.dev/os#ReadDir)
- [cmd/go — testdata directory handling](https://pkg.go.dev/cmd/go#hdr-Test_packages)
- [encoding/json.Decoder.UseNumber — pkg.go.dev](https://pkg.go.dev/encoding/json#Decoder.UseNumber)

---

Back to [00-concepts.md](00-concepts.md) | Next: [../05-benchmarks/00-concepts.md](../05-benchmarks/00-concepts.md)
