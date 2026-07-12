# Exercise 1: Config loader that depends on fs.FS, tested with MapFS

The most common filesystem dependency in a backend service is a config loader.
This exercise builds one that depends on the `fs.FS` interface rather than `os`,
so it is hermetic and injectable: production hands it an `os.DirFS` or an
`embed.FS`, every test hands it a `fstest.MapFS`. No temp dirs, no cleanup, no
disk.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
configloader/                 independent module: example.com/configloader
  go.mod                      go 1.26
  loader.go                   Config; ErrMissingKey; Load(fs.FS, name) (Config, error)
  cmd/
    demo/
      main.go                 loads an embedded MapFS config and prints it
  loader_test.go              table-driven MapFS cases + fstest.TestFS smoke check
```

- Files: `loader.go`, `cmd/demo/main.go`, `loader_test.go`.
- Implement: `Load(fsys fs.FS, name string) (Config, error)` that parses a
  `key=value` file via `fs.ReadFile`, skips blanks and `#` comments, fills
  `Host`/`Port`, returns the sentinel `ErrMissingKey` when a required key is
  absent and a wrapped `strconv` error on a non-numeric port.
- Test: table-driven `t.Parallel` subtests for success, missing-required-key,
  missing-file (`fs.ErrNotExist`), comment-ignoring, and invalid-port, plus a
  `fstest.TestFS` check that the fixture honors the `fs.FS` contract.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/11-testing-filesystems-with-fstest/01-config-loader-over-fsfs/cmd/demo
cd go-solutions/12-testing-ecosystem/11-testing-filesystems-with-fstest/01-config-loader-over-fsfs
```

### Why the signature is the whole design

The single most important line in this exercise is the signature:
`Load(fsys fs.FS, name string)`. Had it been `Load(path string)` with an
`os.ReadFile(path)` inside, the only way to test it would be to write a real
file to a real temp directory, and every one of the six cases below would carry
that cost. Taking an `fs.FS` inverts the dependency: the caller supplies the
filesystem. In `main` that is `os.DirFS(".")`; in a test it is a `MapFS`
literal built in three lines. The body uses `fs.ReadFile(fsys, name)`, the
top-level helper that reads a whole file from any `fs.FS` — it will take the
`ReadFileFS` fast path that `MapFS` implements, or fall back to `Open`+read on a
filesystem that does not.

The parsing itself is ordinary: split on newlines, trim, skip blanks and
`#`-comment lines, split each remaining line on the first `=`. Two error paths
are worth isolating because they are exactly what a test must pin. A non-numeric
port must not be silently dropped — `strconv.Atoi` fails and we wrap it with
`%w` so a caller can `errors.Is` it against `strconv.ErrSyntax` if it wants, and
so the original message survives. A missing required key (`host`) is a
*semantic* failure distinct from any I/O error, so it gets its own package-level
sentinel `ErrMissingKey`, wrapped with `%w`, checkable with `errors.Is`. A
missing *file* needs no sentinel of our own: `fs.ReadFile` already returns a
`*fs.PathError` wrapping `fs.ErrNotExist`, and wrapping that with `%w` preserves
it for `errors.Is(err, fs.ErrNotExist)`.

Create `loader.go`:

```go
package configloader

import (
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
)

// ErrMissingKey signals that a required key was absent from the config file.
var ErrMissingKey = errors.New("missing required key")

// Config is the parsed result of a key=value config file.
type Config struct {
	Host string
	Port int
}

// Load reads and parses a key=value config file from fsys. It skips blank
// lines and lines beginning with '#'. It requires a host key; a non-numeric
// port is an error. Load never touches the OS filesystem directly, so it is
// hermetic and testable with fstest.MapFS.
func Load(fsys fs.FS, name string) (Config, error) {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return Config{}, fmt.Errorf("load %s: %w", name, err)
	}

	var (
		cfg     Config
		hostSet bool
	)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		switch key {
		case "host":
			cfg.Host = val
			hostSet = true
		case "port":
			n, err := strconv.Atoi(val)
			if err != nil {
				return Config{}, fmt.Errorf("load %s: invalid port %q: %w", name, val, err)
			}
			cfg.Port = n
		}
	}
	if !hostSet {
		return Config{}, fmt.Errorf("load %s: %w: host", name, ErrMissingKey)
	}
	return cfg, nil
}
```

### The runnable demo

The demo builds a small `MapFS` in memory — the same kind of value a test uses —
and loads a config from it, so you can watch `Load` work against an injected
filesystem without any file on disk. In production the only change is swapping
the `MapFS` for `os.DirFS(dir)`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"testing/fstest"

	"example.com/configloader"
)

func main() {
	fsys := fstest.MapFS{
		"config.txt": {Data: []byte("# service config\nhost=db.internal\nport=5432\n")},
	}

	cfg, err := configloader.Load(fsys, "config.txt")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("host=%s port=%d\n", cfg.Host, cfg.Port)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
host=db.internal port=5432
```

### Tests

The tests are table-driven with a fresh `MapFS` built per case, run in parallel.
Success asserts field equality; the failure cases assert against the right
sentinel with `errors.Is` — `ErrMissingKey` for the missing-host case,
`fs.ErrNotExist` for the missing-file case (proving the wrapped `*fs.PathError`
survives), and a non-nil error for the invalid port. `TestFixtureHonorsContract`
runs `fstest.TestFS` over a fixture to prove the injected FS actually satisfies
the `fs.FS` contract — a habit worth keeping whenever a test's correctness rests
on the fixture behaving like a real filesystem. The `Example` documents the
happy path with a verified `// Output:` line.

Create `loader_test.go`:

```go
package configloader

import (
	"errors"
	"fmt"
	"io/fs"
	"testing"
	"testing/fstest"
)

func TestLoad(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		fsys    fstest.MapFS
		file    string
		want    Config
		wantErr error // sentinel to match with errors.Is, or nil
		wantAny bool  // expect some error without a specific sentinel
	}{
		{
			name: "success",
			fsys: fstest.MapFS{"config.txt": {Data: []byte("host=db.local\nport=5432\n")}},
			file: "config.txt",
			want: Config{Host: "db.local", Port: 5432},
		},
		{
			name: "comments and blanks ignored",
			fsys: fstest.MapFS{"config.txt": {Data: []byte("# header\n\nhost=db.local\n\n# trailer\nport=6543\n")}},
			file: "config.txt",
			want: Config{Host: "db.local", Port: 6543},
		},
		{
			name:    "missing required key",
			fsys:    fstest.MapFS{"config.txt": {Data: []byte("port=5432\n")}},
			file:    "config.txt",
			wantErr: ErrMissingKey,
		},
		{
			name:    "missing file",
			fsys:    fstest.MapFS{},
			file:    "absent.txt",
			wantErr: fs.ErrNotExist,
		},
		{
			name:    "invalid port",
			fsys:    fstest.MapFS{"config.txt": {Data: []byte("host=db.local\nport=abc\n")}},
			file:    "config.txt",
			wantAny: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := Load(tc.fsys, tc.file)
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Load err = %v, want errors.Is %v", err, tc.wantErr)
				}
			case tc.wantAny:
				if err == nil {
					t.Fatalf("Load err = nil, want an error")
				}
				if got != (Config{}) {
					t.Fatalf("Load returned partial config %+v on error", got)
				}
			default:
				if err != nil {
					t.Fatalf("Load err = %v, want nil", err)
				}
				if got != tc.want {
					t.Fatalf("Load = %+v, want %+v", got, tc.want)
				}
			}
		})
	}
}

func TestFixtureHonorsContract(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"config.txt": {Data: []byte("host=db.local\nport=5432\n")},
	}
	if err := fstest.TestFS(fsys, "config.txt"); err != nil {
		t.Fatalf("fixture violates fs.FS contract: %v", err)
	}
}

func ExampleLoad() {
	fsys := fstest.MapFS{
		"config.txt": {Data: []byte("host=api.local\nport=8080\n")},
	}
	cfg, _ := Load(fsys, "config.txt")
	fmt.Printf("%s:%d\n", cfg.Host, cfg.Port)
	// Output: api.local:8080
}
```

## Review

`Load` is correct when its result is a pure function of the file bytes: a valid
file yields the parsed `Config` and `nil`; a missing `host` yields
`ErrMissingKey`; a missing file yields `fs.ErrNotExist` (unwrapped from the
`*fs.PathError` that `fs.ReadFile` produced); a non-numeric port yields a wrapped
`strconv` error and — importantly — a zero `Config`, never a half-filled one.
The two habits to carry forward: take an `fs.FS`, never a path, so the code is
injectable; and wrap distinct failure modes with `%w` against sentinels so
callers assert with `errors.Is` instead of string matching. The
`fstest.TestFS` check is cheap insurance that the fixture you are trusting is a
real `fs.FS`. Run `go test -race` to confirm the parallel subtests share no
state — they do not, because each builds its own `MapFS`.

## Resources

- [`testing/fstest` (MapFS, MapFile, TestFS)](https://pkg.go.dev/testing/fstest) — the in-memory FS and the contract checker.
- [`io/fs` (FS, ReadFile, ErrNotExist)](https://pkg.go.dev/io/fs) — the interface and the sentinel errors.
- [`strings.Cut`](https://pkg.go.dev/strings#Cut) — the idiomatic single-split used to parse `key=value`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-validate-custom-fs-with-testfs.md](02-validate-custom-fs-with-testfs.md)
