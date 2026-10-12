package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/anthonycoffey/mcp-warden/internal/classify"
	"github.com/anthonycoffey/mcp-warden/internal/history"
	"github.com/anthonycoffey/mcp-warden/internal/procscan"
	"github.com/anthonycoffey/mcp-warden/internal/state"
)

// scanner performs one scan and keeps the history log up to date. It is
// shared by `scan` (once) and `watch` (in a loop).
type scanner struct {
	dir    string
	policy classify.Config
	log    *history.Writer
	prev   map[string]classify.Tree
}

func newScanner(dir string, match, exclude []string) (*scanner, error) {
	policy, err := loadPolicy(dir, match, exclude)
	if err != nil {
		return nil, err
	}
	w, err := history.OpenWriter(dir)
	if err != nil {
		return nil, err
	}
	s := &scanner{dir: dir, policy: policy, log: w}
	if prev, err := history.LoadScan(dir); err == nil {
		s.prev = history.Index(prev.Result.Trees)
	}
	return s, nil
}

func (s *scanner) Close() error { return s.log.Close() }

// scan snapshots and classifies the machine, records changes since the
// previous scan, and saves the new snapshot.
func (s *scanner) scan() (history.Scan, []history.Event, error) {
	procs, err := procscan.Snapshot()
	if err != nil {
		return history.Scan{}, nil, fmt.Errorf("snapshot processes: %w", err)
	}
	views, _ := loadInstances(s.dir)
	now := time.Now()
	res := classify.Classify(procs, liveChildPIDs(views), s.policy)
	cur := history.Scan{Time: now, Result: res}

	curIdx := history.Index(res.Trees)
	events := history.Diff(s.prev, curIdx, now)
	s.prev = curIdx
	if err := s.log.Append(events); err != nil {
		return cur, events, err
	}
	return cur, events, history.SaveScan(s.dir, cur)
}

// kill terminates the given orphaned trees (re-verifying identity against a
// fresh snapshot) and records Killed events. Trees that are no longer
// orphans, or whose root PID was reused, are skipped.
func (s *scanner) kill(cur *history.Scan, keys map[string]bool) (killed []history.Event, errs []error) {
	procs, err := procscan.Snapshot()
	if err != nil {
		return nil, []error{err}
	}
	idx := procscan.NewIndex(procs)
	now := time.Now()
	var keep []classify.Tree
	for _, t := range cur.Result.Trees {
		if !keys[t.Key] || t.Class != classify.Orphaned {
			keep = append(keep, t)
			continue
		}
		root, ok := idx.ByPID[t.Root.PID]
		if !ok || !procscan.SameTime(root.CreateTime, t.Root.CreateTime) {
			continue // already gone
		}
		if _, err := procscan.KillTree(idx, root); err != nil {
			errs = append(errs, fmt.Errorf("pid %d: %w", root.PID, err))
		}
		killed = append(killed, history.NewEvent(history.Killed, t, now))
		delete(s.prev, t.Key) // don't also report it as disappeared next time
	}
	cur.Result.Trees = keep
	if err := s.log.Append(killed); err != nil {
		errs = append(errs, err)
	}
	if err := history.SaveScan(s.dir, *cur); err != nil {
		errs = append(errs, err)
	}
	return killed, errs
}

// ---------------------------------------------------------------------------
// scan

func scanCmd(env Env, args []string) int {
	fs := newFlags(env, "scan", "[--json] [--all] [--kill [--yes]] [--match re] [--exclude re]")
	asJSON := fs.Bool("json", false, "output JSON")
	all := fs.Bool("all", false, "include managed trees in the table")
	doKill := fs.Bool("kill", false, "kill orphaned trees (asks for confirmation)")
	yes := fs.Bool("yes", false, "with --kill: do not ask for confirmation")
	var match, exclude, pids stringList
	fs.Var(&match, "match", "extra regexp marking a command line as an MCP server (repeatable)")
	fs.Var(&exclude, "exclude", "regexp of command lines to ignore (repeatable)")
	fs.Var(&pids, "pid", "with --kill: only kill the orphaned tree rooted at this PID (repeatable)")
	dirFlag := stateDirFlag(fs)
	if code, ok := parse(fs, args); !ok {
		return code
	}
	dir := resolveDir(*dirFlag, state.DefaultDir)
	onlyPIDs := map[int]bool{}
	for _, p := range pids {
		n, err := strconv.Atoi(p)
		if err != nil {
			return fail(env, "--pid %q is not a number", p)
		}
		onlyPIDs[n] = true
	}

	sc, err := newScanner(dir, match, exclude)
	if err != nil {
		return fail(env, "%v", err)
	}
	defer sc.Close()

	cur, _, err := sc.scan()
	if err != nil {
		return fail(env, "%v", err)
	}
	if *asJSON && !*doKill {
		return writeJSON(env, cur)
	}
	if !*asJSON {
		printTrees(env, cur.Result, *all)
	}
	if !*doKill {
		return 0
	}

	orphans := map[string]bool{}
	for _, t := range cur.Result.Trees {
		if t.Class == classify.Orphaned && (len(onlyPIDs) == 0 || onlyPIDs[t.Root.PID]) {
			orphans[t.Key] = true
		}
	}
	if len(orphans) == 0 {
		fmt.Fprintln(env.Stdout, "\nNo orphaned trees to kill.")
		return 0
	}
	if !*yes {
		if !isTerminal(env.Stdin) {
			return fail(env, "refusing to kill without confirmation; pass --yes")
		}
		fmt.Fprintf(env.Stdout, "\nKill %d orphaned tree%s (%s)? [y/N] ", len(orphans), plural(len(orphans)),
			humanBytes(cur.Result.Memory(classify.Orphaned)))
		answer, _ := bufio.NewReader(env.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
			fmt.Fprintln(env.Stdout, "Nothing killed.")
			return 0
		}
	}
	killed, errs := sc.kill(&cur, orphans)
	for _, e := range killed {
		fmt.Fprintf(env.Stdout, "killed tree at pid %d: %d process(es), %s  %s\n",
			e.PID, e.Procs, humanBytes(e.RSS), truncate(e.Command, 60))
	}
	for _, err := range errs {
		fmt.Fprintf(env.Stderr, "warning: %v\n", err)
	}
	if len(errs) > 0 {
		return 1
	}
	return 0
}

func printTrees(env Env, res classify.Result, all bool) {
	now := time.Now()
	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CLASS\tPID\tPROCS\tMEM\tAGE\tOWNER\tCOMMAND")
	shown := 0
	for _, t := range res.Trees {
		if t.Class == classify.Managed && !all {
			continue
		}
		shown++
		fmt.Fprintf(tw, "%s\t%d\t%d\t%s\t%s\t%s\t%s\n",
			t.Class, t.Root.PID, len(t.Members), humanBytes(t.RSS), humanAge(now.Sub(t.Root.CreateTime)),
			truncate(t.Reason, 44), truncate(t.Root.Cmdline, 70))
	}
	if shown > 0 {
		tw.Flush()
		fmt.Fprintln(env.Stdout)
	}
	fmt.Fprintln(env.Stdout, scanLine(summarizeScan(history.Scan{Time: now, Result: res})))
	if n := res.Count(classify.Unmanaged); n > 0 {
		fmt.Fprintln(env.Stdout, "Tip: wrap unmanaged servers with `mcp-warden run` so they are supervised and cleaned up.")
	}
}

// ---------------------------------------------------------------------------
// watch

func watchCmd(env Env, args []string) int {
	fs := newFlags(env, "watch", "[--interval 30s] [--auto-reap --min-age 5m]")
	interval := fs.Duration("interval", 30*time.Second, "time between scans")
	autoReap := fs.Bool("auto-reap", false, "kill trees that stay orphaned for --min-age (off by default)")
	minAge := fs.Duration("min-age", 5*time.Minute, "how long a tree must stay orphaned before --auto-reap kills it")
	var match, exclude stringList
	fs.Var(&match, "match", "extra regexp marking a command line as an MCP server (repeatable)")
	fs.Var(&exclude, "exclude", "regexp of command lines to ignore (repeatable)")
	dirFlag := stateDirFlag(fs)
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if *interval < time.Second {
		return fail(env, "--interval must be at least 1s")
	}
	dir := resolveDir(*dirFlag, state.DefaultDir)
	sc, err := newScanner(dir, match, exclude)
	if err != nil {
		return fail(env, "%v", err)
	}
	defer sc.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(env.Stdout, "watching every %s (history: %s)", *interval, history.LogPath(dir))
	if *autoReap {
		fmt.Fprintf(env.Stdout, "; auto-reaping orphans older than %s", *minAge)
	}
	fmt.Fprintln(env.Stdout)

	orphanSince := map[string]time.Time{}
	tick := time.NewTicker(*interval)
	defer tick.Stop()
	for {
		cur, events, err := sc.scan()
		if err != nil {
			fmt.Fprintf(env.Stderr, "scan failed: %v\n", err)
		} else {
			for _, e := range events {
				printEvent(env, e)
			}
			if *autoReap {
				reapAged(env, sc, &cur, orphanSince, *minAge)
			}
		}
		select {
		case <-ctx.Done():
			return 0
		case <-tick.C:
		}
	}
}

// reapAged kills trees that have been continuously orphaned for minAge.
// Orphan age is tracked from first observation, so a client that restarts
// between two scans never loses its servers.
func reapAged(env Env, sc *scanner, cur *history.Scan, since map[string]time.Time, minAge time.Duration) {
	now := time.Now()
	seen := map[string]bool{}
	due := map[string]bool{}
	for _, t := range cur.Result.Trees {
		if t.Class != classify.Orphaned {
			continue
		}
		seen[t.Key] = true
		first, ok := since[t.Key]
		if !ok {
			since[t.Key] = now
			continue
		}
		if now.Sub(first) >= minAge {
			due[t.Key] = true
		}
	}
	for k := range since {
		if !seen[k] {
			delete(since, k)
		}
	}
	if len(due) == 0 {
		return
	}
	killed, errs := sc.kill(cur, due)
	for _, e := range killed {
		delete(since, e.Key)
		printEvent(env, e)
	}
	for _, err := range errs {
		fmt.Fprintf(env.Stderr, "auto-reap: %v\n", err)
	}
}

func printEvent(env Env, e history.Event) {
	fmt.Fprintf(env.Stdout, "%s  %-11s %-9s pid %-6d %2d proc  %7s  %s\n",
		e.Time.Format("15:04:05"), e.Type, e.Class, e.PID, e.Procs, humanBytes(e.RSS), truncate(e.Command, 70))
}

// ---------------------------------------------------------------------------
// history

func historyCmd(env Env, args []string) int {
	fs := newFlags(env, "history", "[--since 24h] [--top 10] [--json]")
	sinceStr := fs.String("since", "24h", "time range to summarize (e.g. 90m, 24h, 7d)")
	top := fs.Int("top", 10, "number of commands to list")
	asJSON := fs.Bool("json", false, "output JSON")
	dirFlag := stateDirFlag(fs)
	if code, ok := parse(fs, args); !ok {
		return code
	}
	d, err := parseSince(*sinceStr)
	if err != nil {
		return fail(env, "--since: %v", err)
	}
	dir := resolveDir(*dirFlag, state.DefaultDir)
	events, err := history.ReadEvents(dir)
	if err != nil {
		return fail(env, "%v", err)
	}
	s := history.Summarize(events, time.Now().Add(-d), *top)
	if *asJSON {
		return writeJSON(env, s)
	}

	fmt.Fprintf(env.Stdout, "Since %s: %d orphaned tree%s, %d killed, peak orphaned memory %s",
		s.Since.Format("2006-01-02 15:04"), s.Orphans, plural(s.Orphans), s.Killed, humanBytes(s.PeakOrphanedRSS))
	if !s.PeakAt.IsZero() {
		fmt.Fprintf(env.Stdout, " at %s", s.PeakAt.Local().Format("2006-01-02 15:04"))
	}
	fmt.Fprintln(env.Stdout)
	if s.Events == 0 {
		fmt.Fprintln(env.Stdout, "\nNo history yet. Run `mcp-warden scan` now and then, or keep `mcp-warden watch` running.")
		return 0
	}
	if len(s.Days) > 0 {
		fmt.Fprintln(env.Stdout, "\nOrphans per day:")
		for _, d := range s.Days {
			fmt.Fprintf(env.Stdout, "  %s  %3d  %s\n", d.Day, d.Orphans, strings.Repeat("█", min(d.Orphans, 50)))
		}
	}
	if len(s.TopCommands) > 0 {
		fmt.Fprintln(env.Stdout, "\nMost frequently orphaned:")
		tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  ORPHANS\tKILLED\tPEAK MEM\tCOMMAND")
		for _, c := range s.TopCommands {
			fmt.Fprintf(tw, "  %d\t%d\t%s\t%s\n", c.Orphans, c.Killed, humanBytes(c.PeakRSS), truncate(c.Command, 80))
		}
		tw.Flush()
	}
	return 0
}
