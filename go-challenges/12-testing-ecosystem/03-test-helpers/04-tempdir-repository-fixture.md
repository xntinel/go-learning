# Exercise 4: A Temp-Dir Repository Fixture with Seed Helpers

A file-backed repository is a common backend component, and its tests need
isolated on-disk state per case or they contaminate each other. `t.TempDir()` is
the substrate: each call returns a fresh directory removed automatically. A
fixture helper roots a repository there and pairs it with a `seed` helper, so each
subtest starts from known, isolated state with zero manual cleanup.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
repofixture/                 independent module: example.com/repofixture
  go.mod                     go 1.26
  repo.go                    file-backed KV repo: Put, Get, List; ErrNotFound sentinel
  cmd/
    demo/
      main.go                creates a repo in a temp dir, round-trips a record
  repo_test.go               newTestRepo/seed helpers; parallel isolated subtests
```

- Files: `repo.go`, `cmd/demo/main.go`, `repo_test.go`.
- Implement: a `Repo` rooted at a directory, storing each `Record` as `<id>.json`, with `Put`, `Get` (wrapping `ErrNotFound` with `%w`), and a sorted `List`.
- Test: `newTestRepo(t)` builds a repo in `t.TempDir()`; `seed(t, repo, recs...)` inserts records; parallel subtests each get a fresh dir and assert isolation.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/repofixture/cmd/demo
cd ~/go-exercises/repofixture
go mod init example.com/repofixture
```

### The repository and why `t.TempDir` gives isolation

`Repo` stores each record as a JSON file named `<id>.json` under its root
directory. `Put` marshals and writes; `Get` reads and unmarshals, translating a
missing file into `ErrNotFound` wrapped with `%w` so callers branch with
`errors.Is`; `List` reads the directory, strips the `.json` suffix, and returns the
ids sorted for a deterministic result.

The fixture discipline lives in the test. `newTestRepo(t)` calls `t.TempDir()`,
which returns a *unique* directory that the runner removes when the test and its
subtests complete. Crucially, *each call* to `t.TempDir()` — including one per
subtest — yields a different directory. So when four parallel subtests each call
`newTestRepo(t)`, they get four physically separate directories and cannot see one
another's files. That is isolation for free: no shared root to reset, no leftover
files between runs, no `-count=2` flakiness from stale state. The `seed` helper
then loads each repo with just the records that subtest needs, keeping the setup
declarative.

Create `repo.go`:

```go
package repofixture

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ErrNotFound is returned (wrapped) when a record is absent.
var ErrNotFound = errors.New("record not found")

// Record is the stored entity.
type Record struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Repo is a file-backed key/value store rooted at a directory.
type Repo struct {
	dir string
}

// NewRepo returns a Repo rooted at dir. The directory must already exist.
func NewRepo(dir string) *Repo {
	return &Repo{dir: dir}
}

func (r *Repo) path(id string) string {
	return filepath.Join(r.dir, id+".json")
}

// Put stores rec as <id>.json.
func (r *Repo) Put(rec Record) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal %q: %w", rec.ID, err)
	}
	if err := os.WriteFile(r.path(rec.ID), data, 0o644); err != nil {
		return fmt.Errorf("write %q: %w", rec.ID, err)
	}
	return nil
}

// Get loads the record for id, or ErrNotFound if it is absent.
func (r *Repo) Get(id string) (Record, error) {
	data, err := os.ReadFile(r.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, fmt.Errorf("get %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return Record{}, fmt.Errorf("read %q: %w", id, err)
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return Record{}, fmt.Errorf("unmarshal %q: %w", id, err)
	}
	return rec, nil
}

// List returns all stored ids, sorted.
func (r *Repo) List() ([]string, error) {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return nil, fmt.Errorf("read dir: %w", err)
	}
	var ids []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".json") {
			ids = append(ids, strings.TrimSuffix(name, ".json"))
		}
	}
	slices.Sort(ids)
	return ids, nil
}
```

### The runnable demo

The demo uses `os.MkdirTemp` (the non-test analog of `t.TempDir`) so it runs
outside a test, writes a record, reads it back, and lists the store.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"os"

	"example.com/repofixture"
)

func main() {
	dir, _ := os.MkdirTemp("", "repo")
	defer os.RemoveAll(dir)

	repo := repofixture.NewRepo(dir)
	_ = repo.Put(repofixture.Record{ID: "u1", Name: "alice"})

	got, _ := repo.Get("u1")
	fmt.Printf("get u1: %s\n", got.Name)

	_, err := repo.Get("missing")
	fmt.Printf("get missing: notfound=%v\n", errors.Is(err, repofixture.ErrNotFound))

	ids, _ := repo.List()
	fmt.Printf("list: %v\n", ids)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
get u1: alice
get missing: notfound=true
list: [u1]
```

### The tests

`newTestRepo(t)` roots a repo in `t.TempDir()` and returns it. `seed(t, repo,
recs...)` inserts records, failing the test if any `Put` errors. Both call
`t.Helper()`. The parallel subtests each construct their own repo with distinct
seed data and assert round-trips and listings — proving isolation, because if the
subtests shared a directory, `List` would return another subtest's ids and the
assertions would flap under `-count=2`.

Create `repo_test.go`:

```go
package repofixture

import (
	"errors"
	"slices"
	"testing"
)

// newTestRepo builds a Repo rooted in a fresh temp dir owned by the test.
func newTestRepo(t *testing.T) *Repo {
	t.Helper()
	return NewRepo(t.TempDir())
}

// seed inserts records, failing the test if any Put errors.
func seed(t *testing.T, repo *Repo, recs ...Record) {
	t.Helper()
	for _, rec := range recs {
		if err := repo.Put(rec); err != nil {
			t.Fatalf("seed %q: %v", rec.ID, err)
		}
	}
}

func TestGetRoundTrip(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	seed(t, repo, Record{ID: "a", Name: "alice"})

	got, err := repo.Get("a")
	if err != nil {
		t.Fatalf("Get(a): %v", err)
	}
	if got.Name != "alice" {
		t.Fatalf("name = %q, want alice", got.Name)
	}
}

func TestGetMissing(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)

	_, err := repo.Get("nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(nope) = %v, want ErrNotFound", err)
	}
}

func TestListIsolatedPerSubtest(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		recs []Record
		want []string
	}{
		{"one", []Record{{ID: "x", Name: "X"}}, []string{"x"}},
		{"two", []Record{{ID: "b", Name: "B"}, {ID: "a", Name: "A"}}, []string{"a", "b"}},
		{"none", nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			repo := newTestRepo(t)
			seed(t, repo, c.recs...)

			ids, err := repo.List()
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if !slices.Equal(ids, c.want) {
				t.Fatalf("List = %v, want %v", ids, c.want)
			}
		})
	}
}
```

An `Example` documents `List`'s sorted output. It lives in its own test file so
its `fmt`/`os` imports stay separate from the main test file:

Create `example_test.go`:

```go
package repofixture

import (
	"fmt"
	"os"
)

func mustTempDir() string {
	d, err := os.MkdirTemp("", "example")
	if err != nil {
		panic(err)
	}
	return d
}

func ExampleRepo_List() {
	repo := NewRepo(mustTempDir())
	_ = repo.Put(Record{ID: "b", Name: "B"})
	_ = repo.Put(Record{ID: "a", Name: "A"})
	ids, _ := repo.List()
	fmt.Println(ids)
	// Output: [a b]
}
```

## Review

The repository is correct when `Get` on a missing id returns an error satisfying
`errors.Is(err, ErrNotFound)` and `List` is sorted and confined to the repo's own
directory. The fixture is correct when parallel subtests never see one another's
records — the proof is `TestListIsolatedPerSubtest`, whose three parallel subtests
each expect only their own ids; a shared directory would make them fail
intermittently. Run `go test -race -count=2`: `-count=2` reruns everything, and if
any state leaked between the temp dirs the second pass would observe stale files.
The mistake to avoid is rooting all subtests at one directory "to save a mkdir" —
`t.TempDir()` is cheap and its per-call freshness is the entire isolation
guarantee.

## Resources

- [testing.T.TempDir](https://pkg.go.dev/testing#T.TempDir) — unique auto-removed directory per call.
- [os.WriteFile / os.ReadFile](https://pkg.go.dev/os#WriteFile) — the file IO backing the repository.
- [errors.Is](https://pkg.go.dev/errors#Is) — matching the wrapped `ErrNotFound` and `os.ErrNotExist`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-cleanup-lifecycle-worker.md](05-cleanup-lifecycle-worker.md)
