package cli

import (
	"fmt"
	"os"
	"strings"

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
		victims := leftovers(idx, in)
		switch {
		case len(victims) == 0:
			fmt.Fprintf(env.Stdout, "%s: warden %d is gone and left nothing running\n", in.Name, in.WardenPID)
		case *dryRun:
			n := 0
			for _, v := range victims {
				n += 1 + len(idx.Descendants(v.PID))
			}
			fmt.Fprintf(env.Stdout, "%s: would kill %d process(es) left by warden %d (%s)\n",
				in.Name, n, in.WardenPID, truncate(strings.Join(in.Command, " "), 60))
			continue
		default:
			total := 0
			for _, v := range victims {
				killed, err := procscan.KillTree(idx, v)
				total += len(killed)
				if err != nil {
					fmt.Fprintf(env.Stderr, "  warning: pid %d: %v\n", v.PID, err)
				}
			}
			fmt.Fprintf(env.Stdout, "%s: killed %d process(es) left by warden %d\n", in.Name, total, in.WardenPID)
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

// leftovers returns the roots of everything a dead instance left behind:
// its server, if still running, plus (on Unix) any process still in the
// server's process group, such as a grandchild whose parent already exited.
// Nothing is returned for a PID that now belongs to a different process.
func leftovers(idx *procscan.Index, in state.Instance) []procscan.Process {
	if in.ChildPID <= 0 || in.ChildStartedAt.IsZero() {
		return nil
	}
	var roots []procscan.Process
	if child, ok := idx.ByPID[in.ChildPID]; ok {
		if !procscan.SameTime(child.CreateTime, in.ChildStartedAt) {
			return nil // PID reused: neither it nor a group with its number is ours
		}
		roots = append(roots, child)
	}
	covered := map[int]bool{}
	cover := func(p procscan.Process) {
		covered[p.PID] = true
		for _, d := range idx.Descendants(p.PID) {
			covered[d.PID] = true
		}
	}
	for _, r := range roots {
		cover(r)
	}
	for _, m := range idx.GroupMembers(in.ChildPID, in.ChildStartedAt) {
		if !covered[m.PID] {
			roots = append(roots, m)
			cover(m)
		}
	}
	return roots
}
