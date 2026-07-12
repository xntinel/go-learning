# Exercise 4: A CI size-budget gate for a hot struct

A struct you store by the millions has a memory budget, and the day someone adds
a field that blows it should fail the build, not surprise you in production. This
module pins `unsafe.Sizeof(CacheEntry{})` to a documented budget two ways: a
runtime test with a diagnostic failure message, and a compile-time static
assertion that refuses to build an over-budget struct.

This module is fully self-contained: its own `go mod init`, its own demo, its own
test.

## What you'll build

```text
sizegate/                  independent module: example.com/sizegate
  go.mod                   go 1.26
  entry.go                 type CacheEntry; const budget; compile-time size assert
  cmd/
    demo/
      main.go              prints size, budget, headroom, cost at 10M entries
  entry_test.go            fails (with field offsets) if Sizeof exceeds budget
```

- Files: `entry.go`, `cmd/demo/main.go`, `entry_test.go`.
- Implement: a `CacheEntry` struct stored by the millions, a `const budget`, and a compile-time static assertion that the struct fits.
- Test: a test that fails if `unsafe.Sizeof(CacheEntry{})` exceeds `budget`, printing the current size, the budget, and each field's offset so the author sees which field pushed it over.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/07-structs-and-methods/09-struct-memory-layout-and-padding/04-size-regression-gate/cmd/demo
cd go-solutions/07-structs-and-methods/09-struct-memory-layout-and-padding/04-size-regression-gate
```

### Why a size budget belongs in CI

`CacheEntry` models a session-cache row: a user id, issue/expiry timestamps, a
tenant id, a roles bitmask, and a couple of small enums. In a service that holds
ten million live sessions, the struct's size is multiplied by ten million: at 40
bytes that is 400 MB of resident memory, and every field you add is another
`10M * fieldsize` bytes plus whatever padding it drags in. Worse than the raw
bytes, a larger entry means fewer entries per cache line, so a scan over the map
touches more lines and runs slower.

None of that shows up in a functional test — the code is still correct, just
fatter. The defense is a *budget*: a documented ceiling on the struct's size,
checked automatically. Because `unsafe.Sizeof` is a compile-time constant you can
enforce the budget two ways, and this module does both. The runtime test is the
friendly one: when it fails it prints the current size, the budget, and every
field offset, so the author immediately sees that, say, adding a `string` field
(16 bytes) pushed the struct from 40 to 56. The compile-time assertion is the
strict one: an array whose length is `budget - size` has a negative length the
moment the struct exceeds budget, and a negative array length does not compile —
the build simply fails, no test run required.

Create `entry.go`. The static assertion is the `var _ [...]struct{}` line:

```go
// Package sizegate demonstrates enforcing a memory budget on a hot struct both
// at compile time (a static assertion) and in a test with a diagnostic message.
package sizegate

import "unsafe"

// budget is the maximum acceptable size of CacheEntry in bytes. At 10 million
// live entries, every byte here costs ~10 MB of resident memory, so this ceiling
// is a real capacity decision, not a style preference.
const budget = 48

// CacheEntry is one row of a session cache holding millions of entries. Fields
// are ordered largest-alignment-first so the struct stays compact:
// on a 64-bit platform it is 40 bytes, comfortably under the 48-byte budget.
type CacheEntry struct {
	UserID    uint64 // 8 @0
	ExpiresAt int64  // 8 @8  (unix seconds)
	IssuedAt  int64  // 8 @16 (unix seconds)
	TenantID  uint32 // 4 @24
	Roles     uint32 // 4 @28 (bitmask)
	Flags     uint8  // 1 @32
	Tier      uint8  // 1 @33
}

// Static assertion: if CacheEntry ever exceeds budget, (budget - size) is
// negative, the array length is negative, and this file fails to compile. This
// catches a size regression at build time, before any test runs.
var _ [budget - int(unsafe.Sizeof(CacheEntry{}))]struct{}

// Size reports the current size of CacheEntry in bytes.
func Size() uintptr { return unsafe.Sizeof(CacheEntry{}) }

// Budget reports the configured byte budget.
func Budget() uintptr { return budget }
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/sizegate"
)

func main() {
	const entries = 10_000_000
	size := sizegate.Size()
	budget := sizegate.Budget()
	fmt.Printf("CacheEntry = %d bytes (budget %d, headroom %d)\n", size, budget, budget-size)
	fmt.Printf("at %d entries: %d MB actual, %d MB at budget\n",
		entries, uintptr(entries)*size/(1<<20), uintptr(entries)*budget/(1<<20))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (on a 64-bit platform):

```
CacheEntry = 40 bytes (budget 48, headroom 8)
at 10000000 entries: 381 MB actual, 457 MB at budget
```

### Tests

The test re-checks the budget at runtime so a failure carries a helpful message
(the static assertion, by contrast, just fails the build with an array-length
error). On breach it dumps every field offset so the culprit is obvious.

Create `entry_test.go`:

```go
package sizegate

import (
	"testing"
	"unsafe"
)

func TestCacheEntryWithinBudget(t *testing.T) {
	t.Parallel()

	size := unsafe.Sizeof(CacheEntry{})
	if size > budget {
		var e CacheEntry
		t.Errorf("CacheEntry = %d bytes, over budget %d bytes\n"+
			"field offsets: UserID=%d ExpiresAt=%d IssuedAt=%d TenantID=%d Roles=%d Flags=%d Tier=%d",
			size, budget,
			unsafe.Offsetof(e.UserID), unsafe.Offsetof(e.ExpiresAt), unsafe.Offsetof(e.IssuedAt),
			unsafe.Offsetof(e.TenantID), unsafe.Offsetof(e.Roles), unsafe.Offsetof(e.Flags),
			unsafe.Offsetof(e.Tier))
	}
}

func TestDocumentedSize(t *testing.T) {
	t.Parallel()

	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("documented size is for 64-bit platforms")
	}
	if got := unsafe.Sizeof(CacheEntry{}); got != 40 {
		t.Errorf("CacheEntry = %d bytes, want 40 on 64-bit", got)
	}
}

func TestBudgetIsAchievable(t *testing.T) {
	t.Parallel()
	// Sanity: the budget must be at least the current size, or the static
	// assertion in entry.go would already have failed the build.
	if Size() > Budget() {
		t.Fatalf("Size %d exceeds Budget %d", Size(), Budget())
	}
}
```

## Review

The gate is correct when it fails the moment the struct outgrows its budget — the
static assertion at build time, the test with a readable message at test time.
The mistake it prevents is a silent footprint regression: a `CacheEntry` that
grows from 40 to 56 bytes is still functionally correct, so no behavioral test
catches it, yet at ten million entries it costs 160 MB more RAM and more cache
misses on every scan. Keep the exact-40 assertion behind a 64-bit guard (it is a
documented fact, not a portable contract) while the budget check itself is
portable.

## Resources

- [unsafe.Sizeof](https://pkg.go.dev/unsafe#Sizeof) — the compile-time constant the budget is built on.
- [Go spec: Constant expressions](https://go.dev/ref/spec#Constant_expressions) — why the static-assertion array length is evaluated at compile time.
- [Go spec: Array types](https://go.dev/ref/spec#Array_types) — a negative array length is a compile error, the mechanism of the static assert.

---

Back to [03-reflect-layout-auditor.md](03-reflect-layout-auditor.md) | Next: [05-cache-line-false-sharing.md](05-cache-line-false-sharing.md)
