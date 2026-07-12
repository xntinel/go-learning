# Exercise 2: Affinity Partitioner

A `hash(key) % N` partitioner is stable but rescales terribly: adding one partition remaps almost every key, and in a stateful engine every remapped key is state that must migrate. This module builds a rendezvous (highest-random-weight) partitioner that assigns a key to the highest-scoring partition, so growing the partition count moves only the minimal `1/(N+1)` fraction of keys — and a deterministic test proves it moves far fewer keys than modulo hashing.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
affinity.go            Partitioner, ModPartitioner, AffinityPartitioner,
                       weight (rendezvous score), mix64 (SplitMix64 finalizer),
                       MovedKeys, Distribution
cmd/
  demo/
    main.go            compare rescale 4->5 cost: affinity vs modulo
affinity_test.go       stability, in-range, distribution, rescale moves fewer
                       keys than modulo, moved keys land on the new partition
```

- Files: `affinity.go`, `cmd/demo/main.go`, `affinity_test.go`.
- Implement: `AffinityPartitioner` (rendezvous hashing with a SplitMix64 score), a `ModPartitioner` baseline, and the `MovedKeys` / `Distribution` measurement helpers.
- Test: `affinity_test.go` asserts per-key stability, even distribution, and — the core property — that a 4-to-5 rescale moves strictly fewer keys under affinity than under modulo, with every moved key landing on the new partition.
- Verify: `go test -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Why rendezvous hashing, and why the score mixing matters

The rescale cost of a partitioner is the number of keys whose assignment changes when N changes — each one is keyed state that must move before processing resumes. Under `hash(key) % N` the modulus changes for nearly every key when N grows by one, so roughly `(N-1)/N` of keys move; 4 to 5 moves about 80%. Rendezvous hashing instead scores every `(key, partition)` pair and assigns the key to the partition with the maximum score. Adding a partition introduces one new score per key; the key moves only if that new score is the maximum, which for independent uniform scores happens with probability exactly `1/(N+1)`. Growing 4 to 5 therefore moves about 20% — the information-theoretic minimum, since the new partition must receive *some* share and `1/(N+1)` is that share.

The catch is the word "independent". The whole guarantee rests on the per-partition scores of a single key being effectively independent and uniform. If they are correlated — which is exactly what happens if you compute the score by appending the partition index as trailing bytes to a streaming hash of the key — then the argmax is biased, some partitions win more often than others, and the rescale moves more keys than the ideal. The fix is to hash the key once, combine that hash with the partition index by XOR-ing in `partition * 0x9E3779B97F4A7C15` (an odd constant derived from the golden ratio, which spreads consecutive indices far apart), and then run the result through the SplitMix64 finalizer `mix64`, whose three xor-shift-multiply rounds avalanche every input bit across the full 64-bit output. With that mixing the observed rescale cost lands on the 20% ideal rather than drifting to 30%+.

Create `affinity.go`:

```go
// Package affinity provides partitioners that map record keys to partition
// indices. The AffinityPartitioner uses rendezvous (highest-random-weight)
// hashing so that growing or shrinking the partition count moves the minimum
// possible number of keys, in contrast to the modulo partitioner which
// remaps almost every key on a rescale.
package affinity

import (
	"hash/crc32"
	"hash/fnv"
)

// Partitioner maps a key to a partition index in [0, numPartitions).
type Partitioner interface {
	Partition(key []byte, numPartitions int) int
}

// ModPartitioner maps a key with crc32(key) % numPartitions. It distributes
// keys evenly but has no rescale affinity: changing numPartitions remaps the
// great majority of keys.
type ModPartitioner struct{}

// Partition implements Partitioner.
func (ModPartitioner) Partition(key []byte, numPartitions int) int {
	if numPartitions <= 0 || len(key) == 0 {
		return 0
	}
	return int(crc32.ChecksumIEEE(key) % uint32(numPartitions))
}

// AffinityPartitioner assigns a key to the partition with the highest weight,
// where the weight of (key, partition) is a hash of the key mixed with the
// partition index. This is rendezvous hashing. For a fixed numPartitions the
// assignment is stable, and when numPartitions changes only the keys whose new
// highest-weight partition is the added or removed one are reassigned.
type AffinityPartitioner struct{}

// Partition implements Partitioner.
func (AffinityPartitioner) Partition(key []byte, numPartitions int) int {
	if numPartitions <= 0 || len(key) == 0 {
		return 0
	}
	hk := fnv64a(key)
	best := 0
	bestW := weight(hk, 0)
	for i := 1; i < numPartitions; i++ {
		if w := weight(hk, i); w > bestW {
			best, bestW = i, w
		}
	}
	return best
}

// fnv64a is the 64-bit FNV-1a hash of b.
func fnv64a(b []byte) uint64 {
	h := fnv.New64a()
	h.Write(b)
	return h.Sum64()
}

// weight is the rendezvous score of (key-hash, part): the key hash combined
// with the partition index and passed through a strong 64-bit finalizer so
// that the per-partition scores of one key are effectively independent and
// uniform. Independence is what makes the highest-scoring partition uniformly
// distributed, which in turn makes the rescale cost minimal.
func weight(hk uint64, part int) uint64 {
	return mix64(hk ^ (uint64(part)+1)*0x9E3779B97F4A7C15)
}

// mix64 is the SplitMix64 finalizer: it avalanches every input bit across the
// whole 64-bit output.
func mix64(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xBF58476D1CE4E5B9
	x ^= x >> 27
	x *= 0x94D049BB133111EB
	x ^= x >> 31
	return x
}

// MovedKeys returns how many of keys are assigned to a different partition when
// the partition count changes from oldN to newN under p. It is the rescale cost
// of a partitioner: lower is better because each moved key is keyed state that
// must be migrated.
func MovedKeys(p Partitioner, keys [][]byte, oldN, newN int) int {
	moved := 0
	for _, k := range keys {
		if p.Partition(k, oldN) != p.Partition(k, newN) {
			moved++
		}
	}
	return moved
}

// Distribution returns the per-partition key counts for keys under p with
// numPartitions partitions. The returned slice has length numPartitions.
func Distribution(p Partitioner, keys [][]byte, numPartitions int) []int {
	counts := make([]int, numPartitions)
	for _, k := range keys {
		counts[p.Partition(k, numPartitions)]++
	}
	return counts
}
```

`MovedKeys` is the measurement that turns the property into a number: it counts how many keys land on a different partition when the count goes from `oldN` to `newN`. It works for any `Partitioner`, so the test can run it on both the affinity and the modulo implementations and compare. `Distribution` is the complementary check that the affinity hash still spreads keys evenly at a fixed N — minimal movement is worthless if the steady-state balance is poor.

### The runnable demo

The demo builds ten thousand keys, prints a few affinity assignments at N=4, then measures the rescale cost from 4 to 5 partitions under both partitioners and prints the final distribution at N=5. The numbers are deterministic because the key set and both hashes are deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/affinity"
)

func main() {
	// Build a stable key set.
	const numKeys = 10000
	keys := make([][]byte, numKeys)
	for i := range keys {
		keys[i] = []byte(fmt.Sprintf("key-%d", i))
	}

	aff := affinity.AffinityPartitioner{}
	mod := affinity.ModPartitioner{}

	// A few keys are stably assigned under affinity hashing at N=4.
	fmt.Println("affinity assignment at N=4:")
	for _, k := range [][]byte{[]byte("key-0"), []byte("key-1"), []byte("key-2")} {
		fmt.Printf("  %-7s -> partition %d\n", k, aff.Partition(k, 4))
	}

	// Compare rescale cost when growing the partition count 4 -> 5.
	affMoved := affinity.MovedKeys(aff, keys, 4, 5)
	modMoved := affinity.MovedKeys(mod, keys, 4, 5)
	fmt.Printf("rescale 4 -> 5 over %d keys:\n", numKeys)
	fmt.Printf("  affinity moved %d keys (%.1f%%)\n", affMoved, 100*float64(affMoved)/numKeys)
	fmt.Printf("  modulo   moved %d keys (%.1f%%)\n", modMoved, 100*float64(modMoved)/numKeys)

	// Distribution at N=5 stays balanced under affinity hashing.
	dist := affinity.Distribution(aff, keys, 5)
	fmt.Println("affinity distribution at N=5:")
	for i, c := range dist {
		fmt.Printf("  partition %d: %d keys\n", i, c)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
affinity assignment at N=4:
  key-0   -> partition 0
  key-1   -> partition 1
  key-2   -> partition 0
rescale 4 -> 5 over 10000 keys:
  affinity moved 2000 keys (20.0%)
  modulo   moved 7945 keys (79.5%)
affinity distribution at N=5:
  partition 0: 1956 keys
  partition 1: 2034 keys
  partition 2: 1992 keys
  partition 3: 2018 keys
  partition 4: 2000 keys
```

The affinity partitioner moves exactly the `1/5` of keys that the new partition wins; the modulo partitioner moves nearly four times as many.

### Tests

`TestAffinityStable` pins the per-key stability invariant. `TestAffinityDistribution` checks the steady-state balance. The two rescale tests are the point of the exercise: `TestAffinityRescaleMovesFewerKeysThanMod` asserts that affinity moves strictly fewer keys than modulo on a 4-to-5 rescale and that the moved fraction stays under 30% — both numbers are deterministic for a fixed key set, so the comparison never flakes. `TestAffinityRescaleKeepsUnmovedKeysPut` adds the sharper structural claim: under HRW a key that moves on a grow always moves *to the new partition*, never sideways between existing ones.

Create `affinity_test.go`:

```go
package affinity

import (
	"fmt"
	"testing"
)

func testKeys(n int) [][]byte {
	keys := make([][]byte, n)
	for i := range keys {
		keys[i] = []byte(fmt.Sprintf("key-%d", i))
	}
	return keys
}

func TestAffinityStable(t *testing.T) {
	t.Parallel()
	p := AffinityPartitioner{}
	for _, k := range testKeys(200) {
		first := p.Partition(k, 8)
		for i := 0; i < 100; i++ {
			if got := p.Partition(k, 8); got != first {
				t.Fatalf("key %q: got %d, want %d on repeat %d", k, got, first, i)
			}
		}
	}
}

func TestAffinityEmptyKeyToZero(t *testing.T) {
	t.Parallel()
	if got := (AffinityPartitioner{}).Partition(nil, 8); got != 0 {
		t.Fatalf("empty key partition = %d, want 0", got)
	}
}

func TestAffinityInRange(t *testing.T) {
	t.Parallel()
	p := AffinityPartitioner{}
	for _, k := range testKeys(500) {
		got := p.Partition(k, 6)
		if got < 0 || got >= 6 {
			t.Fatalf("key %q partition %d out of range [0,6)", k, got)
		}
	}
}

func TestAffinityDistribution(t *testing.T) {
	t.Parallel()
	const n = 12000
	const parts = 6
	counts := Distribution(AffinityPartitioner{}, testKeys(n), parts)
	// Expected share is 1/6 ~= 0.167. Allow a wide band for a finite sample.
	for i, c := range counts {
		ratio := float64(c) / n
		if ratio < 0.10 || ratio > 0.24 {
			t.Errorf("partition %d ratio %.3f outside [0.10, 0.24]", i, ratio)
		}
	}
}

// TestAffinityRescaleMovesFewerKeysThanMod is the core property: rendezvous
// hashing reassigns far fewer keys than modulo hashing when the partition
// count grows. Both numbers are deterministic for a fixed key set.
func TestAffinityRescaleMovesFewerKeysThanMod(t *testing.T) {
	t.Parallel()
	keys := testKeys(12000)
	affMoved := MovedKeys(AffinityPartitioner{}, keys, 4, 5)
	modMoved := MovedKeys(ModPartitioner{}, keys, 4, 5)

	if affMoved == 0 {
		t.Fatal("affinity moved 0 keys on 4->5 rescale; new partition received nothing")
	}
	if affMoved >= modMoved {
		t.Fatalf("affinity moved %d keys, mod moved %d; affinity must move fewer", affMoved, modMoved)
	}
	// The new partition's expected share is 1/5; moved keys should be well
	// under a third of all keys.
	if frac := float64(affMoved) / float64(len(keys)); frac > 0.30 {
		t.Fatalf("affinity rescale moved fraction %.3f, want <= 0.30", frac)
	}
}

func TestAffinityRescaleKeepsUnmovedKeysPut(t *testing.T) {
	t.Parallel()
	// Every key that is NOT reassigned must keep the exact same partition.
	p := AffinityPartitioner{}
	keys := testKeys(2000)
	moved := 0
	for _, k := range keys {
		before := p.Partition(k, 4)
		after := p.Partition(k, 5)
		if before != after {
			moved++
			// A moved key under HRW always moves to the new partition (index 4).
			if after != 4 {
				t.Fatalf("key %q moved from %d to %d, want new partition 4", k, before, after)
			}
		}
	}
	if moved == 0 {
		t.Fatal("expected some keys to move to the new partition")
	}
}

func ExampleAffinityPartitioner_Partition() {
	p := AffinityPartitioner{}
	// The same key is stable across repeated calls for a fixed partition count.
	a := p.Partition([]byte("user-42"), 4)
	b := p.Partition([]byte("user-42"), 4)
	fmt.Println(a == b)
	// Output:
	// true
}
```

## Review

The implementation is correct when the rescale moves the minimal fraction and the steady-state distribution stays balanced; both are deterministic functions of the key set, so the tests assert them without tolerance for flakiness. The single most common bug is a weak score mix — appending the partition bytes to a streaming hash — which silently inflates the moved fraction; the SplitMix64 finalizer is what keeps it on the `1/(N+1)` ideal, and `TestAffinityRescaleMovesFewerKeysThanMod`'s 30% ceiling is the guard that would catch a regression to a weak mix. The second is forgetting that rendezvous is O(N) per key: it is simpler than a consistent-hash ring but scans every partition, so for very large N a ring's O(log N) lookup wins. Run `go test -race` to confirm the partitioner has no shared mutable state — it is a pure function, so there should be nothing for the race detector to find.

## Resources

- [Rendezvous hashing (Wikipedia)](https://en.wikipedia.org/wiki/Rendezvous_hashing) — the highest-random-weight algorithm this exercise implements, with the `1/(N+1)` movement proof.
- [Consistent hashing (Wikipedia)](https://en.wikipedia.org/wiki/Consistent_hashing) — the ring-based alternative with the same minimal-movement property and O(log N) lookup.
- [hash/fnv](https://pkg.go.dev/hash/fnv) — the FNV-1a key hash used as the rendezvous score seed.
- [hash/crc32](https://pkg.go.dev/hash/crc32) — the CRC-32/IEEE hash behind the `ModPartitioner` baseline.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-parallel-operator.md](01-parallel-operator.md) | Next: [03-keyed-state-operator.md](03-keyed-state-operator.md)
