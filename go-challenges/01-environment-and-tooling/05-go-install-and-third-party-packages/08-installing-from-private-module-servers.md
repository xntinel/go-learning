# Exercise 8: Installing from Private and Corporate Module Servers

A service whose dependencies live half on the public proxy and half on a private
corporate module server needs its toolchain configured deliberately: `GOPRIVATE`
to skip the public proxy and checksum database for internal paths, `GOPROXY` to
describe the fetch chain, and git to handle authentication. This exercise builds a
`GOPROXY` chain parser — the logic a CI setup script uses to validate that
configuration — and walks the surrounding environment.

This module is self-contained and uses only the standard library.

## What you'll build

```text
goproxycfg/                     independent module: example.com/goproxycfg
  go.mod
  goproxycfg.go                 ParseGOPROXY(s) ([]Hop, error); Hop.FallThroughAny; IsTerminal
  goproxycfg_test.go            table test of the chain + fallback semantics; Example
  cmd/demo/
    main.go                     parses a representative GOPROXY value
```

Files: `goproxycfg.go`, `goproxycfg_test.go`, `cmd/demo/main.go`.
Implement: `ParseGOPROXY(s string) ([]Hop, error)` honoring comma (fall through on 404/410) vs pipe (fall through on any error).
Test: table cases covering comma/pipe separators, `direct`/`off` terminals, and malformed input.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/01-environment-and-tooling/05-go-install-and-third-party-packages/08-installing-from-private-module-servers/cmd/demo
cd go-solutions/01-environment-and-tooling/05-go-install-and-third-party-packages/08-installing-from-private-module-servers
```

### The environment a private module needs

Fetching `example.internal/lib@v1.0.0` from a corporate git server, not the
public proxy, takes a coordinated set of settings, all durable via `go env -w`:

```bash
go env -w GOPRIVATE='example.internal,*.corp.example.com'
go env -w GOPROXY='https://proxy.corp.example.com,direct'
git config --global url."https://token@git.corp.example.com/".insteadOf "https://git.corp.example.com/"
```

`GOPRIVATE` is a comma-separated glob list; a module whose path matches is fetched
*directly from VCS* and is *not* verified against the public checksum database, so
its path never leaks to `proxy.golang.org` and its (private) hashes are not
expected in `sum.golang.org`. `GOPROXY` is the fetch chain the next section
parses. `GOINSECURE` (another glob list) permits plain HTTP / unverified TLS for
matching paths in constrained environments; `GOVCS` restricts which VCS may serve
which paths. Authentication is *not* the `go` command's job — it shells out to
git, which reads credentials from `url.insteadOf` rewrites or a `.netrc` file.
Miss the git config and the fetch fails with an authentication error from git;
add it and the same `go get` succeeds. And with `GOPROXY=off`, a public
`go get` fails deterministically rather than reaching the network at all — the
switch that proves your proxy configuration is in control.

### GOPROXY chain semantics

`GOPROXY` is a list of sources separated by commas or pipes, tried left to right.
The separator before each source decides *when* the toolchain falls through to
the next one:

- a comma (`,`) means "fall through only on a not-found (HTTP 404/410)";
- a pipe (`|`) means "fall through on *any* error";
- the keywords `direct` (fetch straight from VCS) and `off` (do not download,
  fail instead) are terminal sources.

So `https://proxy.corp,direct` tries the corporate proxy and, if it returns
404/410 for a module, falls back to fetching directly from VCS — but a *500* from
the proxy is a hard failure. Written with a pipe, `https://proxy.corp|direct`
would fall back on any error, including that 500. Encoding this in a parser is
what lets a setup script validate the chain before a build ever runs.

Create `goproxycfg.go`:

```go
// goproxycfg.go
package goproxycfg

import (
	"fmt"
	"strings"
)

// Hop is one source in a GOPROXY chain. FallThroughAny reports whether the
// toolchain falls through to the next hop on ANY error (the source was preceded
// by a pipe) rather than only on a 404/410 (preceded by a comma). It is false
// for the first hop, which has no preceding separator.
type Hop struct {
	Source         string
	FallThroughAny bool
}

// IsTerminal reports whether the hop's source is a terminal keyword: "direct"
// (fetch from VCS) or "off" (refuse to download).
func (h Hop) IsTerminal() bool {
	return h.Source == "direct" || h.Source == "off"
}

// ParseGOPROXY splits a GOPROXY value into its ordered hops, recording for each
// whether it falls through on any error (pipe) or only on not-found (comma). It
// rejects an empty value and an empty entry (for example a trailing separator).
func ParseGOPROXY(s string) ([]Hop, error) {
	if strings.TrimSpace(s) == "" {
		return nil, fmt.Errorf("GOPROXY is empty")
	}

	var hops []Hop
	start := 0
	fallAny := false // separator preceding the NEXT token

	flush := func(end int, pipe bool) error {
		token := strings.TrimSpace(s[start:end])
		if token == "" {
			return fmt.Errorf("GOPROXY has an empty entry near byte %d", start)
		}
		hops = append(hops, Hop{Source: token, FallThroughAny: fallAny})
		fallAny = pipe
		start = end + 1
		return nil
	}

	for i, r := range s {
		switch r {
		case ',':
			if err := flush(i, false); err != nil {
				return nil, err
			}
		case '|':
			if err := flush(i, true); err != nil {
				return nil, err
			}
		}
	}
	if err := flush(len(s), false); err != nil {
		return nil, err
	}
	return hops, nil
}
```

### The demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"os"

	"example.com/goproxycfg"
)

func main() {
	hops, err := goproxycfg.ParseGOPROXY("https://proxy.corp|https://proxy.golang.org,direct")
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid GOPROXY:", err)
		os.Exit(1)
	}
	for i, h := range hops {
		fmt.Printf("%d: source=%s fallThroughAny=%v terminal=%v\n",
			i, h.Source, h.FallThroughAny, h.IsTerminal())
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
0: source=https://proxy.corp fallThroughAny=false terminal=false
1: source=https://proxy.golang.org fallThroughAny=true terminal=false
2: source=direct fallThroughAny=false terminal=true
```

(Hop 1 was preceded by a pipe, so it falls through on any error; hop 2 is the
terminal `direct`.)

### The test

The table covers the two separators, the `direct`/`off` terminals, and malformed
input (empty, trailing separator). The `Example` pins a simple two-hop chain.

Create `goproxycfg_test.go`:

```go
// goproxycfg_test.go
package goproxycfg

import (
	"fmt"
	"testing"
)

func TestParseGOPROXY(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    []Hop
		wantErr bool
	}{
		{
			name: "comma then direct",
			in:   "https://proxy.corp,direct",
			want: []Hop{{"https://proxy.corp", false}, {"direct", false}},
		},
		{
			name: "pipe falls through on any error",
			in:   "https://a|https://b,direct",
			want: []Hop{{"https://a", false}, {"https://b", true}, {"direct", false}},
		},
		{
			name: "off is terminal",
			in:   "off",
			want: []Hop{{"off", false}},
		},
		{name: "empty", in: "", wantErr: true},
		{name: "trailing separator", in: "https://a,", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseGOPROXY(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseGOPROXY(%q) err = nil, want error", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseGOPROXY(%q) unexpected err = %v", tc.in, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ParseGOPROXY(%q) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("ParseGOPROXY(%q)[%d] = %+v, want %+v", tc.in, i, got[i], tc.want[i])
				}
			}
		})
	}
}

func ExampleParseGOPROXY() {
	hops, _ := ParseGOPROXY("https://proxy.corp,direct")
	for _, h := range hops {
		fmt.Printf("%s terminal=%v\n", h.Source, h.IsTerminal())
	}
	// Output:
	// https://proxy.corp terminal=false
	// direct terminal=true
}
```

## Review

The parser is correct when the separator *before* each source sets its fall-through
behavior — comma to false, pipe to true — and terminal keywords are recognized;
malformed input (empty value, empty entry) is rejected rather than silently
producing a bogus hop. The operational trap is disabling supply-chain checks
globally (`GOSUMDB=off`) to work around a private-fetch failure, when the scoped
fix is `GOPRIVATE`/`GONOSUMDB` for just the internal paths. Confirm the parser
with `go test -race ./...`; confirm the environment with `go env GOPRIVATE GOPROXY`
after `go env -w`, and by watching a public `go get` fail under `GOPROXY=off`.

## Resources

- [Go Modules Reference: GOPROXY protocol and environment](https://go.dev/ref/mod#environment-variables) — `GOPROXY`, `GOPRIVATE`, `GONOSUMDB`, `GOINSECURE`, `GOVCS`.
- [Go Modules Reference: private modules](https://go.dev/ref/mod#private-modules) — direct fetch, checksum-database bypass, and authentication.
- [`go env -w`](https://pkg.go.dev/cmd/go#hdr-Print_Go_environment_information) — writing durable configuration to the `go/env` file.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-reproducible-tooling-with-tool-directive.md](07-reproducible-tooling-with-tool-directive.md) | Next: [09-supply-chain-integrity-gosum-and-checksumdb.md](09-supply-chain-integrity-gosum-and-checksumdb.md)
