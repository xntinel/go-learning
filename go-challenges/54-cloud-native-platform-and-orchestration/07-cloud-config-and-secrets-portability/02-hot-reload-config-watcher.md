# Exercise 2: Hot-Reloading Dynamic Config with Watch and an Atomic Swap

Changing a log level, a feature flag, or a rate limit should not require a
redeploy. This exercise builds the on-the-job machinery for that: a background
reloader that runs `Variable.Watch` in a loop against a `filevar`-backed variable
and atomically publishes each new good snapshot into the running server via
`atomic.Pointer[Config]`, so request handlers always read a consistent config with
zero locks on the hot path.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
hotreload/                   independent module: example.com/hotreload
  go.mod                     require gocloud.dev
  reloader.go                type Config; Reloader with atomic.Pointer[Config]; NewReloader, NewConfigDecoder, OpenFileVariable, Run, Current
  cmd/
    demo/
      main.go                writes a temp file, reloads it live, prints before and after
  reloader_test.go           filevar hot-reload test; Close causes Watch to return ErrClosed and the loop to exit
```

Files: `reloader.go`, `cmd/demo/main.go`, `reloader_test.go`.
Implement: a `Reloader` that watches a `*runtimevar.Variable` and stores each good `*Config` in an `atomic.Pointer[Config]`, exposing `Current() *Config` for lock-free reads and `Run(ctx) error` for the background loop; plus `OpenFileVariable(path)` wiring `filevar` with a JSON decoder.
Test: write a temp JSON file, start the reloader, poll `Current()` until it reflects the initial content, rewrite the file, and poll until `Current()` reflects the new content; then `Close` the variable and assert `Run` returns cleanly (via `ErrClosed`) with no goroutine leak.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/hotreload/cmd/demo
cd ~/go-exercises/hotreload
go mod init example.com/hotreload
go get gocloud.dev@latest
```

### The single-writer, many-reader shape

`Watch` is single-consumer: it must be called from exactly one goroutine, and it
returns only when the value changes. That is the opposite of what a request
handler wants, so the design splits the two roles. One background goroutine owns
`Watch` and is the sole writer of the current config; every handler is a reader.
The handoff between them is an `atomic.Pointer[Config]`. The writer builds a fully
decoded `*Config` and calls `cur.Store(cfg)`; readers call `cur.Load()`. Because
the pointer is swapped in one atomic operation, a reader either sees the entire
old config or the entire new one, never a half-updated struct, and it never takes
a lock. A plain `r.cfg = cfg` field written by the watcher and read by handlers
would be a data race that `go test -race` reports; the atomic pointer is what
makes the hot path both correct and lock-free.

The loop's error handling is the other load-bearing part. `Watch` can return three
kinds of error, and conflating them breaks reload. `runtimevar.ErrClosed` is the
shutdown signal: it appears once someone calls `Variable.Close()`, and the loop
must return `nil` and exit. A cancelled context (`errors.Is(err, context.Canceled)`)
is also terminal, and the loop returns that error. Everything else — a file that
is briefly absent mid-write, a decode failure on a malformed edit, a transient
backend error — is *transient*: log it and continue watching. Returning on a
transient error would kill reloading on the first fat-fingered edit; treating
`ErrClosed` as transient would spin the loop forever after shutdown.

`OpenFileVariable` wires the hermetic backend. `filevar.OpenVariable` takes the
path, a decoder, and `*filevar.Options`. `Options.WaitDuration` controls how often
the driver retries after an error (for example, while the file does not yet
exist); a short value keeps tests responsive. Note the portability boundary:
`filevar` polls and gives no read-consistency guarantee, so mid-write you may
transiently observe an empty or errored value — exactly the transient case the
loop must tolerate. On macOS in particular, replacing a file with an empty one may
not fire a change, so tests write non-empty content and poll with a deadline
rather than sleeping once and asserting.

Create `reloader.go`:

```go
package hotreload

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"gocloud.dev/runtimevar"
	"gocloud.dev/runtimevar/filevar"
)

// Config is the hot-reloadable subset of application configuration.
type Config struct {
	LogLevel  string `json:"log_level"`
	RateLimit int    `json:"rate_limit"`
}

// NewConfigDecoder decodes JSON bytes into a *Config.
func NewConfigDecoder() *runtimevar.Decoder {
	return runtimevar.NewDecoder(&Config{}, runtimevar.JSONDecode)
}

// OpenFileVariable opens a file-backed variable that decodes JSON into *Config.
// WaitDuration governs retry frequency after an error (e.g. a missing file).
func OpenFileVariable(path string) (*runtimevar.Variable, error) {
	v, err := filevar.OpenVariable(path, NewConfigDecoder(), &filevar.Options{
		WaitDuration: 50 * time.Millisecond,
	})
	if err != nil {
		return nil, fmt.Errorf("open file variable %q: %w", path, err)
	}
	return v, nil
}

// Reloader watches a Variable and publishes each good *Config into an atomic
// pointer, so request handlers read the current config with no lock.
type Reloader struct {
	v   *runtimevar.Variable
	cur atomic.Pointer[Config]
}

// NewReloader wraps a Variable. Call Run in a background goroutine.
func NewReloader(v *runtimevar.Variable) *Reloader {
	return &Reloader{v: v}
}

// Current returns the latest good config, or nil if none has arrived yet. It is
// safe for concurrent, lock-free per-request reads.
func (r *Reloader) Current() *Config {
	return r.cur.Load()
}

// Run watches for changes and publishes each good snapshot until the Variable is
// Closed (returns nil) or ctx is cancelled (returns ctx.Err()). Transient read or
// decode errors are logged and retried rather than fatal.
func (r *Reloader) Run(ctx context.Context) error {
	for {
		snap, err := r.v.Watch(ctx)
		if err != nil {
			switch {
			case errors.Is(err, runtimevar.ErrClosed):
				return nil
			case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
				return err
			default:
				log.Printf("hotreload: transient watch error: %v", err)
				continue
			}
		}
		cfg, ok := snap.Value.(*Config)
		if !ok {
			log.Printf("hotreload: unexpected snapshot type %T", snap.Value)
			continue
		}
		r.cur.Store(cfg)
	}
}
```

### The runnable demo

The demo writes a real file, starts the reloader, prints the initial config, then
rewrites the file and prints the reloaded config — no restart. It polls
`Current()` with a deadline rather than sleeping a fixed amount, because file-watch
latency is not guaranteed.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"example.com/hotreload"
)

func writeConfig(path, level string, limit int) error {
	body := fmt.Sprintf(`{"log_level":%q,"rate_limit":%d}`, level, limit)
	return os.WriteFile(path, []byte(body), 0o600)
}

func waitFor(cond func() bool) bool {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func main() {
	dir, err := os.MkdirTemp("", "hotreload-demo")
	if err != nil {
		log.Fatalf("temp dir: %v", err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "config.json")

	if err := writeConfig(path, "info", 100); err != nil {
		log.Fatalf("write config: %v", err)
	}

	v, err := hotreload.OpenFileVariable(path)
	if err != nil {
		log.Fatalf("open variable: %v", err)
	}

	r := hotreload.NewReloader(v)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	if !waitFor(func() bool { return r.Current() != nil }) {
		log.Fatal("initial config never loaded")
	}
	c := r.Current()
	fmt.Printf("initial: log_level=%s rate_limit=%d\n", c.LogLevel, c.RateLimit)

	if err := writeConfig(path, "debug", 500); err != nil {
		log.Fatalf("rewrite config: %v", err)
	}
	if !waitFor(func() bool { c := r.Current(); return c != nil && c.LogLevel == "debug" }) {
		log.Fatal("updated config never reloaded")
	}
	c = r.Current()
	fmt.Printf("updated: log_level=%s rate_limit=%d\n", c.LogLevel, c.RateLimit)

	v.Close()
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
initial: log_level=info rate_limit=100
updated: log_level=debug rate_limit=500
```

### Tests

`TestHotReload` is the proof of the whole design: it writes an initial file, starts
`Run` in a goroutine that reports its exit on a channel, polls `Current()` until it
matches the initial content, rewrites the file, and polls again until the atomic
pointer reflects the new content. Then it calls `v.Close()` and waits on the done
channel: `Close` unblocks the in-flight `Watch` with `ErrClosed`, `Run` returns
`nil`, and the goroutine exits — the absence of a hang proves there is no leak.
Polling with a deadline (not a single sleep) is deliberate: file-watch latency has
no guarantee, and non-empty writes avoid the macOS empty-file caveat.

Create `reloader_test.go`:

```go
package hotreload

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeConfig(t *testing.T, path, level string, limit int) {
	t.Helper()
	body := fmt.Sprintf(`{"log_level":%q,"rate_limit":%d}`, level, limit)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}

func waitFor(cond func() bool) bool {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func TestHotReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeConfig(t, path, "info", 100)

	v, err := OpenFileVariable(path)
	if err != nil {
		t.Fatalf("OpenFileVariable: %v", err)
	}

	r := NewReloader(v)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	if !waitFor(func() bool {
		c := r.Current()
		return c != nil && c.LogLevel == "info" && c.RateLimit == 100
	}) {
		t.Fatalf("initial config not published; got %+v", r.Current())
	}

	writeConfig(t, path, "debug", 500)
	if !waitFor(func() bool {
		c := r.Current()
		return c != nil && c.LogLevel == "debug" && c.RateLimit == 500
	}) {
		t.Fatalf("reload not observed; got %+v", r.Current())
	}

	// Close is the shutdown handshake: Watch returns ErrClosed and Run exits nil.
	if err := v.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil after Close", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not exit after Close: goroutine leak")
	}
}

func TestCurrentNilBeforeFirstValue(t *testing.T) {
	t.Parallel()
	r := NewReloader(nil) // Run is never started; no value has been published.
	if got := r.Current(); got != nil {
		t.Fatalf("Current() = %+v before first value, want nil", got)
	}
}

func Example() {
	dir, _ := os.MkdirTemp("", "hotreload-example")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "config.json")
	_ = os.WriteFile(path, []byte(`{"log_level":"info","rate_limit":100}`), 0o600)

	v, _ := OpenFileVariable(path)
	defer v.Close()

	snap, _ := v.Watch(context.Background())
	cfg := snap.Value.(*Config)
	fmt.Println(cfg.LogLevel, cfg.RateLimit)
	// Output: info 100
}
```

## Review

The reloader is correct when readers never see a torn config and the background
loop shuts down cleanly. The first property comes from `atomic.Pointer[Config]`:
the writer `Store`s a fully built `*Config` and readers `Load` it, so
`go test -race` stays clean — swapping in a plain field would fail it immediately.
The second comes from the error taxonomy: `TestHotReload` closes the variable and
asserts `Run` returns `nil` promptly, which only holds if the loop treats
`ErrClosed` as "exit" rather than as a transient error to retry.

The mistakes to avoid are the ones the loop guards against. Do not call `Watch`
from more than one goroutine or from a request handler — it is single-consumer and
change-driven; read `Current()` instead. Do not return on the first non-`ErrClosed`
error, or a single malformed edit permanently stops reloading. Do not sleep a
fixed amount and assert once: `filevar` polls and offers no read-consistency
guarantee, so poll with a deadline and write non-empty content (the macOS
empty-file caveat). Run `go test -count=1 -race ./...` to confirm the atomic swap
and the clean shutdown together.

## Resources

- [`gocloud.dev/runtimevar`](https://pkg.go.dev/gocloud.dev/runtimevar) — `Variable.Watch`, the `ErrClosed` sentinel, and the `Close` handshake.
- [`gocloud.dev/runtimevar/filevar`](https://pkg.go.dev/gocloud.dev/runtimevar/filevar) — `OpenVariable`, `Options.WaitDuration`, and the file-watch caveats.
- [`sync/atomic.Pointer`](https://pkg.go.dev/sync/atomic#Pointer) — lock-free `Load`/`Store` for the published config.

---

Back to [01-typed-config-loader.md](01-typed-config-loader.md) | Next: [03-envelope-encrypted-config.md](03-envelope-encrypted-config.md)
