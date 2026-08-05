package controller_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/state"
)

// snapshotFor builds one Snapshot from compact test parameters.
func snapshotFor(t *testing.T, mutate func(*controller.Snapshot)) controller.Snapshot {
	t.Helper()
	snap := controller.Snapshot{
		Desired: testDesired("auto"),
		Session: controller.SessionSnapshot{State: controller.SessionAbsent},
	}
	if mutate != nil {
		mutate(&snap)
	}
	return snap
}

func stringPtr(s string) *string { return &s }

func storedRecord(actual, applied *string) *state.Record {
	return &state.Record{
		ID:              "w1",
		Slug:            "slabledger",
		Worktree:        "/w/slabledger",
		ProposedSession: "slabledger",
		ActualSession:   actual,
		AppliedDigest:   applied,
	}
}

func ourLiveSession() *controller.LiveSession {
	return &controller.LiveSession{
		Name:        "slabledger",
		WorkspaceID: "w1",
		Slug:        "slabledger",
		Worktree:    "/w/slabledger",
	}
}

func TestPlanTable(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*controller.Snapshot)
		want   controller.Plan
	}{
		{
			name:   "unregistered and absent creates and records everything",
			mutate: nil,
			want: controller.Plan{
				Session:    controller.SessionActionCreate,
				RecordName: true,
				Container:  controller.ContainerActionNone,
				Reapply:    true,
			},
		},
		{
			name: "recorded, live, applied, and healthy is a no-op",
			mutate: func(s *controller.Snapshot) {
				s.Stored = storedRecord(stringPtr("slabledger"), stringPtr("sha256:desired"))
				s.Session = controller.SessionSnapshot{
					State:      controller.SessionLive,
					ByIdentity: ourLiveSession(),
					ByName:     []controller.LiveSession{*ourLiveSession()},
				}
				s.Container = controller.ContainerSnapshot{
					Observed: &controller.ContainerObservation{
						ContainerID: "c-1", Health: state.HealthPresent,
					},
				}
			},
			want: controller.Plan{
				Session:   controller.SessionActionNone,
				Container: controller.ContainerActionNone,
			},
		},
		{
			name: "live with matching identity but no record adopts",
			mutate: func(s *controller.Snapshot) {
				s.Session = controller.SessionSnapshot{
					State:      controller.SessionLive,
					ByIdentity: ourLiveSession(),
					ByName:     []controller.LiveSession{*ourLiveSession()},
				}
			},
			want: controller.Plan{
				Session:    controller.SessionActionAdopt,
				RecordName: true,
				Container:  controller.ContainerActionNone,
				Reapply:    true,
			},
		},
		{
			// Refusal is global (spec §5): no RecordName, no Reapply, no
			// container action survives it.
			name: "unknown session state refuses every mutating action",
			mutate: func(s *controller.Snapshot) {
				s.Session = controller.SessionSnapshot{
					State: controller.SessionUnknown,
					Err:   errors.New("tmux unobservable"),
				}
			},
			want: controller.Plan{
				Session:   controller.SessionActionRefuse,
				Container: controller.ContainerActionNone,
			},
		},
		{
			name: "an unknown session with a missing container still plans no container action",
			mutate: func(s *controller.Snapshot) {
				s.Session = controller.SessionSnapshot{
					State: controller.SessionUnknown,
					Err:   errors.New("tmux unobservable"),
				}
				s.Container = controller.ContainerSnapshot{
					Observed: &controller.ContainerObservation{Health: state.HealthMissing},
				}
			},
			want: controller.Plan{
				Session:   controller.SessionActionRefuse,
				Container: controller.ContainerActionNone,
			},
		},
		{
			name: "a foreign session on a candidate name refuses",
			mutate: func(s *controller.Snapshot) {
				s.Session = controller.SessionSnapshot{
					State: controller.SessionAbsent,
					ByName: []controller.LiveSession{{
						Name:        "slabledger",
						WorkspaceID: "someone-else",
					}},
				}
			},
			want: controller.Plan{
				Session:   controller.SessionActionRefuse,
				Container: controller.ContainerActionNone,
			},
		},
		{
			name: "a keyless session on a candidate name refuses",
			mutate: func(s *controller.Snapshot) {
				s.Session = controller.SessionSnapshot{
					State:  controller.SessionAbsent,
					ByName: []controller.LiveSession{{Name: "slabledger"}},
				}
			},
			want: controller.Plan{
				Session:   controller.SessionActionRefuse,
				Container: controller.ContainerActionNone,
			},
		},
		{
			// All three identity keys are load-bearing: a right ID with a
			// contradictory worktree is corruption, not a match.
			name: "an identity match with a contradictory worktree refuses",
			mutate: func(s *controller.Snapshot) {
				live := ourLiveSession()
				live.Worktree = "/w/somewhere-else"
				s.Session = controller.SessionSnapshot{
					State:      controller.SessionLive,
					ByIdentity: live,
					ByName:     []controller.LiveSession{*live},
				}
			},
			want: controller.Plan{
				Session:   controller.SessionActionRefuse,
				Container: controller.ContainerActionNone,
			},
		},
		{
			// A stored record naming a different actual session than the
			// live identity-matched session means the record is stale
			// (e.g. crash recovery); adopt the live session and let
			// execution repair the record.
			name: "live with a stored record naming a different session adopts",
			mutate: func(s *controller.Snapshot) {
				s.Stored = storedRecord(stringPtr("slabledger-old"), stringPtr("sha256:desired"))
				s.Session = controller.SessionSnapshot{
					State:      controller.SessionLive,
					ByIdentity: ourLiveSession(),
					ByName:     []controller.LiveSession{*ourLiveSession()},
				}
			},
			want: controller.Plan{
				Session:    controller.SessionActionAdopt,
				RecordName: true,
				Container:  controller.ContainerActionNone,
				Reapply:    false,
			},
		},
		{
			// A zero-value or unrecognized health is uncertainty, not
			// absence: the conservative action is probe-first, never start.
			name: "an unrecognized container health plans probe-first",
			mutate: func(s *controller.Snapshot) {
				s.Container = controller.ContainerSnapshot{
					Observed: &controller.ContainerObservation{Health: ""},
				}
			},
			want: controller.Plan{
				Session:    controller.SessionActionCreate,
				RecordName: true,
				Container:  controller.ContainerActionProbeFirst,
				Reapply:    true,
			},
		},
		{
			// A hand-built Snapshot can claim SessionLive without an
			// identity-matched session; sessionActionForLive must not be
			// reached, or it panics dereferencing a nil ByIdentity.
			name: "live state with no identity-matched session refuses",
			mutate: func(s *controller.Snapshot) {
				s.Session = controller.SessionSnapshot{State: controller.SessionLive}
			},
			want: controller.Plan{
				Session:   controller.SessionActionRefuse,
				Container: controller.ContainerActionNone,
			},
		},
		{
			name: "digest drift on a live recorded session plans reapply",
			mutate: func(s *controller.Snapshot) {
				s.Stored = storedRecord(stringPtr("slabledger"), stringPtr("sha256:stale"))
				s.Session = controller.SessionSnapshot{
					State:      controller.SessionLive,
					ByIdentity: ourLiveSession(),
					ByName:     []controller.LiveSession{*ourLiveSession()},
				}
			},
			want: controller.Plan{
				Session:   controller.SessionActionNone,
				Reapply:   true,
				Container: controller.ContainerActionNone,
			},
		},
		{
			name: "a missing container plans start",
			mutate: func(s *controller.Snapshot) {
				s.Container = controller.ContainerSnapshot{
					Observed: &controller.ContainerObservation{Health: state.HealthMissing},
				}
			},
			want: controller.Plan{
				Session:    controller.SessionActionCreate,
				RecordName: true,
				Container:  controller.ContainerActionStart,
				Reapply:    true,
			},
		},
		{
			name: "an unknown container plans probe-first, not start",
			mutate: func(s *controller.Snapshot) {
				s.Container = controller.ContainerSnapshot{
					Observed: &controller.ContainerObservation{Health: state.HealthUnknown},
					Err:      errors.New("docker daemon unavailable"),
				}
			},
			want: controller.Plan{
				Session:    controller.SessionActionCreate,
				RecordName: true,
				Container:  controller.ContainerActionProbeFirst,
				Reapply:    true,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := controller.BuildPlan(snapshotFor(t, tc.mutate))
			if got.Session != tc.want.Session ||
				got.RecordName != tc.want.RecordName ||
				got.Container != tc.want.Container ||
				got.Reapply != tc.want.Reapply {
				t.Errorf("plan = %+v, want %+v", got, tc.want)
			}
			if tc.want.Session == controller.SessionActionRefuse && got.Refusal == "" {
				t.Error("a refusing plan must carry an explanation")
			}
		})
	}
}

func TestRefusalNamesTheOccupiedSession(t *testing.T) {
	snap := snapshotFor(t, func(s *controller.Snapshot) {
		s.Session = controller.SessionSnapshot{
			State: controller.SessionAbsent,
			ByName: []controller.LiveSession{{
				Name: "slabledger", WorkspaceID: "someone-else",
			}},
		}
	})
	p := controller.BuildPlan(snap)
	if !strings.Contains(p.Refusal, "slabledger") {
		t.Errorf("refusal %q should name the occupied session", p.Refusal)
	}
}
