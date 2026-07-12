# Exercise 7: Fail the build if a local replace directive leaks to release

The workspace lets you develop against local modules with no `replace`. But
developers still reach for `replace example.com/mono/platform => ../platform`, it
works on their laptop, and it breaks the tagged release build that has no sibling
directory. This exercise builds the release gate that stops it: a guard that
parses `go.mod` with `golang.org/x/mod/modfile` and fails if any `replace` points
at a local filesystem path, while allowing a legitimate module-to-module
`replace` that pins a version.

## What you'll build

```text
relguard/                     module: example.com/mono/relguard
  go.mod                      requires golang.org/x/mod
  relguard.go                 CheckNoLocalReplace(name, data), CheckFile(path)
  relguard_test.go            local-replace vs clean vs module-replace cases
  cmd/demo/main.go            checks a written go.mod and a leaked one
```

- Files: `relguard.go`, `relguard_test.go`, `cmd/demo/main.go`.
- Implement: `CheckNoLocalReplace(name string, data []byte) error` using `modfile.Parse`, returning an error wrapping `ErrLocalReplace` for filesystem replacements; and `CheckFile(path string)` reading with `os.ReadFile`.
- Test: a `go.mod` with `=> ../platform` returns an error (`errors.Is` `ErrLocalReplace`); a clean `go.mod` returns `nil`; a module-to-module `replace ... => host v1.2.3` is allowed.
- Verify: `go test -count=1 -race ./...` (needs `golang.org/x/mod` from the module cache/proxy).

Set up the module. This one has a real dependency, so let `go` fetch it:

```bash
go get golang.org/x/mod
```

### Why parse go.mod instead of grepping it

You could `grep replace go.mod`, but a grep cannot tell a filesystem replacement
(`=> ../platform`, the leak) from a module replacement (`=> example.com/fork
v1.2.3`, a legitimate fork pin) — and it trips over comments, block form, and
whitespace. `golang.org/x/mod/modfile` is the same parser the go command uses. It
returns a `*modfile.File` whose `Replace` slice holds `*modfile.Replace` values,
each with an `Old` and a `New` `module.Version`.

The discriminator is precise and comes straight from module semantics: a
**filesystem** replacement has an empty `New.Version` — its `New.Path` is a
directory, not a module version. A **module-to-module** replacement always carries
a version in `New.Version`. So the guard flags exactly the replacements whose
`New.Version == ""`. That is the leak: it resolves to a local directory that
exists on the developer's machine and nowhere else. A version-pinned replacement
is reproducible from the proxy and is allowed.

The guard wraps a package sentinel `ErrLocalReplace` with `%w`, so CI can branch
on the condition with `errors.Is`, and the message names every offending target
so the failure is actionable.

Create `relguard.go`:

```go
package relguard

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/mod/modfile"
)

// ErrLocalReplace is the sentinel a release gate matches when a go.mod carries a
// filesystem replace directive that would break a tagged build.
var ErrLocalReplace = errors.New("go.mod has a local filesystem replace directive")

// CheckNoLocalReplace parses go.mod bytes (name is used only in errors) and
// returns an error wrapping ErrLocalReplace if any replace targets a local path.
// A module-to-module replace that pins a version is allowed.
func CheckNoLocalReplace(name string, data []byte) error {
	f, err := modfile.Parse(name, data, nil)
	if err != nil {
		return fmt.Errorf("parse %s: %w", name, err)
	}

	var local []string
	for _, r := range f.Replace {
		if isLocalPath(r.New.Path, r.New.Version) {
			local = append(local, fmt.Sprintf("%s => %s", r.Old.Path, r.New.Path))
		}
	}
	if len(local) > 0 {
		return fmt.Errorf("%s: %w: %s", name, ErrLocalReplace, strings.Join(local, ", "))
	}
	return nil
}

// isLocalPath reports whether a replace target is a filesystem path. A module
// replacement always has a version; a filesystem replacement never does.
func isLocalPath(path, version string) bool {
	if version != "" {
		return false
	}
	// No version means the target is a directory: relative or absolute.
	return strings.HasPrefix(path, "./") ||
		strings.HasPrefix(path, "../") ||
		strings.HasPrefix(path, "/") ||
		path == "." || path == ".."
}

// CheckFile reads a go.mod from disk and runs CheckNoLocalReplace on it.
func CheckFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	return CheckNoLocalReplace(path, data)
}
```

### The runnable demo

The demo writes two `go.mod` files into a temp directory — one clean, one with a
leaked `../platform` replace — and runs `CheckFile` on each, printing the verdict.
Writing then reading exercises the `os.ReadFile` path a CI job would use.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"

	"example.com/mono/relguard"
)

func main() {
	dir, err := os.MkdirTemp("", "relguard")
	if err != nil {
		fmt.Println("mkdir:", err)
		return
	}
	defer os.RemoveAll(dir)

	// Change into the temp dir so CheckFile sees short, deterministic names
	// ("clean.mod", "leaked.mod") in its error messages instead of an
	// absolute /var/folders/... path that differs on every run.
	if err := os.Chdir(dir); err != nil {
		fmt.Println("chdir:", err)
		return
	}

	clean := "module example.com/mono/api\n\ngo 1.26\n\nrequire example.com/mono/platform v1.4.0\n"
	leaked := clean + "\nreplace example.com/mono/platform => ../platform\n"

	_ = os.WriteFile("clean.mod", []byte(clean), 0o644)
	_ = os.WriteFile("leaked.mod", []byte(leaked), 0o644)

	fmt.Printf("clean:  %v\n", relguard.CheckFile("clean.mod") == nil)
	fmt.Printf("leaked: %v\n", relguard.CheckFile("leaked.mod"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
clean:  true
leaked: leaked.mod: go.mod has a local filesystem replace directive: example.com/mono/platform => ../platform
```

### Tests

The test is a pure function over bytes — no filesystem needed for the core cases.
It covers the three shapes that matter: a filesystem replace is rejected
(`errors.Is` `ErrLocalReplace`), a clean `go.mod` passes, and a module-to-module
replace pinning a version is allowed (a fork pin is legitimate and reproducible).
A final case drives `CheckFile` through `t.TempDir()` to cover the `os.ReadFile`
path.

Create `relguard_test.go`:

```go
package relguard

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckNoLocalReplace(t *testing.T) {
	t.Parallel()

	const base = "module example.com/mono/api\n\ngo 1.26\n\nrequire example.com/mono/platform v1.4.0\n"

	tests := []struct {
		name    string
		gomod   string
		wantErr bool
	}{
		{
			name:    "clean go.mod",
			gomod:   base,
			wantErr: false,
		},
		{
			name:    "filesystem replace leaks",
			gomod:   base + "\nreplace example.com/mono/platform => ../platform\n",
			wantErr: true,
		},
		{
			name:    "relative dot-slash replace leaks",
			gomod:   base + "\nreplace example.com/mono/platform => ./local/platform\n",
			wantErr: true,
		},
		{
			name:    "module-to-module replace is allowed",
			gomod:   base + "\nreplace example.com/mono/platform => example.com/fork/platform v1.4.1\n",
			wantErr: false,
		},
		{
			name:    "replace block with a local target leaks",
			gomod:   base + "\nreplace (\n\texample.com/mono/platform => ../platform\n)\n",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := CheckNoLocalReplace("go.mod", []byte(tc.gomod))
			if tc.wantErr {
				if !errors.Is(err, ErrLocalReplace) {
					t.Fatalf("err = %v, want it to wrap ErrLocalReplace", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
		})
	}
}

func TestCheckFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "go.mod")
	content := "module example.com/mono/api\n\ngo 1.26\n\nreplace example.com/mono/platform => ../platform\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := CheckFile(path); !errors.Is(err, ErrLocalReplace) {
		t.Fatalf("CheckFile = %v, want it to wrap ErrLocalReplace", err)
	}
}
```

## Review

The guard is correct when it flags exactly the filesystem replacements and nothing
else: `New.Version == ""` is the signal that a replace target is a directory rather
than a proxy-resolvable module, so `=> ../platform` and `=> ./local/platform` fail
while `=> example.com/fork/platform v1.4.1` passes. Wrapping `ErrLocalReplace` with
`%w` lets the release job match the condition with `errors.Is` and print the
offending targets.

The mistake this exercise exists to prevent is treating a local `replace` as
harmless because "it builds". It builds *for you*. Run this guard in CI on every
service's `go.mod` before a tag, and pair it with a check that `go.work` is not
present in the release artifact — together they close the two ways a workspace's
convenience leaks into a release that then fails to build from the proxy.

## Resources

- [`golang.org/x/mod/modfile`](https://pkg.go.dev/golang.org/x/mod/modfile) — `Parse`, `File.Replace`, and the `Replace`/`module.Version` types.
- [`golang.org/x/mod/module`](https://pkg.go.dev/golang.org/x/mod/module#Version) — the `Version{Path, Version}` pair a replace resolves to.
- [Go Modules Reference: replace directive](https://go.dev/ref/mod#go-mod-file-replace) — filesystem vs module replacements.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-reconcile-versions-with-work-sync.md](08-reconcile-versions-with-work-sync.md)
