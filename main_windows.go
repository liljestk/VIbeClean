package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/lxn/walk"
)

const appVersion = "1.3.0"

func main() {
	serve := flag.Bool("mcp", false, "serve MCP over standard input and output")
	indexPath := flag.String("index", "", "path to the local SQLite index")
	logLevel := flag.String("log-level", "info", "local diagnostics level: info, debug, or off")
	showVersion := flag.Bool("version", false, "print the Space Review version")
	flag.Parse()
	if *showVersion {
		fmt.Fprintln(os.Stdout, appVersion)
		return
	}

	if err := runApplication(*indexPath, *logLevel, *serve); err != nil {
		if *serve {
			fmt.Fprintln(os.Stderr, "Space Review:", err)
		} else {
			walk.MsgBox(nil, "Space Review", err.Error(), walk.MsgBoxIconError)
		}
		os.Exit(1)
	}
}

func runApplication(indexPath, logLevel string, serve bool) error {
	if logLevel != "info" && logLevel != "debug" && logLevel != "off" {
		return fmt.Errorf("choose info, debug, or off for --log-level")
	}
	started := time.Now()
	var path string
	var err error
	custom := indexPath != ""
	if custom {
		path, err = filepath.Abs(indexPath)
	} else {
		path, err = defaultIndexPath()
	}
	if err != nil {
		return err
	}
	if strings.HasPrefix(path, `\\`) {
		return fmt.Errorf("choose a local path for the SQLite index")
	}
	mode := "desktop"
	if serve {
		mode = "mcp"
	}
	ctx, diagnosticsDir, closeLog, logErr := openDiagnostics(context.Background(), path, mode, logLevel)
	defer closeLog()
	if logErr != nil && serve {
		fmt.Fprintln(os.Stderr, "Space Review: local diagnostics unavailable")
	}
	ctx = diagnosticOperation(ctx)
	defer logPanic(ctx, "application")
	logDiagnostic(ctx, slog.LevelInfo, "app.start", slog.String("log_level", logLevel),
		slog.String("go_version", runtime.Version()), slog.String("arch", runtime.GOARCH),
		slog.Bool("custom_index", custom))
	err = start(ctx, path, diagnosticsDir, serve, custom)
	logDuration(ctx, "app.exit", started, err)
	return err
}

func start(ctx context.Context, path, diagnosticsDir string, serve, custom bool) error {
	if !serve {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		return runGUI(ctx, path, diagnosticsDir, custom)
	}
	idx, err := openIndex(ctx, path)
	if err != nil {
		return fmt.Errorf("open the local index: %w", err)
	}
	defer idx.db.Close()
	return runMCP(ctx, idx)
}
