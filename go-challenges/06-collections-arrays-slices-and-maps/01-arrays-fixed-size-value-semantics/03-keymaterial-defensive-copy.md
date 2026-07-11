# Exercise 3: Defensive Copy of Secret Key Material — Array Field vs Slice Field

At a secret-handling boundary you often hand out a snapshot of a credential and
must guarantee the caller cannot reach back and mutate the source. Whether that
guarantee holds for free depends entirely on one type choice: a `Key [32]byte`
array field is deep-copied when the struct is copied, while a `Key []byte` slice
field is not. This exercise builds both and pins the difference as intended
behavior.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
keymaterial/                 independent module: example.com/keymaterial
  go.mod
  credential.go              Credential{Key [32]byte}.Snapshot(); LeakyCredential{Key []byte}.Snapshot()
  cmd/
    demo/
      main.go                runnable demo: mutate a snapshot, show source safe vs corrupted
  credential_test.go         array snapshot isolates; slice snapshot aliases; == on whole struct
```

- Files: `credential.go`, `cmd/demo/main.go`, `credential_test.go`.
- Implement: `Credential` with a `Key [32]byte` field and a `Snapshot() Credential`; a `LeakyCredential` with a `Key []byte` field and the broken `Snapshot()`.
- Test: mutating an array snapshot leaves the source unchanged; mutating a slice snapshot corrupts the source; two array-based snapshots compare equal with `==`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/keymaterial/cmd/demo
cd ~/go-exercises/keymaterial
go mod init example.com/keymaterial
```

### Why the array field is a real defensive copy

When you copy a struct in Go — by assignment, by passing it as a value parameter,
or by returning it by value — every field is copied field-by-field. For a
`[32]byte` field that means all thirty-two bytes are duplicated into the new
struct; the two structs share no storage. So `Snapshot() Credential` that simply
returns `*c` (or `c` by value) hands the caller a `Credential` whose `Key` is an
independent array. The caller can scribble over `snapshot.Key[0]` all day and the
original `Credential` is untouched. The defensive copy is automatic, a direct
consequence of value semantics.

Now the broken variant. `LeakyCredential` stores the key as `Key []byte`. Copying
that struct copies the *slice header* — the pointer, length, and capacity — but not
the backing array the pointer refers to. So the snapshot's `Key` and the original's
`Key` are two headers pointing at the *same* bytes. When the caller mutates
`snapshot.Key[0]`, they mutate the original's key material too. The struct copy
looks like a defensive copy and is not; this is one of the most common
security-relevant aliasing bugs in Go.

There is a second payoff to the array field: the whole `Credential` struct is
comparable. Because `[32]byte` is comparable and the struct has no incomparable
fields, `cred1 == cred2` is legal and compares the key bytes element-by-element.
The `LeakyCredential` struct, with its `[]byte` field, is *not* comparable at all —
`==` on it does not compile. So the array field buys both a real defensive copy and
whole-struct equality.

Create `credential.go`:

```go
package keymaterial

// Credential holds secret key material in a fixed-size array field. Because the
// array is copied when the struct is copied, a by-value snapshot is a true
// defensive copy, and the whole struct is comparable with ==.
type Credential struct {
	ID  string
	Key [32]byte
}

// Snapshot returns an independent copy of the credential. The [32]byte Key field
// is deep-copied by value semantics, so mutating the snapshot cannot affect the
// source.
func (c Credential) Snapshot() Credential {
	return c
}

// LeakyCredential is the WRONG design: storing the key as a slice. A by-value
// copy duplicates only the slice header, so the "snapshot" still aliases the
// original's backing array. Kept here to pin the difference in a test.
type LeakyCredential struct {
	ID  string
	Key []byte
}

// Snapshot returns a struct copy that shares the underlying key bytes. This looks
// like a defensive copy but is not.
func (c LeakyCredential) Snapshot() LeakyCredential {
	return c
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/keymaterial"
)

func main() {
	// Array-field credential: snapshot is isolated.
	src := keymaterial.Credential{ID: "svc-a"}
	src.Key[0] = 0x11
	snap := src.Snapshot()
	snap.Key[0] = 0xff // mutate the snapshot
	fmt.Printf("array: source[0]=%#x snapshot[0]=%#x isolated=%v\n",
		src.Key[0], snap.Key[0], src.Key[0] != snap.Key[0])

	// Slice-field credential: snapshot aliases.
	leaky := keymaterial.LeakyCredential{ID: "svc-b", Key: make([]byte, 32)}
	leaky.Key[0] = 0x11
	lsnap := leaky.Snapshot()
	lsnap.Key[0] = 0xff // mutate the "snapshot"
	fmt.Printf("slice: source[0]=%#x snapshot[0]=%#x corrupted=%v\n",
		leaky.Key[0], lsnap.Key[0], leaky.Key[0] == lsnap.Key[0])

	// Whole-struct equality works for the array-field credential.
	fmt.Printf("two array snapshots equal: %v\n", src.Snapshot() == src.Snapshot())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
array: source[0]=0x11 snapshot[0]=0xff isolated=true
slice: source[0]=0xff snapshot[0]=0xff corrupted=true
two array snapshots equal: true
```

The array source keeps its `0x11`; the slice source is dragged to `0xff` by a
mutation of its "snapshot". That single line is the whole lesson.

### Tests

`TestArraySnapshotIsolates` mutates a snapshot's key and asserts the source is
unchanged — the defensive copy holds. `TestSliceSnapshotAliases` asserts the
opposite for `LeakyCredential`: mutating the snapshot *does* corrupt the source.
Pinning the broken behavior as an explicit expectation documents exactly why the
array field is required. `TestCredentialComparable` asserts two equal-keyed
credentials are `==` and a one-byte difference is not.

Create `credential_test.go`:

```go
package keymaterial

import (
	"testing"
)

func TestArraySnapshotIsolates(t *testing.T) {
	t.Parallel()

	src := Credential{ID: "svc"}
	src.Key[0] = 0x11

	snap := src.Snapshot()
	snap.Key[0] = 0xff

	if src.Key[0] != 0x11 {
		t.Fatalf("array snapshot leaked into source: source[0]=%#x, want 0x11", src.Key[0])
	}
}

func TestSliceSnapshotAliases(t *testing.T) {
	t.Parallel()

	src := LeakyCredential{ID: "svc", Key: make([]byte, 32)}
	src.Key[0] = 0x11

	snap := src.Snapshot()
	snap.Key[0] = 0xff

	// Intended (broken) behavior: the slice snapshot shares the backing array.
	if src.Key[0] != 0xff {
		t.Fatalf("slice snapshot should alias source; source[0]=%#x, want 0xff", src.Key[0])
	}
}

func TestCredentialComparable(t *testing.T) {
	t.Parallel()

	a := Credential{ID: "svc"}
	b := Credential{ID: "svc"}
	a.Key[0] = 0x11
	b.Key[0] = 0x11
	if a != b {
		t.Fatal("credentials with equal ID and Key should be ==")
	}

	b.Key[0] = 0x12
	if a == b {
		t.Fatal("a one-byte key difference must make credentials unequal")
	}
}
```

## Review

The array field is correct when the by-value snapshot is genuinely independent:
`TestArraySnapshotIsolates` mutates the copy and the source stays put. The slice
field is the cautionary twin — `TestSliceSnapshotAliases` deliberately asserts the
"snapshot" corrupts its source, documenting the aliasing trap rather than hiding
it. The rule to carry out of this exercise: a by-value struct copy is a defensive
copy only for its value-typed fields (arrays, numbers, other structs of the same);
a `[]byte`, map, or channel field is copied by handle and continues to alias. When
you must snapshot key material, store it in a `[N]byte` array field, or copy the
slice explicitly with `append([]byte(nil), key...)`. Run `go test -race` to confirm
both behaviors, and note `TestCredentialComparable` demonstrating the whole-struct
`==` that only the array field enables.

## Resources

- [Go blog: Arrays, slices (and strings)](https://go.dev/blog/slices) — why copying a slice header shares the backing array.
- [Go Specification: Assignability and struct copies](https://go.dev/ref/spec#Assignments) — field-by-field copy semantics.
- [Go Specification: Comparison operators](https://go.dev/ref/spec#Comparison_operators) — a struct is comparable only if all its fields are.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-content-addressed-dedup-store.md](02-content-addressed-dedup-store.md) | Next: [04-header-canonicalize-lookup-table.md](04-header-canonicalize-lookup-table.md)
