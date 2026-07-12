# Exercise 9: Per-case fixture directories discovered with os.ReadDir

Flat `input`/`golden` pairs stop scaling when a single case needs several related files — an input, an expected output, and maybe a per-case config. The organizing move is a directory per case: `testdata/cases/<name>/` holding `input.json`, `want.json`, and optionally `config.json`. `os.ReadDir` enumerates the cases; each becomes a subtest. This module tests an event-enrichment pipeline that way.

## What you'll build

```text
enrich/                       independent module: example.com/enrich
  go.mod                      go 1.26
  enrich.go                   Event, Config, Enriched; Enrich(ev, cfg) Enriched
  cmd/
    demo/
      main.go                 enriches an inline event and prints JSON
  enrich_test.go              os.ReadDir(testdata/cases); one subtest per directory
  testdata/cases/
    basic/          input.json config.json want.json
    unknown-user/   input.json config.json want.json
    no-config/      input.json want.json
    notes.txt       (a non-directory entry, skipped)
```

Files: `enrich.go`, `cmd/demo/main.go`, `enrich_test.go`, and the per-case directories.
Implement: `Enrich` that looks a user up in the config and fills tier/region, defaulting to `unknown` when the user or config is absent.
Test: `os.ReadDir("testdata/cases")`, skip non-directory entries, and for each directory run a subtest that loads `input.json` (+ optional `config.json`) and compares the enriched result to `want.json`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/07-test-fixtures-and-testdata/09-per-case-directories/cmd/demo go-solutions/12-testing-ecosystem/07-test-fixtures-and-testdata/09-per-case-directories/testdata/cases
cd go-solutions/12-testing-ecosystem/07-test-fixtures-and-testdata/09-per-case-directories
```

### When a case needs more than two files

A glob over `*.input`/`*.golden` is perfect while a case is exactly one input and one expected output. Real pipeline cases are rarely that tidy: enriching an event may depend on a lookup config, a feature flag, or a fixture representing upstream state, and the expected output is a separate file again. Cramming those into a naming convention (`basic.input`, `basic.config`, `basic.golden`) works until it doesn't — the relationship between the files is implicit in a shared prefix, and a fourth file makes the scheme creak. A directory per case makes the grouping explicit: everything `basic` needs lives in `testdata/cases/basic/`, and the files inside have plain, uniform names (`input.json`, `want.json`, `config.json`) that read the same in every case.

`os.ReadDir` enumerates the cases directory and returns entries in sorted order. The iteration has two obligations. First, skip anything that is not a directory: a stray `notes.txt` or an editor's scratch file in `testdata/cases/` is not a case, and `if !entry.IsDir() { continue }` filters it out cleanly using the `fs.DirEntry.IsDir` check that `ReadDir` hands you without an extra stat call. Second, treat a required file as mandatory — a case directory missing `want.json` is a broken case and must fail, not silently pass — while an *optional* file like `config.json` is handled by distinguishing "not found" (`errors.Is(err, fs.ErrNotExist)`, use the empty default) from a genuine read error (fatal). That distinction is the crux: a missing optional file is a valid case shape; a permission error reading it is a broken environment, and conflating them hides real problems.

`Enrich` itself is a small, honest pipeline: it looks the event's user up in the config and copies tier and region onto the event, falling back to `unknown` when the user is absent or no config was supplied. That fallback is exactly what the `unknown-user` and `no-config` cases pin.

Create `enrich.go`:

```go
package enrich

// Event is an inbound event to enrich.
type Event struct {
	UserID string `json:"user_id"`
	Action string `json:"action"`
}

// UserInfo is the per-user enrichment data.
type UserInfo struct {
	Tier   string `json:"tier"`
	Region string `json:"region"`
}

// Config maps user IDs to their enrichment data.
type Config struct {
	Users map[string]UserInfo `json:"users"`
}

// Enriched is an event with tier and region attached.
type Enriched struct {
	UserID string `json:"user_id"`
	Action string `json:"action"`
	Tier   string `json:"tier"`
	Region string `json:"region"`
}

// Enrich attaches tier and region from the config, defaulting to "unknown"
// when the user is not present (or no config was supplied).
func Enrich(ev Event, cfg Config) Enriched {
	info := cfg.Users[ev.UserID] // zero UserInfo if absent
	return Enriched{
		UserID: ev.UserID,
		Action: ev.Action,
		Tier:   orDefault(info.Tier, "unknown"),
		Region: orDefault(info.Region, "unknown"),
	}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
```

Now the cases. `basic` has all three files; `unknown-user` supplies a config that lacks the event's user; `no-config` omits `config.json` entirely.

Create `testdata/cases/basic/input.json`:

```json
{"user_id": "u1", "action": "login"}
```

Create `testdata/cases/basic/config.json`:

```json
{"users": {"u1": {"tier": "gold", "region": "us"}}}
```

Create `testdata/cases/basic/want.json`:

```json
{"user_id": "u1", "action": "login", "tier": "gold", "region": "us"}
```

Create `testdata/cases/unknown-user/input.json`:

```json
{"user_id": "u9", "action": "logout"}
```

Create `testdata/cases/unknown-user/config.json`:

```json
{"users": {"u1": {"tier": "gold", "region": "us"}}}
```

Create `testdata/cases/unknown-user/want.json`:

```json
{"user_id": "u9", "action": "logout", "tier": "unknown", "region": "unknown"}
```

Create `testdata/cases/no-config/input.json`:

```json
{"user_id": "u1", "action": "view"}
```

Create `testdata/cases/no-config/want.json`:

```json
{"user_id": "u1", "action": "view", "tier": "unknown", "region": "unknown"}
```

Create `testdata/cases/notes.txt`:

```text
Not a case directory. The test skips non-directory entries.
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"

	"example.com/enrich"
)

func main() {
	ev := enrich.Event{UserID: "u1", Action: "login"}
	cfg := enrich.Config{Users: map[string]enrich.UserInfo{
		"u1": {Tier: "gold", Region: "us"},
	}}
	out, _ := json.Marshal(enrich.Enrich(ev, cfg))
	fmt.Println(string(out))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{"user_id":"u1","action":"login","tier":"gold","region":"us"}
```

### The test

`os.ReadDir` lists the cases; non-directory entries are skipped; each directory becomes a subtest named after it. `input.json` and `want.json` are mandatory (a read/decode failure is fatal); `config.json` is optional (absent means the empty config). `Enriched` is a comparable struct, so the assertion is a plain `!=`.

Create `enrich_test.go`:

```go
package enrich

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func mustReadJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}

func TestEnrichCases(t *testing.T) {
	t.Parallel()

	root := filepath.Join("testdata", "cases")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read cases: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no case directories found")
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue // skip notes.txt and any stray file
		}
		name := e.Name()
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(root, name)

			var ev Event
			mustReadJSON(t, filepath.Join(dir, "input.json"), &ev)

			var cfg Config
			if data, err := os.ReadFile(filepath.Join(dir, "config.json")); err == nil {
				if err := json.Unmarshal(data, &cfg); err != nil {
					t.Fatalf("decode config: %v", err)
				}
			} else if !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("read config: %v", err)
			}

			var want Enriched
			mustReadJSON(t, filepath.Join(dir, "want.json"), &want)

			if got := Enrich(ev, cfg); got != want {
				t.Fatalf("case %s:\ngot:  %+v\nwant: %+v", name, got, want)
			}
		})
	}
}

func ExampleEnrich() {
	got := Enrich(Event{UserID: "x", Action: "a"}, Config{})
	fmt.Println(got.Tier, got.Region)
	// Output: unknown unknown
}
```

## Review

The suite is correct when every case directory drives a subtest, non-directory entries are skipped, a missing required file fails the case, and an absent optional file is distinguished from a read error. The per-directory layout is what lets a case carry more than two files without a brittle naming scheme — the grouping is the directory, and the files inside have uniform names. The traps: forgetting the `IsDir` skip so a stray file becomes a broken "case", and swallowing a `config.json` read error as "absent" instead of separating `fs.ErrNotExist` from a real failure. Adding a case remains a no-Go-edit operation: create a directory, drop in the files.

## Resources

- [os.ReadDir](https://pkg.go.dev/os#ReadDir) — enumerating a directory into sorted `fs.DirEntry` values.
- [io/fs: DirEntry](https://pkg.go.dev/io/fs#DirEntry) — `IsDir` and `Name` without an extra stat.
- [errors.Is with fs.ErrNotExist](https://pkg.go.dev/io/fs#pkg-variables) — distinguishing a missing file from a read error.

---

Back to [08-seed-repository-fixtures.md](08-seed-repository-fixtures.md) | Next: [10-roundtrip-canonical-fixture.md](10-roundtrip-canonical-fixture.md)
