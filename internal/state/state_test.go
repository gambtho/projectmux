package state

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestRootPrefersExplicitOverrideThenXDG(t *testing.T) {
	t.Setenv("PROJECTMUX_STATE_ROOT", "/tmp/override")
	t.Setenv("XDG_STATE_HOME", "/tmp/xdg")
	got, err := Root()
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	if got != "/tmp/override" {
		t.Errorf("Root = %q, want the explicit override", got)
	}

	t.Setenv("PROJECTMUX_STATE_ROOT", "")
	got, err = Root()
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	if got != filepath.Join("/tmp/xdg", "projectmux") {
		t.Errorf("Root = %q, want the XDG state home", got)
	}

	t.Setenv("XDG_STATE_HOME", "")
	got, err = Root()
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	if !strings.HasSuffix(got, filepath.Join(".local", "state", "projectmux")) {
		t.Errorf("Root = %q, want ~/.local/state/projectmux", got)
	}
}
