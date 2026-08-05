package doctor

import (
	"context"

	"github.com/gambtho/projectmux/internal/state"
)

// staleBindings probes each stored container binding once. dockerAbsent
// short-circuits the check: with no client installed every probe would
// fail for the same uninformative reason.
func (r *Runner) staleBindings(ctx context.Context, dockerAbsent bool) Check {
	check := Check{Name: "stale-bindings"}
	if dockerAbsent {
		return verdict(check.Name, StatusUnknown, "docker is not installed")
	}
	if r.Containers == nil {
		return verdict(check.Name, StatusUnknown, "containers are not observable")
	}

	records, err := r.records()
	if err != nil {
		return verdict(check.Name, StatusUnknown, err.Error())
	}
	for _, rec := range records {
		if rec.Container == nil {
			continue
		}
		check.Items = append(check.Items, r.bindingItem(ctx, rec))
	}
	if len(check.Items) == 0 {
		return verdict(check.Name, StatusOK, "no container bindings are recorded")
	}
	return check.aggregate()
}

func (r *Runner) bindingItem(ctx context.Context, rec state.Record) Item {
	item := Item{Subject: rec.Slug}
	obs, err := r.Containers.ProbeContainer(ctx, *rec.Container)
	if err != nil {
		// The daemon may be down or the probe may have timed out;
		// neither establishes that the container is gone.
		item.Status = StatusUnknown
		item.Detail = err.Error()
		return item
	}
	switch obs.Health {
	case state.HealthPresent:
		item.Status = StatusOK
		item.Detail = "container " + rec.Container.ContainerID
	case state.HealthMissing:
		item.Status = StatusWarn
		item.Detail = "container no longer exists: " + rec.Container.ContainerID
	default:
		item.Status = StatusUnknown
		item.Detail = "container health is unknown: " + rec.Container.ContainerID
	}
	return item
}
