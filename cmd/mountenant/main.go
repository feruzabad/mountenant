// Command mountenant is the composition root: it parses the subcommand, loads
// configuration and wires adapters to the application (spec §8.4).
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
)

// version is set at build time with -ldflags "-X main.version=v1.2.3".
var version = ""

const usage = `Usage: mountenant <command>

Commands:
  serve            run the server (applies migrations first)
  migrate          apply database migrations and exit
  hash-password    read a password from stdin and print its argon2id hash
  config validate  validate the configuration and users files
  version          print the version

Configuration is read from MOUNTENANT_CONFIG (default /etc/mountenant/config.json),
MOUNTENANT_USERS and MOUNTENANT_* environment variables; see configs/README.md.
`

// env is everything a command may touch, so tests can run commands in-process.
type env struct {
	args    []string
	environ []string
	stdin   io.Reader
	stdout  io.Writer
	stderr  io.Writer

	// listening, if set, receives the server's address once it accepts
	// connections (tests listen on port 0).
	listening func(addr string)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, env{
		args:    os.Args[1:],
		environ: os.Environ(),
		stdin:   os.Stdin,
		stdout:  os.Stdout,
		stderr:  os.Stderr,
	}))
}

// run executes one command and returns the process exit code.
func run(ctx context.Context, e env) int {
	if len(e.args) == 0 {
		fmt.Fprint(e.stderr, usage)
		return 2
	}
	var err error
	switch cmd, rest := e.args[0], e.args[1:]; cmd {
	case "serve":
		err = serve(ctx, e)
	case "migrate":
		err = migrateCmd(ctx, e)
	case "hash-password":
		err = hashPassword(e, rest)
	case "config":
		if len(rest) != 1 || rest[0] != "validate" {
			fmt.Fprint(e.stderr, usage)
			return 2
		}
		err = validateConfig(e)
	case "version":
		fmt.Fprintln(e.stdout, buildVersion())
	case "help", "-h", "-help", "--help":
		fmt.Fprint(e.stdout, usage)
	default:
		fmt.Fprintf(e.stderr, "unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	if err != nil {
		fmt.Fprintln(e.stderr, "mountenant:", err)
		return 1
	}
	return 0
}

func buildVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		v := bi.Main.Version
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 12 {
				v += " (" + s.Value[:12] + ")"
			}
		}
		return v
	}
	return "unknown"
}
