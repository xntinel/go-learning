# Exercise 1: Upload Limit Policy From Compile-Time Byte Constants

Every service that accepts uploads needs a limit policy: how many bytes an avatar,
a report, or an archive may be. This module encodes those limits once as untyped
byte-size constant expressions and lets the *same* constants flow into three
different numeric return types — `int`, `int64`, and `float32` — so a single
source of truth adapts to every part of the config surface without conversion
noise.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
uploadlimits/                 independent module: example.com/uploadlimits
  go.mod                      go 1.26
  limits.go                   untyped 1<<10 ladder; ForKind, Accepts, DefaultBufferSize (int),
                              DefaultSampleRate32 (float32), MaxAvatarBytes64 (int64)
  cmd/
    demo/
      main.go                 prints the policy for each upload kind
  limits_test.go              behavior tests: boundaries, target-type landing, archive case
```

Files: `limits.go`, `cmd/demo/main.go`, `limits_test.go`.
Implement: a `Limit` policy keyed by upload kind (avatar 5 MiB, report 25 MiB,
archive 100 MiB), with `ForKind`, `Accepts`, and three accessors that return the
same constants as `int`, `int64`, and `float32`.
Test: boundary behavior at limit−1, limit, limit+1, and negative size; proof the
constants land in the right target types; the archive 100 MiB boundary.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/02-variables-types-and-constants/08-untyped-constants-and-constant-expressions/01-upload-limit-policy/cmd/demo
cd go-solutions/02-variables-types-and-constants/08-untyped-constants-and-constant-expressions/01-upload-limit-policy
```

### Why the constants stay untyped

The byte limits are built from a binary ladder: `bytesPerKiB = 1 << 10`,
`bytesPerMiB = bytesPerKiB * bytesPerKiB`, then `maxAvatarBytes = 5 * bytesPerMiB`.
Every one of these is an untyped integer constant — a pure compile-time number with
no `int` or `int64` glued to it. That is what lets `MaxAvatarBytes64` return the
value as an `int64`, `DefaultBufferSize` return a related constant as an `int`, and
`DefaultSampleRate32` return a floating constant as a `float32`, all from constants
that were never typed. If instead you wrote `const maxAvatarBytes int = ...`, the
`int64` accessor would need an explicit `int64(...)` conversion, and the whole
point — one flexible source of truth — would be lost.

The policy itself is deliberately boring, which is the production shape: `ForKind`
maps a string kind to a `Limit{Kind, MaxBytes}` or an error, and `Accepts`
enforces the closed interval `[0, MaxBytes]`. Note `MaxBytes` is `int64` because
byte counts on real uploads exceed 2 GiB on 32-bit builds; the untyped constant
fits `int64` without a cast at the struct literal.

Create `limits.go`:

```go
package uploadlimits

import "fmt"

const (
	bytesPerKiB = 1 << 10
	bytesPerMiB = bytesPerKiB * bytesPerKiB

	maxAvatarBytes  = 5 * bytesPerMiB
	maxReportBytes  = 25 * bytesPerMiB
	maxArchiveBytes = 100 * bytesPerMiB

	defaultSampleRate = 1.0 / 10
)

// Limit is the upload policy for one kind of object.
type Limit struct {
	Kind     string
	MaxBytes int64
}

// ForKind returns the limit policy for an upload kind, or an error for an
// unknown kind.
func ForKind(kind string) (Limit, error) {
	switch kind {
	case "avatar":
		return Limit{Kind: kind, MaxBytes: maxAvatarBytes}, nil
	case "report":
		return Limit{Kind: kind, MaxBytes: maxReportBytes}, nil
	case "archive":
		return Limit{Kind: kind, MaxBytes: maxArchiveBytes}, nil
	default:
		return Limit{}, fmt.Errorf("unknown upload kind: %q", kind)
	}
}

// Accepts reports whether a payload of sizeBytes is allowed for the kind. A
// negative size is never accepted; the boundary at MaxBytes is inclusive.
func Accepts(kind string, sizeBytes int64) (bool, error) {
	limit, err := ForKind(kind)
	if err != nil {
		return false, err
	}
	return sizeBytes >= 0 && sizeBytes <= limit.MaxBytes, nil
}

// DefaultBufferSize returns a 64 KiB read buffer size as an int. The same
// 1<<10 ladder that sizes the limits sizes the buffer.
func DefaultBufferSize() int {
	return 64 * bytesPerKiB
}

// DefaultSampleRate32 returns the upload-audit sample rate as a float32. The
// untyped floating constant lands in float32 without a surprise.
func DefaultSampleRate32() float32 {
	return defaultSampleRate
}

// MaxAvatarBytes64 returns the avatar limit as an int64. The same untyped
// constant that built the Limit flows here without a conversion.
func MaxAvatarBytes64() int64 {
	return maxAvatarBytes
}
```

### The runnable demo

Create `cmd/demo/main.go`. Because `cmd/demo` is a separate `package main`, it can
touch only exported API — which is why the accessors exist:

```go
package main

import (
	"fmt"

	"example.com/uploadlimits"
)

func main() {
	for _, kind := range []string{"avatar", "report", "archive"} {
		limit, err := uploadlimits.ForKind(kind)
		if err != nil {
			fmt.Println("error:", err)
			continue
		}
		fmt.Printf("%-8s max=%d bytes (%d MiB)\n", limit.Kind, limit.MaxBytes, limit.MaxBytes/(1<<20))
	}

	fmt.Printf("buffer=%d bytes sampleRate=%.2f avatarMax=%d\n",
		uploadlimits.DefaultBufferSize(),
		uploadlimits.DefaultSampleRate32(),
		uploadlimits.MaxAvatarBytes64())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
avatar   max=5242880 bytes (5 MiB)
report   max=26214400 bytes (25 MiB)
archive  max=104857600 bytes (100 MiB)
buffer=65536 bytes sampleRate=0.10 avatarMax=5242880
```

### Tests

The tests prove behavior, not type names — they never use `%T`. `TestAccepts`
pins the boundary exactly at limit−1, limit, and limit+1, plus the negative-size
case, for both a small kind (avatar) and the new archive kind at 100 MiB.
`TestConstantsLandInTargetTypes` assigns each accessor's result to an explicitly
typed variable, which is the real proof that the untyped constants fit `int`,
`int64`, and `float32` without a conversion.

Create `limits_test.go`:

```go
package uploadlimits

import "testing"

func TestForKindByteConstants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		kind string
		want int64
	}{
		{"avatar", 5 * 1024 * 1024},
		{"report", 25 * 1024 * 1024},
		{"archive", 100 * 1024 * 1024},
	}
	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			t.Parallel()
			got, err := ForKind(tt.kind)
			if err != nil {
				t.Fatal(err)
			}
			if got.MaxBytes != tt.want {
				t.Fatalf("ForKind(%q).MaxBytes = %d, want %d", tt.kind, got.MaxBytes, tt.want)
			}
		})
	}
}

func TestForKindUnknown(t *testing.T) {
	t.Parallel()
	if _, err := ForKind("video"); err == nil {
		t.Fatal("ForKind(video) = nil error, want unknown-kind error")
	}
}

func TestAccepts(t *testing.T) {
	t.Parallel()

	const avatar = 5 * 1024 * 1024
	const archive = 100 * 1024 * 1024

	tests := []struct {
		name string
		kind string
		size int64
		want bool
	}{
		{"avatar under limit", "avatar", avatar - 1, true},
		{"avatar at limit", "avatar", avatar, true},
		{"avatar over limit", "avatar", avatar + 1, false},
		{"avatar negative", "avatar", -1, false},
		{"archive under limit", "archive", archive - 1, true},
		{"archive at limit", "archive", archive, true},
		{"archive over limit", "archive", archive + 1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Accepts(tt.kind, tt.size)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("Accepts(%q, %d) = %v, want %v", tt.kind, tt.size, got, tt.want)
			}
		})
	}
}

func TestConstantsLandInTargetTypes(t *testing.T) {
	t.Parallel()

	var bufferSize int = DefaultBufferSize()
	var avatarLimit int64 = MaxAvatarBytes64()
	var sampleRate float32 = DefaultSampleRate32()

	if bufferSize != 64*1024 {
		t.Fatalf("bufferSize = %d, want %d", bufferSize, 64*1024)
	}
	if avatarLimit != 5*1024*1024 {
		t.Fatalf("avatarLimit = %d, want %d", avatarLimit, 5*1024*1024)
	}
	if sampleRate <= 0 || sampleRate >= 1 {
		t.Fatalf("sampleRate = %v, want in (0,1)", sampleRate)
	}
}
```

## Review

The policy is correct when `Accepts` is exactly the closed interval `[0, MaxBytes]`
for a known kind and an error for an unknown one, and when each accessor returns
its constant in the target type without a cast at the call site. The type test is
the real proof of the lesson: it assigns to `int`, `int64`, and `float32` variables
directly, which only compiles because the constants were left untyped. The mistake
to avoid is pinning a type on the ladder constants "for clarity" — that would force
`int64(...)` at every wide call site and defeat the single source of truth. The
archive case exists to show the same untyped ladder scaling to 100 MiB with no new
machinery.

## Resources

- [Go Language Specification: Constants](https://go.dev/ref/spec#Constants) — value, kind, and how a constant takes a type from context.
- [The Go Blog: Constants](https://go.dev/blog/constants) — Rob Pike on untyped constants and default types.
- [Go Language Specification: Constant expressions](https://go.dev/ref/spec#Constant_expressions) — compile-time evaluation and the fit-the-target rule.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-retry-budget-durations.md](02-retry-budget-durations.md)
