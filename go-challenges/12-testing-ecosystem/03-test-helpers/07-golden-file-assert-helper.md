# Exercise 7: A Golden-File Assertion Helper with -update

When a function renders a non-trivial artifact — a serialized API response, a
rendered template, a report — asserting it field by field is brittle. A
golden-file helper compares the produced bytes against a committed
`testdata/<name>.golden` file and rewrites that file on demand under
`go test -update`. This module builds `assertGolden` with the standard `-update`
flag and a readable diff on mismatch.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
goldenhelper/                independent module: example.com/goldenhelper
  go.mod                     go 1.26
  render.go                  Render: serialize a health Report to deterministic JSON
  testdata/
    health.golden            the committed golden bytes
  cmd/
    demo/
      main.go                prints the rendered report
  render_test.go             -update flag; assertGolden + goldenMatches; positive and negative subtests
```

- Files: `render.go`, `testdata/health.golden`, `cmd/demo/main.go`, `render_test.go`.
- Implement: `Render(Report) []byte` producing stable, indented JSON with a trailing newline.
- Test: a package-level `-update` flag; `assertGolden(t, name, got)` that compares against `testdata/<name>.golden` (or rewrites it under `-update`); a positive subtest that matches and a negative one that mutates the bytes and asserts a mismatch is detected.
- Verify: `go test -count=1 -race ./...`

### Why `-update` and `testdata/`

A golden helper has two jobs that must not be conflated. On a normal `go test` run
it *compares* `got` against the committed golden bytes and fails with a diff if
they differ — this is what catches an unintended change to a serialized response.
When you *intend* to change the output, `go test -update` *rewrites* the golden so
you can review the diff in version control. The `-update` guard is what keeps the
helper honest: a helper that always writes the golden can never fail, which defeats
the entire purpose. The flag is a package-level `var update = flag.Bool("update",
...)`; `go test` calls `flag.Parse`, so `-update` is picked up automatically.

The directory name `testdata` is special to the `go` tool: it is excluded from
package builds, so fixtures there never become part of your compiled binary. It is
the standard home for golden files, sample inputs, and any test-only asset.

The rendered artifact must be *deterministic* — same input, identical bytes — or
the golden is useless. JSON map iteration order is randomized, so `Render`
serializes a struct (field order is stable) via `json.MarshalIndent`, and appends
a single trailing newline so the golden file is a well-formed text file.

Create `render.go`:

```go
package goldenhelper

import "encoding/json"

// Check is one health probe result.
type Check struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
}

// Report is a service health snapshot rendered for an API response.
type Report struct {
	Service string  `json:"service"`
	Status  string  `json:"status"`
	Checks  []Check `json:"checks"`
}

// Render serializes r to stable, indented JSON with a trailing newline.
func Render(r Report) []byte {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		panic(err) // Report contains only JSON-safe types
	}
	return append(data, '\n')
}
```

### The committed golden file

This is the expected output for the fixed report the tests render. It lives under
`testdata/` and is what a normal run compares against.

Create `testdata/health.golden`:

```text
{
  "service": "payments",
  "status": "healthy",
  "checks": [
    {
      "name": "db",
      "ok": true
    },
    {
      "name": "cache",
      "ok": true
    }
  ]
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"os"

	"example.com/goldenhelper"
)

func main() {
	r := goldenhelper.Report{
		Service: "payments",
		Status:  "healthy",
		Checks: []goldenhelper.Check{
			{Name: "db", OK: true},
			{Name: "cache", OK: true},
		},
	}
	os.Stdout.Write(goldenhelper.Render(r))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{
  "service": "payments",
  "status": "healthy",
  "checks": [
    {
      "name": "db",
      "ok": true
    },
    {
      "name": "cache",
      "ok": true
    }
  ]
}
```

### The tests

`goldenMatches(name, got)` is the pure comparison: it reads the golden file and
reports whether `got` equals it, returning the wanted bytes for a diff. `assertGolden`
wraps it — under `-update` it writes the golden and returns; otherwise it compares
and, on mismatch, `t.Fatalf`s with a got-versus-want diff. Factoring the comparison
into `goldenMatches` is what lets the *negative* subtest assert a mismatch is
detected without failing the real test: it calls `goldenMatches` with deliberately
mutated bytes and checks the boolean. The positive subtest renders the fixed report
and asserts it matches the committed golden.

Create `render_test.go`:

```go
package goldenhelper

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden files")

func goldenPath(name string) string {
	return filepath.Join("testdata", name+".golden")
}

// goldenMatches reports whether got equals the golden bytes, returning them.
func goldenMatches(name string, got []byte) (bool, []byte, error) {
	want, err := os.ReadFile(goldenPath(name))
	if err != nil {
		return false, nil, err
	}
	return bytes.Equal(got, want), want, nil
}

// assertGolden compares got against testdata/<name>.golden, or rewrites it
// under -update.
func assertGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(goldenPath(name), got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	ok, want, err := goldenMatches(name, got)
	if err != nil {
		t.Fatalf("read golden %s (run: go test -update): %v", goldenPath(name), err)
	}
	if !ok {
		t.Fatalf("golden mismatch for %q:\n--- got ---\n%s--- want ---\n%s", name, got, want)
	}
}

func fixedReport() Report {
	return Report{
		Service: "payments",
		Status:  "healthy",
		Checks: []Check{
			{Name: "db", OK: true},
			{Name: "cache", OK: true},
		},
	}
}

func TestRenderMatchesGolden(t *testing.T) {
	assertGolden(t, "health", Render(fixedReport()))
}

func TestGoldenDetectsMismatch(t *testing.T) {
	mutated := bytes.Replace(Render(fixedReport()), []byte("healthy"), []byte("degraded"), 1)
	ok, _, err := goldenMatches("health", mutated)
	if err != nil {
		t.Fatalf("goldenMatches: %v", err)
	}
	if ok {
		t.Fatal("mutated output unexpectedly matched the golden")
	}
}

func ExampleRender() {
	out := Render(Report{Service: "auth", Status: "healthy", Checks: nil})
	fmt.Print(string(out))
	// Output:
	// {
	//   "service": "auth",
	//   "status": "healthy",
	//   "checks": null
	// }
}
```

## Review

The helper is correct when a normal run compares and only `-update` writes — verify
by running `go test` (compares, passes against the committed golden), then
`go test -update` (rewrites, still passes), then `go test` again. `Render` must be
deterministic: it serializes a struct, not a map, and appends exactly one trailing
newline so the file round-trips. `TestGoldenDetectsMismatch` proves the comparison
actually rejects a wrong artifact — asserting the failure path through the pure
`goldenMatches` rather than through `assertGolden`, so testing the negative does not
fail the suite. The mistakes to avoid: a helper with no `-update` guard (can never
fail), and reading or writing a shared golden path from concurrent subtests, which
races under `-race` — give concurrent cases distinct names.

## Resources

- [flag.Bool](https://pkg.go.dev/flag#Bool) — the `-update` flag, parsed by `go test`.
- [cmd/go testdata directory](https://pkg.go.dev/cmd/go#hdr-Test_packages) — why `testdata` is ignored by the build.
- [bytes.Equal](https://pkg.go.dev/bytes#Equal) — the exact-bytes comparison behind the helper.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-setenv-config-helper.md](08-setenv-config-helper.md)
