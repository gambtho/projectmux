package cli

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/gambtho/projectmux/internal/config"
)

// Validation statuses. They are the JSON contract, so they are spelled once
// here rather than inline.
const (
	statusOK      = "ok"
	statusWarn    = "warn"
	statusInvalid = "invalid"
	statusUnknown = "unknown"
)

// validationReport is the versioned JSON envelope for config --validate.
//
// It is additive to schema_version 1, following the convention every other
// command uses: each owns its own top-level shape under the shared version.
type validationReport struct {
	SchemaVersion int                `json:"schema_version"`
	ConfigRoot    string             `json:"config_root"`
	Results       []validationResult `json:"results"`
	Summary       validationSummary  `json:"summary"`
}

type validationResult struct {
	// Subject is a slug, or "defaults.yaml" for the shared layer.
	Subject  string            `json:"subject"`
	Status   string            `json:"status"`
	Problems []reportedProblem `json:"problems"`
	// Detail explains a subject that could not be examined at all.
	Detail string `json:"detail,omitempty"`
	// raw is what Problems was rendered from. It is kept so the returned
	// error can carry the real findings rather than an empty value used only
	// to select an exit code.
	raw []config.Problem
}

// reportedProblem is the JSON form of a validation problem. Origins is always
// present and empty rather than null, so a consumer can iterate it without a
// nil check.
type reportedProblem struct {
	Field   string           `json:"field"`
	Message string           `json:"message"`
	Origins []reportedOrigin `json:"origins"`
}

type reportedOrigin struct {
	File string `json:"file"`
	// Line is omitted when the position within the file is not known.
	Line int `json:"line,omitempty"`
}

type validationSummary struct {
	Subjects     int `json:"subjects"`
	WithProblems int `json:"with_problems"`
	Problems     int `json:"problems"`
}

// runValidate checks configuration files without resolving any worktree.
//
// This is the whole point of the mode: `config <workspace>` resolves through
// git, so a workspace whose worktree has moved reports as unknown and never
// receives a configuration verdict. Here the argument names a workspace file
// directly, which is exactly the case that most needs checking.
func runValidate(root, slug string, asJSON, compact bool, stdout io.Writer) error {
	report := validationReport{
		SchemaVersion: OutputSchemaVersion,
		ConfigRoot:    root,
		Results:       []validationResult{},
	}

	defaults, defaultsErr := config.LoadDefaults(root)
	slugs, slugsErr := config.Slugs(root)

	if slug != "" {
		// Membership is the contract: the mode validates workspace files that
		// exist. It also makes traversal unreachable, since a slug carrying a
		// separator can never match a discovered name.
		if slugsErr != nil {
			return fmt.Errorf("reading the workspaces directory: %w", slugsErr)
		}
		if !slices.Contains(slugs, slug) {
			return usagef("config --validate: no workspace named %q; known workspaces: %s",
				slug, knownList(slugs))
		}
		report.Results = append(report.Results, validateWorkspace(defaults, defaultsErr, root, slug))
	} else {
		report.Results = append(report.Results, validateDefaultsLayer(defaults, defaultsErr))
		if slugsErr != nil {
			// Uncertainty, never absence: a directory that cannot be read is
			// not an installation with no workspaces.
			report.Results = append(report.Results, validationResult{
				Subject:  "workspaces",
				Status:   statusUnknown,
				Problems: []reportedProblem{},
				Detail:   slugsErr.Error(),
			})
		}
		for _, s := range slugs {
			report.Results = append(report.Results, validateWorkspace(defaults, defaultsErr, root, s))
		}
	}
	report.Summary = summarize(report.Results)

	if asJSON {
		if err := writeJSON(stdout, report, compact); err != nil {
			return err
		}
	} else if err := writeValidationText(stdout, report); err != nil {
		return err
	}
	return validationOutcome(report)
}

// validateDefaultsLayer checks defaults.yaml on its own.
//
// Problems here are a warning, not a failure: defaults are the bottom layer
// and a workspace layer may legitimately supply what they omit. Treating them
// as failure would break `config --validate && deploy` on a correct install.
func validateDefaultsLayer(defaults config.Source, defaultsErr error) validationResult {
	result := validationResult{Subject: "defaults.yaml", Status: statusOK, Problems: []reportedProblem{}}
	if defaultsErr != nil {
		result.Status = statusInvalid
		result.raw = config.ProblemsOf(defaultsErr)
		result.Problems = reportProblems(result.raw)
		return result
	}
	if problems := config.ValidateDefaults(defaults); len(problems) > 0 {
		result.Status = statusWarn
		result.raw = problems
		result.Problems = reportProblems(problems)
	}
	return result
}

// validateWorkspace merges defaults with one workspace's layers and reports
// what the merged result rejects.
func validateWorkspace(defaults config.Source, defaultsErr error, root, slug string) validationResult {
	result := validationResult{Subject: slug, Status: statusOK, Problems: []reportedProblem{}}
	if defaultsErr != nil {
		// Nothing can be merged without a defaults layer, so this workspace
		// is unexamined rather than clean.
		result.Status = statusUnknown
		result.Detail = "defaults.yaml could not be read: " + defaultsErr.Error()
		return result
	}
	if _, err := config.Load(root, defaults, slug); err != nil {
		result.Status = statusInvalid
		result.raw = config.ProblemsOf(err)
		result.Problems = reportProblems(result.raw)
	}
	return result
}

func reportProblems(problems []config.Problem) []reportedProblem {
	out := make([]reportedProblem, 0, len(problems))
	for _, p := range problems {
		origins := make([]reportedOrigin, 0, len(p.Origins))
		for _, o := range p.Origins {
			origins = append(origins, reportedOrigin{File: o.File, Line: o.Line})
		}
		out = append(out, reportedProblem{Field: p.Field, Message: p.Message, Origins: origins})
	}
	return out
}

func summarize(results []validationResult) validationSummary {
	summary := validationSummary{Subjects: len(results)}
	for _, r := range results {
		if len(r.Problems) > 0 {
			summary.WithProblems++
			summary.Problems += len(r.Problems)
		}
	}
	return summary
}

// validationOutcome maps the report to an exit code.
//
// A confirmed defect outranks unexamined ground: it is the more actionable
// answer and the report carries both either way. A warning is report content,
// not failure.
func validationOutcome(report validationReport) error {
	var unknown bool
	var found []config.Problem
	for _, r := range report.Results {
		switch r.Status {
		case statusInvalid:
			found = append(found, r.raw...)
		case statusUnknown:
			unknown = true
		}
	}
	switch {
	case len(found) > 0:
		return &reportedError{
			msg: fmt.Sprintf("configuration is invalid (%d problems); the report is above",
				report.Summary.Problems),
			err: &config.InvalidConfigError{Problems: found},
		}
	case unknown:
		return &reportedError{msg: "some configuration could not be examined; the report is above"}
	default:
		return nil
	}
}

// knownList renders the discovered slugs for an unknown-slug message.
func knownList(slugs []string) string {
	if len(slugs) == 0 {
		return "none are configured"
	}
	return strings.Join(slugs, ", ")
}

// writeValidationText renders the human report. As everywhere else, this
// layout is not a compatibility contract; automation uses --json.
func writeValidationText(w io.Writer, report validationReport) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, r := range report.Results {
		status := r.Status
		if len(r.Problems) > 0 {
			status = fmt.Sprintf("%s (%s)", plural(len(r.Problems), "problem"), r.Status)
		}
		fmt.Fprintf(tw, "%s\t%s\n", r.Subject, status)
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}

	for _, r := range report.Results {
		if r.Detail != "" {
			fmt.Fprintf(w, "\n%s: %s\n", r.Subject, r.Detail)
		}
		if len(r.Problems) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s:\n", r.Subject)
		for _, p := range r.Problems {
			fmt.Fprintf(w, "  %s\n", describe(p))
		}
	}

	fmt.Fprintf(w, "\n%s in %d of %s\n",
		plural(report.Summary.Problems, "problem"),
		report.Summary.WithProblems,
		plural(report.Summary.Subjects, "subject"))
	return nil
}

// describe renders one problem with its positions, primary first. A problem
// with no position prints none rather than a fabricated one.
func describe(p reportedProblem) string {
	if len(p.Origins) == 0 {
		return p.Message
	}
	out := renderOrigin(p.Origins[0]) + ": " + p.Message
	if len(p.Origins) > 1 {
		rest := make([]string, 0, len(p.Origins)-1)
		for _, o := range p.Origins[1:] {
			rest = append(rest, renderOrigin(o))
		}
		out += " (also " + strings.Join(rest, ", ") + ")"
	}
	return out
}

func renderOrigin(o reportedOrigin) string {
	if o.Line == 0 {
		return o.File
	}
	return fmt.Sprintf("%s:%d", o.File, o.Line)
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
