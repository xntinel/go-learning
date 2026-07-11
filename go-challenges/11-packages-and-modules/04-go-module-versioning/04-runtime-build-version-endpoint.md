# Exercise 4: Expose build version and VCS revision from a /version endpoint

During an incident the first question is "which commit is actually running?" — and
the deployed tag rarely answers it precisely. Go embeds the answer in every binary
at link time, and `runtime/debug.ReadBuildInfo` reads it back. Here you build the
standard `/version` handler that reports the module version, Go toolchain, and the
`vcs.*` build settings as JSON.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
versionsvc/                 independent module: example.com/billing/versionsvc
  go.mod
  versionsvc.go             Handler(), Info, settingsMap, infoFrom over debug.ReadBuildInfo
  cmd/
    demo/
      main.go               runnable: drive the handler with httptest, print the shape
  versionsvc_test.go        httptest handler test + unit tests on settingsMap/infoFrom
```

- Files: `versionsvc.go`, `cmd/demo/main.go`, `versionsvc_test.go`.
- Implement: `Handler() http.HandlerFunc` reading `debug.ReadBuildInfo`, an `Info` JSON shape, and a `settingsMap([]debug.BuildSetting) map[string]string` helper.
- Test: an `httptest` request asserting a 200 JSON body with a non-empty Go version, plus `settingsMap`/`infoFrom` unit-tested on synthetic slices so the assertions never depend on how the test binary was built.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/versionsvc/cmd/demo
cd ~/go-exercises/versionsvc
go mod init example.com/billing/versionsvc
go mod edit -go=1.26
```

### What the toolchain embeds, and why the helper is split

When `go build` links a binary it stamps in a `debug.BuildInfo`: the main module's
path and version, the full dependency build list, the `GoVersion` string, and a set
of `Settings` — key/value pairs including `vcs.revision` (the git commit),
`vcs.time` (its commit time), and `vcs.modified` (`"true"` if the working tree had
uncommitted changes when built). `debug.ReadBuildInfo()` returns that struct plus an
`ok bool` — `ok` is false only for a binary built in a way that carries no build
info, which a normally-built service never is.

The design choice that makes this testable is separating the *transformation* from
the *source*. `settingsMap` turns the `[]debug.BuildSetting` slice into a
`map[string]string` for O(1) lookup by key; `infoFrom` maps a `*debug.BuildInfo`
into the `Info` response. Neither calls `ReadBuildInfo`, so both can be unit-tested
against a synthetic `BuildInfo` you construct in the test — the assertions do not
depend on how the *test binary* happened to be built (a test binary often has an
empty `Main.Version` and no `vcs.*` stamps, which would make direct assertions
flaky). `Handler` is the thin edge that calls `ReadBuildInfo` and encodes the result;
its test asserts only what is invariant — a 200, JSON content type, and a non-empty
`GoVersion`, which the toolchain always stamps.

The `vcs.modified` flag deserves a note: a `true` there in production means someone
deployed a binary built from a dirty tree — the git revision does *not* fully
describe the running code. Surfacing it turns a subtle forensic gap into a visible
field.

Create `versionsvc.go`:

```go
package versionsvc

import (
	"encoding/json"
	"net/http"
	"runtime/debug"
)

// Info is the JSON shape a service reports from /version.
type Info struct {
	Version   string `json:"version"`
	GoVersion string `json:"go_version"`
	Revision  string `json:"vcs_revision,omitempty"`
	Time      string `json:"vcs_time,omitempty"`
	Modified  bool   `json:"vcs_modified"`
}

// settingsMap indexes build settings by key for O(1) lookup.
func settingsMap(settings []debug.BuildSetting) map[string]string {
	m := make(map[string]string, len(settings))
	for _, s := range settings {
		m[s.Key] = s.Value
	}
	return m
}

// infoFrom projects a BuildInfo into the reported Info. It is pure, so it is
// unit-tested against a synthetic BuildInfo independent of the test binary.
func infoFrom(bi *debug.BuildInfo) Info {
	s := settingsMap(bi.Settings)
	return Info{
		Version:   bi.Main.Version,
		GoVersion: bi.GoVersion,
		Revision:  s["vcs.revision"],
		Time:      s["vcs.time"],
		Modified:  s["vcs.modified"] == "true",
	}
}

// Handler returns the /version endpoint: it reads the linked build info and
// encodes it as JSON.
func Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		bi, ok := debug.ReadBuildInfo()
		if !ok {
			http.Error(w, "build info unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(infoFrom(bi)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}
```

### The runnable demo

The demo drives the handler in-process with `httptest` and prints only the invariant
parts of the response — status, content type, and whether a Go version was reported —
so its output is deterministic regardless of how the demo binary was stamped.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/billing/versionsvc"
)

func main() {
	req := httptest.NewRequest(http.MethodGet, "/version", nil)
	rec := httptest.NewRecorder()
	versionsvc.Handler().ServeHTTP(rec, req)

	fmt.Printf("status: %d\n", rec.Code)
	fmt.Printf("content-type: %s\n", rec.Header().Get("Content-Type"))

	var info versionsvc.Info
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		fmt.Println("decode failed:", err)
		return
	}
	fmt.Printf("go version reported: %v\n", info.GoVersion != "")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
status: 200
content-type: application/json
go version reported: true
```

### Tests

The handler test asserts only invariants. The `settingsMap` and `infoFrom` tests use
synthetic slices, so they exercise the `vcs.*` projection deterministically even
though the test binary carries no such stamps.

Create `versionsvc_test.go`:

```go
package versionsvc

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime/debug"
	"testing"
)

func TestHandlerShape(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodGet, "/version", nil)
	rec := httptest.NewRecorder()

	Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	var got Info
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got.GoVersion == "" {
		t.Fatal("go_version empty; ReadBuildInfo always stamps the toolchain")
	}
}

func TestSettingsMap(t *testing.T) {
	t.Parallel()
	in := []debug.BuildSetting{
		{Key: "vcs.revision", Value: "abc123"},
		{Key: "vcs.time", Value: "2026-07-02T00:00:00Z"},
		{Key: "vcs.modified", Value: "true"},
	}
	m := settingsMap(in)
	if m["vcs.revision"] != "abc123" {
		t.Fatalf("vcs.revision = %q, want abc123", m["vcs.revision"])
	}
	if m["vcs.modified"] != "true" {
		t.Fatalf("vcs.modified = %q, want true", m["vcs.modified"])
	}
}

func TestInfoFrom(t *testing.T) {
	t.Parallel()
	bi := &debug.BuildInfo{
		GoVersion: "go1.26",
		Main:      debug.Module{Path: "example.com/billing/versionsvc", Version: "v1.4.2"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "deadbeefcafe"},
			{Key: "vcs.time", Value: "2026-07-02T09:00:00Z"},
			{Key: "vcs.modified", Value: "true"},
		},
	}
	got := infoFrom(bi)
	if got.Version != "v1.4.2" || got.GoVersion != "go1.26" {
		t.Fatalf("infoFrom = %+v, want Version=v1.4.2 GoVersion=go1.26", got)
	}
	if got.Revision != "deadbeefcafe" || got.Time != "2026-07-02T09:00:00Z" || !got.Modified {
		t.Fatalf("vcs projection wrong: %+v", got)
	}
}

func TestInfoFromCleanTree(t *testing.T) {
	t.Parallel()
	bi := &debug.BuildInfo{
		GoVersion: "go1.26",
		Settings:  []debug.BuildSetting{{Key: "vcs.modified", Value: "false"}},
	}
	if got := infoFrom(bi); got.Modified {
		t.Fatalf("Modified = true for a clean tree; want false")
	}
}
```

## Review

The endpoint is correct when its testable core is pure: `settingsMap` and `infoFrom`
transform data with no dependency on `ReadBuildInfo`, so their tests assert the exact
`vcs.*` projection against a `BuildInfo` you construct — `deadbeefcafe`, `modified:
true` — and never flake on how the test binary was stamped. The handler test asserts
only invariants the toolchain guarantees: a 200, a JSON content type, a non-empty
`GoVersion`. The forensic value is real: `vcs.revision` names the running commit and
`vcs.modified` reveals a build from a dirty tree — the two facts a deployed tag alone
cannot give you. The trap is asserting on `Main.Version` or the live `vcs.revision`
in a test; a test binary usually has neither, so keep those assertions on synthetic
input.

## Resources

- [`runtime/debug.ReadBuildInfo`](https://pkg.go.dev/runtime/debug#ReadBuildInfo) — the entry point and the `ok` contract.
- [`runtime/debug.BuildInfo`](https://pkg.go.dev/runtime/debug#BuildInfo) — `Main`, `Settings`, `GoVersion`, and the `vcs.*` keys.
- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — `NewRequest` and `NewRecorder` for in-process handler tests.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-semver-compatibility-gate.md](05-semver-compatibility-gate.md)
