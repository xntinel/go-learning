# Exercise 9: Scan GOMODCACHE for corrupted or checksum-mismatched entries

An intermittent `verifying ...: checksum mismatch` in CI usually means a poisoned
or partially-written shared module cache. This exercise builds the diagnostic:
walk the cache download tree, recompute each cached zip's hash, cross-check it
against the recorded `.ziphash`, and report the corrupted entries.

## What you'll build

```text
modcachescan/              independent module: example.com/modcachescan
  go.mod                   go 1.26 (requires golang.org/x/mod/sumdb/dirhash)
  scan.go                  Status, Entry, Scan(downloadRoot) []Entry
  cmd/
    demo/
      main.go              builds a cache with good/tampered/orphan entries, scans
  scan_test.go             temp-cache scan: ok, mismatch (expected-vs-actual), missing
  example_test.go          Example scanning a good entry with // Output
```

- Files: `scan.go`, `cmd/demo/main.go`, `scan_test.go`, `example_test.go`.
- Implement: `Scan(downloadRoot string) ([]Entry, error)` that walks `<root>/<module>/@v/<version>.zip`, recomputes the hash, compares it to the sibling `.ziphash`, and reports `ok`/`mismatch`/`missing-ziphash` in deterministic order.
- Test: build a fake cache in `t.TempDir()` with a valid entry, a tampered one, and one missing its `.ziphash`; assert the good one passes, the mismatch reports expected-vs-actual, and the missing one is reported distinctly.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/11-packages-and-modules/07-module-proxies-and-goproxy/09-modcache-integrity-scan/cmd/demo
cd go-solutions/11-packages-and-modules/07-module-proxies-and-goproxy/09-modcache-integrity-scan
go mod edit -go=1.26
go get golang.org/x/mod/sumdb/dirhash
```

### The cache layout and what corruption looks like

The module cache stores downloads under `$GOMODCACHE/cache/download`, in the tree
`<module>/@v/<version>.{info,mod,zip,ziphash,lock}`. The `.ziphash` file holds the
`h1:` hash the `go` command recorded when it first downloaded the zip — a single
line, e.g. `h1:vF1DjpVEshcIqoEaauuHebaLk1O1forxjxBaVn884JQ=`. On every build the
`go` command recomputes the zip's hash and compares; if the cache was corrupted —
a partial write from a killed process, two toolchains racing on a shared cache, or
a genuinely poisoned entry — the recomputed hash no longer matches and the build
fails with `checksum mismatch`. The trouble is that failure is intermittent and
opaque: it depends on which cache entry a given job happens to read.

The scanner makes it explicit. It walks the download tree with `filepath.WalkDir`,
and for every `.zip` it recomputes `dirhash.HashZip` and compares it to the value
in the sibling `.ziphash`. Three outcomes: `ok` (they match), `mismatch` (they
differ — the corruption we are hunting, reported with both the recorded and the
recomputed hash so an operator can see the drift), and `missing-ziphash` (a `.zip`
with no recorded hash at all, a distinct kind of half-written entry). The walk is
strictly read-only — diagnosing a cache must never mutate it — and the output is
sorted so successive scans are comparable. Because `dirhash.HashZip` ignores zip
metadata and hashes only names and contents, a correct entry always reproduces its
recorded hash regardless of when or how it was zipped.

Create `scan.go`:

```go
// Package modcachescan walks a module cache download tree, recomputes each cached
// zip's hash, and cross-checks it against the recorded .ziphash to find corrupted
// or checksum-mismatched entries.
package modcachescan

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/mod/sumdb/dirhash"
)

// Status classifies one scanned cache entry.
type Status string

const (
	// StatusOK: the recomputed zip hash matches the recorded .ziphash.
	StatusOK Status = "ok"
	// StatusMismatch: the recomputed hash differs from the recorded one.
	StatusMismatch Status = "mismatch"
	// StatusMissingZiphash: a .zip with no sibling .ziphash file.
	StatusMissingZiphash Status = "missing-ziphash"
)

// Entry is the scan result for one cached module version.
type Entry struct {
	Module   string
	Version  string
	Status   Status
	Recorded string
	Computed string
}

// Scan walks the cache download tree rooted at downloadRoot (the
// $GOMODCACHE/cache/download directory) and returns one Entry per cached zip, in
// deterministic order. It never writes to the cache.
func Scan(downloadRoot string) ([]Entry, error) {
	var entries []Entry
	err := filepath.WalkDir(downloadRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".zip") {
			return nil
		}
		module, version := splitCachePath(downloadRoot, path)

		e := Entry{Module: module, Version: version}

		hashPath := strings.TrimSuffix(path, ".zip") + ".ziphash"
		recorded, rerr := os.ReadFile(hashPath)
		if rerr != nil {
			if os.IsNotExist(rerr) {
				e.Status = StatusMissingZiphash
				entries = append(entries, e)
				return nil
			}
			return rerr
		}
		e.Recorded = strings.TrimSpace(string(recorded))

		computed, cerr := dirhash.HashZip(path, dirhash.DefaultHash)
		if cerr != nil {
			return cerr
		}
		e.Computed = computed
		if e.Computed == e.Recorded {
			e.Status = StatusOK
		} else {
			e.Status = StatusMismatch
		}
		entries = append(entries, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(entries, func(a, b Entry) int {
		if a.Module != b.Module {
			return strings.Compare(a.Module, b.Module)
		}
		return strings.Compare(a.Version, b.Version)
	})
	return entries, nil
}

// splitCachePath derives the (encoded) module path and version from a cached zip
// path of the form <root>/<module>/@v/<version>.zip.
func splitCachePath(root, path string) (module, version string) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		rel = path
	}
	rel = filepath.ToSlash(rel)
	version = strings.TrimSuffix(filepath.Base(rel), ".zip")
	if i := strings.Index(rel, "/@v/"); i >= 0 {
		module = rel[:i]
	}
	return module, version
}
```

### The runnable demo

The demo builds a cache with three entries — a good one whose recorded hash
matches, a tampered one whose recorded hash is wrong, and an orphan zip with no
`.ziphash` — and scans it.

Create `cmd/demo/main.go`:

```go
package main

import (
	"archive/zip"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"example.com/modcachescan"
	"golang.org/x/mod/sumdb/dirhash"
)

// writeZip writes a minimal module zip and returns its path.
func writeZip(dir, module, version string) string {
	_ = os.MkdirAll(dir, 0o755)
	zp := filepath.Join(dir, version+".zip")
	f, _ := os.Create(zp)
	defer f.Close()
	zw := zip.NewWriter(f)
	w, _ := zw.Create(module + "@" + version + "/go.mod")
	_, _ = w.Write([]byte("module " + module + "\n\ngo 1.26\n"))
	_ = zw.Close()
	return zp
}

func writeHash(zp, value string) {
	_ = os.WriteFile(strings.TrimSuffix(zp, ".zip")+".ziphash", []byte(value+"\n"), 0o644)
}

func main() {
	root, _ := os.MkdirTemp("", "modcache")
	defer os.RemoveAll(root)

	// good: correct recorded hash.
	gd := filepath.Join(root, "example.com", "good", "@v")
	gz := writeZip(gd, "example.com/good", "v1.0.0")
	gh, _ := dirhash.HashZip(gz, dirhash.DefaultHash)
	writeHash(gz, gh)

	// bad: recorded hash does not match the zip (poisoned or partial write).
	bd := filepath.Join(root, "example.com", "bad", "@v")
	bz := writeZip(bd, "example.com/bad", "v1.0.0")
	writeHash(bz, "h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")

	// orphan: zip with no .ziphash at all.
	od := filepath.Join(root, "example.com", "orphan", "@v")
	writeZip(od, "example.com/orphan", "v1.0.0")

	entries, err := modcachescan.Scan(root)
	if err != nil {
		panic(err)
	}
	for _, e := range entries {
		fmt.Printf("%-19s %-8s %s\n", e.Module, e.Version, e.Status)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
example.com/bad     v1.0.0   mismatch
example.com/good    v1.0.0   ok
example.com/orphan  v1.0.0   missing-ziphash
```

### Tests

The test builds a fake cache in `t.TempDir()` with the three conditions and
asserts each: the good entry is `ok`, the tampered entry is `mismatch` with the
exact recorded hash and a differing non-empty computed hash, and the orphan is
`missing-ziphash`. A second test pins the deterministic module ordering.

Create `scan_test.go`:

```go
package modcachescan

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/mod/sumdb/dirhash"
)

func writeZip(t *testing.T, dir, module, version, extra string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	zp := filepath.Join(dir, version+".zip")
	f, err := os.Create(zp)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	w, err := zw.Create(module + "@" + version + "/go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("module " + module + "\n\ngo 1.26\n" + extra)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return zp
}

func writeHash(t *testing.T, zp, value string) {
	t.Helper()
	if err := os.WriteFile(zp[:len(zp)-len(".zip")]+".ziphash", []byte(value+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func byModule(entries []Entry) map[string]Entry {
	m := make(map[string]Entry, len(entries))
	for _, e := range entries {
		m[e.Module] = e
	}
	return m
}

func TestScan(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// Good entry: recorded hash matches the zip.
	gd := filepath.Join(root, "example.com", "good", "@v")
	gz := writeZip(t, gd, "example.com/good", "v1.0.0", "")
	gh, err := dirhash.HashZip(gz, dirhash.DefaultHash)
	if err != nil {
		t.Fatal(err)
	}
	writeHash(t, gz, gh)

	// Tampered entry: recorded hash does not match the zip.
	bd := filepath.Join(root, "example.com", "bad", "@v")
	bz := writeZip(t, bd, "example.com/bad", "v1.0.0", "")
	const wrong = "h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	writeHash(t, bz, wrong)

	// Orphan entry: zip with no .ziphash.
	od := filepath.Join(root, "example.com", "orphan", "@v")
	writeZip(t, od, "example.com/orphan", "v1.0.0", "")

	entries, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries; want 3", len(entries))
	}

	m := byModule(entries)

	if m["example.com/good"].Status != StatusOK {
		t.Errorf("good status = %v; want ok", m["example.com/good"].Status)
	}

	bad := m["example.com/bad"]
	if bad.Status != StatusMismatch {
		t.Errorf("bad status = %v; want mismatch", bad.Status)
	}
	if bad.Recorded != wrong {
		t.Errorf("recorded = %q; want %q", bad.Recorded, wrong)
	}
	if bad.Computed == bad.Recorded || bad.Computed == "" {
		t.Errorf("computed %q should differ from recorded and be non-empty", bad.Computed)
	}

	if m["example.com/orphan"].Status != StatusMissingZiphash {
		t.Errorf("orphan status = %v; want missing-ziphash", m["example.com/orphan"].Status)
	}
}

func TestScanDeterministicOrder(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, mod := range []string{"example.com/z", "example.com/a", "example.com/m"} {
		dir := filepath.Join(root, filepath.FromSlash(mod), "@v")
		zp := writeZip(t, dir, mod, "v1.0.0", "")
		h, _ := dirhash.HashZip(zp, dirhash.DefaultHash)
		writeHash(t, zp, h)
	}
	entries, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"example.com/a", "example.com/m", "example.com/z"}
	for i, w := range want {
		if entries[i].Module != w {
			t.Errorf("entry %d module = %q; want %q", i, entries[i].Module, w)
		}
	}
}
```

Create `example_test.go`:

```go
package modcachescan_test

import (
	"archive/zip"
	"fmt"
	"os"
	"path/filepath"

	"example.com/modcachescan"
	"golang.org/x/mod/sumdb/dirhash"
)

func Example() {
	root, _ := os.MkdirTemp("", "modcache")
	defer os.RemoveAll(root)
	dir := filepath.Join(root, "example.com", "m", "@v")
	_ = os.MkdirAll(dir, 0o755)
	zp := filepath.Join(dir, "v1.0.0.zip")
	f, _ := os.Create(zp)
	zw := zip.NewWriter(f)
	w, _ := zw.Create("example.com/m@v1.0.0/go.mod")
	_, _ = w.Write([]byte("module example.com/m\n\ngo 1.26\n"))
	_ = zw.Close()
	_ = f.Close()
	h, _ := dirhash.HashZip(zp, dirhash.DefaultHash)
	_ = os.WriteFile(dir+"/v1.0.0.ziphash", []byte(h+"\n"), 0o644)

	entries, _ := modcachescan.Scan(root)
	fmt.Println(entries[0].Module, entries[0].Status)
	// Output: example.com/m ok
}
```

## Review

The scanner is correct when it reproduces the `go` command's own check: recompute
`dirhash.HashZip` and compare to the recorded `.ziphash`. The mismatch case is the
one that matters — it is the intermittent CI failure made visible — so the test
records a deliberately wrong hash and asserts both that the status is `mismatch`
and that the reported computed hash differs from it, which is what an operator
needs to distinguish a poisoned entry from a stale one. Keep the walk read-only:
a diagnostic that repairs the cache in place can mask a concurrency bug that will
recur. Report `missing-ziphash` distinctly from `mismatch`; a half-written entry
and a poisoned one have different root causes. Run `go test -race`.

## Resources

- [`path/filepath.WalkDir`](https://pkg.go.dev/path/filepath#WalkDir) — the read-only tree walk over the cache.
- [`golang.org/x/mod/sumdb/dirhash.HashZip`](https://pkg.go.dev/golang.org/x/mod/sumdb/dirhash#HashZip) — recomputing the zip hash the same way the toolchain does.
- [Go Modules Reference: Module cache](https://go.dev/ref/mod#module-cache) — the `cache/download` layout and the `.ziphash` file.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-proxy-failover-healthcheck.md](08-proxy-failover-healthcheck.md) | Next: [10-fetch-policy-engine.md](10-fetch-policy-engine.md)
