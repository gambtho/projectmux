package config

// normalize resolves the schema defaults from section 6 of the design and
// collapses the layer's pointers into decided values. It never invents a
// value the design leaves conditional: an unset window location stays nil.
func normalize(l Layer) Config {
	cfg := Config{
		Environment: map[string]string{},
		Windows:     []Window{},
		DevContainer: DevContainer{
			Enabled:      "auto",
			StartTimeout: Duration(DefaultStartTimeout),
		},
	}
	if l.Version != nil {
		cfg.Version = *l.Version
	}
	if l.Autostart != nil {
		cfg.Autostart = *l.Autostart
	}
	if dc := l.DevContainer; dc != nil {
		if dc.Enabled != nil {
			cfg.DevContainer.Enabled = string(*dc.Enabled)
		}
		cfg.DevContainer.Config = dc.Config
		if dc.StartTimeout != nil {
			cfg.DevContainer.StartTimeout = *dc.StartTimeout
		}
	}
	for k, v := range l.Environment {
		cfg.Environment[k] = v
	}
	for _, w := range l.Windows {
		window := Window{
			Name:     w.Name,
			Agent:    w.Agent,
			Command:  w.Command,
			Cwd:      w.Cwd,
			Location: w.Location,
		}
		if w.Shell != nil {
			window.Shell = *w.Shell
		}
		if w.Focus != nil {
			window.Focus = *w.Focus
		}
		cfg.Windows = append(cfg.Windows, window)
	}
	return cfg
}
