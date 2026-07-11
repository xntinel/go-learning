# Exercise 9: Supply-Chain Integrity — go.sum, the Checksum DB, and Verification

`go.sum` is not a version lock; it is a list of cryptographic hashes of exactly
the bytes each dependency published, and every build re-hashes what it fetched and
compares. This exercise builds a `modhash` package on `golang.org/x/mod/sumdb/dirhash`
— the very code the `go` command uses to compute the `h1:` hashes in `go.sum` —
so you can compute a module-tree hash yourself, see it is deterministic, and watch
it change the instant one byte does.

This module is self-contained and links `golang.org/x/mod`.

## What you'll build

```text
modhash/                        independent module: example.com/modhash
  go.mod                        require golang.org/x/mod
  go.sum
  modhash.go                    HashDir(dir, prefix) (string, error) over dirhash
  modhash_test.go               deterministic + tamper-sensitive tests
  cmd/demo/
    main.go                     hashes a temp tree, prints the h1: hash length
```

Files: `modhash.go`, `modhash_test.go`, `cmd/demo/main.go`.
Implement: `HashDir(dir, prefix string) (string, error)` wrapping `dirhash.HashDir(..., dirhash.DefaultHash)`.
Test: hashing the same tree twice matches; changing one file changes the hash; the result is an `h1:` string.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/modhash/cmd/demo
cd ~/go-exercises/modhash
go mod init example.com/modhash
```

### The trust boundary, in one function

When the `go` command downloads a module version, it computes an `h1:` hash over
the module's file tree and its `go.mod`, records both in `go.sum`, and — on the
*first* download of that version — cross-checks them against the checksum database
(`GOSUMDB`, default `sum.golang.org`), a public append-only transparency log. On
every later build it re-hashes what it has and compares against `go.sum`; a
mismatch aborts the build. `go mod verify` runs that comparison on demand across
the whole module cache, so a tampered cached module is caught, not silently
compiled.

The hash function is not magic — it is `golang.org/x/mod/sumdb/dirhash`, and its
default, `dirhash.DefaultHash` (which is `Hash1`), is precisely what produces the
`h1:` prefix you see in `go.sum`. `HashDir(dir, prefix, hash)` hashes every file
under `dir`, prefixing each path with `prefix` (the `module@version` string, so
the same bytes at different versions hash differently). Wrapping it makes the
supply-chain hash something you can compute and reason about directly.

Create `modhash.go`:

```go
// modhash.go
package modhash

import "golang.org/x/mod/sumdb/dirhash"

// HashDir computes the module-tree hash of dir the same way the go command
// computes the h1: hashes recorded in go.sum. prefix is the module@version
// string that each file path is prefixed with before hashing, so identical
// bytes under different versions produce different hashes. The result begins
// with "h1:".
func HashDir(dir, prefix string) (string, error) {
	return dirhash.HashDir(dir, prefix, dirhash.DefaultHash)
}
```

### The demo

The demo writes a tiny module tree into a temp directory, hashes it, and reports
the hash's shape (an `h1:` prefix and a fixed-length base64 body) — deterministic
across runs because it depends only on the content and the prefix.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"example.com/modhash"
)

func main() {
	dir, err := os.MkdirTemp("", "modtree")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	if err := os.WriteFile(filepath.Join(dir, "lib.go"), []byte("package lib\n"), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	h, err := modhash.HashDir(dir, "example.com/lib@v1.0.0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Printf("has h1 prefix: %v\n", strings.HasPrefix(h, "h1:"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
has h1 prefix: true
```

(The exact hash bytes depend on the content and prefix; its `h1:` form is what
`go.sum` records.)

### The test

Three properties, all deterministic. Hashing the same tree twice yields the same
string (a hash must be stable). Changing one byte in one file changes the hash
(this is what makes `go mod verify` able to detect a tampered cache). And the
result carries the `h1:` prefix that `go.sum` uses.

Create `modhash_test.go`:

```go
// modhash_test.go
package modhash

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTree(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "lib.go"), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return dir
}

func TestHashDir(t *testing.T) {
	t.Parallel()

	const prefix = "example.com/lib@v1.0.0"

	a := writeTree(t, "package lib\n")
	h1, err := HashDir(a, prefix)
	if err != nil {
		t.Fatalf("HashDir: %v", err)
	}
	if !strings.HasPrefix(h1, "h1:") {
		t.Fatalf("hash %q does not start with h1:", h1)
	}

	// Same content, same prefix -> same hash (determinism).
	b := writeTree(t, "package lib\n")
	h2, err := HashDir(b, prefix)
	if err != nil {
		t.Fatalf("HashDir: %v", err)
	}
	if h1 != h2 {
		t.Fatalf("hash not deterministic: %q != %q", h1, h2)
	}

	// One byte changed -> different hash (tamper sensitivity).
	c := writeTree(t, "package lib // tampered\n")
	h3, err := HashDir(c, prefix)
	if err != nil {
		t.Fatalf("HashDir: %v", err)
	}
	if h1 == h3 {
		t.Fatal("hash unchanged after content was modified")
	}
}

func TestHashDirPrefixMatters(t *testing.T) {
	t.Parallel()

	dir := writeTree(t, "package lib\n")
	h1, err := HashDir(dir, "example.com/lib@v1.0.0")
	if err != nil {
		t.Fatalf("HashDir: %v", err)
	}
	h2, err := HashDir(dir, "example.com/lib@v2.0.0")
	if err != nil {
		t.Fatalf("HashDir: %v", err)
	}
	if h1 == h2 {
		t.Fatal("different module@version prefixes produced the same hash")
	}
}
```

## Review

The hash is correct when it is a stable function of file content plus the
`module@version` prefix: identical trees hash equal, a single changed byte hashes
differently, and different version prefixes diverge — the three properties that
let `go.sum` pin a release and `go mod verify` detect tampering. The operational
trap is disabling the checksum database globally (`GOSUMDB=off`) to silence a
private-module error, which removes tamper detection for every module; scope it
with `GOPRIVATE`/`GONOSUMDB` instead. Confirm with `go test -race ./...`, and on a
real cache with `go mod verify` (green on a clean cache; non-zero after a
`-modcacherw` edit under `$(go env GOMODCACHE)`, restored by
`go clean -modcache && go mod download`).

## Resources

- [`golang.org/x/mod/sumdb/dirhash`](https://pkg.go.dev/golang.org/x/mod/sumdb/dirhash) — `HashDir`, `Hash1`, and `DefaultHash`, the `h1:` hash implementation.
- [Go Modules Reference: `go.sum` and the checksum database](https://go.dev/ref/mod#go-sum-files) — what `go.sum` records and how `GOSUMDB` verifies it.
- [`go mod verify`](https://go.dev/ref/mod#go-mod-verify) — detecting a tampered module cache.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-installing-from-private-module-servers.md](08-installing-from-private-module-servers.md) | Next: [10-hermetic-offline-builds-with-vendoring.md](10-hermetic-offline-builds-with-vendoring.md)
