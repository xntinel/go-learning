# Exercise 18: A Bidirectional Index That Cannot Drift Out of Sync

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A user-directory service that needs to look up "which user has this email"
as often as "what is this user's email" reaches for the same trick a
database index does: keep two maps, `userID -> email` and its mirror
`email -> userID`, so both lookups stay O(1). The catch is that now there
are two copies of the same fact, and every mutation has to update both or
the index quietly lies. The bug that actually ships is not in `Set` -- it is
in `Rename`: update the forward map to point `u1` at a new email, forget to
delete the *old* email's entry in the reverse map, and now the old email
still resolves to `u1` even though `u1` no longer owns it. A second user who
legitimately claims that freed-up email later collides with a phantom
reverse entry nobody is looking at.

This module builds that index as a package you can drop into a service:
`Set`, `Rename`, and `Delete`, each one required to leave both maps exact
inverses of each other, and it proves that property the way a database
migration test would: with an explicit invariant checker that walks both
maps end to end, plus a long, seeded (reproducible) randomized sequence of
operations that runs the checker after every single call.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
biindex/                module example.com/biindex
  go.mod                go 1.24
  biindex.go            Index (idToEmail, emailToID); Set/Rename/Delete/EmailOf/IDOf/Len; CheckInvariant
  biindex_test.go       Set/Rename/Delete correctness, stale-reverse-entry regression test,
                         5000-op seeded fuzz, Example
```

- Files: `biindex.go`, `biindex_test.go`.
- Implement: `Index` wrapping two maps, `idToEmail map[string]string` and `emailToID map[string]string`; `Set(id, email string) error` (insert or update, rejecting a taken email with `ErrEmailTaken`); `Rename(id, newEmail string) error` (like `Set` but requires `id` to already exist, returning `ErrNotFound` otherwise); `Delete(id string)`; `EmailOf`/`IDOf` comma-ok lookups; and `CheckInvariant() error`, which walks both maps and returns a descriptive error the instant one direction has no exact match in the other.
- Test: a basic Set-then-lookup-both-directions case; the core regression -- `Rename` must remove the stale reverse entry for the old email; `Set`/`Rename` onto an email already owned by a different id are rejected and leave the index untouched; `Delete` removes both directions and is a no-op on a missing id; a 5000-iteration seeded random sequence of `Set`/`Rename`/`Delete` over a small pool of ids and emails, asserting `CheckInvariant` passes after every single operation; `Example` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why Rename is where this actually breaks, and how the invariant checker catches it

`Set` on a brand-new id is easy to get right: write both directions, done.
The trap is updating an id that already has an email on record. If `Set`
(or `Rename`, built on the same logic) only writes the *new* pair --
`idToEmail[id] = newEmail; emailToID[newEmail] = id` -- and never touches
the *old* email's entry, `emailToID` is left holding `oldEmail -> id` as a
stale leftover. `idToEmail` now correctly says `id`'s email is `newEmail`,
but `emailToID[oldEmail]` still says `id` too, which is simply false: `id`
does not own `oldEmail` anymore. This is not a cosmetic bug -- it means
`IDOf(oldEmail)` keeps returning `id` forever unless something else deletes
that specific email, and if a *different* user later legitimately claims
`oldEmail`, whichever write happened last wins the reverse map while the
loser's forward-map entry silently disagrees.

The fix is one extra line, in the right place: before writing the new pair,
check whether `id` already has an old email on record, and if so, delete
*that* email's reverse entry first. `setLocked` in this module does exactly
that -- `if old, ok := idToEmail[id]; ok && old != email { delete(emailToID, old) }`
-- and both `Set` and `Rename` funnel through it, so there is exactly one
place this logic can be forgotten, not two.

Proving the fix actually holds needs more than a couple of hand-picked
cases, because the bug only shows up on the *second* write to the same id --
a test that only ever calls `Set` once per id would never exercise it at
all. `CheckInvariant` is the general-purpose proof: it walks `idToEmail`
end to end and confirms every entry has a matching, correctly-pointing
partner in `emailToID`, then does the same walk in reverse, and it also
checks the two maps have the same length (a size mismatch alone proves an
entry was orphaned on one side). Running `CheckInvariant` after every single
operation in a long, seeded random sequence -- rather than only at the end --
is what pins down *which* operation broke things the moment it happens,
which is exactly how a real regression test for this class of bug should be
built.

Create `biindex.go`:

```go
// Package biindex maintains a bidirectional userID<->email index and keeps
// both directions exact inverses of each other across inserts, renames, and
// deletes.
package biindex

import (
	"errors"
	"fmt"
	"sync"
)

// ErrNotFound is returned by Rename when the id is not present.
var ErrNotFound = errors.New("biindex: id not found")

// ErrEmailTaken is returned when the requested email is already bound to a
// different id. The index is left unchanged.
var ErrEmailTaken = errors.New("biindex: email already bound to a different id")

// Index is a bidirectional userID<->email index backed by two maps. Every
// exported method that mutates the index keeps idToEmail and emailToID
// exact inverses: for every id->email pair there is exactly one matching
// email->id pair, and vice versa. The trap this guards against is a rename
// that updates the forward map but forgets to delete the now-stale reverse
// entry, which would let two different ids appear to own the same email.
//
// Index is safe for concurrent use by multiple goroutines.
type Index struct {
	mu        sync.Mutex
	idToEmail map[string]string
	emailToID map[string]string
}

// New returns an empty Index.
func New() *Index {
	return &Index{
		idToEmail: make(map[string]string),
		emailToID: make(map[string]string),
	}
}

// Set creates a new id->email association, or updates id's email if id is
// already present. It fails with ErrEmailTaken, leaving the index
// unchanged, if email is already bound to a different id.
func (idx *Index) Set(id, email string) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	return idx.setLocked(id, email)
}

// setLocked performs the actual insert-or-update. It must run under idx.mu.
func (idx *Index) setLocked(id, email string) error {
	if owner, ok := idx.emailToID[email]; ok && owner != id {
		return ErrEmailTaken
	}
	if old, ok := idx.idToEmail[id]; ok && old != email {
		// The reverse entry for the old email is now stale; remove it
		// before writing the new pair, or the invariant breaks.
		delete(idx.emailToID, old)
	}
	idx.idToEmail[id] = email
	idx.emailToID[email] = id
	return nil
}

// Rename changes the email on record for an existing id. It fails with
// ErrNotFound if id is absent, and with ErrEmailTaken if newEmail is
// already bound to a different id; in either failure case the index is
// left unchanged.
func (idx *Index) Rename(id, newEmail string) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if _, ok := idx.idToEmail[id]; !ok {
		return ErrNotFound
	}
	return idx.setLocked(id, newEmail)
}

// Delete removes id and its associated email. It is a no-op if id is not
// present.
func (idx *Index) Delete(id string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	email, ok := idx.idToEmail[id]
	if !ok {
		return
	}
	delete(idx.idToEmail, id)
	delete(idx.emailToID, email)
}

// EmailOf returns the email on record for id, and whether id is present.
func (idx *Index) EmailOf(id string) (string, bool) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	e, ok := idx.idToEmail[id]
	return e, ok
}

// IDOf returns the id on record for email, and whether email is present.
func (idx *Index) IDOf(email string) (string, bool) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	id, ok := idx.emailToID[email]
	return id, ok
}

// Len returns the number of id<->email pairs currently indexed.
func (idx *Index) Len() int {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	return len(idx.idToEmail)
}

// CheckInvariant walks both maps and returns a descriptive error the
// instant it finds an entry in one map without an exact inverse in the
// other, or a size mismatch between the two maps. Tests call it after any
// sequence of Set/Rename/Delete calls to prove the index never drifted out
// of sync.
func (idx *Index) CheckInvariant() error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	if len(idx.idToEmail) != len(idx.emailToID) {
		return fmt.Errorf("biindex: size mismatch: idToEmail has %d entries, emailToID has %d",
			len(idx.idToEmail), len(idx.emailToID))
	}
	for id, email := range idx.idToEmail {
		back, ok := idx.emailToID[email]
		if !ok {
			return fmt.Errorf("biindex: idToEmail[%q]=%q has no reverse entry in emailToID", id, email)
		}
		if back != id {
			return fmt.Errorf("biindex: idToEmail[%q]=%q but emailToID[%q]=%q, not an inverse", id, email, email, back)
		}
	}
	for email, id := range idx.emailToID {
		back, ok := idx.idToEmail[id]
		if !ok {
			return fmt.Errorf("biindex: emailToID[%q]=%q has no reverse entry in idToEmail", email, id)
		}
		if back != email {
			return fmt.Errorf("biindex: emailToID[%q]=%q but idToEmail[%q]=%q, not an inverse", email, id, id, back)
		}
	}
	return nil
}
```

### Using it

`New` returns an empty `Index`; every mutation from then on goes through
`Set`, `Rename`, or `Delete`, and every one of them leaves the index in a
state where `CheckInvariant` passes -- that is the contract a caller can
rely on without re-deriving it from the implementation. `Index` is safe for
concurrent use by multiple goroutines, guarded internally by a single
`sync.Mutex`, so a directory service can share one `Index` across every
request handler without adding a lock of its own. Neither `EmailOf` nor
`IDOf` returns anything that could alias internal state -- both return
plain `string` values, copied out, so there is no aliasing contract beyond
the ordinary comma-ok shape both already document.

The module has no `main.go`, because a bidirectional index is a data
structure you embed in a service, not a tool you run standalone. Its
executable demonstration is `Example`: `go test` runs it and compares its
standard output against the `// Output:` comment, so the usage shown below
cannot drift away from the code. It sets one user's email, renames it, and
confirms the old email no longer resolves to anyone -- the exact regression
this module is built around.

### Tests

`TestSetAndLookupBothDirections` and `TestSetEmailTakenByAnotherIDRejected`
cover the basic `Set` contract, including that a rejected write leaves the
index untouched. `TestRenameRemovesStaleReverseEntry` is the module's core
regression test: it renames an id's email and asserts the *old* email no
longer resolves to anyone, which is exactly the bug a forgetful `Rename`
implementation would fail. `TestRenameNotFound` and
`TestRenameToTakenEmailRejected` cover `Rename`'s two failure modes.
`TestDeleteRemovesBothDirections` and `TestDeleteMissingIsNoop` cover
deletion. `TestRandomizedOperationsPreserveInvariant` is the broad proof: a
seeded 5000-iteration sequence of random `Set`/`Rename`/`Delete` calls over
a small, deliberately collision-prone pool of five ids and five emails,
calling `CheckInvariant` after every single call -- any code path that only
sometimes forgets to clean up a reverse entry gets caught within the first
few dozen iterations. `Example` closes the loop as the runnable
demonstration, printing both directions before and after a rename and
confirming the invariant still holds at the end.

Create `biindex_test.go`:

```go
package biindex

import (
	"errors"
	"fmt"
	"math/rand"
	"testing"
)

func TestSetAndLookupBothDirections(t *testing.T) {
	t.Parallel()

	idx := New()
	if err := idx.Set("u1", "alice@example.com"); err != nil {
		t.Fatalf("Set() = %v, want nil", err)
	}
	if email, ok := idx.EmailOf("u1"); !ok || email != "alice@example.com" {
		t.Fatalf("EmailOf(u1) = %q, %v, want alice@example.com, true", email, ok)
	}
	if id, ok := idx.IDOf("alice@example.com"); !ok || id != "u1" {
		t.Fatalf("IDOf(alice@example.com) = %q, %v, want u1, true", id, ok)
	}
	if err := idx.CheckInvariant(); err != nil {
		t.Fatalf("CheckInvariant() = %v, want nil", err)
	}
}

func TestSetEmailTakenByAnotherIDRejected(t *testing.T) {
	t.Parallel()

	idx := New()
	if err := idx.Set("u1", "alice@example.com"); err != nil {
		t.Fatalf("Set(u1) = %v, want nil", err)
	}
	err := idx.Set("u2", "alice@example.com")
	if !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("Set(u2, taken email) = %v, want ErrEmailTaken", err)
	}
	// The index must be untouched by the rejected write.
	if id, ok := idx.IDOf("alice@example.com"); !ok || id != "u1" {
		t.Fatalf("after rejected Set: IDOf(alice@example.com) = %q, %v, want u1, true", id, ok)
	}
	if _, ok := idx.EmailOf("u2"); ok {
		t.Fatal("after rejected Set: u2 should not have been created")
	}
}

func TestRenameRemovesStaleReverseEntry(t *testing.T) {
	t.Parallel()

	idx := New()
	if err := idx.Set("u1", "alice@example.com"); err != nil {
		t.Fatalf("Set(u1) = %v, want nil", err)
	}
	if err := idx.Rename("u1", "alice.new@example.com"); err != nil {
		t.Fatalf("Rename(u1) = %v, want nil", err)
	}

	// The old email must no longer resolve to anyone: the stale reverse
	// entry is exactly the bug this module guards against.
	if _, ok := idx.IDOf("alice@example.com"); ok {
		t.Fatal("stale reverse entry for old email still present after Rename")
	}
	if id, ok := idx.IDOf("alice.new@example.com"); !ok || id != "u1" {
		t.Fatalf("IDOf(new email) = %q, %v, want u1, true", id, ok)
	}
	if err := idx.CheckInvariant(); err != nil {
		t.Fatalf("CheckInvariant() = %v, want nil", err)
	}
}

func TestRenameNotFound(t *testing.T) {
	t.Parallel()

	idx := New()
	if err := idx.Rename("ghost", "ghost@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Rename(ghost) = %v, want ErrNotFound", err)
	}
}

func TestRenameToTakenEmailRejected(t *testing.T) {
	t.Parallel()

	idx := New()
	idx.Set("u1", "alice@example.com")
	idx.Set("u2", "bob@example.com")

	err := idx.Rename("u1", "bob@example.com")
	if !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("Rename(u1, taken email) = %v, want ErrEmailTaken", err)
	}
	// u1 must still hold its original email; nothing was changed.
	if email, _ := idx.EmailOf("u1"); email != "alice@example.com" {
		t.Fatalf("after rejected Rename: EmailOf(u1) = %q, want alice@example.com", email)
	}
	if err := idx.CheckInvariant(); err != nil {
		t.Fatalf("CheckInvariant() = %v, want nil", err)
	}
}

func TestDeleteRemovesBothDirections(t *testing.T) {
	t.Parallel()

	idx := New()
	idx.Set("u1", "alice@example.com")
	idx.Delete("u1")

	if _, ok := idx.EmailOf("u1"); ok {
		t.Fatal("EmailOf(u1) should report absent after Delete")
	}
	if _, ok := idx.IDOf("alice@example.com"); ok {
		t.Fatal("IDOf(alice@example.com) should report absent after Delete")
	}
	if got := idx.Len(); got != 0 {
		t.Fatalf("Len() = %d, want 0", got)
	}
}

func TestDeleteMissingIsNoop(t *testing.T) {
	t.Parallel()

	idx := New()
	idx.Set("u1", "alice@example.com")
	idx.Delete("ghost")

	if got := idx.Len(); got != 1 {
		t.Fatalf("Len() = %d, want 1 (unaffected by deleting a missing id)", got)
	}
}

// TestRandomizedOperationsPreserveInvariant runs a long, seeded (so
// reproducible) sequence of Set/Rename/Delete calls over a small pool of
// ids and emails -- deliberately small so collisions and re-renames of the
// same id are frequent -- and checks the bidirectional invariant after
// every single operation. A bug that only drops the reverse entry on some
// code paths (e.g. Rename forgetting to clear the old email) would corrupt
// the index within the first few dozen iterations here.
func TestRandomizedOperationsPreserveInvariant(t *testing.T) {
	t.Parallel()

	idx := New()
	rng := rand.New(rand.NewSource(42))
	ids := []string{"u1", "u2", "u3", "u4", "u5"}
	emails := []string{"a@x.com", "b@x.com", "c@x.com", "d@x.com", "e@x.com"}

	for i := 0; i < 5000; i++ {
		id := ids[rng.Intn(len(ids))]
		switch rng.Intn(3) {
		case 0:
			email := emails[rng.Intn(len(emails))]
			idx.Set(id, email) // ErrEmailTaken is a valid, expected outcome here
		case 1:
			email := emails[rng.Intn(len(emails))]
			idx.Rename(id, email) // ErrNotFound / ErrEmailTaken also expected
		case 2:
			idx.Delete(id)
		}
		if err := idx.CheckInvariant(); err != nil {
			t.Fatalf("iteration %d: invariant violated: %v", i, err)
		}
	}
}

// Example creates one user, renames their email, and shows that the old
// email's reverse entry is gone -- the exact regression this module guards
// against.
func Example() {
	idx := New()

	if err := idx.Set("u1", "alice@example.com"); err != nil {
		fmt.Println("Set error:", err)
		return
	}
	fmt.Println("after Set(u1, alice@example.com):")
	printLookup(idx, "u1", "alice@example.com")

	if err := idx.Rename("u1", "alice.new@example.com"); err != nil {
		fmt.Println("Rename error:", err)
		return
	}
	fmt.Println("after Rename(u1, alice.new@example.com):")
	printLookup(idx, "u1", "alice.new@example.com")

	if _, ok := idx.IDOf("alice@example.com"); ok {
		fmt.Println("BUG: stale reverse entry for the old email is still present")
	} else {
		fmt.Println("old email no longer resolves: reverse entry was cleaned up")
	}

	if err := idx.Set("u2", "alice.new@example.com"); err != nil {
		fmt.Println("Set(u2, taken email) rejected as expected:", err)
	}

	idx.Delete("u1")
	fmt.Println("after Delete(u1):")
	printLookup(idx, "u1", "alice.new@example.com")

	if err := idx.CheckInvariant(); err != nil {
		fmt.Println("invariant broken:", err)
	} else {
		fmt.Println("invariant holds: idToEmail and emailToID are exact inverses")
	}

	// Output:
	// after Set(u1, alice@example.com):
	//   EmailOf(u1) = "alice@example.com", true
	//   IDOf(alice@example.com) = "u1", true
	// after Rename(u1, alice.new@example.com):
	//   EmailOf(u1) = "alice.new@example.com", true
	//   IDOf(alice.new@example.com) = "u1", true
	// old email no longer resolves: reverse entry was cleaned up
	// Set(u2, taken email) rejected as expected: biindex: email already bound to a different id
	// after Delete(u1):
	//   EmailOf(u1) = "", false
	//   IDOf(alice.new@example.com) = "", false
	// invariant holds: idToEmail and emailToID are exact inverses
}

func printLookup(idx *Index, id, email string) {
	e, okE := idx.EmailOf(id)
	i, okI := idx.IDOf(email)
	fmt.Printf("  EmailOf(%s) = %q, %v\n", id, e, okE)
	fmt.Printf("  IDOf(%s) = %q, %v\n", email, i, okI)
}
```

## Review

The index is correct exactly when `CheckInvariant` returns nil after any
sequence of operations, and `TestRenameRemovesStaleReverseEntry` is the
specific test that would fail if `Rename` forgot to delete the old email's
reverse entry -- the exact bug this module is built around.
`TestRandomizedOperationsPreserveInvariant` broadens that single scenario
into a 5000-call stress test over a deliberately small id/email pool, so
renames and re-renames of the same id happen constantly and any code path
that only sometimes forgets to clean up gets exercised, not just the one
hand-picked case. The rejected-write tests matter just as much as the
success-path ones: `ErrEmailTaken` and `ErrNotFound` are only trustworthy
guarantees if a rejected `Set` or `Rename` genuinely leaves the index
untouched, which `TestSetEmailTakenByAnotherIDRejected` and
`TestRenameToTakenEmailRejected` both confirm directly. `Example` is the
executable documentation: `go test` verifies its output. Run
`go test -count=1 -race ./...` -- the mutex-guarded `Index` methods should
show no data races even though this exercise's tests do not themselves
spawn goroutines.

## Resources

- [maps.Keys](https://pkg.go.dev/maps#Keys) — background reading on ranging map contents; not used directly here but relevant to how `CheckInvariant` could be extended to report all violations instead of the first.
- [Go Specification: Map types](https://go.dev/ref/spec#Map_types) — the `delete` builtin and comma-ok lookup semantics both maps rely on.
- [errors.Is](https://pkg.go.dev/errors#Is) — how the tests distinguish `ErrEmailTaken` from `ErrNotFound`.
- [math/rand](https://pkg.go.dev/math/rand) — the seeded source behind the reproducible randomized operation sequence.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [17-deterministic-config-render-sorted-keys.md](17-deterministic-config-render-sorted-keys.md) | Next: [19-dependency-dag-cycle-detection.md](19-dependency-dag-cycle-detection.md)
