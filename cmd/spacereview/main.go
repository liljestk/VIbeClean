// Command spacereview is Space Review in one Windows executable: the desktop
// app by default, an MCP server with --mcp, and a gRPC API server with
// --serve. With --connect, the desktop app and the MCP server use a running
// API server instead of opening the index themselves.
package main

//go:generate go tool -modfile=../../tools/go.mod rsrc -manifest app.manifest -arch amd64 -o rsrc_windows_amd64.syso

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/lxn/walk"

	"github.com/liljestk/VIbeClean/internal/buildinfo"
	"github.com/liljestk/VIbeClean/internal/desktop"
	"github.com/liljestk/VIbeClean/internal/diag"
	"github.com/liljestk/VIbeClean/internal/mcpserver"
	"github.com/liljestk/VIbeClean/internal/rpc"
	"github.com/liljestk/VIbeClean/internal/service"
	"github.com/liljestk/VIbeClean/internal/service/local"
	"github.com/liljestk/VIbeClean/internal/store"
)

// DefaultListen is the API address when --listen is not given.
const DefaultListen = "127.0.0.1:47820"

type mode int

const (
	modeDesktop mode = iota
	modeMCP
	modeServe
	modeVersion
)

func (m mode) String() string {
	return [...]string{"desktop", "mcp", "serve", "version"}[m]
}

// options are the parsed command line.
type options struct {
	mode      mode
	index     string
	custom    bool
	connect   string
	listen    string
	tokenFile string
	logLevel  string
}

func parse(args []string, output io.Writer) (options, error) {
	flags := flag.NewFlagSet("Space Review", flag.ContinueOnError)
	flags.SetOutput(output)
	serveMCP := flags.Bool("mcp", false, "serve MCP over standard input and output")
	serveAPI := flags.Bool("serve", false, "serve the gRPC API without opening a window")
	showVersion := flags.Bool("version", false, "print the Space Review version")
	opts := options{}
	flags.StringVar(&opts.index, "index", "", "path to the local SQLite index")
	flags.StringVar(&opts.connect, "connect", "", "use the API server at this loopback address instead of a local index")
	flags.StringVar(&opts.listen, "listen", DefaultListen, "loopback address for --serve")
	flags.StringVar(&opts.tokenFile, "token-file", "", "API token file; defaults to api-token beside the index")
	flags.StringVar(&opts.logLevel, "log-level", "info", "local diagnostics level: info, debug, or off")
	if err := flags.Parse(args); err != nil {
		return options{}, err
	}
	if flags.NArg() != 0 {
		return options{}, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	switch {
	case *showVersion:
		opts.mode = modeVersion
	case *serveMCP && *serveAPI:
		return options{}, errors.New("choose either --mcp or --serve")
	case *serveMCP:
		opts.mode = modeMCP
	case *serveAPI:
		opts.mode = modeServe
	}
	if opts.mode == modeServe && opts.connect != "" {
		return options{}, errors.New("--serve opens the index itself; remove --connect")
	}
	if opts.logLevel != "info" && opts.logLevel != "debug" && opts.logLevel != "off" {
		return options{}, errors.New("choose info, debug, or off for --log-level")
	}
	return opts, opts.resolveIndex()
}

func (o *options) resolveIndex() error {
	o.custom = o.index != ""
	var err error
	if o.custom {
		o.index, err = filepath.Abs(o.index)
	} else {
		o.index, err = store.DefaultPath()
	}
	if err != nil {
		return err
	}
	if strings.HasPrefix(o.index, `\\`) {
		return errors.New("choose a local path for the SQLite index")
	}
	if o.tokenFile == "" {
		o.tokenFile = rpc.DefaultTokenFile(o.index)
	} else if o.tokenFile, err = filepath.Abs(o.tokenFile); err != nil {
		return err
	}
	return nil
}

// open returns the backend: a local index, or a running API server with --connect.
func (o options) open(ctx context.Context) (service.Backend, error) {
	if o.connect != "" {
		token, err := rpc.ReadToken(o.tokenFile)
		if err != nil {
			return nil, fmt.Errorf("read the API token (start the server with --serve first): %w", err)
		}
		client, err := rpc.Dial(o.connect, token)
		if err != nil {
			return nil, err
		}
		return client, nil
	}
	backend, err := local.Open(ctx, o.index)
	if err != nil {
		return nil, fmt.Errorf("open the local index: %w", err)
	}
	return backend, nil
}

// mcpArgs are the arguments an MCP client uses to reach the same history.
func (o options) mcpArgs() []string {
	args := []string{"--mcp"}
	if o.connect != "" {
		args = append(args, "--connect", o.connect)
	} else if o.custom {
		args = append(args, "--index", o.index)
	}
	if o.connect != "" && o.tokenFile != rpc.DefaultTokenFile(o.index) {
		args = append(args, "--token-file", o.tokenFile)
	}
	return args
}

func (o options) location() string {
	if o.connect != "" {
		return "Space Review API at " + o.connect
	}
	return o.index
}

func main() {
	opts, err := parse(os.Args[1:], consoleOutput())
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err == nil {
		err = run(opts)
	}
	if err != nil {
		report(opts.mode, err)
		os.Exit(1)
	}
}

func report(m mode, err error) {
	if m == modeDesktop {
		walk.MsgBox(nil, "Space Review", err.Error(), walk.MsgBoxIconError)
		return
	}
	out := io.Writer(os.Stderr)
	if m != modeMCP {
		out = consoleOutput()
	}
	_, _ = fmt.Fprintln(out, "Space Review:", err)
}

func run(opts options) (err error) {
	if opts.mode == modeVersion {
		_, err = fmt.Fprintln(consoleOutput(), buildinfo.Version)
		return err
	}
	started := time.Now()
	ctx, diagnosticsDir, closeLog, logErr := diag.Open(context.Background(), opts.index, opts.mode.String(), opts.logLevel)
	defer closeLog()
	if logErr != nil && opts.mode == modeMCP {
		_, _ = fmt.Fprintln(os.Stderr, "Space Review: local diagnostics unavailable")
	}
	ctx = diag.Operation(ctx)
	defer diag.RecoverPanic(ctx, "application")
	diag.Log(ctx, slog.LevelInfo, "app.start", slog.String("go_version", runtime.Version()),
		slog.String("arch", runtime.GOARCH), slog.Bool("custom_index", opts.custom), slog.Bool("remote", opts.connect != ""))
	defer func() { diag.Duration(ctx, "app.exit", started, err) }()
	switch opts.mode {
	case modeMCP:
		return serveMCP(ctx, opts)
	case modeServe:
		return serveAPI(ctx, opts)
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	return desktop.Run(ctx, desktop.Options{
		Open: opts.open, Location: opts.location(), MCPCommand: executable, MCPArgs: opts.mcpArgs(),
		DiagnosticsDir: diagnosticsDir,
	})
}

func serveMCP(ctx context.Context, opts options) error {
	backend, err := opts.open(ctx)
	if err != nil {
		return err
	}
	defer backend.Close()
	return mcpserver.Run(ctx, backend)
}

func serveAPI(ctx context.Context, opts options) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	token, err := rpc.LoadOrCreateToken(opts.tokenFile)
	if err != nil {
		return fmt.Errorf("prepare the API token: %w", err)
	}
	backend, err := opts.open(ctx)
	if err != nil {
		return err
	}
	defer backend.Close()
	out := consoleOutput()
	return rpc.Serve(ctx, backend, opts.listen, token, func(address net.Addr) {
		diag.Log(ctx, slog.LevelInfo, "rpc.listening")
		_, _ = fmt.Fprintf(out, "Space Review API listening on %s\nToken file: %s\n", address, opts.tokenFile)
	})
}
