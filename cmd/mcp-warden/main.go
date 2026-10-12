// Command mcp-warden supervises local MCP (Model Context Protocol) servers and
// finds the ones nobody is supervising. See README.md for usage.
package main

import (
	"os"

	"github.com/anthonycoffey/mcp-warden/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], cli.Env{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}))
}
