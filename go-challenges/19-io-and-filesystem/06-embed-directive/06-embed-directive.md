# 6. The embed Directive

Build a library that serves embedded templates and default configuration files. The hard part is understanding that embedded assets are fixed at build time and exposed through normal values: `string`, `[]byte`, or `embed.FS`.

## Concepts

### Embed Runs At Compile Time

`//go:embed` is a compiler directive. The files must exist when the package is built, and changes on disk after compilation do not change the binary. This makes embedded defaults and templates reproducible.

### Embedded Files Can Be A Filesystem

An `embed.FS` implements `fs.FS`, so it works with `fs.ReadFile`, `template.ParseFS`, and code that accepts a filesystem interface. That keeps your code testable: production can use embedded files while tests can use `fstest.MapFS`.

### Patterns Are Package Relative

Embed patterns are relative to the source file containing the directive. They cannot use absolute paths or `..`. This protects builds from depending on arbitrary host paths.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/embeddedsite/cmd/demo ~/go-exercises/embeddedsite/assets/templates
cd ~/go-exercises/embeddedsite
go mod init example.com/embeddedsite
```

Create `assets/config.txt`:

```text
site_name=Example Site
```

Create `assets/templates/page.tmpl`:

```text
Hello, {{.Name}} from {{.Site}}.
```

### Exercise 1: Embed And Render Assets

Create `site.go`:

```go
package embeddedsite

import (
	"embed"
	"fmt"
	"io/fs"
	"strings"
	"text/template"
)

//go:embed assets/config.txt assets/templates/*.tmpl
var assets embed.FS

type PageData struct {
	Name string
	Site string
}

func Assets() fs.FS {
	return assets
}

func DefaultConfig() (string, error) {
	data, err := fs.ReadFile(assets, "assets/config.txt")
	if err != nil {
		return "", fmt.Errorf("read embedded config: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

func RenderPage(fsys fs.FS, data PageData) (string, error) {
	if strings.TrimSpace(data.Name) == "" {
		return "", fmt.Errorf("render page: %w", ErrEmptyName)
	}
	tpl, err := template.ParseFS(fsys, "assets/templates/page.tmpl")
	if err != nil {
		return "", fmt.Errorf("parse page template: %w", err)
	}
	var out strings.Builder
	if err := tpl.Execute(&out, data); err != nil {
		return "", fmt.Errorf("execute page template: %w", err)
	}
	return out.String(), nil
}
```

Create `errors.go`:

```go
package embeddedsite

import "errors"

var ErrEmptyName = errors.New("name must not be empty")
```

### Exercise 2: Test Embedded And Virtual Assets

Create `site_test.go`:

```go
package embeddedsite

import (
	"errors"
	"fmt"
	"testing"
	"testing/fstest"
)

func TestDefaultConfigReadsEmbeddedFile(t *testing.T) {
	t.Parallel()

	got, err := DefaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got != "site_name=Example Site" {
		t.Fatalf("config = %q", got)
	}
}

func TestRenderPageUsesProvidedFilesystem(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"assets/templates/page.tmpl": {Data: []byte("Hi {{.Name}} from {{.Site}}.")},
	}
	got, err := RenderPage(fsys, PageData{Name: "Ada", Site: "Tests"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "Hi Ada from Tests." {
		t.Fatalf("page = %q", got)
	}
}

func TestRenderPageRejectsEmptyName(t *testing.T) {
	t.Parallel()

	_, err := RenderPage(Assets(), PageData{Name: "  ", Site: "Example"})
	if !errors.Is(err, ErrEmptyName) {
		t.Fatalf("err = %v, want ErrEmptyName", err)
	}
}

func ExampleRenderPage() {
	page, _ := RenderPage(Assets(), PageData{Name: "Ada", Site: "Example Site"})
	fmt.Print(page)
	// Output: Hello, Ada from Example Site.
}
```

### Exercise 3: Add A Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/embeddedsite"
)

func main() {
	config, err := embeddedsite.DefaultConfig()
	if err != nil {
		log.Fatal(err)
	}
	page, err := embeddedsite.RenderPage(embeddedsite.Assets(), embeddedsite.PageData{Name: "Demo", Site: "Example Site"})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(config)
	fmt.Print(page)
}
```

## Common Mistakes

### Expecting Runtime File Changes To Appear

Wrong: edit `assets/config.txt` after building the binary and expect the old binary to read the new contents.

Fix: rebuild. Embedded data is captured at compile time.

### Hard-Coding embed.FS Everywhere

Wrong: make `RenderPage` close over the package-level `assets` value.

Fix: accept `fs.FS`; tests can use `fstest.MapFS`, and production can pass `Assets()`.

### Embedding With Absolute Paths

Wrong: `//go:embed /etc/app/config.txt`.

Fix: place assets under the package directory and use package-relative patterns.

## Verification

Run this from `~/go-exercises/embeddedsite`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

Add one more test that passes a filesystem without `assets/templates/page.tmpl` and checks that `RenderPage` returns an error.

## Summary

- `//go:embed` includes files in the binary at compile time.
- `embed.FS` implements `fs.FS`, so code can remain filesystem-agnostic.
- Embed patterns are relative to the package and cannot refer to arbitrary absolute paths.
- Accepting `fs.FS` keeps embedded-asset code easy to test.

## What's Next

Next: [Temporary Files and Directories](../07-temporary-files-directories/07-temporary-files-directories.md).

## Resources

- [embed package](https://pkg.go.dev/embed)
- [Package embed documentation](https://go.dev/doc/go1.16#library-embed)
- [io/fs package](https://pkg.go.dev/io/fs)
- [text/template ParseFS](https://pkg.go.dev/text/template#Template.ParseFS)
