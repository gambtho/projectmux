package doctor

import (
	"errors"
	"io/fs"
	"os"
	"strings"

	"github.com/gambtho/projectmux/internal/config"
)

func (r *Runner) configuration() Check {
	check := Check{Name: "configuration"}
	if r.DefaultsErr != nil {
		// Without a defaults layer no workspace can be merged, so there
		// is nothing further to diagnose here.
		return verdict(check.Name, StatusFail, r.DefaultsErr.Error())
	}
	check.Items = append(check.Items, r.defaultsItem())

	slugs, err := config.Slugs(r.ConfigRoot)
	if err != nil {
		check.Items = append(check.Items, Item{
			Subject: "workspaces",
			Status:  StatusUnknown,
			Detail:  err.Error(),
		})
		return check.aggregate()
	}
	for _, slug := range slugs {
		item := Item{Subject: slug, Status: StatusOK}
		if _, err := config.Load(r.ConfigRoot, r.Defaults, slug); err != nil {
			item.Status = StatusFail
			item.Detail = oneLine(err.Error())
		}
		check.Items = append(check.Items, item)
	}
	return check.aggregate()
}

// defaultsItem validates defaults.yaml on its own. Validation otherwise
// runs only inside config.Load, which needs a slug — so on an
// installation with no workspace files a broken defaults.yaml would read
// as healthy right up until the first open.
//
// Problems found here are a warning, not a failure: defaults are the
// bottom layer and a workspace layer may legitimately override anything
// they state. An absent defaults.yaml is legal outright.
func (r *Runner) defaultsItem() Item {
	item := Item{Subject: "defaults", Status: StatusOK}
	path := config.DefaultsPath(r.ConfigRoot)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			item.Detail = "defaults.yaml absent"
			return item
		}
		item.Status = StatusUnknown
		item.Detail = err.Error()
		return item
	}
	if problems := config.ValidateDefaults(r.Defaults); len(problems) > 0 {
		rendered := make([]string, 0, len(problems))
		for _, p := range problems {
			rendered = append(rendered, p.String())
		}
		item.Status = StatusWarn
		item.Detail = "read alone, defaults.yaml has problems a workspace layer would have to override: " +
			strings.Join(rendered, "; ")
	}
	return item
}

// oneLine flattens a multi-problem error into a single report line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
}
