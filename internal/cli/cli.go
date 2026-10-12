// Package cli implements the mcp-warden command line. Each command is a
// function of (args, stdin, stdout, stderr) returning an exit code, so the
// whole surface is testable without spawning the binary.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// Version is overridden at build time with -ldflags "-X ...cli.Version=v1.2.3".
var Version = "dev"

// Env is the process environment a command runs in.
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

type command struct {
	name    string
	summary string
	run     func(env Env, args []string) int
}

func commands() []command {
	return []command{
		{"run", "Run an MCP server under supervision (use in your client config)", runCmd},
		{"status", "Show supervised instances and the latest scan summary", statusCmd},
		{"logs", "Print or follow an instance's log", logsCmd},
		{"reap", "Kill process trees left behind by dead mcp-warden instances", reapCmd},
		{"scan", "Find MCP server processes on this machine and flag orphans", scanCmd},
		{"watch", "Scan continuously and record changes to the history log", watchCmd},
		{"history", "Summarize which MCP servers have been leaking", historyCmd},
		{"version", "Print the version", versionCmd},
	}
}

// Main dispatches to a subcommand and returns the process exit code.
func Main(args []string, env Env) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		usage(env.Stderr)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	for _, c := range commands() {
		if c.name == args[0] {
			return c.run(env, args[1:])
		}
	}
	fmt.Fprintf(env.Stderr, "mcp-warden: unknown command %q\n\n", args[0])
	usage(env.Stderr)
	return 2
}

func usage(w io.Writer) {
	fmt.Fprint(w, `mcp-warden supervises local MCP servers and finds the ones nobody is supervising.

Usage:
  mcp-warden <command> [flags]

Commands:
`)
	for _, c := range commands() {
		fmt.Fprintf(w, "  %-8s %s\n", c.name, c.summary)
	}
	fmt.Fprint(w, `
Run "mcp-warden <command> -h" for a command's flags.

Example client config entry:
  "github": {
    "command": "mcp-warden",
    "args": ["run", "--name", "github", "--", "npx", "-y", "@modelcontextprotocol/server-github"]
  }
`)
}

func versionCmd(env Env, _ []string) int {
	fmt.Fprintln(env.Stdout, "mcp-warden", Version)
	return 0
}

// newFlags returns a FlagSet that reports errors instead of exiting.
func newFlags(env Env, name, usageLine string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(env.Stderr, "Usage: mcp-warden %s %s\n\nFlags:\n", name, usageLine)
		fs.PrintDefaults()
	}
	return fs
}

// parse handles -h and flag errors uniformly. ok=false means return code.
func parse(fs *flag.FlagSet, args []string) (code int, ok bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, false
		}
		return 2, false
	}
	return 0, true
}

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func fail(env Env, format string, a ...any) int {
	fmt.Fprintf(env.Stderr, "mcp-warden: "+format+"\n", a...)
	return 1
}

// stateDirFlag registers the shared --state-dir flag.
func stateDirFlag(fs *flag.FlagSet) *string {
	return fs.String("state-dir", "", "state directory (default: platform state dir, or $MCP_WARDEN_STATE_DIR)")
}

func resolveDir(flagVal string, def func() string) string {
	if flagVal != "" {
		return flagVal
	}
	return def()
}

// isTerminal is a cheap check used to decide whether to prompt.
func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
