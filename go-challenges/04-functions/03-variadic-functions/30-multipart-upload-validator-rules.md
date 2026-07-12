# Exercise 30: Multipart Form Upload Validator with Rules

**Nivel: Intermedio** — validacion rapida (un test corto).

A form with an avatar image field and a resume PDF field has two entirely
different validation policies per field: the avatar cares about image
formats and a small size cap, the resume cares about PDF-only and a much
larger cap. `ValidateUpload(files, rules...)` runs a variadic list of
per-field rules against every uploaded file and reports every violation
across every field in one pass, rather than bailing out on the first bad
file and leaving the rest of the batch unchecked.

## What you'll build

```text
uploadval/                 independent module: example.com/uploadval
  go.mod                   go 1.24
  uploadval.go             package uploadval; type FieldFile struct{Field, ContentType string; Size int64}; type Rule func(FieldFile) error; MaxSize, AllowedContentType; ValidateUpload(files []FieldFile, rules ...Rule) error
  cmd/
    demo/
      main.go              runnable demo: a good avatar, an oversized resume, and a wrong-format avatar
  uploadval_test.go         table tests: all valid, aggregated cross-file violations, rules scoped to their own field, empty upload
```

- Files: `uploadval.go`, `cmd/demo/main.go`, `uploadval_test.go`.
- Implement: `type FieldFile struct{ Field, ContentType string; Size int64 }`, `type Rule func(FieldFile) error`, constructors `MaxSize(field string, max int64) Rule` and `AllowedContentType(field string, allowed ...string) Rule`, and `ValidateUpload(files []FieldFile, rules ...Rule) error`.
- Test: an all-valid upload returns `nil`; a batch with an oversized resume and a wrong-format avatar returns one error mentioning both; a rule for `"avatar"` never fires against a `"resume"` file; an empty file list is always valid.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why rules are field-scoped, and why nested variadic (`AllowedContentType(field, types...)`) still composes

Every `Rule` here takes the *whole* `FieldFile`, including its `Field`
name, and returns `nil` immediately if the field does not match the one
it cares about. This is what lets `ValidateUpload` stay a flat, uniform
loop — "for every file, run every rule" — instead of needing a
`map[string][]Rule` keyed by field name that the caller has to build and
keep in sync with the form's field list. The cost of that simplicity is
that each rule constructor takes the field name as a parameter and
filters internally; the benefit is that the caller-facing API is one flat
`rules ...Rule` list that reads like a policy document: "avatar caps at
1MB and must be PNG or JPEG, resume caps at 5MB and must be PDF" is
almost literally `MaxSize("avatar", 1_000_000),
AllowedContentType("avatar", "image/png", "image/jpeg"), MaxSize("resume",
5_000_000), AllowedContentType("resume", "application/pdf")`.

`AllowedContentType(field string, allowed ...string)` is itself variadic,
nested one level inside another variadic call (`ValidateUpload(files,
rules...)`), and that composes without friction: `AllowedContentType`
collects its own `allowed` slice at the point it is called, builds one
`Rule` closure that closes over it, and that single `Rule` value is what
gets appended into the outer `rules` list — the two variadic layers never
interact or interfere with each other, because each variadic parameter is
fully consumed into its own closure before the next `...` comes into
play.

Create `uploadval.go`:

```go
// uploadval.go
package uploadval

import (
	"errors"
	"fmt"
	"slices"
)

// FieldFile describes one file received in a multipart form field.
type FieldFile struct {
	Field       string
	ContentType string
	Size        int64
}

// Rule inspects one FieldFile and returns an error describing why it is
// rejected, or nil if it is acceptable. A Rule that does not apply to a
// given field (wrong Field name) should return nil.
type Rule func(FieldFile) error

// ValidateUpload runs every rule against every file and aggregates all of
// the resulting errors with errors.Join, so a caller uploading five files
// with three separate problems learns about all three in one response.
func ValidateUpload(files []FieldFile, rules ...Rule) error {
	var errs []error
	for _, f := range files {
		for _, rule := range rules {
			if err := rule(f); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// MaxSize returns a Rule that rejects files in field larger than max
// bytes. It ignores files in other fields.
func MaxSize(field string, max int64) Rule {
	return func(f FieldFile) error {
		if f.Field != field {
			return nil
		}
		if f.Size > max {
			return fmt.Errorf("%s: size %d bytes exceeds max %d", field, f.Size, max)
		}
		return nil
	}
}

// AllowedContentType returns a Rule that rejects files in field whose
// ContentType is not one of allowed. It ignores files in other fields.
func AllowedContentType(field string, allowed ...string) Rule {
	return func(f FieldFile) error {
		if f.Field != field {
			return nil
		}
		if slices.Contains(allowed, f.ContentType) {
			return nil
		}
		return fmt.Errorf("%s: content type %q not in allowed %v", field, f.ContentType, allowed)
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/uploadval"
)

func main() {
	files := []uploadval.FieldFile{
		{Field: "avatar", ContentType: "image/png", Size: 500_000},
		{Field: "resume", ContentType: "application/pdf", Size: 12_000_000},
		{Field: "avatar", ContentType: "image/gif", Size: 100},
	}

	rules := []uploadval.Rule{
		uploadval.MaxSize("avatar", 1_000_000),
		uploadval.AllowedContentType("avatar", "image/png", "image/jpeg"),
		uploadval.MaxSize("resume", 5_000_000),
		uploadval.AllowedContentType("resume", "application/pdf"),
	}

	err := uploadval.ValidateUpload(files, rules...)
	if err == nil {
		fmt.Println("all files valid")
		return
	}
	fmt.Println("upload errors:")
	fmt.Println(err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
upload errors:
resume: size 12000000 bytes exceeds max 5000000
avatar: content type "image/gif" not in allowed [image/png image/jpeg]
```

### Tests

`TestRulesIgnoreOtherFields` is the one that pins field-scoping: a
`MaxSize("avatar", 1)` and `AllowedContentType("avatar", "image/png")`
must both stay silent against a `"resume"` file no matter how large it is
or what content type it has, because neither rule was ever meant to apply
to that field.

Create `uploadval_test.go`:

```go
// uploadval_test.go
package uploadval

import (
	"strings"
	"testing"
)

func TestValidateUploadAllValidReturnsNil(t *testing.T) {
	t.Parallel()

	files := []FieldFile{
		{Field: "avatar", ContentType: "image/png", Size: 500_000},
	}
	err := ValidateUpload(files,
		MaxSize("avatar", 1_000_000),
		AllowedContentType("avatar", "image/png", "image/jpeg"),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateUploadAggregatesAcrossFiles(t *testing.T) {
	t.Parallel()

	files := []FieldFile{
		{Field: "avatar", ContentType: "image/png", Size: 500_000},
		{Field: "resume", ContentType: "application/pdf", Size: 12_000_000},
		{Field: "avatar", ContentType: "image/gif", Size: 100},
	}
	rules := []Rule{
		MaxSize("avatar", 1_000_000),
		AllowedContentType("avatar", "image/png", "image/jpeg"),
		MaxSize("resume", 5_000_000),
		AllowedContentType("resume", "application/pdf"),
	}

	err := ValidateUpload(files, rules...)
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "resume") || !strings.Contains(msg, "exceeds max") {
		t.Errorf("missing resume size violation: %q", msg)
	}
	if !strings.Contains(msg, "image/gif") {
		t.Errorf("missing avatar content-type violation: %q", msg)
	}
	if strings.Count(msg, "\n") != 1 {
		t.Errorf("expected exactly 2 joined errors, got message %q", msg)
	}
}

func TestRulesIgnoreOtherFields(t *testing.T) {
	t.Parallel()

	files := []FieldFile{
		{Field: "resume", ContentType: "application/pdf", Size: 100},
	}
	err := ValidateUpload(files, MaxSize("avatar", 1), AllowedContentType("avatar", "image/png"))
	if err != nil {
		t.Fatalf("rules for a different field should not apply: %v", err)
	}
}

func TestValidateUploadNoFilesIsValid(t *testing.T) {
	t.Parallel()

	if err := ValidateUpload(nil, MaxSize("avatar", 1)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
```

## Review

`ValidateUpload` is correct when every rule runs against every file, a
rule scoped to one field never rejects (or passes judgment on) a
different field, and every violation across the whole batch is reported
together. The senior point is the field-scoping pattern itself: rather
than the caller pre-sorting files into per-field buckets and calling a
different validator per bucket, each `Rule` closure carries its own
target field and silently no-ops on everything else, which keeps the
public API a single flat variadic list that reads as a declarative policy
rather than an if/else tree the caller has to maintain by hand.

## Resources

- [`net/http`: parsing multipart forms](https://pkg.go.dev/net/http#Request.ParseMultipartForm)
- [`mime/multipart`](https://pkg.go.dev/mime/multipart)
- [`errors.Join`](https://pkg.go.dev/errors#Join)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [29-schema-migration-sequencer-graph.md](29-schema-migration-sequencer-graph.md) | Next: [31-pubsub-broadcast-subscriber-fanout.md](31-pubsub-broadcast-subscriber-fanout.md)
