# Exercise 6: Deterministic config/template discovery with fs.Glob

Layered configuration — a base file plus a `conf.d/*.yaml` drop-in directory — is
everywhere (nginx, systemd, Kubernetes). Discovering the fragments with
`fs.Glob` is easy; doing it *deterministically* is the senior move, because
merge precedence depends on order and glob order is not something to trust. This
exercise builds `DiscoverConfigs` with an explicit sort and proves the bad-pattern
error path.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
configdisco/                 independent module: example.com/configdisco
  go.mod                     go 1.26
  discover.go                DiscoverConfigs(fs.FS, pattern) ([]string, error), sorted
  cmd/
    demo/
      main.go                glob conf.d/*.yaml over a MapFS and print ordered matches
  discover_test.go           sorted-match, no-match, and bad-pattern tests
```

- Files: `discover.go`, `cmd/demo/main.go`, `discover_test.go`.
- Implement: `DiscoverConfigs(fsys fs.FS, pattern string) ([]string, error)`
  that uses `fs.Glob`, returns matches in a deterministic sorted order, and
  surfaces a malformed pattern as `path.ErrBadPattern`.
- Test: `MapFS` with matching and non-matching files asserting the sorted match
  set; a bad pattern asserting `errors.Is(err, path.ErrBadPattern)`; an empty
  match set asserting `nil` error and an empty slice.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/11-testing-filesystems-with-fstest/06-glob-config-discovery/cmd/demo
cd go-solutions/12-testing-ecosystem/11-testing-filesystems-with-fstest/06-glob-config-discovery
```

### Sort the matches; do not trust glob order for precedence

`fs.Glob(fsys, pattern)` returns the names matching a `path.Match` pattern, or
`nil` and `path.ErrBadPattern` if the pattern is malformed (an unclosed
character class like `conf.d/[a-.yaml`). It is the right tool for finding drop-in
fragments: `conf.d/*.yaml` finds every YAML in `conf.d` and nothing in
subdirectories or with other extensions.

The trap is order. When configs are *merged* — later fragments override earlier
keys — the order the fragments are applied is part of the contract, and it must
be reproducible across machines and filesystem implementations. `fs.Glob`'s
ordering follows pattern expansion over `ReadDir`; while `ReadDir` itself is
sorted, building your merge semantics on "whatever order Glob happened to
return" is exactly the kind of implicit dependency that produces a bug that
reproduces on one host and not another. So `DiscoverConfigs` sorts the result
explicitly with `slices.Sort` before returning it. The cost is one line; the
payoff is that `10-overrides.yaml` reliably applies after `00-base.yaml` on
every machine.

Two edge cases the tests pin. A malformed pattern must surface as
`path.ErrBadPattern` (wrapped with `%w`) so a caller can distinguish "you gave me
a bad glob" from "no files matched". And zero matches is *not* an error — it is a
`nil` error with an empty (nil) slice, because an empty drop-in directory is a
perfectly valid configuration.

Create `discover.go`:

```go
package configdisco

import (
	"fmt"
	"io/fs"
	"slices"
)

// DiscoverConfigs returns the paths in fsys matching pattern, in deterministic
// sorted order so that layered-merge precedence is reproducible. A malformed
// pattern is reported as path.ErrBadPattern; zero matches is not an error.
func DiscoverConfigs(fsys fs.FS, pattern string) ([]string, error) {
	matches, err := fs.Glob(fsys, pattern)
	if err != nil {
		return nil, fmt.Errorf("discover %q: %w", pattern, err)
	}
	slices.Sort(matches)
	return matches, nil
}
```

### The runnable demo

The demo globs `conf.d/*.yaml` over a `MapFS` seeded with fragments in a
deliberately non-sorted map order plus decoy files, and prints the sorted
result.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"testing/fstest"

	"example.com/configdisco"
)

func main() {
	fsys := fstest.MapFS{
		"conf.d/10-overrides.yaml": {Data: []byte("k: v")},
		"conf.d/00-base.yaml":      {Data: []byte("k: v")},
		"conf.d/05-region.yaml":    {Data: []byte("k: v")},
		"conf.d/notes.txt":         {Data: []byte("ignored")},
		"other/x.yaml":             {Data: []byte("ignored")},
	}

	matches, err := configdisco.DiscoverConfigs(fsys, "conf.d/*.yaml")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	for _, m := range matches {
		fmt.Println(m)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
conf.d/00-base.yaml
conf.d/05-region.yaml
conf.d/10-overrides.yaml
```

### Tests

`TestSortedMatches` asserts the returned slice equals the expected sorted set and
that decoys (`.txt`, files outside `conf.d`) are excluded. `TestNoMatch` asserts
a pattern that matches nothing returns a `nil` error and an empty slice.
`TestBadPattern` asserts a malformed pattern returns an error satisfying
`errors.Is(err, path.ErrBadPattern)`.

Create `discover_test.go`:

```go
package configdisco

import (
	"errors"
	"path"
	"slices"
	"testing"
	"testing/fstest"
)

func confFS() fstest.MapFS {
	return fstest.MapFS{
		"conf.d/10-overrides.yaml": {Data: []byte("k: v")},
		"conf.d/00-base.yaml":      {Data: []byte("k: v")},
		"conf.d/05-region.yaml":    {Data: []byte("k: v")},
		"conf.d/notes.txt":         {Data: []byte("ignored")},
		"other/x.yaml":             {Data: []byte("ignored")},
	}
}

func TestSortedMatches(t *testing.T) {
	t.Parallel()

	got, err := DiscoverConfigs(confFS(), "conf.d/*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"conf.d/00-base.yaml",
		"conf.d/05-region.yaml",
		"conf.d/10-overrides.yaml",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("matches = %v, want %v", got, want)
	}
}

func TestNoMatch(t *testing.T) {
	t.Parallel()

	got, err := DiscoverConfigs(confFS(), "conf.d/*.toml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("matches = %v, want empty", got)
	}
}

func TestBadPattern(t *testing.T) {
	t.Parallel()

	_, err := DiscoverConfigs(confFS(), "conf.d/[a-.yaml")
	if !errors.Is(err, path.ErrBadPattern) {
		t.Fatalf("err = %v, want errors.Is path.ErrBadPattern", err)
	}
}
```

## Review

Discovery is correct when the returned slice is the sorted set of matching paths,
decoys excluded, a malformed pattern surfaces as `path.ErrBadPattern`, and zero
matches is a clean empty result rather than an error. The one habit that
separates this from a naive glob: sort explicitly whenever the result order feeds
merge or precedence semantics — `fs.Glob`'s ordering is not a contract you should
build on. Assert the exact sorted slice with `slices.Equal` so a regression in
ordering fails loudly.

## Resources

- [`fs.Glob`](https://pkg.go.dev/io/fs#Glob) — pattern matching over an `fs.FS`; only error is `path.ErrBadPattern`.
- [`path.Match`](https://pkg.go.dev/path#Match) — the pattern syntax `fs.Glob` uses.
- [`slices.Sort` / `slices.Equal`](https://pkg.go.dev/slices) — deterministic ordering and exact-slice assertion.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-fs-sub-tenant-scoping.md](05-fs-sub-tenant-scoping.md) | Next: [07-fault-injection-fs-wrapper.md](07-fault-injection-fs-wrapper.md)
