package proxy

import (
	"errors"
	"io"
	"os"
	"os/exec"

	"github.com/anthonycoffey/mcp-warden/internal/procgroup"
)

// Child is one running generation of the MCP server. The proxy depends only
// on this interface; tests substitute an in-memory implementation.
type Child interface {
	Stdin() io.WriteCloser
	Stdout() io.Reader
	Stderr() io.Reader
	Pid() int
	// Wait blocks until the direct child process exits.
	Wait() error
	// Kill terminates the child and all of its descendants.
	Kill() error
	// Close releases resources. Called once, after Wait has returned.
	Close() error
}

// Spawner starts a new generation of the server.
type Spawner func() (Child, error)

// ExecSpawner returns a Spawner that runs argv under a procgroup, so every
// generation's whole process tree is owned and killable.
func ExecSpawner(argv []string, env []string) Spawner {
	return func() (Child, error) {
		if len(argv) == 0 {
			return nil, errors.New("no command given")
		}
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Env = env

		// Explicit os.Pipe pairs rather than cmd.StdoutPipe: exec.Cmd.Wait
		// closes its own pipes as soon as the process exits, which would race
		// the reader for output still buffered in the pipe. With *os.File
		// fields, exec hands the descriptors straight to the child and Wait
		// leaves our ends alone.
		inR, inW, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		outR, outW, err := os.Pipe()
		if err != nil {
			closeAll(inR, inW)
			return nil, err
		}
		errR, errW, err := os.Pipe()
		if err != nil {
			closeAll(inR, inW, outR, outW)
			return nil, err
		}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, errW

		g, err := procgroup.Start(cmd)
		// The child has its own copies now (or failed to start); ours must
		// be closed so EOF propagates when the child exits.
		closeAll(inR, outW, errW)
		if err != nil {
			closeAll(inW, outR, errR)
			return nil, err
		}
		return &execChild{cmd: cmd, group: g, stdin: inW, stdout: outR, stderr: errR}, nil
	}
}

type execChild struct {
	cmd    *exec.Cmd
	group  *procgroup.Group
	stdin  *os.File
	stdout *os.File
	stderr *os.File
}

func (c *execChild) Stdin() io.WriteCloser { return c.stdin }
func (c *execChild) Stdout() io.Reader     { return c.stdout }
func (c *execChild) Stderr() io.Reader     { return c.stderr }
func (c *execChild) Pid() int              { return c.cmd.Process.Pid }
func (c *execChild) Wait() error           { return c.cmd.Wait() }
func (c *execChild) Kill() error           { return c.group.Kill() }

// Degraded reports why process-tree ownership is weaker than intended, if it is.
func (c *execChild) Degraded() error { return c.group.Degraded }

func (c *execChild) Close() error {
	closeAll(c.stdout, c.stderr)
	return c.group.Close()
}

func closeAll(fs ...*os.File) {
	for _, f := range fs {
		f.Close()
	}
}
