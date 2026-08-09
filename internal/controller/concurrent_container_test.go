// The concurrency test lives in the external test package for the same
// reason the ensure tests do: it needs the exported fakes, and
// controller/fake imports controller.
package controller_test

import (
	"context"
	"path"
	"sync"
	"testing"
	"time"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/controller/fake"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

const (
	sharedRepoID        = "repo-1"
	sharedRepoRoot      = "/w/slab"
	sharedContainerID   = "cid-shared"
	sharedContainerUser = "vscode"
	sharedContainerDir  = "/workspaces/slab"
)

// sharedContainer is an observer and an actuator in one, standing in for
// the single real container every session on a repository shares. It is
// stateful where fake.ContainerObserver is canned: discovery reports
// missing until a start has succeeded and reports the started container
// afterwards, which is what the repository-keyed local_folder filter gives
// the real adapter (internal/container/adapter.go:94-137) and is the whole
// subject of this test.
//
// It is local rather than an extension of fake.ContainerObserver for a
// second reason: the shared fakes record their calls with unsynchronized
// slice appends (internal/controller/fake/fake.go:76,84,359). Two Ensure
// passes are serialized by an flock, which the race detector cannot see as
// a happens-before edge, so sharing those fakes across goroutines would be
// reported as a data race even though the serialization is real.
type sharedContainer struct {
	mu      sync.Mutex
	started bool
	starts  int
}

var (
	_ controller.ContainerObserver = (*sharedContainer)(nil)
	_ controller.ContainerActuator = (*sharedContainer)(nil)
)

func (c *sharedContainer) Applies(context.Context, resolve.Workspace, config.Config) (bool, error) {
	return true, nil
}

// DiscoverContainer mirrors the adapter's label query: before any start
// there is nothing to find, and afterwards the match is present but
// unbound, because a label cannot supply the remote user or workdir.
func (c *sharedContainer) DiscoverContainer(context.Context, resolve.Workspace, config.Config) (*controller.ContainerObservation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.started {
		return &controller.ContainerObservation{
			Kind: "devcontainer", Health: state.HealthMissing,
		}, nil
	}
	return &controller.ContainerObservation{
		Kind: "devcontainer", ContainerID: sharedContainerID, Health: state.HealthPresent,
	}, nil
}

// ProbeContainer mirrors the adapter's inspect: a live container carries
// the stored binding's identity back out, so a session that already knows
// the binding needs no acquiring start.
func (c *sharedContainer) ProbeContainer(_ context.Context, b state.ContainerBinding) (controller.ContainerObservation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.started || b.ContainerID != sharedContainerID {
		return controller.ContainerObservation{Health: state.HealthMissing}, nil
	}
	return controller.ContainerObservation{
		Kind:          b.Kind,
		ContainerID:   b.ContainerID,
		ContainerUser: b.ContainerUser,
		Workdir:       b.Workdir,
		Health:        state.HealthPresent,
	}, nil
}

// StartContainer counts the devcontainer up invocations this test exists
// to bound, and makes the container observable from here on.
func (c *sharedContainer) StartContainer(context.Context, resolve.Workspace, config.Config) (controller.ContainerObservation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.starts++
	c.started = true
	return controller.ContainerObservation{
		Kind:          "devcontainer",
		ContainerID:   sharedContainerID,
		ContainerUser: sharedContainerUser,
		Workdir:       sharedContainerDir,
		Health:        state.HealthPresent,
	}, nil
}

func (c *sharedContainer) ExecCommand(b state.ContainerBinding, command, relDir string, _ map[string]string) string {
	return "fake-exec " + b.ContainerID + " " + path.Join(b.Workdir, relDir) + " " + command
}

func (c *sharedContainer) StopContainer(context.Context, string) error { return nil }

func (c *sharedContainer) startCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.starts
}

// concurrentRepoWorkspace builds one session on the shared repository. The
// default session carries the bare slug as its session name; a named one
// gets the slug--session form the resolver produces.
//
// Named repoWorkspace in the task brief, but that name (with a different
// signature) already belongs to lock_ordering_test.go's per-session
// fixture in this same package; renamed here to avoid the redeclaration
// rather than touching an existing, unrelated test.
func concurrentRepoWorkspace(session string) resolve.Workspace {
	ws := resolve.Workspace{
		ID:           "ws-default",
		RepositoryID: sharedRepoID,
		Slug:         "slab",
		RepoRoot:     sharedRepoRoot,
		SessionName:  "slab",
	}
	if session != "" {
		ws.ID = "ws-" + session
		ws.Session = session
		ws.SessionName = "slab--" + session
	}
	return ws
}

// concurrentRepoDesired is named for the same reason: autostart_test.go's
// repoDesired() (no args, builds a controller.RepoDesired) already occupies
// that name in this package.
func concurrentRepoDesired(ws resolve.Workspace) controller.Desired {
	return controller.Desired{
		Workspace: ws,
		Config: config.Config{
			Version:      1,
			DevContainer: config.DevContainer{Enabled: "true"},
			Environment:  map[string]string{"FOO": "bar"},
		},
		Digest: "sha256:desired",
	}
}

// repoSession is the live tmux session the post-create confirmation must
// see. @dev_worktree keeps its name but now carries the repository root
// (design §5.1), so both sessions report the same value there. @dev_session
// carries the workspace's own session component, distinguishing the two.
func repoSession(ws resolve.Workspace) controller.LiveSession {
	return controller.LiveSession{
		Name:        ws.SessionName,
		WorkspaceID: ws.ID,
		Slug:        ws.Slug,
		Worktree:    ws.RepoRoot,
		Session:     ws.Session,
	}
}

// TestConcurrentOpensIssueOneContainerStart is the §6.1 race stated as a
// test: two sessions on one repository opened at the same instant must
// produce one devcontainer up between them. Deduplication is a composition
// of two mechanisms, and neither the locking test nor the adapter test
// covers it — the repository lock makes the second pass wait, and the
// repository-keyed binding then lets it observe the container the first
// pass started instead of starting a second one.
//
// Each goroutine gets its own Controller because scriptedSessions and the
// session actuator are per-pass scripts; the store, the container, and the
// lock directory are what they share, exactly as two processes would.
func TestConcurrentOpensIssueOneContainerStart(t *testing.T) {
	store := fake.NewStore()
	containers := &sharedContainer{}
	lockDir := t.TempDir()

	workspaces := []resolve.Workspace{concurrentRepoWorkspace(""), concurrentRepoWorkspace("review")}
	results := make([]controller.EnsureResult, len(workspaces))
	errs := make([]error, len(workspaces))

	release := make(chan struct{})
	var wg sync.WaitGroup
	for i, ws := range workspaces {
		wg.Add(1)
		go func(i int, ws resolve.Workspace) {
			defer wg.Done()
			ctrl := &controller.Controller{
				Store: store,
				Sessions: &scriptedSessions{steps: []func(controller.SessionQuery) (controller.SessionObservation, error){
					absentStep(),              // initial observation
					absentStep(),              // allocated-name squat check
					liveStep(repoSession(ws)), // post-create confirmation
				}},
				Containers:   containers,
				Clock:        &fake.Clock{Time: ensureTime},
				Actuator:     &fake.SessionActuator{},
				ContainerAct: containers,
			}
			// A start gate, not a barrier: close(release) runs as soon as
			// the spawn loop finishes, so a goroutine arriving late finds
			// the channel already closed and never blocks. It widens the
			// window in which the two Ensure calls overlap; it does not
			// release them together.
			//
			// Nothing below depends on true simultaneity. The single
			// container start is the shared binding's doing, not the
			// lock's: stubbing the repository lock out entirely leaves
			// this test green. lock_ordering_test.go owns the lock
			// guarantee, and fails when it is stubbed.
			<-release
			results[i], errs[i] = ctrl.Ensure(context.Background(), concurrentRepoDesired(ws),
				[]controller.WindowIntent{{Name: "shell"}}, lockDir, 5*time.Second)
		}(i, ws)
	}
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Ensure for session %q: %v", workspaces[i].SessionName, err)
		}
	}
	if got := containers.startCount(); got != 1 {
		t.Fatalf("devcontainer up invocations = %d, want exactly 1", got)
	}
	if results[0].Session == results[1].Session {
		t.Errorf("both opens landed on session %q; the two sessions must stay distinct",
			results[0].Session)
	}
	for i, res := range results {
		if res.Container == nil || res.Container.ContainerID != sharedContainerID {
			t.Errorf("session %q reported container %+v, want %q",
				workspaces[i].SessionName, res.Container, sharedContainerID)
		}
	}
	for _, ws := range workspaces {
		rec, err := store.Workspace(ws.ID)
		if err != nil {
			t.Fatalf("Workspace(%s): %v", ws.ID, err)
		}
		if rec.Container == nil || rec.Container.ContainerID != sharedContainerID {
			t.Errorf("stored binding for session %q = %+v, want %q",
				ws.SessionName, rec.Container, sharedContainerID)
		}
	}
}
