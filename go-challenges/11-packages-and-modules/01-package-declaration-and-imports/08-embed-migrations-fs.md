# Exercise 8: Bundling SQL Migrations into the Binary with go:embed

Migrations, HTML templates, and OpenAPI specs are code the service needs at
runtime — and shipping them as loose files next to the binary is a reliable source
of "file not found in production" incidents when a container image or a deploy
script forgets to copy the directory. `//go:embed` folds those files *into* the
binary at compile time. This exercise embeds an ordered set of SQL migrations and
exposes them sorted by version, so the migrations travel with the executable and
there is no runtime filesystem dependency.

This module is self-contained. Nothing here imports another exercise. Because the
embedded assets are real `.sql` files on disk, create them exactly as shown before
building.

## What you'll build

```text
migrations/                        module: example.com/migrations
  go.mod
  migrations/sql/0001_create_users.sql
  migrations/sql/0002_add_orders_index.sql
  migrations/migrations.go         //go:embed sql/*.sql; All() []Migration sorted by version
  migrations/migrations_test.go    asserts count, ascending order, and known content
  cmd/demo/main.go                 prints each migration's version and name
```

- Files: the six above (two are `.sql` assets).
- Implement: `//go:embed sql/*.sql` into an `embed.FS`, and `All()` returning `[]Migration{Version, Name, SQL}` sorted ascending by version.
- Test: the embedded FS yields exactly two migrations, in ascending version order, with non-empty, matching content.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/11-packages-and-modules/01-package-declaration-and-imports/08-embed-migrations-fs/migrations/sql go-solutions/11-packages-and-modules/01-package-declaration-and-imports/08-embed-migrations-fs/cmd/demo
cd go-solutions/11-packages-and-modules/01-package-declaration-and-imports/08-embed-migrations-fs
go mod edit -go=1.26
```

### Why embed, and the embed rules that bite

`//go:embed` is a compiler directive: at build time it reads the matched files
from disk (relative to the source file) and materializes them as a read-only
`embed.FS` value. After that, the files are literally bytes inside the binary —
`go test`, `go run`, and the deployed executable all see the same content with no
directory to mount. Three rules matter here. First, using `//go:embed` requires
importing `embed`; when you embed into an `embed.FS` the package is referenced by
the variable's type, so a plain `import "embed"` suffices — but when you embed into
a `string` or `[]byte` and never otherwise name the package, you must write
`import _ "embed"` or the file will not compile. Second, the directive must sit
immediately above the `var` it annotates, with no blank line between them. Third,
patterns match relative to the `.go` file's directory and, for `*` and directory
patterns, exclude files whose names start with `.` or `_`.

First, the migration assets. These are real files the embed directive reads;
create them at `migrations/sql/`.

Create `migrations/sql/0001_create_users.sql`:

```sql
CREATE TABLE users (
    id    TEXT PRIMARY KEY,
    email TEXT NOT NULL UNIQUE
);
```

Create `migrations/sql/0002_add_orders_index.sql`:

```sql
CREATE INDEX idx_orders_user_id ON orders (user_id);
```

### The package: embed and order by version

`All()` reads the embedded directory, parses the `NNNN_name.sql` filename into a
version number and a name, loads each file's content, and returns the set sorted
ascending by version. Ordering is not cosmetic: migrations must be applied in
version order, and directory-read order is not guaranteed to be sorted, so the
sort is a correctness requirement. Reading is done through the `io/fs` interfaces
(`fs.ReadDir`) and `embed.FS`'s own `ReadFile`, which is exactly how a real
migration runner would consume the assets.

Create `migrations/migrations.go`:

```go
package migrations

import (
	"embed"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

//go:embed sql/*.sql
var files embed.FS

// Migration is one ordered SQL migration bundled into the binary.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

// All returns every embedded migration, sorted ascending by version. Filenames
// must be NNNN_name.sql; the numeric prefix is the version.
func All() ([]Migration, error) {
	entries, err := fs.ReadDir(files, "sql")
	if err != nil {
		return nil, err
	}
	out := make([]Migration, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		base := strings.TrimSuffix(name, ".sql")
		numStr, rest, ok := strings.Cut(base, "_")
		if !ok {
			continue
		}
		version, err := strconv.Atoi(numStr)
		if err != nil {
			return nil, err
		}
		content, err := files.ReadFile("sql/" + name)
		if err != nil {
			return nil, err
		}
		out = append(out, Migration{Version: version, Name: rest, SQL: string(content)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}
```

### The demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/migrations/migrations"
)

func main() {
	all, err := migrations.All()
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	for _, m := range all {
		fmt.Printf("%04d %s\n", m.Version, m.Name)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
0001 create_users
0002 add_orders_index
```

### Tests

The test reads the embedded FS with no filesystem access of its own — it works
because the bytes are compiled into the test binary. It pins the count, the
ascending order, and a known file's content.

Create `migrations/migrations_test.go`:

```go
package migrations

import (
	"strings"
	"testing"
)

func TestAllReturnsEmbeddedMigrations(t *testing.T) {
	t.Parallel()

	all, err := All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("len = %d, want 2", len(all))
	}
}

func TestAllSortedByVersion(t *testing.T) {
	t.Parallel()

	all, err := All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].Version >= all[i].Version {
			t.Fatalf("not ascending: %d before %d", all[i-1].Version, all[i].Version)
		}
	}
	if all[0].Version != 1 || all[0].Name != "create_users" {
		t.Fatalf("first = %d/%s, want 1/create_users", all[0].Version, all[0].Name)
	}
}

func TestMigrationContentEmbedded(t *testing.T) {
	t.Parallel()

	all, err := All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if !strings.Contains(all[0].SQL, "CREATE TABLE users") {
		t.Fatalf("first migration SQL = %q, want CREATE TABLE users", all[0].SQL)
	}
}
```

## Review

The package is correct when `All()` returns every embedded file parsed into a
`Migration` and sorted ascending by version, and when the tests pass with no
filesystem access — the proof that the assets are inside the binary. The
version-order sort is the load-bearing invariant: apply migrations out of order and
you corrupt a schema. Two embed traps to remember: the `//go:embed` line must sit
directly on top of its `var` (a blank line between them silently disables it), and
the `import _ "embed"` blank form is mandatory only for the `string`/`[]byte`
variants where the package is not otherwise referenced — the `embed.FS` form used
here references the package by type, so a normal import is enough. If a build fails
with `pattern sql/*.sql: no matching files found`, the `.sql` assets were not
created at the path the directive expects; embed reads real files from disk at
compile time, so they must exist next to the source.

## Resources

- [`embed`](https://pkg.go.dev/embed) — the `//go:embed` directive, `embed.FS`, and the `import _ "embed"` rule.
- [`io/fs.ReadDir`](https://pkg.go.dev/io/fs#ReadDir) — reading a directory through the `fs.FS` interface an `embed.FS` implements.
- [Go 1.16 release notes: embedding files](https://go.dev/doc/go1.16#embed) — the feature's introduction and intended uses.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-build-constraints-env-impl.md](07-build-constraints-env-impl.md) | Next: [09-example-tests-as-compiled-docs.md](09-example-tests-as-compiled-docs.md)
