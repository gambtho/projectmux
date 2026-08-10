package target

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gambtho/projectmux/internal/bindpath"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

// Select turns a parsed target into the workspace to act on. stateRoot is
// the state directory; the bind lookup opens its database read-only.
//
// Selection keys on target *presence*, not on whether the target carried a
// session (design §3). There are exactly three cases:
//
//  1. <repo>/<session> — that session. Explicit and final.
//  2. <repo>          — the default session. The cwd gets no vote; this is
//     also the only way to address the default session from inside a bound
//     directory.
//  3. no target       — resolve the repository from the cwd, then look up a
//     bind that contains the cwd, falling back to the default session.
//
// Cases 1 and 2 are both "the target is present", which is why the branch
// below tests ref.Present rather than ref.HasSession. Collapsing them would
// make `open myrepo`, run from inside a bound directory, open a named session
// the user did not ask for.
func Select(ref Ref, roots []string, cwd, stateRoot string) (resolve.Workspace, error) {
	ws, err := resolve.Resolve(ref.Name, ref.Session, roots, cwd)
	if err != nil {
		return resolve.Workspace{}, err
	}
	if ref.Present {
		return ws, nil
	}
	session, err := sessionForCwd(ws, cwd, stateRoot)
	if err != nil {
		return resolve.Workspace{}, err
	}
	return resolve.WithSession(ws, session), nil
}

// sessionForCwd finds the session on ws's repository whose bind contains cwd,
// or "" for the default session when none does.
func sessionForCwd(ws resolve.Workspace, cwd, stateRoot string) (string, error) {
	// state.OpenReadOnly's failures are typed and deliberately not
	// interchangeable (readonly.go:14-73). Collapsing them all to "fall back
	// to the default session" would let a corrupt database silently open the
	// wrong workspace, so the rule is stated as two named fallbacks and
	// "propagate everything else":
	//
	//   Fall back, silently:
	//     - state.IsMissingDatabase(err) — a fresh installation, in which
	//       nothing is registered and so nothing can be bound.
	//     - *state.PendingMigrationError from insp.Usable() — a diagnosis,
	//       and resolution is not the command that should act on it.
	//
	//   Propagate:
	//     - any other OpenReadOnly error, and
	//     - any other insp.Usable() error — an integrity failure and
	//       *state.FutureSchemaError land here.
	//
	// A permission failure and *state.IncompleteWALError have no dedicated
	// predicate to test for, which is exactly why the rule is written as
	// "propagate everything that is not the two named fallbacks" rather than
	// as a list of propagating types: a refusal added to readonly.go later
	// propagates by default instead of silently joining the fallbacks.
	ro, insp, err := state.OpenReadOnly(stateRoot)
	if err != nil {
		if state.IsMissingDatabase(err) {
			return "", nil
		}
		return "", err
	}
	defer func() { _ = ro.Close() }()
	if err := insp.Usable(); err != nil {
		var pending *state.PendingMigrationError
		if errors.As(err, &pending) {
			return "", nil
		}
		return "", err
	}

	records, err := ro.Workspaces()
	if err != nil {
		return "", err
	}

	// Canonicalize the cwd the same way a bind is canonicalized, by taking it
	// through the repository-relative form and back. A cwd outside the
	// repository root — a linked worktree, which lives outside the main tree —
	// cannot be contained by any repository-relative bind, so it answers with
	// the default session rather than an error.
	relCwd, err := bindpath.Rel(ws.RepoRoot, cwd)
	if err != nil {
		return "", nil
	}
	canonicalCwd, err := bindpath.Resolve(ws.RepoRoot, relCwd)
	if err != nil {
		return "", nil
	}

	best := -1
	var matched []string
	for _, rec := range records {
		if rec.RepositoryID != ws.RepositoryID || rec.Bind == nil {
			continue
		}
		// A bind that no longer canonicalizes inside the repository is
		// treated as missing rather than followed (design §4). It is not a
		// hard failure here: the session simply does not claim this cwd.
		resolved, err := bindpath.Resolve(ws.RepoRoot, *rec.Bind)
		if err != nil {
			continue
		}
		if !bindpath.Contains(resolved, canonicalCwd) {
			continue
		}
		switch d := depth(resolved); {
		case d > best:
			best, matched = d, []string{rec.Session}
		case d == best:
			matched = append(matched, rec.Session)
		}
	}

	switch len(matched) {
	case 0:
		return "", nil
	case 1:
		return matched[0], nil
	}
	// Equal depth and both containing the cwd means the same directory, so
	// this is two sessions bound to one place. resolve.AmbiguousError is the
	// exit-3 shape the CLI already maps; its message is worded for repository
	// names, so the candidates are given as targets the user can pass, which
	// is the actionable part.
	candidates := make([]string, 0, len(matched))
	for _, session := range matched {
		candidates = append(candidates, targetString(ws.Slug, session))
	}
	slices.Sort(candidates)
	return "", &resolve.AmbiguousError{Name: ws.Slug, Candidates: candidates}
}

// depth counts the path components of an absolute path. Longest match is
// measured in components, not string length: /r/services/apiary is longer as a
// string than /r/services/api/v1 but shallower as a path.
func depth(path string) int {
	n := 0
	for _, part := range strings.Split(filepath.ToSlash(filepath.Clean(path)), "/") {
		if part != "" && part != "." {
			n++
		}
	}
	return n
}

// targetString renders the argument form that addresses a session.
func targetString(slug, session string) string {
	if session == "" {
		return slug
	}
	return fmt.Sprintf("%s/%s", slug, session)
}
