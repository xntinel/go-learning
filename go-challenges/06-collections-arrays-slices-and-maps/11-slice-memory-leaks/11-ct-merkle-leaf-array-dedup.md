# Exercise 11: A Certificate Transparency Dedup Index That Cannot Pin a Certificate

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A Certificate Transparency log (RFC 6962) -- the append-only, publicly
auditable ledger behind every browser's CT enforcement, run in production by
Google, Let's Encrypt, Sigstore's Rekor, and others -- rejects a resubmission
of a certificate it has already sequenced. The check is a dedup index keyed
by the certificate's Merkle tree *leaf hash*: `SHA-256(0x00 || entry)`, a
fixed 32-byte digest. At the scale a real CT log runs at, this index tracks
hundreds of millions of entries, and every design decision about what it
stores per entry gets multiplied by that count. Store one unnecessary
kilobyte per entry and the index costs hundreds of gigabytes it did not need
to.

The design temptation is the same one that shows up throughout this lesson:
"keep the raw certificate around too, it might be useful for a debug dump."
A `map[string][]byte` or `map[[32]byte]RawEntry` that retains the full
submitted DER bytes alongside the hash answers a question the dedup index was
never asked -- "has this been seen?" needs only the hash and a tiny sequence
number, nothing else. This module's twist on that lesson is the key type
itself: because a SHA-256 digest is *always* exactly 32 bytes, the natural
key is `[32]byte`, not `[]byte`. That single choice removes an entire class
of the aliasing bugs this lesson otherwise spends its time on -- there is no
backing array a caller could still hold a mutable view into, because
comparing or assigning a `[32]byte` always copies all 32 bytes by value. The
only way this index can leak is if something *other* than the hash gets
stored alongside it, and that is exactly what the tests measure.

This module builds `Index`, the dedup structure itself: a bounded map keyed
by `[32]byte` leaf hash, storing only a tiny sequence number per entry, never
the submitted certificate bytes. The raw-bytes-retained design lives only in
the test file, as the antipattern the tests measure and reject.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
ctleaf/                   module example.com/ctleaf
  go.mod                  go 1.24
  ctleaf.go               LeafHash, Index; NewIndex, Submit, Len; two sentinel errors
  ctleaf_test.go          hash conformance, duplicate detection, capacity table, the
                          raw-bytes-retained contrast via MemStats, concurrency, ExampleIndex_Submit
```

- Files: `ctleaf.go`, `ctleaf_test.go`.
- Implement: `LeafHash(entry []byte) [sha256.Size]byte` computing the RFC 6962 leaf hash `HASH(0x00 || entry)`; `NewIndex(capacity int) (*Index, error)` rejecting a non-positive capacity with `ErrInvalidCapacity`; `(*Index).Submit(entry []byte, sequence int64) (leafHash [sha256.Size]byte, duplicate bool, firstSeen int64, err error)` recording a new entry or reporting a duplicate, returning `ErrIndexFull` once `capacity` distinct entries are tracked; `(*Index).Len() int`.
- Test: leaf-hash conformance against the RFC 6962 prefix, including an empty entry; duplicate detection and the original sequence number it reports; distinct entries both recorded; rejection beyond capacity, with a duplicate of an already-recorded entry still succeeding at capacity; the raw-bytes-retained contrast -- a naive index that keeps the submitted bytes per entry costs memory proportional to entry size times count, `Index` does not; safe concurrent `Submit`; and `ExampleIndex_Submit` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### A fixed-size digest as a map key needs no copy discipline at all

Every other module in this lesson that keys or caches by a derived value
has to worry about *how* that value was derived: was it a sub-slice of a
larger buffer (in which case it pins that buffer), or a fresh copy (in which
case it does not)? A cryptographic digest sidesteps the question entirely,
because its size is fixed and known at compile time. `sha256.Sum256` in the
standard library already returns `[32]byte`, not `[]byte`, for exactly this
reason; the same Go 1.20 slice-to-array conversion this lesson's arrays
module uses for an HMAC digest applies here to build `LeafHash` on top of
the streaming `hash.Hash` API:

```go
func LeafHash(entry []byte) [sha256.Size]byte {
    h := sha256.New()
    h.Write([]byte{0x00})
    h.Write(entry)
    return [sha256.Size]byte(h.Sum(nil))   // always exactly 32 bytes
}
```

`h.Sum(nil)` returns a `[]byte`, but it is *always* 32 bytes for SHA-256, so
the conversion to `[sha256.Size]byte` can never panic -- it turns "always 32
bytes" from a fact about the algorithm into a fact the type system carries
forward. Once the hash is an array, using it as a map key needs no Clone, no
Clip, no aliasing contract at all: `map[[32]byte]int64{leafHash: sequence}`
copies the full 32 bytes on every insert and lookup, by ordinary Go value
semantics, whether `entry` was four bytes or four megabytes.

That guarantee does not extend to anything else you might be tempted to
store next to the hash:

```go
// The trap: the hash is safe, but raw keeps entry itself reachable.
type leakyRecord struct {
    seq int64
    raw []byte   // the full submitted certificate -- kept "just in case"
}
```

`raw` is not a hash; it is a slice header over the caller's certificate
bytes (or a copy of them), and either way it pins kilobytes per record that
the dedup check never reads again. At CT-log scale that is the difference
between an index sized in megabytes and one sized in gigabytes, for
information nothing queries.

Create `ctleaf.go`:

```go
// Package ctleaf deduplicates certificate submissions by their Certificate
// Transparency (RFC 6962) Merkle tree leaf hash, the check a CT log runs
// before sequencing a submitted certificate: has this exact entry already
// been logged?
//
// The leaf hash is a fixed 32-byte SHA-256 digest, and Index stores it as a
// [32]byte array key rather than a []byte. That choice is the point of the
// module: an array is a value, so a map keyed on it needs no defensive-copy
// discipline at all -- there is no backing array a caller could still hold
// a mutable alias to, because assigning or comparing a [32]byte always
// copies the full 32 bytes. The tests contrast this with the more tempting
// design of also keeping the raw submitted certificate bytes around.
package ctleaf

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
)

var (
	// ErrInvalidCapacity means NewIndex was called with a non-positive
	// capacity.
	ErrInvalidCapacity = errors.New("ctleaf: capacity must be positive")
	// ErrIndexFull means Submit was called with a new leaf hash and the
	// index is already tracking capacity distinct entries.
	ErrIndexFull = errors.New("ctleaf: index is full")
)

// LeafHash computes the RFC 6962 section 2.1 Merkle tree leaf hash of entry:
// HASH(0x00 || entry). The 0x00 prefix is the "leaf" domain separator that
// keeps a leaf hash from ever colliding with an interior Merkle node hash,
// which RFC 6962 computes with a 0x01 prefix instead.
func LeafHash(entry []byte) [sha256.Size]byte {
	h := sha256.New()
	h.Write([]byte{0x00})
	h.Write(entry)
	return [sha256.Size]byte(h.Sum(nil))
}

// Index deduplicates certificate submissions by leaf hash, recording the
// sequence number of the first submission of each distinct entry.
//
// Index is safe for concurrent use by multiple goroutines.
type Index struct {
	mu       sync.Mutex
	seen     map[[sha256.Size]byte]int64
	capacity int
}

// NewIndex returns an Index that tracks at most capacity distinct leaf
// hashes. It returns ErrInvalidCapacity if capacity is not positive.
func NewIndex(capacity int) (*Index, error) {
	if capacity <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidCapacity, capacity)
	}
	return &Index{seen: make(map[[sha256.Size]byte]int64, capacity), capacity: capacity}, nil
}

// Submit computes entry's leaf hash and records it against sequence if this
// is the first time that hash has been seen. It reports the leaf hash,
// whether the entry is a duplicate, and -- for a duplicate -- the sequence
// number of the original submission.
//
// entry is read but never retained: Submit stores only the 32-byte leaf
// hash (a value, not a view into entry) and the caller-supplied sequence
// number. An Index therefore never pins the certificates it deduplicates,
// no matter how many megabytes entry itself is.
//
// Submit returns ErrIndexFull if entry's hash is new and the index already
// tracks capacity distinct entries; the hash and duplicate status are still
// valid to inspect, but the entry was not recorded.
func (x *Index) Submit(entry []byte, sequence int64) (leafHash [sha256.Size]byte, duplicate bool, firstSeen int64, err error) {
	leafHash = LeafHash(entry)

	x.mu.Lock()
	defer x.mu.Unlock()
	if first, ok := x.seen[leafHash]; ok {
		return leafHash, true, first, nil
	}
	if len(x.seen) >= x.capacity {
		return leafHash, false, 0, fmt.Errorf("%w: capacity %d", ErrIndexFull, x.capacity)
	}
	x.seen[leafHash] = sequence
	return leafHash, false, 0, nil
}

// Len reports how many distinct leaf hashes are currently recorded.
func (x *Index) Len() int {
	x.mu.Lock()
	defer x.mu.Unlock()
	return len(x.seen)
}
```

### Using it

Construct one `Index` per log shard with a capacity sized to how many
distinct certificates that shard is expected to hold before rotation or
compaction, then call `Submit` for every incoming certificate before
sequencing it. `duplicate == true` is not an error -- resubmission of an
already-logged certificate is routine (a CA or monitor retries, a client
misbehaves) -- so it is reported as a boolean, not a sentinel error;
`ErrIndexFull` is reserved for the one condition that actually is
exceptional, a genuinely new certificate arriving with no room left to
record it.

`entry` itself never crosses into long-term storage: `Submit` reads it once,
computes a 32-byte value from it, and forgets it. That is the whole aliasing
contract, and it holds regardless of whether `entry` is a hundred-byte
precert or an eight-kilobyte chain with intermediates.

`ExampleIndex_Submit` is the runnable demonstration of this module: `go
test` runs it and compares its standard output against the `// Output:`
comment, so the usage shown below cannot drift from the code.

```go
func ExampleIndex_Submit() {
	idx, err := NewIndex(100)
	if err != nil {
		panic(err)
	}

	certA := []byte("-----BEGIN CERTIFICATE-----\nMIIB...A\n-----END CERTIFICATE-----")
	certB := []byte("-----BEGIN CERTIFICATE-----\nMIIB...B\n-----END CERTIFICATE-----")

	h1, dup1, _, err := idx.Submit(certA, 1001)
	if err != nil {
		panic(err)
	}
	fmt.Printf("first submission:  duplicate=%v leaf=%x\n", dup1, h1[:4])

	_, dup2, first, err := idx.Submit(certA, 1002)
	if err != nil {
		panic(err)
	}
	fmt.Printf("resubmission:      duplicate=%v firstSeen=%d\n", dup2, first)

	_, dup3, _, err := idx.Submit(certB, 1003)
	if err != nil {
		panic(err)
	}
	fmt.Printf("different cert:    duplicate=%v\n", dup3)

	fmt.Println("distinct entries:", idx.Len())

	// Output:
	// first submission:  duplicate=false leaf=5d95ecda
	// resubmission:      duplicate=true firstSeen=1001
	// different cert:    duplicate=false
	// distinct entries: 2
}
```

### Tests

`TestLeafHashMatchesRFC6962Prefix` and `TestLeafHashEmptyEntry` pin `LeafHash`
against `sha256.Sum256(append([]byte{0x00}, entry...))` computed directly in
the test, including the empty-entry edge case. `TestNewIndexRejectsNonPositiveCapacity`
pins the sentinel error. `TestSubmitDetectsDuplicates` and
`TestSubmitDistinctEntriesBothRecorded` check the core dedup behavior;
`TestSubmitRejectsBeyondCapacity` checks both the capacity boundary and that
a duplicate of an already-tracked entry still succeeds even when the index is
otherwise full, since it does not grow the map.

`TestNaiveIndexRetainsRawCertificates` and `TestIndexDoesNotRetainRawCertificates`
are the heart of the module, reusing the `runtime.ReadMemStats` discipline
from Exercise 2: force GC twice, read `HeapAlloc`, compare a 4 MiB allocation's
delta (64 synthetic 64 KiB certificates) against a 2 MiB threshold.
`leakyRecord` is an unexported test type that keeps the raw entry bytes
alongside the sequence number -- never exported, never reachable from the
package API -- and its test shows the heap growing with the certificate
volume. `Index`'s test performs the same 64 submissions and shows the heap
staying far below the threshold, because only 32-byte hashes and `int64`
sequence numbers are retained. Neither memory test calls `t.Parallel`:
`HeapAlloc` is process-global, so a concurrently allocating test would
perturb the reading.

`TestIndexIsSafeForConcurrentUse` submits 500 distinct entries from 500
goroutines under `-race`.

Create `ctleaf_test.go`:

```go
package ctleaf

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
)

// readHeap returns HeapAlloc after two full GC cycles. The second GC
// completes the sweep started by the first, so the reading is stable.
func readHeap() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// fillBuf allocates an n-byte slice and writes a pattern across it so the
// pages are committed and the object is a genuine, individually reclaimable
// allocation.
func fillBuf(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i) + seed
	}
	return b
}

func TestLeafHashMatchesRFC6962Prefix(t *testing.T) {
	t.Parallel()

	entry := []byte("a DER-encoded certificate, stand-in")
	got := LeafHash(entry)

	want := sha256.Sum256(append([]byte{0x00}, entry...))
	if got != want {
		t.Fatalf("LeafHash mismatch:\n got  %x\n want %x", got, want)
	}
}

func TestLeafHashEmptyEntry(t *testing.T) {
	t.Parallel()

	got := LeafHash(nil)
	want := sha256.Sum256([]byte{0x00})
	if got != want {
		t.Fatalf("LeafHash(nil) = %x, want %x", got, want)
	}
}

func TestNewIndexRejectsNonPositiveCapacity(t *testing.T) {
	t.Parallel()

	for _, c := range []int{0, -1} {
		if _, err := NewIndex(c); !errors.Is(err, ErrInvalidCapacity) {
			t.Errorf("NewIndex(%d) error = %v, want ErrInvalidCapacity", c, err)
		}
	}
}

func TestSubmitDetectsDuplicates(t *testing.T) {
	t.Parallel()

	idx, err := NewIndex(10)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	entry := []byte("certificate A")

	h1, dup1, _, err := idx.Submit(entry, 1)
	if err != nil {
		t.Fatalf("first Submit: %v", err)
	}
	if dup1 {
		t.Fatal("first submission reported as duplicate")
	}

	h2, dup2, first, err := idx.Submit(bytes.Clone(entry), 2)
	if err != nil {
		t.Fatalf("second Submit: %v", err)
	}
	if h1 != h2 {
		t.Fatalf("identical entries hashed differently: %x vs %x", h1, h2)
	}
	if !dup2 {
		t.Fatal("resubmission of an identical entry not detected as duplicate")
	}
	if first != 1 {
		t.Fatalf("firstSeen = %d, want 1", first)
	}
	if got := idx.Len(); got != 1 {
		t.Fatalf("Len() = %d, want 1", got)
	}
}

func TestSubmitDistinctEntriesBothRecorded(t *testing.T) {
	t.Parallel()

	idx, err := NewIndex(10)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	if _, dup, _, err := idx.Submit([]byte("certificate A"), 1); err != nil || dup {
		t.Fatalf("submit A: dup=%v err=%v", dup, err)
	}
	if _, dup, _, err := idx.Submit([]byte("certificate B"), 2); err != nil || dup {
		t.Fatalf("submit B: dup=%v err=%v", dup, err)
	}
	if got := idx.Len(); got != 2 {
		t.Fatalf("Len() = %d, want 2", got)
	}
}

func TestSubmitRejectsBeyondCapacity(t *testing.T) {
	t.Parallel()

	idx, err := NewIndex(2)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	if _, _, _, err := idx.Submit([]byte("A"), 1); err != nil {
		t.Fatalf("submit A: %v", err)
	}
	if _, _, _, err := idx.Submit([]byte("B"), 2); err != nil {
		t.Fatalf("submit B: %v", err)
	}
	if _, _, _, err := idx.Submit([]byte("C"), 3); !errors.Is(err, ErrIndexFull) {
		t.Fatalf("submit C error = %v, want ErrIndexFull", err)
	}
	// A duplicate of an already-recorded entry must still succeed even at
	// capacity: it does not grow the index.
	if _, dup, _, err := idx.Submit([]byte("A"), 4); err != nil || !dup {
		t.Fatalf("resubmit A at capacity: dup=%v err=%v", dup, err)
	}
}

// leakyRecord is what a naive dedup index stores per entry: the raw
// submitted certificate bytes "in case a debug dump needs them," alongside
// the sequence number. It is never exported and never reachable from the
// package API; it exists only so the tests can measure what it costs.
type leakyRecord struct {
	seq int64
	raw []byte
}

// TestNaiveIndexRetainsRawCertificates is the core of this module: a dedup
// index that keeps the raw entry bytes per record, at the scale a CT log
// operates at (millions of certificates, each several kilobytes), retains
// gigabytes no lookup ever needs -- the leaf hash alone answers every
// question the index exists to answer.
//
// This test deliberately does not call t.Parallel: it forces GC and reads
// process-global heap stats, which a concurrently allocating goroutine
// would perturb.
func TestNaiveIndexRetainsRawCertificates(t *testing.T) {
	const n = 64
	const entrySize = 64 << 10 // 64 KiB per synthetic certificate
	total := int64(n * entrySize)
	half := total / 2

	base := readHeap()

	leaky := make(map[[sha256.Size]byte]leakyRecord, n)
	for i := range n {
		entry := fillBuf(entrySize, byte(i))
		h := LeafHash(entry)
		leaky[h] = leakyRecord{seq: int64(i), raw: entry}
	}

	after := readHeap()
	if delta := int64(after) - int64(base); delta < half {
		t.Fatalf("naive index did not retain raw entries: delta %d bytes, want >= %d", delta, half)
	}
	runtime.KeepAlive(leaky)
}

// TestIndexDoesNotRetainRawCertificates is the fix, measured the same way:
// Index stores only the 32-byte leaf hash and an int64 sequence number per
// entry, so submitting the same volume of certificate data leaves the heap
// far below the naive index's footprint.
func TestIndexDoesNotRetainRawCertificates(t *testing.T) {
	const n = 64
	const entrySize = 64 << 10
	total := int64(n * entrySize)
	half := total / 2

	base := readHeap()

	idx, err := NewIndex(n)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	for i := range n {
		entry := fillBuf(entrySize, byte(i))
		if _, _, _, err := idx.Submit(entry, int64(i)); err != nil {
			t.Fatalf("Submit %d: %v", i, err)
		}
	}

	after := readHeap()
	if delta := int64(after) - int64(base); delta >= half {
		t.Fatalf("Index retained raw entries: delta %d bytes, want < %d", delta, half)
	}
	if got := idx.Len(); got != n {
		t.Fatalf("Len() = %d, want %d", got, n)
	}
}

func TestIndexIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	idx, err := NewIndex(500)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	var wg sync.WaitGroup
	for i := range 500 {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			entry := fmt.Appendf(nil, "certificate-%d", i)
			if _, dup, _, err := idx.Submit(entry, int64(i)); err != nil || dup {
				t.Errorf("Submit %d: dup=%v err=%v", i, dup, err)
			}
		}()
	}
	wg.Wait()
	if got := idx.Len(); got != 500 {
		t.Fatalf("Len() = %d, want 500", got)
	}
}

// ExampleIndex_Submit is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment.
func ExampleIndex_Submit() {
	idx, err := NewIndex(100)
	if err != nil {
		panic(err)
	}

	certA := []byte("-----BEGIN CERTIFICATE-----\nMIIB...A\n-----END CERTIFICATE-----")
	certB := []byte("-----BEGIN CERTIFICATE-----\nMIIB...B\n-----END CERTIFICATE-----")

	h1, dup1, _, err := idx.Submit(certA, 1001)
	if err != nil {
		panic(err)
	}
	fmt.Printf("first submission:  duplicate=%v leaf=%x\n", dup1, h1[:4])

	_, dup2, first, err := idx.Submit(certA, 1002)
	if err != nil {
		panic(err)
	}
	fmt.Printf("resubmission:      duplicate=%v firstSeen=%d\n", dup2, first)

	_, dup3, _, err := idx.Submit(certB, 1003)
	if err != nil {
		panic(err)
	}
	fmt.Printf("different cert:    duplicate=%v\n", dup3)

	fmt.Println("distinct entries:", idx.Len())

	// Output:
	// first submission:  duplicate=false leaf=5d95ecda
	// resubmission:      duplicate=true firstSeen=1001
	// different cert:    duplicate=false
	// distinct entries: 2
}
```

## Review

`Index` is correct when identical entries hash identically and are reported
as duplicates against their original sequence number, and distinct entries
are both recorded -- `TestSubmitDetectsDuplicates` and
`TestSubmitDistinctEntriesBothRecorded` pin exactly that.
`TestSubmitRejectsBeyondCapacity` pins `ErrIndexFull` and the boundary case
that a duplicate never counts against capacity. The mechanism worth
internalizing is that `LeafHash`'s fixed 32-byte return type removes the
Clone/Clip discipline this lesson otherwise drills into every module: a
`[32]byte` is a value, so keying a map on it can never alias or pin anything.
The mistake the module guards against is what happens *around* that safe
key -- `TestNaiveIndexRetainsRawCertificates` and
`TestIndexDoesNotRetainRawCertificates` measure, with a
`runtime.ReadMemStats` delta, the cost of also keeping the raw submitted
bytes "just in case," which at CT-log scale turns a megabyte-sized dedup
index into a gigabyte-sized one for data nothing ever reads back. Run
`go test -count=1 -race ./...` to confirm both the correctness table and the
memory contrast hold under the race detector.

## Resources

- [RFC 6962: Certificate Transparency](https://www.rfc-editor.org/rfc/rfc6962) — section 2.1 defines the Merkle tree leaf hash this module implements.
- [`crypto/sha256`](https://pkg.go.dev/crypto/sha256#pkg-constants) — `sha256.Size`, the fixed 32-byte digest length used as the map key type.
- [Go Specification: Conversions from slice to array or array pointer](https://go.dev/ref/spec#Conversions_from_slice_to_array_or_array_pointer) — the Go 1.20+ conversion used to turn the digest slice into `[sha256.Size]byte`.
- [`runtime.MemStats` and `ReadMemStats`](https://pkg.go.dev/runtime#MemStats) — the leak-detection technique this module's core test reuses from Exercise 2.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-audit-hook-closure-buffer-pin.md](10-audit-hook-closure-buffer-pin.md) | Next: [12-tls-cert-atomic-swap-reachability.md](12-tls-cert-atomic-swap-reachability.md)
