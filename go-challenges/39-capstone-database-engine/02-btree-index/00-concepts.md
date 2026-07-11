# 2. B+Tree Index — Concepts

A B+Tree is the dominant on-disk index structure in relational databases because it delivers O(log_B n) lookups, sorted range scans through linked leaf pages, and a branching factor large enough to keep tree height at three or four levels even for billion-row tables. This lesson builds a complete, page-backed B+Tree in Go, one feature at a time: fixed 4096-byte pages with binary encoding, leaf splits that copy a separator key up, internal splits that push a separator key up, point lookups, range iteration through linked leaves, deletion with and without rebalancing, bottom-up bulk loading, and a seekable cursor. This file is the conceptual foundation: read it once and you will have the theory needed to reason through every exercise, each of which is its own independent, self-contained Go module.

## Concepts

### Why B+Tree and Not a Binary Search Tree

A binary search tree on disk is catastrophically slow: every pointer follow is a random page read and the tree height is O(log_2 n). With n = 10^9 rows that is roughly 30 disk reads per lookup. A B+Tree stores hundreds of keys per node — the branching factor B — so height is O(log_B n). With B = 200 and n = 10^9, height is 4: only four page reads per lookup, regardless of dataset size.

The critical design decision is that leaf nodes store all key-value pairs and are linked in a sorted singly-linked list, while internal nodes store only separator keys and child pointers and contain no values at all. Two consequences follow directly:

- Range scans follow leaf sibling pointers without touching internal nodes after the initial descent.
- Leaf splits copy the separator key to the parent (the key stays in the leaf so a search can still find it). Internal splits push the separator key up (the key does not appear in either child, because an internal node is a pure router).

### Page Layout

Every node lives in exactly one 4096-byte page. The layout is binary and fixed-size on the wire, so the tree can be re-opened after a crash without parsing text. This lesson uses big-endian integers throughout.

A leaf page is a 19-byte header followed by variable-length entries:

```text
[0]      nodeType  uint8   (kindLeaf = 1)
[1:9]    parentID  uint64  (0 = root, no parent)
[9:11]   keyCount  uint16
[11:19]  nextLeaf  uint64  (0 = last leaf)
[19:]    entries:  [keyLen uint8][key bytes][value uint64] ...
```

An internal page is an 11-byte header, then the first child pointer, then alternating keys and child pointers:

```text
[0]      nodeType  uint8   (kindInternal = 0)
[1:9]    parentID  uint64
[9:11]   keyCount  uint16
[11:19]  child[0]  uint64
[19:]    [keyLen uint8][key bytes][child uint64] * keyCount
```

An internal node with k keys has k+1 children. The routing invariant is that every key in the subtree rooted at child[i] satisfies keys[i-1] <= key < keys[i], using -infinity and +infinity as the sentinels at the two ends. A leaf entry's value is a `RecordID`, a uint64 that packs a (pageID, slotIndex) pair: the upper 48 bits are the heap page that holds the row and the lower 16 bits are the slot within that page. Packing the pair into one integer keeps each leaf entry to a fixed 8-byte value field.

### Splits and the Root

When a leaf overflows — more than the leaf capacity after an insert — it splits:

1. Allocate a new right sibling.
2. Move the upper half of the entries to the right sibling.
3. Copy the right sibling's smallest key into the parent as the separator (copy-up).
4. Relink the leaf chain: the left leaf's `nextLeaf` points at the new right sibling, and the right sibling inherits the left leaf's old `nextLeaf`.

When an internal node overflows it splits differently:

1. Allocate a new right sibling.
2. Move the upper half of the keys and their corresponding children to the right.
3. Push the middle key up to the parent and remove it from both children (push-up).
4. If the parent now overflows, repeat upward.

If the root itself splits, allocate a brand-new root with two children; this is the only event that increases tree height. Height decreases only during a delete-driven merge.

### Capacity and Branching Factor

With a 255-byte maximum key, each leaf entry costs at most 264 bytes (1 length byte + 255 key bytes + 8 value bytes). The available leaf payload is 4096 - 19 = 4077 bytes, giving a worst-case minimum leaf capacity of 15 entries. With typical 8-byte keys the real capacity is far larger — about floor(4077/17) ≈ 239 entries. The branching factor of an internal node follows the same arithmetic. This lesson computes capacity from the conservative maximum entry size, so `leafCap` and `innerCap` are 15; this keeps the splitting and merging logic exercised by tests of only a few hundred keys rather than tens of thousands.

### Lexicographic Key Ordering

All key comparison uses `bytes.Compare`, which gives lexicographic ordering of arbitrary byte slices. The practical consequence is that numeric keys must be serialized in a form whose byte order matches their numeric order: big-endian fixed-width integers, or zero-padded decimal strings. The decimal string `"10"` sorts before `"9"` lexicographically, so an unpadded `strconv.Itoa` is a latent ordering bug.

### Fanout, Tree Height, and the Page/Cache Hierarchy

Fanout — the branching factor f — is the number of children an internal node holds, bounded by how many separator keys plus child pointers fit in one page. A tree storing n keys in its leaves has height h = ceil(log_f n), so height shrinks as 1/log f. Concretely, with n = 10^9: f = 200 gives log_200(10^9) ≈ 3.91, rounding to 4 levels; doubling to f = 400 gives ≈ 3.46, still 4 levels. The win from a larger page is real but sublinear, because what dominates a disk lookup is the count of random page reads — one per level — and height moves slowly.

This sets up the central trade-off in choosing a page size. A larger page raises f (fewer levels) and amortizes the fixed cost of a seek over more useful bytes, but it wastes bandwidth and cache when a query needs only one key from the page, and it raises write amplification because a single-key update must read-modify-write the whole page. A smaller page lowers write amplification and cache pollution but raises height and the random-read count. Disk-resident B+Trees therefore size the page to the storage device's natural transfer unit, commonly 4-16 KiB, which also aligns with the OS page cache and SSD program/erase granularity — this is why this lesson uses 4096 bytes. Cache-conscious in-memory B+Trees make the opposite choice and size a node to a small multiple of the 64-byte CPU cache line, because there the bottleneck is cache misses during the in-node search, not disk seeks. Disk-optimized fanout and cache-optimized fanout are different optimization targets for the same structure.

### B+Tree vs B-Tree vs LSM-Tree

These three structures occupy different points on the read/write/space trade-off surface:

- A classic B-Tree stores values in every node, internal ones included. A key found high in the tree needs fewer hops, but values consume space in internal nodes, which lowers fanout and raises height. Range scans are awkward because there is no leaf-link list, so a scan must walk back up and down the tree.
- A B+Tree stores values only in leaves; internal nodes are pure routers. This maximizes fanout — more separators per page means a shorter tree — and gives the linked-leaf list that makes a range scan one pointer-follow per step. The cost is that each leaf separator is duplicated upward by copy-up. Nearly every relational engine uses a B+Tree for its primary index for exactly these reasons.
- An LSM-Tree buffers writes in an in-memory table, flushes immutable sorted runs (SSTables), and merges them with background compaction. Writes are sequential and fast; the costs are read amplification — a key may live in several runs, mitigated by Bloom filters and per-run indexes — and space and write amplification from compaction.

The underlying tension is the RUM conjecture: you cannot simultaneously minimize Read overhead, Update overhead, and Memory (space) overhead — improving one usually worsens another. A B+Tree optimizes reads at the cost of random writes and the write amplification of page splits; an LSM optimizes writes at the cost of read and space amplification. Choosing between them is a workload decision, not a correctness one.

### The Half-Full Invariant and Why Deletes Are Harder Than Inserts

Every node except the root must be at least half full — at least ceil(cap/2) entries. This does two things: it caps wasted space at roughly 50%, and, more importantly, it bounds height, because half-full nodes guarantee fanout >= cap/2, keeping height O(log_{cap/2} n). Without the invariant a sequence of deletes could leave a chain of one-key nodes and degrade the tree toward a linked list.

Inserts only ever make a node fuller. The single failure mode is overflow, repaired locally by one split whose effect propagates strictly upward and monotonically: a split can cause one more split, never a different kind of repair. Deletes are fundamentally harder. A delete can cause underflow, dropping a node below the minimum, and the repair is not local — it must inspect an adjacent sibling. There are two repair operations, redistribute or merge, and the choice depends on the sibling's occupancy. A merge removes a separator from the parent, which can itself underflow, cascading toward the root and possibly shrinking the tree's height. So delete has strictly more states than insert: a sibling-dependent branch, two repair shapes, and an upward cascade that can change height. This is why many production engines defer or skip merging entirely, tolerating underfull nodes and reclaiming space lazily or during a periodic rebuild — strict rebalancing under delete/insert oscillation at a node boundary can thrash. SQLite, for instance, does not rebalance aggressively on delete; it leaves free space on the page for reuse.

### Redistribution vs Merge Policy

When a node underflows there are two ways to restore the invariant. Redistribution, also called borrowing, moves one entry from an adjacent sibling that has more than the minimum, rotating it through the parent's separator key. It touches exactly three nodes — the node, one sibling, and the parent — and can never cascade, because the parent's key count does not change. A merge instead combines the underfull node with a sibling and pulls the parent's separator down between them, which deletes one entry from the parent. A merge always fits, because two minimum-sized nodes plus one separator never exceed capacity (min + min <= cap), but it shrinks the parent and may cascade.

The standard policy is redistribute-then-merge: borrow when a sibling can spare an entry, and fall back to merge only when both siblings sit at the minimum. The leaf/internal asymmetry from splits reappears here. At a leaf, redistribution simply copies the new boundary key into the parent separator. At an internal node, redistribution rotates: the parent separator descends into the underfull node and a sibling key ascends to replace it, because an internal separator is a router, not a stored value. "Always merge" is simpler to code but churns structure; "always redistribute" is impossible when every sibling is minimal — hence the hybrid.

### Prefix Compression and Suffix Truncation of Separator Keys

A separator key in an internal node only has to be discriminative: any byte string that routes searches correctly between two subtrees will do, and it need not equal any stored key. Two techniques exploit this. Suffix truncation, when a split copies a key up, stores only the shortest prefix that still separates the largest key of the left subtree from the smallest key of the right; separating `"internationalization"` from `"internet"` needs only `"interne"`, and shorter separators mean more of them per page, higher fanout, and lower height. Prefix compression observes that keys co-located in one node share a long common prefix because they are sorted and adjacent, so it stores the common prefix once per page and only the differing suffix per entry, reconstructing full keys on read. Both trade a little CPU for fanout and both pay off most for long string keys; for fixed 8-byte integer keys the gain is negligible. This lesson stores full keys for clarity, but production engines apply these aggressively — PostgreSQL's nbtree does both suffix truncation and deduplication.

### Concurrency: Latch Crabbing (Conceptual)

A single global lock over the whole tree serializes every operation and destroys throughput, so real engines latch at the page level. A latch is a short-duration physical lock protecting a page's bytes during a single operation; it is distinct from a transactional lock, which protects a logical tuple for the duration of a transaction. The protocol for descending the tree safely is latch crabbing, or coupling: acquire the child's latch before releasing the parent's — like a crab moving one claw at a time — so a descending operation never loses its place if another thread is restructuring the level below. For a read, take latches in shared mode and release the parent as soon as the child is latched. For an insert or delete, take latches in exclusive mode and release ancestors only once the child is known safe — a node that cannot split on insert because it has free space, or cannot underflow on delete because it is above the minimum. Only a safe child guarantees the structural change will not propagate to that ancestor, so the ancestor latches can be dropped; if the child is unsafe, the held ancestor latches are retained. Because most nodes are safe most of the time, optimistic crabbing descends with shared latches assuming safety and restarts with exclusive latches only on the rare unsafe path. All of this is conceptual for this lesson — the `Tree` built here is documented as not safe for concurrent use — but it is exactly how a production B+Tree couples with a buffer pool's page latches.

## Common Mistakes

### Treating Leaf Split and Internal Split as Symmetric

The most common B+Tree bug is copying the middle key into the parent for both leaf and internal splits. For a leaf split the separator must remain in the right leaf so a later search can still find it; for an internal split the separator key is pushed up and deleted from both halves, because keeping it would duplicate the key and corrupt routing. The rule is leaf split = copy-up (the right leaf retains the separator), internal split = push-up (the separator is removed from both halves and given only to the parent).

### Off-by-One in the Child Pointer After an Insert

When inserting a separator key at position `pos` in an internal node, the new right child belongs at `children[pos+1]`, not `children[pos]` — `children[pos]` is the existing left child that was already there. Writing the right child to `children[pos]` overwrites the left subtree's pointer and orphans an entire subtree. The shift is `copy(parent.children[pos+2:], parent.children[pos+1:])` followed by `parent.children[pos+1] = rightID`.

### Using == to Compare Keys

In Go, byte slices are not comparable with `==`; the expression does not even compile for `[]byte`. Key equality is `bytes.Equal(a, b)` and key ordering is `bytes.Compare(a, b)`. Reaching for `==` out of habit is a frequent first error when the keys are strings that have already been converted to `[]byte`.

### Storing Numeric Keys as Decimal Strings Without Padding

Inserting integers as `strconv.Itoa(n)` produces lexicographic order, in which `"10"` sorts before `"9"`. Pad to a fixed width with `fmt.Sprintf("%08d", n)`, or encode as big-endian uint64 bytes; both preserve numeric sort order under `bytes.Compare`. This bug is invisible until the dataset crosses a digit-count boundary, which is exactly when it silently breaks range scans.

### Ignoring Returned Errors

Every tree method that touches the store can fail, and `Search` returns three values whose `ok` and `error` are distinct signals. Discarding the error from `Insert`, or checking only `ok` from `Search` while dropping the error, hides I/O failures and corrupt pages until they surface as a wrong answer far from their cause. Handle every returned error, and compare sentinel errors with `errors.Is`.

---

Next: [01-page-encoding-and-store.md](01-page-encoding-and-store.md)
