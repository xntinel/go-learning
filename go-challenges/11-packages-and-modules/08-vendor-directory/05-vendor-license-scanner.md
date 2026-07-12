# Exercise 5: A license-compliance scanner over the vendored tree

Every dependency bump changes the license surface of your product, and a platform
team gates that surface. This exercise builds a scanner that walks the vendored
tree, finds each module's license file, classifies it against an allowlist, and
reports the modules that violate policy — the license gate that runs on every
`go.mod` change.

This module is fully self-contained: its own `go mod init`, standard-library
only, its own demo and `fstest`-driven tests. Nothing here imports another
exercise.

## What you'll build

```text
licensescan/                 independent module: example.com/licensescan
  go.mod                     go 1.26
  licensescan.go             type Finding; Scan(fs.FS, []string) ([]Finding, error); Classify
  cmd/
    demo/
      main.go                scans an in-memory tree and prints violations
  licensescan_test.go        fstest.MapFS tree with MIT/Apache/GPL/missing modules
```

- Files: `licensescan.go`, `cmd/demo/main.go`, `licensescan_test.go`.
- Implement: `Classify`, mapping a license body to a SPDX-ish id and an allowed flag, and `Scan`, which walks each module's subtree with `io/fs.WalkDir`, locates its license file, and returns a `Finding` for every module that is blocked or missing a license.
- Test: build the tree with `testing/fstest.MapFS` — modules carrying MIT, Apache-2.0, and GPL-3.0 license bodies, plus one module with no license file — and assert the violation set; allowlisted modules produce none.
- Verify: `go test -count=1 -race ./...`

### Why scan over `fs.FS`, not a real directory

Vendored license files live on disk under `vendor/`, but a scanner written
against `*os.File` and `filepath.Walk` can only be tested by materializing a real
directory tree — slow, and awkward to make hermetic. Written against the `fs.FS`
interface and `io/fs.WalkDir`, the same code runs unchanged over a real directory
(`os.DirFS("vendor")`) and over an in-memory `testing/fstest.MapFS`. The tests
construct a synthetic vendored tree entirely in memory — modules with different
license bodies, and one deliberately missing a license — with zero disk I/O. This
is the standard Go pattern for making filesystem-walking code testable.

### Classification is heuristic, and that is honest

There is no reliable machine-readable license marker in a plain `LICENSE` file, so
classification keys off canonical phrases: Apache-2.0's "Apache License" plus
"Version 2.0", the MIT grant "Permission is hereby granted, free of charge", the
BSD "Redistribution and use in source and binary forms", and the GPL/AGPL titles.
Real tooling (`go-licenses`, `licensecheck`) uses a far larger corpus of hashed
templates, but the shape is identical: match the body against known signatures,
and treat anything unrecognized as blocked rather than silently allowing it. A
license gate that fails open is not a gate.

### Finding the module's license with `WalkDir`

`Scan` takes the set of module paths to check (in production, these come from the
`modules.txt` inventory of Exercise 2). For each, it walks the module's subtree
with `fs.WalkDir`, collecting files whose base name is a known license name
(`LICENSE`, `LICENSE.md`, `LICENSE.txt`, `COPYING`). Vendoring copies the module's
top-level `LICENSE`, so the relevant file is the shallowest match; `Scan` keeps
the candidate with the shortest path. A module with no such file yields a "missing
license" finding, because an unlicensed dependency is a policy failure, not a
pass.

Create `licensescan.go`:

```go
package licensescan

import (
	"io/fs"
	"path"
	"sort"
	"strings"
)

// Finding is one module that violates license policy.
type Finding struct {
	Module  string
	License string // classified id, or "none"
	Reason  string
}

// allowlist holds the license ids permitted in the vendored tree.
var allowlist = map[string]bool{
	"MIT":        true,
	"BSD":        true,
	"Apache-2.0": true,
}

var licenseNames = map[string]bool{
	"LICENSE":     true,
	"LICENSE.md":  true,
	"LICENSE.txt": true,
	"COPYING":     true,
}

// Classify maps a license body to an id and whether it is on the allowlist.
// Unrecognized bodies are classified "unknown" and are not allowed: a license
// gate must fail closed.
func Classify(body string) (id string, allowed bool) {
	switch {
	case strings.Contains(body, "GNU AFFERO GENERAL PUBLIC LICENSE"):
		id = "AGPL"
	case strings.Contains(body, "GNU LESSER GENERAL PUBLIC LICENSE"):
		id = "LGPL"
	case strings.Contains(body, "GNU GENERAL PUBLIC LICENSE"):
		id = "GPL"
	case strings.Contains(body, "Apache License") && strings.Contains(body, "Version 2.0"):
		id = "Apache-2.0"
	case strings.Contains(body, "Permission is hereby granted, free of charge"):
		id = "MIT"
	case strings.Contains(body, "Redistribution and use in source and binary forms"):
		id = "BSD"
	default:
		id = "unknown"
	}
	return id, allowlist[id]
}

// Scan walks each module's subtree in fsys, classifies its license, and returns
// a Finding for every module that is blocked or missing a license. The result
// is sorted by module path.
func Scan(fsys fs.FS, modules []string) ([]Finding, error) {
	var findings []Finding
	for _, mod := range modules {
		licPath, err := findLicense(fsys, mod)
		if err != nil {
			return nil, err
		}
		if licPath == "" {
			findings = append(findings, Finding{Module: mod, License: "none", Reason: "no LICENSE file in vendored tree"})
			continue
		}
		body, err := fs.ReadFile(fsys, licPath)
		if err != nil {
			return nil, err
		}
		id, ok := Classify(string(body))
		if !ok {
			findings = append(findings, Finding{Module: mod, License: id, Reason: "license " + id + " not in allowlist"})
		}
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].Module < findings[j].Module })
	return findings, nil
}

// findLicense returns the shallowest license-named file under the module root,
// or "" if none exists.
func findLicense(fsys fs.FS, mod string) (string, error) {
	best := ""
	err := fs.WalkDir(fsys, mod, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !licenseNames[path.Base(p)] {
			return nil
		}
		if best == "" || len(p) < len(best) {
			best = p
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return best, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"testing/fstest"

	"example.com/licensescan"
)

func main() {
	tree := fstest.MapFS{
		"github.com/ok/mit/LICENSE":     {Data: []byte("MIT License\n\nPermission is hereby granted, free of charge, ...")},
		"github.com/ok/mit/mit.go":      {Data: []byte("package mit\n")},
		"github.com/bad/gpl/LICENSE":    {Data: []byte("GNU GENERAL PUBLIC LICENSE\nVersion 3, 29 June 2007\n")},
		"github.com/bad/nolic/nolic.go": {Data: []byte("package nolic\n")},
	}
	modules := []string{"github.com/ok/mit", "github.com/bad/gpl", "github.com/bad/nolic"}

	findings, err := licensescan.Scan(tree, modules)
	if err != nil {
		panic(err)
	}
	if len(findings) == 0 {
		fmt.Println("all vendored modules pass license policy")
		return
	}
	for _, f := range findings {
		fmt.Printf("%s: %s (%s)\n", f.Module, f.License, f.Reason)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
github.com/bad/gpl: GPL (license GPL not in allowlist)
github.com/bad/nolic: none (no LICENSE file in vendored tree)
```

### Tests

The tree is an in-memory `fstest.MapFS` with four modules: MIT and Apache-2.0
(allowed, no findings), GPL-3.0 (blocked), and one with no license file (missing).
The test asserts the exact violation set and that the allowed modules produce no
findings. `TestClassify` pins the classifier on each canonical body.

Create `licensescan_test.go`:

```go
package licensescan

import (
	"fmt"
	"reflect"
	"testing"
	"testing/fstest"
)

const (
	mitBody    = "MIT License\n\nPermission is hereby granted, free of charge, to any person ..."
	apacheBody = "\n                                 Apache License\n                           Version 2.0, January 2004\n"
	gplBody    = "                    GNU GENERAL PUBLIC LICENSE\n                       Version 3, 29 June 2007\n"
)

func testTree() fstest.MapFS {
	return fstest.MapFS{
		"github.com/ok/mit/LICENSE":       {Data: []byte(mitBody)},
		"github.com/ok/mit/mit.go":        {Data: []byte("package mit\n")},
		"github.com/ok/apache/LICENSE.md": {Data: []byte(apacheBody)},
		"github.com/bad/gpl/COPYING":      {Data: []byte(gplBody)},
		"github.com/bad/nolic/nolic.go":   {Data: []byte("package nolic\n")},
	}
}

func TestScan(t *testing.T) {
	t.Parallel()
	modules := []string{
		"github.com/ok/mit",
		"github.com/ok/apache",
		"github.com/bad/gpl",
		"github.com/bad/nolic",
	}
	got, err := Scan(testTree(), modules)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	want := []Finding{
		{Module: "github.com/bad/gpl", License: "GPL", Reason: "license GPL not in allowlist"},
		{Module: "github.com/bad/nolic", License: "none", Reason: "no LICENSE file in vendored tree"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Scan findings:\n got %#v\nwant %#v", got, want)
	}
}

func TestClassify(t *testing.T) {
	t.Parallel()
	cases := []struct {
		body        string
		wantID      string
		wantAllowed bool
	}{
		{mitBody, "MIT", true},
		{apacheBody, "Apache-2.0", true},
		{gplBody, "GPL", false},
		{"Redistribution and use in source and binary forms", "BSD", true},
		{"GNU AFFERO GENERAL PUBLIC LICENSE", "AGPL", false},
		{"some proprietary blob", "unknown", false},
	}
	for _, tc := range cases {
		t.Run(tc.wantID, func(t *testing.T) {
			t.Parallel()
			id, allowed := Classify(tc.body)
			if id != tc.wantID || allowed != tc.wantAllowed {
				t.Fatalf("Classify = (%q,%v); want (%q,%v)", id, allowed, tc.wantID, tc.wantAllowed)
			}
		})
	}
}

func Example() {
	tree := fstest.MapFS{
		"github.com/bad/gpl/LICENSE": {Data: []byte(gplBody)},
	}
	findings, _ := Scan(tree, []string{"github.com/bad/gpl"})
	fmt.Println(findings[0].Module, findings[0].License)
	// Output: github.com/bad/gpl GPL
}
```

## Review

The scanner is correct when it fails closed: an unrecognized or absent license is
a finding, never a silent pass, because the whole point of the gate is to block
what policy has not approved. Writing it against `fs.FS` is what lets the tests
construct a full vendored tree in memory with `fstest.MapFS` and assert the exact
violation set with no disk. The shallowest-match rule in `findLicense` matters
because a real vendored module may contain nested `LICENSE` files in sub-packages;
the module's own top-level license is the shortest path, so length is a reliable
tiebreak. The classifier is intentionally heuristic — production tooling matches a
much larger template corpus — but the allowlist-by-id structure is exactly how a
real license gate is wired.

## Resources

- [`io/fs.WalkDir`](https://pkg.go.dev/io/fs#WalkDir) — walking an `fs.FS` with `DirEntry` callbacks.
- [`testing/fstest.MapFS`](https://pkg.go.dev/testing/fstest#MapFS) — an in-memory filesystem for hermetic tests.
- [google/go-licenses](https://github.com/google/go-licenses) — a real license scanner for Go modules, for comparison of scope.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-supply-chain-denylist-policy.md](06-supply-chain-denylist-policy.md)
