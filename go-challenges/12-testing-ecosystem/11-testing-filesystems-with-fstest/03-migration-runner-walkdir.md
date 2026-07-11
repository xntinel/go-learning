# Exercise 3: SQL migration discovery via fs.WalkDir over an embedded tree

A migration runner (golang-migrate, goose, or a hand-rolled one) starts by
discovering the migration files: scan a directory, pick the `*.up.sql` files,
parse the version from each name, sort by version, reject duplicates. This
exercise builds that discovery step over an `fs.FS` with `fs.WalkDir`, so it is
tested entirely in memory with a nested `MapFS` and wired to an `embed.FS` in
production.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
migrations/                  independent module: example.com/migrations
  go.mod                     go 1.26
  discover.go                Migration; ErrDuplicateVersion; CollectMigrations(fs.FS, root)
  cmd/
    demo/
      main.go                walk a MapFS migrations tree and print the ordered plan
  discover_test.go           ordering, filtering, duplicate, and walk-error tests
```

- Files: `discover.go`, `cmd/demo/main.go`, `discover_test.go`.
- Implement: `CollectMigrations(fsys fs.FS, root string) ([]Migration, error)`
  that walks with `fs.WalkDir`, selects `*.up.sql` files, parses the leading
  integer version, rejects duplicate versions, and returns migrations sorted by
  version.
- Test: `MapFS` fixtures asserting version-sorted output, ignored non-`.sql`
  files, a duplicate-version error, and walk-error propagation via a fault FS.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/migrations/cmd/demo
cd ~/go-exercises/migrations
go mod init example.com/migrations
```

### Why WalkDir, and where the errors hide

`fs.WalkDir` walks the tree in lexical order and hands the callback a
`fs.DirEntry` per node — the cheap variant that carries name and type without a
per-entry `Stat`, which matters when a real migrations tree has hundreds of
files. The callback does the selection: skip directories, skip anything that is
not `*.up.sql`, and for a match parse the leading integer version out of the
filename (`0007_add_index.up.sql` -> `7`). The two things that separate a real
discovery function from a toy are both about *rejecting bad input loudly*.

First, the version namespace must be a set: two files claiming version 7 is an
operator error that would make the applied order ambiguous, so a duplicate
version returns a descriptive `ErrDuplicateVersion` rather than silently keeping
one. Second — the subtle one — the callback's third argument is an error. When
`WalkDir` cannot read a directory (permission denied, I/O failure), it does not
abort on its own; it calls the callback with a non-nil `err`, and it is the
callback's job to return it. A callback that ignores that argument turns a
permission failure deep in the tree into a *silently incomplete* migration plan —
the worst possible outcome for a migration runner, because it might skip a
migration and report success. So the very first line of the callback checks
`err` and returns it.

Sorting is explicit: `WalkDir` visits in lexical (name) order, but migration
order is *numeric* version order, and `0010` sorts before `0002`
lexically-with-different-widths only by luck of zero-padding. Never rely on walk
order for semantics — collect, then `slices.SortFunc` by version.

Create `discover.go`:

```go
package migrations

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"
)

// ErrDuplicateVersion signals two migration files claiming the same version.
var ErrDuplicateVersion = errors.New("duplicate migration version")

// Migration is one discovered up-migration file.
type Migration struct {
	Version int
	Name    string // base filename, e.g. "0007_add_index.up.sql"
	Path    string // full path within the walked FS
}

// CollectMigrations walks root in fsys, selecting *.up.sql files, parsing the
// leading integer version from each name, rejecting duplicate versions, and
// returning the migrations sorted by version.
func CollectMigrations(fsys fs.FS, root string) ([]Migration, error) {
	var found []Migration
	seen := make(map[int]string)

	err := fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err // propagate a directory-read failure, do not swallow it
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".up.sql") {
			return nil
		}
		version, err := parseVersion(d.Name())
		if err != nil {
			return fmt.Errorf("migration %s: %w", p, err)
		}
		if prev, dup := seen[version]; dup {
			return fmt.Errorf("%w: %d claimed by %s and %s", ErrDuplicateVersion, version, prev, p)
		}
		seen[version] = p
		found = append(found, Migration{Version: version, Name: d.Name(), Path: p})
		return nil
	})
	if err != nil {
		return nil, err
	}

	slices.SortFunc(found, func(a, b Migration) int {
		return a.Version - b.Version
	})
	return found, nil
}

// parseVersion reads the leading integer prefix of a migration filename.
func parseVersion(name string) (int, error) {
	base := path.Base(name)
	digits := base
	if i := strings.IndexFunc(base, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
		digits = base[:i]
	}
	if digits == "" {
		return 0, fmt.Errorf("no leading version in %q", name)
	}
	return strconv.Atoi(digits)
}
```

### The runnable demo

The demo builds a nested `MapFS` that mirrors a real `migrations/` layout —
zero-padded up/down files plus a stray `.md` note — and prints the discovered,
ordered plan.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"testing/fstest"

	"example.com/migrations"
)

func main() {
	fsys := fstest.MapFS{
		"migrations/0002_add_users.up.sql":     {Data: []byte("CREATE TABLE users();")},
		"migrations/0002_add_users.down.sql":   {Data: []byte("DROP TABLE users;")},
		"migrations/0010_add_index.up.sql":     {Data: []byte("CREATE INDEX ...;")},
		"migrations/0001_init.up.sql":          {Data: []byte("CREATE SCHEMA app;")},
		"migrations/README.md":                 {Data: []byte("notes")},
	}

	plan, err := migrations.CollectMigrations(fsys, "migrations")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	for _, m := range plan {
		fmt.Printf("%04d %s\n", m.Version, m.Name)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
0001 0001_init.up.sql
0002 0002_add_users.up.sql
0010 0010_add_index.up.sql
```

### Tests

`TestOrdering` proves the result is version-sorted regardless of walk order and
that `.down.sql` and non-`.sql` files are ignored. `TestDuplicate` uses two
files claiming version 3 and asserts `errors.Is(err, ErrDuplicateVersion)`.
`TestWalkErrorPropagates` is the important one: it composes a `faultWalkFS` over
a `MapFS` that fails `ReadDir` for the root with a sentinel, and asserts the
walk aborts and returns that sentinel rather than a partial plan — the coverage
`MapFS` alone can never give, because `MapFS` never fails a directory read.

Create `discover_test.go`:

```go
package migrations

import (
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"
)

func sampleFS() fstest.MapFS {
	return fstest.MapFS{
		"migrations/0002_add_users.up.sql":   {Data: []byte("up2")},
		"migrations/0002_add_users.down.sql": {Data: []byte("down2")},
		"migrations/0010_add_index.up.sql":   {Data: []byte("up10")},
		"migrations/0001_init.up.sql":        {Data: []byte("up1")},
		"migrations/README.md":               {Data: []byte("notes")},
	}
}

func TestOrdering(t *testing.T) {
	t.Parallel()

	plan, err := CollectMigrations(sampleFS(), "migrations")
	if err != nil {
		t.Fatal(err)
	}
	want := []int{1, 2, 10}
	if len(plan) != len(want) {
		t.Fatalf("got %d migrations, want %d: %+v", len(plan), len(want), plan)
	}
	for i, v := range want {
		if plan[i].Version != v {
			t.Fatalf("plan[%d].Version = %d, want %d", i, plan[i].Version, v)
		}
	}
}

func TestDuplicate(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"migrations/0003_a.up.sql": {Data: []byte("a")},
		"migrations/0003_b.up.sql": {Data: []byte("b")},
	}
	_, err := CollectMigrations(fsys, "migrations")
	if !errors.Is(err, ErrDuplicateVersion) {
		t.Fatalf("err = %v, want errors.Is ErrDuplicateVersion", err)
	}
}

// faultWalkFS wraps a MapFS but fails ReadDir for a chosen directory, letting a
// test cover the walk-error branch that MapFS can never trigger.
type faultWalkFS struct {
	fstest.MapFS
	failDir string
	failErr error
}

func (f faultWalkFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name == f.failDir {
		return nil, f.failErr
	}
	return f.MapFS.ReadDir(name)
}

func TestWalkErrorPropagates(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("disk read failed")
	fsys := faultWalkFS{MapFS: sampleFS(), failDir: "migrations", failErr: sentinel}

	_, err := CollectMigrations(fsys, "migrations")
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want errors.Is sentinel", err)
	}
}
```

## Review

Discovery is correct when the output is a version-sorted set of the `*.up.sql`
files and nothing else — `.down.sql` and `.md` files ignored, duplicate versions
rejected with `ErrDuplicateVersion`, and any directory-read failure propagated,
never swallowed. The two traps this exercise drills: do not build ordering on
walk order (`WalkDir` is lexical, migrations are numeric — sort explicitly), and
do not drop the `err` argument in the `WalkDirFunc` (that is how a permission
failure becomes a silently short migration plan). The fault-FS test is the proof
that the error branch actually runs; a `MapFS`-only suite would leave it dark.

## Resources

- [`fs.WalkDir` and `fs.WalkDirFunc`](https://pkg.go.dev/io/fs#WalkDir) — lexical walk, DirEntry, the error argument, SkipDir/SkipAll.
- [`slices.SortFunc`](https://pkg.go.dev/slices#SortFunc) — the explicit numeric sort migration order requires.
- [golang-migrate file source](https://github.com/golang-migrate/migrate/tree/master/source/file) — how a real runner discovers and orders migration files.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-validate-custom-fs-with-testfs.md](02-validate-custom-fs-with-testfs.md) | Next: [04-asset-server-fileserverfs.md](04-asset-server-fileserverfs.md)
