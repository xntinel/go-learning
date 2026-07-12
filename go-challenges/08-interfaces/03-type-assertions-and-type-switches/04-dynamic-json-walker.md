# Exercise 4: Walk Decoded-Into-any JSON Without Panicking

When you decode arbitrary JSON into `any` — a webhook payload, a config blob, a
log line — you get a tree of exactly six shapes and no schema. Walking it to
collect leaf paths or redact secrets means a type switch that covers those shapes
exactly, with the one detail that trips everyone: every JSON number arrives as
`float64`, so there is no `int` case.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
jsonwalk/                   independent module: example.com/jsonwalk
  go.mod                    module path
  jsonwalk.go               Decode, LeafPaths (walk to scalars), Redact (mask secret keys)
  cmd/
    demo/
      main.go               runnable demo over a nested payload
  jsonwalk_test.go          leaf coverage, integer-is-float64 guard, top-level scalar, redaction
```

Files: `jsonwalk.go`, `cmd/demo/main.go`, `jsonwalk_test.go`.
Implement: `Decode(data) (any, error)`, `LeafPaths(v) []string`, `Redact(v, secrets) any`, all type-switching over the six JSON shapes.
Test: nested payload of objects/arrays/strings/ints/floats/bools/null; assert every leaf is visited, an integer arrives as `float64`, a top-level scalar does not panic, and secret keys are masked.
Verify: `go test -count=1 -race ./...`

### The six shapes, and the float64 trap

`json.Unmarshal(data, &v)` where `v` is `any` produces exactly these dynamic types:
`map[string]any` for a JSON object, `[]any` for an array, `string`, `float64` for
*every* number, `bool`, and `nil` for JSON `null`. A total walker type-switches over
precisely those six. The trap is the number case: `{"port": 8080}` decodes `8080`
as `float64(8080)`, not `int`. An `int` arm in the switch is dead code — it never
matches — and integers fall through to `default`, which in a redactor means a leaf
you meant to visit is silently skipped. The `walk` function below has a `float64`
arm and no `int` arm on purpose; if you need an integer you narrow the `float64`.
(If you truly need integer fidelity for very large values, `json.Decoder.UseNumber`
yields `json.Number` instead — a different opt-in shape, out of scope here.)

The `default` case is not decoration. In principle `json.Unmarshal` into `any`
cannot produce anything outside the six shapes, but a `default` that records an
unknown marker means that if you ever feed the walker a value from another source
(a hand-built `map[string]any` containing an `int`, say) the anomaly is visible
rather than dropped. `LeafPaths` visits every scalar and records `path=value`;
`Redact` rebuilds the tree, replacing the value of any secret key with a mask,
recursing through objects and arrays and returning non-container values unchanged.
Both are careful never to use the panic form — a decoded payload is the most
hostile boundary there is.

Create `jsonwalk.go`:

```go
package jsonwalk

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
)

// Decode unmarshals JSON into any, producing the six dynamic shapes:
// map[string]any, []any, string, float64, bool, nil.
func Decode(data []byte) (any, error) {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// LeafPaths returns "path=value" for every scalar leaf, sorted for determinism.
// A top-level scalar has the empty path. Numbers render from float64.
func LeafPaths(v any) []string {
	leaves := map[string]string{}
	walk("", v, leaves)

	paths := make([]string, 0, len(leaves))
	for p := range leaves {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = p + "=" + leaves[p]
	}
	return out
}

func walk(path string, v any, out map[string]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			walk(join(path, k), child, out)
		}
	case []any:
		for i, child := range x {
			walk(join(path, strconv.Itoa(i)), child, out)
		}
	case string:
		out[path] = x
	case float64: // every JSON number lands here; there is no int case
		out[path] = strconv.FormatFloat(x, 'g', -1, 64)
	case bool:
		out[path] = strconv.FormatBool(x)
	case nil:
		out[path] = "null"
	default:
		out[path] = fmt.Sprintf("<unexpected %T>", v)
	}
}

func join(base, key string) string {
	if base == "" {
		return key
	}
	return base + "." + key
}

// Redact returns a deep copy of v with the value of any key present in secrets
// replaced by "***". It recurses through objects and arrays; other values pass
// through unchanged.
func Redact(v any, secrets map[string]bool) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, child := range x {
			if secrets[k] {
				out[k] = "***"
				continue
			}
			out[k] = Redact(child, secrets)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, child := range x {
			out[i] = Redact(child, secrets)
		}
		return out
	default:
		return v
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"

	"example.com/jsonwalk"
)

func main() {
	payload := []byte(`{
		"service": "auth",
		"port": 8080,
		"ratio": 0.5,
		"enabled": true,
		"password": "hunter2",
		"tags": ["a", "b"],
		"note": null
	}`)

	v, err := jsonwalk.Decode(payload)
	if err != nil {
		fmt.Println("decode error:", err)
		return
	}

	for _, leaf := range jsonwalk.LeafPaths(v) {
		fmt.Println(leaf)
	}

	redacted := jsonwalk.Redact(v, map[string]bool{"password": true})
	b, _ := json.Marshal(redacted)
	fmt.Println("redacted:", string(b))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
enabled=true
note=null
password=hunter2
port=8080
ratio=0.5
service=auth
tags.0=a
tags.1=b
redacted: {"enabled":true,"note":null,"password":"***","port":8080,"ratio":0.5,"service":"auth","tags":["a","b"]}
```

### Tests

The critical assertion is that the integer `8080` decodes to `float64`, guarding
against anyone "fixing" the walker by adding a dead `int` case. The other cases
prove every leaf is visited, a bare top-level scalar does not panic, and redaction
masks the secret while leaving structure intact.

Create `jsonwalk_test.go`:

```go
package jsonwalk

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestIntegerDecodesAsFloat64(t *testing.T) {
	t.Parallel()
	v, err := Decode([]byte(`{"port": 8080}`))
	if err != nil {
		t.Fatal(err)
	}
	m := v.(map[string]any)
	if _, ok := m["port"].(float64); !ok {
		t.Fatalf("port has type %T, want float64", m["port"])
	}
	if _, ok := m["port"].(int); ok {
		t.Fatal("port unexpectedly asserted as int")
	}
}

func TestLeafPathsVisitsEveryLeaf(t *testing.T) {
	t.Parallel()
	v, err := Decode([]byte(`{"a":1,"b":{"c":"x"},"d":[true,null]}`))
	if err != nil {
		t.Fatal(err)
	}
	got := LeafPaths(v)
	want := []string{"a=1", "b.c=x", "d.0=true", "d.1=null"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LeafPaths = %v, want %v", got, want)
	}
}

func TestTopLevelScalarDoesNotPanic(t *testing.T) {
	t.Parallel()
	for _, in := range []string{`42`, `"hello"`, `true`, `null`} {
		v, err := Decode([]byte(in))
		if err != nil {
			t.Fatalf("Decode(%s): %v", in, err)
		}
		paths := LeafPaths(v) // must not panic; leaf has empty path
		if len(paths) != 1 || !strings.HasPrefix(paths[0], "=") {
			t.Fatalf("LeafPaths(%s) = %v, want one empty-path leaf", in, paths)
		}
	}
}

func TestRedactMasksSecretKeys(t *testing.T) {
	t.Parallel()
	v, err := Decode([]byte(`{"user":"alice","password":"pw","nested":{"password":"pw2"}}`))
	if err != nil {
		t.Fatal(err)
	}
	out := Redact(v, map[string]bool{"password": true})
	b, _ := json.Marshal(out)
	got := string(b)
	if strings.Contains(got, "pw") {
		t.Fatalf("redacted output still contains a secret: %s", got)
	}
	if !strings.Contains(got, `"user":"alice"`) {
		t.Fatalf("redaction dropped a non-secret field: %s", got)
	}
}

func ExampleLeafPaths() {
	v, _ := Decode([]byte(`{"port":8080,"name":"auth"}`))
	fmt.Println(LeafPaths(v))
	// Output: [name=auth port=8080]
}
```

## Review

The walker is correct when its type switch covers the six JSON shapes and only
those, with `float64` (never `int`) as the number case, and when no arm uses the
panic form. `TestIntegerDecodesAsFloat64` is the guard that keeps a well-meaning
`int` case out; `TestTopLevelScalarDoesNotPanic` proves a bare scalar is walked, not
crashed. Redaction rebuilds the tree rather than mutating in place, so the caller's
original value is untouched. The one subtlety to remember is that a `default` arm is
still worth keeping even though `json.Unmarshal` cannot produce a seventh shape: it
makes a hand-built `map[string]any` carrying an `int` observable instead of silently
dropped. Run `go test -race` to confirm.

## Resources

- [json.Unmarshal](https://pkg.go.dev/encoding/json#Unmarshal) — the six shapes produced when decoding into `any`.
- [json.Decoder.UseNumber](https://pkg.go.dev/encoding/json#Decoder.UseNumber) — opt into `json.Number` instead of `float64`.
- [Go Specification: Type switches](https://go.dev/ref/spec#Type_switches)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-responsewriter-interface-upgrades.md](03-responsewriter-interface-upgrades.md) | Next: [05-domain-error-to-http-status.md](05-domain-error-to-http-status.md)
