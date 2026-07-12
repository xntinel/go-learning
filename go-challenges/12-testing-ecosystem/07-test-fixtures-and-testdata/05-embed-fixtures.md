# Exercise 5: Embed fixtures into the test binary with //go:embed

`os.ReadFile` reads fixtures from the filesystem at test time, which is fine — until you want the test binary to be self-contained, or you run it from a directory where the loose files are not present. `//go:embed` compiles the fixtures directly into the binary, read through an `embed.FS`. This module validates service configs against fixtures that travel inside the binary.

## What you'll build

```text
schemacheck/                  independent module: example.com/schemacheck
  go.mod                      go 1.26
  validate.go                 Validate([]byte) (ServiceConfig, error); ErrSchema sentinel
  cmd/
    demo/
      main.go                 validates an inline config and prints it
  embed_test.go               //go:embed testdata; iterate ReadDir; validate each
  testdata/
    valid-api.json  valid-worker.json  invalid-port.json  invalid-empty-name.json
```

Files: `validate.go`, `cmd/demo/main.go`, `embed_test.go`, and four JSON fixtures.
Implement: `Validate` decoding a `ServiceConfig` strictly and enforcing schema rules, wrapping `ErrSchema` with `%w`.
Test: a `var fixtures embed.FS` with `//go:embed testdata`; iterate `fixtures.ReadDir("testdata")`, `ReadFile` each, and assert `valid-*` pass and `invalid-*` return `ErrSchema`; a second test proves the embedded read is independent of the working directory.
Verify: `go test -count=1 -race ./...`

### Why embed, and the embed rules that bite

`os.ReadFile` couples the test to the filesystem: the fixture must exist on disk, at a path relative to the package directory, at the moment the test runs. That coupling is usually harmless, but it breaks the moment you want a hermetic test binary (`go test -c` produces a binary you can run anywhere) or you invoke the test from a working directory where the loose `testdata/` is not reachable. `//go:embed` removes the coupling entirely: the compiler reads the files at build time and bakes their bytes into the binary. At runtime there is no filesystem access, so the test cannot fail on a missing or misplaced file.

The `//go:embed` directive attaches to a package-level variable and must be immediately followed by that declaration, with the `embed` package imported. `var fixtures embed.FS` with `//go:embed testdata` above it embeds the whole `testdata` tree. Two rules are worth committing to memory. First, `//go:embed` silently excludes any file or directory whose name begins with `.` or `_` — so a `.env` fixture or a `_scratch` subdirectory is not embedded unless you write the pattern with the `all:` prefix (`//go:embed all:testdata`). A directory literally named `testdata` embeds normally; the exclusion is only about the `.`/`_` leading character. Second, `embed.FS` implements `io/fs.FS`, so `ReadFile`, `ReadDir`, and `Open` all work uniformly — the same iteration code would run against a real directory wrapped as an `fs.FS`, which is what makes embedded fixtures a drop-in for on-disk ones.

Because an `embed.FS` uses forward-slash paths regardless of platform, join embedded paths with `path.Join`, not `filepath.Join`. The validator itself is a strict JSON schema check: unknown fields rejected, name required, replicas at least one, port in range — every violation wrapped over `ErrSchema`.

Create `validate.go`:

```go
package schemacheck

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrSchema wraps every schema violation for a service config.
var ErrSchema = errors.New("schema violation")

// ServiceConfig is a validated deployment descriptor.
type ServiceConfig struct {
	Name     string `json:"name"`
	Replicas int    `json:"replicas"`
	Port     int    `json:"port"`
}

// Validate strictly decodes and schema-checks a service config.
func Validate(data []byte) (ServiceConfig, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var c ServiceConfig
	if err := dec.Decode(&c); err != nil {
		return ServiceConfig{}, fmt.Errorf("%w: decode: %v", ErrSchema, err)
	}
	switch {
	case c.Name == "":
		return ServiceConfig{}, fmt.Errorf("%w: name is required", ErrSchema)
	case c.Replicas < 1:
		return ServiceConfig{}, fmt.Errorf("%w: replicas = %d, want >= 1", ErrSchema, c.Replicas)
	case c.Port < 1 || c.Port > 65535:
		return ServiceConfig{}, fmt.Errorf("%w: port = %d out of range", ErrSchema, c.Port)
	}
	return c, nil
}
```

Now the fixtures. The `valid-` prefix marks the ones expected to pass; `invalid-` marks the ones expected to be rejected.

Create `testdata/valid-api.json`:

```json
{"name": "api", "replicas": 3, "port": 8080}
```

Create `testdata/valid-worker.json`:

```json
{"name": "worker", "replicas": 1, "port": 9090}
```

Create `testdata/invalid-port.json`:

```json
{"name": "api", "replicas": 2, "port": 70000}
```

Create `testdata/invalid-empty-name.json`:

```json
{"name": "", "replicas": 1, "port": 80}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/schemacheck"
)

func main() {
	c, err := schemacheck.Validate([]byte(`{"name":"api","replicas":3,"port":8080}`))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s: %d replicas on port %d\n", c.Name, c.Replicas, c.Port)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
api: 3 replicas on port 8080
```

### The test

The embed variable and directive live in the test file. The first test iterates the embedded directory and validates each entry, keying the expectation off the filename prefix. The second changes the working directory to an empty temp dir and reads the same fixture — it still succeeds, because the bytes are in the binary, not on disk. (`t.Chdir` cannot be combined with `t.Parallel`, so that test runs serially.)

Create `embed_test.go`:

```go
package schemacheck

import (
	"embed"
	"errors"
	"fmt"
	"path"
	"strings"
	"testing"
)

//go:embed testdata
var fixtures embed.FS

func TestValidateEmbeddedFixtures(t *testing.T) {
	t.Parallel()

	entries, err := fixtures.ReadDir("testdata")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no embedded fixtures found")
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			data, err := fixtures.ReadFile(path.Join("testdata", name))
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			_, err = Validate(data)
			wantOK := strings.HasPrefix(name, "valid-")
			switch {
			case wantOK && err != nil:
				t.Fatalf("Validate(%s) = %v, want ok", name, err)
			case !wantOK && !errors.Is(err, ErrSchema):
				t.Fatalf("Validate(%s) err = %v, want ErrSchema", name, err)
			}
		})
	}
}

func TestEmbeddedIndependentOfCwd(t *testing.T) {
	t.Chdir(t.TempDir()) // no testdata/ here on disk

	data, err := fixtures.ReadFile("testdata/valid-api.json")
	if err != nil {
		t.Fatalf("ReadFile after chdir: %v", err)
	}
	if _, err := Validate(data); err != nil {
		t.Fatalf("Validate after chdir: %v", err)
	}
}

func ExampleValidate() {
	c, _ := Validate([]byte(`{"name":"api","replicas":2,"port":443}`))
	fmt.Println(c.Name, c.Replicas, c.Port)
	// Output: api 2 443
}
```

## Review

The suite is correct when every embedded fixture validates according to its name prefix and when the chdir test proves the read needs no filesystem. Embedding is the tool for a hermetic, relocatable test: the fixtures ship inside the binary, so nothing depends on loose files or the working directory. The rules that trip people are the exclusions — a `.`- or `_`-prefixed fixture is silently dropped unless you use `all:` — and the path style: `embed.FS` is forward-slash, so join with `path.Join`. Because `embed.FS` satisfies `io/fs.FS`, the exact iteration code here would also run against a real directory, which is why embedding is a drop-in rather than a rewrite.

## Resources

- [embed package](https://pkg.go.dev/embed) — the `//go:embed` directive, `embed.FS`, and the `.`/`_`/`all:` rules.
- [io/fs: FS and ReadDir](https://pkg.go.dev/io/fs#FS) — the interface `embed.FS` implements.
- [testing: T.Chdir](https://pkg.go.dev/testing#T.Chdir) — changing the working directory within a test.

---

Back to [04-glob-driven-table.md](04-glob-driven-table.md) | Next: [06-normalize-nondeterministic.md](06-normalize-nondeterministic.md)
