# Exercise 8: Attest vendored bytes against a recorded h1: hash

Committing `vendor/` proves what source *was* vendored; it does not prove the
committed bytes were not altered afterward. This exercise builds the verifier that
closes that gap: recompute the `dirhash` of a vendored module tree and compare it
to a recorded `h1:` hash — the exact primitive behind `go mod verify`, hardened
into a release-gate attestation.

This module is fully self-contained: its own `go mod init`, its own demo and
tests. Nothing here imports another exercise.

## What you'll build

```text
attest/                      independent module: example.com/attest
  go.mod                     go 1.26 (requires golang.org/x/mod)
  attest.go                  Attest; Verify; SumHash; ErrTampered
  cmd/
    demo/
      main.go                attests a tree, verifies clean, then detects tampering
  attest_test.go             HashDir round-trip + mutation-detected + go.sum parsing
```

- Files: `attest.go`, `cmd/demo/main.go`, `attest_test.go`.
- Implement: `Attest`, wrapping `dirhash.HashDir(dir, prefix, dirhash.Hash1)`; `Verify`, recomputing and comparing to a recorded hash (returning `ErrTampered` on mismatch); and `SumHash`, extracting a module's `h1:` from a `go.sum` body.
- Test: in `t.TempDir()`, materialize a tree, compute the expected hash with `dirhash.HashDir`, assert `Verify` matches; mutate one file and assert `ErrTampered`; parse a `go.sum` fixture with `SumHash`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/11-packages-and-modules/08-vendor-directory/08-reproducible-vendor-attestation/cmd/demo
cd go-solutions/11-packages-and-modules/08-vendor-directory/08-reproducible-vendor-attestation
go get golang.org/x/mod
```

### The h1: algorithm, and why the prefix matters

`go.sum` stores hashes computed by `golang.org/x/mod/sumdb/dirhash`. The `Hash1`
algorithm hashes a sorted list of lines, each `sha256(file)␠␠name`, then base64
-encodes the SHA-256 of that list, prefixed with `h1:`. `HashDir(dir, prefix,
dirhash.Hash1)` walks a directory and computes that hash, but replaces the on-disk
directory name with `prefix` in each `name` — so the hash is a function of the file
*contents* and their paths under `prefix`, not of where the tree happens to live on
disk. The canonical `prefix` is `module@version` (for example
`example.com/logfmt@v1.4.0`). Getting the prefix right is what makes a recomputed
hash comparable to a recorded one: the same bytes under a different prefix hash
differently, by design.

### The honest subtlety about vendored trees

A vendored subtree is not byte-identical to the full module the cache stores:
`go mod vendor` prunes to the packages your build imports and strips the
dependency's `_test.go` files. So the hash of a *vendored* module directory does
not equal that module's `go.sum` entry, which covers the complete module zip. The
correct attestation pattern is therefore to capture the vendored tree's hash *at
vendor time* into an attestation manifest, then re-verify the committed tree
against that recorded value on every release — using the same `dirhash` primitive
that `go mod verify` uses for the cache. `SumHash` is included so you can read a
module's recorded `h1:` from a `go.sum`, which is the analogous check for the
full-module hashes.

Create `attest.go`:

```go
package attest

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"

	"golang.org/x/mod/sumdb/dirhash"
)

// ErrTampered is returned when a tree's recomputed hash does not match the
// recorded value.
var ErrTampered = errors.New("attest: vendored bytes do not match recorded hash")

// Attest computes the h1: hash of the tree rooted at dir, using prefix
// (conventionally "module@version") in place of the directory name.
func Attest(dir, prefix string) (string, error) {
	h, err := dirhash.HashDir(dir, prefix, dirhash.Hash1)
	if err != nil {
		return "", fmt.Errorf("attest %s: %w", prefix, err)
	}
	return h, nil
}

// Verify recomputes the hash of dir under prefix and compares it to want.
// A mismatch returns an error wrapping ErrTampered.
func Verify(dir, prefix, want string) error {
	got, err := Attest(dir, prefix)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%w: %s: got %s, want %s", ErrTampered, prefix, got, want)
	}
	return nil
}

// SumHash returns the module-tree h1: hash recorded for modPath@version in a
// go.sum body. It ignores the separate "/go.mod" hash line.
func SumHash(goSum []byte, modPath, version string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(goSum))
	for sc.Scan() {
		fields := bytesFields(sc.Text())
		if len(fields) != 3 {
			continue
		}
		if fields[0] == modPath && fields[1] == version {
			return fields[2], nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("read go.sum: %w", err)
	}
	return "", fmt.Errorf("%s %s not found in go.sum", modPath, version)
}

// bytesFields splits a go.sum line on whitespace.
func bytesFields(line string) []string {
	var out []string
	for _, f := range bytes.Fields([]byte(line)) {
		out = append(out, string(f))
	}
	return out
}
```

### The runnable demo

The demo materializes a tiny module tree in a temp directory, attests it, verifies
the clean tree, then mutates a file and shows the tamper detection fire. Because
`HashDir` hashes contents under a fixed prefix (not the temp path), the printed
hash is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"example.com/attest"
)

const prefix = "example.com/logfmt@v1.4.0"

func main() {
	dir, err := os.MkdirTemp("", "vendored-logfmt")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	must(os.MkdirAll(filepath.Join(dir, "logfmt"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "logfmt", "logfmt.go"), []byte("package logfmt\n\nconst Version = \"v1.4.0\"\n"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "LICENSE"), []byte("MIT License\n"), 0o644))

	recorded, err := attest.Attest(dir, prefix)
	if err != nil {
		panic(err)
	}
	fmt.Println("attested:", recorded)

	if err := attest.Verify(dir, prefix, recorded); err != nil {
		fmt.Println("verify (clean):", err)
	} else {
		fmt.Println("verify (clean): ok")
	}

	// Tamper: append a byte to a vendored file.
	must(os.WriteFile(filepath.Join(dir, "logfmt", "logfmt.go"), []byte("package logfmt\n\nconst Version = \"v1.4.0\" // patched\n"), 0o644))
	if err := attest.Verify(dir, prefix, recorded); err != nil {
		fmt.Println("verify (tampered): blocked")
	} else {
		fmt.Println("verify (tampered): PASSED (bug!)")
	}
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
attested: h1:Xrm3AZajLUoWyNdsHm+RcVw2QisPYJWwv8JLe6cKjTk=
verify (clean): ok
verify (tampered): blocked
```

### Tests

`TestVerifyRoundTrip` materializes a tree in `t.TempDir()`, records its hash with
`dirhash.HashDir` directly, and asserts `Verify` matches — then mutates a file and
asserts `Verify` returns `ErrTampered`. `TestSumHash` parses a `go.sum` fixture,
returning the module-tree hash and skipping the `/go.mod` line.

Create `attest_test.go`:

```go
package attest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/mod/sumdb/dirhash"
)

func writeTree(t *testing.T) (dir, prefix string) {
	t.Helper()
	dir = t.TempDir()
	prefix = "example.com/logfmt@v1.4.0"
	if err := os.MkdirAll(filepath.Join(dir, "logfmt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "logfmt", "logfmt.go"), []byte("package logfmt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "LICENSE"), []byte("MIT License\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, prefix
}

func TestVerifyRoundTrip(t *testing.T) {
	t.Parallel()
	dir, prefix := writeTree(t)

	want, err := dirhash.HashDir(dir, prefix, dirhash.Hash1)
	if err != nil {
		t.Fatalf("HashDir: %v", err)
	}
	if err := Verify(dir, prefix, want); err != nil {
		t.Fatalf("Verify clean tree: %v", err)
	}

	// Mutate a vendored file: the recorded hash must no longer match.
	if err := os.WriteFile(filepath.Join(dir, "logfmt", "logfmt.go"), []byte("package logfmt // tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = Verify(dir, prefix, want)
	if !errors.Is(err, ErrTampered) {
		t.Fatalf("Verify tampered tree = %v; want ErrTampered", err)
	}
}

func TestSumHash(t *testing.T) {
	t.Parallel()
	goSum := "golang.org/x/mod v0.37.0 h1:AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHHIIIIJJJJKKK=\n" +
		"golang.org/x/mod v0.37.0/go.mod h1:ZZZZYYYYXXXXWWWWVVVVUUUUTTTTSSSSRRRRQQQ=\n"
	got, err := SumHash([]byte(goSum), "golang.org/x/mod", "v0.37.0")
	if err != nil {
		t.Fatalf("SumHash: %v", err)
	}
	if want := "h1:AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHHIIIIJJJJKKK="; got != want {
		t.Fatalf("SumHash = %q; want %q (must skip the /go.mod line)", got, want)
	}
	if _, err := SumHash([]byte(goSum), "nonexistent.com/x", "v1.0.0"); err == nil {
		t.Fatal("SumHash for absent module: want error, got nil")
	}
}

func Example() {
	dir, _ := os.MkdirTemp("", "attest-example")
	defer os.RemoveAll(dir)
	_ = os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644)
	h, _ := Attest(dir, "example.com/a@v1.0.0")
	fmt.Println(h)
	// Output: h1:VEUcYKYLbKBYq0eA/VEJ3mMQBzi4jjWpusjuduGJcBQ=
}
```

## Review

The verifier is correct when the recomputed hash is a pure function of the tree's
contents under the recorded prefix, which is exactly what `dirhash.HashDir`
guarantees — mutate any byte in any vendored file and the hash changes, so
`Verify` returns `ErrTampered`. The prefix discipline is load-bearing: verifying
with a different `module@version` than was recorded fails even on identical bytes,
because the prefix is folded into every hashed line. The honest limitation, stated
in the concepts and worth repeating, is that a pruned vendored subtree does not
reproduce the full module's `go.sum` entry; you attest against a hash captured at
`go mod vendor` time, not against the cache's full-module hash. `SumHash` reads the
module-tree line and deliberately skips the `/go.mod` line, which records a
separate hash of just the module's `go.mod` file.

## Resources

- [`golang.org/x/mod/sumdb/dirhash`](https://pkg.go.dev/golang.org/x/mod/sumdb/dirhash) — `HashDir`, `Hash1`, and `DirFiles`.
- [`go mod verify`](https://go.dev/ref/mod#go-mod-verify) — the command this attestation mirrors.
- [go.sum and module authentication](https://go.dev/ref/mod#go-sum-files) — what the `h1:` and `/go.mod` lines record.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-vendored-tool-directive-audit.md](09-vendored-tool-directive-audit.md)
