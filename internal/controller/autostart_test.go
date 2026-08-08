package controller_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/controller/fake"
	"github.com/gambtho/projectmux/internal/state"
)

// repoRig wires the container phase alone. It deliberately does not use
// ensureRig: autostart no longer reads a workspace row, and a rig that
// registered one would hide a regression into reading it again.
type repoRig struct {
	store     *fake.Store
	sessions  *scriptedSessions
	actuatorC *fake.ContainerActuator
	ctrl      *controller.Controller
	lockDir   string
}

func newRepoRig(t *testing.T) *repoRig {
	t.Helper()
	r := &repoRig{
		store:    fake.NewStore(),
		sessions: &scriptedSessions{},
		actuatorC: &fake.ContainerActuator{
			StartResult: controller.ContainerObservation{
				Kind: "devcontainer", ContainerID: "cid-1",
				ContainerUser: "vscode", Workdir: "/workspaces/slab",
				Health: state.HealthPresent,
			},
		},
		lockDir: t.TempDir(),
	}
	r.ctrl = &controller.Controller{
		Store:        r.store,
		Sessions:     r.sessions,
		Containers:   &fake.ContainerObserver{AppliesResult: true},
		Clock:        &fake.Clock{Time: ensureTime},
		ContainerAct: r.actuatorC,
	}
	if err := r.store.RegisterWorkspace(ensureWorkspace(), "sha256:x", ensureTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	return r
}

func (r *repoRig) start(t *testing.T, d controller.RepoDesired) (controller.ContainerStartOutcome, *controller.ContainerObservation, error) {
	t.Helper()
	return r.ctrl.StartRepositoryContainer(context.Background(), d, r.lockDir, time.Second)
}

func repoDesired() controller.RepoDesired {
	return controller.RepoDesired{
		Repository: state.Repository{ID: "r1", Slug: "slab", RepoRoot: "/w/slab"},
		Config: config.Config{
			Version:      1,
			DevContainer: config.DevContainer{Enabled: "true"},
			Environment:  map[string]string{"FOO": "bar"},
		},
		Digest: "sha256:desired",
	}
}

func repoBinding(t *testing.T, s *fake.Store, id string) *state.ContainerBinding {
	t.Helper()
	repos, err := s.Repositories()
	if err != nil {
		t.Fatalf("Repositories: %v", err)
	}
	for _, repo := range repos {
		if repo.ID == id {
			return repo.Container
		}
	}
	t.Fatalf("no repository %s in %+v", id, repos)
	return nil
}

func TestStartRepositoryContainerStarts(t *testing.T) {
	r := newRepoRig(t)
	r.ctrl.Containers = &fake.ContainerObserver{
		AppliesResult:  true,
		DiscoverResult: &controller.ContainerObservation{Health: state.HealthMissing, Kind: "devcontainer"},
	}

	outcome, obs, err := r.start(t, repoDesired())
	if err != nil {
		t.Fatalf("StartRepositoryContainer: %v", err)
	}
	if outcome != controller.ContainerStarted || obs == nil || obs.ContainerID != "cid-1" {
		t.Errorf("outcome = %v, obs = %+v", outcome, obs)
	}
	if len(r.actuatorC.Started) != 1 || r.actuatorC.Started[0] != "r1" {
		t.Errorf("Started = %v, want one start keyed on the repository", r.actuatorC.Started)
	}
	if b := repoBinding(t, r.store, "r1"); b == nil || b.ContainerID != "cid-1" {
		t.Errorf("binding = %+v, want cid-1 recorded on the repository", b)
	}
	// The whole pass is container-only: tmux must never be consulted.
	if len(r.sessions.queries) != 0 {
		t.Errorf("session observer was consulted: %v", r.sessions.queries)
	}
}

func TestStartRepositoryContainerAlreadyRunning(t *testing.T) {
	r := newRepoRig(t)
	if err := r.store.RecordContainerObservation("r1", state.ContainerObservation{
		Kind: "devcontainer", ContainerID: "cid-1", ContainerUser: "vscode",
		Workdir: "/workspaces/slab", Health: state.HealthPresent,
	}, ensureTime); err != nil {
		t.Fatalf("bind: %v", err)
	}
	r.ctrl.Containers = &fake.ContainerObserver{
		AppliesResult: true,
		ProbeResult: controller.ContainerObservation{
			Health: state.HealthPresent, Kind: "devcontainer", ContainerID: "cid-1",
			ContainerUser: "vscode", Workdir: "/workspaces/slab",
		},
	}
	// The caller reads repositories out of the store, so the binding
	// arrives attached to the repository rather than fetched again.
	d := repoDesired()
	d.Repository.Container = repoBinding(t, r.store, "r1")

	outcome, obs, err := r.start(t, d)
	if err != nil {
		t.Fatalf("StartRepositoryContainer: %v", err)
	}
	if outcome != controller.ContainerAlreadyRunning || obs == nil {
		t.Errorf("outcome = %v, obs = %+v", outcome, obs)
	}
	if len(r.actuatorC.Started) != 0 {
		t.Errorf("an already-running container was started again: %v", r.actuatorC.Started)
	}
}

func TestStartRepositoryContainerNoneApplies(t *testing.T) {
	r := newRepoRig(t)
	r.ctrl.Containers = &fake.ContainerObserver{AppliesResult: false}
	d := repoDesired()
	d.Config.DevContainer.Enabled = "auto"

	outcome, obs, err := r.start(t, d)
	if err != nil {
		t.Fatalf("StartRepositoryContainer: %v", err)
	}
	if outcome != controller.ContainerNoneApplies || obs != nil {
		t.Errorf("outcome = %v, obs = %+v", outcome, obs)
	}
	if len(r.actuatorC.Started) != 0 {
		t.Errorf("Started = %v, want none", r.actuatorC.Started)
	}
}

func TestStartRepositoryContainerUnobservableFails(t *testing.T) {
	r := newRepoRig(t)
	r.ctrl.Containers = &fake.ContainerObserver{
		AppliesResult: true,
		DiscoverErr:   errors.New("docker down"),
	}

	if _, _, err := r.start(t, repoDesired()); err == nil {
		t.Fatal("an unobservable container start was swallowed")
	}
	if len(r.actuatorC.Started) != 0 {
		t.Error("uncertainty reached the container actuator")
	}
	if b := repoBinding(t, r.store, "r1"); b != nil {
		t.Errorf("binding = %+v, want none recorded on a failed observation", b)
	}
}
