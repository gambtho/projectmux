package cli

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

var cliTestTime = time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

// guardedStore fails the test if an observation command mutates the
// store: design §8/§12 — list and status must not mutate workspaces.
type guardedStore struct {
	*fake.Store
	t *testing.T
}

func (g guardedStore) Close() error { return nil }

func (g guardedStore) forbidden(method string) error {
	g.t.Errorf("observation command called %s", method)
	return errors.New("observation commands must not mutate the store")
}

func (g guardedStore) RegisterWorkspace(resolve.Workspace, string, time.Time) error {
	return g.forbidden("RegisterWorkspace")
}

func (g guardedStore) AllocateSessionName(string, time.Time) (string, error) {
	return "", g.forbidden("AllocateSessionName")
}

func (g guardedStore) RecordContainerObservation(string, state.ContainerObservation, time.Time) error {
	return g.forbidden("RecordContainerObservation")
}

func (g guardedStore) RecordOperation(string, state.Operation, time.Time) error {
	return g.forbidden("RecordOperation")
}

func (g guardedStore) CommitReconciliation(string, state.ReconciliationResult, time.Time) error {
	return g.forbidden("CommitReconciliation")
}

func installFakeStore(t *testing.T, s *fake.Store) {
	t.Helper()
	orig := openStore
	t.Cleanup(func() { openStore = orig })
	openStore = func() (stateStore, error) {
		return guardedStore{Store: s, t: t}, nil
	}
}

func installLiveSessions(t *testing.T, sessions []controller.LiveSession, err error) {
	t.Helper()
	orig := liveSessions
	t.Cleanup(func() { liveSessions = orig })
	liveSessions = func(context.Context) ([]controller.LiveSession, error) {
		return sessions, err
	}
}

func installSessionObserver(t *testing.T, obs controller.SessionObservation, err error) {
	t.Helper()
	orig := newSessionObserver
	t.Cleanup(func() { newSessionObserver = orig })
	newSessionObserver = func() controller.SessionObserver {
		return &fake.SessionObserver{Observation: obs, Err: err}
	}
}

func TestUnprobedObserverAlwaysFails(t *testing.T) {
	var obs controller.ContainerObserver = unprobedObserver{}
	if _, err := obs.ProbeContainer(context.Background(), state.ContainerBinding{}); err == nil {
		t.Error("ProbeContainer pretended to probe")
	}
	if _, err := obs.DiscoverContainer(context.Background(), resolve.Workspace{},
		config.Config{Version: 1}); err == nil {
		t.Error("DiscoverContainer pretended to discover")
	}
}

func TestStoredContainerRendersTimestampsUTC(t *testing.T) {
	info := storedContainer(&state.ContainerBinding{
		Kind:        "devcontainer",
		ContainerID: "c1",
		Health:      state.HealthMissing,
		ObservedAt:  cliTestTime,
	})
	if info.Health != "missing" {
		t.Errorf("Health = %q, want missing", info.Health)
	}
	if info.ObservedAt != "2026-08-05T12:00:00Z" {
		t.Errorf("ObservedAt = %q", info.ObservedAt)
	}
	if storedContainer(nil) != nil {
		t.Error("storedContainer(nil) should be nil")
	}
}
