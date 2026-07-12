# Exercise 9: CI Guards and Detecting Orphaned Goldens

A snapshot suite rots without operational guards. Two keep it honest: a check
that refuses to run in `-update` mode under CI (so the pipeline can never
rubber-stamp a snapshot), and a walk of `testdata/` that fails on golden files no
case references. You build both around a notification renderer.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
notifygold/                independent module: example.com/notifygold
  go.mod                   go 1.26
  notify.go                RenderNotification(kind, data) via text/template
  testdata/
    alert.golden
    digest.golden
  cmd/
    demo/
      main.go              renders a notification and prints it
  notify_test.go           CI update guard + orphan-golden walk + goldens
```

Files: `notify.go`, two `testdata/*.golden`, `cmd/demo/main.go`, `notify_test.go`.
Implement: `RenderNotification(kind string, data map[string]any) (string, error)`.
Test: a `forbidUpdateInCI` guard (sentinel `ErrUpdateInCI`), a `findOrphans` walk of `testdata/` diffed against the case set with `maps`/`slices` helpers, and the per-case goldens themselves.
Verify: `go test -count=1 -race ./...`

### Two guards that make the suite self-policing

A golden suite has two long-term failure modes that no single test catches, and
this exercise closes both.

The first is the update flag running in automation. The flag exists so a
developer can regenerate goldens deliberately, then review the diff — but if a CI
job can pass the flag, the pipeline regenerates the snapshot and compares it
against what it just wrote, which can never fail. The guard is a function
`forbidUpdateInCI(update, ci)` that returns a wrapped sentinel `ErrUpdateInCI`
when `update` is set and a CI environment variable is present. Every golden test
calls it first, so a `go test -update` invoked under CI fails loudly instead of
silently blessing the output. Using a sentinel wrapped with `%w` lets a test
assert the exact condition with `errors.Is`, and lets a caller distinguish this
policy failure from a real mismatch.

The second is orphaned goldens: files that no case references anymore because the
case was renamed or removed. They accumulate as dead weight and mislead
reviewers into thinking a fixture is live. The guard is `findOrphans`, which
walks `testdata/` with `filepath.WalkDir`, collects every `.golden` file, and
returns the ones absent from the expected set the case table declares. A
`TestNoOrphanGoldens` fails on any extra, so a deleted case cannot leave its
fixture behind. The set diff uses the modern `maps.Keys`/`slices.Sorted` helpers
to produce a stable, sorted report.

Together these make the corpus honest: no un-reviewed updates, no dead fixtures.

Create `notify.go`:

```go
package notifygold

import (
	"bytes"
	"strings"
	"text/template"
)

var tmpl = template.Must(template.New("root").Parse(`
{{define "alert"}}ALERT [{{.Severity}}] {{.Service}}: {{.Message}}{{end}}
{{define "digest"}}Daily digest for {{.Date}}
New: {{.New}}  Resolved: {{.Resolved}}{{end}}
`))

// RenderNotification executes the named template with data, applying a single-
// trailing-newline policy for a stable golden.
func RenderNotification(kind string, data map[string]any) (string, error) {
	var b bytes.Buffer
	if err := tmpl.ExecuteTemplate(&b, kind, data); err != nil {
		return "", err
	}
	return strings.TrimRight(b.String(), "\n") + "\n", nil
}
```

Now the committed goldens.

Create `testdata/alert.golden`:

```text
ALERT [high] payments: error rate 5%
```

Create `testdata/digest.golden`:

```text
Daily digest for 2026-07-02
New: 3  Resolved: 5
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/notifygold"
)

func main() {
	out, err := notifygold.RenderNotification("alert", map[string]any{
		"Severity": "high", "Service": "payments", "Message": "error rate 5%",
	})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Print(out)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
ALERT [high] payments: error rate 5%
```

### Tests

`forbidUpdateInCI` and `findOrphans` are unit-tested in isolation so their logic
is proven without depending on the ambient environment, and then applied to the
real suite.

Create `notify_test.go`:

```go
package notifygold

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

var update = flag.Bool("update", false, "regenerate golden files in testdata/")

// ErrUpdateInCI is returned when a golden update is requested under CI.
var ErrUpdateInCI = errors.New("refusing to regenerate goldens in CI")

func forbidUpdateInCI(update bool, ci string) error {
	if update && ci != "" {
		return fmt.Errorf("%w (CI=%q)", ErrUpdateInCI, ci)
	}
	return nil
}

// findOrphans returns the .golden files under dir that are not in expected.
func findOrphans(dir string, expected map[string]bool) ([]string, error) {
	var orphans []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".golden" {
			return nil
		}
		if base := filepath.Base(path); !expected[base] {
			orphans = append(orphans, base)
		}
		return nil
	})
	slices.Sort(orphans)
	return orphans, err
}

type notifyCase struct {
	name string
	data map[string]any
}

func notifyCases() []notifyCase {
	return []notifyCase{
		{"alert", map[string]any{"Severity": "high", "Service": "payments", "Message": "error rate 5%"}},
		{"digest", map[string]any{"Date": "2026-07-02", "New": 3, "Resolved": 5}},
	}
}

func expectedGoldens() map[string]bool {
	m := map[string]bool{}
	for _, c := range notifyCases() {
		m[c.name+".golden"] = true
	}
	return m
}

func TestForbidUpdateInCI(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		update  bool
		ci      string
		wantErr bool
	}{
		{"update under CI is refused", true, "true", true},
		{"update locally is allowed", true, "", false},
		{"read under CI is allowed", false, "true", false},
		{"read locally is allowed", false, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := forbidUpdateInCI(tc.update, tc.ci)
			if tc.wantErr {
				if !errors.Is(err, ErrUpdateInCI) {
					t.Fatalf("err = %v, want ErrUpdateInCI", err)
				}
			} else if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
		})
	}
}

func TestFindOrphansDetectsExtra(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, name := range []string{"alert.golden", "digest.golden", "stale_removed.golden"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	orphans, err := findOrphans(dir, map[string]bool{"alert.golden": true, "digest.golden": true})
	if err != nil {
		t.Fatalf("findOrphans: %v", err)
	}
	if want := []string{"stale_removed.golden"}; !slices.Equal(orphans, want) {
		t.Fatalf("orphans = %v, want %v", orphans, want)
	}
}

func TestNoOrphanGoldens(t *testing.T) {
	t.Parallel()
	expected := expectedGoldens()
	orphans, err := findOrphans("testdata", expected)
	if err != nil {
		t.Fatalf("findOrphans: %v", err)
	}
	if len(orphans) > 0 {
		t.Fatalf("orphaned goldens %v; expected only %v", orphans, slices.Sorted(maps.Keys(expected)))
	}
}

func TestNotificationGoldens(t *testing.T) {
	for _, tc := range notifyCases() {
		t.Run(tc.name, func(t *testing.T) {
			if err := forbidUpdateInCI(*update, os.Getenv("CI")); err != nil {
				t.Fatal(err)
			}
			got, err := RenderNotification(tc.name, tc.data)
			if err != nil {
				t.Fatalf("RenderNotification(%q): %v", tc.name, err)
			}
			path := filepath.Join("testdata", tc.name+".golden")
			if *update {
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden %s: %v (run: go test -update)", path, err)
			}
			if string(want) != got {
				t.Fatalf("golden mismatch for %s (run: go test -update)\n--- got ---\n%s--- want ---\n%s", path, got, want)
			}
		})
	}
}

func ExampleRenderNotification() {
	out, _ := RenderNotification("digest", map[string]any{"Date": "2026-07-02", "New": 3, "Resolved": 5})
	fmt.Print(out)
	// Output:
	// Daily digest for 2026-07-02
	// New: 3  Resolved: 5
}
```

## Review

The two guards are what stop a snapshot suite from decaying into theater.
`forbidUpdateInCI`, asserted through the `ErrUpdateInCI` sentinel, guarantees a
pipeline can never regenerate and pass its own goldens — the flag stays a local,
deliberate, reviewed act. `findOrphans` guarantees the corpus matches the case
table: a removed case that leaves its fixture behind fails `TestNoOrphanGoldens`,
so dead goldens cannot masquerade as live ones. Both are unit-tested in isolation
(`TestForbidUpdateInCI`, `TestFindOrphansDetectsExtra`) so their logic is proven
independent of the ambient environment, then applied to the real suite. This is
the operational maturity that separates a golden suite you can trust at scale from
one the team learns to ignore.

## Resources

- [path/filepath.WalkDir](https://pkg.go.dev/path/filepath#WalkDir) — walking `testdata/` to find every golden.
- [maps.Keys / slices.Sorted](https://pkg.go.dev/slices#Sorted) — the modern set-to-sorted-slice idiom.
- [errors.Is](https://pkg.go.dev/errors#Is) — asserting the wrapped `ErrUpdateInCI` sentinel.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../20-test-coverage-analysis/00-concepts.md](../20-test-coverage-analysis/00-concepts.md)
