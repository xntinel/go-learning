# Exercise 2: The Shallow-Clone Trap — Why maps.Clone Is Not a Safe Snapshot

A repository that hands data to callers owes them a defensive copy, and the
tempting one-liner is `maps.Clone(byID)` or `slices.Clone(items)`. For a store of
pointers, that clone is a lie: it duplicates the container but shares every
pointee. This module builds both snapshot methods side by side and makes the
leakage executable.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
snapclone/                    independent module: example.com/snapclone
  go.mod                      go 1.24
  store.go                    Store{byID map[string]*Job}; SnapshotShared (shallow) vs SnapshotDeep (values)
  store_test.go               proves shallow clone aliases, deep snapshot is independent
  cmd/demo/main.go            runnable demo contrasting the two snapshots
```

Files: `store.go`, `store_test.go`, `cmd/demo/main.go`.
Implement: a `Store` over `map[string]*Job`; `SnapshotShared() map[string]*Job`
returning `maps.Clone(byID)`; `SnapshotDeep() map[string]Job` built by
dereferencing each `*Job`.
Test: mutating through the shared clone changes the store; mutating a deep-snapshot
value does not.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/snapclone/cmd/demo
cd ~/go-exercises/snapclone
go mod init example.com/snapclone
```

### What "shallow" means, concretely

`maps.Clone` is declared `func Clone[M ~map[K]V, K comparable, V any](m M) M`. It
allocates a new map and copies each key/value pair by assignment. When `V` is
`*Job`, the value that gets assigned into the new map is the *pointer* — the same
address the original map held. So `SnapshotShared()` returns a genuinely new map
you can add and delete keys on without touching the store, but every value in it
points at the store's live `Job`. Write `snap[id].Status = StatusFailed` through
that clone and you have written into the store. This is exactly the false sense of
safety that ships bugs: the method is named `Snapshot`, it calls a `Clone`
function, and it still aliases everything that matters.

`SnapshotDeep()` returns `map[string]Job` — values, not pointers. It walks the
store and does `out[id] = *j`, copying each `Job` by value into the result. Now
the caller holds independent structs; mutating `snap[id]` (a value) does not, and
cannot, reach the store. For a flat struct with no nested reference fields this is
a complete defensive copy. (When the struct contains a nested slice or map, even
this value copy shares the nested backing array — that deeper trap is Exercise 10.
Here the `Job` fields are all value types, so `*j` fully severs it.)

Create `store.go`:

```go
package snapclone

import (
	"maps"
	"sync"
	"time"
)

type Status string

const (
	StatusPending Status = "pending"
	StatusRunning Status = "running"
	StatusFailed  Status = "failed"
)

// Job has only value-typed fields, so *j is a complete copy of it.
type Job struct {
	ID        string
	Status    Status
	UpdatedAt time.Time
}

// Store keeps jobs by pointer for in-place mutation.
type Store struct {
	mu   sync.RWMutex
	byID map[string]*Job
}

func NewStore() *Store {
	return &Store{byID: make(map[string]*Job)}
}

// Put stores a job by pointer.
func (s *Store) Put(j *Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID[j.ID] = j
}

// Status reports a job's current status (reads through the stored pointer).
func (s *Store) Status(id string) Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if j, ok := s.byID[id]; ok {
		return j.Status
	}
	return ""
}

// SnapshotShared is the TRAP: maps.Clone copies the map but every value is the
// same *Job the store holds. Mutating through the result mutates the store.
func (s *Store) SnapshotShared() map[string]*Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return maps.Clone(s.byID)
}

// SnapshotDeep dereferences each *Job into a value, severing the alias. The
// returned map is fully independent of the store.
func (s *Store) SnapshotDeep() map[string]Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]Job, len(s.byID))
	for id, j := range s.byID {
		out[id] = *j
	}
	return out
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/snapclone"
)

func main() {
	s := snapclone.NewStore()
	s.Put(&snapclone.Job{ID: "j1", Status: snapclone.StatusPending})

	// Shallow clone: mutation leaks into the store.
	shared := s.SnapshotShared()
	shared["j1"].Status = snapclone.StatusRunning
	fmt.Printf("after shared mutation: store=%s\n", s.Status("j1"))

	// Deep snapshot: mutation stays local.
	deep := s.SnapshotDeep()
	j := deep["j1"]
	j.Status = snapclone.StatusFailed
	deep["j1"] = j
	fmt.Printf("after deep mutation:   store=%s\n", s.Status("j1"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
after shared mutation: store=running
after deep mutation:   store=running
```

### Tests

`TestShallowCloneAliasesUnderlying` documents the hazard as a passing assertion:
mutate through the shared clone, and the store *did* change.
`TestDeepSnapshotIsIndependent` proves the safe path. The table test runs both
contrasting cases so the difference is executable, not prose.

Create `store_test.go`:

```go
package snapclone

import "testing"

func TestShallowCloneAliasesUnderlying(t *testing.T) {
	t.Parallel()

	s := NewStore()
	s.Put(&Job{ID: "j1", Status: StatusPending})

	shared := s.SnapshotShared()
	shared["j1"].Status = StatusRunning // writes through the shared pointer

	if got := s.Status("j1"); got != StatusRunning {
		t.Fatalf("store status = %q, want running (shallow clone should alias)", got)
	}
}

func TestDeepSnapshotIsIndependent(t *testing.T) {
	t.Parallel()

	s := NewStore()
	s.Put(&Job{ID: "j1", Status: StatusPending})

	deep := s.SnapshotDeep()
	j := deep["j1"]
	j.Status = StatusFailed
	deep["j1"] = j

	if got := s.Status("j1"); got != StatusPending {
		t.Fatalf("store status = %q, want pending (deep snapshot must not alias)", got)
	}
}

func TestSnapshotAliasingContrast(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		mutate      func(s *Store)
		wantChanged bool
	}{
		{
			name: "shared clone leaks",
			mutate: func(s *Store) {
				s.SnapshotShared()["j1"].Status = StatusRunning
			},
			wantChanged: true,
		},
		{
			name: "deep snapshot isolates",
			mutate: func(s *Store) {
				d := s.SnapshotDeep()
				j := d["j1"]
				j.Status = StatusRunning
				d["j1"] = j
			},
			wantChanged: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := NewStore()
			s.Put(&Job{ID: "j1", Status: StatusPending})
			tc.mutate(s)
			changed := s.Status("j1") != StatusPending
			if changed != tc.wantChanged {
				t.Fatalf("changed = %v, want %v", changed, tc.wantChanged)
			}
		})
	}
}
```

## Review

The trap is that `Snapshot` sounds like isolation while `maps.Clone` delivers
only container isolation. `TestShallowCloneAliasesUnderlying` passing on the
assertion `store == running` is the whole lesson: the "copy" wrote through into the
store. `SnapshotDeep` is correct precisely because `out[id] = *j` copies the struct
by value; `TestDeepSnapshotIsIndependent` confirms the store stays `pending` after
the caller mutates its copy. The rule to carry forward: `slices.Clone` and
`maps.Clone` isolate the *container*, never the *pointees*. A defensive snapshot of
a pointer store must dereference to values (or deep-copy nested reference fields,
per Exercise 10). Anything less hands a caller a live handle wearing a copy's name.

## Resources

- [`maps.Clone`](https://pkg.go.dev/maps#Clone) — the doc states it is a shallow clone: values are copied using assignment.
- [`slices.Clone`](https://pkg.go.dev/slices#Clone) — same shallow contract for slices.
- [Go Blog: Arrays, slices (and strings)](https://go.dev/blog/slices) — how element assignment copies headers and pointers, not pointees.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-slice-of-values-addressability.md](03-slice-of-values-addressability.md)
