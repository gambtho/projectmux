package container

import (
	"strings"
	"testing"

	"github.com/gambtho/projectmux/internal/state"
)

func execBinding() state.ContainerBinding {
	return state.ContainerBinding{
		Kind:          "devcontainer",
		ContainerID:   "c0ffee",
		ContainerUser: "vscode",
		Workdir:       "/workspaces/proj",
		Health:        state.HealthPresent,
	}
}

func TestExecCommandRendersFullRequest(t *testing.T) {
	a := &Adapter{}
	got := a.ExecCommand(execBinding(), "make watch", "sub/dir",
		map[string]string{"B_KEY": "2", "A_KEY": "1"})
	want := `docker exec -i -t -u 'vscode' -w '/workspaces/proj/sub/dir' ` +
		`-e 'A_KEY=1' -e 'B_KEY=2' 'c0ffee' sh -lc 'make watch'`
	if got != want {
		t.Errorf("ExecCommand =\n%q\nwant\n%q", got, want)
	}
}

func TestExecCommandShellWindowAndOmittedUser(t *testing.T) {
	a := &Adapter{}
	b := execBinding()
	b.ContainerUser = ""
	got := a.ExecCommand(b, "", "", nil)
	want := `docker exec -i -t -w '/workspaces/proj' 'c0ffee' sh -l`
	if got != want {
		t.Errorf("ExecCommand = %q, want %q", got, want)
	}
	if strings.Contains(got, "-u") {
		t.Error("an empty user must omit -u")
	}
}

func TestExecCommandQuotesHostileValues(t *testing.T) {
	a := &Adapter{}
	got := a.ExecCommand(execBinding(), `echo "$HOME"; touch /tmp/pwned'`, "",
		map[string]string{"EV": `x'; rm -rf / #`})
	for _, want := range []string{
		`sh -lc 'echo "$HOME"; touch /tmp/pwned'\'''`,
		`-e 'EV=x'\''; rm -rf / #'`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ExecCommand %q\nmissing %q", got, want)
		}
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"plain":   "'plain'",
		"a b":     "'a b'",
		"it's":    `'it'\''s'`,
		"$(evil)": "'$(evil)'",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}
