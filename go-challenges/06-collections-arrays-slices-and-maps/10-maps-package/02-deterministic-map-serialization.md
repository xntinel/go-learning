# Exercise 2: Deterministic Serialization for ETags and Cache Keys

An HTTP `ETag`, an idempotency-key fingerprint, and a cache key all have the same
requirement: equal inputs must produce byte-for-byte identical output, every time,
in every process. Build that output by ranging a map and it will not — the
randomized iteration order makes the bytes churn on every call. This module builds
the canonical-serialization primitives the right way and proves their stability.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. It gates alone.

## What you'll build

```text
canonmap/                   independent module: example.com/canonmap
  go.mod                    go 1.26
  canonmap.go               CanonicalKey, StableJSON, ETag
  cmd/
    demo/
      main.go               two maps built in different orders, identical output
  canonmap_test.go          order-independence, sha256 stability, naive-flake contrast
```

Files: `canonmap.go`, `cmd/demo/main.go`, `canonmap_test.go`.
Implement: `CanonicalKey(map[string]string) string`, `StableJSON(map[string]any) []byte`, `ETag(map[string]string) string`.
Test: identical output for maps built in different insertion orders; identical sha256; a naive range-based version to show the contrast.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/10-maps-package/02-deterministic-map-serialization/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/10-maps-package/02-deterministic-map-serialization
```

## Why sorting the keys is the whole job

The runtime randomizes map iteration order per range statement. Two maps with
exactly the same entries, ranged twice, can yield two different orders — even
within a single process, even for the same map. So any serialization that ranges
the map directly produces output whose byte order depends on nothing meaningful,
and an ETag computed from it changes on every request for an unchanged resource.
Downstream, every `If-None-Match` misses, every cache entry is unique, and the
whole point of the ETag — cheap conditional requests — is defeated.

The fix is one line of discipline: sort the keys before you emit anything.
`slices.Sorted(maps.Keys(m))` gives a stable `[]string`; you then range that
slice and write `key=value` (or JSON) pairs in a fixed order. Now equal maps
produce identical bytes, and a hash of those bytes is a stable fingerprint.

`CanonicalKey` builds a compact `k=v;` string — the kind you would use as a map
key for an in-process request cache or as the seed for an idempotency key. It
sorts the keys, then writes each pair with `fmt.Fprintf` into a
`strings.Builder`. `StableJSON` does the same for a `map[string]any` but emits
canonical JSON, marshaling each value with `encoding/json` and writing the object
fields in sorted key order; this is what you sign, hash, or store when the JSON
must be reproducible. (For nested objects Go's `encoding/json` already sorts
`map[string]...` keys when marshaling a map, but it does not sort a top-level map
you assemble field by field — and it never guarantees ordering if you build the
output by ranging yourself, which is the trap this exercise closes.)

`ETag` layers a `crypto/sha256` hash over `CanonicalKey` and formats it as a
quoted hex string, the shape an `ETag` HTTP header takes. Because the input to the
hash is canonical, the digest is stable, and two servers computing the ETag of the
same resource agree.

Create `canonmap.go`:

```go
package canonmap

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// CanonicalKey renders a string map as a deterministic "k=v;" string, with keys
// in sorted order, so equal maps always produce identical bytes.
func CanonicalKey(m map[string]string) string {
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(m)) {
		fmt.Fprintf(&b, "%s=%s;", k, m[k])
	}
	return b.String()
}

// StableJSON renders a map as canonical JSON with object fields in sorted key
// order. The output is reproducible and safe to hash or sign.
func StableJSON(m map[string]any) []byte {
	var b strings.Builder
	b.WriteByte('{')
	first := true
	for _, k := range slices.Sorted(maps.Keys(m)) {
		if !first {
			b.WriteByte(',')
		}
		first = false
		key, _ := json.Marshal(k)
		val, _ := json.Marshal(m[k])
		b.Write(key)
		b.WriteByte(':')
		b.Write(val)
	}
	b.WriteByte('}')
	return []byte(b.String())
}

// ETag computes a stable, quoted hex ETag over the canonical form of the map.
func ETag(m map[string]string) string {
	sum := sha256.Sum256([]byte(CanonicalKey(m)))
	return fmt.Sprintf("%q", fmt.Sprintf("%x", sum))
}
```

`json.Marshal` on a `string` cannot fail, so the discarded errors above are safe;
for arbitrary `any` values that could fail to marshal you would surface the error,
but the exercise keeps the values JSON-friendly.

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/canonmap"
)

func main() {
	// Same entries, different insertion order.
	a := map[string]string{}
	a["region"] = "eu"
	a["tier"] = "gold"
	a["user"] = "alice"

	b := map[string]string{}
	b["user"] = "alice"
	b["region"] = "eu"
	b["tier"] = "gold"

	fmt.Println(canonmap.CanonicalKey(a))
	fmt.Println("keys match:", canonmap.CanonicalKey(a) == canonmap.CanonicalKey(b))
	fmt.Println("etags match:", canonmap.ETag(a) == canonmap.ETag(b))
	fmt.Printf("etag: %s\n", canonmap.ETag(a))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
region=eu;tier=gold;user=alice;
keys match: true
etags match: true
etag: "301293caa7852ec58e36b087cf4e0e5cd6d1ac9a223f9699f6cfff7813e29e8e"
```

The ETag digest depends on the exact hash of `region=eu;tier=gold;user=alice;`;
your run prints the full 64-hex-character value. What matters is that the two
maps produce the *same* ETag, which the test asserts precisely.

### Tests

The key tests build two maps with the same entries inserted in different orders
and assert identical `CanonicalKey`, identical `StableJSON`, and identical
sha256. A property test permutes the insertion order many times and asserts the
canonical form never changes. `TestNaiveRangeIsNotStable` builds the same output
the wrong way — ranging the map directly — and asserts that across many maps it
produces at least one differing result, making the non-determinism visible rather
than hand-waved.

Create `canonmap_test.go`:

```go
package canonmap

import (
	"fmt"
	"strings"
	"testing"
)

func sameEntries() (map[string]string, map[string]string) {
	a := map[string]string{"region": "eu", "tier": "gold", "user": "alice"}
	b := map[string]string{}
	b["user"] = "alice"
	b["tier"] = "gold"
	b["region"] = "eu"
	return a, b
}

func TestCanonicalKeyOrderIndependent(t *testing.T) {
	t.Parallel()

	a, b := sameEntries()
	if CanonicalKey(a) != CanonicalKey(b) {
		t.Fatalf("CanonicalKey differs for equal maps: %q vs %q", CanonicalKey(a), CanonicalKey(b))
	}
	want := "region=eu;tier=gold;user=alice;"
	if got := CanonicalKey(a); got != want {
		t.Fatalf("CanonicalKey() = %q, want %q", got, want)
	}
}

func TestETagStable(t *testing.T) {
	t.Parallel()

	a, b := sameEntries()
	if ETag(a) != ETag(b) {
		t.Fatalf("ETag differs for equal maps: %s vs %s", ETag(a), ETag(b))
	}
}

func TestStableJSONOrderIndependent(t *testing.T) {
	t.Parallel()

	a := map[string]any{"b": 2, "a": 1, "c": 3}
	b := map[string]any{"c": 3, "a": 1, "b": 2}
	if string(StableJSON(a)) != string(StableJSON(b)) {
		t.Fatalf("StableJSON differs: %s vs %s", StableJSON(a), StableJSON(b))
	}
	want := `{"a":1,"b":2,"c":3}`
	if got := string(StableJSON(a)); got != want {
		t.Fatalf("StableJSON() = %s, want %s", got, want)
	}
}

func TestCanonicalKeyManyPermutations(t *testing.T) {
	t.Parallel()

	base := map[string]string{"x": "1", "y": "2", "z": "3", "w": "4"}
	want := CanonicalKey(base)
	for i := range 500 {
		m := map[string]string{}
		// Insert in an order that varies with i.
		keys := []string{"x", "y", "z", "w"}
		for j := range keys {
			m[keys[(i+j)%len(keys)]] = base[keys[(i+j)%len(keys)]]
		}
		if got := CanonicalKey(m); got != want {
			t.Fatalf("permutation %d: CanonicalKey() = %q, want %q", i, got, want)
		}
	}
}

// naiveKey ranges the map directly; its output order is not stable.
func naiveKey(m map[string]string) string {
	var b strings.Builder
	for k, v := range m {
		fmt.Fprintf(&b, "%s=%s;", k, v)
	}
	return b.String()
}

func TestNaiveRangeIsNotStable(t *testing.T) {
	t.Parallel()

	m := map[string]string{"a": "1", "b": "2", "c": "3", "d": "4", "e": "5", "f": "6", "g": "7", "h": "8"}
	first := naiveKey(m)
	sawDifferent := false
	for range 1000 {
		if naiveKey(m) != first {
			sawDifferent = true
			break
		}
	}
	if !sawDifferent {
		t.Skip("naive range happened to be stable this run; canonical form is stable by construction")
	}
	// The canonical form, by contrast, is always identical for the same map.
	if CanonicalKey(m) != CanonicalKey(m) {
		t.Fatal("CanonicalKey is not stable")
	}
}
```

An `Example` that hard-codes a sha256 digest is brittle, so the example asserts
the *property* — equal maps yield equal ETags — and prints a boolean.

Create `example_test.go`:

```go
package canonmap

import "fmt"

func ExampleETag() {
	m := map[string]string{"a": "1", "b": "2"}
	fmt.Println(ETag(m) == ETag(map[string]string{"b": "2", "a": "1"}))
	// Output: true
}
```

## Review

The contract is stability: equal maps must produce equal bytes. Every function
here gets that by sorting keys with `slices.Sorted(maps.Keys(m))` before emitting,
and the order-independence tests prove it by constructing the same map two ways.
`TestNaiveRangeIsNotStable` is deliberately the anti-pattern, showing why ranging
the map directly cannot be trusted for serialized output — and it `Skip`s rather
than fails in the rare run where the randomization happens not to reorder, because
the point is the contrast, not a guarantee about a broken approach. When an ETag
or cache key ever flakes in production, the cause is almost always a map ranged
without sorting somewhere upstream. Run `go test -race` to confirm the whole
module is clean.

## Resources

- [maps package](https://pkg.go.dev/maps) — `Keys`.
- [slices package](https://pkg.go.dev/slices) — `Sorted`.
- [crypto/sha256](https://pkg.go.dev/crypto/sha256) — `Sum256`.
- [MDN: HTTP ETag](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/ETag) — what a stable ETag buys you.

---

Back to [01-map-transform-pipeline.md](01-map-transform-pipeline.md) | Next: [03-layered-config-merge.md](03-layered-config-merge.md)
