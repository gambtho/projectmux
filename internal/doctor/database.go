package doctor

import "fmt"

func (r *Runner) database() Check {
	check := Check{Name: "database", Status: StatusOK}
	switch {
	case r.DB.Missing:
		// A fresh installation is healthy: the database is created by
		// the first command that registers a workspace.
		check.Detail = "no state database yet"
	case r.DB.OpenErr != nil:
		check.Status = StatusUnknown
		check.Detail = r.DB.OpenErr.Error()
	case r.DB.IntegrityErr != nil:
		check.Status = StatusFail
		check.Detail = r.DB.IntegrityErr.Error()
	case r.DB.Version > r.DB.Supported:
		check.Status = StatusFail
		check.Detail = fmt.Sprintf(
			"the database is schema version %d; this build supports %d — it was written by a newer projectmux",
			r.DB.Version, r.DB.Supported)
	case r.DB.Version < r.DB.Supported:
		check.Status = StatusWarn
		check.Detail = fmt.Sprintf(
			"the database is schema version %d; this build supports %d — migrations run on the next mutating command",
			r.DB.Version, r.DB.Supported)
	default:
		check.Detail = fmt.Sprintf("schema version %d at %s", r.DB.Version, r.DB.Path)
	}
	return check
}
