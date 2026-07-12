# Exercise 6: Verify a downloaded module zip against its go.sum h1 hash

A supply-chain integrity gate in CI must fail the build when a module's bytes do
not match the hash pinned in `go.sum`. This exercise computes the canonical `h1:`
hashes for a module zip and its `go.mod` and verifies both, reporting the
expected-versus-actual values on a tamper.

## What you'll build

```text
ziphash/                   independent module: example.com/ziphash
  go.mod                   go 1.26 (requires golang.org/x/mod/sumdb/dirhash)
  verify.go                HashZip, HashGoMod, ParseSum, VerifyModule, ErrChecksumMismatch
  cmd/
    demo/
      main.go              build a zip, record go.sum, verify clean then tampered
  verify_test.go           temp-zip build/verify, tamper-detect, go.sum parse tests
  example_test.go          ExampleSumEntry_IsGoMod with // Output
```

- Files: `verify.go`, `cmd/demo/main.go`, `verify_test.go`, `example_test.go`.
- Implement: `HashZip`/`HashGoMod` using `dirhash`, a `ParseSum` for go.sum lines, and `VerifyModule` that compares both recomputed hashes to the matching `go.sum` entries, returning `ErrChecksumMismatch` on a mismatch.
- Test: build a module zip in a temp dir, record its hashes, verify it passes; flip a byte and assert the verifier reports the mismatch with expected-vs-actual.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/11-packages-and-modules/07-module-proxies-and-goproxy/06-module-ziphash-verifier/cmd/demo
cd go-solutions/11-packages-and-modules/07-module-proxies-and-goproxy/06-module-ziphash-verifier
go mod edit -go=1.26
go get golang.org/x/mod/sumdb/dirhash
```

### The two hashes, computed exactly as the go command computes them

Every module has two `go.sum` lines. `example.com/m v1.2.3 h1:...` is the hash of
the module zip; `example.com/m v1.2.3/go.mod h1:...` is the hash of just the
`go.mod`. Getting these byte-for-byte right matters, because a verifier that
computes a slightly different hash would either pass tampered bytes or fail clean
ones. The zip hash is `dirhash.HashZip(zip, dirhash.DefaultHash)`, which hashes
only file names and contents — zip metadata, compression, and modification times
are deliberately ignored, so re-zipping the same tree yields the same hash. The
`go.mod` hash is the detail people get wrong: it is `dirhash.Hash1` over a single
file whose name is literally `"go.mod"` (not the module path, not
`module@version/go.mod`) and whose content is the `go.mod` bytes. That is exactly
what `cmd/go/internal/modfetch` does, and computing it against any real cached
module reproduces the recorded `/go.mod` line.

`VerifyModule` looks up the two `go.sum` entries for the module and version,
recomputes both hashes from the files on disk, and compares. A difference means a
proxy or cache served bytes that do not match what `go.sum` pins — a tamper or a
corrupt download — so it returns `ErrChecksumMismatch` wrapped with the
expected-versus-actual values, which is precisely the message a CI gate wants in
its failure output. Missing a required `go.sum` entry is a distinct error, not a
silent pass.

Create `verify.go`:

```go
// Package ziphash verifies a downloaded module zip and its go.mod against the two
// h1: hashes recorded in go.sum, using the canonical dirhash algorithm.
package ziphash

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/mod/sumdb/dirhash"
)

// ErrChecksumMismatch signals a tamper: a recomputed hash differs from go.sum.
var ErrChecksumMismatch = errors.New("checksum mismatch")

// HashZip computes the h1: hash of a module zip, exactly as the go command does
// when recording the "module version h1:..." line in go.sum.
func HashZip(zipPath string) (string, error) {
	return dirhash.HashZip(zipPath, dirhash.DefaultHash)
}

// HashGoMod computes the h1: hash of a module's go.mod, matching the
// "module version/go.mod h1:..." line in go.sum. The go command hashes it as a
// single file literally named "go.mod".
func HashGoMod(gomodPath string) (string, error) {
	return dirhash.Hash1([]string{"go.mod"}, func(string) (io.ReadCloser, error) {
		return os.Open(gomodPath)
	})
}

// SumEntry is one parsed go.sum line.
type SumEntry struct {
	Module  string
	Version string // as written, e.g. "v1.2.3" or "v1.2.3/go.mod"
	Hash    string
}

// IsGoMod reports whether the entry is the /go.mod hash rather than the zip hash.
func (e SumEntry) IsGoMod() bool { return strings.HasSuffix(e.Version, "/go.mod") }

// BaseVersion returns the version without any /go.mod suffix.
func (e SumEntry) BaseVersion() string {
	return strings.TrimSuffix(e.Version, "/go.mod")
}

// ParseSum reads go.sum lines into entries. Blank lines are skipped; a line that
// is not exactly three fields is an error.
func ParseSum(r io.Reader) ([]SumEntry, error) {
	var entries []SumEntry
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 3 {
			return nil, fmt.Errorf("malformed go.sum line: %q", line)
		}
		entries = append(entries, SumEntry{Module: f[0], Version: f[1], Hash: f[2]})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

// Result reports the outcome of verifying one module@version.
type Result struct {
	ZipExpected, ZipActual     string
	GoModExpected, GoModActual string
}

// ZipOK reports whether the zip hash matched.
func (r Result) ZipOK() bool { return r.ZipExpected == r.ZipActual }

// GoModOK reports whether the go.mod hash matched.
func (r Result) GoModOK() bool { return r.GoModExpected == r.GoModActual }

// VerifyModule recomputes both hashes for module@version and compares them to the
// matching go.sum entries. It returns ErrChecksumMismatch (wrapped) if either
// hash differs, alongside the populated Result.
func VerifyModule(module, version, zipPath, gomodPath string, entries []SumEntry) (Result, error) {
	var res Result
	for _, e := range entries {
		if e.Module != module || e.BaseVersion() != version {
			continue
		}
		if e.IsGoMod() {
			res.GoModExpected = e.Hash
		} else {
			res.ZipExpected = e.Hash
		}
	}
	if res.ZipExpected == "" {
		return res, fmt.Errorf("no go.sum zip entry for %s %s", module, version)
	}
	if res.GoModExpected == "" {
		return res, fmt.Errorf("no go.sum /go.mod entry for %s %s", module, version)
	}

	zh, err := HashZip(zipPath)
	if err != nil {
		return res, fmt.Errorf("hash zip: %w", err)
	}
	res.ZipActual = zh

	gh, err := HashGoMod(gomodPath)
	if err != nil {
		return res, fmt.Errorf("hash go.mod: %w", err)
	}
	res.GoModActual = gh

	if !res.ZipOK() {
		return res, fmt.Errorf("%w: %s %s zip: expected %s got %s", ErrChecksumMismatch, module, version, res.ZipExpected, res.ZipActual)
	}
	if !res.GoModOK() {
		return res, fmt.Errorf("%w: %s %s /go.mod: expected %s got %s", ErrChecksumMismatch, module, version, res.GoModExpected, res.GoModActual)
	}
	return res, nil
}
```

### The runnable demo

The demo builds a module zip, records its two hashes as a `go.sum`, verifies the
clean module passes, then overwrites the zip with a poisoned build and shows the
verifier reject it. The hashes are deterministic because `dirhash` ignores zip
metadata, so the printed values are stable.

Create `cmd/demo/main.go`:

```go
package main

import (
	"archive/zip"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"example.com/ziphash"
)

const goMod = "module example.com/tiny\n\ngo 1.26\n"

// buildZip writes a module zip with the canonical module@version/ prefix.
func buildZip(path string, tamper bool) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	files := map[string]string{
		"example.com/tiny@v1.0.0/go.mod":  goMod,
		"example.com/tiny@v1.0.0/tiny.go": "package tiny\n",
	}
	if tamper {
		files["example.com/tiny@v1.0.0/tiny.go"] = "package tiny // injected\n"
	}
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		if _, err := w.Write([]byte(content)); err != nil {
			return err
		}
	}
	return zw.Close()
}

func main() {
	dir, _ := os.MkdirTemp("", "modverify")
	defer os.RemoveAll(dir)

	zipPath := filepath.Join(dir, "v1.0.0.zip")
	gomodPath := filepath.Join(dir, "v1.0.0.mod")
	_ = buildZip(zipPath, false)
	_ = os.WriteFile(gomodPath, []byte(goMod), 0o644)

	zh, _ := ziphash.HashZip(zipPath)
	gh, _ := ziphash.HashGoMod(gomodPath)
	fmt.Printf("recorded go.sum:\n")
	fmt.Printf("example.com/tiny v1.0.0 %s\n", zh)
	fmt.Printf("example.com/tiny v1.0.0/go.mod %s\n", gh)

	sum := fmt.Sprintf("example.com/tiny v1.0.0 %s\nexample.com/tiny v1.0.0/go.mod %s\n", zh, gh)
	entries, _ := ziphash.ParseSum(strings.NewReader(sum))

	_, err := ziphash.VerifyModule("example.com/tiny", "v1.0.0", zipPath, gomodPath, entries)
	fmt.Printf("clean verify: %v\n", err)

	// Now a poisoned zip with the same recorded hashes.
	_ = buildZip(zipPath, true)
	_, err = ziphash.VerifyModule("example.com/tiny", "v1.0.0", zipPath, gomodPath, entries)
	fmt.Printf("tampered verify error: %v\n", err != nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
recorded go.sum:
example.com/tiny v1.0.0 h1:JqEgt/8q+FNjWqmFGpkYjkcSPI5WQd0PQ3/GsWKuDbI=
example.com/tiny v1.0.0/go.mod h1:4iBd9/HYWET8tKt7IoprY1R+/B5ACTXhJnYwyxGxqpY=
clean verify: <nil>
tampered verify error: true
```

### Tests

The tests build a real module zip in `t.TempDir()`, record its hashes, and verify
it passes; then a second test overwrites the zip with a poisoned build and asserts
`VerifyModule` returns `ErrChecksumMismatch` with differing expected-vs-actual zip
hashes. The parse tests cover the `/go.mod` suffix handling and reject a malformed
line.

Create `verify_test.go`:

```go
package ziphash

import (
	"archive/zip"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testGoMod = "module example.com/tiny\n\ngo 1.26\n"

// buildModuleZip writes a module zip using the canonical module@version/ prefix
// and returns its path. When tamper is true one file's content is altered.
func buildModuleZip(t *testing.T, dir string, tamper bool) string {
	t.Helper()
	path := filepath.Join(dir, "v1.0.0.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	body := "package tiny\n"
	if tamper {
		body = "package tiny // injected\n"
	}
	files := []struct{ name, content string }{
		{"example.com/tiny@v1.0.0/go.mod", testGoMod},
		{"example.com/tiny@v1.0.0/tiny.go", body},
	}
	for _, fl := range files {
		w, err := zw.Create(fl.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(fl.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeGoMod(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "v1.0.0.mod")
	if err := os.WriteFile(p, []byte(testGoMod), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestVerifyModulePasses(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	zipPath := buildModuleZip(t, dir, false)
	gomodPath := writeGoMod(t, dir)

	zh, err := HashZip(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(zh, "h1:") {
		t.Errorf("zip hash %q missing h1: prefix", zh)
	}
	gh, err := HashGoMod(gomodPath)
	if err != nil {
		t.Fatal(err)
	}

	sum := fmt.Sprintf("example.com/tiny v1.0.0 %s\nexample.com/tiny v1.0.0/go.mod %s\n", zh, gh)
	entries, err := ParseSum(strings.NewReader(sum))
	if err != nil {
		t.Fatal(err)
	}
	res, err := VerifyModule("example.com/tiny", "v1.0.0", zipPath, gomodPath, entries)
	if err != nil {
		t.Fatalf("clean module failed verification: %v", err)
	}
	if !res.ZipOK() || !res.GoModOK() {
		t.Errorf("result not OK: %+v", res)
	}
}

func TestVerifyModuleDetectsTamper(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Record hashes from the clean zip.
	clean := buildModuleZip(t, dir, false)
	gomodPath := writeGoMod(t, dir)
	zh, _ := HashZip(clean)
	gh, _ := HashGoMod(gomodPath)
	sum := fmt.Sprintf("example.com/tiny v1.0.0 %s\nexample.com/tiny v1.0.0/go.mod %s\n", zh, gh)
	entries, _ := ParseSum(strings.NewReader(sum))

	// Overwrite the zip with a poisoned one and re-verify.
	tampered := buildModuleZip(t, dir, true)
	res, err := VerifyModule("example.com/tiny", "v1.0.0", tampered, gomodPath, entries)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("err = %v; want ErrChecksumMismatch", err)
	}
	if res.ZipOK() {
		t.Error("tampered zip reported ZipOK")
	}
	if res.ZipExpected == res.ZipActual {
		t.Error("expected and actual zip hashes should differ after tamper")
	}
}

func TestParseSum(t *testing.T) {
	t.Parallel()
	in := "example.com/tiny v1.0.0 h1:aaa=\nexample.com/tiny v1.0.0/go.mod h1:bbb=\n\n"
	entries, err := ParseSum(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries; want 2", len(entries))
	}
	if entries[0].IsGoMod() {
		t.Error("first entry should be the zip hash")
	}
	if !entries[1].IsGoMod() || entries[1].BaseVersion() != "v1.0.0" {
		t.Errorf("second entry wrong: %+v", entries[1])
	}
}

func TestParseSumRejectsMalformed(t *testing.T) {
	t.Parallel()
	if _, err := ParseSum(strings.NewReader("example.com/m v1.0.0\n")); err == nil {
		t.Error("expected error for malformed line")
	}
}
```

Create `example_test.go`:

```go
package ziphash

import (
	"fmt"
	"strings"
)

func ExampleSumEntry_IsGoMod() {
	entries, _ := ParseSum(strings.NewReader("example.com/m v1.0.0/go.mod h1:x="))
	fmt.Println(entries[0].IsGoMod(), entries[0].BaseVersion())
	// Output: true v1.0.0
}
```

## Review

The verifier is correct when its two hashes reproduce what the `go` command
records: the zip hash via `dirhash.HashZip` (which ignores zip metadata) and the
`go.mod` hash via `dirhash.Hash1` over a single file named `"go.mod"`. The most
common error is hashing the `go.mod` under the wrong file name — using the module
path or the `module@version/go.mod` form — which produces a hash that never
matches `go.sum` and makes every module look tampered. The tamper test is the real
proof: recording the hash of a clean zip and then flipping a byte must surface
`ErrChecksumMismatch` with a different actual hash. Assert the mismatch with
`errors.Is`, treat a missing `go.sum` entry as its own error rather than a pass,
and run `go test -race`.

## Resources

- [`golang.org/x/mod/sumdb/dirhash`](https://pkg.go.dev/golang.org/x/mod/sumdb/dirhash) — `HashZip`, `Hash1`, `DefaultHash`, and the `h1:` format definition.
- [Go Modules Reference: go.sum files](https://go.dev/ref/mod#go-sum-files) — the two-line-per-module structure and what each hash covers.
- [Go Modules Reference: Authenticating modules](https://go.dev/ref/mod#authenticating) — how the hashes fit the checksum-database trust chain.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-goprivate-pattern-matcher.md](05-goprivate-pattern-matcher.md) | Next: [07-gosum-tidy-auditor.md](07-gosum-tidy-auditor.md)
