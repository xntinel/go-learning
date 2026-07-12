# Exercise 6: Deterministic JSON Config Snapshots

A config or feature-flag document is a perfect golden target: it is large,
structured, and stable. But JSON has three determinism levers you must set
deliberately — sorted map keys, stable indentation, and HTML-escaping policy —
plus a trailing-newline gotcha. You build a config serializer and pin its exact
snapshot.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
configsnap/                independent module: example.com/configsnap
  go.mod                   go 1.26
  config.go                Config; MarshalConfig (Encoder, no HTML escaping)
  testdata/
    config.golden          exact snapshot of the effective config
  cmd/
    demo/
      main.go              serializes a config and prints it
  config_test.go           byte golden, sorted-keys, escaping-policy tests
```

Files: `config.go`, `testdata/config.golden`, `cmd/demo/main.go`, `config_test.go`.
Implement: `MarshalConfig(Config) ([]byte, error)` using a `json.Encoder` with `SetEscapeHTML(false)`, `SetIndent`, and a single-trailing-newline policy.
Test: byte-compare the snapshot to the golden; prove map keys serialize sorted regardless of insertion order; prove `SetEscapeHTML(false)` keeps `<`, `>`, `&` literal.
Verify: `go test -count=1 -race ./...`

### The three determinism levers, and the two policies you must set

`encoding/json` pins two things for free, which is why JSON is a friendly golden
format. Map keys are marshaled in sorted order, so a `map[string]bool` of feature
flags serializes identically no matter the insertion order — the test proves this
by building the same map two ways and getting byte-identical output. And struct
fields serialize in declaration order, so the field layout of the golden is the
struct's contract: reorder the fields and the golden changes, which is the signal
you want. `MarshalIndent` (or `Encoder.SetIndent`) adds the third lever, stable
indentation.

Two policies are *not* decided for you, and both bite golden tests. The first is
HTML escaping: by default `encoding/json` escapes `<`, `>`, and `&` into
`<`, `>`, and `&`, because the default output is safe to embed in
HTML. In a config snapshot that turns a readable `value > 0 && status` into an
unreadable `value > 0 && status`, so you disable it with a
`json.Encoder` and `SetEscapeHTML(false)`. The test contrasts the two: the
default `MarshalIndent` escapes, `MarshalConfig` does not. The second policy is
the trailing newline: `Encoder.Encode` appends exactly one `\n`, but
`MarshalIndent` appends none, and POSIX tools and editors expect a single
trailing LF. You pick one policy — here, exactly one trailing newline — and apply
it in both the writer and the golden file. Getting this wrong is the single most
common confusing byte-golden failure: the content looks identical but the bytes
differ by one invisible newline.

Create `config.go`:

```go
package configsnap

import (
	"bytes"
	"encoding/json"
)

// Config is an effective configuration document. Field declaration order is the
// snapshot's contract; the flags map serializes with sorted keys.
type Config struct {
	Service        string          `json:"service"`
	Version        string          `json:"version"`
	Debug          bool            `json:"debug"`
	Flags          map[string]bool `json:"flags"`
	QueryTemplate  string          `json:"query_template"`
	AllowedOrigins []string        `json:"allowed_origins"`
}

// MarshalConfig serializes cfg deterministically: indented, without HTML
// escaping (so <, >, & stay literal), with exactly one trailing newline.
func MarshalConfig(cfg Config) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(cfg); err != nil {
		return nil, err
	}
	return append(bytes.TrimRight(buf.Bytes(), "\n"), '\n'), nil
}
```

Now the committed snapshot.

Create `testdata/config.golden`:

```text
{
  "service": "payments",
  "version": "1.4.0",
  "debug": false,
  "flags": {
    "audit_log": true,
    "beta_ui": false,
    "new_checkout": true
  },
  "query_template": "value > 0 && status != <nil>",
  "allowed_origins": [
    "https://a.example.com",
    "https://b.example.com"
  ]
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"

	"example.com/configsnap"
)

func main() {
	cfg := configsnap.Config{
		Service:        "payments",
		Version:        "1.4.0",
		Debug:          false,
		Flags:          map[string]bool{"new_checkout": true, "beta_ui": false, "audit_log": true},
		QueryTemplate:  "value > 0 && status != <nil>",
		AllowedOrigins: []string{"https://a.example.com", "https://b.example.com"},
	}
	out, err := configsnap.MarshalConfig(cfg)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	os.Stdout.Write(out)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{
  "service": "payments",
  "version": "1.4.0",
  "debug": false,
  "flags": {
    "audit_log": true,
    "beta_ui": false,
    "new_checkout": true
  },
  "query_template": "value > 0 && status != <nil>",
  "allowed_origins": [
    "https://a.example.com",
    "https://b.example.com"
  ]
}
```

### Tests

Create `config_test.go`:

```go
package configsnap

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "regenerate golden files in testdata/")

func sampleConfig() Config {
	return Config{
		Service:        "payments",
		Version:        "1.4.0",
		Debug:          false,
		Flags:          map[string]bool{"new_checkout": true, "beta_ui": false, "audit_log": true},
		QueryTemplate:  "value > 0 && status != <nil>",
		AllowedOrigins: []string{"https://a.example.com", "https://b.example.com"},
	}
}

func TestConfigGolden(t *testing.T) {
	got, err := MarshalConfig(sampleConfig())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join("testdata", "config.golden")
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run: go test -update)", path, err)
	}
	if !bytes.Equal(want, got) {
		t.Fatalf("config golden mismatch (run: go test -update)\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestSortedMapKeysDeterministic(t *testing.T) {
	t.Parallel()
	a := sampleConfig()
	b := sampleConfig()
	// Rebuild b's flags in a different insertion order.
	b.Flags = map[string]bool{}
	for _, k := range []string{"new_checkout", "audit_log", "beta_ui"} {
		b.Flags[k] = a.Flags[k]
	}
	ba, err := MarshalConfig(a)
	if err != nil {
		t.Fatalf("marshal a: %v", err)
	}
	bb, err := MarshalConfig(b)
	if err != nil {
		t.Fatalf("marshal b: %v", err)
	}
	if !bytes.Equal(ba, bb) {
		t.Fatalf("map key order leaked into output:\n%s\nvs\n%s", ba, bb)
	}
}

func TestEscapeHTMLPolicy(t *testing.T) {
	t.Parallel()
	got, err := MarshalConfig(sampleConfig())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(got), "value > 0 && status != <nil>") {
		t.Error("MarshalConfig should keep <, >, & literal")
	}
	// The default MarshalIndent escapes them, which is why we use an Encoder.
	def, err := json.MarshalIndent(sampleConfig(), "", "  ")
	if err != nil {
		t.Fatalf("marshalindent: %v", err)
	}
	if !strings.Contains(string(def), `\u003e`) {
		t.Error("default MarshalIndent should escape > into \\u003e")
	}
}

func ExampleMarshalConfig() {
	cfg := Config{Service: "cache", Version: "2", Debug: true, Flags: map[string]bool{"lru": true}, QueryTemplate: "n>0", AllowedOrigins: []string{"*"}}
	out, _ := MarshalConfig(cfg)
	fmt.Print(string(out))
	// Output:
	// {
	//   "service": "cache",
	//   "version": "2",
	//   "debug": true,
	//   "flags": {
	//     "lru": true
	//   },
	//   "query_template": "n>0",
	//   "allowed_origins": [
	//     "*"
	//   ]
	// }
}
```

## Review

The snapshot is correct when it is stable across runs and across insertion order:
`TestSortedMapKeysDeterministic` proves the flags map serializes identically no
matter how it was built, because `encoding/json` sorts keys. `TestEscapeHTMLPolicy`
makes the escaping lever explicit — the golden is readable only because
`MarshalConfig` disables HTML escaping, and the default escapes. The trailing-
newline policy is the quiet correctness requirement: the writer and the golden
file must agree on exactly one trailing LF, or a byte compare fails on a byte you
cannot see. When you legitimately add a config field, regenerate with
`go test -update` and read the one-line diff; a field added in the wrong
declaration position moves the whole snapshot, which is itself a useful signal.

## Resources

- [json.Encoder.SetEscapeHTML](https://pkg.go.dev/encoding/json#Encoder.SetEscapeHTML) — keeping `<`, `>`, `&` literal in a snapshot.
- [json.MarshalIndent](https://pkg.go.dev/encoding/json#MarshalIndent) — stable indentation and sorted map keys.
- [json.Encoder.SetIndent](https://pkg.go.dev/encoding/json#Encoder.SetIndent) — the encoder-based indent with an explicit trailing newline.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-sql-builder-golden.md](07-sql-builder-golden.md)
