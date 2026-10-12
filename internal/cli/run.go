package cli

import (
	"context"
	"errors"
	"fmt"
	iofs "io/fs"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/anthonycoffey/mcp-warden/internal/logfile"
	"github.com/anthonycoffey/mcp-warden/internal/metrics"
	"github.com/anthonycoffey/mcp-warden/internal/procscan"
	"github.com/anthonycoffey/mcp-warden/internal/proxy"
	"github.com/anthonycoffey/mcp-warden/internal/state"
	"github.com/anthonycoffey/mcp-warden/internal/supervisor"
)

func runCmd(env Env, args []string) int {
	fs := newFlags(env, "run", "--name <name> [flags] -- <command> [args...]")
	name := fs.String("name", "", "instance name, used for the state file and log (required)")
	dirFlag := stateDirFlag(fs)
	grace := fs.Duration("grace", 3*time.Second, "time to wait for the server to exit after stdin closes")
	maxRestarts := fs.Int("max-restarts", 5, "crashes allowed within --restart-window before giving up")
	window := fs.Duration("restart-window", time.Minute, "sliding window for --max-restarts")
	bufSize := fs.Int("buffer", 256, "client messages buffered while the server restarts")
	maxMsg := fs.Int("max-message", 16<<20, "maximum message size in bytes")
	initTimeout := fs.Duration("init-timeout", 10*time.Second, "time allowed for a restarted server to answer the replayed initialize")
	backoffBase := fs.Duration("backoff-base", 250*time.Millisecond, "first restart delay")
	backoffMax := fs.Duration("backoff-max", 30*time.Second, "maximum restart delay")
	verbose := fs.Bool("v", false, "verbose (debug) logging to stderr")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	argv := fs.Args()
	if len(argv) == 0 {
		fs.Usage()
		return 2
	}
	if err := state.ValidateName(*name); err != nil {
		return fail(env, "--name: %v", err)
	}
	dir := resolveDir(*dirFlag, state.DefaultDir)

	selfCreated, _ := procscan.CreateTime(os.Getpid())
	instName := uniqueName(dir, *name)

	lf, err := logfile.Open(state.LogPath(dir, instName), 0, 0)
	if err != nil {
		return fail(env, "open log: %v", err)
	}
	defer lf.Close()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	fileLog := slog.New(slog.NewJSONHandler(lf, &slog.HandlerOptions{Level: slog.LevelDebug})).With("instance", instName)
	log := slog.New(slog.NewMultiHandler(
		fileLog.Handler(),
		slog.NewTextHandler(env.Stderr, &slog.HandlerOptions{Level: level}).WithAttrs([]slog.Attr{slog.String("mcp-warden", instName)}),
	))
	if instName != *name {
		log.Warn("another live instance already uses this name", "name", *name, "using", instName)
	}

	m := metrics.New()
	rec := proxy.NewStatusRecorder()
	inst := state.Instance{
		Schema: state.SchemaVersion, Name: instName, Command: argv,
		WardenPID: os.Getpid(), WardenCreatedAt: selfCreated,
		State: state.Starting, StartedAt: time.Now(),
	}
	path := state.InstancePath(dir, instName)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	writerDone := make(chan struct{})
	writerCtx, stopWriter := context.WithCancel(context.Background())
	go func() {
		defer close(writerDone)
		publishState(writerCtx, path, &inst, rec, m, log)
	}()

	err = proxy.Run(ctx, proxy.Config{
		Spawn:    proxy.ExecSpawner(argv, os.Environ()),
		In:       env.Stdin,
		Out:      env.Stdout,
		Log:      log,
		Metrics:  m,
		OnStatus: rec.Set,
		ChildStderr: func(line string) {
			// Pass through so the client's own log capture still works, and
			// keep a copy in our log file.
			fmt.Fprintln(env.Stderr, line)
			fileLog.Debug("server stderr", "line", line)
		},
		Backoff:       supervisor.Backoff{Base: *backoffBase, Max: *backoffMax, Factor: 2, Jitter: 0.2},
		MaxRestarts:   *maxRestarts,
		RestartWindow: *window,
		BufferSize:    *bufSize,
		MaxMessage:    *maxMsg,
		InitTimeout:   *initTimeout,
		Grace:         *grace,
	})
	stopWriter()
	<-writerDone

	var se *proxy.SpawnError
	if errors.As(err, &se) {
		log.Error("could not start MCP server", "command", argv, "err", se.Err)
		os.Remove(path)
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, iofs.ErrNotExist) {
			return 127
		}
		return 1
	}
	if err != nil {
		log.Error("proxy failed", "err", err)
		return 1
	}
	// Clean shutdown: the instance no longer exists, so its state file goes
	// too. A file left behind therefore always means "warden died".
	os.Remove(path)
	return 0
}

// uniqueName returns name, or name-<pid> if a live instance already owns it.
func uniqueName(dir, name string) string {
	var other state.Instance
	if state.ReadFile(state.InstancePath(dir, name), &other) == nil &&
		other.WardenPID != os.Getpid() &&
		procscan.Alive(other.WardenPID, other.WardenCreatedAt) {
		return fmt.Sprintf("%s-%d", name, os.Getpid())
	}
	return name
}

// publishState writes the state file once a second and promptly after a
// status change (but never more than once every 250ms), until ctx ends.
func publishState(ctx context.Context, path string, inst *state.Instance, rec *proxy.StatusRecorder, m *metrics.Metrics, log *slog.Logger) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var last time.Time
	childPID := 0
	write := func() {
		s := rec.Get()
		if s.Phase != "" {
			inst.State = s.Phase
		}
		inst.Restarts = s.Restarts
		inst.LastRestartAt = s.LastRestartAt
		inst.LastExit = s.LastExit
		inst.ChildPID = s.ChildPID
		if s.ChildPID != childPID {
			// Record the OS creation time, not our own clock, so `reap` can
			// verify identity before killing.
			childPID = s.ChildPID
			inst.ChildStartedAt, _ = procscan.CreateTime(childPID)
		}
		if s.ChildPID == 0 {
			inst.ChildStartedAt = time.Time{}
		}
		snap := m.Snapshot()
		inst.Counts = state.Counts{Requests: snap.Requests, Notifications: snap.Notifications, Errors: snap.Errors}
		inst.LatencyMs = state.Latency{P50: snap.P50.Milliseconds(), P95: snap.P95.Milliseconds(), Max: snap.Max.Milliseconds()}
		inst.UpdatedAt = time.Now()
		if err := state.WriteFile(path, inst); err != nil {
			log.Debug("write state file", "err", err)
		}
		last = time.Now()
	}
	write()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			write()
		case <-rec.Changed():
			if wait := 250*time.Millisecond - time.Since(last); wait > 0 {
				select {
				case <-time.After(wait):
				case <-ctx.Done():
					return
				}
			}
			write()
		}
	}
}
