# Exercise 9: Regenerate go.work Deterministically From A Script

A monorepo with dozens of modules needs its `go.work` rebuilt by tooling, not by
hand — a generator that adds every module, drops removed ones, and never produces
a malformed file or a nondeterministic diff. The right primitives are
`go work edit -use`/`-dropuse` to mutate the file and `go work edit -json` to read
it back as structured data. This exercise parses that JSON and validates the
regenerated workspace: every expected module present exactly once, and a `go`
version set.

## What you'll build

```text
workspacegen/                  module: example.com/monorepo/workspacegen
  go.mod                       go 1.26
  workspacegen.go              parse `go work edit -json`; validate the use set
  workspacegen_test.go         asserts presence, dedup, and idempotent regeneration
  cmd/
    demo/
      main.go                  prints the parsed go version and sorted use paths
```

- Files: `workspacegen.go`, `workspacegen_test.go`, `cmd/demo/main.go`.
- Implement: `ParseWorkFile([]byte) (*WorkFile, error)` and `Validate(wf, want []string) error` (sentinels `ErrNoGoVersion`, `ErrMissingModule`, `ErrDuplicateModule`).
- Test: a valid workspace validates; a duplicate `use` fails; a dropped module is gone after regeneration.
- Verify: the JSON shape matches `go work edit -json`; validation is deterministic regardless of `use` order.

Set up the module:

```bash
mkdir -p ~/monorepo/workspacegen/cmd/demo
cd ~/monorepo/workspacegen
go mod init example.com/monorepo/workspacegen
go mod edit -go=1.26
```

### Drive go.work with go work edit, read it back as JSON

A generator rebuilds the workspace by calling `go work edit` once per module, then
reads the result as JSON to verify it:

```bash
cd ~/mono
go work edit -go=1.26
for m in text services/greeter services/billing; do
	go work edit -use=./$m
done
go work edit -dropuse=./services/legacy    # remove a retired module
go work edit -json                          # structured read-back for verification
```

`go work edit -json` prints the workspace as JSON with this shape (the field names
are exactly those the `go` command emits):

```json
{
	"Go": "1.26",
	"Toolchain": "",
	"Use": [
		{"DiskPath": "./text", "ModulePath": "example.com/platform/text"},
		{"DiskPath": "./services/greeter", "ModulePath": "example.com/platform/greeter"},
		{"DiskPath": "./services/billing", "ModulePath": "example.com/platform/billing"}
	],
	"Replace": null
}
```

Using `-use`/`-dropuse` instead of appending text guarantees a well-formed file,
and reading it back with `-json` lets tooling assert invariants structurally
rather than by grepping. The parser below models that JSON and validates two
properties a good regeneration must hold: a non-empty `go` version, and every
expected module present exactly once (a duplicate `use` — the classic
text-munging bug — is rejected).

Create `workspacegen.go`:

```go
// workspacegen.go
package workspacegen

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// Sentinel errors returned by Validate.
var (
	ErrNoGoVersion     = errors.New("workspace: missing go version")
	ErrMissingModule   = errors.New("workspace: module not in use set")
	ErrDuplicateModule = errors.New("workspace: module listed more than once")
)

// WorkFile mirrors the JSON printed by `go work edit -json`.
type WorkFile struct {
	Go        string
	Toolchain string
	Use       []Use
	Replace   []Replace
}

// Use is one entry of the go.work use directive.
type Use struct {
	DiskPath   string
	ModulePath string
}

// Replace is one entry of the go.work replace directive.
type Replace struct {
	Old Module
	New Module
}

// Module is a path/version pair inside a replace directive.
type Module struct {
	Path    string
	Version string
}

// ParseWorkFile decodes the output of `go work edit -json`.
func ParseWorkFile(data []byte) (*WorkFile, error) {
	var wf WorkFile
	if err := json.Unmarshal(data, &wf); err != nil {
		return nil, fmt.Errorf("parsing go.work json: %w", err)
	}
	return &wf, nil
}

// UsePaths returns the workspace's use disk paths, sorted for a stable diff.
func (wf *WorkFile) UsePaths() []string {
	paths := make([]string, len(wf.Use))
	for i, u := range wf.Use {
		paths[i] = u.DiskPath
	}
	sort.Strings(paths)
	return paths
}

// Validate checks that the regenerated workspace has a go version and lists each
// wanted disk path exactly once.
func (wf *WorkFile) Validate(want []string) error {
	if wf.Go == "" {
		return ErrNoGoVersion
	}
	counts := make(map[string]int, len(wf.Use))
	for _, u := range wf.Use {
		counts[u.DiskPath]++
	}
	for path, n := range counts {
		if n > 1 {
			return fmt.Errorf("%q (%d times): %w", path, n, ErrDuplicateModule)
		}
	}
	for _, path := range want {
		if counts[path] == 0 {
			return fmt.Errorf("%q: %w", path, ErrMissingModule)
		}
	}
	return nil
}
```

### The demo

The demo parses a representative `go work edit -json` payload and prints the go
version and the sorted use paths a generator would verify.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/monorepo/workspacegen"
)

func main() {
	const jsonOut = `{
		"Go": "1.26",
		"Use": [
			{"DiskPath": "./services/billing"},
			{"DiskPath": "./text"},
			{"DiskPath": "./services/greeter"}
		]
	}`
	wf, err := workspacegen.ParseWorkFile([]byte(jsonOut))
	if err != nil {
		fmt.Println("parse error:", err)
		return
	}
	fmt.Println("go:", wf.Go)
	for _, p := range wf.UsePaths() {
		fmt.Println("use:", p)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
go: 1.26
use: ./services/billing
use: ./services/greeter
use: ./text
```

### Tests

The valid case pins the JSON shape and order-independent validation; the duplicate
case catches the text-munging bug; the drop case proves regeneration is idempotent
(re-reading after a `-dropuse` shows the module gone).

Create `workspacegen_test.go`:

```go
// workspacegen_test.go
package workspacegen

import (
	"errors"
	"testing"
)

const threeModules = `{
	"Go": "1.26",
	"Use": [
		{"DiskPath": "./text", "ModulePath": "example.com/platform/text"},
		{"DiskPath": "./services/greeter"},
		{"DiskPath": "./services/billing"}
	],
	"Replace": null
}`

func TestValidateOK(t *testing.T) {
	t.Parallel()
	wf, err := ParseWorkFile([]byte(threeModules))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// order-independent: want listed in a different order than the file
	want := []string{"./services/billing", "./text", "./services/greeter"}
	if err := wf.Validate(want); err != nil {
		t.Fatalf("Validate = %v, want nil", err)
	}
}

func TestValidateDuplicate(t *testing.T) {
	t.Parallel()
	const dup = `{"Go":"1.26","Use":[{"DiskPath":"./text"},{"DiskPath":"./text"}]}`
	wf, err := ParseWorkFile([]byte(dup))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := wf.Validate([]string{"./text"}); !errors.Is(err, ErrDuplicateModule) {
		t.Fatalf("Validate = %v, want ErrDuplicateModule", err)
	}
}

func TestValidateNoGoVersion(t *testing.T) {
	t.Parallel()
	wf, err := ParseWorkFile([]byte(`{"Use":[{"DiskPath":"./text"}]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := wf.Validate([]string{"./text"}); !errors.Is(err, ErrNoGoVersion) {
		t.Fatalf("Validate = %v, want ErrNoGoVersion", err)
	}
}

func TestRegenerationDropsModule(t *testing.T) {
	t.Parallel()
	// after `go work edit -dropuse=./services/legacy`, the re-read JSON omits it
	const afterDrop = `{"Go":"1.26","Use":[{"DiskPath":"./text"},{"DiskPath":"./services/greeter"}]}`
	wf, err := ParseWorkFile([]byte(afterDrop))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := wf.Validate([]string{"./services/legacy"}); !errors.Is(err, ErrMissingModule) {
		t.Fatalf("Validate = %v, want ErrMissingModule for the dropped module", err)
	}
	if err := wf.Validate([]string{"./text", "./services/greeter"}); err != nil {
		t.Fatalf("Validate of remaining modules = %v, want nil", err)
	}
}
```

## Review

The generator discipline is: mutate `go.work` only through `go work edit`
(`-use`, `-dropuse`, `-go`) and verify it by reading `-json`, never by
string-appending, which is how malformed files and noisy diffs appear. The parser
models the exact JSON the `go` command emits and enforces the two invariants a
regeneration must hold — a set `go` version and each module present exactly once —
returning wrapped sentinels so tooling branches on `errors.Is` rather than string
matching. `UsePaths` sorts, so the verification is order-independent and the diff
is stable across runs; that stability is what makes the regeneration idempotent,
which the drop test confirms by re-reading after a `-dropuse`.

## Resources

- [go command — Workspace maintenance](https://pkg.go.dev/cmd/go#hdr-Workspace_maintenance) — `go work edit` and its `-use`, `-dropuse`, `-go`, `-print`, `-json` flags.
- [Go Modules Reference — go.work files](https://go.dev/ref/mod#workspaces) — the directives the JSON models.
- [`encoding/json`](https://pkg.go.dev/encoding/json) — `Unmarshal` and struct-field matching used to decode the `-json` output.

---

Back to [00-concepts.md](00-concepts.md) | Next: [10-per-module-test-and-vet-across-workspace.md](10-per-module-test-and-vet-across-workspace.md)
