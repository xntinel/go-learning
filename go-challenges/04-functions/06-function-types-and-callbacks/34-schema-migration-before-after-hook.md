# Exercise 34: Schema Migration Pre/Post Callbacks with Function Types

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye concurrencia).

A migration runner needs to validate a change before it touches the
schema and reconcile against the result afterward — whether that result
is a new version or a failure — without either concern living inside the
migration's own `Up` function. This module builds that as `PreHook` and
`PostHook` callbacks around `Runner.Apply`, with the one detail that is
easy to get wrong done correctly: `Schema` contains a map, and a snapshot
taken by naive struct assignment would silently alias it instead of
freezing it.

## What you'll build

```text
migrator/                     independent module: example.com/schema-migration-before-after-hook
  go.mod                       go 1.24
  migrator.go                  type Schema, type Migration, type PreHook, type PostHook, type Runner: OnPre, OnPost, Apply
  cmd/
    demo/
      main.go                    runnable demo: a successful migration, then a failing one
  migrator_test.go               Up runs and advances version, pre-hook veto, post-hook snapshots (incl. no aliasing), post-hook on failure, sequential migrations, concurrency (-race)
```

Files: `migrator.go`, `cmd/demo/main.go`, `migrator_test.go`.
Implement: `type Schema struct { Version int; Tables map[string]int }` with a `clone` deep-copy, `type Migration struct { Name string; Up func(*Schema) error }`, `type PreHook func(current Schema, m Migration) error`, `type PostHook func(before, after Schema, m Migration, err error)`, and `Runner` with `OnPre(h)`, `OnPost(h)`, and `Apply(schema *Schema, m Migration) error`; a pre-hook error aborts before `Up` runs, `Apply` mutates the live `*Schema` only on success, and the post-hook always runs with independent, non-aliased `before`/`after` snapshots.
Test: a successful `Up` advances the schema and is observed by the post-hook; a rejecting pre-hook prevents `Up` from running and leaves the schema unchanged; the post-hook's `before`/`after` snapshots are correct and remain correct even after the live schema is mutated later (no map aliasing); the post-hook still runs when `Up` fails, with an unchanged `after` snapshot; several migrations applied in sequence accumulate the version correctly; concurrent hook registration alongside `Apply` is race-free.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/schema-migration-before-after-hook/cmd/demo
cd ~/go-exercises/schema-migration-before-after-hook
go mod init example.com/schema-migration-before-after-hook
go mod edit -go=1.24
```

### Why `Schema` needs `clone`, not `:=`

`Schema` has a `Tables map[string]int` field, and Go struct assignment —
`before := *schema`, or returning `Schema` by value — only copies the map
*header*, three words pointing at the same underlying buckets. Two
"independent" `Schema` values built that way still share every table
entry: mutate one's map and the other's map changes too, because there is
only ever one map. That would quietly break the entire premise of a
`PostHook` seeing a `before` snapshot from before the migration and an
`after` snapshot from after it — without a real copy, `before` would
retroactively become identical to `after` the instant `Up` mutated the
shared map, since they are, in fact, the same map. `clone()` fixes this
by allocating a fresh map and copying every key: `Apply` calls it to take
`before` from the live schema before anything runs, again to build the
`working` copy `Up` mutates (so a failing migration never touches the
live schema at all, only a scratch copy), and once more to freeze `after`
for the post-hooks once the outcome is known.
`TestPostHookSeesCorrectBeforeAndAfterSnapshots` proves this directly: it
mutates the live schema's map *after* `Apply` returns and asserts the
snapshot the post-hook already received did not change — which would
fail immediately if `clone` were replaced with a shallow struct copy.

Create `migrator.go`:

```go
// Package migrator applies schema migrations between Pre and Post
// callback hooks, so validation of an upcoming change and reconciliation
// against the resulting state both live outside the migration's own Up
// function.
package migrator

import (
	"fmt"
	"sync"
)

// Schema is a minimal, in-memory stand-in for a database schema: a
// version counter and a table-name-to-column-count map.
type Schema struct {
	Version int
	Tables  map[string]int
}

// clone returns a deep copy of s, since Tables is a map and Go's struct
// assignment only copies the map header, not its entries.
func (s Schema) clone() Schema {
	tables := make(map[string]int, len(s.Tables))
	for k, v := range s.Tables {
		tables[k] = v
	}
	return Schema{Version: s.Version, Tables: tables}
}

// Migration names a schema change and the function that applies it.
type Migration struct {
	Name string
	Up   func(s *Schema) error
}

// PreHook validates a migration against the schema as it stands right
// now, before Up runs. Returning an error aborts the migration.
type PreHook func(current Schema, m Migration) error

// PostHook observes a migration after it ran (or after a PreHook aborted
// it), seeing the schema snapshot from immediately before and
// immediately after, plus the final error (nil on success).
type PostHook func(before, after Schema, m Migration, err error)

// Runner holds the hooks shared by every migration applied through it.
type Runner struct {
	mu   sync.Mutex
	pre  []PreHook
	post []PostHook
}

// NewRunner returns an empty Runner.
func NewRunner() *Runner {
	return &Runner{}
}

// OnPre registers h to validate every migration before it runs.
func (r *Runner) OnPre(h PreHook) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pre = append(r.pre, h)
}

// OnPost registers h to observe every migration after it runs.
func (r *Runner) OnPost(h PostHook) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.post = append(r.post, h)
}

// Apply runs every PreHook against schema and m (any error aborts before
// Up runs), then runs m.Up on a working copy, commits it back into schema
// only on success, and finally runs every PostHook with the before/after
// snapshots and the resulting error.
func (r *Runner) Apply(schema *Schema, m Migration) (err error) {
	before := schema.clone()

	r.mu.Lock()
	pres := append([]PreHook(nil), r.pre...)
	posts := append([]PostHook(nil), r.post...)
	r.mu.Unlock()

	var after Schema
	defer func() {
		for _, h := range posts {
			h(before, after, m, err)
		}
	}()

	for _, h := range pres {
		if perr := h(before, m); perr != nil {
			err = fmt.Errorf("migration %q rejected by pre-hook: %w", m.Name, perr)
			after = before.clone()
			return err
		}
	}

	working := before.clone()
	if uerr := m.Up(&working); uerr != nil {
		err = fmt.Errorf("migration %q failed: %w", m.Name, uerr)
		after = before.clone()
		return err
	}

	*schema = working
	after = working.clone()
	return nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/schema-migration-before-after-hook"
)

func main() {
	schema := migrator.Schema{Version: 1, Tables: map[string]int{"users": 3}}

	r := migrator.NewRunner()
	r.OnPre(func(current migrator.Schema, m migrator.Migration) error {
		fmt.Printf("pre-check: %q against version %d\n", m.Name, current.Version)
		return nil
	})
	r.OnPost(func(before, after migrator.Schema, m migrator.Migration, err error) {
		if err != nil {
			fmt.Printf("post: %q failed: %v (still at version %d)\n", m.Name, err, after.Version)
			return
		}
		fmt.Printf("post: %q applied, version %d -> %d\n", m.Name, before.Version, after.Version)
	})

	addColumn := migrator.Migration{
		Name: "add-users-email",
		Up: func(s *migrator.Schema) error {
			s.Tables["users"]++
			s.Version++
			return nil
		},
	}
	if err := r.Apply(&schema, addColumn); err != nil {
		fmt.Println("error:", err)
	}

	dropTable := migrator.Migration{
		Name: "drop-legacy-table",
		Up: func(s *migrator.Schema) error {
			return errors.New("legacy table still referenced by a view")
		},
	}
	if err := r.Apply(&schema, dropTable); err != nil {
		fmt.Println("error:", err)
	}

	fmt.Printf("final schema: version=%d users-columns=%d\n", schema.Version, schema.Tables["users"])
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
pre-check: "add-users-email" against version 1
post: "add-users-email" applied, version 1 -> 2
pre-check: "drop-legacy-table" against version 2
post: "drop-legacy-table" failed: migration "drop-legacy-table" failed: legacy table still referenced by a view (still at version 2)
error: migration "drop-legacy-table" failed: legacy table still referenced by a view
final schema: version=2 users-columns=4
```

### Tests

Create `migrator_test.go`:

```go
package migrator

import (
	"errors"
	"sync"
	"testing"
)

func newSchema() Schema {
	return Schema{Version: 1, Tables: map[string]int{"users": 2}}
}

func TestApplyRunsUpAndAdvancesVersion(t *testing.T) {
	t.Parallel()
	r := NewRunner()
	schema := newSchema()
	m := Migration{Name: "add-col", Up: func(s *Schema) error {
		s.Tables["users"]++
		s.Version++
		return nil
	}}

	if err := r.Apply(&schema, m); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if schema.Version != 2 {
		t.Fatalf("Version = %d, want 2", schema.Version)
	}
	if schema.Tables["users"] != 3 {
		t.Fatalf("Tables[users] = %d, want 3", schema.Tables["users"])
	}
}

func TestPreHookRejectingPreventsUpFromRunning(t *testing.T) {
	t.Parallel()
	r := NewRunner()
	r.OnPre(func(current Schema, m Migration) error {
		return errors.New("policy: no drops on Fridays")
	})
	schema := newSchema()
	upRan := false
	m := Migration{Name: "drop-table", Up: func(s *Schema) error {
		upRan = true
		return nil
	}}

	err := r.Apply(&schema, m)
	if err == nil {
		t.Fatal("expected an error from the pre-hook rejection")
	}
	if upRan {
		t.Fatal("Up ran despite the pre-hook rejecting the migration")
	}
	if schema.Version != 1 {
		t.Fatalf("schema.Version = %d, want unchanged 1", schema.Version)
	}
}

func TestPostHookSeesCorrectBeforeAndAfterSnapshots(t *testing.T) {
	t.Parallel()
	r := NewRunner()
	var gotBefore, gotAfter Schema
	var gotErr error
	r.OnPost(func(before, after Schema, m Migration, err error) {
		gotBefore, gotAfter, gotErr = before, after, err
	})

	schema := newSchema()
	m := Migration{Name: "add-col", Up: func(s *Schema) error {
		s.Tables["users"]++
		s.Version++
		return nil
	}}
	if err := r.Apply(&schema, m); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if gotErr != nil {
		t.Fatalf("post-hook saw err = %v, want nil", gotErr)
	}
	if gotBefore.Version != 1 || gotBefore.Tables["users"] != 2 {
		t.Fatalf("before snapshot = %+v, want version=1 users=2", gotBefore)
	}
	if gotAfter.Version != 2 || gotAfter.Tables["users"] != 3 {
		t.Fatalf("after snapshot = %+v, want version=2 users=3", gotAfter)
	}

	// Mutating the live schema after Apply must not retroactively change
	// the snapshots the post-hook already received (proves clone(), not
	// map aliasing).
	schema.Tables["users"] = 999
	if gotAfter.Tables["users"] != 3 {
		t.Fatalf("after snapshot mutated via alias: got %d, want 3", gotAfter.Tables["users"])
	}
}

func TestPostHookRunsWhenUpFailsWithUnchangedAfterSnapshot(t *testing.T) {
	t.Parallel()
	r := NewRunner()
	var gotAfter Schema
	var gotErr error
	postCalled := false
	r.OnPost(func(before, after Schema, m Migration, err error) {
		postCalled = true
		gotAfter = after
		gotErr = err
	})

	schema := newSchema()
	failErr := errors.New("constraint violation")
	m := Migration{Name: "bad-migration", Up: func(s *Schema) error {
		return failErr
	}}

	err := r.Apply(&schema, m)
	if !errors.Is(err, failErr) {
		t.Fatalf("Apply err = %v, want wrapping %v", err, failErr)
	}
	if !postCalled {
		t.Fatal("post-hook did not run for a failed migration")
	}
	if !errors.Is(gotErr, failErr) {
		t.Fatalf("post-hook err = %v, want wrapping %v", gotErr, failErr)
	}
	if gotAfter.Version != 1 || gotAfter.Tables["users"] != 2 {
		t.Fatalf("after snapshot on failure = %+v, want unchanged version=1 users=2", gotAfter)
	}
	if schema.Version != 1 {
		t.Fatalf("live schema.Version = %d, want unchanged 1", schema.Version)
	}
}

func TestSequentialMigrationsAccumulateVersion(t *testing.T) {
	t.Parallel()
	r := NewRunner()
	schema := newSchema()

	bump := func(name string) Migration {
		return Migration{Name: name, Up: func(s *Schema) error {
			s.Version++
			return nil
		}}
	}

	for _, name := range []string{"m1", "m2", "m3"} {
		if err := r.Apply(&schema, bump(name)); err != nil {
			t.Fatalf("Apply(%s): %v", name, err)
		}
	}
	if schema.Version != 4 {
		t.Fatalf("Version = %d, want 4", schema.Version)
	}
}

func TestConcurrentHookRegistrationAndApplyIsRaceFree(t *testing.T) {
	t.Parallel()
	r := NewRunner()
	var mu sync.Mutex
	postCount := 0

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				r.OnPre(func(current Schema, m Migration) error { return nil })
			} else {
				r.OnPost(func(before, after Schema, m Migration, err error) {
					mu.Lock()
					postCount++
					mu.Unlock()
				})
			}
		}(i)
	}
	wg.Wait()

	schema := newSchema()
	noop := Migration{Name: "noop", Up: func(s *Schema) error { return nil }}
	if err := r.Apply(&schema, noop); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}
```

## Review

`Apply` is correct when the live `*Schema` only ever reflects a migration
that both passed every pre-hook and ran `Up` successfully — never a
partially-applied one. `TestPreHookRejectingPreventsUpFromRunning` proves
the first half, and `TestPostHookRunsWhenUpFailsWithUnchangedAfterSnapshot`
proves the second: `Up` mutates a scratch `working` copy, not the live
schema, so a failing migration leaves `*schema` byte-for-byte as it found
it, and the post-hook's `after` snapshot reflects that same unchanged
state rather than whatever `Up` half-wrote before failing.
`TestPostHookSeesCorrectBeforeAndAfterSnapshots` is the test that would
catch the map-aliasing bug described above — it deliberately mutates the
live schema's map after `Apply` returns and checks the already-delivered
snapshot did not move, which only holds because `clone()` allocates a
genuinely separate map every time. The concurrency test is scoped to what
actually needs proving: that registering hooks concurrently with an
`Apply` in flight cannot corrupt the `Runner`'s own slices, the same
copy-under-lock discipline used by the transaction hooks and FSM
callbacks earlier in this chapter.

## Resources

- [Go Specification: Function types](https://go.dev/ref/spec#Function_types)
- [golang-migrate/migrate (a production Go migration tool)](https://github.com/golang-migrate/migrate)
- [Go Specification: Assignability (why map/slice copies alias)](https://go.dev/ref/spec#Assignability)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [33-resource-factory-lifecycle-callback.md](33-resource-factory-lifecycle-callback.md) | Next: [35-tracing-context-propagator-decorator.md](35-tracing-context-propagator-decorator.md)
