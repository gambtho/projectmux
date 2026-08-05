package doctor

import (
	"context"
	"errors"
	"io/fs"
	"os"
)

func (r *Runner) orphanedSessions(ctx context.Context) Check {
	check := Check{Name: "orphaned-sessions"}
	if r.Sessions == nil {
		return verdict(check.Name, StatusUnknown, "tmux is not observable")
	}
	live, err := r.Sessions.Sessions(ctx)
	if err != nil {
		return verdict(check.Name, StatusUnknown, err.Error())
	}
	// No server is not an error and not a finding: nothing is live, so
	// nothing is orphaned.
	if len(live) == 0 {
		return verdict(check.Name, StatusOK, "no live sessions")
	}

	records, err := r.records()
	if err != nil {
		return verdict(check.Name, StatusUnknown, err.Error())
	}
	registered := make(map[string]string, len(records))
	for _, rec := range records {
		registered[rec.ID] = rec.Worktree
	}

	for _, session := range live {
		// Sessions without identity keys belong to someone else.
		if session.WorkspaceID == "" {
			continue
		}
		check.Items = append(check.Items, orphanItem(session.Name, session.WorkspaceID, registered))
	}
	if len(check.Items) == 0 {
		check.Detail = "no projectmux sessions are live"
	}
	return check.aggregate()
}

func orphanItem(name, workspaceID string, registered map[string]string) Item {
	item := Item{Subject: name, Status: StatusOK}
	worktree, ok := registered[workspaceID]
	if !ok {
		item.Status = StatusWarn
		item.Detail = "session not registered: no workspace " + workspaceID
		return item
	}
	if _, err := os.Stat(worktree); err != nil {
		// Only a confirmed absence is a finding. A permission or I/O
		// error says nothing about whether the worktree is there.
		if errors.Is(err, fs.ErrNotExist) {
			item.Status = StatusWarn
			item.Detail = "worktree no longer exists: " + worktree
		} else {
			item.Status = StatusUnknown
			item.Detail = err.Error()
		}
	}
	return item
}
