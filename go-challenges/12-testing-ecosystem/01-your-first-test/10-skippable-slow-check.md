# Exercise 10: testing.Short: A First Test That Opts Out Of Slow Work

Some checks are cheap and some are exhaustive. A first test suite can keep both by
guarding the slow one behind `testing.Short`, so the fast local loop skips it and
the full gate still runs it. This is the on-ramp to separating unit tests from
integration tests without deleting coverage.

## What you'll build

```text
manifest/                  independent module: example.com/manifest
  go.mod
  manifest.go              type Entry; sentinels; func ValidateManifest([]Entry) error
  manifest_test.go         TestValidateManifest_Fast, TestValidateManifest_Exhaustive, Example
  cmd/
    demo/
      main.go              validates a good manifest and a bad one
```

- Files: `manifest.go`, `manifest_test.go`, `cmd/demo/main.go`.
- Implement: `ValidateManifest(entries []Entry) error` — reject empty names, negative sizes, and duplicate names, with wrapped sentinels.
- Test: a fast test that always runs; an exhaustive test that begins with `if testing.Short() { t.Skip(...) }`.
- Verify: `gofmt -l .`, `go vet ./...`, `go test -count=1 -race ./...` (full), and `go test -short ./...` (skips the slow path).

### One validator, two test speeds

`ValidateManifest` is the check a deploy or backup tool runs over a manifest of
files before trusting it: every entry must have a non-empty name, a non-negative
size, and a name unique within the manifest. It walks the slice once, tracking
seen names in a set, and returns the first violation wrapped around a
package-level sentinel — `ErrEmptyName`, `ErrNegativeSize`, or `ErrDuplicate` —
so a caller can branch on the category with `errors.Is` while a log line carries
the offending index and name. The function itself is always fully correct; there
is no "fast mode" inside it.

The two speeds live in the *tests*, and `testing.Short` is the switch.
`TestValidateManifest_Fast` always runs: it validates a small good manifest and
confirms one bad manifest trips the right sentinel — cheap, so it belongs in every
`go test` invocation, including the tight save-run-save loop. `TestValidateManifest_Exhaustive`
begins with `if testing.Short() { t.Skip("exhaustive check skipped in -short") }`
and then builds a large manifest (tens of thousands of entries) to exercise the
validator at scale, including a duplicate injected near the end so the
whole-slice scan is really traversed. That test is worth running in CI but not on
every local save, so `go test -short` skips it while a plain `go test` includes
it.

This is the lightest possible version of the unit-versus-integration split.
`-short` is a convention, not magic: it just sets `testing.Short()` to true, and
your test decides what "short" means for it. Later lessons build the full
integration story with build tags (lesson 18); here you learn the one flag and
`t.Skip` so a first suite can already separate the fast path from the slow one
without throwing away coverage. `t.Skip` marks the test skipped (not passed, not
failed) and stops it immediately, much like `Fatal` but without recording a
failure.

Create `manifest.go`:

```go
package manifest

import (
	"errors"
	"fmt"
)

// Sentinels for the three manifest violations, wrapped with %w so callers can
// branch with errors.Is.
var (
	ErrEmptyName    = errors.New("entry has empty name")
	ErrNegativeSize = errors.New("entry has negative size")
	ErrDuplicate    = errors.New("duplicate entry name")
)

// Entry is one file recorded in a manifest.
type Entry struct {
	Name string
	Size int64
}

// ValidateManifest checks that every entry has a non-empty name, a non-negative
// size, and a name unique within the manifest. It returns the first violation,
// wrapped around the matching sentinel, or nil if the manifest is valid.
func ValidateManifest(entries []Entry) error {
	seen := make(map[string]struct{}, len(entries))
	for i, e := range entries {
		switch {
		case e.Name == "":
			return fmt.Errorf("entry %d: %w", i, ErrEmptyName)
		case e.Size < 0:
			return fmt.Errorf("entry %d %q: %w", i, e.Name, ErrNegativeSize)
		}
		if _, dup := seen[e.Name]; dup {
			return fmt.Errorf("entry %d %q: %w", i, e.Name, ErrDuplicate)
		}
		seen[e.Name] = struct{}{}
	}
	return nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/manifest"
)

func main() {
	good := []manifest.Entry{
		{Name: "app.bin", Size: 1024},
		{Name: "config.yaml", Size: 256},
	}
	fmt.Printf("good manifest: %v\n", manifest.ValidateManifest(good))

	bad := []manifest.Entry{
		{Name: "app.bin", Size: 1024},
		{Name: "app.bin", Size: 1024},
	}
	fmt.Printf("bad manifest:  %v\n", manifest.ValidateManifest(bad))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
good manifest: <nil>
bad manifest:  entry 1 "app.bin": duplicate entry name
```

### The tests

Create `manifest_test.go`:

```go
package manifest

import (
	"errors"
	"fmt"
	"testing"
)

func TestValidateManifest_Fast(t *testing.T) {
	t.Parallel()

	// A small valid manifest passes.
	good := []Entry{{Name: "a", Size: 1}, {Name: "b", Size: 2}}
	if err := ValidateManifest(good); err != nil {
		t.Fatalf("ValidateManifest(good) = %v, want nil", err)
	}

	// An empty name trips the right sentinel.
	bad := []Entry{{Name: "", Size: 1}}
	err := ValidateManifest(bad)
	if err == nil {
		t.Fatalf("ValidateManifest(bad) = nil, want ErrEmptyName")
	}
	if !errors.Is(err, ErrEmptyName) {
		t.Errorf("ValidateManifest(bad) error = %v, want errors.Is ErrEmptyName", err)
	}
}

func TestValidateManifest_Exhaustive(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("exhaustive check skipped in -short")
	}

	// Build a large valid manifest and validate it at scale.
	const n = 50_000
	entries := make([]Entry, 0, n+1)
	for i := range n {
		entries = append(entries, Entry{Name: fmt.Sprintf("file-%d", i), Size: int64(i)})
	}
	if err := ValidateManifest(entries); err != nil {
		t.Fatalf("ValidateManifest(large valid) = %v, want nil", err)
	}

	// Inject a duplicate near the end so the full scan is exercised.
	entries = append(entries, Entry{Name: "file-0", Size: 0})
	err := ValidateManifest(entries)
	if !errors.Is(err, ErrDuplicate) {
		t.Errorf("ValidateManifest(with duplicate) error = %v, want errors.Is ErrDuplicate", err)
	}
}

func ExampleValidateManifest() {
	err := ValidateManifest([]Entry{{Name: "a", Size: -1}})
	fmt.Println(err)
	// Output: entry 0 "a": entry has negative size
}
```

## Review

The validator is correct when it returns the first violation wrapped around the
matching sentinel and `nil` for a clean manifest; the tests assert the category
with `errors.Is`, which works only because every failure path wraps with `%w`. The
lesson mechanic is `testing.Short`: run `go test` and both tests execute; run
`go test -short` and the exhaustive one reports as skipped, not passed and not
failed. That is how a first suite keeps an expensive check without paying for it on
every save — the same instinct that later grows into the build-tagged integration
split. Gate with `gofmt -l .`, `go vet ./...`, and `go test -count=1 -race ./...`;
confirm the skip with `go test -short -v ./...`.

## Resources

- [testing.Short and T.Skip](https://pkg.go.dev/testing#Short) — the `-short` flag and skipping.
- [cmd/go: Testing flags](https://pkg.go.dev/cmd/go#hdr-Testing_flags) — `-short`, `-count`, `-race`.
- [errors package](https://pkg.go.dev/errors) — `errors.Is` and `%w` wrapping.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [09-blackbox-featureflag-eval.md](09-blackbox-featureflag-eval.md) | Next: [../02-table-driven-tests/00-concepts.md](../02-table-driven-tests/00-concepts.md)
