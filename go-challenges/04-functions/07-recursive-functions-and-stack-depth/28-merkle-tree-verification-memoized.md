# Exercise 28: Compute and Verify Merkle Tree Hashes Recursively

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A Merkle tree turns a list of data items into a single root hash by
recursively pairing subtrees and hashing the pair — the textbook divide
and conquer recurrence, and the reason two systems can agree an entire
dataset matches by comparing one 32-byte value instead of the whole thing.
The interesting engineering problem is not building the tree once; it is
what happens on the *next* sync, when a device appends or edits a handful
of items and needs a new root without rehashing everything it already
hashed a moment ago. This module builds the tree recursively, verifies
individual leaf inclusion recursively against a root hash, and adds a
memoizing builder that reuses subtree hashes across successive builds —
exactly the shape an incremental sync check needs.

This module is fully self-contained: its own `go mod init`, the tree
inline, its own demo and tests.

## What you'll build

```text
merkletree/                   independent module: example.com/merkletree
  go.mod                        go 1.24
  merkletree.go                  type Node; Build (recursive); type Builder (memoized); Proof/Verify (recursive)
  merkletree_test.go              empty leaves, single leaf, determinism, proof accepts/rejects, out-of-range, memo reuse
  cmd/
    demo/
      main.go                     builds v1, appends a leaf for v2 with a shared Builder, verifies a leaf proof
```

- Files: `merkletree.go`, `cmd/demo/main.go`, `merkletree_test.go`.
- Implement: `Node{Hash [32]byte; Left, Right *Node; Leaf []byte}`, `Build(leaves [][]byte) (*Node, error)`, `Builder` with a memoizing `(*Builder) Build`, `Proof(leaves [][]byte, index int) ([]ProofStep, error)`, and `Verify(leaf []byte, proof []ProofStep, root [32]byte) bool`.
- Test: empty leaves rejected; a single leaf's hash; determinism across repeated builds; every leaf's proof verifies against the root; a substituted leaf's proof is rejected; an out-of-range proof index; a shared `Builder` reuses the unchanged prefix subtree when a leaf set grows.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Domain-separated hashing, and memoizing on content instead of position

Two design choices matter here beyond "recurse and hash pairs." First,
leaf hashes are computed as `SHA-256(0x00 || data)` and internal-node
hashes as `SHA-256(0x01 || left || right)`, following RFC 6962's Merkle
Tree Hash definition. Without that prefix byte, an attacker who can choose
what gets hashed could present an internal node's hash as though it were
some leaf's hash — a second-preimage forgery, since both are otherwise
just "a SHA-256 output." The one-byte domain tag makes a leaf hash and an
internal hash structurally impossible to confuse. Second, an odd-sized
leaf range is never handled by duplicating the last leaf (a mistake with
its own known weaknesses); instead `build` splits at
`largestPowerOfTwoLessThan(len(leaves))`, the same rule RFC 6962 uses, so
a tree's shape is a pure function of leaf count.

`Builder` layers memoization on top of `build`'s recursion, but the memo
key is not the leaf range's position — it is a hash of the leaf range's
own content (`signature`). That distinction is what makes the memo useful
across two different calls to `Build` rather than only within one: when
`v2` appends a fifth leaf to `v1`'s four, the recursive split for five
leaves happens to be `4 + 1`, so `Builder.build` recurses into
`build(leaves[0:4])` — the *exact same four leaves, in the same order*,
that `v1`'s call already fully hashed. Because the memo key is derived
from content, that recursive call is a hit: the entire left subtree,
including every one of its own internal recursive calls, is skipped
outright. A position-keyed memo (e.g., `fmt.Sprintf("%d-%d", start, end)`
on indices into whatever backing slice happened to be passed) would miss
here, because `v1` and `v2` are different slices with no shared indices to
key on — content is the only thing that is actually the same.

Create `merkletree.go`:

```go
// Package merkletree builds Merkle trees over a leaf list, recursively, and
// verifies leaf inclusion against a root hash. It follows the RFC 6962
// Merkle Tree Hash construction: leaf and internal hashes are domain
// separated (a 0x00 prefix for leaves, 0x01 for internal nodes), which
// defeats the classic second-preimage attack where an internal node's hash
// is replayed as though it were a leaf's. A memoized Builder additionally
// reuses previously computed subtree hashes across successive builds --
// the shape a two-device sync check actually needs, since most of a large
// tree is usually unchanged between snapshots.
package merkletree

import (
	"crypto/sha256"
	"errors"
	"fmt"
)

// ErrEmptyLeaves is returned when Build or a Builder is asked to hash zero
// leaves.
var ErrEmptyLeaves = errors.New("merkletree: cannot build tree with zero leaves")

// Node is one node (leaf or internal) of a Merkle tree.
type Node struct {
	Hash  [32]byte
	Left  *Node
	Right *Node
	Leaf  []byte // non-nil only for leaf nodes
}

func leafHash(data []byte) [32]byte {
	buf := make([]byte, 0, 1+len(data))
	buf = append(buf, 0x00)
	buf = append(buf, data...)
	return sha256.Sum256(buf)
}

func nodeHash(left, right [32]byte) [32]byte {
	buf := make([]byte, 0, 1+64)
	buf = append(buf, 0x01)
	buf = append(buf, left[:]...)
	buf = append(buf, right[:]...)
	return sha256.Sum256(buf)
}

// largestPowerOfTwoLessThan returns the largest power of two strictly less
// than n, for n > 1. RFC 6962 splits an n-leaf range at this point rather
// than duplicating a leftover leaf, so a tree's shape is a well-defined
// function of leaf count alone.
func largestPowerOfTwoLessThan(n int) int {
	k := 1
	for k*2 < n {
		k *= 2
	}
	return k
}

// Build recursively builds a Merkle tree over leaves.
func Build(leaves [][]byte) (*Node, error) {
	if len(leaves) == 0 {
		return nil, ErrEmptyLeaves
	}
	return build(leaves), nil
}

func build(leaves [][]byte) *Node {
	if len(leaves) == 1 {
		return &Node{Hash: leafHash(leaves[0]), Leaf: leaves[0]}
	}
	k := largestPowerOfTwoLessThan(len(leaves))
	left := build(leaves[:k])
	right := build(leaves[k:])
	return &Node{Hash: nodeHash(left.Hash, right.Hash), Left: left, Right: right}
}

// Builder builds Merkle trees the same way Build does, but memoizes every
// subtree it computes, keyed by the exact leaf content that subtree
// covers. Reusing a Builder across two successive leaf sets that share a
// contiguous run of unchanged leaves -- the common case when syncing an
// appended-to or lightly edited dataset -- turns an unrelated fraction of
// the recomputation into memo hits instead of rehashing.
type Builder struct {
	memo   map[string]*Node
	Hits   int
	Misses int
}

// NewBuilder returns a Builder with an empty memo.
func NewBuilder() *Builder {
	return &Builder{memo: make(map[string]*Node)}
}

// Build builds a Merkle tree over leaves, reusing any subtree already
// present in b's memo from an earlier call.
func (b *Builder) Build(leaves [][]byte) (*Node, error) {
	if len(leaves) == 0 {
		return nil, ErrEmptyLeaves
	}
	return b.build(leaves), nil
}

func (b *Builder) build(leaves [][]byte) *Node {
	key := signature(leaves)
	if n, ok := b.memo[key]; ok {
		b.Hits++
		return n
	}
	b.Misses++

	var n *Node
	if len(leaves) == 1 {
		n = &Node{Hash: leafHash(leaves[0]), Leaf: leaves[0]}
	} else {
		k := largestPowerOfTwoLessThan(len(leaves))
		left := b.build(leaves[:k])
		right := b.build(leaves[k:])
		n = &Node{Hash: nodeHash(left.Hash, right.Hash), Left: left, Right: right}
	}
	b.memo[key] = n
	return n
}

// signature canonicalizes a leaf slice by its content, not its position, so
// the same run of leaf bytes hashes to the same memo key wherever it
// appears across successive builds.
func signature(leaves [][]byte) string {
	h := sha256.New()
	for _, l := range leaves {
		h.Write(l)
		h.Write([]byte{0})
	}
	return string(h.Sum(nil))
}

// ProofStep is one level of a Merkle inclusion proof: the sibling hash at
// that level, and whether the sibling sits to the left of the hash being
// verified.
type ProofStep struct {
	Sibling [32]byte
	IsLeft  bool
}

// Proof returns the Merkle inclusion proof for the leaf at index among
// leaves, ordered from the leaf's own sibling up to the level just below
// the root.
func Proof(leaves [][]byte, index int) ([]ProofStep, error) {
	if index < 0 || index >= len(leaves) {
		return nil, fmt.Errorf("merkletree: index %d out of range [0,%d)", index, len(leaves))
	}
	return proof(leaves, index), nil
}

func proof(leaves [][]byte, index int) []ProofStep {
	if len(leaves) == 1 {
		return nil
	}
	k := largestPowerOfTwoLessThan(len(leaves))
	if index < k {
		rightHash := build(leaves[k:]).Hash
		return append(proof(leaves[:k], index), ProofStep{Sibling: rightHash, IsLeft: false})
	}
	leftHash := build(leaves[:k]).Hash
	return append(proof(leaves[k:], index-k), ProofStep{Sibling: leftHash, IsLeft: true})
}

// Verify reports whether leaf is included, at the position proof was
// generated for, in the tree whose root hash is root. It recomputes the
// path from leaf to root one proof step at a time and compares the final
// hash against root.
func Verify(leaf []byte, proof []ProofStep, root [32]byte) bool {
	return verify(leafHash(leaf), proof, root)
}

func verify(current [32]byte, proof []ProofStep, root [32]byte) bool {
	if len(proof) == 0 {
		return current == root
	}
	step := proof[0]
	var next [32]byte
	if step.IsLeft {
		next = nodeHash(step.Sibling, current)
	} else {
		next = nodeHash(current, step.Sibling)
	}
	return verify(next, proof[1:], root)
}
```

### The runnable demo

The demo builds a four-leaf tree, then a five-leaf tree (the first four
leaves unchanged, one appended) using the *same* `Builder`, printing the
hit/miss counters to show the unchanged prefix subtree gets reused. It
then verifies one leaf's inclusion proof, and shows an unrelated leaf
failing the same proof.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/hex"
	"fmt"

	"example.com/merkletree"
)

func leaves(strs ...string) [][]byte {
	out := make([][]byte, len(strs))
	for i, s := range strs {
		out[i] = []byte(s)
	}
	return out
}

func main() {
	builder := merkletree.NewBuilder()

	v1 := leaves("a", "b", "c", "d")
	root1, err := builder.Build(v1)
	if err != nil {
		panic(err)
	}
	fmt.Printf("v1 root: %s (hits=%d misses=%d)\n", hex.EncodeToString(root1.Hash[:8]), builder.Hits, builder.Misses)

	v2 := leaves("a", "b", "c", "d", "e")
	root2, err := builder.Build(v2)
	if err != nil {
		panic(err)
	}
	fmt.Printf("v2 root: %s (hits=%d misses=%d)\n", hex.EncodeToString(root2.Hash[:8]), builder.Hits, builder.Misses)

	proof, err := merkletree.Proof(v2, 2)
	if err != nil {
		panic(err)
	}
	ok := merkletree.Verify([]byte("c"), proof, root2.Hash)
	fmt.Println("leaf \"c\" verifies against v2 root:", ok)

	tampered := merkletree.Verify([]byte("z"), proof, root2.Hash)
	fmt.Println("leaf \"z\" verifies against v2 root:", tampered)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
v1 root: 33376a3bd63e9993 (hits=0 misses=7)
v2 root: fe14a5426fbd70c0 (hits=1 misses=9)
leaf "c" verifies against v2 root: true
leaf "z" verifies against v2 root: false
```

### Tests

`TestBuildRejectsEmptyLeaves`, `TestBuildSingleLeafIsItsOwnHash`, and
`TestBuildIsDeterministic` check the basic contract. `TestVerifyAcceptsEveryLeaf`
proves every leaf's own generated proof verifies against the tree's root.
`TestVerifyRejectsWrongLeaf` proves a proof is bound to the leaf it was
generated for, not just to the tree shape. `TestProofRejectsOutOfRangeIndex`
covers input validation. `TestBuilderMemoReusesUnchangedPrefixSubtree` is
the test this exercise exists for: building `v1` then `v2` with the same
`Builder` must produce at least one memo hit and exactly two new misses
(the new leaf, and the new top-level combination) — proof the unchanged
four-leaf prefix subtree was reused rather than rehashed — while still
matching the unmemoized `Build`'s root hash exactly.

Create `merkletree_test.go`:

```go
package merkletree

import "testing"

func leaves(strs ...string) [][]byte {
	out := make([][]byte, len(strs))
	for i, s := range strs {
		out[i] = []byte(s)
	}
	return out
}

func TestBuildRejectsEmptyLeaves(t *testing.T) {
	t.Parallel()

	if _, err := Build(nil); err != ErrEmptyLeaves {
		t.Fatalf("Build(nil) error = %v, want %v", err, ErrEmptyLeaves)
	}
}

func TestBuildSingleLeafIsItsOwnHash(t *testing.T) {
	t.Parallel()

	root, err := Build(leaves("only"))
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	want := leafHash([]byte("only"))
	if root.Hash != want {
		t.Fatalf("root.Hash = %x, want %x", root.Hash, want)
	}
}

func TestBuildIsDeterministic(t *testing.T) {
	t.Parallel()

	l := leaves("a", "b", "c", "d", "e")
	r1, err := Build(l)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	r2, err := Build(l)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if r1.Hash != r2.Hash {
		t.Fatal("Build() is not deterministic for the same leaf set")
	}
}

func TestVerifyAcceptsEveryLeaf(t *testing.T) {
	t.Parallel()

	l := leaves("a", "b", "c", "d", "e")
	root, err := Build(l)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	for i, leaf := range l {
		proof, err := Proof(l, i)
		if err != nil {
			t.Fatalf("Proof(%d) error = %v", i, err)
		}
		if !Verify(leaf, proof, root.Hash) {
			t.Errorf("Verify() = false for leaf %d (%q), want true", i, leaf)
		}
	}
}

func TestVerifyRejectsWrongLeaf(t *testing.T) {
	t.Parallel()

	l := leaves("a", "b", "c", "d")
	root, err := Build(l)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	proof, err := Proof(l, 2)
	if err != nil {
		t.Fatalf("Proof() error = %v", err)
	}
	if Verify([]byte("tampered"), proof, root.Hash) {
		t.Error("Verify() = true for a leaf that was never in the tree, want false")
	}
}

func TestProofRejectsOutOfRangeIndex(t *testing.T) {
	t.Parallel()

	l := leaves("a", "b")
	if _, err := Proof(l, 5); err == nil {
		t.Fatal("expected error for out-of-range index")
	}
}

// TestBuilderMemoReusesUnchangedPrefixSubtree is the test that justifies
// the whole exercise: building an appended-to leaf set with the same
// Builder must reuse the hash of the unchanged four-leaf prefix rather
// than recomputing it, proving synchronization work scales with what
// changed, not with the whole dataset.
func TestBuilderMemoReusesUnchangedPrefixSubtree(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	v1 := leaves("a", "b", "c", "d")
	root1, err := b.Build(v1)
	if err != nil {
		t.Fatalf("Build(v1) error = %v", err)
	}
	missesAfterV1 := b.Misses
	if b.Hits != 0 {
		t.Fatalf("Hits after first build = %d, want 0", b.Hits)
	}

	v2 := leaves("a", "b", "c", "d", "e")
	root2, err := b.Build(v2)
	if err != nil {
		t.Fatalf("Build(v2) error = %v", err)
	}

	if b.Hits == 0 {
		t.Fatal("Hits after second build = 0, want at least 1 (the unchanged a,b,c,d subtree)")
	}
	// Only "e" (a new leaf) and the new 5-leaf top combination should be
	// new work; everything under the unchanged prefix must be served from
	// the memo.
	newMisses := b.Misses - missesAfterV1
	if newMisses != 2 {
		t.Fatalf("new misses building v2 = %d, want 2 (leaf \"e\" + new root combination)", newMisses)
	}

	// Cross-check against unmemoized Build: both builders must agree on
	// the root hash regardless of memoization.
	wantRoot1, _ := Build(v1)
	wantRoot2, _ := Build(v2)
	if root1.Hash != wantRoot1.Hash {
		t.Fatal("memoized root1 does not match unmemoized Build")
	}
	if root2.Hash != wantRoot2.Hash {
		t.Fatal("memoized root2 does not match unmemoized Build")
	}
}
```

## Review

The tree is correct when `Build` and `Builder.Build` always agree on the
root hash for the same leaves, and when `Verify` accepts exactly the leaf
and position a proof was generated for and rejects anything else.
`TestBuilderMemoReusesUnchangedPrefixSubtree` is the test that would fail
(with `Hits` stuck at 0) on a version of this exercise that memoizes by
range position (`start, end` indices into the current call's slice)
instead of by leaf content — a plausible-looking simplification that
happens to work within a single `Build` call but never fires across two
different calls, which is the entire point of exposing a reusable
`Builder`. The other correctness-critical detail, `leafHash`'s and
`nodeHash`'s distinct domain-separation prefixes, has no dedicated test
here precisely because getting it wrong does not fail any test in this
file — it silently opens a forgery the tests as written cannot see, which
is exactly why the constant needs to be understood, not just typed
correctly.

## Resources

- [RFC 6962: Certificate Transparency (Merkle Tree Hash, Section 2.1)](https://www.rfc-editor.org/rfc/rfc6962#section-2.1)
- [crypto/sha256 package](https://pkg.go.dev/crypto/sha256)
- [Wikipedia: Merkle tree](https://en.wikipedia.org/wiki/Merkle_tree)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [27-distributed-trace-span-aggregation.md](27-distributed-trace-span-aggregation.md) | Next: [29-feature-flag-rule-evaluation-memoized.md](29-feature-flag-rule-evaluation-memoized.md)
