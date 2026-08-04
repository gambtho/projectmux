package config

import "fmt"

// mergeLayers applies over on top of base. Later layers win; absent keys
// inherit.
func mergeLayers(base, over Layer) (Layer, error) {
	out := base
	if over.Version != nil {
		out.Version = over.Version
	}
	if over.RepositoryRoots != nil {
		out.RepositoryRoots = over.RepositoryRoots
	}
	if over.Autostart != nil {
		out.Autostart = over.Autostart
	}
	out.DevContainer = mergeDevContainer(base.DevContainer, over.DevContainer)
	out.Environment = mergeEnvironment(base.Environment, over.Environment)

	windows, err := mergeWindows(base.Windows, over.Windows)
	if err != nil {
		return Layer{}, err
	}
	out.Windows = windows
	return out, nil
}

func mergeDevContainer(base, over *DevContainerLayer) *DevContainerLayer {
	if over == nil {
		return base
	}
	if base == nil {
		copied := *over
		return &copied
	}
	out := *base
	if over.Enabled != nil {
		out.Enabled = over.Enabled
	}
	if over.Config != nil {
		out.Config = over.Config
	}
	if over.StartTimeout != nil {
		out.StartTimeout = over.StartTimeout
	}
	return &out
}

func mergeEnvironment(base, over map[string]string) map[string]string {
	if base == nil && over == nil {
		return nil
	}
	out := make(map[string]string, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

// mergeWindows merges by name rather than by array position, so a workspace can
// adjust one window without restating the whole layout. Windows the base
// already names keep their base position; windows only the overlay names are
// appended in the overlay's own order.
func mergeWindows(base, over []WindowLayer) ([]WindowLayer, error) {
	if err := rejectDuplicates(base); err != nil {
		return nil, err
	}
	if err := rejectDuplicates(over); err != nil {
		return nil, err
	}

	// Match on the whole name. Substring matching would swallow a new window
	// whose name is a substring of an existing one ("age" into "agent-1").
	index := make(map[string]int, len(base))
	out := make([]WindowLayer, len(base))
	for i, w := range base {
		out[i] = w
		index[w.Name] = i
	}
	for _, w := range over {
		if i, ok := index[w.Name]; ok {
			out[i] = mergeWindow(out[i], w)
			continue
		}
		index[w.Name] = len(out)
		out = append(out, w)
	}
	return out, nil
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
