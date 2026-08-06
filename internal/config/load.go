package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Root returns the configuration directory: the PROJECTMUX_CONFIG_ROOT
// override, else $XDG_CONFIG_HOME/projectmux, else ~/.config/projectmux.
func Root() (string, error) {
	if v := os.Getenv("PROJECTMUX_CONFIG_ROOT"); v != "" {
		return v, nil
	}
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "projectmux"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot locate the configuration root: %w", err)
	}
	return filepath.Join(home, ".config", "projectmux"), nil
}

// DefaultsPath is the shared layer read before workspace resolution.
func DefaultsPath(root string) string { return filepath.Join(root, "defaults.yaml") }

// WorkspacePath is the tracked per-workspace layer.
func WorkspacePath(root, slug string) string {
	return filepath.Join(root, "workspaces", slug+".yaml")
}

// LocalPath is the optional machine-local layer, which holds secrets and
// machine-specific values and is expected to be ignored by version control.
func LocalPath(root, slug string) string {
	return filepath.Join(root, "workspaces", slug+".local.yaml")
}

// Source is one layer file: its decoded form, its path, and its document
// node.
//
// The node is kept beside Layer rather than inside it because Layer's fields
// must stay concretely typed: a yaml.Node field would reopen the
// unknown-field hole that keeps a misspelled key from silently becoming
// default behavior.
type Source struct {
	Layer Layer
	File  string
	root  *yaml.Node
}

// LoadDefaults reads defaults.yaml alone.
//
// It is separate from Load because defaults.yaml carries repository_roots,
// which workspace resolution needs before the slug — and therefore the
// remaining layer paths — is known. A corrupt defaults.yaml consequently fails
// before any resolution, which is the correct direction for "invalid
// configuration fails before any mutation".
func LoadDefaults(root string) (Source, error) {
	return loadLayer(DefaultsPath(root))
}

// Load merges defaults with the workspace layers for slug and returns the
// normalized, validated configuration and its digest.
func Load(root string, defaults Source, slug string) (Effective, error) {
	merged, err := mergeLayers(Merged{root: root}, defaults)
	if err != nil {
		return Effective{}, err
	}
	for _, path := range []string{WorkspacePath(root, slug), LocalPath(root, slug)} {
		src, err := loadLayer(path)
		if err != nil {
			return Effective{}, err
		}
		if src.Layer.RepositoryRoots != nil {
			return Effective{}, invalid(fmt.Sprintf(
				"%s: repository_roots may only be set in defaults.yaml", path))
		}
		merged, err = mergeLayers(merged, src)
		if err != nil {
			return Effective{}, err
		}
	}

	cfg := normalize(merged.Layer)
	if problems := validate(merged, cfg); len(problems) > 0 {
		return Effective{}, &InvalidConfigError{Problems: problems}
	}
	digest, err := digest(cfg)
	if err != nil {
		return Effective{}, err
	}
	return Effective{Config: cfg, Digest: digest}, nil
}

// loadLayer decodes one YAML file strictly. A missing file, an empty file, and
// a comment-only file all behave as an empty document rather than as an error
// or a null that poisons the merge.
//
// The file is decoded twice: once strictly into Layer, and once into a
// yaml.Node that carries positions. The passes stay separate because
// Node.Decode does not honor KnownFields — routing the strict decode through
// the node would trade unknown-field rejection for line numbers, and
// rejection is the more valuable of the two.
func loadLayer(path string) (Source, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Source{File: path}, nil
	}
	if err != nil {
		return Source{}, fmt.Errorf("reading %s: %w", path, err)
	}

	var layer Layer
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&layer); err != nil {
		// io.EOF means the document was empty or held only comments.
		if errors.Is(err, io.EOF) {
			return Source{File: path}, nil
		}
		return Source{}, invalid(fmt.Sprintf("%s: %s", path, cleanYAMLError(err)))
	}
	// A second document would silently discard configuration. Anything other
	// than io.EOF means more content followed, including a second document too
	// malformed to decode — which must not read as "there was nothing there".
	var extra Layer
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return Source{}, invalid(fmt.Sprintf(
			"%s: contains more than one YAML document; use a single document", path))
	}

	// Positions are best-effort: the strict decode above has already proved
	// the document is well formed and schema-valid, so a failure here would
	// mean only that lines are unavailable, never that the layer is bad.
	src := Source{Layer: layer, File: path}
	var node yaml.Node
	if err := yaml.Unmarshal(raw, &node); err == nil {
		src.root = &node
	}
	return src, nil
}

// yamlTypeSuffix strips the Go type name yaml.v3 appends to unknown-field
// errors: the reader owns a YAML file, not a Go struct.
var yamlTypeSuffix = regexp.MustCompile(`not found in type \S+`)

func cleanYAMLError(err error) string {
	msg := err.Error()
	msg = strings.TrimPrefix(msg, "yaml: unmarshal errors:\n")
	msg = yamlTypeSuffix.ReplaceAllString(msg, "is not a known field")
	lines := strings.Split(msg, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(line)
	}
	return strings.Join(lines, "; ")
}
