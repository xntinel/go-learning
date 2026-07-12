# Exercise 13: Route a File to a Storage Class With Multi-Value Cases

**Nivel: Intermedio** — validacion rapida (un test corto).

An object-storage upload path needs to pick a storage class before it writes
a single byte: images and video go to one tier, cold backups to another, and
whatever nobody anticipated goes somewhere safe by default. This module
builds that router as an expression switch on the normalized file extension,
where comma-separated case lists group families of extensions into one rule
each.

## What you'll build

```text
storageclass/               independent module: example.com/file-extension-storage-class
  go.mod                     go 1.24
  storageclass.go            package storageclass; ErrNoExtension; ClassOf(filename) (string, error)
  storageclass_test.go       table over each class family, an unknown extension, and no extension
```

- Implement: `ClassOf(filename string) (string, error)` — normalize the extension, then an expression switch with one comma-separated case per storage-class family and a safe, non-error default.
- Test: a table covering one filename per family (mixed case, to prove normalization), an unrecognized extension, and a filename with no extension at all.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why comma case lists, not five separate cases

Five extensions — `jpg`, `jpeg`, `png`, `gif`, `webp` — all mean the same
thing here: "this is an image, put it in the images tier." Writing five
separate cases with identical bodies invites the maintenance bug where a
sixth image extension gets added to one switch and forgotten in another. A
single `case "jpg", "jpeg", "png", "gif", "webp":` states the equivalence
once, as data, and there is nothing to keep in sync when the list grows —
you edit the one case. Each family here is intentionally a comma list rather
than a written-out OR chain, for exactly the reason a comma list exists: an
expression switch case matches when the tag equals *any* of the listed
values.

Unlike the log-level loader, an unrecognized extension is not an error here —
it is a legitimate, if unremarkable, file, so the default hands it a
conservative general-purpose tier instead of failing the upload. A missing
extension, on the other hand, is refused outright: it is usually a sign of a
misconfigured upload path, not a real file type, so `ClassOf` fails closed on
that one specific shape of bad input via a named sentinel.

Create `storageclass.go`:

```go
package storageclass

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// ErrNoExtension marks a filename with no extension to classify.
var ErrNoExtension = errors.New("storageclass: no extension")

// ClassOf maps a filename to the storage class its contents should live in,
// based on the file extension alone.
func ClassOf(filename string) (string, error) {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(filename), "."))
	if ext == "" {
		return "", fmt.Errorf("%w: %q", ErrNoExtension, filename)
	}

	switch ext {
	case "jpg", "jpeg", "png", "gif", "webp":
		return "images-standard", nil
	case "mp4", "mov", "avi", "mkv":
		return "video-standard", nil
	case "csv", "json", "xml", "txt", "md":
		return "documents-standard", nil
	case "log":
		return "logs-infrequent-access", nil
	case "bak", "old", "tmp":
		return "archive-glacier", nil
	default:
		return "general-standard", nil
	}
}
```

### Test

`TestClassOf` runs a table over one filename per family — with mixed case in
the extension to prove `strings.ToLower` runs before the switch, not after —
plus an unrecognized extension that should land safely in the default, and a
filename with no extension that should fail via `ErrNoExtension`.

Create `storageclass_test.go`:

```go
package storageclass

import (
	"errors"
	"testing"
)

func TestClassOf(t *testing.T) {
	t.Parallel()

	tests := []struct {
		filename string
		want     string
		wantErr  bool
	}{
		{"photo.JPG", "images-standard", false},
		{"clip.mp4", "video-standard", false},
		{"report.CSV", "documents-standard", false},
		{"notes.md", "documents-standard", false},
		{"app.log", "logs-infrequent-access", false},
		{"session.bak", "archive-glacier", false},
		{"data.parquet", "general-standard", false},
		{"README", "", true},
	}

	for _, tc := range tests {
		got, err := ClassOf(tc.filename)
		if tc.wantErr {
			if !errors.Is(err, ErrNoExtension) {
				t.Errorf("ClassOf(%q) error = %v, want errors.Is match for ErrNoExtension", tc.filename, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("ClassOf(%q) unexpected error: %v", tc.filename, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ClassOf(%q) = %q, want %q", tc.filename, got, tc.want)
		}
	}
}
```

Verify with:

```bash
go test -count=1 ./...
```

## Review

The router is correct when every extension in a family lands on that
family's class regardless of how it was cased, an unrecognized extension
still gets a usable (if unremarkable) class instead of blocking the upload,
and a missing extension is refused by name. Carry this forward: when several
raw values share one behavior, express that with a comma case list instead of
duplicating the case body, and decide deliberately whether "unrecognized"
should be a safe default or a fail-closed error — they are not the same
shape of unknown.

## Resources

- [Go Specification: Switch statements](https://go.dev/ref/spec#Switch_statements) — comma-separated expression list per case.
- [path/filepath.Ext](https://pkg.go.dev/path/filepath#Ext) — extracting the extension before normalizing it.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-card-brand-detector.md](12-card-brand-detector.md) | Next: [14-db-error-code-mapper.md](14-db-error-code-mapper.md)
