// Package configcmd implements the `lgllm config` command: an editor for the
// YAML configuration that does not need a running server.
//
// Edits are applied to the YAML node tree rather than by re-marshalling a Go
// struct, so comments, key order and untouched entries (including ${ENV}
// placeholders and enc:v1: secrets) survive. Every write is atomic and the
// resulting file is validated before it replaces the old one.
package configcmd

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"lgllm/internal/config"
	"lgllm/internal/registry"
)

// document is one config file held as a YAML node tree.
type document struct {
	path string
	mode os.FileMode
	node *yaml.Node // document node
}

// loadDocument reads path into a node tree. A missing file is an error: only
// `config init` creates a file, so a typo in --config never silently starts an
// empty configuration.
func loadDocument(path string) (*document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	mode := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}

	doc := &yaml.Node{Kind: yaml.DocumentNode}
	if len(bytes.TrimSpace(data)) == 0 {
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	} else if err := yaml.Unmarshal(data, doc); err != nil {
		return nil, fmt.Errorf("%s is not valid YAML: %w", path, err)
	} else if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: the document root must be a mapping", path)
	}
	return &document{path: path, mode: mode, node: doc}, nil
}

func (d *document) root() *yaml.Node { return d.node.Content[0] }

// bytes renders the tree with the two-space indentation the example config uses.
func (d *document) bytes() ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(d.node); err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	return buf.Bytes(), nil
}

// rawConfig decodes the current tree without resolving ${ENV}. It is used for
// listing and for computing effective provider fields; credentials are not
// needed for those.
func (d *document) rawConfig() (*config.Config, error) {
	data, err := d.bytes()
	if err != nil {
		return nil, err
	}
	cfg := config.Default()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	return &cfg, nil
}

// save validates and atomically replaces the file. It returns warnings for
// checks that could not run (an unresolved ${ENV} in this shell).
func (d *document) save(dryRun bool, stdout io.Writer) ([]string, error) {
	data, err := d.bytes()
	if err != nil {
		return nil, err
	}
	warnings, err := validateConfig(data)
	if err != nil {
		return nil, err
	}
	if dryRun {
		if _, err := stdout.Write(data); err != nil {
			return warnings, err
		}
		return warnings, nil
	}
	if err := writeAtomic(d.path, data, d.mode); err != nil {
		return warnings, err
	}
	return warnings, nil
}

// writeAtomic writes data next to path and renames it into place, so a crash or
// a full disk leaves the old file intact.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".lgllm-config-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		cleanup()
		return fmt.Errorf("chmod temporary file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

// validateConfig runs the same parser the server uses. A config that refers to
// an environment variable missing in this shell is accepted with a warning: the
// placeholder must stay in the file, and the operator may intend to export it
// only when the server starts.
func validateConfig(data []byte) ([]string, error) {
	cfg, err := config.Parse(data)
	if err != nil {
		if strings.Contains(err.Error(), "unresolved environment reference") {
			return []string{"not fully validated: " + err.Error()}, nil
		}
		return nil, fmt.Errorf("resulting config is invalid: %w", err)
	}
	if _, err := registry.New(cfg); err != nil {
		return nil, fmt.Errorf("resulting config is invalid: %w", err)
	}
	return nil, nil
}

// --- node helpers ------------------------------------------------------------

func newMap() *yaml.Node { return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"} }
func newSeq() *yaml.Node { return &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"} }

func scalar(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}

func boolNode(v bool) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: fmt.Sprintf("%t", v)}
}

// cloneNode deep-copies a node so a sequence can be rebuilt without sharing
// child pointers with the old one.
func cloneNode(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	cp := *n
	if len(n.Content) > 0 {
		cp.Content = make([]*yaml.Node, len(n.Content))
		for i, child := range n.Content {
			cp.Content[i] = cloneNode(child)
		}
	}
	return &cp
}

// mappingGet returns the value for key in a mapping, or nil.
func mappingGet(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// mappingSet inserts or replaces a mapping key, keeping the old node's comments.
func mappingSet(m *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			old := m.Content[i+1]
			value.HeadComment = old.HeadComment
			value.LineComment = old.LineComment
			value.FootComment = old.FootComment
			m.Content[i+1] = value
			return
		}
	}
	m.Content = append(m.Content, scalar(key), value)
}

func mappingDelete(m *yaml.Node, key string) bool {
	if m == nil || m.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return true
		}
	}
	return false
}

// setOrDeleteString writes a scalar member, or removes it when value is empty.
func setOrDeleteString(m *yaml.Node, key, value string) {
	if strings.TrimSpace(value) == "" {
		mappingDelete(m, key)
		return
	}
	mappingSet(m, key, scalar(value))
}

// mappingString returns a scalar member as a string, or "".
func mappingString(m *yaml.Node, key string) string {
	v := mappingGet(m, key)
	if v == nil || v.Kind != yaml.ScalarNode {
		return ""
	}
	return v.Value
}

func mappingBool(m *yaml.Node, key string) bool {
	switch strings.ToLower(mappingString(m, key)) {
	case "true", "yes", "on", "1":
		return true
	}
	return false
}

func ensureSequence(m *yaml.Node, key string) (*yaml.Node, error) {
	if v := mappingGet(m, key); v != nil {
		if v.Kind != yaml.SequenceNode {
			return nil, fmt.Errorf("%q in the config is not a list", key)
		}
		// A starter file writes `providers: []` in flow style; appending to that
		// would put every entry on one line. Edited lists become block lists.
		v.Style = 0
		return v, nil
	}
	seq := newSeq()
	mappingSet(m, key, seq)
	return seq, nil
}

func ensureMapping(m *yaml.Node, key string) (*yaml.Node, error) {
	if v := mappingGet(m, key); v != nil {
		if v.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("%q in the config is not a mapping", key)
		}
		v.Style = 0
		return v, nil
	}
	out := newMap()
	mappingSet(m, key, out)
	return out, nil
}

// sequenceFind finds an item whose key equals value.
func sequenceFind(seq *yaml.Node, key, value string) (*yaml.Node, int) {
	if seq == nil {
		return nil, -1
	}
	for i, item := range seq.Content {
		if item.Kind == yaml.MappingNode && mappingString(item, key) == value {
			return item, i
		}
	}
	return nil, -1
}

func sequenceDelete(seq *yaml.Node, index int) {
	if seq == nil || index < 0 || index >= len(seq.Content) {
		return
	}
	seq.Content = append(seq.Content[:index], seq.Content[index+1:]...)
}

// --- flag helpers ------------------------------------------------------------

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard) // errors are reported by the caller with a hint
	return fs
}

// stringList is a repeatable string flag. Values are taken verbatim (a comma is
// part of the value), which matters for headers and combo members.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, strings.TrimSpace(v))
	return nil
}

// csvList is a repeatable flag where each occurrence may also be a comma list.
type csvList []string

func (s *csvList) String() string { return strings.Join(*s, ",") }
func (s *csvList) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*s = append(*s, part)
		}
	}
	return nil
}

// changedFlags reports which flags were explicitly provided, which is how update
// commands distinguish "keep the current value" from an empty value.
func changedFlags(fs *flag.FlagSet) map[string]bool {
	changed := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { changed[f.Name] = true })
	return changed
}

// parseFlags parses args and wraps the error with a usage hint.
func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%s: %w (try `lgllm config help`)", fs.Name(), err)
	}
	return nil
}

// parsePositional accepts the one positional id these commands take, whether it
// appears before or after the flags. Go's flag package stops at the first
// non-flag, so `combo add cheap --member x` needs the leading id lifted out.
func parsePositional(fs *flag.FlagSet, args []string) (string, error) {
	leading := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		leading = args[0]
		args = args[1:]
	}
	if err := parseFlags(fs, args); err != nil {
		return "", err
	}
	switch {
	case fs.NArg() == 0:
		return leading, nil
	case fs.NArg() > 1:
		return "", fmt.Errorf("%s takes at most one positional argument", fs.Name())
	case leading != "":
		return "", fmt.Errorf("%s: unexpected argument %q", fs.Name(), fs.Arg(0))
	default:
		return fs.Arg(0), nil
	}
}

// splitHeader parses "Name: value" or "Name=value".
func splitHeader(raw string) (string, string, error) {
	name, value, ok := strings.Cut(raw, "=")
	if !ok {
		name, value, ok = strings.Cut(raw, ":")
	}
	name = strings.TrimSpace(name)
	value = strings.TrimSpace(value)
	if !ok || name == "" {
		return "", "", fmt.Errorf("header %q must look like Name=Value", raw)
	}
	return name, value, nil
}
