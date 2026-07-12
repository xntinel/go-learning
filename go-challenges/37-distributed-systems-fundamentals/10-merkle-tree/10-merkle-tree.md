# 10. Merkle Tree

A Merkle tree is a binary hash tree where each leaf holds the hash of a data block and each internal node holds the hash of its two children. Comparing two trees takes O(1) to detect any difference (root hash mismatch) and O(log N + D) in the best case to locate which blocks differ (when differences cluster in the same subtree), or O(D * log N) in the worst case when D differing blocks are spread across the tree, where D is the number of differing blocks and N is the total number of leaves. That property makes Merkle trees the core anti-entropy primitive in Cassandra, DynamoDB, Git, and certificate transparency logs.

```text
merkle/
  go.mod
  merkle.go
  merkle_test.go
  cmd/merkle/main.go
```

The package exposes `MerkleTree` with construction from data blocks, `RootHash`, `Diff`, `Update`, `ProofOfInclusion`, and `VerifyProof`. The tests are the verification — there is no eyeballed output block.

## Concepts

### Hash Tree Structure

Store the tree as a flat array using the heap indexing convention: for a node at index `i`, its left child is at `2i+1`, its right child is at `2i+2`, and its parent is at `(i-1)/2`. Leaves occupy the last level. If the number of data blocks is not a power of two, pad with empty-hash leaves so the tree is always complete. Padding keeps the index arithmetic constant; a missing leaf contributes a fixed sentinel value rather than shifting all sibling indices.

Leaf hash: `H(data)`. Internal node hash: `H(left_hash || right_hash)`. The concatenation before hashing is the defining property — it prevents second-preimage attacks that would allow an attacker to forge a membership proof by claiming a node is a leaf.

### Difference Detection in O(log N + D)

Two replicas compare root hashes. If they match, the datasets are identical — no transfer needed. If they differ, compare the left subtree root and right subtree root independently, then recurse only into branches where hashes differ. Each matching subtree prunes an entire half of the remaining search space. The total nodes visited is O(log N) per differing leaf, so O((log N) * D) in the worst case when differences are spread across the tree, which is strictly better than O(N) naive comparison for D << N.

### Incremental Updates in O(log N)

Updating a single leaf requires recomputing only the hashes on the path from that leaf to the root — O(log N) nodes. The siblings on that path are unchanged. This is the same invariant maintained by a segment tree or a binary indexed tree: only the ancestors of the modified cell are invalidated.

### Proof of Inclusion (Merkle Proof)

To prove that block at index `i` is in a tree with root hash `R`, provide the sibling hash at each level from the leaf up to the root. The verifier recomputes the root hash using only the block data and the sibling hashes, without any other tree node. If the recomputed root matches `R`, membership is proven. The proof length is O(log N) — exactly the height of the tree.

### Anti-Entropy Application

Two database replicas exchange root hashes over a lightweight RPC. A hash match means no repair is needed. A hash mismatch triggers a recursive subtree comparison that narrows the inconsistency to specific key ranges, which are then synced. The key efficiency guarantee: a tree with one million leaves and three inconsistent blocks causes only about sixty hash comparisons rather than one million data comparisons.

## Exercises

### Exercise 1: MerkleTree Construction and RootHash

Create `merkle.go`:

```go
// merkle.go
package merkle

import (
	"crypto/sha256"
	"encoding/hex"
)

// MerkleTree is a complete binary hash tree stored as a flat array.
// Index 0 is the root. For node i: left child = 2i+1, right child = 2i+2.
type MerkleTree struct {
	nodes     [][]byte // tree node hashes, len = 2*capacity - 1
	leaves    [][]byte // raw data blocks (parallel to the leaf level)
	leafCount int      // number of actual leaves (rest are padding)
	capacity  int      // next power of two >= leafCount
}

// emptyHash is the canonical hash for a padding leaf.
var emptyHash = sha256.Sum256(nil)

// hashData returns SHA-256 of a data block (leaf hash).
func hashData(data []byte) []byte {
	h := sha256.Sum256(data)
	return h[:]
}

// hashPair returns SHA-256 of the concatenation of two child hashes (internal node hash).
func hashPair(left, right []byte) []byte {
	combined := make([]byte, len(left)+len(right))
	copy(combined, left)
	copy(combined[len(left):], right)
	h := sha256.Sum256(combined)
	return h[:]
}

// nextPow2 returns the smallest power of two that is >= n, minimum 1.
func nextPow2(n int) int {
	if n <= 1 {
		return 1
	}
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}

// New builds a Merkle tree from the given data blocks.
// The tree is padded with empty-hash leaves to the next power of two.
func New(blocks [][]byte) *MerkleTree {
	cap := nextPow2(len(blocks))
	totalNodes := 2*cap - 1

	t := &MerkleTree{
		nodes:     make([][]byte, totalNodes),
		leaves:    make([][]byte, len(blocks)),
		leafCount: len(blocks),
		capacity:  cap,
	}

	// Copy leaf data.
	for i, b := range blocks {
		t.leaves[i] = b
	}

	// Set leaf hashes. leafOffset is the index of the first leaf in nodes[].
	leafOffset := cap - 1
	for i := 0; i < cap; i++ {
		if i < len(blocks) {
			t.nodes[leafOffset+i] = hashData(blocks[i])
		} else {
			h := emptyHash
			t.nodes[leafOffset+i] = h[:]
		}
	}

	// Build internal nodes bottom-up.
	for i := leafOffset - 1; i >= 0; i-- {
		t.nodes[i] = hashPair(t.nodes[2*i+1], t.nodes[2*i+2])
	}

	return t
}

// RootHash returns the root hash of the tree (fingerprint of the entire dataset).
// Returns nil for an empty tree.
func (t *MerkleTree) RootHash() []byte {
	if len(t.nodes) == 0 {
		return nil
	}
	return t.nodes[0]
}

// RootHashHex returns the root hash as a hex string.
func (t *MerkleTree) RootHashHex() string {
	return hex.EncodeToString(t.RootHash())
}

// Diff returns the indices of data blocks that differ between t and other.
// Both trees must have been built from datasets of the same size.
// Traversal visits only branches where hashes differ.
func (t *MerkleTree) Diff(other *MerkleTree) []int {
	if t.capacity != other.capacity {
		// Different sizes: report all indices of the smaller tree as differing.
		min := t.leafCount
		if other.leafCount < min {
			min = other.leafCount
		}
		out := make([]int, min)
		for i := range out {
			out[i] = i
		}
		return out
	}
	var result []int
	t.diffNode(other, 0, &result)
	return result
}

// diffNode recurses through the tree comparing nodes.
func (t *MerkleTree) diffNode(other *MerkleTree, nodeIdx int, result *[]int) {
	// Same hash: entire subtree is identical.
	if string(t.nodes[nodeIdx]) == string(other.nodes[nodeIdx]) {
		return
	}

	leafOffset := t.capacity - 1
	if nodeIdx >= leafOffset {
		// This is a leaf node.
		leafIdx := nodeIdx - leafOffset
		if leafIdx < t.leafCount {
			*result = append(*result, leafIdx)
		}
		return
	}

	// Internal node: recurse into both children.
	t.diffNode(other, 2*nodeIdx+1, result)
	t.diffNode(other, 2*nodeIdx+2, result)
}

// Update replaces the data block at index with newData and recomputes
// all ancestor hashes from the leaf to the root in O(log N).
func (t *MerkleTree) Update(index int, newData []byte) {
	if index < 0 || index >= t.leafCount {
		return
	}
	t.leaves[index] = newData

	// Update the leaf hash.
	leafOffset := t.capacity - 1
	nodeIdx := leafOffset + index
	t.nodes[nodeIdx] = hashData(newData)

	// Walk up to the root, recomputing each parent.
	for nodeIdx > 0 {
		parent := (nodeIdx - 1) / 2
		t.nodes[parent] = hashPair(t.nodes[2*parent+1], t.nodes[2*parent+2])
		nodeIdx = parent
	}
}

// ProofOfInclusion returns the sibling hashes along the path from the leaf
// at index to the root. The proof has length equal to the tree height.
func (t *MerkleTree) ProofOfInclusion(index int) [][]byte {
	if index < 0 || index >= t.leafCount {
		return nil
	}

	leafOffset := t.capacity - 1
	nodeIdx := leafOffset + index
	proof := make([][]byte, 0, 8) // tree height <= log2(capacity)

	for nodeIdx > 0 {
		var sibling int
		if nodeIdx%2 == 0 {
			sibling = nodeIdx - 1 // right node: sibling is to the left
		} else {
			sibling = nodeIdx + 1 // left node: sibling is to the right
		}
		h := make([]byte, len(t.nodes[sibling]))
		copy(h, t.nodes[sibling])
		proof = append(proof, h)
		nodeIdx = (nodeIdx - 1) / 2
	}

	return proof
}

// VerifyProof verifies that data at leafIndex is a member of a tree with the
// given rootHash, using the provided proof (sibling hashes from leaf to root).
func VerifyProof(data []byte, leafIndex int, proof [][]byte, rootHash []byte) bool {
	current := hashData(data)

	// Reconstruct the root hash using the proof hashes.
	// At each level we must know whether the current node is a left or right child.
	// We track this with the same index arithmetic used during tree construction.
	// Start at the conceptual leaf node index and work up.
	//
	// capacity must be inferred from proof length: capacity = 2^len(proof).
	capacity := 1
	for range proof {
		capacity <<= 1
	}
	leafOffset := capacity - 1
	nodeIdx := leafOffset + leafIndex

	for _, sibling := range proof {
		if nodeIdx%2 == 0 {
			// Current node is a right child: sibling is to the left.
			current = hashPair(sibling, current)
		} else {
			// Current node is a left child: sibling is to the right.
			current = hashPair(current, sibling)
		}
		nodeIdx = (nodeIdx - 1) / 2
	}

	return string(current) == string(rootHash)
}
```

The flat-array representation keeps the index arithmetic simple: the root is always at index 0 regardless of tree size, and the leaf-to-root path is a sequence of `(i-1)/2` parent computations.

### Exercise 2: Test the Tree

Create `merkle_test.go`:

```go
// merkle_test.go
package merkle

import (
	"bytes"
	"fmt"
	"testing"
)

func blocks(ss ...string) [][]byte {
	out := make([][]byte, len(ss))
	for i, s := range ss {
		out[i] = []byte(s)
	}
	return out
}

func TestRootHashDeterministic(t *testing.T) {
	t.Parallel()

	b := blocks("alpha", "beta", "gamma", "delta")
	t1 := New(b)
	t2 := New(b)
	if !bytes.Equal(t1.RootHash(), t2.RootHash()) {
		t.Fatal("identical datasets must produce identical root hashes")
	}
}

func TestRootHashDifferentData(t *testing.T) {
	t.Parallel()

	t1 := New(blocks("a", "b"))
	t2 := New(blocks("a", "X"))
	if bytes.Equal(t1.RootHash(), t2.RootHash()) {
		t.Fatal("different datasets must produce different root hashes")
	}
}

func TestRootHashSingleBlock(t *testing.T) {
	t.Parallel()

	// A single-block tree has root == leaf hash.
	t1 := New(blocks("only"))
	if t1.RootHash() == nil {
		t.Fatal("RootHash must not be nil")
	}
}

func TestDiffNoDifferences(t *testing.T) {
	t.Parallel()

	b := blocks("a", "b", "c", "d")
	t1 := New(b)
	t2 := New(b)
	if diff := t1.Diff(t2); len(diff) != 0 {
		t.Fatalf("identical trees: Diff = %v, want []", diff)
	}
}

func TestDiffOneDifference(t *testing.T) {
	t.Parallel()

	t1 := New(blocks("a", "b", "c", "d"))
	t2 := New(blocks("a", "b", "Z", "d"))
	diff := t1.Diff(t2)
	if len(diff) != 1 || diff[0] != 2 {
		t.Fatalf("Diff = %v, want [2]", diff)
	}
}

func TestDiffMultipleDifferences(t *testing.T) {
	t.Parallel()

	t1 := New(blocks("a", "b", "c", "d", "e", "f", "g", "h"))
	t2 := New(blocks("a", "B", "c", "D", "e", "f", "g", "h"))
	diff := t1.Diff(t2)
	if len(diff) != 2 {
		t.Fatalf("Diff = %v, want 2 differences", diff)
	}
	found := map[int]bool{}
	for _, i := range diff {
		found[i] = true
	}
	if !found[1] || !found[3] {
		t.Fatalf("Diff = %v, want indices 1 and 3", diff)
	}
}

func TestDiffNonPowerOfTwoSize(t *testing.T) {
	t.Parallel()

	// 5 blocks: not a power of two; padding must not create false positives.
	b := blocks("a", "b", "c", "d", "e")
	t1 := New(b)
	t2 := New(b)
	if diff := t1.Diff(t2); len(diff) != 0 {
		t.Fatalf("Diff = %v on identical 5-block trees, want []", diff)
	}

	t3 := New(blocks("a", "b", "c", "d", "E"))
	diff := t1.Diff(t3)
	if len(diff) != 1 || diff[0] != 4 {
		t.Fatalf("Diff = %v, want [4]", diff)
	}
}

func TestUpdate(t *testing.T) {
	t.Parallel()

	b := blocks("a", "b", "c", "d")
	tree := New(b)
	original := make([]byte, len(tree.RootHash()))
	copy(original, tree.RootHash())

	tree.Update(2, []byte("Z"))
	after := tree.RootHash()
	if bytes.Equal(original, after) {
		t.Fatal("Update must change the root hash")
	}

	// After update the tree must agree with a freshly built tree.
	fresh := New(blocks("a", "b", "Z", "d"))
	if !bytes.Equal(fresh.RootHash(), after) {
		t.Fatalf("Update result %x != fresh build %x", after, fresh.RootHash())
	}
}

func TestUpdateIdempotent(t *testing.T) {
	t.Parallel()

	b := blocks("a", "b", "c", "d")
	tree := New(b)
	original := make([]byte, len(tree.RootHash()))
	copy(original, tree.RootHash())

	// Updating with the same data must leave the root hash unchanged.
	tree.Update(1, []byte("b"))
	if !bytes.Equal(original, tree.RootHash()) {
		t.Fatal("Update with same data must not change the root hash")
	}
}

func TestProofOfInclusionValid(t *testing.T) {
	t.Parallel()

	b := blocks("a", "b", "c", "d")
	tree := New(b)
	root := tree.RootHash()

	for i, block := range b {
		proof := tree.ProofOfInclusion(i)
		if !VerifyProof(block, i, proof, root) {
			t.Errorf("VerifyProof failed for index %d", i)
		}
	}
}

func TestProofOfInclusionWrongData(t *testing.T) {
	t.Parallel()

	b := blocks("a", "b", "c", "d")
	tree := New(b)
	proof := tree.ProofOfInclusion(0)
	if VerifyProof([]byte("tampered"), 0, proof, tree.RootHash()) {
		t.Fatal("VerifyProof must return false for tampered data")
	}
}

func TestProofOfInclusionWrongIndex(t *testing.T) {
	t.Parallel()

	b := blocks("a", "b", "c", "d")
	tree := New(b)
	// Proof for index 0 must not verify for the data at index 1.
	proof := tree.ProofOfInclusion(0)
	if VerifyProof([]byte("b"), 1, proof, tree.RootHash()) {
		t.Fatal("VerifyProof must return false for wrong index/proof pair")
	}
}

func TestProofAfterUpdate(t *testing.T) {
	t.Parallel()

	b := blocks("a", "b", "c", "d")
	tree := New(b)
	tree.Update(3, []byte("D"))
	root := tree.RootHash()

	proof := tree.ProofOfInclusion(3)
	if !VerifyProof([]byte("D"), 3, proof, root) {
		t.Fatal("proof must be valid after Update")
	}
}

func TestProofOutOfRange(t *testing.T) {
	t.Parallel()

	tree := New(blocks("a", "b"))
	if tree.ProofOfInclusion(-1) != nil {
		t.Fatal("out-of-range index must return nil proof")
	}
	if tree.ProofOfInclusion(2) != nil {
		t.Fatal("out-of-range index must return nil proof")
	}
}

func ExampleNew() {
	t1 := New([][]byte{[]byte("x"), []byte("y")})
	t2 := New([][]byte{[]byte("x"), []byte("y")})
	// Identical datasets produce identical root hashes.
	if string(t1.RootHash()) == string(t2.RootHash()) {
		fmt.Println("hashes match")
	}
	// Output:
	// hashes match
}
```

The tests cover: construction, root hash determinism, difference detection with zero/one/many diffs, non-power-of-two leaf counts, incremental updates, idempotent updates, proof generation and verification (valid, tampered data, wrong index), and proof validity after an update.

### Exercise 3: Command-Line Demo

Create `cmd/merkle/main.go`:

```go
// cmd/merkle/main.go
package main

import (
	"fmt"
	"os"

	"example.com/merkle"
)

func main() {
	// Build two trees; one block differs.
	replica1 := merkle.New([][]byte{
		[]byte("block-0"),
		[]byte("block-1"),
		[]byte("block-2"),
		[]byte("block-3"),
		[]byte("block-4"),
		[]byte("block-5"),
		[]byte("block-6"),
		[]byte("block-7"),
	})

	replica2 := merkle.New([][]byte{
		[]byte("block-0"),
		[]byte("block-1"),
		[]byte("CORRUPTED"), // block 2 differs
		[]byte("block-3"),
		[]byte("block-4"),
		[]byte("block-5"),
		[]byte("block-6"),
		[]byte("block-7"),
	})

	fmt.Fprintf(os.Stdout, "replica1 root: %s\n", replica1.RootHashHex()[:16]+"...")
	fmt.Fprintf(os.Stdout, "replica2 root: %s\n", replica2.RootHashHex()[:16]+"...")

	diff := replica1.Diff(replica2)
	fmt.Fprintf(os.Stdout, "differing block indices: %v\n", diff)

	// Proof of inclusion for block 2 in replica1.
	proof := replica1.ProofOfInclusion(2)
	ok := merkle.VerifyProof([]byte("block-2"), 2, proof, replica1.RootHash())
	fmt.Fprintf(os.Stdout, "proof for block 2 in replica1 verified: %v\n", ok)

	// Repair replica2: update block 2 to match replica1.
	replica2.Update(2, []byte("block-2"))
	fmt.Fprintf(os.Stdout, "after repair, roots match: %v\n",
		replica1.RootHashHex() == replica2.RootHashHex())
}
```

Run the demo with `go run ./cmd/merkle` from the module root.

Your turn: add a benchmark in `merkle_test.go` named `BenchmarkDiff` that constructs two 1024-block trees that differ in one block and measures how many nanoseconds `Diff` takes. Compare that to a naive loop over all blocks.

## Common Mistakes

### Forgetting to Copy Sibling Hashes in the Proof

Wrong: `proof = append(proof, t.nodes[sibling])` — this appends a slice header pointing into the tree's internal storage. A subsequent `Update` call overwrites the backing array and silently corrupts the already-returned proof.

Fix: copy the bytes before appending, as `ProofOfInclusion` does: `h := make([]byte, ...); copy(h, t.nodes[sibling]); proof = append(proof, h)`.

### Off-by-one in Left vs. Right Child Detection

Wrong: checking `nodeIdx%2 == 1` to detect a left child then taking the wrong sibling. In the heap layout, odd indices are left children (parent is at `(i-1)/2`), even indices are right children. The proof verification must use the same parity rule as proof generation; a mismatch silently inverts the hash concatenation order and produces a wrong root every time.

Fix: use the same parity check in both `ProofOfInclusion` and `VerifyProof`: `nodeIdx%2 == 0` means the current node is a right child.

### Using String Comparison Instead of bytes.Equal for Hash Equality

Wrong: `t.nodes[i] == other.nodes[i]` — slices are not comparable with `==` in Go and this will not compile. Using `fmt.Sprintf("%x", ...)` equality is correct but slow.

Fix: use `bytes.Equal(t.nodes[i], other.nodes[i])` or compare with `string(a) == string(b)`. The Go compiler special-cases `[]byte`-to-`string` conversion inside a comparison expression and eliminates the allocation entirely (zero bytes allocated, verified by benchmark), so the string form is both correct and allocation-free. The lesson's `diffNode` uses the string form; `bytes.Equal` is an equally valid stylistic choice. Either form is acceptable in production.

### Concatenate-then-hash vs. Hash-then-concatenate

Wrong: `H(H(left) + H(right))` where `+` is string concatenation of hex strings rather than byte concatenation of raw hashes. Hex encoding doubles the length and changes the input to the hash function, producing a different tree than every other Merkle implementation.

Fix: concatenate the raw hash bytes, as `hashPair` does. Keep hashes as `[]byte` throughout; only convert to hex for display.

## Verification

From `~/go-exercises/merkle`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. The `ExampleNew` function is verified by `go test` — the `// Output:` comment is the ground truth.

## Summary

- Merkle trees store one hash per node; leaf hashes cover data blocks, internal node hashes cover their subtree.
- Root hash comparison detects any difference in O(1); difference detection locates differing blocks in O(log N + D) best case (clustered differences) or O(D * log N) worst case (spread differences).
- Flat-array storage with heap indexing keeps the parent/child arithmetic to one line.
- Incremental updates recompute O(log N) hashes walking from the modified leaf to the root.
- Proof of inclusion is O(log N) sibling hashes; verification reconstructs the root without the full tree.
- Anti-entropy protocols use the root hash as a cheap "are we in sync?" check before any data transfer.

## What's Next

Next: [Service Discovery](../11-service-discovery/11-service-discovery.md).

## Resources

- [Wikipedia: Merkle tree](https://en.wikipedia.org/wiki/Merkle_tree) -- complete algorithm and properties
- [Amazon Dynamo paper, Section 4.7](https://www.allthingsdistributed.com/files/amazon-dynamo-sosp2007.pdf) -- anti-entropy with Merkle trees in production
- [Certificate Transparency: How CT Works](https://certificate.transparency.dev/howctworks/) -- proof-of-inclusion in a live system
- [crypto/sha256 package](https://pkg.go.dev/crypto/sha256) -- the hash function used in this lesson
