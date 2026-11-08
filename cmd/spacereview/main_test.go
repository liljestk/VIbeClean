package main

import (
	"io"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseModesAndValidation(t *testing.T) {
	opts, err := parse([]string{"--serve", "--index", "history.sqlite3"}, io.Discard)
	if err != nil || opts.mode != modeServe || !opts.custom || !filepath.IsAbs(opts.index) ||
		opts.listen != DefaultListen || opts.tokenFile != filepath.Join(filepath.Dir(opts.index), "api-token") {
		t.Fatalf("serve options: %+v, %v", opts, err)
	}
	opts, err = parse([]string{"--mcp", "--connect", "127.0.0.1:1"}, io.Discard)
	if err != nil || opts.mode != modeMCP || opts.location() != "Space Review API at 127.0.0.1:1" ||
		!slices.Equal(opts.mcpArgs(), []string{"--mcp", "--connect", "127.0.0.1:1"}) {
		t.Fatalf("remote MCP options: %+v, %v", opts, err)
	}
	opts, err = parse(nil, io.Discard)
	if err != nil || opts.mode != modeDesktop || opts.custom || !slices.Equal(opts.mcpArgs(), []string{"--mcp"}) {
		t.Fatalf("default options: %+v, %v", opts, err)
	}
	for _, args := range [][]string{
		{"--mcp", "--serve"},
		{"--serve", "--connect", "127.0.0.1:1"},
		{"--log-level", "loud"},
		{"--index", `\\server\share\index.sqlite3`},
		{"extra"},
	} {
		if _, err := parse(args, io.Discard); err == nil {
			t.Errorf("parse(%q) should fail", args)
		}
	}
}

// TestFrontEndsUseOnlyTheContract keeps the user interfaces decoupled: they
// may depend on the backend contract, never on its implementation.
func TestFrontEndsUseOnlyTheContract(t *testing.T) {
	const module = "github.com/liljestk/VIbeClean/"
	forbidden := []string{"internal/store", "internal/scan", "internal/docker", "internal/volume", "internal/service/local"}
	for _, front := range []string{"internal/desktop", "internal/mcpserver", "internal/rpc"} {
		output, err := exec.Command("go", "list", "-deps", module+front).CombinedOutput()
		if err != nil {
			t.Fatalf("go list %s: %v\n%s", front, err, output)
		}
		for _, dep := range strings.Fields(string(output)) {
			for _, banned := range forbidden {
				if dep == module+banned {
					t.Errorf("%s depends on %s; use the service.Backend contract instead", front, banned)
				}
			}
		}
	}
}
