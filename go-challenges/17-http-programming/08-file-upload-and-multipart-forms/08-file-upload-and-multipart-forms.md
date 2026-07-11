# 8. File Upload and Multipart Forms

File uploads use `multipart/form-data` to send files and fields in one request. This lesson builds an `uploads` package that validates request size, parses multipart forms, detects file content type, sanitizes filenames, and serves saved files.

## Concepts

The `net/http` package provides `http.MaxBytesReader`, `(*http.Request).ParseMultipartForm`, `(*http.Request).FormFile`, `http.DetectContentType`, `http.FileServer`, and `http.StripPrefix`. `MaxBytesReader` limits the request body before parsing. `ParseMultipartForm` stores up to `maxMemory` bytes in memory and the rest in temporary files. `FormFile` returns a `multipart.File` and `*multipart.FileHeader` for a named upload field.

Never trust client filenames or `Content-Type` headers. Use `filepath.Base` to strip path components and `http.DetectContentType` on file bytes to validate content.

## Exercises

Create this module layout:

```text
file-uploads/
    go.mod
    uploads.go
    uploads_example_test.go
    uploads_test.go
    cmd/demo/main.go
```

Create `go.mod`:

```go
module example.com/file-uploads

go 1.26
```

Create `uploads.go`:

```go
package uploads

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

var (
	ErrMissingFile     = errors.New("missing file")
	ErrUnsupportedType = errors.New("unsupported content type")
	ErrSaveFailed      = errors.New("save failed")
)

type SavedFile struct {
	Name        string
	ContentType string
	Bytes       int64
}

type Service struct {
	Dir          string
	MaxBodyBytes int64
	MaxMemory    int64
	AllowedTypes map[string]bool
}

func NewService(dir string) *Service {
	return &Service{
		Dir:          dir,
		MaxBodyBytes: 10 << 20,
		MaxMemory:    10 << 20,
		AllowedTypes: map[string]bool{
			"application/pdf":           true,
			"image/jpeg":                true,
			"image/png":                 true,
			"text/plain; charset=utf-8": true,
			"application/octet-stream":  false,
		},
	}
}

func (s *Service) UploadHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		file, err := s.SaveSingle(w, r, "document")
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, ErrMissingFile) || errors.Is(err, ErrUnsupportedType) {
				status = http.StatusBadRequest
			}
			http.Error(w, err.Error(), status)
			return
		}
		fmt.Fprintf(w, "uploaded %s %s %d\n", file.Name, file.ContentType, file.Bytes)
	}
}

func (s *Service) SaveSingle(w http.ResponseWriter, r *http.Request, field string) (SavedFile, error) {
	r.Body = http.MaxBytesReader(w, r.Body, s.MaxBodyBytes)
	if err := r.ParseMultipartForm(s.MaxMemory); err != nil {
		return SavedFile{}, fmt.Errorf("%w: parse multipart form: %v", ErrMissingFile, err)
	}

	file, header, err := r.FormFile(field)
	if err != nil {
		return SavedFile{}, fmt.Errorf("%w: %s", ErrMissingFile, field)
	}
	defer file.Close()

	contentType, err := detect(file)
	if err != nil {
		return SavedFile{}, fmt.Errorf("%w: detect content: %v", ErrSaveFailed, err)
	}
	if !s.AllowedTypes[contentType] {
		return SavedFile{}, fmt.Errorf("%w: %s", ErrUnsupportedType, contentType)
	}

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return SavedFile{}, fmt.Errorf("%w: rewind file: %v", ErrSaveFailed, err)
	}

	name := filepath.Base(header.Filename)
	path := filepath.Join(s.Dir, name)
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return SavedFile{}, fmt.Errorf("%w: create upload dir: %v", ErrSaveFailed, err)
	}
	dst, err := os.Create(path)
	if err != nil {
		return SavedFile{}, fmt.Errorf("%w: create file: %v", ErrSaveFailed, err)
	}
	defer dst.Close()

	n, err := io.Copy(dst, file)
	if err != nil {
		return SavedFile{}, fmt.Errorf("%w: copy file: %v", ErrSaveFailed, err)
	}
	return SavedFile{Name: name, ContentType: contentType, Bytes: n}, nil
}

func (s *Service) FileServer(prefix string) http.Handler {
	return http.StripPrefix(prefix, http.FileServer(http.Dir(s.Dir)))
}

func detect(file io.Reader) (string, error) {
	buf := make([]byte, 512)
	n, err := file.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return http.DetectContentType(buf[:n]), nil
}
```

Create `uploads_example_test.go`:

```go
package uploads_test

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"

	"example.com/file-uploads"
)

func ExampleService_SaveSingle() {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("document", "note.txt")
	part.Write([]byte("hello"))
	writer.Close()

	service := uploads.NewService("example-uploads")
	req := httptest.NewRequest(http.MethodPost, "/upload", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	file, err := service.SaveSingle(httptest.NewRecorder(), req, "document")
	fmt.Println(file.Name, file.Bytes, err == nil)

	// Output:
	// note.txt 5 true
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"log"
	"net/http"

	"example.com/file-uploads"
)

func main() {
	service := uploads.NewService("uploads")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /upload", service.UploadHandler())
	mux.Handle("GET /files/", service.FileServer("/files/"))

	log.Fatal(http.ListenAndServe(":8080", mux))
}
```

Create `uploads_test.go`:

```go
package uploads

import (
	"bytes"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveSingleValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		filename  string
		contents  string
		fieldName string
		wantErr   error
	}{
		{name: "missing field", filename: "note.txt", contents: "hello", fieldName: "wrong", wantErr: ErrMissingFile},
		{name: "unsupported", filename: "page.html", contents: "<html></html>", fieldName: "document", wantErr: ErrUnsupportedType},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := multipartRequest(t, "document", tt.filename, tt.contents)
			_, err := NewService(t.TempDir()).SaveSingle(httptest.NewRecorder(), req, tt.fieldName)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("expected %v, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestSaveSingleSanitizesFilename(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	req := multipartRequest(t, "document", "../../note.txt", "hello")
	saved, err := NewService(dir).SaveSingle(httptest.NewRecorder(), req, "document")
	if err != nil {
		t.Fatalf("SaveSingle returned error: %v", err)
	}
	if saved.Name != "note.txt" {
		t.Fatalf("name = %q", saved.Name)
	}
	if _, err := os.Stat(filepath.Join(dir, "note.txt")); err != nil {
		t.Fatalf("saved file missing: %v", err)
	}
}

func TestUploadHandler(t *testing.T) {
	t.Parallel()

	req := multipartRequest(t, "document", "note.txt", "hello")
	rec := httptest.NewRecorder()
	NewService(t.TempDir()).UploadHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "uploaded note.txt") {
		t.Fatalf("response = %d %q", rec.Code, rec.Body.String())
	}
}

func multipartRequest(t *testing.T, field, filename, contents string) *http.Request {
	t.Helper()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile(field, filename)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part.Write([]byte(contents)); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/upload", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req
}
```

## Common Mistakes

- Calling `ParseMultipartForm` without first wrapping the body with `http.MaxBytesReader`.
- Saving `header.Filename` directly without `filepath.Base`.
- Trusting the client-supplied `Content-Type` header instead of `http.DetectContentType`.
- Forgetting to close uploaded files or destination files.
- Writing tests that depend on permanent local directories instead of `t.TempDir()`.

## Verification

Run these commands from the module root:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

## Summary

You built upload handlers around `http.MaxBytesReader`, `ParseMultipartForm`, `FormFile`, `DetectContentType`, sanitized filenames with `filepath.Base`, served files with `http.FileServer`, and tested handlers with `httptest`.

## What's Next

Next: [Server-Sent Events](../09-server-sent-events/09-server-sent-events.md).

## Resources

- [net/http MaxBytesReader](https://pkg.go.dev/net/http#MaxBytesReader)
- [net/http Request.ParseMultipartForm](https://pkg.go.dev/net/http#Request.ParseMultipartForm)
- [net/http Request.FormFile](https://pkg.go.dev/net/http#Request.FormFile)
- [net/http DetectContentType](https://pkg.go.dev/net/http#DetectContentType)
