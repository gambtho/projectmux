// Package rebuild recovers lost workspace registrations from live tmux
// sessions. Classification is pure — no I/O, no clock, no git — because
// the case analysis is the part most likely to be wrong, and a pure
// function is exhaustively testable from literals. Everything that has to
// touch the world happens in application, against the classification a
// second, lock-held pass produces.
package rebuild

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/state"
)

// Case is what rebuild would do about one live session.
type Case int

const (
	// CaseSettled: a row exists and already records this session name.
	// It produces nothing at all — no candidate and no conflict — which
	// is what makes a second run a silent no-op that exits 0.
	CaseSettled Case = iota
	// CaseRegister: a live session with no row at all. The primary
	// recovery path, and the only case that inserts.
	CaseRegister
	// CaseAdopt: a row exists with no actual session, and one live
	// session claims it. Adoption alone; the row is never rewritten.
	CaseAdopt
	// CaseSessionMismatch: the row records a different session name.
	CaseSessionMismatch
	// CaseIdentityMismatch: the live slug or worktree contradicts the
	// row. Acting here would repoint a workspace at the wrong tree.
	CaseIdentityMismatch
	// CaseDuplicateID: two live sessions carry one workspace ID.
	CaseDuplicateID
	// CaseNameTaken: the live name is another workspace's actual session.
	CaseNameTaken
)

// String names the case for test failures and debugging. It is not part of
// the JSON output; the report renders Conflict.Reason.
func (c Case) String() string {
	switch c {
	case CaseSettled:
		return "settled"
	case CaseRegister:
		return "register"
	case CaseAdopt:
		return "adopt"
	case CaseSessionMismatch:
		return "session-mismatch"
	case CaseIdentityMismatch:
		return "identity-mismatch"
	case CaseDuplicateID:
		return "duplicate-id"
	case CaseNameTaken:
		return "name-taken"
	}
	return fmt.Sprintf("Case(%d)", int(c))
}

// Candidate is a session rebuild would write for: CaseRegister or
// CaseAdopt only. Every other case writes nothing, so it has no candidate.
type Candidate struct {
	Case    Case
	Session controller.LiveSession
	// Record is the stored row, nil for CaseRegister. It points into a
	// copy Classify owns, so a caller cannot reach its own input through
	// it.
	Record *state.Record
}

// Conflict is a session rebuild declines to act on, with the reason a
// reader needs. Subject is the live session name.
type Conflict struct {
	Subject string
	Reason  string
}

// Plan is one classification pass over live sessions and stored records.
type Plan struct {
	Candidates []Candidate
	Conflicts  []Conflict
}

// Classify sorts live sessions into what rebuild would do about each.
//
// Precedence matters, because one session can match several rows at once:
// duplicate ID, then name taken, then identity mismatch, then settled,
// then adopt, then register — with a session mismatch as the residual when
// a row exists and already names a different session. Uncertainty wins
// over action every time, which is what keeps rebuild fill-only: no case
// that could overwrite a recorded value reaches a candidate.
//
// Sessions carrying no workspace ID belong to someone else and are ignored
// entirely — neither candidate nor conflict, as in buildList.
//
// The output is deterministically ordered and does not depend on the order
// tmux happened to list sessions in: candidates by session slug then name,
// conflicts by subject.
func Classify(live []controller.LiveSession, records []state.Record) Plan {
	// A copy sorted by ID, so the indexes below are built in an order
	// that does not depend on the caller's, and so the pointers handed
	// out in candidates cannot reach the caller's slice. Records are
	// unique by ID: the column is the primary key.
	rows := slices.Clone(records)
	slices.SortFunc(rows, func(a, b state.Record) int { return cmp.Compare(a.ID, b.ID) })

	byID := make(map[string]*state.Record, len(rows))
	byActualSession := make(map[string]*state.Record, len(rows))
	for i := range rows {
		row := &rows[i]
		byID[row.ID] = row
		if row.ActualSession != nil {
			byActualSession[*row.ActualSession] = row
		}
	}

	// Claimant names per workspace ID, sorted so the duplicate-ID reason
	// reads the same however tmux ordered its output.
	claimants := make(map[string][]string, len(live))
	for _, s := range live {
		if s.WorkspaceID == "" {
			continue
		}
		claimants[s.WorkspaceID] = append(claimants[s.WorkspaceID], s.Name)
	}
	for id := range claimants {
		slices.Sort(claimants[id])
	}

	var plan Plan
	conflict := func(s controller.LiveSession, reason string) {
		plan.Conflicts = append(plan.Conflicts, Conflict{Subject: s.Name, Reason: reason})
	}
	for _, s := range live {
		if s.WorkspaceID == "" {
			continue
		}
		row := byID[s.WorkspaceID]
		owner := byActualSession[s.Name]
		switch {
		case len(claimants[s.WorkspaceID]) > 1:
			conflict(s, duplicateIDReason(s, claimants[s.WorkspaceID]))
		case owner != nil && owner.ID != s.WorkspaceID:
			conflict(s, nameTakenReason(s, owner))
		case row != nil && (row.Slug != s.Slug || row.Worktree != s.Worktree):
			conflict(s, identityMismatchReason(s, row))
		case row != nil && row.ActualSession != nil && *row.ActualSession == s.Name:
			// Settled. Deliberately silent.
		case row != nil && row.ActualSession == nil:
			plan.Candidates = append(plan.Candidates, Candidate{
				Case: CaseAdopt, Session: s, Record: row,
			})
		case row != nil:
			conflict(s, sessionMismatchReason(s, row))
		default:
			plan.Candidates = append(plan.Candidates, Candidate{
				Case: CaseRegister, Session: s,
			})
		}
	}

	slices.SortFunc(plan.Candidates, func(a, b Candidate) int {
		if c := cmp.Compare(a.Session.Slug, b.Session.Slug); c != 0 {
			return c
		}
		return cmp.Compare(a.Session.Name, b.Session.Name)
	})
	slices.SortFunc(plan.Conflicts, func(a, b Conflict) int {
		return cmp.Compare(a.Subject, b.Subject)
	})
	return plan
}

// duplicateIDReason names every claimant, because the operator's next step
// is to look at those sessions and kill or rename one.
func duplicateIDReason(s controller.LiveSession, names []string) string {
	return fmt.Sprintf(
		"sessions %s all carry workspace ID %s, so rebuild cannot tell which one "+
			"is the workspace; none of them is registered.",
		quotedList(names), s.WorkspaceID)
}

// nameTakenReason names the workspace that already holds the name, since
// that is where the operator has to look to free it.
func nameTakenReason(s controller.LiveSession, owner *state.Record) string {
	return fmt.Sprintf(
		"session %q is already recorded as the session of workspace %s (%s), so "+
			"rebuild will not also adopt it for workspace %s.",
		s.Name, owner.ID, owner.Slug, s.WorkspaceID)
}

// identityMismatchReason prints both identities side by side, because the
// disagreement is the whole finding.
func identityMismatchReason(s controller.LiveSession, row *state.Record) string {
	return fmt.Sprintf(
		"session %q carries slug %q and worktree %q, but workspace %s is recorded "+
			"as slug %q and worktree %q; that contradiction is evidence of "+
			"corruption or collision rather than a match, so nothing is written.",
		s.Name, s.Slug, s.Worktree, row.ID, row.Slug, row.Worktree)
}

// sessionMismatchReason states the fill-only rule outright, since this is
// the case where an operator is most likely to expect an overwrite.
func sessionMismatchReason(s controller.LiveSession, row *state.Record) string {
	return fmt.Sprintf(
		"workspace %s (%s) already records session %q, but the live session "+
			"carrying its identity keys is named %q; rebuild fills in missing "+
			"state and never overwrites a recorded session name.",
		row.ID, row.Slug, *row.ActualSession, s.Name)
}

// quotedList renders names as prose: "a", "a" and "b", or "a", "b", and "c".
func quotedList(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = fmt.Sprintf("%q", name)
	}
	switch len(quoted) {
	case 0:
		return ""
	case 1:
		return quoted[0]
	case 2:
		return quoted[0] + " and " + quoted[1]
	}
	return strings.Join(quoted[:len(quoted)-1], ", ") + ", and " + quoted[len(quoted)-1]
}
