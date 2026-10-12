package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/anthonycoffey/mcp-warden/internal/state"
)

func logsCmd(env Env, args []string) int {
	fs := newFlags(env, "logs", "<name> [-f] [-n lines]")
	follow := fs.Bool("f", false, "follow the log as it grows")
	lines := fs.Int("n", 100, "number of trailing lines to print (0 = all)")
	dirFlag := stateDirFlag(fs)
	// Allow the name before or after flags.
	var name string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if name == "" && fs.NArg() > 0 {
		name = fs.Arg(0)
	}
	if err := state.ValidateName(name); err != nil {
		fs.Usage()
		return 2
	}
	dir := resolveDir(*dirFlag, state.DefaultDir)
	path := state.LogPath(dir, name)

	f, err := os.Open(path)
	if err != nil {
		return fail(env, "no log for %q (%v)", name, err)
	}
	defer f.Close()
	if err := tail(f, env.Stdout, *lines); err != nil {
		return fail(env, "%v", err)
	}
	if !*follow {
		return 0
	}
	return followFile(env, f, path)
}

// tail prints the last n lines of r (all if n <= 0).
func tail(r io.Reader, w io.Writer, n int) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 1<<20)
	var ring []string
	for sc.Scan() {
		if n > 0 && len(ring) == n {
			ring = ring[1:]
		}
		ring = append(ring, sc.Text())
	}
	for _, l := range ring {
		fmt.Fprintln(w, l)
	}
	return sc.Err()
}

// followFile polls for new data, reopening the file when it is rotated.
func followFile(env Env, f *os.File, path string) int {
	offset, _ := f.Seek(0, io.SeekEnd)
	for {
		time.Sleep(500 * time.Millisecond)
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if info.Size() < offset { // rotated
			f.Close()
			if f, err = os.Open(path); err != nil {
				continue
			}
			offset = 0
		}
		if info.Size() > offset {
			f.Seek(offset, io.SeekStart)
			n, _ := io.Copy(env.Stdout, f)
			offset += n
		}
	}
}
