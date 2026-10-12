package cli

import (
	"fmt"
	"os"

	"github.com/anthonycoffey/mcp-warden/internal/procscan"
	"github.com/anthonycoffey/mcp-warden/internal/state"
)

func reapCmd(env Env, args []string) int {
	fs := newFlags(env, "reap", "[--dry-run]")
	dryRun := fs.Bool("dry-run", false, "show what would be killed without killing")
	dirFlag := stateDirFlag(fs)
	if code, ok := parse(fs, args); !ok {
		return code
	}
	dir := resolveDir(*dirFlag, state.DefaultDir)
	entries, err := state.List(dir)
	if err != nil {
		return fail(env, "%v", err)
	}
	procs, err := procscan.Snapshot()
	if err != nil {
		return fail(env, "snapshot processes: %v", err)
	}
	idx := procscan.NewIndex(procs)

	stale := 0
	for _, e := range entries {
		in := e.Instance
		if procscan.Alive(in.WardenPID, in.WardenCreatedAt) {
			continue
		}
		stale++
		root, ok := idx.ByPID[in.ChildPID]
		alive := ok && in.ChildPID > 0 && !in.ChildStartedAt.IsZero() && procscan.SameTime(root.CreateTime, in.ChildStartedAt)
		switch {
		case !alive:
			fmt.Fprintf(env.Stdout, "%s: warden %d is gone and left nothing running\n", in.Name, in.WardenPID)
		case *dryRun:
			n := 1 + len(idx.Descendants(root.PID))
			fmt.Fprintf(env.Stdout, "%s: would kill %d process(es) rooted at pid %d (%s)\n", in.Name, n, root.PID, truncate(root.Cmdline, 60))
			continue
		default:
			killed, err := procscan.KillTree(idx, root)
			fmt.Fprintf(env.Stdout, "%s: killed %d process(es) rooted at pid %d\n", in.Name, len(killed), root.PID)
			if err != nil {
				fmt.Fprintf(env.Stderr, "  warning: %v\n", err)
			}
		}
		if !*dryRun {
			os.Remove(e.Path)
		}
	}
	if stale == 0 {
		fmt.Fprintln(env.Stdout, "Nothing to reap: every recorded mcp-warden instance is alive.")
	}
	return 0
}
