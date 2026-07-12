# Exercise 7: Supply-chain gate — verify go.sum completeness against go.mod

`go.sum` is not a lock file; it is a checksum database, and a *missing* entry (not a
wrong one) is what breaks a `-mod=readonly` CI build. This exercise builds the
integrity gate: parse `go.sum`, confirm every module required by `go.mod` has both
its module-content hash and its `/go.mod` hash, and flag orphan entries — teaching
exactly what `go.sum` guarantees.

This module is fully self-contained. It has its own `go mod init`, its own demo,
and its own tests, and imports nothing from the other exercises.

## What you'll build

```text
gosumcheck/                independent module: example.com/gosumcheck
  go.mod                   go 1.26; requires golang.org/x/mod
  gosumcheck.go            ParseGoSum(data); Verify(gomod, gosum) (missing, orphans, error)
  cmd/
    demo/
      main.go              verifies a complete pair, then one missing a /go.mod hash
  gosumcheck_test.go       PASS on complete; precise failure naming the incomplete require
```

- Files: `gosumcheck.go`, `cmd/demo/main.go`, `gosumcheck_test.go`.
- Implement: a `go.sum` parser (both the `path version h1:` and `path version/go.mod h1:` line forms) and a `Verify` that requires both hashes per require and reports orphan go.sum entries.
- Test: a complete fixture passes; a fixture missing a require's `/go.mod` hash fails naming it; an orphan entry is reported.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/11-packages-and-modules/06-dependency-management/07-gosum-integrity-verifier/cmd/demo
cd go-solutions/11-packages-and-modules/06-dependency-management/07-gosum-integrity-verifier
go get golang.org/x/mod
```

### What go.sum records, and what "missing" means

Each dependency contributes *two* lines to `go.sum`:

```text
github.com/google/uuid v1.6.0 h1:NIvaJDMOsjHA8n1jAhLSgzrAzy1Hgr+hNrb57e+94F0=
github.com/google/uuid v1.6.0/go.mod h1:TIyPZe4MgqvfeYDBFedMoGGpEw/LqOeaOT+nhxU+yHo=
```

The first is the hash of the module's file tree; the second, with the `/go.mod`
suffix on the version, is the hash of that module's own `go.mod`. Go needs the
`/go.mod` hash for every module in the pruned graph (to verify the graph without
downloading source) and the content hash for every module it actually compiles. So
completeness is: every require in `go.mod` must have *both* lines. The failure a
`-mod=readonly` build reports as "missing go.sum entry" is precisely one of these two
lines being absent — which is exactly what you get from hand-editing a `require` line
instead of running `go get`. An *orphan* is the inverse: a `go.sum` entry for a
module that `go.mod` no longer requires, left behind by an incomplete edit.

Parse each line with `strings.Fields` into `[path, version, hash]`; a version ending
in `/go.mod` (test with `strings.HasSuffix`) is the go.mod hash, and stripping that
suffix gives the real version. Key each module by `path` plus `version` and record
which of the two hashes are present.

Create `gosumcheck.go`:

```go
package gosumcheck

import (
	"bufio"
	"bytes"
	"fmt"
	"sort"
	"strings"

	"golang.org/x/mod/modfile"
)

// SumEntry records which of a module version's two go.sum hashes are present.
type SumEntry struct {
	HasModule bool // the h1: content hash
	HasGoMod  bool // the /go.mod h1: hash
}

func key(path, version string) string { return path + "@" + version }

// ParseGoSum parses go.sum into a map keyed by "path@version".
func ParseGoSum(data []byte) (map[string]*SumEntry, error) {
	sums := make(map[string]*SumEntry)
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, fmt.Errorf("malformed go.sum line: %q", line)
		}
		path, version := fields[0], fields[1]
		isGoMod := strings.HasSuffix(version, "/go.mod")
		version = strings.TrimSuffix(version, "/go.mod")
		k := key(path, version)
		e := sums[k]
		if e == nil {
			e = &SumEntry{}
			sums[k] = e
		}
		if isGoMod {
			e.HasGoMod = true
		} else {
			e.HasModule = true
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return sums, nil
}

// Verify checks that every require in go.mod has both go.sum hashes and reports
// any go.sum entry for a module no longer required. missing and orphans are
// sorted; both empty means go.sum is complete and consistent.
func Verify(gomod, gosum []byte) (missing, orphans []string, err error) {
	f, err := modfile.Parse("go.mod", gomod, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("parse go.mod: %w", err)
	}
	sums, err := ParseGoSum(gosum)
	if err != nil {
		return nil, nil, fmt.Errorf("parse go.sum: %w", err)
	}

	required := make(map[string]bool)
	for _, req := range f.Require {
		k := key(req.Mod.Path, req.Mod.Version)
		required[k] = true
		e := sums[k]
		switch {
		case e == nil:
			missing = append(missing, fmt.Sprintf("%s %s: no go.sum entry", req.Mod.Path, req.Mod.Version))
		case !e.HasModule:
			missing = append(missing, fmt.Sprintf("%s %s: missing module checksum", req.Mod.Path, req.Mod.Version))
		case !e.HasGoMod:
			missing = append(missing, fmt.Sprintf("%s %s: missing /go.mod checksum", req.Mod.Path, req.Mod.Version))
		}
	}
	for k := range sums {
		if !required[k] {
			orphans = append(orphans, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(orphans)
	return missing, orphans, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/gosumcheck"
)

const goMod = `module example.com/orders

go 1.24

require github.com/google/uuid v1.6.0
`

const complete = `github.com/google/uuid v1.6.0 h1:AAAA=
github.com/google/uuid v1.6.0/go.mod h1:BBBB=
`

const incomplete = `github.com/google/uuid v1.6.0 h1:AAAA=
`

func main() {
	missing, orphans, _ := gosumcheck.Verify([]byte(goMod), []byte(complete))
	fmt.Printf("complete: missing=%d orphans=%d\n", len(missing), len(orphans))

	missing, orphans, _ = gosumcheck.Verify([]byte(goMod), []byte(incomplete))
	fmt.Printf("incomplete: missing=%d orphans=%d\n", len(missing), len(orphans))
	for _, m := range missing {
		fmt.Println("  " + m)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
complete: missing=0 orphans=0
incomplete: missing=1 orphans=0
  github.com/google/uuid v1.6.0: missing /go.mod checksum
```

### Tests

Create `gosumcheck_test.go`:

```go
package gosumcheck

import (
	"fmt"
	"strings"
	"testing"
)

const goMod = `module example.com/orders

go 1.24

require (
	github.com/google/uuid v1.6.0
	golang.org/x/text v0.14.0
)
`

const complete = `github.com/google/uuid v1.6.0 h1:AAAA=
github.com/google/uuid v1.6.0/go.mod h1:BBBB=
golang.org/x/text v0.14.0 h1:CCCC=
golang.org/x/text v0.14.0/go.mod h1:DDDD=
`

func TestVerifyComplete(t *testing.T) {
	t.Parallel()
	missing, orphans, err := Verify([]byte(goMod), []byte(complete))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(missing) != 0 || len(orphans) != 0 {
		t.Fatalf("want clean, got missing=%v orphans=%v", missing, orphans)
	}
}

func TestVerifyMissingGoModHash(t *testing.T) {
	t.Parallel()
	// Drop the /go.mod line for x/text only.
	sum := `github.com/google/uuid v1.6.0 h1:AAAA=
github.com/google/uuid v1.6.0/go.mod h1:BBBB=
golang.org/x/text v0.14.0 h1:CCCC=
`
	missing, _, err := Verify([]byte(goMod), []byte(sum))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(missing) != 1 || !strings.Contains(missing[0], "golang.org/x/text") || !strings.Contains(missing[0], "/go.mod") {
		t.Fatalf("want x/text /go.mod flagged, got %v", missing)
	}
}

func TestVerifyMissingEntirely(t *testing.T) {
	t.Parallel()
	sum := `github.com/google/uuid v1.6.0 h1:AAAA=
github.com/google/uuid v1.6.0/go.mod h1:BBBB=
`
	missing, _, err := Verify([]byte(goMod), []byte(sum))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(missing) != 1 || !strings.Contains(missing[0], "no go.sum entry") {
		t.Fatalf("want x/text no-entry flagged, got %v", missing)
	}
}

func TestVerifyOrphan(t *testing.T) {
	t.Parallel()
	sum := complete + "github.com/rs/zerolog v1.33.0 h1:EEEE=\ngithub.com/rs/zerolog v1.33.0/go.mod h1:FFFF=\n"
	_, orphans, err := Verify([]byte(goMod), []byte(sum))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(orphans) != 1 || !strings.Contains(orphans[0], "github.com/rs/zerolog") {
		t.Fatalf("want zerolog orphan, got %v", orphans)
	}
}

func ExampleVerify() {
	gm := "module example.com/x\n\ngo 1.24\n\nrequire github.com/google/uuid v1.6.0\n"
	sum := "github.com/google/uuid v1.6.0 h1:AAAA=\ngithub.com/google/uuid v1.6.0/go.mod h1:BBBB=\n"
	missing, orphans, _ := Verify([]byte(gm), []byte(sum))
	fmt.Println(len(missing) + len(orphans))
	// Output: 0
}
```

## Review

The gate is correct when a `go.mod`/`go.sum` pair produced by `go mod tidy` passes,
and any hand-edit fails with the specific defect named: a require with no `go.sum`
entry, a require missing just its `/go.mod` hash, or an orphan entry for a dropped
dependency. The two-hash requirement is the crux — verifying only the content hash
would let the pruned-graph verification break at build time with the "missing go.sum
entry" error this gate exists to pre-empt. Note this checks *completeness*
(structure), not the hashes themselves; verifying the actual `h1:` values against the
downloaded module content is the `go mod verify`/checksum-database job. Run
`go test -race`.

## Resources

- [`go.sum` files](https://go.dev/ref/mod#go-sum-files) — the two-hash structure and what each guarantees.
- [Module authentication](https://go.dev/ref/mod#authenticating) — how the go command uses go.sum and the checksum database.
- [`golang.org/x/mod/modfile`](https://pkg.go.dev/golang.org/x/mod/modfile) — `Parse` and `File.Require`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-semver-upgrade-policy.md](06-semver-upgrade-policy.md) | Next: [08-private-module-router.md](08-private-module-router.md)
