# Exercise 17: Merkle Root Over Fixed [32]byte Hash Arrays

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A Merkle tree is how Certificate Transparency logs let anyone verify a
certificate was actually logged without downloading the whole log, and how
git derives a commit-independent identity for a tree of blobs: hash the
leaves, then repeatedly hash pairs of hashes together until one root hash
remains, so that a single 32-byte value commits to an entire, potentially
huge, ordered set of leaves. `crypto/sha256.Sum256` is the reason this is
pleasant to write in Go — it returns `[32]byte`, not `[]byte`, so every node
in the tree, leaf or interior, is the same small, comparable, stack-friendly
array type. No defensive copy is ever needed when combining two hashes,
because both are independent values the moment they come out of `Sum256`.

This exercise builds `merkleroot`, a command that reads one leaf value per
line on stdin, hashes each with sha256, and prints the hex-encoded Merkle
root. The tool has to handle the three shapes that trip up a naive
implementation — an odd node count, a single leaf, and an empty tree — and
this module proves that changing one leaf anywhere changes the root.

This module is fully self-contained: its own `go mod init`, an executable
command, and its tests. Nothing here imports another exercise.

## What you'll build

```text
merkle/                        module example.com/merkle
  go.mod                       go 1.24
  merkle.go                    package main — Root(leaves [][32]byte) [32]byte; hashPair; leafOf
  merkle_test.go                package main — table over 4/3/2/1/0 leaves, tamper detection, determinism, run() end to end
  main.go                      package main — reads lines from stdin, prints the hex root
```

- Files: `merkle.go`, `merkle_test.go`, `main.go`.
- Implement: `Root(leaves [][32]byte) [32]byte` that repeatedly pairs adjacent nodes and hashes their 64-byte concatenation with `sha256.Sum256` until one node remains, duplicating the last node of an odd-length level, returning the single leaf unchanged for one leaf and the zero array for no leaves; `hashPair(a, b [32]byte) [32]byte` as the pairing primitive; `leafOf(line string) [32]byte` hashing one input line.
- Tool: `merkleroot` streams stdin line by line with `bufio.Scanner`, hashing each line into a leaf as it arrives rather than buffering the whole input, then prints the hex root followed by a newline. It takes no arguments — only stdin — so any positional argument is a usage error. Exit 0 on success, exit 2 for a bad flag or an unexpected argument, exit 1 for a stdin read failure.
- Test: four leaves (two full levels), three leaves (odd-node duplication), two leaves, a single leaf, an empty tree — each expected value hand-computed via nested `sha256.Sum256` calls in the test itself; tampering one leaf changes the root; the root is deterministic for equal leaf content held in different slices; `run` end to end over `strings.Reader` and `bytes.Buffer`, including the empty-stdin case and a rejected positional argument.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/merkle
cd ~/go-exercises/merkle
go mod init example.com/merkle
go mod edit -go=1.24
```

### Why [32]byte leaves need no defensive copy

`sha256.Sum256(data []byte) [32]byte` is a deliberate API design choice in
the standard library: it returns an array value, not a slice. That means
every digest handed around in this package — a leaf, an interior node, the
final root — is a genuinely independent 32-byte value the instant it is
produced. Contrast this with what would happen if `Sum256` returned
`[]byte`: two leaves computed from different inputs could, through some
aliasing bug, end up sharing backing storage, and mutating one through a
slice operation could corrupt the other silently. With `[32]byte`, that
category of bug cannot exist. `hashPair` takes two `[32]byte` values by
value, copies their 32 bytes each into a local `[64]byte` buffer, and hashes
that buffer — no allocation beyond the fixed 64-byte stack buffer, no shared
state between calls, and the caller's leaves are untouched no matter how many
times `Root` combines them going up the tree.

The three shapes that make a naive Merkle implementation wrong are all about
what happens away from the "nice" case of a power-of-two leaf count. An
**odd-length level** needs a partner for its last node; this implementation
duplicates that node and hashes it with itself, the common convention used by
Bitcoin-style and Certificate-Transparency-style trees (conventions differ
across systems — the load-bearing point for this exercise is picking one and
applying it consistently, since the verifier must know exactly which rule
the prover used). A **single leaf** has no pair at all, so the loop condition
`len(level) > 1` never fires and the root is simply that leaf, unchanged. An
**empty tree** has no leaves to hash; returning the zero `[32]byte` is the
one sane default (real systems typically treat this as "no valid tree" at a
higher layer, which is a decision that belongs outside this primitive).

Create `merkle.go`:

```go
// Command merkleroot reads leaf values as lines on stdin, hashes each one to
// a fixed 32-byte leaf, and prints the hex-encoded Merkle root -- the shape
// used by Certificate Transparency logs and by git's object graph (with
// sha1 there, sha256 here).
package main

import "crypto/sha256"

// Root computes the Merkle root of leaves. Each level pairs adjacent nodes
// and hashes their concatenation with sha256.Sum256, halving the level size
// on every pass until one node remains. A level with an odd number of nodes
// duplicates its last node so it can still be paired, per the common Merkle
// convention (as used by Bitcoin and Certificate Transparency). Root of a
// single leaf is that leaf unchanged. Root of no leaves is the zero array,
// since sha256.Sum256 returns [32]byte -- every node in the tree, leaf or
// interior, is that same fixed-size, comparable, allocation-free type.
func Root(leaves [][32]byte) [32]byte {
	if len(leaves) == 0 {
		return [32]byte{}
	}
	level := make([][32]byte, len(leaves))
	copy(level, leaves)

	for len(level) > 1 {
		next := make([][32]byte, 0, (len(level)+1)/2)
		for i := 0; i < len(level); i += 2 {
			if i+1 < len(level) {
				next = append(next, hashPair(level[i], level[i+1]))
			} else {
				// Odd node out: duplicate it so it still has a pair.
				next = append(next, hashPair(level[i], level[i]))
			}
		}
		level = next
	}
	return level[0]
}

// hashPair hashes the 64-byte concatenation of two fixed 32-byte digests.
// Because sha256.Sum256 returns [32]byte rather than []byte, a and b are
// already independent, immutable values -- no defensive copy is needed
// before combining them, and the concatenation buffer lives on the stack.
func hashPair(a, b [32]byte) [32]byte {
	var buf [64]byte
	copy(buf[:32], a[:])
	copy(buf[32:], b[:])
	return sha256.Sum256(buf[:])
}

// leafOf hashes one input line to a fixed-size leaf. Each line is hashed
// independently, so two identical lines produce identical, equal leaves.
func leafOf(line string) [32]byte {
	return sha256.Sum256([]byte(line))
}
```

### The tool

`merkleroot` streams: `bufio.Scanner` reads one line at a time and hands
each straight to `leafOf`, so the tool never holds more than one line's
worth of raw text in memory at once, only the growing `[][32]byte` slice of
already-hashed leaves. That matters because the leaves themselves are the
smallest possible representation of arbitrarily long input lines — hashing
early is what keeps a multi-gigabyte input from ever being buffered whole.
`run` takes `args`, an `io.Reader` for stdin, and an `io.Writer` for stdout,
so the test drives it with `strings.Reader` and `bytes.Buffer` without a
real process. This tool accepts no positional arguments — every leaf comes
from stdin — so `fs.NArg() > 0` is itself a usage error, mapped to exit code
2 alongside a bad flag; a genuine stdin read failure maps to exit code 1.

Create `main.go`:

```go
package main

import (
	"bufio"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

// errUsage marks a failure the caller can fix by changing the command line:
// this tool takes no arguments, only stdin, so any positional argument is a
// usage error. main maps errUsage to exit code 2; a stdin read failure maps
// to exit code 1.
var errUsage = errors.New("usage")

// run reads one leaf value per line from stdin, computes the Merkle root
// over their sha256 hashes, and writes the hex-encoded root followed by a
// newline to stdout. It streams the input with bufio.Scanner rather than
// buffering the whole stream, so it never allocates more than one line's
// worth of memory ahead of the leaf slice itself.
func run(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("merkleroot", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%w: merkleroot takes no arguments, only stdin", errUsage)
	}

	var leaves [][32]byte
	scanner := bufio.NewScanner(stdin)
	for scanner.Scan() {
		leaves = append(leaves, leafOf(scanner.Text()))
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading stdin: %w", err)
	}

	root := Root(leaves)
	fmt.Fprintln(stdout, hex.EncodeToString(root[:]))
	return nil
}

func main() {
	flag.CommandLine.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: merkleroot < leaves.txt")
		fmt.Fprintln(os.Stderr, "reads one leaf per line from stdin, prints the hex Merkle root.")
	}
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "merkleroot:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
```

Run it:

```bash
printf 'tx1\ntx2\ntx3\ntx4\n' | go run .
printf 'tx1\ntx2\ntx3\n' | go run .
printf '' | go run .
```

Expected output:

```text
ea59a369466be42d1a4783f09ae0721a5a157d6dba9c4b053d407b5a4b9af145
88f65ae747487b0d7756dd09f2ee1391506691e9644bcd632d7e1892e6d07ba9
0000000000000000000000000000000000000000000000000000000000000000
```

Each printed line is exactly 64 hex digits (32 bytes). The first line is the
root of four leaves (two full pairing levels); the second is three leaves,
exercising odd-node duplication; the third is the empty-tree base case, the
all-zero root, produced when stdin has no lines at all. Feeding a positional
argument instead of relying on stdin alone -- `echo tx1 | go run . leaves.txt`
-- prints `merkleroot: usage: merkleroot takes no arguments, only stdin` to
stderr and exits 2, since this tool reads only stdin.

### Tests

`TestRoot` is the shape table: four leaves exercises two full pairing
levels, three leaves exercises the odd-node duplication rule, two leaves is
the minimal pairing case, one leaf is the identity base case, and no leaves
is the zero-value base case. Each expected value is computed independently
in the test with a second helper, `pair`, that reproduces `hashPair`'s exact
64-byte concatenation — so the test is checking the algorithm's structure
against a hand-built reference tree, not just calling `Root` and comparing
`Root` to itself. `TestTamperChangesRoot` is the property that makes a
Merkle root useful as a commitment: flipping one leaf must change the root.
`TestRootDeterministic` asserts the root depends only on leaf content, not
on which `[][32]byte` slice value holds it. `TestRun` drives the command
end to end: the same four- and three-leaf cases through stdin instead of
calling `Root` directly, the empty-stdin base case, and a rejected
positional argument.

Create `merkle_test.go`:

```go
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func leaf(s string) [32]byte {
	return sha256.Sum256([]byte(s))
}

// pair reproduces the exact 64-byte concatenation hashPair performs, kept as
// a second, independent expression in the test so the pinned expectations
// below are not just "call Root and compare Root to itself".
func pair(a, b [32]byte) [32]byte {
	var buf [64]byte
	copy(buf[:32], a[:])
	copy(buf[32:], b[:])
	return sha256.Sum256(buf[:])
}

func TestRoot(t *testing.T) {
	t.Parallel()

	a, b, c, d := leaf("a"), leaf("b"), leaf("c"), leaf("d")

	cases := []struct {
		name   string
		leaves [][32]byte
		want   [32]byte
	}{
		{
			name:   "four leaves, two full levels",
			leaves: [][32]byte{a, b, c, d},
			want:   pair(pair(a, b), pair(c, d)),
		},
		{
			name:   "three leaves, last node duplicated",
			leaves: [][32]byte{a, b, c},
			want:   pair(pair(a, b), pair(c, c)),
		},
		{
			name:   "two leaves",
			leaves: [][32]byte{a, b},
			want:   pair(a, b),
		},
		{
			name:   "single leaf returns itself unchanged",
			leaves: [][32]byte{a},
			want:   a,
		},
		{
			name:   "empty tree returns the zero array",
			leaves: nil,
			want:   [32]byte{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Root(tc.leaves)
			if got != tc.want {
				t.Fatalf("Root(%s) = %x, want %x", tc.name, got, tc.want)
			}
		})
	}
}

// TestTamperChangesRoot asserts that flipping a single leaf anywhere in the
// tree changes the root -- the property that makes a Merkle root usable as
// a tamper-evident commitment to the whole leaf set.
func TestTamperChangesRoot(t *testing.T) {
	t.Parallel()

	original := [][32]byte{leaf("tx1"), leaf("tx2"), leaf("tx3"), leaf("tx4")}
	tampered := make([][32]byte, len(original))
	copy(tampered, original)
	tampered[2] = leaf("tx3-tampered")

	rootOriginal := Root(original)
	rootTampered := Root(tampered)
	if rootOriginal == rootTampered {
		t.Fatal("tampering one leaf must change the root, but it did not")
	}
}

// TestRootDeterministic asserts the same leaf set always produces the same
// root, independent of the [][32]byte slice's own identity.
func TestRootDeterministic(t *testing.T) {
	t.Parallel()

	leaves := [][32]byte{leaf("x"), leaf("y"), leaf("z")}
	copyLeaves := make([][32]byte, len(leaves))
	copy(copyLeaves, leaves)

	if Root(leaves) != Root(copyLeaves) {
		t.Fatal("Root must be deterministic for equal leaf content")
	}
}

// rootHex reproduces run's own leaf-hashing and root computation over a
// fixed list of lines, so the expectations below are computed the same way
// the tool computes them, then compared against the tool's actual stdout.
func rootHex(lines ...string) string {
	leaves := make([][32]byte, len(lines))
	for i, l := range lines {
		leaves[i] = leaf(l)
	}
	root := Root(leaves)
	return hex.EncodeToString(root[:])
}

// TestRun exercises the command end to end over strings.Reader and
// bytes.Buffer: four lines (two full levels), three lines (odd-node
// duplication), no lines (the empty-tree base case), and a rejected
// positional argument.
func TestRun(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		args    []string
		stdin   string
		want    string
		wantErr bool
	}{
		{
			name:  "four leaves",
			stdin: "tx1\ntx2\ntx3\ntx4\n",
			want:  rootHex("tx1", "tx2", "tx3", "tx4") + "\n",
		},
		{
			name:  "three leaves, odd node duplicated",
			stdin: "tx1\ntx2\ntx3\n",
			want:  rootHex("tx1", "tx2", "tx3") + "\n",
		},
		{
			name:  "empty stdin yields the zero root",
			stdin: "",
			want:  strings.Repeat("00", 32) + "\n",
		},
		{
			name:    "positional argument is rejected",
			args:    []string{"leaves.txt"},
			stdin:   "tx1\n",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var stdout bytes.Buffer
			err := run(tc.args, strings.NewReader(tc.stdin), &stdout)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("run(%v): want error, got nil", tc.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("run(%v): %v", tc.args, err)
			}
			if stdout.String() != tc.want {
				t.Fatalf("run stdout = %q, want %q", stdout.String(), tc.want)
			}
		})
	}
}
```

## Review

`Root` is correct when it reduces `leaves` to a single `[32]byte` by
repeatedly hashing pairs, when an odd-length level duplicates its last node
instead of dropping it or panicking, and when the two degenerate cases —
one leaf, zero leaves — return the documented base values instead of
crashing on an empty slice index. The mistake this design avoids is
forgetting the odd-node case entirely and only testing tree shapes that
happen to be powers of two: that gap is exactly what
`"three leaves, last node duplicated"` exists to close, and it is a subtle
enough shape that a `for i := 0; i+1 < len(level); i += 2` loop (which
silently drops the trailing node instead of duplicating it) would still pass
every even-count test while producing a root that omits a leaf's
contribution — a serious correctness bug for a commitment scheme.
`merkleroot` streams its input rather than buffering it and rejects a
positional argument as a usage error (exit 2), reserving exit 1 for a real
stdin failure. Run `go test -count=1 -race ./...` to confirm the shape
table, the tamper-detection property, determinism, and `run`'s end-to-end
behavior.

## Resources

- [crypto/sha256](https://pkg.go.dev/crypto/sha256#Sum256) — `Sum256` returning `[32]byte`, the array type this whole module is built around.
- [RFC 6962 (Certificate Transparency)](https://www.rfc-editor.org/rfc/rfc6962) — a real-world Merkle tree spec, including its own odd-node convention (section 2.1).
- [Merkle tree (Wikipedia)](https://en.wikipedia.org/wiki/Merkle_tree) — the general structure and its use in Bitcoin, git, and CT logs.
- [bufio.Scanner](https://pkg.go.dev/bufio#Scanner) — the line-at-a-time streaming reader `merkleroot` uses instead of buffering all of stdin.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [16-hotp-truncation-hmac-array.md](16-hotp-truncation-hmac-array.md) | Next: [18-dns-header-12-byte-codec.md](18-dns-header-12-byte-codec.md)
