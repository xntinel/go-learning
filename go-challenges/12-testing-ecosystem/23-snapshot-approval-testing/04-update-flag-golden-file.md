# Exercise 4: Implement the -update flag and golden-file workflow

Inline literals do not scale, and hand-editing them is error-prone. The canonical
Go idiom moves the expectation into `testdata/<name>.golden`, reads it with a
helper, and regenerates it with a package-level `-update` flag. This module
replaces the original lesson's hand-waved `UPDATE=1` mention with a real, typed
implementation and explains why the golden lives under `testdata/`.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
goldenflag/                independent module: example.com/goldenflag
  go.mod                   go 1.26
  format.go                User, Render(User) []byte (JSON + trailing newline)
  testdata/
    user.golden            committed reference output
  cmd/
    demo/
      main.go              renders a sample user to stdout
  format_test.go           -update flag, goldenFile helper, TempDir write path
```

Files: `format.go`, `testdata/user.golden`, `cmd/demo/main.go`, `format_test.go`.
Implement: `Render(User) []byte` producing indented JSON with exactly one trailing newline.
Test: a `goldenFile(t, name, got)` helper that writes under `-update` and otherwise reads and byte-compares, naming the golden path and the regenerate command on mismatch; plus a `TempDir` round-trip of the write path.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/goldenflag/cmd/demo ~/go-exercises/goldenflag/testdata
cd ~/go-exercises/goldenflag
go mod init example.com/goldenflag
```

### The -update flag, the helper, and the testdata rule

The idiom rests on one package-level flag:

```go
var update = flag.Bool("update", false, "regenerate golden files in testdata/")
```

The `go test` binary parses its own flags before your tests run, so `go test
-update` sets this with no `flag.Parse()` of your own. A typed flag is preferable
to an `UPDATE=1` environment variable: it is discoverable (`go test -args -h`
lists it), it is namespaced with the other test flags, and it cannot be set by a
stray inherited env var in CI. A single `goldenFile` helper centralizes the
read/write/compare logic so every assertion is one line at the call site. Under
`*update` it creates `testdata/` with `MkdirAll(..., 0o755)` and writes the bytes
with `WriteFile(..., 0o644)`; otherwise it reads the committed golden and compares
bytes exactly. On mismatch it prints the golden *path* and the exact `-update`
command — that path is the difference between a golden suite a reviewer can act on
and one that infuriates.

The golden lives under `testdata/` for a concrete reason: the `go` tool ignores
any directory named `testdata`, so the file is never compiled, vetted, or mistaken
for a package. It is a fixture, committed like source and read at test time.

Trailing-newline policy matters for byte equality. `Render` ends its output with
exactly one `\n`, and the golden file on disk carries exactly that. Most editors
and `json.Encoder.Encode` add a trailing newline, so matching that convention here
avoids the classic "looks identical but bytes differ" failure. `Render` builds the
newline explicitly by appending `'\n'` to `MarshalIndent`'s output, which never
adds one itself.

Create `format.go`:

```go
package goldenflag

import (
	"encoding/json"
	"fmt"
)

// User is the record pinned as a golden file.
type User struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// Render returns indented JSON for u with exactly one trailing newline, which is
// the golden file's trailing-newline contract. MarshalIndent never adds the
// newline, so Render appends it explicitly.
func Render(u User) []byte {
	data, err := json.MarshalIndent(u, "", "  ")
	if err != nil {
		// A struct of strings cannot fail to marshal; treat any failure as a bug.
		panic(fmt.Sprintf("render: marshal: %v", err))
	}
	return append(data, '\n')
}
```

Now the committed golden. In a real project you generate this once with
`go test -update` and commit it; here it is provided so the test has a reference
to read.

Create `testdata/user.golden`:

```text
{
  "id": "u1",
  "name": "Alice",
  "email": "alice@example.com"
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"os"

	"example.com/goldenflag"
)

func main() {
	u := goldenflag.User{ID: "u1", Name: "Alice", Email: "alice@example.com"}
	os.Stdout.Write(goldenflag.Render(u))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{
  "id": "u1",
  "name": "Alice",
  "email": "alice@example.com"
}
```

### Tests

`TestRenderGolden` renders the sample and runs it through `goldenFile`, which
compares against `testdata/user.golden` on the normal path.
`TestGoldenWriteRoundTrip` exercises the write path against a `t.TempDir` (so it
never clobbers the committed fixture): it writes, reads back, and asserts byte
equality, proving `MkdirAll`/`WriteFile` round-trip the exact bytes.

Create `format_test.go`:

```go
package goldenflag

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "regenerate golden files in testdata/")

// goldenFile compares got against testdata/name, or regenerates it under -update.
func goldenFile(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run: go test -run %s -update)", path, err, t.Name())
	}
	if !bytes.Equal(want, got) {
		t.Fatalf("golden mismatch for %s (run: go test -run %s -update to regenerate)\n--- got ---\n%s\n--- want ---\n%s",
			path, t.Name(), got, want)
	}
}

func TestRenderGolden(t *testing.T) {
	got := Render(User{ID: "u1", Name: "Alice", Email: "alice@example.com"})
	goldenFile(t, "user.golden", got)
}

func TestGoldenWriteRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "out.golden")
	got := Render(User{ID: "u9", Name: "Zoe", Email: "zoe@example.com"})

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, got, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	back, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(back, got) {
		t.Fatalf("round-trip mismatch:\ngot:\n%s\nback:\n%s", got, back)
	}
}
```

## Review

The pattern is correct when a default `go test` run only reads and compares, and
only `-update` writes — so CI, which never passes `-update`, can only fail on a
real drift. The golden lives under `testdata/`, which the tool excludes from
builds and vet, and is committed like source; the mismatch message names the path
and the exact regenerate command so a reviewer can act in one paste. Two traps
this exercise guards against: writing without a trailing-newline policy (the
renderer and the file must agree on exactly one), and letting the write path touch
the committed fixture during a normal run — the round-trip test uses `t.TempDir`
precisely to avoid that. The discipline the flag cannot enforce is the one that
matters most: read the `git diff` of the regenerated golden before committing, or
`-update` becomes a rubber stamp for whatever the code now emits.

## Resources

- [flag: Bool](https://pkg.go.dev/flag#Bool) — declaring the package-level `-update` flag the test binary parses.
- [os: WriteFile / ReadFile](https://pkg.go.dev/os#WriteFile) — the golden read and write primitives, with explicit file modes.
- [cmd/go: test packages and testdata](https://pkg.go.dev/cmd/go#hdr-Test_packages) — why `testdata/` is ignored by the build tool.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-multi-user-snapshot.md](03-multi-user-snapshot.md) | Next: [05-redact-volatile-fields.md](05-redact-volatile-fields.md)
