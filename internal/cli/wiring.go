package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/container"
	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/doctor"
	"github.com/gambtho/projectmux/internal/rebuild"
	runner "github.com/gambtho/projectmux/internal/run"
	"github.com/gambtho/projectmux/internal/state"
	"github.com/gambtho/projectmux/internal/tmux"
)

// stateStore is what the commands need from the state store. Rebuild's
// migration pass deletes, which nothing else does, so its slice is named
// separately rather than folded into controller.Store.
type stateStore interface {
	controller.Store
	rebuild.MigrationStore
	Close() error
}

// The observation commands' dependencies are package variables so command
// tests can substitute fakes; the defaults are the production wiring.
var (
	openStore = func() (stateStore, error) {
		root, err := state.Root()
		if err != nil {
			return nil, err
		}
		return state.Open(root)
	}
	liveSessions = func(ctx context.Context) ([]controller.LiveSession, error) {
		return (&tmux.Client{Socket: tmux.EnvSocket()}).Sessions(ctx)
	}
	newSessionObserver = func() controller.SessionObserver {
		return &tmux.Client{Socket: tmux.EnvSocket()}
	}
)

// systemClock satisfies controller.Clock. Nothing is persisted by the
// observation commands, but the controller requires a clock.
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// stamp renders a stored timestamp for output, matching the store's
// RFC3339Nano UTC convention.
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// storedContainerInfo is the stored binding as rendered in JSON
// envelopes. It is always last-observed state, never a live probe.
type storedContainerInfo struct {
	Kind          string `json:"kind"`
	ContainerID   string `json:"container_id"`
	ContainerUser string `json:"container_user,omitempty"`
	Workdir       string `json:"workdir,omitempty"`
	Health        string `json:"health"`
	ObservedAt    string `json:"observed_at"`
}

func storedContainer(b *state.ContainerBinding) *storedContainerInfo {
	if b == nil {
		return nil
	}
	return &storedContainerInfo{
		Kind:          b.Kind,
		ContainerID:   b.ContainerID,
		ContainerUser: b.ContainerUser,
		Workdir:       b.Workdir,
		Health:        string(b.Health),
		ObservedAt:    stamp(b.ObservedAt),
	}
}

// newSessionActuator is the mutation seam mirroring newSessionObserver.
var newSessionActuator = func() controller.SessionActuator {
	return &tmux.Client{Socket: tmux.EnvSocket()}
}

// newSessionRetagger is the mutation seam for rewriting a live session's
// identity keys, used only by rebuild's upgrade pass.
var newSessionRetagger = func() rebuild.Retagger {
	return &tmux.Client{Socket: tmux.EnvSocket()}
}

// Container observation and actuation seams; the defaults are the real
// adapter.
var (
	newContainerObserver = func() controller.ContainerObserver {
		return &container.Adapter{}
	}
	newContainerActuator = func() controller.ContainerActuator {
		return &container.Adapter{}
	}
)

// windowIntents derives the actuator window intents purely from merged
// configuration: implicit shell window when none is configured; the
// location tri-state is resolved against the binding inside Ensure.
// The implicit window also gets the default shell pane here — it lives
// outside the digested config, so its pane arrives at derivation too
// (the two-pane spec's §3 exception).
func windowIntents(cfg config.Config) []controller.WindowIntent {
	if len(cfg.Windows) == 0 {
		return []controller.WindowIntent{{
			Name:  "shell",
			Panes: []controller.PaneIntent{{Name: "shell"}},
		}}
	}
	intents := make([]controller.WindowIntent, 0, len(cfg.Windows))
	for _, w := range cfg.Windows {
		in := controller.WindowIntent{Name: w.Name, Focus: w.Focus}
		switch {
		case w.Agent != nil:
			in.Command = *w.Agent
		case w.Command != nil:
			in.Command = *w.Command
		}
		if w.Cwd != nil {
			in.RelDir = *w.Cwd
		}
		if w.Location != nil {
			in.Location = controller.WindowLocation(*w.Location)
		}
		// Allocate only when panes exist so a pane-less window keeps a nil
		// slice: normalized config always carries the list, but tests build
		// Config values directly, and nil-vs-empty matters to DeepEqual.
		if len(w.Panes) > 0 {
			in.Panes = make([]controller.PaneIntent, 0, len(w.Panes))
			for _, p := range w.Panes {
				pane := controller.PaneIntent{Name: p.Name, Focus: p.Focus}
				switch {
				case p.Agent != nil:
					pane.Command = *p.Agent
				case p.Command != nil:
					pane.Command = *p.Command
				}
				if p.Cwd != nil {
					pane.RelDir = *p.Cwd
				}
				in.Panes = append(in.Panes, pane)
			}
		}
		intents = append(intents, in)
	}
	return intents
}

// tmuxArgv builds a tmux command line, inserting -L when an alternate
// server is configured. The attach seams below cannot use tmux.Client —
// one execs and one needs a live client — so this is the single place
// the socket flag is applied to a hand-built argv.
func tmuxArgv(args ...string) []string {
	argv := []string{"tmux"}
	if socket := tmux.EnvSocket(); socket != "" {
		argv = append(argv, "-L", socket)
	}
	return append(argv, args...)
}

// Terminal attachment seams: a real attach replaces the process and a
// real switch-client needs a live tmux client, so tests substitute all
// three (open/attach spec §8).
var (
	execAttach = func(session string) error {
		path, err := exec.LookPath("tmux")
		if err != nil {
			return fmt.Errorf("finding tmux: %w", err)
		}
		return syscall.Exec(path,
			tmuxArgv("attach-session", "-t", "="+session), os.Environ())
	}
	switchClient = func(ctx context.Context, session string) error {
		res, err := runner.Run(ctx, runner.Command{
			Argv:    tmuxArgv("switch-client", "-t", "="+session),
			Timeout: tmux.DefaultTimeout,
		})
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("tmux switch-client exited %d: %s",
				res.ExitCode, bytes.TrimSpace(res.Stderr))
		}
		return nil
	}
	// currentSocket is the path of the socket this terminal's tmux client
	// is attached to, or "" when the terminal is not inside tmux at all.
	// $TMUX is "<socket-path>,<server-pid>,<session-index>", so the first
	// field is the path. It is one seam rather than two because "am I
	// inside tmux" and "which server" must never disagree.
	//
	// The whole path is kept, not its base name: a client started with
	// "tmux -S /elsewhere/pmx" has the same base name as "-L pmx" while
	// addressing a different server, and comparing names would call that
	// a match.
	currentSocket = func() string {
		v := os.Getenv("TMUX")
		if v == "" {
			return ""
		}
		path, _, _ := strings.Cut(v, ",")
		return path
	}
)

// crossServerRefusal reports the terminal being attached to a tmux
// server other than the one projectmux drives, and nil otherwise
// (including when the terminal is not inside tmux at all).
//
// tmux switch-client is intra-server only and attach-session refuses to
// nest, so attaching across servers cannot succeed; and the failure it
// guards against — acting on the other server's sessions — is the whole
// point of running on a separate socket (design §13 step 6).
//
// Servers are compared by socket path, which is what actually
// distinguishes them. A tmux started with -S outside the default socket
// directory therefore never matches and is refused; refusing is the safe
// direction.
func crossServerRefusal() error {
	got := currentSocket()
	if got == "" {
		return nil
	}
	want := tmux.SocketPath(tmux.EnvSocket())
	if filepath.Clean(got) == want {
		return nil
	}
	return &controller.RefusalError{Reason: fmt.Sprintf(
		"this terminal is attached to the tmux server at %s, but projectmux "+
			"is driving %s; tmux cannot move a client between servers. "+
			"Detach first, or run with --no-attach.", got, want)}
}

// attachTerminal connects the terminal to the session: switch-client
// inside tmux, an exec of attach-session outside (open/attach spec §2).
func attachTerminal(ctx context.Context, session string) error {
	if err := crossServerRefusal(); err != nil {
		return err
	}
	if currentSocket() == "" {
		return execAttach(session)
	}
	return switchClient(ctx, session)
}

// sessionListerFunc adapts the liveSessions seam to doctor's bulk
// enumeration interface.
type sessionListerFunc func(ctx context.Context) ([]controller.LiveSession, error)

func (f sessionListerFunc) Sessions(ctx context.Context) ([]controller.LiveSession, error) {
	return f(ctx)
}

// inspectDatabase is doctor's database seam. It never returns an error:
// every outcome — missing, unreadable, corrupt, drifted — is a finding
// the report carries. The store is non-nil only when the view is safe to
// query, and the returned close is always safe to call.
//
// Doctor deliberately does not go through openStore: state.Open creates
// the file, enables WAL, and migrates, and a command that reports
// "migrations pending" must not perform them in the same breath. The
// database's own bytes are never written, and nothing new appears beside
// it either — see state.OpenReadOnly for how the -shm/-wal sidecars a WAL
// reader would otherwise leave behind are avoided.
var inspectDatabase = func(root string) (doctor.Database, doctor.Store, func()) {
	db := doctor.Database{Path: state.DBPath(root), Supported: state.SchemaVersion}
	ro, insp, err := state.OpenReadOnly(root)
	if err != nil {
		if state.IsMissingDatabase(err) {
			// A fresh installation: nothing is registered, which is a
			// fact rather than uncertainty.
			db.Missing = true
		} else {
			db.OpenErr = err
		}
		return db, nil, func() {}
	}
	closeDB := func() { _ = ro.Close() }
	db.IntegrityErr = insp.IntegrityErr
	db.Version = insp.UserVersion
	if insp.Usable() != nil {
		return db, nil, closeDB
	}
	return db, ro, closeDB
}

// rebuildDatabaseCheck decides whether rebuild may open the database
// read-write. A nil error means proceed.
//
// state.Open is not a corruption test: against a database already at the
// current schema it needs only a successful PRAGMA user_version, so
// damage elsewhere in the file would surface mid-run, after rebuild had
// begun writing. Rebuild therefore classifies the file the way doctor
// does — read-only, through PRAGMA integrity_check — before opening it
// to write (spec §5).
//
// Two verdicts differ from doctor's, both deliberately. A pending
// migration is doctor's finding but rebuild's normal path, because
// rebuild is a mutating command. An unrecovered write-ahead log is the
// crash rebuild exists to recover from, so refusing it would refuse the
// main case; state.Open recovers the log.
var rebuildDatabaseCheck = func(root string) error {
	path := state.DBPath(root)

	ro, insp, err := state.OpenReadOnly(root)
	if err != nil {
		switch {
		case state.IsMissingDatabase(err):
			// A fresh installation. state.Open creates it and rebuild
			// proceeds against an empty database; nothing is destroyed.
			return nil
		case state.IsIncompleteWAL(err):
			return nil
		default:
			return corruptDatabaseError(path, err)
		}
	}
	defer func() { _ = ro.Close() }()

	usable := insp.Usable()
	if usable == nil {
		return nil
	}
	var pending *state.PendingMigrationError
	if errors.As(usable, &pending) {
		return nil
	}
	// A newer build's database is refused, but its contents are good and
	// the message must not tell anyone to move them aside.
	var future *state.FutureSchemaError
	if errors.As(usable, &future) {
		return futureSchemaError(path, usable)
	}
	// What is left is confirmed damage, and not something to guess past.
	return corruptDatabaseError(path, usable)
}

// corruptDatabaseError is the refusal message, which is the deliverable
// here. Naming the sidecars is not pedantry: moving state.db alone
// leaves a stale write-ahead log that a freshly created database would
// inherit.
func corruptDatabaseError(path string, cause error) error {
	return fmt.Errorf(
		"the state database at %s cannot be read: %w\n"+
			"rebuild will not move it aside for you. Move all three of %s, %s, and %s "+
			"aside, then run projectmux rebuild again to recover what live tmux "+
			"sessions still describe",
		path, cause, path, path+"-wal", path+"-shm")
}

// futureSchemaError refuses a database this build is too old to write,
// and says nothing about moving files. The data is intact and a newer
// projectmux — probably still installed — reads it correctly; rebuilding
// over it would discard everything the newer schema records that this one
// has no column for. The wrapped error already names both versions and
// says to upgrade, so this adds only the path and the reason not to
// reach for the corrupt-database remedy.
func futureSchemaError(path string, cause error) error {
	return fmt.Errorf(
		"the state database at %s was written by a newer projectmux: %w\n"+
			"its contents are intact, and rebuilding with this build would discard "+
			"what the newer schema records",
		path, cause)
}

// newVersionRunner is doctor's dependency-probe seam.
var newVersionRunner = func() doctor.VersionRunner { return toolProbe{} }

// toolProbe runs one tool's version command. PATH resolution is separate
// from execution so an absent binary is reported as confirmed absence
// rather than as an execution failure.
type toolProbe struct{}

func (toolProbe) Probe(ctx context.Context, argv ...string) (doctor.ProbeResult, error) {
	if len(argv) == 0 {
		return doctor.ProbeResult{}, errors.New("probe requires a command")
	}
	if _, err := exec.LookPath(argv[0]); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return doctor.ProbeResult{}, nil
		}
		return doctor.ProbeResult{}, fmt.Errorf("finding %s: %w", argv[0], err)
	}
	res, err := runner.Run(ctx, runner.Command{Argv: argv, Timeout: tmux.DefaultTimeout})
	if err != nil {
		return doctor.ProbeResult{}, err
	}
	return doctor.ProbeResult{
		Stdout:   string(bytes.TrimSpace(res.Stdout)),
		Stderr:   string(bytes.TrimSpace(res.Stderr)),
		ExitCode: res.ExitCode,
		Found:    true,
	}, nil
}
