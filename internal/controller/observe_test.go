package controller_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/controller/fake"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

var testTime = time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

func testDesired(enabled string) controller.Desired {
	return controller.Desired{
		Workspace: resolve.Workspace{
			ID:           "w1",
			RepositoryID: "r1",
			Slug:         "slabledger",
			RepoRoot:     "/w/slabledger",
			SessionName:  "slabledger",
		},
		Config: config.Config{
			Version:      1,
			DevContainer: config.DevContainer{Enabled: enabled},
		},
		Digest: "sha256:desired",
	}
}

type deps struct {
	store      *fake.Store
	sessions   *fake.SessionObserver
	containers *fake.ContainerObserver
	ctrl       *controller.Controller
}

func newDeps() *deps {
	d := &deps{
		store:      fake.NewStore(),
		sessions:   &fake.SessionObserver{},
		containers: &fake.ContainerObserver{AppliesResult: true},
	}
	d.ctrl = &controller.Controller{
		Store:      d.store,
		Sessions:   d.sessions,
		Containers: d.containers,
		Clock:      &fake.Clock{Time: testTime},
	}
	return d
}

func TestObserveUnregisteredWorkspace(t *testing.T) {
	d := newDeps()
	snap, err := d.ctrl.Observe(context.Background(), testDesired("false"))
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if snap.Stored != nil {
		t.Errorf("stored = %+v, want nil for an unregistered workspace", snap.Stored)
	}
	if snap.Session.State != controller.SessionAbsent {
		t.Errorf("session state = %q, want absent", snap.Session.State)
	}
	if len(d.sessions.Queries) != 1 ||
		d.sessions.Queries[0].WorkspaceID != "w1" ||
		len(d.sessions.Queries[0].CandidateNames) != 1 ||
		d.sessions.Queries[0].CandidateNames[0] != "slabledger" {
		t.Errorf("session query = %+v", d.sessions.Queries)
	}
}

func TestObserveQueriesTheAssignedNameToo(t *testing.T) {
	d := newDeps()
	ws := testDesired("false").Workspace
	if err := d.store.RegisterWorkspace(ws, "sha256:desired", testTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	// Occupy "slabledger" with another workspace so w1 gets a suffixed name.
	other := ws
	other.ID = "w0"
	other.RepoRoot = "/w/other"
	if err := d.store.RegisterWorkspace(other, "sha256:x", testTime); err != nil {
		t.Fatalf("register other: %v", err)
	}
	if _, err := d.store.AllocateSessionName("w0", testTime); err != nil {
		t.Fatalf("allocate other: %v", err)
	}
	if _, err := d.store.AllocateSessionName("w1", testTime); err != nil {
		t.Fatalf("allocate: %v", err)
	}

	if _, err := d.ctrl.Observe(context.Background(), testDesired("false")); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	q := d.sessions.Queries[0]
	if len(q.CandidateNames) != 2 || q.CandidateNames[0] != "slabledger" ||
		q.CandidateNames[1] != "slabledger-2" {
		t.Errorf("candidate names = %v, want proposed plus assigned", q.CandidateNames)
	}
}

// TestSessionObservationFailureIsUnknownNotAbsence is the spec-§5 gate: a
// tmux outage must not read as absence.
func TestSessionObservationFailureIsUnknownNotAbsence(t *testing.T) {
	d := newDeps()
	d.sessions.Err = errors.New("tmux: no server and no socket permission")
	snap, err := d.ctrl.Observe(context.Background(), testDesired("false"))
	if err != nil {
		t.Fatalf("Observe should not fail on an observer error: %v", err)
	}
	if snap.Session.State != controller.SessionUnknown {
		t.Errorf("session state = %q, want unknown", snap.Session.State)
	}
	if snap.Session.Err == nil {
		t.Error("the observer error should be retained in the snapshot")
	}
}

func TestObserveProbesAnExistingBinding(t *testing.T) {
	d := newDeps()
	ws := testDesired("auto").Workspace
	if err := d.store.RegisterWorkspace(ws, "sha256:desired", testTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := d.store.RecordContainerObservation("w1", state.ContainerObservation{
		Kind: "devcontainer", ContainerID: "c-1", Health: state.HealthPresent,
	}, testTime); err != nil {
		t.Fatalf("seed binding: %v", err)
	}
	d.containers.ProbeResult = controller.ContainerObservation{
		Kind: "devcontainer", ContainerID: "c-1", Health: state.HealthPresent,
	}

	snap, err := d.ctrl.Observe(context.Background(), testDesired("auto"))
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(d.containers.Probed) != 1 || d.containers.Probed[0].ContainerID != "c-1" {
		t.Errorf("probes = %+v, want the stored binding", d.containers.Probed)
	}
	if len(d.containers.Discovered) != 0 {
		t.Errorf("discovery ran despite an existing binding")
	}
	if snap.Container.Observed == nil || snap.Container.Observed.Health != state.HealthPresent {
		t.Errorf("container snapshot = %+v", snap.Container)
	}
}

func TestObserveDiscoversWhenNeverBound(t *testing.T) {
	d := newDeps()
	d.containers.DiscoverResult = &controller.ContainerObservation{Health: state.HealthMissing}
	snap, err := d.ctrl.Observe(context.Background(), testDesired("auto"))
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(d.containers.Discovered) != 1 || d.containers.Discovered[0] != "w1" {
		t.Errorf("discoveries = %v", d.containers.Discovered)
	}
	if snap.Container.Observed == nil || snap.Container.Observed.Health != state.HealthMissing {
		t.Errorf("container snapshot = %+v", snap.Container)
	}
}

// TestAutoResolvingToNoContainerObservesNone is the "auto means maybe
// nothing" gate: a nil discovery result must read exactly like
// enabled "false".
func TestAutoResolvingToNoContainerObservesNone(t *testing.T) {
	d := newDeps()
	d.containers.DiscoverResult = nil
	snap, err := d.ctrl.Observe(context.Background(), testDesired("auto"))
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(d.containers.Discovered) != 1 {
		t.Errorf("discoveries = %v, want one attempt", d.containers.Discovered)
	}
	if snap.Container.Observed != nil || snap.Container.Err != nil {
		t.Errorf("container snapshot = %+v, want empty when no container applies", snap.Container)
	}
}

func TestObserveSkipsContainersWhenDisabled(t *testing.T) {
	d := newDeps()
	snap, err := d.ctrl.Observe(context.Background(), testDesired("false"))
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if snap.Container.Observed != nil {
		t.Errorf("container snapshot = %+v, want none when disabled", snap.Container)
	}
	if len(d.containers.Probed)+len(d.containers.Discovered) != 0 {
		t.Error("no container observation should run when devcontainer is disabled")
	}
}

// TestObserveSkipsContainersWhenUnnormalized is the zero-value gate: an
// unnormalized config.Config leaves Enabled as "", which must read as
// disabled, not as "auto" (design §9 discovery would otherwise run on
// every hand-built or partially-loaded config).
func TestObserveSkipsContainersWhenUnnormalized(t *testing.T) {
	d := newDeps()
	snap, err := d.ctrl.Observe(context.Background(), testDesired(""))
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if snap.Container.Observed != nil {
		t.Errorf("container snapshot = %+v, want none for an unnormalized config", snap.Container)
	}
	if len(d.containers.Probed)+len(d.containers.Discovered) != 0 {
		t.Error("no container observation should run when enabled is the zero value")
	}
}

// TestContainerProbeFailureIsUnknownNotLoss is the design-§9 gate.
func TestContainerProbeFailureIsUnknownNotLoss(t *testing.T) {
	d := newDeps()
	ws := testDesired("true").Workspace
	if err := d.store.RegisterWorkspace(ws, "sha256:desired", testTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := d.store.RecordContainerObservation("w1", state.ContainerObservation{
		Kind: "devcontainer", ContainerID: "c-1", Health: state.HealthPresent,
	}, testTime); err != nil {
		t.Fatalf("seed binding: %v", err)
	}
	d.containers.ProbeErr = errors.New("docker daemon unavailable")

	snap, err := d.ctrl.Observe(context.Background(), testDesired("true"))
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if snap.Container.Observed == nil || snap.Container.Observed.Health != state.HealthUnknown {
		t.Errorf("container snapshot = %+v, want health unknown", snap.Container)
	}
	if snap.Container.Err == nil {
		t.Error("the probe error should be retained in the snapshot")
	}
}

func TestObserveAutoNotApplicableSkipsContainer(t *testing.T) {
	d := newDeps()
	d.containers.AppliesResult = false
	if err := d.store.RegisterWorkspace(testDesired("auto").Workspace, "sha256:x", testTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := d.store.RecordContainerObservation("w1", state.ContainerObservation{
		Kind: "devcontainer", ContainerID: "c1", Health: state.HealthPresent,
	}, testTime); err != nil {
		t.Fatalf("bind: %v", err)
	}

	snap, err := d.ctrl.Observe(context.Background(), testDesired("auto"))
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if snap.Container.Observed != nil || snap.Container.Err != nil {
		t.Errorf("container snapshot = %+v; a non-applicable workspace must look disabled, stored binding or not",
			snap.Container)
	}
	if len(d.containers.Probed) != 0 {
		t.Error("Applies=false still probed the stored binding")
	}
}

func TestObserveAutoAppliesErrorIsUnknown(t *testing.T) {
	d := newDeps()
	d.containers.AppliesErr = errors.New("stat exploded")
	snap, err := d.ctrl.Observe(context.Background(), testDesired("auto"))
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if snap.Container.Observed == nil || snap.Container.Observed.Health != state.HealthUnknown ||
		snap.Container.Err == nil {
		t.Errorf("container snapshot = %+v, want unknown with the error", snap.Container)
	}
}
