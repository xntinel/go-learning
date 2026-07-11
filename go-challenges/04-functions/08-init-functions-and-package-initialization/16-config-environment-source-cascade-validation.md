# Exercise 16: Config Cascade at init — Environment, File, Defaults, All Validated Together

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Real services rarely load configuration from one source. This exercise
builds a package that cascades three sources — hardcoded defaults, a config
file, and environment variables, in increasing precedence — and validates the
merged result at `init()`, reporting every problem at once with
`errors.Join` instead of crashing on the first missing key and forcing a
restart-and-retry loop.

## What you'll build

```text
configcascade/             independent module: example.com/configcascade
  go.mod                    module example.com/configcascade
  config.go                 defaults, embedded file content, cascade, validate, init()
  cmd/
    demo/
      main.go                prints the config loaded from the real process env + embedded file
  config_test.go             precedence table + joined-error test + invalid-port table
```

Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
Implement: `cascade(getenv, fileContent, defaults)` merging three sources by precedence (env > file > defaults); `validate(values) (*Config, error)` collecting every problem via `errors.Join`; `buildConfig` combining both, called from `init()` which panics on error.
Test: cascade precedence across all three sources; `errors.Join` surfaces every missing key from one `buildConfig` call; a table of invalid port values (non-numeric, zero, too large, negative) is each rejected.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/configcascade/cmd/demo
cd ~/go-exercises/configcascade
go mod init example.com/configcascade
go mod edit -go=1.24
```

### Why cascade-then-validate, and why at init

A config that only reads the environment cannot express "use this file's
value unless the environment overrides it, and if neither is set, fall back
to a sane default" — yet that three-tier cascade is exactly what most
services need: defaults ship with the binary, a file is deployed per
environment, and an environment variable lets an operator override a single
value at runtime without touching the file. Getting the precedence order
backwards (say, defaults overriding the file) silently ignores real
configuration, which is a far worse failure mode than a missing key, because
nothing about it looks wrong until the service behaves unexpectedly in
production.

Validating at `init()` — rather than deferring to whatever code path first
touches the config — means a misconfigured deployment fails at process
startup, before it can accept a single request with configuration it cannot
trust. `errors.Join` matters here specifically because a cascade has more
than one thing that can be wrong simultaneously (a missing name *and* an
invalid port), and reporting only the first forces an operator through a
fix-restart-fix-restart cycle to discover the second.

The cascade and validation logic lives in `buildConfig`, extracted from
`init()` exactly like `validatePatterns` was extracted in an earlier
exercise: `init()` calls it with the real `os.Getenv` and the module's
embedded file content, while tests call it directly with fake sources,
never touching the process environment.

Create `config.go`:

```go
// config.go
// Package configcascade loads service configuration by cascading three
// sources — environment variables, a config file, and hardcoded defaults —
// and validates the result at init, reporting every problem at once via
// errors.Join instead of failing on the first one found.
package configcascade

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Sentinel errors for each validation failure, so a caller can match a
// specific problem with errors.Is even though buildConfig joins them all.
var (
	ErrMissingName = errors.New("name is required")
	ErrMissingPort = errors.New("port is required")
	ErrInvalidPort = errors.New("port must be an integer between 1 and 65535")
)

// Config is the fully cascaded and validated configuration.
type Config struct {
	Name string
	Port int
}

// defaultValues is the lowest-precedence source: the values used when
// neither the config file nor the environment supplies a key.
var defaultValues = map[string]string{
	"name": "svcdefault",
	"port": "8080",
}

// defaultFileContent stands in for a config file's contents. It is embedded
// as a constant here so the module stays self-contained; in a real service
// this would be the result of os.ReadFile on a deployed config file.
const defaultFileContent = "name=svcfromfile\n"

// Cfg is the configuration loaded once at package init from the real
// process environment and the embedded file content.
var Cfg *Config

func init() {
	cfg, err := buildConfig(os.Getenv, defaultFileContent, defaultValues)
	if err != nil {
		panic(fmt.Errorf("configcascade: invalid configuration: %w", err))
	}
	Cfg = cfg
}

// parseFileContent parses a tiny "key=value" per line format. Blank lines
// are ignored.
func parseFileContent(content string) map[string]string {
	values := make(map[string]string)
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		values[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return values
}

// cascade merges defaults, then file content, then environment variables, in
// increasing precedence: env overrides file, file overrides defaults.
func cascade(getenv func(string) string, fileContent string, defaults map[string]string) map[string]string {
	values := make(map[string]string, len(defaults))
	for k, v := range defaults {
		values[k] = v
	}
	for k, v := range parseFileContent(fileContent) {
		values[k] = v
	}
	if v := getenv("SVC_NAME"); v != "" {
		values["name"] = v
	}
	if v := getenv("SVC_PORT"); v != "" {
		values["port"] = v
	}
	return values
}

// validate checks the cascaded values and builds a Config, collecting every
// problem instead of stopping at the first.
func validate(values map[string]string) (*Config, error) {
	var errs []error
	cfg := &Config{}

	name, ok := values["name"]
	if !ok || name == "" {
		errs = append(errs, ErrMissingName)
	} else {
		cfg.Name = name
	}

	portStr, ok := values["port"]
	switch {
	case !ok || portStr == "":
		errs = append(errs, ErrMissingPort)
	default:
		n, err := strconv.Atoi(portStr)
		if err != nil || n < 1 || n > 65535 {
			errs = append(errs, fmt.Errorf("%w: got %q", ErrInvalidPort, portStr))
		} else {
			cfg.Port = n
		}
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return cfg, nil
}

// buildConfig cascades then validates. It is extracted from init so a test
// can drive the whole pipeline with fake sources instead of the real
// process environment.
func buildConfig(getenv func(string) string, fileContent string, defaults map[string]string) (*Config, error) {
	return validate(cascade(getenv, fileContent, defaults))
}
```

### The runnable demo

The demo relies on the package's own `init()`, which already cascaded the
real process environment (empty, in a fresh shell) with the embedded file
content and the defaults. The file sets `name`, so it wins over the default;
nothing sets `port`, so the default `8080` is used.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/configcascade"
)

func main() {
	fmt.Printf("config loaded: name=%s port=%d\n", configcascade.Cfg.Name, configcascade.Cfg.Port)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
config loaded: name=svcfromfile port=8080
```

Run it again with `SVC_PORT=9090 SVC_NAME=svcfromenv go run ./cmd/demo` and
both values come from the environment instead — the highest-precedence
source wins, exactly as `cascade` implements it.

### Tests

Create `config_test.go`:

```go
// config_test.go
package configcascade

import (
	"errors"
	"testing"
)

// TestPackageLoaded proves init did not panic: if the shipped defaults and
// embedded file content were inconsistent, this test binary would never
// reach a test body.
func TestPackageLoaded(t *testing.T) {
	if Cfg == nil {
		t.Fatal("Cfg is nil: package init did not run or was skipped")
	}
	if Cfg.Name == "" || Cfg.Port == 0 {
		t.Fatalf("Cfg = %+v, want populated fields", Cfg)
	}
}

func fakeEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestCascadePrecedence(t *testing.T) {
	tests := []struct {
		name        string
		getenv      func(string) string
		fileContent string
		defaults    map[string]string
		wantName    string
		wantPort    int
	}{
		{
			name:        "defaults only",
			getenv:      fakeEnv(map[string]string{}),
			fileContent: "",
			defaults:    map[string]string{"name": "d", "port": "1000"},
			wantName:    "d",
			wantPort:    1000,
		},
		{
			name:        "file overrides defaults",
			getenv:      fakeEnv(map[string]string{}),
			fileContent: "name=fromfile\n",
			defaults:    map[string]string{"name": "d", "port": "1000"},
			wantName:    "fromfile",
			wantPort:    1000,
		},
		{
			name:        "env overrides file and defaults",
			getenv:      fakeEnv(map[string]string{"SVC_NAME": "fromenv", "SVC_PORT": "2000"}),
			fileContent: "name=fromfile\nport=1500\n",
			defaults:    map[string]string{"name": "d", "port": "1000"},
			wantName:    "fromenv",
			wantPort:    2000,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := buildConfig(tt.getenv, tt.fileContent, tt.defaults)
			if err != nil {
				t.Fatalf("buildConfig error = %v", err)
			}
			if cfg.Name != tt.wantName || cfg.Port != tt.wantPort {
				t.Fatalf("cfg = %+v, want name=%s port=%d", cfg, tt.wantName, tt.wantPort)
			}
		})
	}
}

func TestValidationJoinsAllFailures(t *testing.T) {
	_, err := buildConfig(fakeEnv(map[string]string{}), "", map[string]string{})
	if err == nil {
		t.Fatal("expected error for empty defaults, file, and env")
	}
	if !errors.Is(err, ErrMissingName) {
		t.Errorf("joined error missing ErrMissingName: %v", err)
	}
	if !errors.Is(err, ErrMissingPort) {
		t.Errorf("joined error missing ErrMissingPort: %v", err)
	}
}

func TestInvalidPortRejected(t *testing.T) {
	tests := []struct {
		name string
		port string
	}{
		{"not a number", "abc"},
		{"zero", "0"},
		{"too large", "70000"},
		{"negative", "-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := buildConfig(fakeEnv(map[string]string{}), "", map[string]string{
				"name": "svc",
				"port": tt.port,
			})
			if !errors.Is(err, ErrInvalidPort) {
				t.Fatalf("port %q: err = %v, want ErrInvalidPort", tt.port, err)
			}
		})
	}
}
```

## Review

`TestCascadePrecedence` is the table that matters most: it proves the three
sources merge in the right order in isolation from each other, defaults-only,
file-overriding-defaults, and env-overriding-everything, so a bug that
silently flips two of them would be caught immediately rather than months
later when someone wonders why a config file change had no effect.
`TestValidationJoinsAllFailures` proves the aggregation contract: both
sentinels come back from a single `buildConfig` call, recoverable
independently with `errors.Is`, which is the entire reason to prefer
`errors.Join` over returning only the first problem found.

The mistake to avoid is validating each source independently instead of
validating the *cascaded result*. A file that sets a valid port and an
environment override that sets an invalid one must still be caught — and it
is, because `validate` only ever looks at the merged `values` map, never at
any one source in isolation.

## Resources

- [errors.Join](https://pkg.go.dev/errors#Join) — aggregate multiple validation failures into one error that `errors.Is` can unwrap.
- [strconv.Atoi](https://pkg.go.dev/strconv#Atoi) — parses the cascaded port string, rejected on error or out-of-range value.
- [os.Getenv](https://pkg.go.dev/os#Getenv) — the default, highest-precedence source `init()` reads through the injected `getenv` seam.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [15-multi-package-init-ordering-with-dependencies.md](15-multi-package-init-ordering-with-dependencies.md) | Next: [17-connection-pool-lazy-oncevalue-per-dialect.md](17-connection-pool-lazy-oncevalue-per-dialect.md)
