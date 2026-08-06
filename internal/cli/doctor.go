package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/doctor"
	"github.com/gambtho/projectmux/internal/state"
)

const doctorHelp = `usage: projectmux doctor [--json] [--compact]

Diagnose the installation: dependencies, configuration, the state
database, orphaned sessions, and stale container bindings. Doctor only
reports — it never starts, kills, migrates, or repairs anything.

  --json     emit the versioned JSON envelope instead of human-readable text
  --compact  emit the JSON on a single line (implies --json)
`

// doctorEnvelope is the versioned JSON structure for projectmux doctor.
// Checks is always the full fixed-order list: a check that could not run
// reports "unknown" in place rather than disappearing.
type doctorEnvelope struct {
	SchemaVersion int           `json:"schema_version"`
	Checks        []doctorCheck `json:"checks"`
}

type doctorCheck struct {
	Name   string       `json:"name"`
	Status string       `json:"status"`
	Detail string       `json:"detail,omitempty"`
	Items  []doctorItem `json:"items"`
}

type doctorItem struct {
	Subject string `json:"subject"`
	Status  string `json:"status"`
	Detail  string `json:"detail,omitempty"`
}

func runDoctor(ctx context.Context, args []string, stdout io.Writer) error {
	fs := newFlagSet("doctor")
	asJSON := fs.Bool("json", false, "emit the versioned JSON envelope")
	compact := fs.Bool("compact", false, "emit the JSON on a single line")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, doctorHelp)
			return nil
		}
		return usagef("doctor: %s", err)
	}
	if fs.NArg() > 0 {
		// Doctor diagnoses the whole installation; there is no
		// workspace to scope it to.
		return usagef("doctor: unexpected argument %q", fs.Arg(0))
	}
	if *compact {
		*asJSON = true
	}

	env, err := buildDoctor(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(stdout, env, *compact)
	}
	return writeDoctorHuman(stdout, env)
}

// buildDoctor assembles the runner and renders its report. Only an
// undeterminable config or state root fails the command: everything else
// the diagnosis can find is report content, so doctor exits 0 whenever
// it produced a report.
func buildDoctor(ctx context.Context) (doctorEnvelope, error) {
	configRoot, err := config.Root()
	if err != nil {
		return doctorEnvelope{}, err
	}
	stateRoot, err := state.Root()
	if err != nil {
		return doctorEnvelope{}, err
	}

	// A defaults layer that will not load is a finding, not a failure:
	// the configuration check reports it and the other four still run.
	defaults, defaultsErr := config.LoadDefaults(configRoot)

	db, store, closeDB := inspectDatabase(stateRoot)
	defer closeDB()

	runner := &doctor.Runner{
		ConfigRoot:  configRoot,
		Defaults:    defaults,
		DefaultsErr: defaultsErr,
		DB:          db,
		Store:       store,
		Sessions:    sessionListerFunc(liveSessions),
		Containers:  newContainerObserver(),
		Versions:    newVersionRunner(),
	}
	return doctorEnvelopeFrom(runner.Diagnose(ctx)), nil
}

func doctorEnvelopeFrom(report doctor.Report) doctorEnvelope {
	env := doctorEnvelope{SchemaVersion: OutputSchemaVersion, Checks: []doctorCheck{}}
	for _, check := range report.Checks {
		out := doctorCheck{
			Name:   check.Name,
			Status: string(check.Status),
			Detail: check.Detail,
			Items:  []doctorItem{},
		}
		for _, item := range check.Items {
			out.Items = append(out.Items, doctorItem{
				Subject: item.Subject,
				Status:  string(item.Status),
				Detail:  item.Detail,
			})
		}
		env.Checks = append(env.Checks, out)
	}
	return env
}

// writeDoctorHuman renders one line per check with indented item lines.
// This layout is not a compatibility contract; automation should use
// --json.
func writeDoctorHuman(w io.Writer, env doctorEnvelope) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, check := range env.Checks {
		fmt.Fprintln(tw, cells(check.Status, check.Name, check.Detail))
		for _, item := range check.Items {
			fmt.Fprintln(tw, cells("  "+item.Status, "  "+item.Subject, item.Detail))
		}
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}
	return nil
}

// cells joins a row, dropping an empty trailing detail so lines without
// one do not end in whitespace.
func cells(status, subject, detail string) string {
	if detail == "" {
		return status + "\t" + subject
	}
	return status + "\t" + subject + "\t" + detail
}
