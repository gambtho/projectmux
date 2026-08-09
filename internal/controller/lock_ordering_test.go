package controller_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/controller/fake"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

// overlapActuator counts how many StartContainer calls are in flight at
// once. A repository has one container, so the repository lock must hold
// that count at one; the sleep widens the window so an unserialized
// second start overlaps observably rather than by scheduling luck.
type overlapActuator struct {
	mu       sync.Mutex
	inFlight int
	peak     int
	starts   int
}

func (a *overlapActuator) StartContainer(context.Context, resolve.Workspace, config.Config) (controller.ContainerObservation, error) {
	a.mu.Lock()
	a.inFlight++
	a.starts++
	if a.inFlight > a.peak {
		a.peak = a.inFlight
	}
	a.mu.Unlock()

	time.Sleep(50 * time.Millisecond)

	a.mu.Lock()
	a.inFlight--
	a.mu.Unlock()
	return controller.ContainerObservation{
		Kind: "devcontainer", ContainerID: "cid-1", ContainerUser: "vscode",
		Workdir: "/workspaces/slab", Health: state.HealthPresent,
	}, nil
}

func (a *overlapActuator) ExecCommand(_ state.ContainerBinding, command, _ string, _ map[string]string) string {
	return command
}

func (a *overlapActuator) StopContainer(context.Context, string) error { return nil }

// repoWorkspace builds one session's workspace on the shared repository.
// Resolution produces only the default session in this slice, so the
// second session is constructed directly here: it is exactly the case
// the repository lock exists to serialize.
func repoWorkspace(id, session, sessionName string) resolve.Workspace {
	return resolve.Workspace{
		ID: id, RepositoryID: "r1", Slug: "slab", RepoRoot: "/w/slab",
		Session: session, SessionName: sessionName,
	}
}

func TestConcurrentOpensOnOneRepositorySerializeTheContainerStart(t *testing.T) {
	lockDir := t.TempDir()
	store := fake.NewStore()
	containers := &overlapActuator{}

	workspaces := []resolve.Workspace{
		repoWorkspace("w1", "", "slab"),
		repoWorkspace("w2", "review", "slab--review"),
	}
	for _, ws := range workspaces {
		if err := store.RegisterWorkspace(ws, "sha256:desired", ensureTime); err != nil {
			t.Fatalf("register %s: %v", ws.ID, err)
		}
		if _, err := store.AllocateSessionName(ws.ID, ensureTime); err != nil {
			t.Fatalf("allocate %s: %v", ws.ID, err)
		}
	}

	ready := make(chan struct{})
	errs := make([]error, len(workspaces))
	var wg sync.WaitGroup
	for i, ws := range workspaces {
		// Only the store, the lock directory, and the container actuator
		// are genuinely shared: the observers record their calls without
		// synchronization, so each goroutine gets its own.
		ctrl := &controller.Controller{
			Store: store,
			Sessions: &scriptedSessions{steps: []func(controller.SessionQuery) (controller.SessionObservation, error){
				liveStep(controller.LiveSession{
					Name: ws.SessionName, WorkspaceID: ws.ID,
					Slug: ws.Slug, Worktree: ws.RepoRoot, Session: ws.Session,
				}),
			}},
			Containers: &fake.ContainerObserver{
				AppliesResult: true,
				DiscoverResult: &controller.ContainerObservation{
					Kind: "devcontainer", Health: state.HealthMissing,
				},
				// The second session through the lock reads the binding
				// its sibling just wrote on the shared repository, so it
				// probes rather than discovers. Both paths must report
				// the same missing container for this test to be about
				// the lock rather than about observation order.
				ProbeResult: controller.ContainerObservation{
					Kind: "devcontainer", Health: state.HealthMissing,
				},
			},
			Clock:        &fake.Clock{Time: ensureTime},
			Actuator:     &fake.SessionActuator{},
			ContainerAct: containers,
		}
		d := controller.Desired{
			Workspace: ws,
			Config: config.Config{
				Version:      1,
				DevContainer: config.DevContainer{Enabled: "true"},
			},
			Digest: "sha256:desired",
		}
		wg.Add(1)
		go func(i int, ctrl *controller.Controller, d controller.Desired) {
			defer wg.Done()
			<-ready
			_, errs[i] = ctrl.Ensure(context.Background(), d,
				[]controller.WindowIntent{{Name: "shell"}}, lockDir, 10*time.Second)
		}(i, ctrl, d)
	}
	close(ready)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Ensure(%s): %v", workspaces[i].ID, err)
		}
	}
	if containers.peak > 1 {
		t.Errorf("%d concurrent devcontainer ups; the repository lock must serialize them",
			containers.peak)
	}
	// Both sessions still start: the lock serializes the container phase,
	// it does not deduplicate it. One up per repository is the shared
	// binding's job, not the lock's.
	if containers.starts != 2 {
		t.Errorf("starts = %d, want 2", containers.starts)
	}
}
