# Exercise 8: Minimal Version Selection, go mod graph, and go mod why

When two dependencies transitively demand different versions of a third, what does
your build actually use? Go answers with Minimal Version Selection, and the answer
is auditable with `go mod graph` and `go mod why`. This exercise builds a module
with a real transitive graph and reasons about what MVS selected and why.

This module is fully self-contained: its own `go mod init`, all code inline, its
own tests. Nothing here imports another exercise.

## What you'll build

```text
mvsdemo/                    independent module: example.com/mvsdemo
  go.mod                    require golang.org/x/text (has its own transitive graph)
  langtag.go                Canonicalize(s) using language.Parse
  cmd/
    demo/
      main.go               canonicalizes a few tags
  langtag_test.go           table over valid and invalid tags
```

- Files: `langtag.go`, `cmd/demo/main.go`, `langtag_test.go`.
- Implement: `Canonicalize(s string) (string, error)` over `language.Parse`.
- Test: table over well-formed and unknown tags.
- Verify: read the graph with `go mod graph`, trace with `go mod why`, prune with `exclude`; `go build`, `go test -race`.

Set up the module:

```bash
mkdir -p ~/go-exercises/mvsdemo/cmd/demo
cd ~/go-exercises/mvsdemo
go mod init example.com/mvsdemo
go get golang.org/x/text/language
```

### What MVS actually selects

Picture a diamond. Your module requires two libraries, `A` and `B`. Both depend on
a third library `C`, but at different minimums: `A` requires `C v1.2.0`, `B`
requires `C v1.4.0`. What does the build use? Not the newest `C` that exists on the
proxy, and not the lowest — MVS selects the **maximum of the minimums** demanded
anywhere in the graph, so it picks `C v1.4.0`. If `A` later raises its requirement
to `C v1.5.0`, the selection rises to `v1.5.0`; if nobody requires anything above
`v1.4.0`, it stays at `v1.4.0` even when `v1.9.0` exists. This is the property that
makes Go builds deterministic and low-churn without a separate lockfile solver:
the build list is a pure function of the `require` graph, recomputed the same way
every time.

The corollary bites in practice: an unpinned dependency does not float upward on
its own. It sits at the highest floor the graph demands until you raise a `require`
(`go get pkg@version`) or some upgraded dependency raises it for you. A version
that is quietly behind — and quietly vulnerable — can persist for a long time,
which is why auditing the graph is a real task, not a curiosity.

### A real graph to read

`golang.org/x/text` has its own dependencies, so importing it gives a genuine
graph to inspect. The library canonicalizes a BCP-47 language tag with
`language.Parse`, which returns a normalized `Tag` (or an error for an unknown
subtag).

Create `langtag.go`:

```go
package mvsdemo

import "golang.org/x/text/language"

// Canonicalize parses a BCP-47 language tag and returns its canonical string
// form (for example "en-US" from "EN-us"). It returns an error for a
// well-formed but unknown tag.
func Canonicalize(s string) (string, error) {
	tag, err := language.Parse(s)
	if err != nil {
		return "", err
	}
	return tag.String(), nil
}
```

### The demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/mvsdemo"
)

func main() {
	for _, s := range []string{"en-US", "EN", "fr"} {
		out, _ := mvsdemo.Canonicalize(s)
		fmt.Printf("%-6s -> %s\n", s, out)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
en-US  -> en-US
EN     -> en
fr     -> fr
```

### The test

Create `langtag_test.go`:

```go
package mvsdemo

import "testing"

func TestCanonicalize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "region preserved", input: "en-US", want: "en-US"},
		{name: "lowercased", input: "EN", want: "en"},
		{name: "plain language", input: "fr", want: "fr"},
		{name: "unknown subtag", input: "zz-nonsense", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := Canonicalize(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Canonicalize(%q) = %q, nil; want error", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Canonicalize(%q) unexpected err = %v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("Canonicalize(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
```

### Reading and pruning the graph

`go mod graph` prints every edge `requirer requirement@version`. The edges from
your module and from `x/text` look like this (versions depend on what you
selected):

```text
example.com/mvsdemo golang.org/x/text@v0.38.0
golang.org/x/text@v0.38.0 golang.org/x/tools@v0.45.0
golang.org/x/text@v0.38.0 golang.org/x/mod@v0.36.0
golang.org/x/text@v0.38.0 golang.org/x/sync@v0.21.0
```

`go mod why` answers "why is this package in my build?" by printing the shortest
import path from your module to it:

```bash
go mod why golang.org/x/text/language
```

```text
# golang.org/x/text/language
example.com/mvsdemo
golang.org/x/text/language
```

`go mod why -m <module>` does the same at module granularity, which is what you run
when a dependency you have never heard of shows up in `go list -m all` and you need
to know who dragged it in. To bar a specific version — say a release with a known
regression — add an `exclude`, and MVS selects the next version that satisfies the
graph instead:

```bash
go mod edit -exclude golang.org/x/text@v0.38.0
go mod tidy
go list -m golang.org/x/text
```

`exclude` prunes exactly one version from the MVS computation; the build list then
shows whatever version MVS falls back to. (`retract`, by contrast, is declared by a
library's *own* author in its `go.mod` to warn everyone off a bad release; you
consume retractions, you do not write them for other people's modules.)

## Review

The graph reasoning is correct when the version in `go list -m all` equals the
maximum of the minimums the `go mod graph` edges demand — not the newest release
that exists. The mistakes to avoid: expecting MVS to pick the latest version (it
picks the maximum required floor, so pin deliberately when you want newer);
hand-editing `require` lines to "fix" a selection instead of using `go get`,
`exclude`, or a raised floor; and treating an unaudited unpinned dependency as
safe because "it builds" — it can lag at an old, vulnerable version indefinitely.
Use `go mod graph` and `go mod why` as the audit trail: they answer, mechanically,
what is in the build and how it got there, which is exactly the evidence a
supply-chain review needs.

## Resources

- [Minimal Version Selection](https://go.dev/ref/mod#minimal-version-selection) — the algorithm and the maximum-of-minimums rule.
- [go mod graph](https://go.dev/ref/mod#go-mod-graph) and [go mod why](https://go.dev/ref/mod#go-mod-why) — reading and tracing the module graph.
- [exclude and retract directives](https://go.dev/ref/mod#go-mod-file-exclude) — pruning versions from the build list.

---

Back to [07-go-mod-tool-directive-reproducible-tooling.md](07-go-mod-tool-directive-reproducible-tooling.md) | Next: [09-supply-chain-security-gate.md](09-supply-chain-security-gate.md)
