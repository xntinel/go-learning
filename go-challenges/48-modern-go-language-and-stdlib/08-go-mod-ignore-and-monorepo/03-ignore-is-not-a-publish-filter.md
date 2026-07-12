# Exercise 3: Prove ignore Is Not a Publish Filter

The single most important senior nuance of the `ignore` directive is what it does
*not* do: it does not shrink the published module. Files under an ignored directory
are excluded from the build view but still ship in the module zip. This exercise
produces one artifact and makes two opposing observations of it — the divergence
you must internalize before you reach for `ignore` expecting a smaller download.

This module is fully self-contained: its own `go mod init`, a `require` on
`x/mod`, a fixture builder, a zip inspector, a demo, and tests that assert both
sides.

## What you'll build

```text
ignorezip/                      independent module: example.com/ignorezip
  go.mod                        go 1.25; require golang.org/x/mod
  fixture.go                    WriteFixture(dir) writes a module with an ignored generated/ subtree
  zipproof.go                   ZipEntries(dir, modulePath, version) []string via zip.CreateFromDir
  cmd/
    demo/
      main.go                   builds the fixture, prints the module zip's contents
  zipproof_test.go              zip-contains-ignored + build-view-excludes-ignored + Example
```

- Files: `fixture.go`, `zipproof.go`, `cmd/demo/main.go`, `zipproof_test.go`.
- Implement: `WriteFixture` (a module whose `go.mod` ignores `./generated`) and `ZipEntries` (build the module zip with `x/mod/zip.CreateFromDir` and list its entries).
- Test: assert the `generated/` files ARE in the zip, and — from a subprocess `go list ./...` — that the `generated` package is NOT in the build view.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
go get golang.org/x/mod@v0.37.0
```

### The two observations of one artifact

The Go 1.25 release notes are explicit: ignored files "will be ignored by the `go`
command when matching package patterns, such as `all` or `./...`, but will still
be included in module zip files." This exercise turns that sentence into two
tests over the same fixture. The fixture is a module whose `go.mod` contains
`ignore ./generated`, with a real `app` package, an `internal/store` package, and
a `generated/` subtree of two `.pb.go` files.

The first observation uses `golang.org/x/mod/zip.CreateFromDir`, the exact code
path the module proxy uses to build a module zip when someone runs
`go get your/module@version`. It writes the archive to a `bytes.Buffer`, which
`archive/zip.NewReader` then reads back. `CreateFromDir` does not read the
`ignore` directive at all — it omits only VCS directories (`.git`, `.hg`, and
friends) and files that belong to nested modules — so the `generated/` files are
present in the archive. That is the publish view.

The second observation runs `go list ./...` as a subprocess against the same
directory. Here the directive *is* honored, so `example.com/app/generated` is
absent. That is the build view. Same bytes on disk, two answers — because
`ignore` filters pattern matching, not distribution.

### Why CreateFromDir needs a module.Version

`CreateFromDir(w io.Writer, m module.Version, dir string) error` names the module
and version so it can prefix every archive entry with `module@version/`, exactly
as a real module zip is laid out (`example.com/app@v0.1.0/app.go`). `ZipEntries`
strips that prefix and sorts the names so the output is stable and comparable. The
version must be a valid semantic version; the content is otherwise unaffected by
which version you pass.

Create `fixture.go`:

```go
package ignorezip

import (
	"os"
	"path/filepath"
)

// WriteFixture materializes a module rooted at dir: a buildable app package, an
// internal package, and a generated/ subtree that the go.mod ignores. It returns
// the module path.
func WriteFixture(dir string) (string, error) {
	files := map[string]string{
		"go.mod":                  "module example.com/app\n\ngo 1.25\n\nignore ./generated\n",
		"app.go":                  "package app\n\nfunc Hello() string { return \"hi\" }\n",
		"internal/store/store.go": "package store\n\nfunc Ping() string { return \"pong\" }\n",
		"generated/api.pb.go":     "package generated\n\nfunc Gen() int { return 1 }\n",
		"generated/model.pb.go":   "package generated\n\nfunc Model() int { return 2 }\n",
	}
	for rel, body := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			return "", err
		}
	}
	return "example.com/app", nil
}
```

Create `zipproof.go`:

```go
package ignorezip

import (
	"archive/zip"
	"bytes"
	"fmt"
	"slices"
	"strings"

	"golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"
)

// ZipEntries builds a module zip for (modulePath, version) from dir using the
// same code path the module proxy uses, then returns the sorted archive entry
// names with the "module@version/" prefix stripped.
func ZipEntries(dir, modulePath, version string) ([]string, error) {
	var buf bytes.Buffer
	m := module.Version{Path: modulePath, Version: version}
	if err := modzip.CreateFromDir(&buf, m, dir); err != nil {
		return nil, fmt.Errorf("create module zip: %w", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		return nil, fmt.Errorf("read zip: %w", err)
	}
	prefix := m.Path + "@" + m.Version + "/"
	names := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		names = append(names, strings.TrimPrefix(f.Name, prefix))
	}
	slices.Sort(names)
	return names, nil
}
```

### The runnable demo

The demo builds the fixture in a temp directory and prints the contents of the
module zip a proxy would serve — including the ignored `generated/` files, making
the publish view concrete.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"os"

	"example.com/ignorezip"
)

func main() {
	dir, err := os.MkdirTemp("", "ignorezip-demo-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	modulePath, err := ignorezip.WriteFixture(dir)
	if err != nil {
		log.Fatal(err)
	}

	entries, err := ignorezip.ZipEntries(dir, modulePath, "v0.1.0")
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("published module zip contains:")
	for _, e := range entries {
		fmt.Printf("  %s\n", e)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
published module zip contains:
  app.go
  generated/api.pb.go
  generated/model.pb.go
  go.mod
  internal/store/store.go
```

### Tests

`TestZipStillContainsIgnored` asserts the publish side: the `generated/` files are
present in the zip that `CreateFromDir` produces, proving `ignore` does not filter
distribution. `TestBuildViewExcludesIgnored` asserts the build side by running a
hermetic `go list ./...` subprocess and confirming the `generated` package is
absent while `app` is present. `ExampleZipEntries` shows the full, sorted archive
listing. The two tests over one fixture are the whole lesson.

Create `zipproof_test.go`:

```go
package ignorezip

import (
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func TestZipStillContainsIgnored(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	modulePath, err := WriteFixture(dir)
	if err != nil {
		t.Fatalf("WriteFixture: %v", err)
	}
	entries, err := ZipEntries(dir, modulePath, "v0.1.0")
	if err != nil {
		t.Fatalf("ZipEntries: %v", err)
	}
	for _, want := range []string{"generated/api.pb.go", "generated/model.pb.go"} {
		if !slices.Contains(entries, want) {
			t.Fatalf("module zip is missing %q; ignore must not filter the zip.\nentries: %v", want, entries)
		}
	}
	// The build sources are there too; only VCS dirs are ever dropped.
	if !slices.Contains(entries, "app.go") {
		t.Fatalf("expected app.go in zip, got %v", entries)
	}
}

func hermeticEnv() []string {
	drop := map[string]bool{"GOFLAGS": true, "GOTOOLCHAIN": true, "GOPROXY": true}
	env := make([]string, 0, len(os.Environ())+3)
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); !drop[k] {
			env = append(env, kv)
		}
	}
	return append(env, "GOTOOLCHAIN=auto", "GOPROXY=off", "GOFLAGS=")
}

func TestBuildViewExcludesIgnored(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := WriteFixture(dir); err != nil {
		t.Fatalf("WriteFixture: %v", err)
	}
	cmd := exec.CommandContext(t.Context(), "go", "list", "./...")
	cmd.Dir = dir
	cmd.Env = hermeticEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list ./...: %v\n%s", err, out)
	}
	pkgs := strings.Fields(string(out))
	if slices.Contains(pkgs, "example.com/app/generated") {
		t.Fatalf("generated package appeared in the build view despite ignore:\n%s", out)
	}
	if !slices.Contains(pkgs, "example.com/app") {
		t.Fatalf("expected the app package in the build view:\n%s", out)
	}
}

func ExampleZipEntries() {
	dir, _ := os.MkdirTemp("", "ignorezip-example-")
	defer os.RemoveAll(dir)
	modulePath, _ := WriteFixture(dir)
	entries, _ := ZipEntries(dir, modulePath, "v0.1.0")
	for _, e := range entries {
		fmt.Println(e)
	}
	// Output:
	// app.go
	// generated/api.pb.go
	// generated/model.pb.go
	// go.mod
	// internal/store/store.go
}
```

## Review

The proof is the divergence: the same fixture yields `generated/` files present in
`ZipEntries` and the `generated` package absent from the subprocess `go list`. If
`TestZipStillContainsIgnored` ever fails, either `CreateFromDir` began honoring
the directive (it does not) or the fixture stopped ignoring — check the `go.mod`
still carries `ignore ./generated`. If `TestBuildViewExcludesIgnored` fails, the
child toolchain is older than 1.25 (hence `GOTOOLCHAIN=auto`, not `local`) or the
directive is missing.

The mistake this exercise exists to prevent is treating `ignore` as a size or
publish filter. It is not: the download is byte-for-byte the same, and
`go mod download` fetches the ignored files all the same. If the actual goal is a
smaller published module, `ignore` will disappoint; that is a separate proposal
(golang/go #76208), not this feature. Use `ignore` to keep wildcards green and
output uncluttered, and reach for the future zip-exclusion mechanism when the goal
is a leaner artifact.

## Resources

- [`golang.org/x/mod/zip`](https://pkg.go.dev/golang.org/x/mod/zip) — `CreateFromDir` and its file-selection rules (it does not honor `ignore`).
- [Go 1.25 release notes: go command](https://go.dev/doc/go1.25#go-command) — "still be included in module zip files".
- [`archive/zip`](https://pkg.go.dev/archive/zip) — `NewReader` and `Reader.File` for reading the archive back.

---

Back to [02-gomod-ignore-reconciler.md](02-gomod-ignore-reconciler.md) | Next: [../09-testing-attributes-and-output/00-concepts.md](../09-testing-attributes-and-output/00-concepts.md)
