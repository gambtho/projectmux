package config

import (
	"fmt"
	"slices"
)

// mergeLayers applies over on top of base. Later layers win; absent keys
// inherit.
//
// Every branch that takes a value from over also records where it came from,
// so a problem found after merging can name the file and line that set it.
// The two must stay together: a merge rule and its attribution that drift
// apart produce reports pointing at the wrong file, which is worse than no
// report at all.
func mergeLayers(base Merged, over Source) (Merged, error) {
	out := base
	layer := over.Layer
	if over.File != "" && !slices.Contains(out.files, over.File) {
		out.files = append(slices.Clone(out.files), over.File)
	}

	if layer.Version != nil {
		out.Layer.Version = layer.Version
		out.credit(over, "version")
	}
	if layer.RepositoryRoots != nil {
		out.Layer.RepositoryRoots = layer.RepositoryRoots
		out.credit(over, "repository_roots")
	}
	if layer.Autostart != nil {
		out.Layer.Autostart = layer.Autostart
		out.credit(over, "autostart")
	}
	out.Layer.DevContainer = mergeDevContainer(&out, over, base.Layer.DevContainer, layer.DevContainer)
	out.Layer.Environment = mergeEnvironment(&out, over, base.Layer.Environment, layer.Environment)

	windows, err := mergeWindows(&out, over, base.Layer.Windows, layer.Windows)
	if err != nil {
		return Merged{}, err
	}
	out.Layer.Windows = windows
	return out, nil
}

func mergeDevContainer(out *Merged, over Source, base, layer *DevContainerLayer) *DevContainerLayer {
	if layer == nil {
		return base
	}
	if base == nil {
		copied := *layer
		creditDevContainer(out, over, *layer)
		return &copied
	}
	merged := *base
	if layer.Enabled != nil {
		merged.Enabled = layer.Enabled
	}
	if layer.Config != nil {
		merged.Config = layer.Config
	}
	if layer.StartTimeout != nil {
		merged.StartTimeout = layer.StartTimeout
	}
	creditDevContainer(out, over, *layer)
	return &merged
}

func creditDevContainer(out *Merged, over Source, layer DevContainerLayer) {
	if layer.Enabled != nil {
		out.credit(over, "devcontainer.enabled")
	}
	if layer.Config != nil {
		out.credit(over, "devcontainer.config")
	}
	if layer.StartTimeout != nil {
		out.credit(over, "devcontainer.start_timeout")
	}
}

func mergeEnvironment(out *Merged, over Source, base, layer map[string]string) map[string]string {
	if base == nil && layer == nil {
		return nil
	}
	merged := make(map[string]string, len(base)+len(layer))
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range layer {
		merged[k] = v
		out.credit(over, "environment."+k)
	}
	return merged
}

// mergeWindows merges by name rather than by array position, so a workspace can
// adjust one window without restating the whole layout. Windows the base
// already names keep their base position; windows only the overlay names are
// appended in the overlay's own order.
func mergeWindows(out *Merged, over Source, base, layer []WindowLayer) ([]WindowLayer, error) {
	if err := rejectDuplicates(base); err != nil {
		return nil, err
	}
	if err := rejectDuplicates(layer); err != nil {
		return nil, err
	}

	// Match on the whole name. Substring matching would swallow a new window
	// whose name is a substring of an existing one ("age" into "agent-1").
	index := make(map[string]int, len(base))
	merged := make([]WindowLayer, len(base))
	for i, w := range base {
		merged[i] = w
		index[w.Name] = i
	}
	for _, w := range layer {
		if i, ok := index[w.Name]; ok {
			merged[i] = mergeWindow(merged[i], w)
			creditWindow(out, over, w, false)
			continue
		}
		index[w.Name] = len(merged)
		merged = append(merged, w)
		creditWindow(out, over, w, true)
	}
	return merged, nil
}

// creditWindow attributes the fields this layer stated about one window.
//
// introduced marks a window this layer names for the first time, which also
// makes it the origin of the window itself — the position the fallback ladder
// reports for a problem owning no single field.
func creditWindow(out *Merged, over Source, w WindowLayer, introduced bool) {
	prefix := fmt.Sprintf("windows[%s]", w.Name)
	if introduced {
		out.credit(over, prefix)
	}
	// Mode merges as a unit: naming any of the three replaces all three, so
	// all three are credited here. Crediting them field-wise would blame an
	// earlier layer for a command this one removed.
	//
	// They share one position — the key this layer actually wrote — because
	// the other two names appear nowhere in the file and would resolve to a
	// line of 0, reporting the right file with no position in it.
	if w.setsMode() {
		out.creditAs(
			over.originOf(prefix+"."+modeKey(w)),
			prefix+".agent", prefix+".command", prefix+".shell")
	}
	if w.Cwd != nil {
		out.credit(over, prefix+".cwd")
	}
	if w.Location != nil {
		out.credit(over, prefix+".location")
	}
	if w.Focus != nil {
		out.credit(over, prefix+".focus")
	}
}

// modeKey names the mode key this layer wrote, for attributing the unit.
//
// A layer that wrote more than one is invalid, but merging runs before
// validation, so the order here decides only which of several wrong keys is
// pointed at. Validation reports that case against the window itself.
func modeKey(w WindowLayer) string {
	switch {
	case w.Agent != nil:
		return "agent"
	case w.Command != nil:
		return "command"
	default:
		return "shell"
	}
}

// rejectDuplicates catches a name repeated within one file. Repetition across
// files is the merge working as intended; repetition inside a single file is
// always a mistake.
func rejectDuplicates(windows []WindowLayer) error {
	seen := make(map[string]bool, len(windows))
	for _, w := range windows {
		if seen[w.Name] {
			return invalid(fmt.Sprintf("window %q is defined more than once", w.Name))
		}
		seen[w.Name] = true
	}
	return nil
}

func mergeWindow(base, over WindowLayer) WindowLayer {
	out := base
	if over.setsMode() {
		out.Agent, out.Command, out.Shell = over.Agent, over.Command, over.Shell
	}
	if over.Cwd != nil {
		out.Cwd = over.Cwd
	}
	if over.Location != nil {
		out.Location = over.Location
	}
	if over.Focus != nil {
		out.Focus = over.Focus
	}
	return out
}
