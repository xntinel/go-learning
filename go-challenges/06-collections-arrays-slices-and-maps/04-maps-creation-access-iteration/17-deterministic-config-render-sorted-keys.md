# Exercise 17: Deterministic Config Rendering with Sorted Keys

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A config renderer that turns `KEY=VALUE` lines into a canonical text file --
environment variables for a container, a `.properties` file, a diff-friendly
snapshot committed alongside infrastructure code -- has one hard requirement
its author usually does not write down: the same config must always render
to the exact same bytes. CI diffs the rendered output against a golden file,
a deploy pipeline hashes it to detect drift, a code reviewer reads the diff
to see what changed. None of that works if two runs of the identical config
can produce different byte sequences.

Go's map iteration order is not just unspecified, it is deliberately
re-randomized on every `range`, precisely so code cannot accidentally come
to depend on an order that was never promised. A renderer that builds its
output by ranging the map directly will pass every test that only checks
"does this look like the config" and then produce a flapping diff the first
time CI happens to seed the range differently than the golden file was
generated with. This module builds that renderer as a real command-line
tool -- it reads `KEY=VALUE` lines from stdin, the same shape as a `.env`
file or the output of `env`, and writes the byte-stable rendering to
stdout -- and proves both halves of the claim: the sorted renderer is
byte-stable across a hundred calls, and a version that renders straight off
map range order is not.

This module is fully self-contained: its own `go mod init`, an executable
command, and its tests. Nothing here imports another exercise.

## What you'll build

```text
configrender/                  module example.com/configrender
  go.mod                       go 1.24
  configrender.go               package main — ParseConfig(r, strict) (map[string]string, error); Render(cfg) string
  configrender_test.go          package main — ParseConfig table, 100-run byte equality, sorted-order golden,
                                empty config, raw-range flakiness demonstration, run() end to end
  main.go                       package main — -strict flag, exit codes
```

- Files: `configrender.go`, `configrender_test.go`, `main.go`.
- Implement: `ParseConfig(r io.Reader, strict bool) (map[string]string, error)`, which reads `KEY=VALUE` lines, skips blank lines and `#` comments, and either lets a repeated key overwrite (default) or rejects it with `ErrDuplicateKey` (`-strict`); malformed lines fail with `ErrMalformedLine`, an empty key with `ErrEmptyKey`, both wrapped with `%w` and the 1-based line number; `Render(cfg map[string]string) string`, which collects the keys via `slices.Sorted(maps.Keys(cfg))` and writes `"key=value\n"` lines in that sorted order.
- Tool: `configrender` reads the whole config from stdin and writes the sorted rendering to stdout. `-strict` rejects a repeated key instead of letting the last one win. Exit 0 on success, exit 2 for any parse failure or unknown flag (a usage error the caller fixes by changing the input or the command line), exit 1 is reserved for a runtime failure -- none exists in this tool, since rendering a parsed map cannot fail.
- Test: `ParseConfig`'s table (comments, blank lines, a value containing `=`, an empty value, a malformed line, an empty key, a duplicate key under both modes, empty input); `Render` called 100 times on the same map produces byte-identical output every time; a small map renders to the expected sorted string exactly; an empty and a nil map both render to the empty string; an unexported `rawKeyOrder` helper, reachable only from the test file, demonstrating that a plain map range produces more than one distinct ordering across 50 calls; `run` end to end over a `strings.Reader` and a `bytes.Buffer`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/04-maps-creation-access-iteration/17-deterministic-config-render-sorted-keys
cd go-solutions/06-collections-arrays-slices-and-maps/04-maps-creation-access-iteration/17-deterministic-config-render-sorted-keys
go mod edit -go=1.24
```

### Why sorting the keys is the whole fix, and why "it worked once" is not proof

The naive renderer -- `for k, v := range cfg { b.WriteString(k + "=" + v + "\n") }`
-- looks correct because it is: every key and value in the map does end up
in the output, with nothing missing and nothing duplicated. What it does not
guarantee is the *order* those lines appear in, and Go makes that concrete
rather than theoretical: `for k, v := range m` visits entries in an order
the runtime seeds randomly at the start of every range, even over the same
unmodified map, even in the same process. Two consecutive calls to a naive
renderer, back to back, with no modification to the map in between, can
legitimately produce two different byte strings. A test that calls the
naive renderer once and asserts the output looks right will pass -- it has
to, because "looks right" only checks content, not order -- and will tell
you nothing about whether the *next* call produces the same bytes.

The fix is one line, not a redesign: collect the keys, sort them, then
range the sorted slice instead of the map. `slices.Sorted(maps.Keys(cfg))`
does exactly that in modern Go -- `maps.Keys` returns an `iter.Seq[K]` over
the map's keys, and `slices.Sorted` drains that iterator into a sorted
`[]string`. Ranging the *slice* afterward is deterministic because slices
have a real, fixed order; the only place randomness could enter -- the
single call to `maps.Keys` -- happens once, and its output is immediately
pinned by the sort. `Render` never ranges the map for anything but building
that one sorted key list; the value lookup `cfg[k]` inside the loop is a
plain indexed read, not a second range, so it introduces no additional
ordering question.

The parsing half of the tool has its own small design decision worth
naming: what happens when the same key appears twice in the input. The
default is last-write-wins, the same overlay semantics `maps.Copy` gives
you when layering one config source onto another -- a later line silently
replaces an earlier one for the same key. `-strict` turns that same
situation into a parse error instead, for callers who consider a repeated
key in a single file a sign of a broken generator rather than an intentional
override. Both paths are real, documented behavior of `ParseConfig`; neither
is "the bug" -- the bug this module actually teaches lives only in how the
*rendering* step chooses to range the map, never in how parsing handles a
duplicate key.

Create `configrender.go`:

```go
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
)

// ErrMalformedLine is returned when a stdin line is not "KEY=VALUE".
var ErrMalformedLine = errors.New("configrender: malformed line, want KEY=VALUE")

// ErrEmptyKey is returned when a line's key, the part before '=', is empty.
var ErrEmptyKey = errors.New("configrender: empty key")

// ErrDuplicateKey is returned in strict mode when a key appears more than
// once.
var ErrDuplicateKey = errors.New("configrender: duplicate key")

// ParseConfig reads "KEY=VALUE" lines from r into a map. Blank lines and
// lines starting with '#' are skipped. Outside strict mode a repeated key
// silently overwrites the earlier value -- the same last-write-wins
// layering maps.Copy performs when overlaying one map onto another. In
// strict mode a repeated key is a parse error, ErrDuplicateKey. Every
// error is wrapped with %w and the 1-based line number that failed, so a
// caller can locate the exact offending line in a large file.
func ParseConfig(r io.Reader, strict bool) (map[string]string, error) {
	cfg := make(map[string]string)
	sc := bufio.NewScanner(r)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		key, value, ok := strings.Cut(text, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: %w: %q", line, ErrMalformedLine, text)
		}
		if key == "" {
			return nil, fmt.Errorf("line %d: %w", line, ErrEmptyKey)
		}
		if _, exists := cfg[key]; exists && strict {
			return nil, fmt.Errorf("line %d: %w: %q", line, ErrDuplicateKey, key)
		}
		cfg[key] = value
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("configrender: reading input: %w", err)
	}
	return cfg, nil
}

// Render renders cfg as "key=value" lines, one per entry, each terminated
// by a newline, with keys sorted lexically. Because it sorts the keys
// before ranging, Render's output is byte-identical for the same cfg no
// matter how many times it is called or what process ran it.
func Render(cfg map[string]string) string {
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(cfg)) {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(cfg[k])
		b.WriteByte('\n')
	}
	return b.String()
}
```

### The tool

`configrender` reads the entire config from stdin -- there is no filename
argument, because the tool's whole purpose is to sit in a pipeline between
whatever produces a config (`env`, a template renderer, a secrets fetch) and
whatever consumes the canonical form (a hash step, a diff, a file write).
`run` takes the flag arguments, an `io.Reader` for stdin, and an `io.Writer`
for stdout, so a test can drive it with a `strings.Reader` and a
`bytes.Buffer` without touching a real process. `flag.NewFlagSet` with
`flag.ContinueOnError` lets `run` return the parse error instead of the
package-level flag set calling `os.Exit` out from under a test. Every
failure `run` can produce -- an unknown flag or any error `ParseConfig`
returns -- is something the caller fixes by changing the command line or the
input, so all of them wrap `errUsage` and `main` maps that to exit code 2.
Exit code 1 is defined by convention for a runtime failure, but this
particular tool has none: once the input parses, rendering a map that
already exists in memory cannot fail.

Create `main.go`:

```go
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

// errUsage marks a failure the caller can fix by changing the input: a
// malformed line, an empty key, or (in strict mode) a duplicate key. main
// maps it to exit code 2; any other error maps to exit code 1.
var errUsage = errors.New("usage")

// run parses stdin as KEY=VALUE lines and writes the deterministic, sorted
// rendering to stdout. It never touches os.Stdin, os.Stdout, or os.Exit
// directly, so it can be driven end to end in a test with a strings.Reader
// and a bytes.Buffer.
func run(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("configrender", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	strict := fs.Bool("strict", false, "reject a repeated key instead of letting the last one win")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}

	cfg, err := ParseConfig(stdin, *strict)
	if err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	fmt.Fprint(stdout, Render(cfg))
	return nil
}

func main() {
	flag.CommandLine.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: configrender [-strict] < config.env")
		fmt.Fprintln(os.Stderr, "reads KEY=VALUE lines from stdin and prints a deterministic, sorted rendering.")
	}
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "configrender:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
```

Run it:

```bash
printf 'database.port=5432\ndatabase.host=db-primary.internal\ncache.ttl=300s\nlog.level=info\ntls.enabled=true\n' | go run .
printf 'a=1\na=2\n' | go run . -strict
printf 'not-a-pair\n' | go run .
```

Expected output:

```text
cache.ttl=300s
database.host=db-primary.internal
database.port=5432
log.level=info
tls.enabled=true
configrender: usage: line 2: configrender: duplicate key: "a"
configrender: usage: line 1: configrender: malformed line, want KEY=VALUE: "not-a-pair"
```

The first command's five lines come out alphabetically sorted by key,
regardless of the order they arrived on stdin. The second command shows
`-strict` catching a repeated `a` on line 2 and exiting 2 rather than
silently keeping the last value. The third shows the same exit-2 path for a
line with no `=` at all -- both failures are usage errors, fixable by
correcting the input, never a crash.

### Tests

`TestParseConfig` is the table over the input shapes a real config file can
take: comments and blank lines skipped, a value that legitimately contains
its own `=` (a URL with a query string), an explicitly empty value, a
malformed line, an empty key, a duplicate key handled both ways, and empty
input. `TestRenderDeterministicAcross100Runs` is the load-bearing test for
the module's actual claim: it renders the same 15-key config 100 times and
asserts every call matches the first byte for byte, which a naive
range-based renderer would fail on almost immediately.
`TestRenderSortedOrder` pins the exact expected string for a small,
hand-checkable map, so a regression that broke the sort comparator itself,
not just determinism, would be caught too. `TestRenderEmptyAndNilConfig`
covers both the empty-map and nil-map edge cases.

`rawKeyOrder` is the counterexample, and it lives only in this file: an
unexported helper that returns a map's keys straight off a plain range,
with no sort, unreachable from the tool's stdin/stdout path.
`TestRawRangeOrderIsFlaky` collects its output across 50 iterations of the
same unmodified 15-key map and asserts more than one distinct ordering
appears -- proving empirically, not just by citing the spec, that range
order cannot be trusted for anything byte-stable. `TestRun` drives the
whole tool end to end: a mixed-order, commented input rendering sorted, the
default and strict handling of a duplicate key, a malformed line, and an
unknown flag, all through `run` over a `strings.Reader` and a
`bytes.Buffer`, with every failure case checked against `errUsage` via
`errors.Is`.

Create `configrender_test.go`:

```go
package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func testConfig() map[string]string {
	return map[string]string{
		"database.host":     "db-primary.internal",
		"database.port":     "5432",
		"database.timeout":  "30s",
		"cache.host":        "redis-0.internal",
		"cache.ttl":         "300s",
		"log.level":         "info",
		"log.format":        "json",
		"queue.name":        "orders",
		"queue.concurrency": "8",
		"tls.enabled":       "true",
		"tls.min_version":   "1.3",
		"http.port":         "8080",
		"http.read_timeout": "5s",
		"metrics.enabled":   "true",
		"metrics.namespace": "orders_api",
	}
}

func TestParseConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		strict  bool
		want    map[string]string
		wantErr error
	}{
		{
			name:  "basic lines",
			input: "a=1\nb=2\n",
			want:  map[string]string{"a": "1", "b": "2"},
		},
		{
			name:  "blank lines and comments skipped",
			input: "# top comment\na=1\n\n  \n# another\nb=2\n",
			want:  map[string]string{"a": "1", "b": "2"},
		},
		{
			name:  "value may contain an equals sign",
			input: "url=https://host/path?a=b\n",
			want:  map[string]string{"url": "https://host/path?a=b"},
		},
		{
			name:  "empty value is legal",
			input: "flag=\n",
			want:  map[string]string{"flag": ""},
		},
		{
			name:    "malformed line has no equals sign",
			input:   "not-a-pair\n",
			wantErr: ErrMalformedLine,
		},
		{
			name:    "empty key",
			input:   "=value\n",
			wantErr: ErrEmptyKey,
		},
		{
			name:  "duplicate key last write wins outside strict mode",
			input: "a=1\na=2\n",
			want:  map[string]string{"a": "2"},
		},
		{
			name:    "duplicate key rejected in strict mode",
			input:   "a=1\na=2\n",
			strict:  true,
			wantErr: ErrDuplicateKey,
		},
		{
			name:  "empty input",
			input: "",
			want:  map[string]string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseConfig(strings.NewReader(tc.input), tc.strict)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ParseConfig() error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseConfig(): unexpected error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ParseConfig() = %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("ParseConfig()[%q] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestRenderDeterministicAcross100Runs(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	first := Render(cfg)
	for i := 0; i < 100; i++ {
		got := Render(cfg)
		if got != first {
			t.Fatalf("run %d: Render() differs from run 0\nrun0: %q\nrun%d: %q", i, first, i, got)
		}
	}
}

func TestRenderSortedOrder(t *testing.T) {
	t.Parallel()

	cfg := map[string]string{"zeta": "3", "alpha": "1", "mu": "2"}
	got := Render(cfg)
	want := "alpha=1\nmu=2\nzeta=3\n"
	if got != want {
		t.Fatalf("Render() = %q, want %q", got, want)
	}
}

func TestRenderEmptyAndNilConfig(t *testing.T) {
	t.Parallel()

	if got := Render(map[string]string{}); got != "" {
		t.Fatalf("Render(empty) = %q, want empty string", got)
	}
	if got := Render(nil); got != "" {
		t.Fatalf("Render(nil) = %q, want empty string", got)
	}
}

// rawKeyOrder is the renderer as it is usually written the first time: it
// ranges the map directly with no sort. It is never exported, never
// reachable from the tool's stdin/stdout path, and exists only so the test
// below can demonstrate -- not just assert -- why Render always sorts
// first.
func rawKeyOrder(cfg map[string]string) []string {
	keys := make([]string, 0, len(cfg))
	for k := range cfg {
		keys = append(keys, k)
	}
	return keys
}

// TestRawRangeOrderIsFlaky demonstrates the failure mode Render exists to
// avoid: collecting rawKeyOrder across many iterations of the same
// unmodified map produces more than one distinct ordering, so any renderer
// built directly from a map range would produce a flapping diff in CI.
func TestRawRangeOrderIsFlaky(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	orders := make(map[string]bool)
	for i := 0; i < 50; i++ {
		orders[strings.Join(rawKeyOrder(cfg), ",")] = true
	}
	if len(orders) < 2 {
		t.Fatalf("expected raw range order to vary across iterations, got %d distinct order(s) in 50 tries",
			len(orders))
	}
	t.Logf("observed %d distinct raw range orderings across 50 iterations", len(orders))
}

func TestRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		stdin   string
		want    string
		wantErr bool
	}{
		{
			name:  "sorted rendering",
			stdin: "b=2\na=1\n# comment\n\nc=3\n",
			want:  "a=1\nb=2\nc=3\n",
		},
		{
			name:  "duplicate key last write wins by default",
			stdin: "a=1\na=2\n",
			want:  "a=2\n",
		},
		{
			name:    "duplicate key rejected in strict mode",
			args:    []string{"-strict"},
			stdin:   "a=1\na=2\n",
			wantErr: true,
		},
		{
			name:    "malformed line is a usage error",
			stdin:   "not-a-pair\n",
			wantErr: true,
		},
		{
			name:    "unknown flag is a usage error",
			args:    []string{"-bogus"},
			wantErr: true,
		},
		{
			name:  "empty input renders empty output",
			stdin: "",
			want:  "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var stdout bytes.Buffer
			err := run(tc.args, strings.NewReader(tc.stdin), &stdout)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("run(%v): want error, got nil", tc.args)
				}
				if !errors.Is(err, errUsage) {
					t.Fatalf("run(%v) error = %v, want it to wrap errUsage", tc.args, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("run(%v): %v", tc.args, err)
			}
			if stdout.String() != tc.want {
				t.Fatalf("run(%v) stdout = %q, want %q", tc.args, stdout.String(), tc.want)
			}
		})
	}
}
```

## Review

`Render` is correct exactly when its output is byte-identical for the same
input on every call, and `TestRenderDeterministicAcross100Runs` is the test
that actually proves it -- a single call proves nothing about the property
that matters here. The fix is small on purpose: `slices.Sorted(maps.Keys(cfg))`
collects the keys once and pins their order before anything ranges over
them, so the one place randomness could leak in is closed off immediately.
`TestRawRangeOrderIsFlaky` is the module's sharpest lesson -- it does not
just claim range order is unreliable, it demonstrates the same map
producing multiple distinct orderings across 50 calls with nothing modified
in between, which is the concrete failure a naive renderer would hit in CI,
and that naive version never appears anywhere `configrender` itself can
call it. `ParseConfig`'s duplicate-key handling is a separate, legitimate
design choice, not a bug: `-strict` rejects it with exit code 2, the default
mode overlays it, and either is a defensible contract for a config format.
Exit code 2 covers every way stdin or the flags can be wrong; exit code 1
is reserved but unreachable here, since nothing after a successful parse can
fail. Run `go test -count=1 -race ./...` before trusting any change to the
renderer.

## Resources

- [maps.Keys](https://pkg.go.dev/maps#Keys) — the iterator over a map's keys that `Render` sorts before use.
- [slices.Sorted](https://pkg.go.dev/slices#Sorted) — drains an iterator into a sorted slice; the one line that fixes the whole module.
- [Go Specification: For statements](https://go.dev/ref/spec#For_statements) — documents that map range order is unspecified and iteration-start-randomized.
- [Go blog: Go maps in action](https://go.dev/blog/maps) — background on why map iteration order was deliberately randomized.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [16-sharded-map-vs-rwmutex-contention.md](16-sharded-map-vs-rwmutex-contention.md) | Next: [18-bidirectional-index-consistency.md](18-bidirectional-index-consistency.md)
