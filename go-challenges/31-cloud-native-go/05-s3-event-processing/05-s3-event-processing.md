# 5. S3 Event Processing

S3 event notifications are the most common Lambda trigger for data pipelines. The hard parts are not the handler signature — they are the details AWS buries in the payload: object keys are `application/x-www-form-urlencoded` (spaces become `+`, not `%20`), a single event batch can carry multiple records, `EventName` does not carry the `s3:` prefix it has in bucket notification configs, and the right response to an unprocessable record is to log-and-continue rather than abort the whole batch.

This lesson builds a self-contained `s3processor` package: a handler that decodes keys, filters by extension and event type, records per-record results, and surfaces the first fatal error. The package is tested without a real AWS connection using fabricated `events.S3Event` values.

```text
s3processor/
  go.mod
  processor.go
  processor_test.go
  cmd/demo/main.go
```

## Concepts

### The S3Event Payload Structure

AWS delivers a batch of notifications as one `events.S3Event`:

```go
type S3Event struct {
	Records []S3EventRecord `json:"Records"`
}

type S3EventRecord struct {
	EventVersion string    `json:"eventVersion"`
	EventSource  string    `json:"eventSource"`
	AWSRegion    string    `json:"awsRegion"`
	EventTime    time.Time `json:"eventTime"`
	EventName    string    `json:"eventName"` // e.g. "ObjectCreated:Put"
	S3           S3Entity  `json:"s3"`
}

type S3Entity struct {
	Bucket S3Bucket `json:"bucket"`
	Object S3Object `json:"object"`
}

type S3Bucket struct {
	Name string `json:"name"`
	Arn  string `json:"arn"`
}

type S3Object struct {
	Key       string `json:"key"`       // URL-encoded
	Size      int64  `json:"size,omitempty"`
	VersionID string `json:"versionId"`
	ETag      string `json:"eTag"`
	Sequencer string `json:"sequencer"`
}
```

`EventName` values in the payload match the AWS notification type list but without the `s3:` prefix. A `PUT` upload arrives as `"ObjectCreated:Put"`, not `"s3:ObjectCreated:Put"`. This distinction trips up every developer who reads the S3 console config (which shows `s3:ObjectCreated:Put`) before looking at the actual event JSON.

### URL Decoding Object Keys

The AWS documentation states explicitly: "The object key name value is URL encoded. For example, `red flower.jpg` becomes `red+flower.jpg`. (Amazon S3 returns `application/x-www-form-urlencoded` as the content type in the response.)"

The correct decoder is `url.QueryUnescape`, which converts `+` back to space and handles `%XX` sequences. `url.PathUnescape` does not convert `+` to space, so a key like `reports/q1+summary.csv` would be decoded incorrectly as `reports/q1+summary.csv` instead of `reports/q1 summary.csv`.

```go
import "net/url"

decoded, err := url.QueryUnescape(record.S3.Object.Key)
if err != nil {
	// key is malformed — log and skip; do not abort the batch
}
```

Always call `url.QueryUnescape` before any path inspection (`filepath.Ext`, string comparison, logging). The raw `Key` value from the event must never be used as a filesystem path or displayed to a user.

### Filtering Records: Extension and Event Type

Two filters gate processing:

1. **Event type** — check `record.EventName` against the set of events the handler cares about. A handler that processes new uploads checks for `"ObjectCreated:Put"`, `"ObjectCreated:Post"`, and `"ObjectCreated:CompleteMultipartUpload"`. Wildcards like `"ObjectCreated:*"` do not arrive in the payload; matching must be done by prefix or exact set.

2. **File extension** — `filepath.Ext(decodedKey)` returns the extension including the leading dot (`.csv`, not `csv`). Comparing the result to `".csv"` is correct; comparing to `"csv"` is wrong and matches nothing.

### Batch Semantics: Continue or Abort

S3 delivers a batch asynchronously. Returning an error from the handler causes Lambda to retry the entire batch. For data-ingestion pipelines, the correct behavior depends on failure mode:

- **Transient failure** (downstream unavailable): return the error; let Lambda retry.
- **Poison record** (malformed key, wrong content type): log and continue. Aborting retries a malformed record forever.

The lesson models this with an explicit `Result` per record: the handler accumulates results and returns the first processing error (which would be transient in production). Skipped records (wrong event type, wrong extension) are not errors.

### Testing Without AWS

`events.S3Event` is a plain struct — no SDK client, no credentials, no network. Construct test events directly:

```go
events.S3Event{
	Records: []events.S3EventRecord{
		{
			EventName: "ObjectCreated:Put",
			S3: events.S3Entity{
				Bucket: events.S3Bucket{Name: "my-bucket"},
				Object: events.S3Object{Key: "data/q1+report.csv", Size: 4096},
			},
		},
	},
}
```

The handler under test is a plain function; no Lambda runtime is involved. This makes the full test suite hermetic — no network, no mocks, no build tags.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/31-cloud-native-go/05-s3-event-processing/05-s3-event-processing/cmd/demo
cd go-solutions/31-cloud-native-go/05-s3-event-processing/05-s3-event-processing
go get github.com/aws/aws-lambda-go@v1.47.0
```

This is a library with a thin `cmd/demo` entry point. Verification is done with `go test`, not `go run`.

### Exercise 1: The Result Type and Sentinel Errors

Create `processor.go`:

```go
package s3processor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/aws/aws-lambda-go/events"
)

// Sentinel errors for classifying outcomes without string matching.
var (
	ErrMalformedKey    = errors.New("s3processor: malformed object key")
	ErrUnsupportedType = errors.New("s3processor: unsupported event type")
	ErrUnsupportedExt  = errors.New("s3processor: unsupported file extension")
)

// Result carries the outcome of processing a single S3 event record.
type Result struct {
	Bucket     string
	Key        string // decoded key
	Size       int64
	EventName  string
	Skipped    bool // true when the record was filtered, not processed
	SkipReason string
}

// allowedEventTypes is the set of event names this processor accepts.
// EventName in the payload does NOT carry the "s3:" prefix that appears
// in bucket notification configurations.
var allowedEventTypes = map[string]bool{
	"ObjectCreated:Put":                     true,
	"ObjectCreated:Post":                    true,
	"ObjectCreated:CompleteMultipartUpload": true,
}

// Processor holds configuration for the S3 event handler.
type Processor struct {
	extensions []string // accepted extensions, e.g. ".csv"
	logger     *slog.Logger
}

// New creates a Processor that accepts the given lowercase file extensions
// (including the leading dot, e.g. ".csv"). If no extensions are provided,
// all extensions are accepted.
func New(extensions []string, logger *slog.Logger) *Processor {
	if logger == nil {
		logger = slog.Default()
	}
	exts := make([]string, len(extensions))
	for i, e := range extensions {
		exts[i] = strings.ToLower(e)
	}
	return &Processor{extensions: exts, logger: logger}
}

// Handle processes an S3 event batch. It returns the Results for every record
// (including skipped ones) and the first non-skip processing error, if any.
// Skipped records are not errors.
func (p *Processor) Handle(ctx context.Context, event events.S3Event) ([]Result, error) {
	results := make([]Result, 0, len(event.Records))
	var firstErr error

	for _, rec := range event.Records {
		r, err := p.processRecord(ctx, rec)
		results = append(results, r)
		if err != nil && !errors.Is(err, ErrUnsupportedType) && !errors.Is(err, ErrUnsupportedExt) {
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	return results, firstErr
}

func (p *Processor) processRecord(ctx context.Context, rec events.S3EventRecord) (Result, error) {
	bucket := rec.S3.Bucket.Name
	rawKey := rec.S3.Object.Key
	size := rec.S3.Object.Size
	eventName := rec.EventName

	// Decode the URL-encoded key. S3 uses application/x-www-form-urlencoded:
	// spaces become "+", percent-encoding for other special characters.
	decodedKey, err := url.QueryUnescape(rawKey)
	if err != nil {
		err = fmt.Errorf("%w: %q: %v", ErrMalformedKey, rawKey, err)
		p.logger.ErrorContext(ctx, "malformed key", "bucket", bucket, "raw_key", rawKey, "error", err)
		return Result{Bucket: bucket, Key: rawKey, Size: size, EventName: eventName}, err
	}

	// Filter by event type. Only process object-creation events.
	if !allowedEventTypes[eventName] {
		p.logger.InfoContext(ctx, "skipping non-creation event",
			"bucket", bucket, "key", decodedKey, "event_name", eventName)
		return Result{
			Bucket:     bucket,
			Key:        decodedKey,
			Size:       size,
			EventName:  eventName,
			Skipped:    true,
			SkipReason: fmt.Sprintf("event type %q not in allowed set", eventName),
		}, ErrUnsupportedType
	}

	// Filter by file extension.
	ext := strings.ToLower(filepath.Ext(decodedKey))
	if len(p.extensions) > 0 && !p.acceptsExt(ext) {
		p.logger.InfoContext(ctx, "skipping unsupported extension",
			"bucket", bucket, "key", decodedKey, "ext", ext)
		return Result{
			Bucket:     bucket,
			Key:        decodedKey,
			Size:       size,
			EventName:  eventName,
			Skipped:    true,
			SkipReason: fmt.Sprintf("extension %q not in accepted set", ext),
		}, ErrUnsupportedExt
	}

	// At this point the record is accepted. In production this would call
	// the S3 GetObject API or enqueue the key for downstream processing.
	p.logger.InfoContext(ctx, "processing object",
		"bucket", bucket, "key", decodedKey, "size_bytes", size, "event_name", eventName)

	return Result{
		Bucket:    bucket,
		Key:       decodedKey,
		Size:      size,
		EventName: eventName,
	}, nil
}

func (p *Processor) acceptsExt(ext string) bool {
	for _, e := range p.extensions {
		if e == ext {
			return true
		}
	}
	return false
}
```

`Handle` accumulates all results and returns the first non-skip error. Skip errors (`ErrUnsupportedType`, `ErrUnsupportedExt`) are informational — they do not cause Lambda retries in a real deployment.

### Exercise 2: Test the Contract

Create `processor_test.go`:

```go
package s3processor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

// nullLogger discards log output during tests so test output stays clean.
var nullLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

func newTestProcessor(extensions ...string) *Processor {
	return New(extensions, nullLogger)
}

func makeRecord(eventName, bucket, key string, size int64) events.S3EventRecord {
	return events.S3EventRecord{
		EventName: eventName,
		S3: events.S3Entity{
			Bucket: events.S3Bucket{Name: bucket},
			Object: events.S3Object{Key: key, Size: size},
		},
	}
}

func TestHandleSingleCSVPut(t *testing.T) {
	t.Parallel()

	p := newTestProcessor(".csv")
	event := events.S3Event{
		Records: []events.S3EventRecord{
			makeRecord("ObjectCreated:Put", "data-bucket", "uploads/report.csv", 2048),
		},
	}

	results, err := p.Handle(context.Background(), event)
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	r := results[0]
	if r.Bucket != "data-bucket" {
		t.Errorf("Bucket = %q, want %q", r.Bucket, "data-bucket")
	}
	if r.Key != "uploads/report.csv" {
		t.Errorf("Key = %q, want %q", r.Key, "uploads/report.csv")
	}
	if r.Size != 2048 {
		t.Errorf("Size = %d, want 2048", r.Size)
	}
	if r.Skipped {
		t.Errorf("Skipped = true, want false")
	}
}

func TestHandleURLEncodedKey(t *testing.T) {
	t.Parallel()

	// S3 encodes "q1 report.csv" as "q1+report.csv" in the event payload.
	p := newTestProcessor(".csv")
	event := events.S3Event{
		Records: []events.S3EventRecord{
			makeRecord("ObjectCreated:Put", "data-bucket", "reports/q1+report.csv", 512),
		},
	}

	results, err := p.Handle(context.Background(), event)
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if got, want := results[0].Key, "reports/q1 report.csv"; got != want {
		t.Errorf("decoded key = %q, want %q", got, want)
	}
}

func TestHandleURLEncodedKeyWithPercent(t *testing.T) {
	t.Parallel()

	// S3 percent-encodes special characters.
	// "data/2024%2F01%2Freport.csv" decodes to "data/2024/01/report.csv".
	p := newTestProcessor(".csv")
	event := events.S3Event{
		Records: []events.S3EventRecord{
			makeRecord("ObjectCreated:Put", "bucket", "data/2024%2F01%2Freport.csv", 100),
		},
	}

	results, err := p.Handle(context.Background(), event)
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if got, want := results[0].Key, "data/2024/01/report.csv"; got != want {
		t.Errorf("decoded key = %q, want %q", got, want)
	}
}

func TestHandleSkipsNonCSV(t *testing.T) {
	t.Parallel()

	p := newTestProcessor(".csv")
	event := events.S3Event{
		Records: []events.S3EventRecord{
			makeRecord("ObjectCreated:Put", "bucket", "upload.json", 64),
		},
	}

	results, err := p.Handle(context.Background(), event)
	// ErrUnsupportedExt is a skip, not a batch error.
	if err != nil {
		t.Fatalf("Handle() error = %v, want nil (skips are not errors)", err)
	}
	if !results[0].Skipped {
		t.Errorf("Skipped = false, want true for non-CSV file")
	}
}

func TestHandleSkipsNonPutEvent(t *testing.T) {
	t.Parallel()

	p := newTestProcessor(".csv")
	event := events.S3Event{
		Records: []events.S3EventRecord{
			// Delete event — should be skipped.
			makeRecord("ObjectRemoved:Delete", "bucket", "file.csv", 0),
		},
	}

	results, err := p.Handle(context.Background(), event)
	if err != nil {
		t.Fatalf("Handle() error = %v, want nil (skips are not errors)", err)
	}
	if !results[0].Skipped {
		t.Errorf("Skipped = false, want true for delete event")
	}
}

func TestHandleMultipleRecords(t *testing.T) {
	t.Parallel()

	p := newTestProcessor(".csv")
	event := events.S3Event{
		Records: []events.S3EventRecord{
			makeRecord("ObjectCreated:Put", "bucket", "a.csv", 100),
			makeRecord("ObjectCreated:Put", "bucket", "b.json", 200), // skipped
			makeRecord("ObjectCreated:Put", "bucket", "c.csv", 300),
		},
	}

	results, err := p.Handle(context.Background(), event)
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3", len(results))
	}
	if results[0].Skipped || results[2].Skipped {
		t.Errorf("CSV records should not be skipped")
	}
	if !results[1].Skipped {
		t.Errorf("JSON record should be skipped")
	}
}

func TestHandleMalformedKey(t *testing.T) {
	t.Parallel()

	p := newTestProcessor(".csv")
	// "%ZZ" is not valid percent-encoding; url.QueryUnescape returns an error.
	event := events.S3Event{
		Records: []events.S3EventRecord{
			makeRecord("ObjectCreated:Put", "bucket", "bad%ZZkey.csv", 0),
		},
	}

	_, err := p.Handle(context.Background(), event)
	if !errors.Is(err, ErrMalformedKey) {
		t.Errorf("err = %v, want ErrMalformedKey", err)
	}
}

func TestHandleAllExtensionsAcceptedWhenNoneConfigured(t *testing.T) {
	t.Parallel()

	// No extension filter — all extensions pass through.
	p := newTestProcessor()
	event := events.S3Event{
		Records: []events.S3EventRecord{
			makeRecord("ObjectCreated:Put", "bucket", "archive.tar.gz", 9999),
		},
	}

	results, err := p.Handle(context.Background(), event)
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if results[0].Skipped {
		t.Errorf("record should not be skipped when no extension filter is set")
	}
}

func TestHandleObjectCreatedPost(t *testing.T) {
	t.Parallel()

	// ObjectCreated:Post is also a creation event and should be processed.
	p := newTestProcessor(".csv")
	event := events.S3Event{
		Records: []events.S3EventRecord{
			makeRecord("ObjectCreated:Post", "bucket", "form-upload.csv", 512),
		},
	}

	results, err := p.Handle(context.Background(), event)
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if results[0].Skipped {
		t.Errorf("ObjectCreated:Post should not be skipped")
	}
}

func ExampleProcessor_Handle() {
	p := New([]string{".csv"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	event := events.S3Event{
		Records: []events.S3EventRecord{
			{
				EventName: "ObjectCreated:Put",
				S3: events.S3Entity{
					Bucket: events.S3Bucket{Name: "my-bucket"},
					Object: events.S3Object{Key: "data/report.csv", Size: 1024},
				},
			},
		},
	}
	results, err := p.Handle(context.Background(), event)
	if err != nil {
		panic(err)
	}
	fmt.Printf("processed=%d skipped=%d\n", countProcessed(results), countSkipped(results))
	// Output: processed=1 skipped=0
}

func countProcessed(rs []Result) int {
	n := 0
	for _, r := range rs {
		if !r.Skipped {
			n++
		}
	}
	return n
}

func countSkipped(rs []Result) int {
	n := 0
	for _, r := range rs {
		if r.Skipped {
			n++
		}
	}
	return n
}
```

The `ExampleProcessor_Handle` function uses `// Output:` and is auto-verified by `go test`. Your turn: add `TestHandleSkipsEventWithS3Prefix` that fabricates a record with `EventName: "s3:ObjectCreated:Put"` (the form used in bucket notification configs, not the event payload) and verifies it is skipped — this pins the contract that the `s3:` prefix does not appear in the payload.

### Exercise 3: The Demo Entry Point

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/aws/aws-lambda-go/events"

	"example.com/s3processor"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	p := s3processor.New([]string{".csv", ".json"}, logger)

	// Simulate an S3 batch with mixed event types and extensions.
	event := events.S3Event{
		Records: []events.S3EventRecord{
			{
				EventName: "ObjectCreated:Put",
				S3: events.S3Entity{
					Bucket: events.S3Bucket{Name: "ingestion-bucket"},
					Object: events.S3Object{Key: "2024/q1+report.csv", Size: 153600},
				},
			},
			{
				EventName: "ObjectCreated:Put",
				S3: events.S3Entity{
					Bucket: events.S3Bucket{Name: "ingestion-bucket"},
					Object: events.S3Object{Key: "2024/config.yaml", Size: 512},
				},
			},
			{
				EventName: "ObjectRemoved:Delete",
				S3: events.S3Entity{
					Bucket: events.S3Bucket{Name: "ingestion-bucket"},
					Object: events.S3Object{Key: "2024/old.csv", Size: 0},
				},
			},
			{
				EventName: "ObjectCreated:Put",
				S3: events.S3Entity{
					Bucket: events.S3Bucket{Name: "ingestion-bucket"},
					Object: events.S3Object{Key: "2024%2Fmetadata.json", Size: 2048},
				},
			},
		},
	}

	results, err := p.Handle(context.Background(), event)

	fmt.Printf("\n--- Results (%d records) ---\n", len(results))
	for i, r := range results {
		if r.Skipped {
			fmt.Printf("[%d] SKIP  key=%q reason=%s\n", i, r.Key, r.SkipReason)
		} else {
			fmt.Printf("[%d] OK    key=%q size=%d event=%s\n", i, r.Key, r.Size, r.EventName)
		}
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "batch error: %v\n", err)
		os.Exit(1)
	}
}
```

Run it with:

```bash
go run ./cmd/demo
```

The output shows which records were processed, which were skipped, and why.

## Common Mistakes

### Using `url.PathUnescape` Instead of `url.QueryUnescape`

Wrong:

```go
decoded, err := url.PathUnescape(record.S3.Object.Key)
```

What happens: `url.PathUnescape` does not convert `+` to space. A key like `reports/q1+summary.csv` decodes to `reports/q1+summary.csv` — the `+` is left as a literal plus sign. The downstream code sees a file with a `+` in its name, not a space.

Fix:

```go
decoded, err := url.QueryUnescape(record.S3.Object.Key)
```

`url.QueryUnescape` implements `application/x-www-form-urlencoded` decoding, which is what S3 uses.

### Comparing EventName With the `s3:` Prefix

Wrong:

```go
if rec.EventName == "s3:ObjectCreated:Put" { ... }
```

What happens: the condition never matches. In the event payload, `EventName` is `"ObjectCreated:Put"` — the `s3:` prefix appears only in the bucket notification configuration in the AWS console, not in the delivered JSON.

Fix:

```go
if rec.EventName == "ObjectCreated:Put" { ... }
```

### Comparing filepath.Ext Result Without the Leading Dot

Wrong:

```go
if filepath.Ext(key) == "csv" { ... }
```

What happens: `filepath.Ext` returns `".csv"` (with the leading dot). The comparison always fails and all records are skipped.

Fix:

```go
if filepath.Ext(key) == ".csv" { ... }
```

### Aborting the Batch on a Skippable Record

Wrong:

```go
for _, rec := range event.Records {
	if err := process(rec); err != nil {
		return err // stops processing remaining records
	}
}
```

What happens: if one record has an unsupported extension, all subsequent records in the batch are silently skipped.

Fix: log-and-continue for filter rejections; only propagate errors that are genuinely transient and worth retrying:

```go
for _, rec := range event.Records {
	if err := process(rec); err != nil {
		if errors.Is(err, ErrUnsupportedExt) || errors.Is(err, ErrUnsupportedType) {
			continue
		}
		return err
	}
}
```

### Using `strings.HasSuffix` Instead of `filepath.Ext`

Wrong:

```go
if strings.HasSuffix(key, "csv") { ... }
```

What happens: `file.csvbackup` matches but should not. A file named `.csv` (extension only) also matches incorrectly.

Fix: use `filepath.Ext`, which extracts only the final suffix starting at the last dot in the base name:

```go
if filepath.Ext(key) == ".csv" { ... }
```

## Verification

The tests in `processor_test.go` are the verification — there is no eyeballed `main`. From `~/go-exercises/s3processor`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. To run the demo:

```bash
go run ./cmd/demo
```

## Summary

- `events.S3Event.Records` is a slice; always iterate over all records and process them independently.
- Object keys are `application/x-www-form-urlencoded`: spaces become `+`. Use `url.QueryUnescape`, not `url.PathUnescape`.
- `EventName` in the payload is `"ObjectCreated:Put"`, not `"s3:ObjectCreated:Put"`.
- `filepath.Ext` returns the extension with the leading dot (`.csv`).
- Skip records (wrong extension, wrong event type) are not batch errors; only transient processing failures should be propagated.
- Tests fabricate `events.S3Event` structs directly — no AWS credentials, no network, no mocks required.

## What's Next

Next: [Kubernetes client-go](../06-kubernetes-client-go/06-kubernetes-client-go.md).

## Resources

- [events.S3Event — pkg.go.dev](https://pkg.go.dev/github.com/aws/aws-lambda-go/events#S3Event)
- [Amazon S3 Event Notification message structure](https://docs.aws.amazon.com/AmazonS3/latest/userguide/notification-content-structure.html)
- [url.QueryUnescape — pkg.go.dev](https://pkg.go.dev/net/url#QueryUnescape)
- [Lambda with S3 — AWS documentation](https://docs.aws.amazon.com/lambda/latest/dg/with-s3.html)
- [path/filepath.Ext — pkg.go.dev](https://pkg.go.dev/path/filepath#Ext)
