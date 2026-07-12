# Exercise 10: Parse and order pseudo-versions from untagged commits

When a dependency resolves to something like
`v0.0.0-20191109021931-daa7c04131f5`, that is a pseudo-version: the toolchain's way
of giving an *untagged* commit a place in the semver order. Here you build the
inspector that validates one, extracts its commit time and revision, and orders two
chronologically — the tool that answers "what commit is this, and is it newer than
that one."

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
pseudover/                  independent module: example.com/pseudover
  go.mod
  pseudover.go              Inspect() over module.PseudoVersion*; ErrNotPseudoVersion
  cmd/
    demo/
      main.go               runnable: inspect two pseudo-versions, order them
  pseudover_test.go         validity, time/rev extraction, chronological ordering
```

- Files: `pseudover.go`, `cmd/demo/main.go`, `pseudover_test.go`.
- Implement: `Inspect(v string) (Info, error)` returning base, 12-char revision, and UTC time; `ErrNotPseudoVersion` for a tagged version.
- Test: `IsPseudoVersion` true for the timestamped form, false for `v1.2.3`; `Rev`/`Time` extract the expected hash and instant; `semver.Compare` orders an earlier-timestamp pseudo-version below a later one sharing a base.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Anatomy of a pseudo-version

A pseudo-version encodes three things into one semver string: a base, a UTC
timestamp, and a 12-character commit hash. `module` gives you a function for each
part. `module.IsPseudoVersion(v)` reports whether `v` is one at all — true for
`v0.0.0-20191109021931-daa7c04131f5`, false for a real tag like `v1.2.3`.
`module.PseudoVersionTime(v)` returns the commit's UTC `time.Time`,
`module.PseudoVersionRev(v)` returns the 12-char revision, and
`module.PseudoVersionBase(v)` returns the base tag the commit descends from — which
is empty for the `v0.0.0-...` form (a commit with no earlier tag on its line) and a
real version like `v1.2.3` for the `vX.Y.Z-0.timestamp-rev` form (a commit after that
tag).

The ordering is the payoff, and it is engineered into the string layout. Because the
timestamp sits in the pre-release segment in `yyyymmddhhmmss` form — fixed width,
lexically sortable — two pseudo-versions sharing a base sort *chronologically* under
plain `semver.Compare`: the earlier commit is the lower version. And a pseudo-version
sorts *above* its base tag but *below* the next real release, so an untagged commit
is correctly "newer than the release it came after, older than the release that will
supersede it." That is exactly the belief MVS needs to hold about an untagged
dependency. `Inspect` surfaces the parts; the ordering falls out of `semver.Compare`
for free.

Guarding the entry with `ErrNotPseudoVersion` keeps the extraction honest: the
`PseudoVersion*` functions assume a pseudo-version, so you check `IsPseudoVersion`
first and return a typed error for a plain tag rather than a confusing partial parse.

Create `pseudover.go`:

```go
package pseudover

import (
	"errors"
	"fmt"
	"time"

	"golang.org/x/mod/module"
)

// ErrNotPseudoVersion is returned by Inspect for a version that is a real tag,
// not a pseudo-version.
var ErrNotPseudoVersion = errors.New("pseudover: not a pseudo-version")

// Info is the decoded content of a pseudo-version.
type Info struct {
	Version string
	Base    string // base tag; empty for the v0.0.0-... form
	Rev     string // 12-char commit revision
	Time    time.Time
}

// Inspect decodes a pseudo-version into its base, revision, and commit time. It
// returns ErrNotPseudoVersion if v is a plain tagged version.
func Inspect(v string) (Info, error) {
	if !module.IsPseudoVersion(v) {
		return Info{}, fmt.Errorf("pseudover: %q: %w", v, ErrNotPseudoVersion)
	}
	base, err := module.PseudoVersionBase(v)
	if err != nil {
		return Info{}, fmt.Errorf("pseudover: base of %q: %w", v, err)
	}
	rev, err := module.PseudoVersionRev(v)
	if err != nil {
		return Info{}, fmt.Errorf("pseudover: rev of %q: %w", v, err)
	}
	ts, err := module.PseudoVersionTime(v)
	if err != nil {
		return Info{}, fmt.Errorf("pseudover: time of %q: %w", v, err)
	}
	return Info{Version: v, Base: base, Rev: rev, Time: ts}, nil
}
```

### The runnable demo

The demo inspects both pseudo-version shapes, shows a real tag is rejected, and
orders two same-base pseudo-versions chronologically.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/pseudover"
	"golang.org/x/mod/semver"
)

func main() {
	for _, v := range []string{
		"v0.0.0-20191109021931-daa7c04131f5",
		"v1.2.4-0.20201230000000-abcdefabcdef",
	} {
		info, err := pseudover.Inspect(v)
		if err != nil {
			fmt.Printf("%s: %v\n", v, err)
			continue
		}
		base := info.Base
		if base == "" {
			base = "(none)"
		}
		fmt.Printf("%s\n  base=%s rev=%s time=%s\n",
			info.Version, base, info.Rev, info.Time.UTC().Format(time.RFC3339))
	}

	if _, err := pseudover.Inspect("v1.2.3"); err != nil {
		fmt.Println("v1.2.3:", err)
	}

	older := "v0.0.0-20191109021931-daa7c04131f5"
	newer := "v0.0.0-20201230000000-abcdefabcdef"
	fmt.Printf("order: earlier before later = %v\n", semver.Compare(older, newer) < 0)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
v0.0.0-20191109021931-daa7c04131f5
  base=(none) rev=daa7c04131f5 time=2019-11-09T02:19:31Z
v1.2.4-0.20201230000000-abcdefabcdef
  base=v1.2.3 rev=abcdefabcdef time=2020-12-30T00:00:00Z
v1.2.3: pseudover: "v1.2.3": pseudover: not a pseudo-version
order: earlier before later = true
```

### Tests

The tests assert validity, the exact extracted revision and instant, the empty-vs-set
base for the two forms, and the two ordering facts: chronological among same-base
pseudo-versions, and pseudo-above-base-tag.

Create `pseudover_test.go`:

```go
package pseudover

import (
	"errors"
	"testing"
	"time"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

func TestIsPseudoVersion(t *testing.T) {
	t.Parallel()
	if !module.IsPseudoVersion("v0.0.0-20191109021931-daa7c04131f5") {
		t.Fatal("expected the timestamped form to be a pseudo-version")
	}
	if module.IsPseudoVersion("v1.2.3") {
		t.Fatal("a real tag must not be a pseudo-version")
	}
}

func TestInspectExtracts(t *testing.T) {
	t.Parallel()
	info, err := Inspect("v0.0.0-20191109021931-daa7c04131f5")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if info.Rev != "daa7c04131f5" {
		t.Fatalf("Rev = %q, want daa7c04131f5", info.Rev)
	}
	if info.Base != "" {
		t.Fatalf("Base = %q, want empty for the v0.0.0 form", info.Base)
	}
	want := time.Date(2019, time.November, 9, 2, 19, 31, 0, time.UTC)
	if !info.Time.Equal(want) {
		t.Fatalf("Time = %s, want %s", info.Time.UTC(), want)
	}
}

func TestInspectBaseVariant(t *testing.T) {
	t.Parallel()
	info, err := Inspect("v1.2.4-0.20201230000000-abcdefabcdef")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if info.Base != "v1.2.3" {
		t.Fatalf("Base = %q, want v1.2.3", info.Base)
	}
	if info.Rev != "abcdefabcdef" {
		t.Fatalf("Rev = %q, want abcdefabcdef", info.Rev)
	}
}

func TestInspectRejectsTag(t *testing.T) {
	t.Parallel()
	if _, err := Inspect("v1.2.3"); !errors.Is(err, ErrNotPseudoVersion) {
		t.Fatalf("Inspect(v1.2.3) err = %v, want ErrNotPseudoVersion", err)
	}
}

func TestChronologicalOrder(t *testing.T) {
	t.Parallel()
	older := "v0.0.0-20191109021931-daa7c04131f5"
	newer := "v0.0.0-20201230000000-abcdefabcdef"
	if semver.Compare(older, newer) >= 0 {
		t.Fatalf("expected earlier-timestamp pseudo-version to sort below later")
	}
}

func TestPseudoSortsAboveBaseTag(t *testing.T) {
	t.Parallel()
	base := "v1.2.3"
	pseudo := "v1.2.4-0.20201230000000-abcdefabcdef"
	// A pseudo-version descending from v1.2.3 is greater than the tag itself.
	if semver.Compare(base, pseudo) >= 0 {
		t.Fatalf("base %s should sort below its descendant pseudo-version %s", base, pseudo)
	}
}
```

## Review

The inspector is correct when it guards with `IsPseudoVersion` before extracting, so a
plain tag returns `ErrNotPseudoVersion` instead of a partial parse, and when it reads
the three parts straight from the `module.PseudoVersion*` functions. The tests pin the
exact revision (`daa7c04131f5`), the exact UTC instant, and the empty-vs-`v1.2.3` base
that distinguishes the two forms. The ordering facts are the operational point:
same-base pseudo-versions sort chronologically because the timestamp is a fixed-width
lexical field, and a pseudo-version sorts above its base tag — which is why MVS treats
an untagged commit as newer than the release it descends from and older than the next
one. Seeing a pseudo-version in a `go.mod` is a signal to check whether that
unreleased pin was intended.

## Resources

- [Go Modules Reference: pseudo-versions](https://go.dev/ref/mod#pseudo-versions) — the three forms and their ordering.
- [`module.IsPseudoVersion` and friends](https://pkg.go.dev/golang.org/x/mod/module#IsPseudoVersion) — `PseudoVersionBase`, `PseudoVersionTime`, `PseudoVersionRev`.
- [`golang.org/x/mod/semver`](https://pkg.go.dev/golang.org/x/mod/semver) — the `Compare` that orders them.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../05-multi-module-workspaces/00-concepts.md](../05-multi-module-workspaces/00-concepts.md)
