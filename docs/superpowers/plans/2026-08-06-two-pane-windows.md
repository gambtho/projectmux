# Two-Pane Windows Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every ProjectMux window renders as two tmux panes by default — the configured primary plus a shell pane in the same directory and location — with an optional `panes` list for opt-out (`panes: []`) and customization.

**Architecture:** The spec is `docs/superpowers/specs/2026-08-06-two-pane-windows-design.md`; read it before starting. The change flows through the existing pipeline: config (`PaneLayer` → normalized `Pane`, default materialized at normalization, unit-merge, validation), controller (`PaneIntent` → `PaneSpec` in `renderWindows`, container panes via `ExecCommand`), CLI wiring (`windowIntents`), and the tmux actuator (`createArgv` gains chained `split-window` segments; focused pane = split without `-d`; ≥2 extra panes get `select-layout even-horizontal`). No observation, reconciliation, stop, attach, or state changes.

**Tech Stack:** Go, yaml.v3, tmux 3.4 (isolated `-L` sockets for integration tests).

## Global Constraints

- Schema version stays `1`; `panes` is optional and every existing document stays valid.
- `panes` lists *additional* panes; the window's own fields keep describing the primary pane. Omitted `panes` normalizes to `[{"name": "shell", "shell": true}]`; explicit `panes: []` normalizes to an empty list.
- `panes` merges **as a unit** across layers (present replaces the whole list; absent inherits), like the window mode trio.
- Panes have **no `location` field**; they inherit the window's resolved location.
- Pane `focus` selects the active pane within its window (at most one per window); it is implemented by omitting `-d` on that pane's `split-window`, never by pane-index targeting (`pane-base-index` is user-configurable).
- Every new tmux argv element passes through `escapeChainArg`.
- Creation stays one chained subprocess. Identity `set-option` calls stay immediately after `new-session`.
- Panes are create-only: no observation, no reconciliation, no drift from a user closing one.
- All existing tests stay green: `go test ./...` from the worktree root.

---

### Task 1: Config schema — `PaneLayer`, `Pane`, normalization default, digest behavior

**Files:**
- Modify: `internal/config/layer.go` (add `PaneLayer`, extend `WindowLayer`)
- Modify: `internal/config/config.go` (add `Pane`, extend `Window`)
- Modify: `internal/config/normalize.go` (normalize panes, materialize the default)
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `config.Pane{Name string; Agent *string; Command *string; Shell bool; Cwd *string; Focus bool}` with JSON tags `name`, `agent`, `command`, `shell`, `cwd`, `focus`; `config.Window` gains `Panes []Pane` (JSON tag `panes`, declared after `Focus`); `config.WindowLayer` gains `Panes *[]PaneLayer` (YAML tag `panes`); `config.PaneLayer{Name string; Agent *string; Command *string; Shell *bool; Cwd *string; Focus *bool}` with matching YAML tags.
- Consumes: nothing new.

- [ ] **Step 1: Write the failing tests**

Append to `internal/config/config_test.go` (match the file's existing helper style; it already loads YAML through `Load` in other tests — these tests exercise `normalize` and `digest` through whatever seam the file already uses for round-trip tests around line 654, where key order is asserted):

```go
func TestNormalizeDefaultsPanes(t *testing.T) {
	shell := true
	cfg := normalize(Layer{Windows: []WindowLayer{{Name: "dev", Shell: &shell}}})
	if len(cfg.Windows) != 1 {
		t.Fatalf("windows = %+v", cfg.Windows)
	}
	panes := cfg.Windows[0].Panes
	if len(panes) != 1 || panes[0].Name != "shell" || !panes[0].Shell {
		t.Errorf("omitted panes should normalize to the default shell pane, got %+v", panes)
	}
}

func TestNormalizeEmptyPanesOptsOut(t *testing.T) {
	shell := true
	empty := []PaneLayer{}
	cfg := normalize(Layer{Windows: []WindowLayer{
		{Name: "dev", Shell: &shell, Panes: &empty},
	}})
	if panes := cfg.Windows[0].Panes; len(panes) != 0 || panes == nil {
		t.Errorf("panes: [] should normalize to an empty non-nil list, got %#v", panes)
	}
}

func TestNormalizeExplicitPanes(t *testing.T) {
	shell := true
	cmd := "tail -f log/dev.log"
	cwd := "services/api"
	declared := []PaneLayer{{Name: "logs", Command: &cmd, Cwd: &cwd}}
	cfg := normalize(Layer{Windows: []WindowLayer{
		{Name: "dev", Shell: &shell, Panes: &declared},
	}})
	panes := cfg.Windows[0].Panes
	if len(panes) != 1 || panes[0].Name != "logs" ||
		panes[0].Command == nil || *panes[0].Command != cmd ||
		panes[0].Cwd == nil || *panes[0].Cwd != cwd {
		t.Errorf("declared panes should pass through, got %+v", panes)
	}
}

func TestDigestPanesBehavior(t *testing.T) {
	shell := true
	empty := []PaneLayer{}
	base := Layer{Windows: []WindowLayer{{Name: "dev", Shell: &shell}}}
	optOut := Layer{Windows: []WindowLayer{{Name: "dev", Shell: &shell, Panes: &empty}}}

	dBase, err := digest(normalize(base))
	if err != nil {
		t.Fatal(err)
	}
	dOptOut, err := digest(normalize(optOut))
	if err != nil {
		t.Fatal(err)
	}
	if dBase == dOptOut {
		t.Error("omitted panes (default pane) and panes: [] must digest differently")
	}

	dAgain, err := digest(normalize(base))
	if err != nil {
		t.Fatal(err)
	}
	if dBase != dAgain {
		t.Error("identical configuration must digest stably")
	}
}

func TestDigestZeroWindowConfigCarriesNoPanes(t *testing.T) {
	// The spec's §3 exception: a workspace with no windows digests an empty
	// list; its implicit shell window (and that window's default pane) is
	// invented at derivation, outside the digest. The canonical JSON must
	// therefore contain no pane content at all.
	cfg := normalize(Layer{})
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "panes") {
		t.Errorf("zero-window config must not encode panes: %s", encoded)
	}
}
```

Add `"encoding/json"` and `"strings"` to the test file's imports if not already present.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config/ -run 'TestNormalize.*Panes|TestDigestPanes|TestDigestZeroWindow' -v`
Expected: compile error — `PaneLayer` and `Panes` are undefined.

- [ ] **Step 3: Implement the types and normalization**

In `internal/config/layer.go`, extend `WindowLayer` and add `PaneLayer` directly below it:

```go
// WindowLayer is the undecided form of Window.
type WindowLayer struct {
	Name     string  `yaml:"name"`
	Agent    *string `yaml:"agent"`
	Command  *string `yaml:"command"`
	Shell    *bool   `yaml:"shell"`
	Cwd      *string `yaml:"cwd"`
	Location *string `yaml:"location"`
	Focus    *bool   `yaml:"focus"`
	// Panes is a pointer to a slice so that an omitted key (inherit or
	// default) is distinguishable from an explicit empty list (opt out of
	// the default pane). It merges as a unit, like the mode trio.
	Panes *[]PaneLayer `yaml:"panes"`
}

// PaneLayer is the undecided form of Pane.
type PaneLayer struct {
	Name    string  `yaml:"name"`
	Agent   *string `yaml:"agent"`
	Command *string `yaml:"command"`
	Shell   *bool   `yaml:"shell"`
	Cwd     *string `yaml:"cwd"`
	Focus   *bool   `yaml:"focus"`
}
```

In `internal/config/config.go`, extend `Window` (the `Panes` field goes last, after `Focus`, so existing JSON field order is undisturbed) and add `Pane` below it:

```go
// Window is one normalized tmux window.
type Window struct {
	Name string `json:"name"`
	// Exactly one of Agent, Command, or Shell describes how the window runs.
	Agent   *string `json:"agent"`
	Command *string `json:"command"`
	Shell   bool    `json:"shell"`
	// Cwd is an optional worktree-relative working directory.
	Cwd *string `json:"cwd"`
	// Location is "host", "container", or nil. It stays nil when omitted: the
	// design's default is "container when one exists", which is conditional on
	// the workspace having a container, and no container binding is in scope
	// here. Collapsing nil to "container" during normalization is what made a
	// plain repository unopenable in the Bash implementation.
	Location *string `json:"location"`
	Focus    bool    `json:"focus"`
	// Panes are the window's additional panes beyond the primary the window
	// fields above describe. Normalization materializes the default shell
	// pane here, so the digested document states the panes that will render;
	// an explicit `panes: []` stays empty (single-pane opt-out).
	Panes []Pane `json:"panes"`
}

// Pane is one normalized additional pane of a window. Panes have no
// location: they inherit the window's resolved location.
type Pane struct {
	Name string `json:"name"`
	// Exactly one of Agent, Command, or Shell describes how the pane runs.
	Agent   *string `json:"agent"`
	Command *string `json:"command"`
	Shell   bool    `json:"shell"`
	// Cwd is an optional worktree-relative working directory; absent means
	// the window's directory.
	Cwd   *string `json:"cwd"`
	Focus bool    `json:"focus"`
}
```

In `internal/config/normalize.go`, inside the `for _, w := range l.Windows` loop, after the existing `if w.Focus != nil { ... }` block and before `cfg.Windows = append(...)` (adjust to the loop's actual shape), add:

```go
		if w.Panes == nil {
			window.Panes = []Pane{{Name: "shell", Shell: true}}
		} else {
			window.Panes = make([]Pane, 0, len(*w.Panes))
			for _, p := range *w.Panes {
				pane := Pane{
					Name:    p.Name,
					Agent:   p.Agent,
					Command: p.Command,
					Cwd:     p.Cwd,
				}
				if p.Shell != nil {
					pane.Shell = *p.Shell
				}
				if p.Focus != nil {
					pane.Focus = *p.Focus
				}
				window.Panes = append(window.Panes, pane)
			}
		}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/config/ -run 'TestNormalize.*Panes|TestDigestPanes|TestDigestZeroWindow' -v`
Expected: PASS.

- [ ] **Step 5: Run the whole config package and fix any digest/golden fallout**

Run: `go test ./internal/config/ -v`
Any test asserting an exact canonical JSON encoding of a config with windows will now expect `"panes":[...]`. Update those expected strings to include the materialized default (`"panes":[{"name":"shell","agent":null,"command":null,"shell":true,"cwd":null,"focus":false}]`). Do not weaken assertions — extend them.

- [ ] **Step 6: Commit**

```bash
git add internal/config/layer.go internal/config/config.go internal/config/normalize.go internal/config/config_test.go
git commit -m "feat(config): add the pane schema and materialize the default shell pane"
```

---

### Task 2: Config merge — `panes` merges as a unit, with attribution

**Files:**
- Modify: `internal/config/merge.go` (`mergeWindow`, `creditWindow`)
- Test: `internal/config/config_test.go` (merge behavior), `internal/config/merge_origin_test.go` (attribution)

**Interfaces:**
- Consumes: `WindowLayer.Panes *[]PaneLayer` from Task 1.
- Produces: merge semantics later tasks rely on — a layer with `Panes != nil` replaces the merged window's whole pane list; `Panes == nil` inherits; origin key `windows[<name>].panes`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/config/config_test.go`. Follow the existing merge tests' construction style (they build `Source{Layer: ..., File: ...}` values and call `mergeLayers`; mirror the nearby tests exactly):

```go
func TestMergePanesAsUnit(t *testing.T) {
	shell := true
	cmd := "make watch"
	basePanes := []PaneLayer{{Name: "watch", Command: &cmd}}
	base := Source{File: "defaults.yaml", Layer: Layer{Windows: []WindowLayer{
		{Name: "dev", Shell: &shell, Panes: &basePanes},
	}}}

	t.Run("absent inherits", func(t *testing.T) {
		over := Source{File: "workspace.yaml", Layer: Layer{Windows: []WindowLayer{
			{Name: "dev"},
		}}}
		m := mergeLayers(mergeLayers(Merged{}, base), over)
		got := m.Layer.Windows[0].Panes
		if got == nil || len(*got) != 1 || (*got)[0].Name != "watch" {
			t.Errorf("absent panes must inherit the base list, got %#v", got)
		}
	})

	t.Run("empty replaces", func(t *testing.T) {
		empty := []PaneLayer{}
		over := Source{File: "workspace.yaml", Layer: Layer{Windows: []WindowLayer{
			{Name: "dev", Panes: &empty},
		}}}
		m := mergeLayers(mergeLayers(Merged{}, base), over)
		got := m.Layer.Windows[0].Panes
		if got == nil || len(*got) != 0 {
			t.Errorf("panes: [] must replace the inherited list, got %#v", got)
		}
	})

	t.Run("stated replaces whole list", func(t *testing.T) {
		other := "htop"
		overPanes := []PaneLayer{{Name: "mon", Command: &other}}
		over := Source{File: "workspace.yaml", Layer: Layer{Windows: []WindowLayer{
			{Name: "dev", Panes: &overPanes},
		}}}
		m := mergeLayers(mergeLayers(Merged{}, base), over)
		got := m.Layer.Windows[0].Panes
		if got == nil || len(*got) != 1 || (*got)[0].Name != "mon" {
			t.Errorf("a stated panes list must replace, not merge, got %#v", got)
		}
	})
}
```

Append to `internal/config/merge_origin_test.go`, following its existing attribution assertions (the file asserts `originOf`-style lookups; mirror the nearest window-field test and assert the origin of `windows[dev].panes` points at the overlay file):

```go
func TestMergeCreditsPanes(t *testing.T) {
	shell := true
	empty := []PaneLayer{}
	base := Source{File: "defaults.yaml", Layer: Layer{Windows: []WindowLayer{
		{Name: "dev", Shell: &shell},
	}}}
	over := Source{File: "workspace.yaml", Layer: Layer{Windows: []WindowLayer{
		{Name: "dev", Panes: &empty},
	}}}
	m := mergeLayers(mergeLayers(Merged{}, base), over)
	origin := m.originOf("windows[dev].panes")
	if origin.File != "workspace.yaml" {
		t.Errorf("windows[dev].panes should be credited to workspace.yaml, got %+v", origin)
	}
}
```

Adjust the origin-lookup call to whatever accessor `merge_origin_test.go` actually uses (`originOf`, `origins[...]`, or a helper) — read the file first and copy its pattern.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config/ -run 'TestMergePanes|TestMergeCreditsPanes' -v`
Expected: FAIL — inherit works by accident of struct copy, but "empty replaces"/"stated replaces" fail because `mergeWindow` never copies `Panes`, and the credit test fails with no origin.

- [ ] **Step 3: Implement the merge and attribution**

In `internal/config/merge.go`, extend `mergeWindow`:

```go
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
	// Panes merges as a unit, like the mode trio: a layer that states the
	// key replaces the whole list. Per-pane merging would make `panes: []`
	// merge nothing and silently inherit, so opting out would be
	// impossible to express.
	if over.Panes != nil {
		out.Panes = over.Panes
	}
	return out
}
```

In `creditWindow` (same file), after the `w.Focus != nil` block:

```go
	if w.Panes != nil {
		out.credit(over, prefix+".panes")
	}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/config/ -run 'TestMergePanes|TestMergeCreditsPanes' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/merge.go internal/config/config_test.go internal/config/merge_origin_test.go
git commit -m "feat(config): merge a window's panes as a unit with origin attribution"
```

---

### Task 3: Config validation — pane names, modes, cwd, focus, duplicates

**Files:**
- Modify: `internal/config/validate.go`
- Test: `internal/config/validate_test.go`

**Interfaces:**
- Consumes: `config.Pane` from Task 1.
- Produces: problems with field paths `windows[<window>].panes[<pane>]` and `windows[<window>].panes[<pane>].<field>`; the rule set later tasks assume config-level rejection for.

- [ ] **Step 1: Write the failing tests**

Append to `internal/config/validate_test.go`, following its existing table/assertion style (read the nearest window-validation test and copy its construction — most build a `Merged` + `Config` pair or go through `Load` with temp files; use the same seam):

```go
func TestValidatePanes(t *testing.T) {
	str := func(s string) *string { return &s }

	cases := []struct {
		name  string
		panes []Pane
		want  string // substring of the expected problem message; "" = valid
	}{
		{"default is valid", []Pane{{Name: "shell", Shell: true}}, ""},
		{"empty is valid", []Pane{}, ""},
		{"no mode", []Pane{{Name: "p1"}},
			`pane "p1" of window "dev" must set exactly one of agent, command, or shell: true`},
		{"two modes", []Pane{{Name: "p1", Agent: str("claude"), Shell: true}},
			`pane "p1" of window "dev" must set exactly one of agent, command, or shell: true`},
		{"empty agent", []Pane{{Name: "p1", Agent: str("  ")}},
			`pane "p1" of window "dev" has an empty agent`},
		{"empty command", []Pane{{Name: "p1", Command: str("")}},
			`pane "p1" of window "dev" has an empty command`},
		{"bad name", []Pane{{Name: "sp ace", Shell: true}},
			`invalid name`},
		{"absolute cwd", []Pane{{Name: "p1", Shell: true, Cwd: str("/etc")}},
			`must be relative to the worktree`},
		{"escaping cwd", []Pane{{Name: "p1", Shell: true, Cwd: str("../out")}},
			`must not escape the worktree`},
		{"duplicate names", []Pane{{Name: "p1", Shell: true}, {Name: "p1", Shell: true}},
			`pane "p1" of window "dev" is defined more than once`},
		{"two focused", []Pane{
			{Name: "p1", Shell: true, Focus: true},
			{Name: "p2", Shell: true, Focus: true}},
			`more than one pane of window "dev" sets focus`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig() // reuse/adapt the file's existing valid-config helper
			cfg.Windows = []Window{{Name: "dev", Shell: true, Panes: tc.panes}}
			problems := validate(Merged{}, cfg)
			if tc.want == "" {
				if len(problems) != 0 {
					t.Fatalf("expected valid, got %v", problems)
				}
				return
			}
			found := false
			for _, p := range problems {
				if strings.Contains(p.Message, tc.want) {
					found = true
				}
			}
			if !found {
				t.Errorf("problems %v\nmissing %q", problems, tc.want)
			}
		})
	}
}
```

If `validate_test.go` has no `validConfig()` helper, build the minimal valid `Config` inline the way its other tests do (version set, positive start timeout). Match message spellings exactly to what Step 3 implements.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config/ -run TestValidatePanes -v`
Expected: FAIL — pane cases produce no problems.

- [ ] **Step 3: Implement pane validation**

In `internal/config/validate.go`, add a call at the end of `validateWindow` (before `return problems`):

```go
	problems = append(problems, validatePanes(m, w)...)
```

Then add below `validateWindow`:

```go
// validatePanes checks one window's additional panes. The rules mirror
// validateWindow where they carry over; there is no numeric-name rule
// because panes are never addressed by name in a tmux target, and no
// location rule because panes inherit the window's location by design.
//
// Duplicates are checked here rather than in duplicateWindows: panes
// merge as a unit, so the merged window's list comes verbatim from one
// layer and survives to the normalized config intact.
func validatePanes(m Merged, w Window) []Problem {
	var problems []Problem
	seen := map[string]bool{}
	reported := map[string]bool{}
	var focused []string
	for _, p := range w.Panes {
		pane := fmt.Sprintf("windows[%s].panes[%s]", w.Name, p.Name)

		if !windowNamePattern.MatchString(p.Name) {
			problems = append(problems, m.problem(pane, fmt.Sprintf(
				"pane %q of window %q has an invalid name; use characters from [A-Za-z0-9._-]",
				p.Name, w.Name)))
		}
		switch {
		case !seen[p.Name]:
			seen[p.Name] = true
		case !reported[p.Name]:
			reported[p.Name] = true
			problems = append(problems, m.problem(pane, fmt.Sprintf(
				"pane %q of window %q is defined more than once", p.Name, w.Name)))
		}

		var modes []string
		if p.Agent != nil {
			modes = append(modes, "agent")
		}
		if p.Command != nil {
			modes = append(modes, "command")
		}
		if p.Shell {
			modes = append(modes, "shell")
		}
		if len(modes) != 1 {
			detail := "it sets none"
			if len(modes) > 1 {
				detail = "it sets " + strings.Join(modes, " and ")
			}
			problems = append(problems, m.problem(pane, fmt.Sprintf(
				"pane %q of window %q must set exactly one of agent, command, or shell: true (%s)",
				p.Name, w.Name, detail)))
		}
		if p.Agent != nil && strings.TrimSpace(*p.Agent) == "" {
			problems = append(problems, m.problem(pane+".agent", fmt.Sprintf(
				"pane %q of window %q has an empty agent", p.Name, w.Name)))
		}
		if p.Command != nil && strings.TrimSpace(*p.Command) == "" {
			problems = append(problems, m.problem(pane+".command", fmt.Sprintf(
				"pane %q of window %q has an empty command", p.Name, w.Name)))
		}
		if p.Cwd != nil {
			if msg := checkContained(fmt.Sprintf("pane %q of window %q cwd", p.Name, w.Name), *p.Cwd); msg != "" {
				problems = append(problems, m.problem(pane+".cwd", msg))
			}
		}
		if p.Focus {
			focused = append(focused, p.Name)
		}
	}
	if len(focused) > 1 {
		problems = append(problems, m.problem(fmt.Sprintf("windows[%s].panes", w.Name), fmt.Sprintf(
			"more than one pane of window %q sets focus: %s",
			w.Name, strings.Join(focused, ", "))))
	}
	return problems
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/config/ -run TestValidatePanes -v`
Expected: PASS. Then run `go test ./internal/config/` — all green (existing valid fixtures gain a default pane, which is valid).

- [ ] **Step 5: Commit**

```bash
git add internal/config/validate.go internal/config/validate_test.go
git commit -m "feat(config): validate pane names, modes, cwds, focus, and duplicates"
```

---

### Task 4: Controller — `PaneIntent`, `PaneSpec`, and pane rendering in `renderWindows`

**Files:**
- Modify: `internal/controller/interfaces.go` (extend `WindowIntent`, `WindowSpec`; add `PaneIntent`, `PaneSpec`)
- Modify: `internal/controller/ensure.go` (`renderWindows`)
- Test: `internal/controller/ensure_test.go`

**Interfaces:**
- Consumes: `ContainerActuator.ExecCommand(binding state.ContainerBinding, command, relDir string, env map[string]string) string` (existing).
- Produces: `controller.PaneIntent{Name string; Command string; RelDir string; Focus bool}` (empty `Command` = shell pane; empty `RelDir` = the window's directory); `controller.PaneSpec{Name string; Command string; Dir string; Focus bool}` (`Dir` absolute); `WindowIntent.Panes []PaneIntent`; `WindowSpec.Panes []PaneSpec`. Task 6's `createArgv` renders `WindowSpec.Panes`; Task 5's wiring fills `WindowIntent.Panes`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/controller/ensure_test.go`. The file already has helpers that run `Ensure` with fakes (see its uses of `controller.WindowIntent` around lines 459–535); these tests target `renderWindows` directly if it is exported to the test (it is in-package for `controller` white-box tests — check the test file's package clause; if it is `package controller_test`, put these in a new in-package file `internal/controller/render_test.go` with `package controller`):

```go
func TestRenderWindowsHostPanes(t *testing.T) {
	d := Desired{Workspace: state.Workspace{Worktree: "/w/slab"}}
	intents := []WindowIntent{{
		Name:    "dev",
		Command: "claude",
		Panes: []PaneIntent{
			{Name: "shell"},
			{Name: "logs", Command: "tail -f dev.log", RelDir: "services/api", Focus: true},
		},
	}}
	specs, err := renderWindows(intents, d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	panes := specs[0].Panes
	if len(panes) != 2 {
		t.Fatalf("panes = %+v", panes)
	}
	if panes[0].Dir != "/w/slab" || panes[0].Command != "" {
		t.Errorf("default pane should be a shell in the window's dir, got %+v", panes[0])
	}
	if panes[1].Dir != "/w/slab/services/api" || !panes[1].Focus {
		t.Errorf("explicit pane cwd/focus not rendered, got %+v", panes[1])
	}
}

func TestRenderWindowsPaneInheritsWindowDir(t *testing.T) {
	d := Desired{Workspace: state.Workspace{Worktree: "/w/slab"}}
	intents := []WindowIntent{{
		Name:   "api",
		RelDir: "services/api",
		Panes:  []PaneIntent{{Name: "shell"}},
	}}
	specs, err := renderWindows(intents, d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := specs[0].Panes[0].Dir; got != "/w/slab/services/api" {
		t.Errorf("a pane without cwd must inherit the window's directory, got %q", got)
	}
}

func TestRenderWindowsContainerPanes(t *testing.T) {
	d := Desired{Workspace: state.Workspace{Worktree: "/w/slab"},
		Config: config.Config{Environment: map[string]string{"K": "v"}}}
	obs := &ContainerObservation{Kind: "devcontainer", ContainerID: "abc",
		ContainerUser: "dev", Workdir: "/workspaces/slab"}
	act := &fakeContainerActuator{} // reuse the file's existing fake
	intents := []WindowIntent{{
		Name:     "dev",
		Command:  "claude",
		Location: WindowContainer,
		Panes:    []PaneIntent{{Name: "shell", RelDir: "sub"}},
	}}
	specs, err := renderWindows(intents, d, obs, act)
	if err != nil {
		t.Fatal(err)
	}
	pane := specs[0].Panes[0]
	if pane.Dir != "/w/slab" {
		t.Errorf("container pane host dir should be the worktree, got %q", pane.Dir)
	}
	want := act.ExecCommand(state.ContainerBinding{
		Kind: "devcontainer", ContainerID: "abc",
		ContainerUser: "dev", Workdir: "/workspaces/slab",
	}, "", "sub", d.Config.Environment)
	if pane.Command != want {
		t.Errorf("container pane command = %q, want the exec rendering %q", pane.Command, want)
	}
}
```

Adapt the fake actuator name and the `Desired`/`state.Workspace` construction to what `ensure_test.go` actually uses — read its existing container-window test first and mirror it. The assertions to preserve exactly: host pane dir inheritance, pane `RelDir` joining, container pane `Dir` = worktree, container pane `Command` = `ExecCommand` with the pane's own relDir and empty command for a shell pane.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/controller/ -run TestRenderWindows -v`
Expected: compile error — `PaneIntent`/`Panes` undefined.

- [ ] **Step 3: Implement the types and rendering**

In `internal/controller/interfaces.go`:

```go
// WindowIntent is a window as configuration describes it, before the
// container binding is known. Ensure renders intents into concrete
// WindowSpecs after its container phase.
type WindowIntent struct {
	Name     string
	Command  string // empty => shell window
	RelDir   string // config cwd, relative; "" => workspace root
	Focus    bool
	Location WindowLocation
	Panes    []PaneIntent
}

// PaneIntent is one additional pane as configuration describes it.
// Panes have no location of their own: they inherit the window's.
type PaneIntent struct {
	Name    string
	Command string // empty => shell pane
	RelDir  string // config cwd, relative; "" => the window's directory
	Focus   bool
}
```

```go
// WindowSpec is one window the actuator creates. An empty Command means
// the default shell; Dir is absolute (derivation resolves relative
// cwds against the worktree).
type WindowSpec struct {
	Name    string
	Command string
	Dir     string
	Focus   bool
	Panes   []PaneSpec
}

// PaneSpec is one additional pane the actuator creates alongside the
// window's primary pane. An empty Command means the default shell; Dir
// is absolute. Focus marks the pane left active within its window.
type PaneSpec struct {
	Name    string
	Command string
	Dir     string
	Focus   bool
}
```

In `internal/controller/ensure.go`, rework `renderWindows` so both branches render panes. Replace the function body's two `specs = append(...)` sites with versions that build panes first:

```go
func renderWindows(intents []WindowIntent, d Desired, container *ContainerObservation, act ContainerActuator) ([]WindowSpec, error) {
	specs := make([]WindowSpec, 0, len(intents))
	for _, in := range intents {
		inContainer := false
		switch in.Location {
		case WindowContainer:
			if container == nil {
				return nil, &ContainerWindowError{Window: in.Name}
			}
			inContainer = true
		case WindowAuto:
			inContainer = container != nil
		}
		if inContainer {
			if act == nil {
				return nil, ErrContainerActionUnsupported
			}
			binding := state.ContainerBinding{
				Kind:          container.Kind,
				ContainerID:   container.ContainerID,
				ContainerUser: container.ContainerUser,
				Workdir:       container.Workdir,
			}
			panes := make([]PaneSpec, 0, len(in.Panes))
			for _, p := range in.Panes {
				// A pane inherits the window's directory unless it sets
				// its own; inside the container that is the exec relDir,
				// while the host-side -c stays the worktree, matching the
				// window itself.
				relDir := in.RelDir
				if p.RelDir != "" {
					relDir = p.RelDir
				}
				panes = append(panes, PaneSpec{
					Name:    p.Name,
					Command: act.ExecCommand(binding, p.Command, relDir, d.Config.Environment),
					Dir:     d.Workspace.Worktree,
					Focus:   p.Focus,
				})
			}
			specs = append(specs, WindowSpec{
				Name:    in.Name,
				Command: act.ExecCommand(binding, in.Command, in.RelDir, d.Config.Environment),
				Dir:     d.Workspace.Worktree,
				Focus:   in.Focus,
				Panes:   panes,
			})
			continue
		}
		dir := d.Workspace.Worktree
		if in.RelDir != "" {
			dir = filepath.Join(d.Workspace.Worktree, in.RelDir)
		}
		panes := make([]PaneSpec, 0, len(in.Panes))
		for _, p := range in.Panes {
			paneDir := dir
			if p.RelDir != "" {
				paneDir = filepath.Join(d.Workspace.Worktree, p.RelDir)
			}
			panes = append(panes, PaneSpec{
				Name: p.Name, Command: p.Command, Dir: paneDir, Focus: p.Focus,
			})
		}
		specs = append(specs, WindowSpec{
			Name: in.Name, Command: in.Command, Dir: dir, Focus: in.Focus, Panes: panes,
		})
	}
	return specs, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/controller/ -run TestRenderWindows -v` then `go test ./internal/controller/`
Expected: PASS; existing controller tests unaffected (intents without panes render empty pane lists).

- [ ] **Step 5: Commit**

```bash
git add internal/controller/interfaces.go internal/controller/ensure.go internal/controller/ensure_test.go
git commit -m "feat(controller): render pane intents into pane specs, container exec included"
```

---

### Task 5: CLI wiring — pane intents from normalized config

**Files:**
- Modify: `internal/cli/wiring.go` (`windowIntents`)
- Test: `internal/cli/wiring_test.go`

**Interfaces:**
- Consumes: `config.Window.Panes []config.Pane` (Task 1), `controller.PaneIntent` (Task 4).
- Produces: `windowIntents` output that carries panes; the implicit shell window (zero-window config) also carries the default shell pane — the spec's §3 derivation-time exception.

- [ ] **Step 1: Write the failing tests**

Append to `internal/cli/wiring_test.go`, matching its existing `windowIntents` tests:

```go
func TestWindowIntentsCarryPanes(t *testing.T) {
	agent := "claude"
	cmd := "tail -f dev.log"
	cwd := "services/api"
	cfg := config.Config{Windows: []config.Window{{
		Name:  "dev",
		Agent: &agent,
		Panes: []config.Pane{
			{Name: "shell", Shell: true},
			{Name: "logs", Command: &cmd, Cwd: &cwd, Focus: true},
		},
	}}}
	intents := windowIntents(cfg)
	panes := intents[0].Panes
	if len(panes) != 2 {
		t.Fatalf("panes = %+v", panes)
	}
	if panes[0].Command != "" || panes[0].Name != "shell" {
		t.Errorf("shell pane should map to an empty command, got %+v", panes[0])
	}
	if panes[1].Command != cmd || panes[1].RelDir != cwd || !panes[1].Focus {
		t.Errorf("command pane not mapped, got %+v", panes[1])
	}
}

func TestWindowIntentsImplicitWindowGetsDefaultPane(t *testing.T) {
	// Spec §3 exception: the implicit window lives outside the digest, so
	// its default pane is supplied here, at derivation.
	intents := windowIntents(config.Config{})
	if len(intents) != 1 || intents[0].Name != "shell" {
		t.Fatalf("intents = %+v", intents)
	}
	panes := intents[0].Panes
	if len(panes) != 1 || panes[0].Name != "shell" || panes[0].Command != "" {
		t.Errorf("implicit window should carry the default shell pane, got %+v", panes)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/cli/ -run TestWindowIntents -v`
Expected: FAIL — no panes on the intents.

- [ ] **Step 3: Implement the mapping**

In `internal/cli/wiring.go`, replace `windowIntents`:

```go
// windowIntents derives the actuator window intents purely from merged
// configuration: implicit shell window when none is configured; the
// location tri-state is resolved against the binding inside Ensure.
// The implicit window also gets the default shell pane here — it lives
// outside the digested config, so its pane arrives at derivation too
// (the two-pane spec's §3 exception).
func windowIntents(cfg config.Config) []controller.WindowIntent {
	if len(cfg.Windows) == 0 {
		return []controller.WindowIntent{{
			Name:  "shell",
			Panes: []controller.PaneIntent{{Name: "shell"}},
		}}
	}
	intents := make([]controller.WindowIntent, 0, len(cfg.Windows))
	for _, w := range cfg.Windows {
		in := controller.WindowIntent{Name: w.Name, Focus: w.Focus}
		switch {
		case w.Agent != nil:
			in.Command = *w.Agent
		case w.Command != nil:
			in.Command = *w.Command
		}
		if w.Cwd != nil {
			in.RelDir = *w.Cwd
		}
		if w.Location != nil {
			in.Location = controller.WindowLocation(*w.Location)
		}
		in.Panes = make([]controller.PaneIntent, 0, len(w.Panes))
		for _, p := range w.Panes {
			pane := controller.PaneIntent{Name: p.Name, Focus: p.Focus}
			switch {
			case p.Agent != nil:
				pane.Command = *p.Agent
			case p.Command != nil:
				pane.Command = *p.Command
			}
			if p.Cwd != nil {
				pane.RelDir = *p.Cwd
			}
			in.Panes = append(in.Panes, pane)
		}
		intents = append(intents, in)
	}
	return intents
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/cli/ -run TestWindowIntents -v` then `go test ./internal/cli/`
Expected: PASS. If any CLI JSON-envelope golden test asserts a config encoding, update it for the `panes` field (extend, don't weaken).

- [ ] **Step 5: Commit**

```bash
git add internal/cli/wiring.go internal/cli/wiring_test.go
git commit -m "feat(cli): carry pane intents, default pane on the implicit window"
```

---

### Task 6: tmux actuator — chained `split-window` segments in `createArgv`

**Files:**
- Modify: `internal/tmux/actuate.go` (`createArgv`)
- Test: `internal/tmux/actuate_test.go`

**Interfaces:**
- Consumes: `controller.WindowSpec.Panes []controller.PaneSpec` (Task 4), existing `escapeChainArg`, `envArgs`, `windowDir`.
- Produces: the final argv contract the integration tests (Task 7) exercise.

**Rendering contract (spec §4, verified on tmux 3.4):** after all window-creation segments (identity `set-option`s stay immediately after `new-session`), one `split-window` segment per pane per window, in window order:
`; split-window -h [-d] -t <session>:<window> -c <dir> [-e K=V]... [command]` — `-d` omitted on exactly the pane with `Focus: true` (that is the focus mechanism; never a pane-index target); then `; select-layout -t <session>:<window> even-horizontal` for windows with ≥2 panes; the existing `select-window` focus loop stays last.

- [ ] **Step 1: Write the failing tests**

Append to `internal/tmux/actuate_test.go`:

```go
func TestCreateArgvSplitsPanes(t *testing.T) {
	spec := actuateSpec()
	spec.Windows[0].Panes = []controller.PaneSpec{{Name: "shell", Dir: "/w/slab"}}
	spec.Windows[1].Panes = []controller.PaneSpec{{Name: "logs", Command: "tail -f d.log", Dir: "/w/slab/sub"}}
	argv := createArgv(spec)
	joined := strings.Join(argv, " ")

	for _, want := range []string{
		"; split-window -h -d -t slab:agent-1 -c /w/slab -e A_KEY=1 -e B_KEY=2",
		"; split-window -h -d -t slab:shell -c /w/slab/sub -e A_KEY=1 -e B_KEY=2 tail -f d.log",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv %q\nmissing %q", joined, want)
		}
	}
	// Identity keys must come before any split: a partial creation stays
	// identity-tagged and recoverable (spec §4, partial failure).
	if strings.Index(joined, "@dev_worktree") > strings.Index(joined, "split-window") {
		t.Errorf("identity set-options must precede splits: %q", joined)
	}
	if strings.Contains(joined, "select-layout") {
		t.Errorf("single extra pane needs no select-layout: %q", joined)
	}
}

func TestCreateArgvFocusedPaneOmitsDetach(t *testing.T) {
	spec := actuateSpec()
	spec.Windows[0].Panes = []controller.PaneSpec{
		{Name: "shell", Dir: "/w/slab", Focus: true},
	}
	argv := createArgv(spec)
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "; split-window -h -t slab:agent-1 -c /w/slab") {
		t.Errorf("focused pane must split without -d: %q", joined)
	}
	if strings.Contains(joined, "; split-window -h -d -t slab:agent-1") {
		t.Errorf("focused pane must not carry -d: %q", joined)
	}
}

func TestCreateArgvManyPanesEqualize(t *testing.T) {
	spec := actuateSpec()
	spec.Windows[0].Panes = []controller.PaneSpec{
		{Name: "a", Dir: "/w/slab"},
		{Name: "b", Dir: "/w/slab"},
	}
	argv := createArgv(spec)
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "; select-layout -t slab:agent-1 even-horizontal") {
		t.Errorf("two extra panes need even-horizontal: %q", joined)
	}
	if strings.Count(joined, "select-layout") != 1 {
		t.Errorf("select-layout should appear exactly once: %q", joined)
	}
}

func TestCreateArgvPaneEscaping(t *testing.T) {
	// escapeChainArg escapes only a TRAILING ";" (tmux's chain parser
	// inspects the last character of each argv element), so every
	// adversarial value here ends in ";". The composite -t target ends in
	// the window name, which is why the window name carries the ";".
	spec := actuateSpec()
	spec.Windows[0].Name = "agent;"
	spec.Windows[0].Panes = []controller.PaneSpec{
		{Name: "p", Command: "run;", Dir: "/w/dir;"},
	}
	argv := createArgv(spec)
	joined := strings.Join(argv, " ")
	for _, want := range []string{
		`split-window -h -d -t slab:agent\;`,
		`-c /w/dir\;`,
		`run\;`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv %q\nmissing escaped %q", joined, want)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/tmux/ -run TestCreateArgv -v`
Expected: new tests FAIL (no split-window segments); existing `TestCreateArgvShape` still passes.

- [ ] **Step 3: Implement the split rendering**

In `internal/tmux/actuate.go`, extend `createArgv`: after the `for _, w := range spec.Windows[1:]` loop and **before** the focus (`select-window`) loop, insert:

```go
	// Panes split after every window exists; identity keys are already set,
	// so a mid-chain failure leaves an identity-tagged, recoverable session
	// (two-pane spec §4). The focused pane's split omits -d — split-window
	// without -d makes the new pane active — because a pane-index target
	// would depend on the user's pane-base-index option.
	for _, w := range spec.Windows {
		winTarget := escapeChainArg(spec.Name + ":" + w.Name)
		for _, p := range w.Panes {
			argv = append(argv, ";", "split-window", "-h")
			if !p.Focus {
				argv = append(argv, "-d")
			}
			argv = append(argv, "-t", winTarget, "-c", escapeChainArg(p.Dir))
			argv = append(argv, envArgs(spec.Env)...)
			if p.Command != "" {
				argv = append(argv, escapeChainArg(p.Command))
			}
		}
		if len(w.Panes) >= 2 {
			argv = append(argv, ";", "select-layout", "-t", winTarget, "even-horizontal")
		}
	}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/tmux/ -run TestCreateArgv -v` then `go test ./internal/tmux/`
Expected: PASS, existing tests green (specs without panes emit no new segments).

- [ ] **Step 5: Commit**

```bash
git add internal/tmux/actuate.go internal/tmux/actuate_test.go
git commit -m "feat(tmux): chain split-window segments for pane specs"
```

---

### Task 7: tmux integration tests — real panes on an isolated socket

**Files:**
- Modify: `internal/tmux/integration_test.go`

**Interfaces:**
- Consumes: `(*Client).CreateSession` with a pane-carrying `SessionSpec` (Tasks 4+6); the file's `isolatedSocket` and `tmuxOK` helpers.
- Produces: end-to-end proof of pane count, cwd, env, and active pane on tmux 3.4.

- [ ] **Step 1: Write the integration test**

Append to `internal/tmux/integration_test.go`:

```go
func TestIntegrationCreateSessionWithPanes(t *testing.T) {
	socket := isolatedSocket(t, "panes")
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	spec := controller.SessionSpec{
		Name:        "twopane",
		WorkspaceID: "w1",
		Slug:        "proj",
		Worktree:    dir,
		Env:         map[string]string{"PANE_PROBE": "yes"},
		Windows: []controller.WindowSpec{
			{Name: "dev", Dir: dir, Focus: true, Panes: []controller.PaneSpec{
				{Name: "shell", Dir: sub, Focus: true},
			}},
			{Name: "solo", Dir: dir},
		},
	}
	if err := (&Client{Socket: socket}).CreateSession(context.Background(), spec); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	panes := func(window string) []string {
		out, err := exec.Command("tmux", "-L", socket, "list-panes", "-t",
			"twopane:"+window, "-F",
			"#{pane_current_path}|#{pane_active}").CombinedOutput()
		if err != nil {
			t.Fatalf("list-panes %s: %v\n%s", window, err, out)
		}
		return strings.Split(strings.TrimSpace(string(out)), "\n")
	}

	dev := panes("dev")
	if len(dev) != 2 {
		t.Fatalf("dev panes = %v, want 2", dev)
	}
	if dev[0] != dir+"|0" {
		t.Errorf("primary pane = %q, want %q (unfocused, worktree cwd)", dev[0], dir+"|0")
	}
	if dev[1] != sub+"|1" {
		t.Errorf("split pane = %q, want %q (focused, sub cwd)", dev[1], sub+"|1")
	}
	if solo := panes("solo"); len(solo) != 1 {
		t.Errorf("solo panes = %v, want 1 (empty pane list)", solo)
	}

	// The configured environment must reach the split pane via -e.
	out, err := exec.Command("tmux", "-L", socket, "show-environment", "-t",
		"twopane", "PANE_PROBE").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "PANE_PROBE=yes") {
		t.Errorf("session environment missing PANE_PROBE: %v %s", err, out)
	}
}
```

Note the env assertion uses the session environment (`show-environment`), matching how existing tests verify `-e`; the spec's manual transcripts already proved the value is visible inside the split pane's shell. Add `"path/filepath"` and `"strings"` to the imports if missing.

- [ ] **Step 2: Run the integration test**

Run: `go test ./internal/tmux/ -run TestIntegrationCreateSessionWithPanes -v`
Expected: PASS on a machine with tmux; the test skips where tmux is absent. If pane order assertions fail because `list-panes` orders differently, key the assertions on the cwd value rather than position — but verify on the real output first, do not guess.

- [ ] **Step 3: Run the whole tmux package**

Run: `go test ./internal/tmux/ -v`
Expected: all green.

- [ ] **Step 4: Commit**

```bash
git add internal/tmux/integration_test.go
git commit -m "test(tmux): prove pane count, cwd, focus, and env on an isolated socket"
```

---

### Task 8: Partial-failure pin — a failed creation stays recoverable

**Files:**
- Modify: `internal/controller/ensure_test.go`

**Interfaces:**
- Consumes: the file's existing fakes (store, observer, actuator) and `Ensure` harness.
- Produces: a pinned regression test for spec §4's partial-failure semantics.

- [ ] **Step 1: Check existing coverage, then write the missing half**

Read `ensure_test.go`'s creation-failure tests first. The likely-covered half: `CreateSession` returning an error makes `Ensure` fail and record the failure. The half to pin (add it if absent): the **next** ensure, observing the identity-tagged partial session the failed chain left behind, reports `EnsureAlreadyRunning` with `Drifted: true` — accepted, not repaired, recoverable via stop.

```go
func TestEnsureAcceptsPartialSessionAfterFailedCreation(t *testing.T) {
	// A failed chained creation aborts mid-chain but leaves the session
	// alive with its identity keys set (they come right after
	// new-session). The next ensure must accept it as already-running
	// with the drift flag set — never repair or recreate (spec §4).
	r := newEnsureRig(t) // reuse the file's existing rig/fixture helper
	r.actuator.createErr = fmt.Errorf("tmux new-session exited 1: boom")

	_, err := r.controller.Ensure(context.Background(), r.desired,
		[]controller.WindowIntent{{Name: "shell"}}, r.lockDir, time.Second)
	if err == nil {
		t.Fatal("first ensure should fail with the actuator error")
	}

	// The chain died after identity was set: simulate the surviving
	// partial session in the observer.
	r.actuator.createErr = nil
	r.observer.sessions = []controller.LiveSession{{
		ID: "$7", Name: r.desired.Workspace.Slug,
		WorkspaceID: r.desired.Workspace.ID,
		Slug:        r.desired.Workspace.Slug,
		Worktree:    r.desired.Workspace.Worktree,
	}}

	res, err := r.controller.Ensure(context.Background(), r.desired,
		[]controller.WindowIntent{{Name: "shell"}}, r.lockDir, time.Second)
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if res.Action != controller.EnsureAlreadyRunning {
		t.Errorf("action = %v, want already-running (accepted, not repaired)", res.Action)
	}
	if !res.Drifted {
		t.Error("a partial session with no committed digest must report drift")
	}
}
```

Adapt the rig construction (`newEnsureRig`, fake field names, `Desired` shape) to the file's actual helpers — read them first; the semantics to preserve are exactly the two assertions.

- [ ] **Step 2: Run it**

Run: `go test ./internal/controller/ -run TestEnsureAcceptsPartialSession -v`
Expected: PASS if the plumbing already behaves per spec (this is a pin, not a change); investigate any failure — it would be a real gap between spec and code.

- [ ] **Step 3: Commit**

```bash
git add internal/controller/ensure_test.go
git commit -m "test(controller): pin the partial-creation recovery semantics"
```

---

### Task 9: Documentation — README and design.md

**Files:**
- Modify: `README.md` (the quick-start YAML and nearby prose)
- Modify: `docs/design.md` (the `windows` schema bullet near line 179)

- [ ] **Step 1: Update the schema documentation**

In `docs/design.md`, extend the `windows` schema bullet (around line 179) with a `panes` sub-bullet:

```markdown
- `windows[].panes`: optional list of *additional* panes beyond the primary
  pane the window's own fields describe. Each entry mirrors the window
  fields minus `location` (panes inherit the window's resolved location):
  `name`, exactly one of `agent`/`command`/`shell: true`, optional `cwd`,
  optional `focus` (at most one per window; the primary pane is active by
  default). Omitted, it defaults to a single shell pane in the window's
  directory — every window is two panes by default. `panes: []` opts a
  window back to single-pane. Across config layers, `panes` merges as a
  unit: a layer that states it replaces the whole list.
```

In `README.md`, after the quick-start YAML block, add one sentence of prose (match the surrounding voice):

```markdown
Every window opens with a second shell pane beside it by default — same
directory, same container if the window runs in one. Add `panes: []` to a
window to keep it single-pane, or declare your own `panes` list.
```

- [ ] **Step 2: Verify the full suite and docs build**

Run: `go test ./...` and `gofmt -l internal/` (expect no output) and `go vet ./...`
Expected: everything green.

- [ ] **Step 3: Commit**

```bash
git add README.md docs/design.md
git commit -m "docs: document two-pane default windows and the panes schema"
```
