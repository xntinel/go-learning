# Exercise 16: Config Overlay That Refuses to Clobber Protected Keys

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A Consul- or etcd-style config overlay usually lets an environment layer win
on collision: defaults, then a file, then env, each later layer overriding
the earlier one, exactly the merge `00-concepts.md` describes with
`maps.Clone` and `maps.Copy`. But that last-writer-wins semantics is the
wrong default for a handful of security-critical keys — a kill switch, a
`tls_verify` flag, a rate-limit ceiling — where an operator who forgot to
strip a stray line from an env overlay should never be able to silently
disable a safety mechanism in production. `00-concepts.md` names this gap
directly: `maps.Copy(dst, src)` "on a key collision `src` wins with no
signal." A protected-key list closes exactly that hole by turning a subset
of collisions into a hard rejection instead of a silent win.

The trap is where the naive merge usually gets written first: directly on
the caller's own base map, with no protection list at all. That single
choice creates two independent problems at once — it mutates an input the
caller still holds a reference to (the "defaults" map every other request
also reads), and it has no mechanism to refuse anything, so a protected key
is overridden exactly as silently as an ordinary one. This module builds the
merge that solves both: it clones before it writes, and it inspects the
overlay for protected keys before committing to any part of the merge.

This module is fully self-contained: its own `go mod init`, an executable
command, and its tests. Nothing here imports another exercise.

## What you'll build

```text
overlaymerge/                  module example.com/overlaymerge
  go.mod                       go 1.24
  overlaymerge.go               package main — ParseKV, Merge
  overlaymerge_test.go          package main — parsing table, merge table,
                                the mergeBlind contrast, run() end to end
  main.go                       package main — -protect flag, base file arg, exit codes
```

- Files: `overlaymerge.go`, `overlaymerge_test.go`, `main.go`.
- Implement: `ParseKV(r io.Reader) (map[string]string, error)` parsing `key=value` lines, rejecting a line with no `=` or an empty key with `ErrMalformedLine`; `Merge(base, overlay map[string]string, protected []string) (map[string]string, error)` cloning `base`, rejecting the merge with `ErrProtectedKeyOverridden` if the overlay sets any protected key, and otherwise copying `overlay` onto the clone.
- Tool: `overlaymerge -protect key1,key2 base.env` reads the base KV file named on the command line and the overlay from stdin, merges them, and prints the merged KV sorted by key to stdout. Exit 0 on success, exit 2 on a malformed line, a missing base argument, or a protected-key violation (every offending key named on stderr), exit 1 is reserved for a runtime failure such as an unreadable base file.
- Test: KV parsing including the malformed cases; the merge table including collision, an unprotected overlay key, a protected key rejected whether or not it matches base's existing value, a protected key absent from base, and nil inputs; a multi-key violation naming every offending key sorted; the `mergeBlind` contrast pinning both the mutation and the silent clobber (and, by contrast, that `Merge` mutates neither input); `run` end to end using a temp base file, a `strings.Reader`, and a `bytes.Buffer`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/10-maps-package/16-protected-key-config-overlay
cd go-solutions/06-collections-arrays-slices-and-maps/10-maps-package/16-protected-key-config-overlay
go mod edit -go=1.24
```

### Clone before you copy, and check before you clone wins

`maps.Copy(dst, src)` is the right primitive for a merge — it is what lets
"later layer wins" fall out of a single line instead of a hand-written loop
— but it is destructive on whichever map you pass as `dst`. The naive form
of this tool would be `maps.Copy(base, overlay)` called directly on the
caller's own base map:

```go
func mergeBlind(base, overlay map[string]string) {
	maps.Copy(base, overlay) // mutates an input; nothing can refuse a key
}
```

That single line is wrong twice over. First, `base` is very often a shared
value — the package-level defaults, or a config object another goroutine is
mid-read on — and mutating it in place means every later caller sees this
request's overlay baked permanently in. Second, and specific to this
module, there is no place in that line to say "except this key": once you
call `maps.Copy`, every collision resolves the same way, protected or not.

`Merge` fixes the first problem the way `00-concepts.md`'s layered-merge
idiom already prescribes — `maps.Clone(base)` before any write touches it —
and fixes the second by inspecting `overlay` for every name in `protected`
*before* doing any copying at all. If even one protected key is present in
the overlay, `Merge` returns `ErrProtectedKeyOverridden` naming every
offending key and applies none of the overlay, not even the safe parts: a
partial merge that silently drops the protected assignment but keeps
everything else is just as dangerous, because an operator scanning the
output would see their env change appear to have failed for exactly one
key with no explanation of why.

Create `overlaymerge.go`:

```go
// Command overlaymerge merges an environment overlay onto a base
// configuration, refusing to merge at all if the overlay attempts to set a
// protected key.
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

// ErrMalformedLine is returned when a KV line is not "key=value".
var ErrMalformedLine = errors.New("overlaymerge: malformed line")

// ErrProtectedKeyOverridden is returned when the overlay sets one or more
// protected keys. The error text names every offending key, sorted.
var ErrProtectedKeyOverridden = errors.New("overlaymerge: overlay sets protected key(s)")

// ParseKV parses "key=value" lines from r into a map, one entry per line.
// Blank lines are skipped. A line without '=' or with an empty key returns
// an error wrapping ErrMalformedLine together with its 1-based line number.
func ParseKV(r io.Reader) (map[string]string, error) {
	kv := make(map[string]string)
	sc := bufio.NewScanner(r)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		if line == "" {
			continue
		}
		i := strings.IndexByte(line, '=')
		if i <= 0 {
			return nil, fmt.Errorf("line %d: %w: %q", lineNo, ErrMalformedLine, line)
		}
		kv[line[:i]] = line[i+1:]
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("overlaymerge: reading input: %w", err)
	}
	return kv, nil
}

// Merge overlays overlay onto base, with overlay winning on key collision,
// except for any key named in protected: if overlay sets any protected key
// at all -- present in base or not, matching value or not -- Merge rejects
// the whole merge and returns ErrProtectedKeyOverridden naming every
// offending key, sorted, instead of applying any part of the overlay.
//
// Merge never mutates base or overlay: it clones base before copying, so
// both inputs remain safe for the caller to reuse. The returned map shares
// no backing storage with either input.
func Merge(base, overlay map[string]string, protected []string) (map[string]string, error) {
	var offending []string
	for _, key := range protected {
		if _, set := overlay[key]; set {
			offending = append(offending, key)
		}
	}
	if len(offending) > 0 {
		slices.Sort(offending)
		return nil, fmt.Errorf("%w: %s", ErrProtectedKeyOverridden, strings.Join(offending, ", "))
	}

	merged := maps.Clone(base)
	if merged == nil {
		merged = make(map[string]string)
	}
	maps.Copy(merged, overlay)
	return merged, nil
}
```

### The tool

`overlaymerge` takes its base as a file argument, because a base
configuration lives on disk, and its overlay from stdin, because that is
where an operator or a deploy pipeline pipes an environment-specific patch.
`run` opens the base file itself and returns any `os.Open` error unwrapped
— that is a runtime failure (exit 1), distinct from every other error the
tool can produce, which is a usage mistake the caller can fix by changing
the command line or the input (exit 2): a malformed KV line, a missing base
argument, or `Merge` refusing a protected key. Wrapping `Merge`'s error in
`errUsage` before returning it is what makes the protected-key names that
`Merge` already collected land on stderr without `main` having to know
anything about the merge's internals.

Create `main.go`:

```go
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
)

// errUsage marks a fixable input mistake: a malformed KV line, a missing
// base argument, or a protected-key violation. main maps it to exit code 2;
// every other error maps to exit code 1.
var errUsage = errors.New("usage")

// run parses -protect and a base-file argument, reads the overlay from
// stdin, merges the two, and writes the result sorted by key to stdout. It
// never touches os.Exit, so a test can drive it with a temp file, a
// strings.Reader, and a bytes.Buffer.
func run(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("overlaymerge", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	protectFlag := fs.String("protect", "", "comma-separated list of keys the overlay must never set")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("%w: expected exactly one base file argument", errUsage)
	}

	baseFile, err := os.Open(fs.Arg(0))
	if err != nil {
		return err // runtime failure: unreadable base file
	}
	defer baseFile.Close()

	base, err := ParseKV(baseFile)
	if err != nil {
		return fmt.Errorf("%w: base: %v", errUsage, err)
	}
	overlay, err := ParseKV(stdin)
	if err != nil {
		return fmt.Errorf("%w: overlay: %v", errUsage, err)
	}

	var protected []string
	if *protectFlag != "" {
		protected = strings.Split(*protectFlag, ",")
	}

	merged, err := Merge(base, overlay, protected)
	if err != nil {
		if errors.Is(err, ErrProtectedKeyOverridden) {
			return fmt.Errorf("%w: %v", errUsage, err)
		}
		return err
	}

	for _, k := range slices.Sorted(maps.Keys(merged)) {
		fmt.Fprintf(stdout, "%s=%s\n", k, merged[k])
	}
	return nil
}

func main() {
	flag.CommandLine.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: overlaymerge [-protect key1,key2] base.env")
		fmt.Fprintln(os.Stderr, "reads an overlay from stdin and merges it onto the base file.")
	}
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "overlaymerge:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
```

Run it:

```bash
printf 'port=8080\nlog_level=info\nkill_switch=off\n' > base.env
printf 'log_level=debug\nport=9090\n' | go run . -protect kill_switch,tls_verify base.env
printf 'kill_switch=on\n' | go run . -protect kill_switch,tls_verify base.env
```

Expected output:

```text
kill_switch=off
log_level=debug
port=9090
overlaymerge: usage: overlaymerge: overlay sets protected key(s): kill_switch
```

The first run is an ordinary merge: `port` and `log_level` come from the
overlay, `kill_switch` is untouched because the overlay never mentions it.
The second run's overlay tries to flip `kill_switch` on; because that name
is in `-protect`, `Merge` refuses the entire run before writing anything,
and the offending key is named on stderr with exit code 2 -- no merged
output is printed at all.

### Tests

`TestParseKV` and `TestMerge` are the tables: ordinary collision, an
unprotected key merging normally, a protected key rejected whether or not
base already had it and whether or not the value would even change, and nil
base/overlay normalizing to an empty non-nil map. `TestMergeReportsEveryOffendingKeySorted`
pins that a multi-key violation names all of them, sorted, not just the
first one found.

`TestMergeBlindMutatesInputAndClobbersProtectedKey` is the module's reason
to exist. `mergeBlind` is unexported and unreachable from any exported
function; the test shows it doing both things this module exists to
prevent in one call — mutating `base` in place and silently accepting a
kill-switch override — and then shows `Merge`, given the identical inputs,
refusing the merge and leaving `base` untouched, which is also where the
"never mutates its inputs" contract on `Merge`'s doc comment gets pinned.
`TestRun` drives the command end to end against a real temp file: a normal
merge, a protected-key rejection that must not print anything to stdout, an
unreadable base file mapping to a non-usage error, and a missing argument.

Create `overlaymerge_test.go`:

```go
package main

import (
	"bytes"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseKV(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    map[string]string
		wantErr bool
	}{
		{name: "two entries, blank line skipped", input: "a=1\n\nb=2\n", want: map[string]string{"a": "1", "b": "2"}},
		{name: "empty input", input: "", want: map[string]string{}},
		{name: "value contains equals", input: "url=host=1\n", want: map[string]string{"url": "host=1"}},
		{name: "no equals sign", input: "bogus\n", wantErr: true},
		{name: "empty key", input: "=x\n", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseKV(strings.NewReader(tc.input))
			if tc.wantErr {
				if !errors.Is(err, ErrMalformedLine) {
					t.Fatalf("ParseKV err = %v, want ErrMalformedLine", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseKV: %v", err)
			}
			if !maps.Equal(got, tc.want) {
				t.Fatalf("ParseKV = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMerge(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		base      map[string]string
		overlay   map[string]string
		protected []string
		want      map[string]string
		wantErr   bool
	}{
		{name: "overlay wins on collision", base: map[string]string{"port": "8080", "log_level": "info"}, overlay: map[string]string{"log_level": "debug"}, want: map[string]string{"port": "8080", "log_level": "debug"}},
		{name: "unprotected overlay key merges normally", base: map[string]string{"a": "1"}, overlay: map[string]string{"b": "2"}, protected: []string{"kill_switch"}, want: map[string]string{"a": "1", "b": "2"}},
		// Rejected even though base has the identical value already: presence
		// in the overlay is what's forbidden, not a value change.
		{name: "overlay setting a protected key is rejected", base: map[string]string{"kill_switch": "off"}, overlay: map[string]string{"kill_switch": "off"}, protected: []string{"kill_switch"}, wantErr: true},
		{name: "protected key absent from base is still rejected", base: map[string]string{}, overlay: map[string]string{"kill_switch": "on"}, protected: []string{"kill_switch"}, wantErr: true},
		{name: "nil base and overlay yield an empty non-nil map", base: nil, overlay: nil, want: map[string]string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := Merge(tc.base, tc.overlay, tc.protected)
			if tc.wantErr {
				if !errors.Is(err, ErrProtectedKeyOverridden) {
					t.Fatalf("Merge err = %v, want ErrProtectedKeyOverridden", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Merge: %v", err)
			}
			if got == nil {
				t.Fatal("Merge returned a nil map")
			}
			if !maps.Equal(got, tc.want) {
				t.Fatalf("Merge = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMergeReportsEveryOffendingKeySorted(t *testing.T) {
	t.Parallel()

	base := map[string]string{}
	overlay := map[string]string{"tls_verify": "false", "kill_switch": "on", "port": "9090"}
	_, err := Merge(base, overlay, []string{"kill_switch", "tls_verify"})
	if !errors.Is(err, ErrProtectedKeyOverridden) {
		t.Fatalf("Merge err = %v, want ErrProtectedKeyOverridden", err)
	}
	if !strings.Contains(err.Error(), "kill_switch, tls_verify") {
		t.Fatalf("Merge err = %v, want it to name both offending keys sorted", err)
	}
}

// mergeBlind is the merge as it is often first written: maps.Copy(base,
// overlay) directly, no clone, no protection list. It is never exported and
// exists only so the tests can pin what it gets wrong: it mutates base -- an
// input the caller still holds a reference to -- and it has no way to
// refuse a protected key, so the overlay always wins.
func mergeBlind(base, overlay map[string]string) {
	maps.Copy(base, overlay)
}

func TestMergeBlindMutatesInputAndClobbersProtectedKey(t *testing.T) {
	t.Parallel()

	base := map[string]string{"kill_switch": "off", "other": "1"}
	overlay := map[string]string{"kill_switch": "on"}

	mergeBlind(base, overlay)

	if base["kill_switch"] != "on" {
		t.Fatalf("mergeBlind left kill_switch = %q, want it clobbered to %q", base["kill_switch"], "on")
	}

	// The real Merge, given the same inputs, refuses instead of clobbering,
	// and leaves the caller's base map untouched.
	base2 := map[string]string{"kill_switch": "off", "other": "1"}
	base2Before := maps.Clone(base2)
	_, err := Merge(base2, overlay, []string{"kill_switch"})
	if !errors.Is(err, ErrProtectedKeyOverridden) {
		t.Fatalf("Merge err = %v, want ErrProtectedKeyOverridden", err)
	}
	if !maps.Equal(base2, base2Before) {
		t.Fatalf("Merge mutated base: %v, want %v", base2, base2Before)
	}
}

func writeTempBase(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "base.env")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestRun(t *testing.T) {
	t.Parallel()

	t.Run("merged output sorted", func(t *testing.T) {
		t.Parallel()
		basePath := writeTempBase(t, "port=8080\nlog_level=info\n")
		var stdout bytes.Buffer
		err := run([]string{basePath}, strings.NewReader("log_level=debug\n"), &stdout)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		want := "log_level=debug\nport=8080\n"
		if stdout.String() != want {
			t.Fatalf("run stdout = %q, want %q", stdout.String(), want)
		}
	})

	t.Run("protected key violation exits usage naming the key", func(t *testing.T) {
		t.Parallel()
		basePath := writeTempBase(t, "kill_switch=off\n")
		var stdout bytes.Buffer
		err := run([]string{"-protect", "kill_switch", basePath}, strings.NewReader("kill_switch=on\n"), &stdout)
		if !errors.Is(err, errUsage) {
			t.Fatalf("run err = %v, want it to wrap errUsage", err)
		}
		if !strings.Contains(err.Error(), "kill_switch") {
			t.Fatalf("run err = %v, want it to name kill_switch", err)
		}
		if stdout.Len() != 0 {
			t.Fatalf("run wrote to stdout on rejection: %q", stdout.String())
		}
	})

	t.Run("unreadable base file is a runtime failure", func(t *testing.T) {
		t.Parallel()
		var stdout bytes.Buffer
		err := run([]string{filepath.Join(t.TempDir(), "missing.env")}, strings.NewReader(""), &stdout)
		if err == nil || errors.Is(err, errUsage) {
			t.Fatalf("run err = %v, want a non-usage runtime error", err)
		}
	})

	t.Run("missing base argument is a usage error", func(t *testing.T) {
		t.Parallel()
		var stdout bytes.Buffer
		err := run(nil, strings.NewReader(""), &stdout)
		if !errors.Is(err, errUsage) {
			t.Fatalf("run err = %v, want it to wrap errUsage", err)
		}
	})
}
```

## Review

The merge is correct when it does exactly one of two things: apply the
whole overlay onto a fresh clone of `base`, or apply none of it and name
every protected key the overlay tried to set. There is no partial-merge
path, because a merge that silently drops one key while applying the rest
is exactly the kind of near-miss that makes an operator trust output they
should not. `maps.Clone(base)` before any write is what keeps `Merge` from
mutating a base map the caller (very often a shared defaults object) still
holds; checking the overlay against `protected` before that clone is even
made is what lets the rejection be total rather than partial.
`mergeBlind`'s single-line `maps.Copy(base, overlay)` fails both properties
at once, which is exactly why the naive version of this tool is dangerous
in a way that is easy to miss in review -- it looks identical to the
correct merge from its call site. Exit code 2 covers every input mistake:
a malformed KV line, a missing base argument, a protected-key violation.
Exit code 1 is reserved for `os.Open` failing on the base file. Run
`go test -count=1 -race ./...` to confirm the parsing table, the merge
table, the `mergeBlind` contrast, and `run` end to end.

## Resources

- [`maps.Copy`](https://pkg.go.dev/maps#Copy) — destructive, last-writer-wins, and silent on collision; the primitive this module wraps with a refusal.
- [`maps.Clone`](https://pkg.go.dev/maps#Clone) — the shallow copy that keeps a merge from mutating its base.
- [HashiCorp Consul: Configuration](https://developer.hashicorp.com/consul/docs/agent/config) — a real layered-overlay configuration system with security-relevant flags.
- [The Twelve-Factor App: Config](https://12factor.net/config) — the precedence model (defaults, then file, then env) this module's merge implements, with one carve-out.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [15-wal-log-compaction-tombstones.md](15-wal-log-compaction-tombstones.md) | Next: [17-fingerprint-dedup-stream-filter.md](17-fingerprint-dedup-stream-filter.md)
