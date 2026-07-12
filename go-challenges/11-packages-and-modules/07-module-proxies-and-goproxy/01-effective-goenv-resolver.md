# Exercise 1: Resolve the effective module-download environment for a CI preflight

Before any `go build` runs, a build image should prove what its download settings
actually resolve to. This exercise upgrades the original `proxycfg` CLI into a
pure resolver that applies the documented defaults and the `GOPRIVATE` shorthand,
plus a thin command that prints the effective settings as a preflight report.

## What you'll build

```text
goenv/                     independent module: example.com/goenv
  go.mod                   go 1.26
  goenv.go                 type Settings; Resolve(env) Settings; Print(w, Settings)
  cmd/
    demo/
      main.go              prints resolved settings for four preflight scenarios
  goenv_test.go            table-driven Resolve tests + TestMainPrintsDefaults
  example_test.go          Example functions with // Output
```

- Files: `goenv.go`, `cmd/demo/main.go`, `goenv_test.go`, `example_test.go`.
- Implement: a pure `Resolve(env map[string]string) Settings` that computes the effective `GOPROXY`, `GOMODCACHE`, `GOSUMDB`, `GOPRIVATE`, `GONOPROXY`, `GONOSUMDB`, plus a `Print` that tabulates them.
- Test: table-driven cases over env maps (defaults, `GOPROXY=off`, custom `GOMODCACHE`, `GOPRIVATE` shorthand, explicit override, `GOSUMDB=off`) and the preserved `TestMainPrintsDefaults` contract.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/11-packages-and-modules/07-module-proxies-and-goproxy/01-effective-goenv-resolver/cmd/demo
cd go-solutions/11-packages-and-modules/07-module-proxies-and-goproxy/01-effective-goenv-resolver
go mod edit -go=1.26
```

### Why the resolver is pure

The original lesson read `GOPROXY` and `GOMODCACHE` straight from
`os.Getenv` inside `main`. That is fine for a one-shot print but impossible to
table-test without mutating the process environment, which makes tests
order-dependent and unsafe under `t.Parallel()`. The upgrade splits the job in
two: `Resolve(env map[string]string) Settings` is a pure function of its input
map — it never touches the real environment — and a thin `main` supplies the map.
Now every default and every shorthand is a deterministic table row.

The defaults encode the documented behavior. `GOPROXY` defaults to
`https://proxy.golang.org,direct`. `GOMODCACHE` defaults to `$GOPATH/pkg/mod`; if
`GOPATH` is present in the map we expand it, otherwise we emit the literal
`$GOPATH/pkg/mod` so the report is honest about what will be expanded at build
time. `GOSUMDB` defaults to `sum.golang.org`.

The one piece of real logic is the `GOPRIVATE` shorthand. `GOPRIVATE` is not a
matcher the `go` command consults directly for routing; it is a convenience that
seeds `GONOPROXY` and `GONOSUMDB`. So `Resolve` mirrors the toolchain: if
`GONOPROXY`/`GONOSUMDB` are unset, they take the value of `GOPRIVATE`; if they are
set explicitly, the explicit value wins. `cmp.Or` expresses "first non-empty"
cleanly for each of these defaults.

Create `goenv.go`:

```go
package goenv

import (
	"cmp"
	"fmt"
	"io"
	"text/tabwriter"
)

// Settings is the effective module-download environment a build image resolves
// during its CI preflight, before any go build runs.
type Settings struct {
	GOPROXY    string
	GOMODCACHE string
	GOSUMDB    string
	GOPRIVATE  string
	GONOPROXY  string
	GONOSUMDB  string
}

// Resolve computes the effective settings from a raw environment map by applying
// the documented defaults and the GOPRIVATE shorthand. It is pure: it never reads
// or mutates the process environment, so it is safe to table-test.
func Resolve(env map[string]string) Settings {
	proxy := cmp.Or(env["GOPROXY"], "https://proxy.golang.org,direct")

	modcache := env["GOMODCACHE"]
	if modcache == "" {
		if gp := env["GOPATH"]; gp != "" {
			modcache = gp + "/pkg/mod"
		} else {
			modcache = "$GOPATH/pkg/mod"
		}
	}

	sumdb := cmp.Or(env["GOSUMDB"], "sum.golang.org")

	private := env["GOPRIVATE"]
	// GOPRIVATE is shorthand: it seeds GONOPROXY and GONOSUMDB unless they are
	// set explicitly.
	noproxy := cmp.Or(env["GONOPROXY"], private)
	nosumdb := cmp.Or(env["GONOSUMDB"], private)

	return Settings{
		GOPROXY:    proxy,
		GOMODCACHE: modcache,
		GOSUMDB:    sumdb,
		GOPRIVATE:  private,
		GONOPROXY:  noproxy,
		GONOSUMDB:  nosumdb,
	}
}

// Print writes the settings as an aligned KEY value table.
func Print(w io.Writer, s Settings) {
	tw := tabwriter.NewWriter(w, 0, 0, 1, ' ', 0)
	fmt.Fprintf(tw, "GOPROXY\t%s\n", s.GOPROXY)
	fmt.Fprintf(tw, "GOMODCACHE\t%s\n", s.GOMODCACHE)
	fmt.Fprintf(tw, "GOSUMDB\t%s\n", s.GOSUMDB)
	fmt.Fprintf(tw, "GOPRIVATE\t%s\n", empty(s.GOPRIVATE))
	fmt.Fprintf(tw, "GONOPROXY\t%s\n", empty(s.GONOPROXY))
	fmt.Fprintf(tw, "GONOSUMDB\t%s\n", empty(s.GONOSUMDB))
	tw.Flush()
}

func empty(v string) string {
	if v == "" {
		return "(unset)"
	}
	return v
}
```

### The runnable demo

The demo runs the four preflight scenarios a build image cares about — plain
defaults, an offline `GOPROXY=off`, a custom cache directory, and a private
registry that must derive `GONOPROXY`/`GONOSUMDB` from `GOPRIVATE` — and prints
each resolved table. Using explicit maps (not the ambient environment) keeps the
output deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"

	"example.com/goenv"
)

func main() {
	scenarios := []struct {
		name string
		env  map[string]string
	}{
		{"defaults", map[string]string{}},
		{"offline", map[string]string{"GOPROXY": "off"}},
		{"custom cache", map[string]string{"GOMODCACHE": "/build/cache/mod"}},
		{"private registry", map[string]string{"GOPRIVATE": "*.corp.example.com"}},
	}
	for i, sc := range scenarios {
		if i > 0 {
			fmt.Fprintln(os.Stdout)
		}
		fmt.Fprintf(os.Stdout, "# %s\n", sc.name)
		goenv.Print(os.Stdout, goenv.Resolve(sc.env))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
# defaults
GOPROXY    https://proxy.golang.org,direct
GOMODCACHE $GOPATH/pkg/mod
GOSUMDB    sum.golang.org
GOPRIVATE  (unset)
GONOPROXY  (unset)
GONOSUMDB  (unset)

# offline
GOPROXY    off
GOMODCACHE $GOPATH/pkg/mod
GOSUMDB    sum.golang.org
GOPRIVATE  (unset)
GONOPROXY  (unset)
GONOSUMDB  (unset)

# custom cache
GOPROXY    https://proxy.golang.org,direct
GOMODCACHE /build/cache/mod
GOSUMDB    sum.golang.org
GOPRIVATE  (unset)
GONOPROXY  (unset)
GONOSUMDB  (unset)

# private registry
GOPROXY    https://proxy.golang.org,direct
GOMODCACHE $GOPATH/pkg/mod
GOSUMDB    sum.golang.org
GOPRIVATE  *.corp.example.com
GONOPROXY  *.corp.example.com
GONOSUMDB  *.corp.example.com
```

### Tests

The table drives every default and the shorthand, and `TestMainPrintsDefaults`
preserves the original contract: the default `GOPROXY` is the standard
`proxy,direct` chain. Because `Resolve` is pure, no test mutates the environment,
so every case runs under `t.Parallel()`.

Create `goenv_test.go`:

```go
package goenv

import (
	"bytes"
	"strings"
	"testing"
)

func TestResolve(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		env  map[string]string
		want Settings
	}{
		{
			name: "empty env applies documented defaults",
			env:  map[string]string{},
			want: Settings{
				GOPROXY:    "https://proxy.golang.org,direct",
				GOMODCACHE: "$GOPATH/pkg/mod",
				GOSUMDB:    "sum.golang.org",
			},
		},
		{
			name: "GOPROXY=off preserved verbatim",
			env:  map[string]string{"GOPROXY": "off"},
			want: Settings{
				GOPROXY:    "off",
				GOMODCACHE: "$GOPATH/pkg/mod",
				GOSUMDB:    "sum.golang.org",
			},
		},
		{
			name: "custom GOMODCACHE overrides default",
			env:  map[string]string{"GOMODCACHE": "/build/cache/mod"},
			want: Settings{
				GOPROXY:    "https://proxy.golang.org,direct",
				GOMODCACHE: "/build/cache/mod",
				GOSUMDB:    "sum.golang.org",
			},
		},
		{
			name: "GOPATH derives the default cache",
			env:  map[string]string{"GOPATH": "/home/ci/go"},
			want: Settings{
				GOPROXY:    "https://proxy.golang.org,direct",
				GOMODCACHE: "/home/ci/go/pkg/mod",
				GOSUMDB:    "sum.golang.org",
			},
		},
		{
			name: "GOPRIVATE seeds GONOPROXY and GONOSUMDB",
			env:  map[string]string{"GOPRIVATE": "*.corp.example.com"},
			want: Settings{
				GOPROXY:    "https://proxy.golang.org,direct",
				GOMODCACHE: "$GOPATH/pkg/mod",
				GOSUMDB:    "sum.golang.org",
				GOPRIVATE:  "*.corp.example.com",
				GONOPROXY:  "*.corp.example.com",
				GONOSUMDB:  "*.corp.example.com",
			},
		},
		{
			name: "explicit GONOPROXY overrides the GOPRIVATE shorthand",
			env: map[string]string{
				"GOPRIVATE": "*.corp.example.com",
				"GONOPROXY": "vcs.corp.example.com",
			},
			want: Settings{
				GOPROXY:    "https://proxy.golang.org,direct",
				GOMODCACHE: "$GOPATH/pkg/mod",
				GOSUMDB:    "sum.golang.org",
				GOPRIVATE:  "*.corp.example.com",
				GONOPROXY:  "vcs.corp.example.com",
				GONOSUMDB:  "*.corp.example.com",
			},
		},
		{
			name: "GOSUMDB=off disables the checksum database",
			env:  map[string]string{"GOSUMDB": "off"},
			want: Settings{
				GOPROXY:    "https://proxy.golang.org,direct",
				GOMODCACHE: "$GOPATH/pkg/mod",
				GOSUMDB:    "off",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Resolve(tt.env)
			if got != tt.want {
				t.Errorf("Resolve(%v):\n got %+v\nwant %+v", tt.env, got, tt.want)
			}
		})
	}
}

// TestMainPrintsDefaults preserves the original proxycfg contract: the default
// GOPROXY must be the standard proxy,direct chain.
func TestMainPrintsDefaults(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	Print(&buf, Resolve(map[string]string{}))
	out := buf.String()
	if !strings.Contains(out, "https://proxy.golang.org,direct") {
		t.Fatalf("default output missing standard GOPROXY:\n%s", out)
	}
	if !strings.Contains(out, "sum.golang.org") {
		t.Fatalf("default output missing default GOSUMDB:\n%s", out)
	}
}
```

Create `example_test.go`:

```go
package goenv

import (
	"fmt"
	"os"
)

func ExampleResolve() {
	s := Resolve(map[string]string{"GOPRIVATE": "*.corp.example.com"})
	fmt.Println(s.GONOPROXY)
	fmt.Println(s.GONOSUMDB)
	// Output:
	// *.corp.example.com
	// *.corp.example.com
}

func ExamplePrint() {
	Print(os.Stdout, Resolve(map[string]string{"GOPROXY": "off"}))
	// Output:
	// GOPROXY    off
	// GOMODCACHE $GOPATH/pkg/mod
	// GOSUMDB    sum.golang.org
	// GOPRIVATE  (unset)
	// GONOPROXY  (unset)
	// GONOSUMDB  (unset)
}
```

## Review

The resolver is correct when it is a pure function of its input map. The subtle
rule is the `GOPRIVATE` shorthand: it seeds `GONOPROXY` and `GONOSUMDB` only when
they are unset, so the explicit-override test row is the one that proves you did
not clobber a caller's intent. Keep the default `GOMODCACHE` honest — emit the
literal `$GOPATH/pkg/mod` when `GOPATH` is absent rather than guessing a home
directory, because the report is about what the build image will resolve, not what
this process happens to see. The mistake to avoid is reintroducing `os.Getenv`
inside `Resolve`; that turns a testable function back into the untestable
`main` the original had. Run `go test -race` and confirm every row, including the
preserved `TestMainPrintsDefaults`, holds.

## Resources

- [Go Modules Reference: Environment variables](https://go.dev/ref/mod#environment-variables) — the authoritative list and defaults for `GOPROXY`, `GOMODCACHE`, `GOSUMDB`, `GOPRIVATE`, `GONOPROXY`, `GONOSUMDB`.
- [`cmp.Or`](https://pkg.go.dev/cmp#Or) — returns the first non-zero argument; the clean way to express a default.
- [`text/tabwriter`](https://pkg.go.dev/text/tabwriter) — column alignment for the preflight report.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-goproxy-chain-parser.md](02-goproxy-chain-parser.md)
