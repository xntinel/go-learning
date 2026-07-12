# Exercise 3: WASI Capabilities — Filesystem Preopens, Env, and Determinism

The guest is ordinary Go asking for files, environment variables, a clock, and
randomness. What it actually gets is entirely up to the host. This exercise builds
a file-processing guest and a host that grants it exactly one read-only input
mount, one writable output mount, one environment variable, and nothing else —
then shows how to opt into a real clock and real entropy when the guest needs
them. This is what least-privilege plugin file access looks like on a platform.

This module is fully self-contained: the processing logic and a capability-audit
helper are pure Go the host tests exercise directly.

## What you'll build

```text
fsguest/                    independent module: example.com/fsguest
  go.mod                    go 1.24; requires github.com/tetratelabs/wazero
  summary.go                pure: Summarize (no wazero, no build tag)
  grant.go                  Grant capability set; Audit (pure, testable)
  host.go                   RunFile: FSConfig mounts + env + clock/rand toggles; ErrGuestFailed
  guest/
    main.go                 //go:build wasip1 — reads /in, writes /out, prints a nonce+timestamp
  cmd/
    demo/
      main.go               host: least-privilege run over real temp dirs
  fsguest_test.go           table tests for Summarize and Grant.Audit; Examples
```

- Files: `summary.go`, `grant.go`, `host.go`, `guest/main.go`, `cmd/demo/main.go`, `fsguest_test.go`.
- Implement: pure `Summarize`, a `Grant` with an `Audit`, and `RunFile` building an `FSConfig` with `WithReadOnlyDirMount`/`WithDirMount`, plus `WithEnv` and the `WithSysWalltime`/`WithRandSource` opt-ins.
- Test: table-driven `Summarize`; `Grant.Audit` for a default-deny grant and a full grant; `Example`s with `// Output:`.
- Verify: build the guest, then `go run ./cmd/demo`.

Set up the module:

```bash
mkdir -p go-solutions/53-wasm-and-extensibility/04-tinygo-wasi-guest-modules/03-wasi-capabilities-sandbox/guest go-solutions/53-wasm-and-extensibility/04-tinygo-wasi-guest-modules/03-wasi-capabilities-sandbox/cmd/demo
cd go-solutions/53-wasm-and-extensibility/04-tinygo-wasi-guest-modules/03-wasi-capabilities-sandbox
go mod edit -go=1.24
go get github.com/tetratelabs/wazero@latest
```

### Capabilities are granted, never inherited

The mental model to hold is default-deny. A WASI guest has no ambient filesystem:
it sees only directories the host *preopens* for it, each mapped to a guest path.
`FSConfig` is where those grants live. `WithReadOnlyDirMount(hostDir, "/in")` lets
the guest `os.Open("/in/...")` but not write; `WithDirMount(hostDir, "/out")`
grants read-write at `/out`. Env is the same: `WithEnv("LABEL", "auth")` is the
*only* reason `os.Getenv("LABEL")` returns anything. Nothing falls back to the
host process's real filesystem or environment. You therefore build the guest's
world up capability by capability, and anything you do not name is denied.

Two rules keep the grant safe. Mount the *narrowest* directory the guest needs —
never the host root — because a mount is a capability handed over wholesale. And
know the sharp edge: WASI resolves paths inside a preopen, but a guest can still
reach with `../` relative lookups, so a broad mount is more reachable than it
looks. Prefer a purpose-built directory, and prefer read-only whenever the guest
only reads.

### A capability audit you can test

`Grant` is a plain description of the surface a host will expose, and `Audit`
turns it into a stable, human-readable list — the kind of line you would log or
assert in a policy test to prove a guest got least privilege. Crucially, `Audit`
reports the *guest-visible* capability (the mode and guest path), not the host
directory, so it is deterministic and free of machine-specific paths. Anything not
granted reads as `denied` or `none`, which is what makes the default-deny posture
visible at a glance.

Create `grant.go`:

```go
package fsguest

import (
	"sort"
	"strings"
)

// Grant is the capability surface a host exposes to the guest. The zero value
// grants nothing: default-deny.
type Grant struct {
	ReadOnlyIn  string            // host dir mounted read-only at /in; "" = denied
	WritableOut string            // host dir mounted read-write at /out; "" = denied
	Env         map[string]string // environment variables to expose
	RealClock   bool              // wire the real wall clock instead of the fake one
	RealRandom  bool              // wire crypto/rand instead of the deterministic source
}

// Audit returns the guest-visible capability surface, one line per capability, in
// a stable order. It names guest paths and modes, never host directories, so it is
// deterministic. Anything not granted reads as denied/none.
func (g Grant) Audit() []string {
	inState := "denied"
	if g.ReadOnlyIn != "" {
		inState = "read-only mount at /in"
	}
	outState := "denied"
	if g.WritableOut != "" {
		outState = "read-write mount at /out"
	}
	envState := "none"
	if len(g.Env) > 0 {
		keys := make([]string, 0, len(g.Env))
		for k := range g.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		envState = strings.Join(keys, ",")
	}
	clockState := "deterministic (fake)"
	if g.RealClock {
		clockState = "real walltime"
	}
	randState := "deterministic"
	if g.RealRandom {
		randState = "crypto/rand"
	}
	return []string{
		"fs.in: " + inState,
		"fs.out: " + outState,
		"env: " + envState,
		"clock: " + clockState,
		"rand: " + randState,
	}
}
```

### The processing logic, pure and shared

`Summarize` is the guest's real work, kept out of the guest so the tests can run
it directly: it counts the non-empty lines in the input and returns a one-line
report tagged with a label. It is deterministic — the same bytes and label always
produce the same output — which matters for the reproducibility discussion below.

Create `summary.go`:

```go
package fsguest

import (
	"fmt"
	"strings"
)

// Summarize counts the non-empty lines in data and returns a one-line report
// tagged with label. It is pure, so the same code runs in the guest and in tests.
func Summarize(data []byte, label string) []byte {
	lines := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" {
			lines++
		}
	}
	return []byte(fmt.Sprintf("label=%s nonempty_lines=%d\n", label, lines))
}
```

### Determinism is the default; opt into reality

Under `wazero`'s defaults the guest's clock and randomness are *fake and
reproducible*: the wall clock advances a fixed 1ms per read rather than tracking
real time, and `random_get` returns a deterministic sequence. So a guest that
stamps its output with `time.Now()` and a `crypto/rand` nonce produces
byte-identical output every run — exactly what you want for a replayable pipeline
or a reproducible plugin test. When the guest genuinely needs real time or entropy
(a session token, a real timestamp), the host opts in: `WithSysWalltime()`,
`WithSysNanotime()`, and `WithSysNanosleep()` wire the host clock, and
`WithRandSource(crypto/rand.Reader)` wires real entropy. The trap cuts both ways —
forget to opt in and your "random" token is a fixed constant; opt in by accident
and your reproducible pipeline stops being reproducible.

The guest below reads `/in/input.txt`, summarizes it, writes `/out/output.txt`,
and prints a nonce and timestamp to stderr so you can watch the determinism toggle
without disturbing the demo's stdout.

Create `guest/main.go`:

```go
//go:build wasip1

// Command fsguest is a WASI file-processing guest. Build it with
// GOOS=wasip1 GOARCH=wasm go build -o summarizer.wasm ./guest
// or tinygo build -o summarizer.wasm -target=wasip1 ./guest
package main

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"os"
	"time"

	"example.com/fsguest"
)

func main() {
	data, err := os.ReadFile("/in/input.txt")
	if err != nil {
		fmt.Fprintln(os.Stderr, "read /in/input.txt:", err)
		os.Exit(1)
	}
	label := os.Getenv("LABEL")
	if label == "" {
		label = "unlabeled"
	}
	if err := os.WriteFile("/out/output.txt", fsguest.Summarize(data, label), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write /out/output.txt:", err)
		os.Exit(1)
	}

	// Fake and reproducible under wazero defaults; real once the host opts in.
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		fmt.Fprintln(os.Stderr, "rand:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "nonce=%d ts=%d\n", binary.BigEndian.Uint64(b[:]), time.Now().UnixNano())
}
```

### The host: translate a Grant into a ModuleConfig

`RunFile` is where a `Grant` becomes concrete capabilities. It attaches an
`FSConfig` with only the mounts the grant names, sets only the granted env vars,
and wires the real clock and random source only when the grant asks. It reuses the
exit-code discipline from Exercise 1: a WASI command surfaces even a clean run as
`*sys.ExitError`, so `RunFile` treats code 0 as success and anything else as a
wrapped `ErrGuestFailed`.

Create `host.go`:

```go
package fsguest

import (
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"io"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

// ErrGuestFailed reports that the guest ran but exited with a non-zero code.
var ErrGuestFailed = errors.New("guest failed")

// RunFile instantiates the file-processing guest with exactly the capabilities in
// g and nothing more. A clean run returns nil; a non-zero guest exit is wrapped
// with ErrGuestFailed; a host-side instantiation error is returned as-is.
func RunFile(ctx context.Context, wasm []byte, g Grant, stdout io.Writer) error {
	rt := wazero.NewRuntime(ctx)
	defer rt.Close(ctx)
	wasi_snapshot_preview1.MustInstantiate(ctx, rt)

	fsc := wazero.NewFSConfig()
	if g.ReadOnlyIn != "" {
		fsc = fsc.WithReadOnlyDirMount(g.ReadOnlyIn, "/in")
	}
	if g.WritableOut != "" {
		fsc = fsc.WithDirMount(g.WritableOut, "/out")
	}

	cfg := wazero.NewModuleConfig().
		WithArgs("summarizer").
		WithStdout(stdout).
		WithStderr(io.Discard).
		WithFSConfig(fsc)
	for k, v := range g.Env {
		cfg = cfg.WithEnv(k, v)
	}
	if g.RealClock {
		cfg = cfg.WithSysWalltime().WithSysNanotime().WithSysNanosleep()
	}
	if g.RealRandom {
		cfg = cfg.WithRandSource(crand.Reader)
	}

	_, err := rt.InstantiateWithConfig(ctx, wasm, cfg)
	var exit *sys.ExitError
	if errors.As(err, &exit) {
		if exit.ExitCode() == 0 {
			return nil
		}
		return fmt.Errorf("%w: exit code %d", ErrGuestFailed, exit.ExitCode())
	}
	return err
}
```

### The runnable demo

The demo grants least privilege: a read-only input directory, a writable output
directory, and one env var. It prints the capability audit (host-computed, so it
is exact), runs the guest, and prints the resulting `/out/output.txt`. Both the
audit and the summary are deterministic, so the output is stable across runs even
though the guest also emits a nonce — that nonce goes to the discarded stderr.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"example.com/fsguest"
)

func main() {
	ctx := context.Background()
	wasm, err := os.ReadFile("summarizer.wasm")
	if err != nil {
		fmt.Fprintln(os.Stderr, "read summarizer.wasm (build the guest first):", err)
		os.Exit(1)
	}

	inDir, err := os.MkdirTemp("", "in")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer os.RemoveAll(inDir)
	outDir, err := os.MkdirTemp("", "out")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer os.RemoveAll(outDir)

	input := []byte("login alice\nlogout alice\n\nlogin bob\n")
	if err := os.WriteFile(filepath.Join(inDir, "input.txt"), input, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	g := fsguest.Grant{
		ReadOnlyIn:  inDir,
		WritableOut: outDir,
		Env:         map[string]string{"LABEL": "auth"},
	}
	fmt.Println("capability audit:")
	for _, line := range g.Audit() {
		fmt.Println("  " + line)
	}

	if err := fsguest.RunFile(ctx, wasm, g, io.Discard); err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		os.Exit(1)
	}
	result, err := os.ReadFile(filepath.Join(outDir, "output.txt"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Print("result: ", string(result))
}
```

Build the guest, then run the host:

```bash
GOOS=wasip1 GOARCH=wasm go build -o summarizer.wasm ./guest
# or, for a smaller, faster-loading artifact:
tinygo build -o summarizer.wasm -target=wasip1 ./guest
go run ./cmd/demo
```

Expected output:

```
capability audit:
  fs.in: read-only mount at /in
  fs.out: read-write mount at /out
  env: LABEL
  clock: deterministic (fake)
  rand: deterministic
result: label=auth nonempty_lines=3
```

### Tests

`TestSummarize` covers line counting, including blank and whitespace-only lines
that must not count. `TestGrantAudit` asserts the two ends of the spectrum: the
zero-value `Grant` audits to all-denied (proving default-deny), and a full grant
audits to the granted capabilities. The `Example`s pin a summary and a
least-privilege audit.

Create `fsguest_test.go`:

```go
package fsguest

import (
	"fmt"
	"slices"
	"testing"
)

func TestSummarize(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		data  string
		label string
		want  string
	}{
		{"three lines", "login alice\nlogout alice\n\nlogin bob\n", "auth", "label=auth nonempty_lines=3\n"},
		{"blanks ignored", "\n   \n\t\n", "x", "label=x nonempty_lines=0\n"},
		{"single no newline", "one", "y", "label=y nonempty_lines=1\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := string(Summarize([]byte(tc.data), tc.label)); got != tc.want {
				t.Fatalf("Summarize(%q, %q) = %q, want %q", tc.data, tc.label, got, tc.want)
			}
		})
	}
}

func TestGrantAudit(t *testing.T) {
	t.Parallel()

	denyAll := Grant{}.Audit()
	wantDeny := []string{
		"fs.in: denied",
		"fs.out: denied",
		"env: none",
		"clock: deterministic (fake)",
		"rand: deterministic",
	}
	if !slices.Equal(denyAll, wantDeny) {
		t.Fatalf("zero Grant audit = %v, want %v", denyAll, wantDeny)
	}

	full := Grant{
		ReadOnlyIn:  "/host/in",
		WritableOut: "/host/out",
		Env:         map[string]string{"LABEL": "auth", "TIER": "gold"},
		RealClock:   true,
		RealRandom:  true,
	}.Audit()
	wantFull := []string{
		"fs.in: read-only mount at /in",
		"fs.out: read-write mount at /out",
		"env: LABEL,TIER",
		"clock: real walltime",
		"rand: crypto/rand",
	}
	if !slices.Equal(full, wantFull) {
		t.Fatalf("full Grant audit = %v, want %v", full, wantFull)
	}
}

func ExampleSummarize() {
	fmt.Print(string(Summarize([]byte("a\n\nb\n"), "demo")))
	// Output: label=demo nonempty_lines=2
}

func ExampleGrant_Audit() {
	g := Grant{ReadOnlyIn: "/data", Env: map[string]string{"LABEL": "x"}}
	for _, line := range g.Audit() {
		fmt.Println(line)
	}
	// Output:
	// fs.in: read-only mount at /in
	// fs.out: denied
	// env: LABEL
	// clock: deterministic (fake)
	// rand: deterministic
}
```

## Review

The grant is correct when the guest can reach only what you named. Because
`wazero` denies filesystem, env, clock, and randomness by default, a guest that
`os.Open("/in/...")`s without a matching `WithReadOnlyDirMount` fails with a
not-found (default-deny in action), and `os.Getenv("LABEL")` is empty without
`WithEnv`. Confirm the posture with `TestGrantAudit`: the zero-value `Grant` must
audit to all-denied. The two safety habits are non-negotiable — mount the
narrowest directory (never the host root) and prefer read-only — because a mount is
a capability handed over in full, and even a narrow one is reachable through `../`.

Determinism is a design decision, not an accident. Under the defaults the guest's
clock and `crypto/rand` are fake and reproducible, which is why the demo's stdout
is byte-stable; opting in with `WithSysWalltime` and
`WithRandSource(crypto/rand.Reader)` is what you do when the guest needs real time
or entropy, and it is the line to check first when a nonce comes out constant.
Note the exit handling reused from Exercise 1: `RunFile` treats a `*sys.ExitError`
with code 0 as success and only a non-zero code as `ErrGuestFailed`, so a clean run
is never mistaken for a failure.

## Resources

- [wazero FSConfig](https://pkg.go.dev/github.com/tetratelabs/wazero#FSConfig) — `WithReadOnlyDirMount`, `WithDirMount`, and `WithFSMount`.
- [wazero ModuleConfig](https://pkg.go.dev/github.com/tetratelabs/wazero#ModuleConfig) — `WithFSConfig`, `WithEnv`, `WithRandSource`, `WithSysWalltime`/`WithSysNanotime`/`WithSysNanosleep`.
- [WASI filesystem and preopens](https://github.com/WebAssembly/WASI/blob/main/legacy/preview1/docs.md) — the capability model behind preopened directories.
- [wazero.io documentation](https://wazero.io/docs/) — configuring the sandbox and the default-deny posture.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-wasi-reactor-wasmexport.md](02-wasi-reactor-wasmexport.md) | Next: [../05-hashicorp-go-plugin/00-concepts.md](../05-hashicorp-go-plugin/00-concepts.md)
