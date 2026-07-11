# Exercise 6: Prune Expired Sessions in Place Without Leaking Pointers

A session table stored as `[]*Session` is pruned in place with
`slices.DeleteFunc`. Beyond shifting survivors down, `DeleteFunc` zeroes the freed
tail of the backing array — and with pointer elements that zeroing is exactly what
stops a removed `*Session` from staying reachable and leaking on the heap. This
exercise builds the pruner, proves survivors and order are correct, and inspects
the backing array's tail to pin the no-leak contract against a manual shift that
forgets to nil.

Self-contained module: own `go mod init`, own demo, own tests.

## What you'll build

```text
sessions/                  independent module: example.com/sessions
  go.mod                   go 1.26
  sessions.go              type Session; Prune (DeleteFunc), pruneManualLeaky (bug)
  cmd/
    demo/
      main.go              prune a mixed table, print survivors
  sessions_test.go         survivors+order, tail-zeroed contract, manual-leak negative
```

Files: `sessions.go`, `cmd/demo/main.go`, `sessions_test.go`.
Implement: `Prune(sessions, now)` via `slices.DeleteFunc`; a `pruneManualLeaky` doing the compaction by hand without niling the tail.
Test: prune a mix of expired/live, assert survivors and order and shrunk length; inspect the backing tail to confirm freed slots are `nil`; a negative sub-test shows the manual shift leaves dangling pointers.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/sessions/cmd/demo
cd ~/go-exercises/sessions
go mod init example.com/sessions
```

### Why the freed tail must be zeroed

Removing elements from a slice in place is a compaction: shift the survivors down
to fill the gaps and return a shorter slice. The manual idiom for deleting element
`i` is `copy(s[i:], s[i+1:]); s = s[:len(s)-1]` — `copy` acts like `memmove` and
slides the tail left one slot. For a slice of values (`[]int`) that is the whole
story. For a slice of *pointers* (`[]*Session`) it is not: after the shift, the
original last slot still holds a copy of a pointer to a session that is no longer
logically in the table. The table's length hides it, but the backing array still
references it, so the garbage collector cannot free that `*Session` (and whatever
it retains — buffers, connections). That is a heap leak that grows with every
prune, and it is invisible until you profile.

`slices.DeleteFunc` avoids it. Since Go 1.22 it shifts survivors *and* zeroes the
elements between the new length and the original length. For pointer element types
that zeroing nils the dangling tail slots, so removed sessions become unreachable
and collectable. This is why you prefer `slices.DeleteFunc` over a hand-rolled
compaction for pointer-bearing slices: the standard-library version does not forget
the step that a manual loop almost always omits. If you must shift by hand, you
have to nil the tail yourself (`clear(s[newLen:])`).

Create `sessions.go`:

```go
package sessions

import (
	"slices"
	"time"
)

// Session is a live connection/auth session with an expiry instant.
type Session struct {
	ID        string
	ExpiresAt time.Time
}

func (s *Session) expired(now time.Time) bool {
	return !now.Before(s.ExpiresAt)
}

// Prune removes expired sessions in place, keeping survivor order, and returns
// the shortened slice. DeleteFunc zeroes the freed tail so removed *Session
// values do not stay reachable through the backing array.
func Prune(sessions []*Session, now time.Time) []*Session {
	return slices.DeleteFunc(sessions, func(s *Session) bool {
		return s.expired(now)
	})
}

// pruneManualLeaky compacts by hand but forgets to nil the freed tail, so the
// backing array keeps dangling pointers to removed sessions. Used only in tests.
func pruneManualLeaky(sessions []*Session, now time.Time) []*Session {
	w := 0
	for _, s := range sessions {
		if !s.expired(now) {
			sessions[w] = s
			w++
		}
	}
	return sessions[:w] // tail [w:] still holds old pointers
}
```

### The runnable demo

The demo builds a table with a mix of live and expired sessions relative to a
fixed "now", prunes it, and prints the survivors in order.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/sessions"
)

func main() {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	mk := func(id string, mins int) *sessions.Session {
		return &sessions.Session{ID: id, ExpiresAt: now.Add(time.Duration(mins) * time.Minute)}
	}

	table := []*sessions.Session{
		mk("a", 10),  // live
		mk("b", -5),  // expired
		mk("c", 30),  // live
		mk("d", -1),  // expired
		mk("e", -20), // expired
	}

	table = sessions.Prune(table, now)
	for _, s := range table {
		fmt.Println(s.ID)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
a
c
```

### Tests

`TestPruneKeepsLiveInOrder` asserts survivors, their order, and the shrunk length.
`TestPruneZeroesFreedTail` holds the original slice header and inspects the backing
array's tail after pruning, asserting the freed slots are `nil` — the no-leak
contract. `TestManualShiftLeaksWithoutNiling` runs the buggy manual compaction and
asserts the tail still holds a non-nil pointer to a removed session, making the
leak explicit.

Create `sessions_test.go`:

```go
package sessions

import (
	"testing"
	"time"
)

var now = time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)

func mk(id string, mins int) *Session {
	return &Session{ID: id, ExpiresAt: now.Add(time.Duration(mins) * time.Minute)}
}

func mixedTable() []*Session {
	return []*Session{
		mk("a", 10),  // live
		mk("b", -5),  // expired
		mk("c", 30),  // live
		mk("d", -1),  // expired
		mk("e", -20), // expired
	}
}

func TestPruneKeepsLiveInOrder(t *testing.T) {
	t.Parallel()

	pruned := Prune(mixedTable(), now)
	if len(pruned) != 2 {
		t.Fatalf("len after prune = %d, want 2", len(pruned))
	}
	want := []string{"a", "c"}
	for i, id := range want {
		if pruned[i].ID != id {
			t.Fatalf("survivor[%d] = %q, want %q", i, pruned[i].ID, id)
		}
	}
}

func TestPruneZeroesFreedTail(t *testing.T) {
	t.Parallel()

	table := mixedTable()
	orig := table // same backing array
	origLen := len(table)

	pruned := Prune(table, now)

	// Freed tail slots [len(pruned):origLen] must be nil so removed sessions
	// are collectable.
	for i := len(pruned); i < origLen; i++ {
		if orig[i] != nil {
			t.Fatalf("freed tail slot %d not zeroed: %#v", i, orig[i])
		}
	}
}

func TestManualShiftLeaksWithoutNiling(t *testing.T) {
	t.Parallel()

	table := mixedTable()
	orig := table
	origLen := len(table)

	pruned := pruneManualLeaky(table, now)

	// The manual shift left dangling pointers in the tail: at least one freed
	// slot still references a removed session.
	leaked := false
	for i := len(pruned); i < origLen; i++ {
		if orig[i] != nil {
			leaked = true
		}
	}
	if !leaked {
		t.Fatal("expected manual shift to leak dangling pointers in the tail")
	}
}
```

## Review

The pruner is correct when survivors keep their order and the freed tail is
zeroed: `TestPruneKeepsLiveInOrder` pins the first, `TestPruneZeroesFreedTail` pins
the second by reaching through the original header into the shared backing array
and asserting the vacated slots are `nil`. The negative
`TestManualShiftLeaksWithoutNiling` shows what a hand-rolled compaction leaves
behind — live pointers to removed sessions — which is precisely the heap leak
`slices.DeleteFunc` prevents. The takeaway: for value slices a manual `copy`-shift
is fine, but for pointer or pointer-containing element types, deleting in place
must nil the freed tail, and `slices.Delete`/`slices.DeleteFunc` do that for you.

## Resources

- [slices package (`Delete`, `DeleteFunc`)](https://pkg.go.dev/slices#DeleteFunc)
- [Go 1.22 release notes (slices Delete zeroing)](https://go.dev/doc/go1.22)
- [`clear` builtin](https://pkg.go.dev/builtin#clear)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-scanner-token-must-be-copied.md](05-scanner-token-must-be-copied.md) | Next: [07-metrics-batcher-flush-and-reuse.md](07-metrics-batcher-flush-and-reuse.md)
