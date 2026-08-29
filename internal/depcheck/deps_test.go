// Package depcheck contains no code. It exists only to hold the test that
// enforces flakescope's zero-dependency rule.
//
// CLAUDE.md rule 1 says go.mod has no require block, ever. The whole install
// story ("go install, done") rests on that one claim, so it is enforced by a
// test rather than asserted in prose, where it would rot silently.
package depcheck

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// isExternalImport reports whether importPath names a package from outside both
// the standard library and the main module.
//
// The discriminator is a dot in the FIRST path segment. Standard library import
// paths are dotless in that position ("fmt", "os/exec", "internal/abi", and the
// stdlib's own vendored copies, which live under "vendor/..."). Module paths are
// domain-rooted, so their first segment is a hostname and always contains a dot.
// Packages belonging to the main module also start with a hostname, so they are
// excluded by prefix before the dot test runs.
func isExternalImport(modulePath, importPath string) bool {
	if importPath == modulePath || strings.HasPrefix(importPath, modulePath+"/") {
		return false
	}
	first, _, _ := strings.Cut(importPath, "/")
	return strings.Contains(first, ".")
}

func TestIsExternalImport(t *testing.T) {
	const module = "github.com/AarinB1/flakescope"

	tests := []struct {
		name       string
		importPath string
		want       bool
	}{
		{"stdlib single segment", "fmt", false},
		{"stdlib nested", "os/exec", false},
		{"stdlib internal", "internal/abi", false},
		{"stdlib vendored", "vendor/golang.org/x/net/dns/dnsmessage", false},
		{"unsafe", "unsafe", false},
		{"main module root", module, false},
		{"main module subpackage", module + "/internal/gotest", false},
		{"external module", "github.com/stretchr/testify/assert", true},
		{"external module root", "gopkg.in/yaml.v3", true},
		{"external single segment with dot", "example.com", true},
		// A module whose path merely CONTAINS the module path is not part of it.
		{"lookalike module", "github.com/AarinB1/flakescope-extras/x", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isExternalImport(module, tc.importPath); got != tc.want {
				t.Errorf("isExternalImport(%q, %q) = %v, want %v",
					module, tc.importPath, got, tc.want)
			}
		})
	}
}

// TestNoNonStdlibDependencies is the guard itself. It asks the go tool for the
// full transitive import graph of every package in the module and rejects any
// import path that is neither stdlib nor ours.
func TestNoNonStdlibDependencies(t *testing.T) {
	root := moduleRoot(t)
	modulePath := strings.TrimSpace(goList(t, root, "-m"))
	if modulePath == "" {
		t.Fatal("go list -m returned an empty module path")
	}

	// -test includes the imports of the test binaries as well as of the
	// packages themselves. A test-only dependency is still a require line.
	out := goList(t, root, "-deps", "-test", "./...")

	examined := 0
	var external []string
	for _, line := range strings.Split(out, "\n") {
		importPath := strings.TrimSpace(line)
		if importPath == "" {
			continue
		}
		examined++
		if isExternalImport(modulePath, importPath) {
			external = append(external, importPath)
		}
	}

	// Guard the guard. `go list` exiting 0 with nothing on stdout, or a future
	// refactor that stops matching any package, would otherwise leave this test
	// passing forever while checking nothing at all. The threshold is a floor,
	// not a measurement: the standard library alone puts any real package well
	// past it.
	const minPackages = 10
	if examined < minPackages {
		t.Fatalf("examined %d packages, want at least %d; the dependency guard is not actually looking at anything",
			examined, minPackages)
	}

	if len(external) > 0 {
		t.Errorf("flakescope must have zero non-stdlib dependencies, found %d:\n\t%s",
			len(external), strings.Join(external, "\n\t"))
	}
	t.Logf("examined %d packages, 0 external", examined)
}

// goList runs `go list` with the given arguments in dir and returns stdout.
func goList(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("go", append([]string{"list"}, args...)...)
	cmd.Dir = dir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list %s in %s: %v\n%s", strings.Join(args, " "), dir, err, stderr.String())
	}
	return string(out)
}

// moduleRoot walks up from the test's working directory to the directory
// holding go.mod. The test runs in internal/depcheck, where ./... would match
// only this package.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod found above %s", dir)
		}
		dir = parent
	}
}
