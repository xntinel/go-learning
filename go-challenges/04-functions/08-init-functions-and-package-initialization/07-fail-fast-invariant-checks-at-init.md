# Exercise 7: Fail-Fast Startup — Verifying embed.FS Assets and Templates at init

This is the second production-legitimate use of `init()`: aborting a structurally
broken binary before it can accept traffic. This exercise embeds required assets
with `go:embed`, verifies at `init` that every expected file is present and every
template parses, and panics if not — so a mis-packaged binary crashes at startup
instead of failing the first request.

This module is fully self-contained: its own module, embedded assets, demo, and
tests.

## What you'll build

```text
bootcheck/                     independent module: example.com/bootcheck
  go.mod                       module example.com/bootcheck
  assets.go                    //go:embed migrations; verifyAssets + parseTemplates; init() fail-fast
  migrations/0001_users.go     embedded asset (SQL kept as a Go const so the module is self-contained)
  migrations/0002_index.go     embedded asset
  cmd/demo/main.go             prints the verified asset + template names
  assets_test.go               init did not panic; helpers reject a broken fs.FS
```

Files: `assets.go`, `migrations/0001_users.go`, `migrations/0002_index.go`, `cmd/demo/main.go`, `assets_test.go`.
Implement: `verifyAssets(fsys fs.FS, required []string) error` and `parseTemplates(bodies map[string]string) (*template.Template, error)`, called from `init()` on the embedded FS and the named-template map, panicking on failure.
Test: the package loaded (so `init` did not panic) and exposes the expected names; the helpers return an error when handed a deliberately broken `fs.FS` (`fstest.MapFS`).
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/bootcheck/migrations ~/go-exercises/bootcheck/cmd/demo
cd ~/go-exercises/bootcheck
go mod init example.com/bootcheck
```

### Why fail-fast at init is legitimate here

The rule elsewhere in this lesson is "keep work out of `init()`". This exercise is
the sanctioned exception. Verifying that embedded assets are present and that
templates parse is cheap, depends on nothing external, and has an answer fixed at
build time: either the binary was packaged correctly or it was not. A missing
migration file or an unparseable template is not a runtime condition to handle
gracefully — it means the binary itself is wrong, and the right response is to
refuse to start. `regexp.MustCompile` and `template.Must` encode exactly this
contract (panic on a bad static input); this exercise generalizes it to an embedded
asset set.

`//go:embed migrations` bakes the `migrations/` directory into the binary as an
`embed.FS`. Because `embed.FS` implements `fs.FS`, all the validation logic is
written against the `fs.FS` interface — which is what makes it testable. The two
helpers do the real work: `verifyAssets` walks a `required` list and confirms each
name resolves via `fs.Stat`; `parseTemplates` iterates a map of named in-memory
template bodies, parsing each with `root.New(name).Parse(body)`, and returns the
assembled set or an error if any body fails to parse. `init()` calls both on the
*real* embedded FS and panics on error. The tests call the same helpers with an
in-memory `fstest.MapFS` — a good one to confirm success, and a deliberately broken
one (missing a file, or containing a malformed template) to confirm each helper
returns an error *before* the panic wrapper. That separation — a pure helper that
returns an error, wrapped by an `init()` that panics — is how you make an
`init`-time fail-fast testable: you never trigger the panic in a test; you test the
helper it is built on.

One practical note for this self-contained module: the gate assembles only Go
sources, so the embedded "migration" files here carry a `.go` extension and keep
their SQL in a Go string constant. In a real service they would be `.sql` files and
the embed pattern would be `//go:embed migrations/*.sql`; the validation logic over
`fs.FS` is byte-for-byte identical either way.

Create the embedded assets. `migrations/0001_users.go`:

```go
// migrations/0001_users.go
package migrations

// Up is the forward migration. In a real project this would be a .sql file;
// here it is a Go const so the module embeds and builds with no external files.
const Users = `CREATE TABLE users (id BIGINT PRIMARY KEY, email TEXT NOT NULL);`
```

`migrations/0002_index.go`:

```go
// migrations/0002_index.go
package migrations

const Index = `CREATE UNIQUE INDEX users_email_idx ON users (email);`
```

Create `assets.go` — the embed, the helpers, and the fail-fast `init`:

```go
// assets.go
package bootcheck

import (
	"embed"
	"fmt"
	"io/fs"
	"text/template"
)

// migrationsFS embeds the migrations directory into the binary. If the directory
// were missing at build time, compilation itself would fail.
//
//go:embed migrations
var migrationsFS embed.FS

// requiredMigrations are the assets the binary cannot run without.
var requiredMigrations = []string{
	"migrations/0001_users.go",
	"migrations/0002_index.go",
}

// namedTemplates are precompiled at init and validated for parseability.
var namedTemplates = map[string]string{
	"greeting": `Hello {{.Name}}, you have {{.Count}} messages.`,
	"footer":   `-- sent at {{.Time}}`,
}

// Templates is the parsed template set, populated by init after validation.
var Templates *template.Template

// verifyAssets confirms every required name resolves in fsys. It is a pure
// function over fs.FS (which embed.FS satisfies), so a test can call it with a
// broken fstest.MapFS to exercise the failure path without triggering init's panic.
func verifyAssets(fsys fs.FS, required []string) error {
	for _, name := range required {
		if _, err := fs.Stat(fsys, name); err != nil {
			return fmt.Errorf("required asset %q missing: %w", name, err)
		}
	}
	return nil
}

// parseTemplates parses each named template body and returns the assembled set,
// or an error naming the first template that fails to parse.
func parseTemplates(bodies map[string]string) (*template.Template, error) {
	root := template.New("")
	for name, body := range bodies {
		if _, err := root.New(name).Parse(body); err != nil {
			return nil, fmt.Errorf("template %q failed to parse: %w", name, err)
		}
	}
	return root, nil
}

// init is the sanctioned fail-fast: cheap, dependency-free, build-time-answerable
// checks that abort a structurally broken binary before it serves traffic.
func init() {
	if err := verifyAssets(migrationsFS, requiredMigrations); err != nil {
		panic("bootcheck: " + err.Error())
	}
	t, err := parseTemplates(namedTemplates)
	if err != nil {
		panic("bootcheck: " + err.Error())
	}
	Templates = t
}

// AssetNames returns the embedded migration file names, sorted by fs.Glob.
func AssetNames() ([]string, error) {
	return fs.Glob(migrationsFS, "migrations/*.go")
}

// TemplateNames returns the parsed template names.
func TemplateNames() []string {
	var names []string
	for _, t := range Templates.Templates() {
		if t.Name() != "" {
			names = append(names, t.Name())
		}
	}
	return names
}
```

### The runnable demo

If `init` had panicked, the binary would never reach `main`. The demo therefore
just reports what was verified — reaching this code at all is proof the fail-fast
passed.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"bytes"
	"fmt"
	"sort"

	"example.com/bootcheck"
)

func main() {
	assets, err := bootcheck.AssetNames()
	if err != nil {
		fmt.Println("asset error:", err)
		return
	}
	fmt.Println("verified assets:", assets)

	names := bootcheck.TemplateNames()
	sort.Strings(names)
	fmt.Println("verified templates:", names)

	var buf bytes.Buffer
	if err := bootcheck.Templates.ExecuteTemplate(&buf, "greeting", map[string]any{
		"Name": "Ada", "Count": 3,
	}); err != nil {
		fmt.Println("execute error:", err)
		return
	}
	fmt.Println("render:", buf.String())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
verified assets: [migrations/0001_users.go migrations/0002_index.go]
verified templates: [footer greeting]
render: Hello Ada, you have 3 messages.
```

### Tests

The positive test relies on the package having loaded at all: if `init` had
panicked, the test binary would have crashed before any test ran. The negative
tests exercise the extracted helpers directly with a broken `fstest.MapFS` and a
malformed template body — proving the failure path returns an error, without ever
firing the real `init` panic.

Create `assets_test.go`:

```go
// assets_test.go
package bootcheck

import (
	"testing"
	"testing/fstest"
)

func TestPackageLoadedAndAssetsPresent(t *testing.T) {
	t.Parallel()

	names, err := AssetNames()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 {
		t.Fatalf("AssetNames() = %v, want 2 entries", names)
	}
	if Templates == nil {
		t.Fatal("Templates is nil; init did not populate it")
	}
}

func TestVerifyAssetsAcceptsCompleteFS(t *testing.T) {
	t.Parallel()

	good := fstest.MapFS{
		"migrations/0001_users.go": {Data: []byte("package migrations")},
		"migrations/0002_index.go": {Data: []byte("package migrations")},
	}
	if err := verifyAssets(good, requiredMigrations); err != nil {
		t.Fatalf("verifyAssets on complete FS = %v, want nil", err)
	}
}

func TestVerifyAssetsRejectsMissingFile(t *testing.T) {
	t.Parallel()

	broken := fstest.MapFS{
		"migrations/0001_users.go": {Data: []byte("package migrations")},
		// 0002_index.go deliberately absent
	}
	if err := verifyAssets(broken, requiredMigrations); err == nil {
		t.Fatal("verifyAssets on incomplete FS = nil, want error")
	}
}

func TestParseTemplatesRejectsMalformed(t *testing.T) {
	t.Parallel()

	_, err := parseTemplates(map[string]string{
		"broken": `{{.Unclosed`,
	})
	if err == nil {
		t.Fatal("parseTemplates on malformed body = nil, want error")
	}
}

func TestParseTemplatesAcceptsValid(t *testing.T) {
	t.Parallel()

	tmpl, err := parseTemplates(map[string]string{
		"ok": `{{.X}}`,
	})
	if err != nil {
		t.Fatalf("parseTemplates on valid body = %v, want nil", err)
	}
	if tmpl.Lookup("ok") == nil {
		t.Fatal("parsed set missing template \"ok\"")
	}
}
```

## Review

The fail-fast is correct when the binary refuses to start unless every required
asset is present and every template parses — and when that logic is nonetheless
testable. The design that achieves both is the split: pure helpers
(`verifyAssets`, `parseTemplates`) that return errors, wrapped by an `init()` that
panics. `TestPackageLoadedAndAssetsPresent` passing at all is proof the real `init`
succeeded (a panic there would crash the whole test binary), while the negative
tests drive the helpers with a broken `fstest.MapFS` to confirm the error path,
without ever tripping the panic.

The mistake to avoid is putting un-testable logic directly in `init()` with no
extractable helper — then the only way to observe the failure is to actually
mis-package the binary. Keep the checkable logic in a function over `fs.FS`; let
`init()` be a thin panic wrapper. And remember the boundary of this exception: this
is legitimate because the checks are cheap, external-dependency-free, and
build-time-answerable. Opening a database or dialing a service in `init()` is none
of those and does not qualify.

## Resources

- [embed package: go:embed and embed.FS](https://pkg.go.dev/embed) — baking assets into the binary; `embed.FS` implements `fs.FS`.
- [text/template: Template.Parse](https://pkg.go.dev/text/template#Template.Parse) — parsing a named template body into a set; the returned error is what the fail-fast escalates to a panic.
- [testing/fstest.MapFS](https://pkg.go.dev/testing/fstest#MapFS) — an in-memory `fs.FS` for exercising the broken-asset path.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-testmain-bootstrap-vs-init.md](08-testmain-bootstrap-vs-init.md)
