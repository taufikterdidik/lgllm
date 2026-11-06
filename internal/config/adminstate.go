package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"lgllm/internal/secret"
)

// AdminState holds resources created through the admin API.
//
// It lives in its own file (admin.yaml by default) instead of being written back
// to the main config, because the main file may contain ${ENV} references and
// comments that a round-trip would destroy.
type AdminState struct {
	Providers   []Provider   `yaml:"providers,omitempty"`
	Connections []Connection `yaml:"connections,omitempty"`
	Combos      []Combo      `yaml:"combos,omitempty"`
	Keys        []string     `yaml:"keys,omitempty"`
}

// Clone returns a deep copy safe to mutate.
func (s *AdminState) Clone() *AdminState {
	if s == nil {
		return &AdminState{}
	}
	out := &AdminState{
		Providers:   append([]Provider(nil), s.Providers...),
		Connections: append([]Connection(nil), s.Connections...),
		Combos:      append([]Combo(nil), s.Combos...),
		Keys:        append([]string(nil), s.Keys...),
	}
	for i := range out.Providers {
		out.Providers[i].Headers = copyStrings(out.Providers[i].Headers)
		out.Providers[i].Models = append([]Model(nil), out.Providers[i].Models...)
	}
	for i := range out.Combos {
		out.Combos[i].Members = append([]string(nil), out.Combos[i].Members...)
	}
	return out
}

func copyStrings(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// AdminStatePath is the file the manager API writes to.
func AdminStatePath(c *Config) string {
	if c == nil {
		return "admin.yaml"
	}
	if c.Admin.StateFile != "" {
		return c.Admin.StateFile
	}
	if c.Dir != "" {
		return filepath.Join(c.Dir, "admin.yaml")
	}
	return "admin.yaml"
}

func resolveAdminStatePath(c *Config) error {
	if c.Admin.StateFile == "" || filepath.IsAbs(c.Admin.StateFile) {
		return nil
	}
	if c.Dir == "" {
		return nil
	}
	c.Admin.StateFile = filepath.Join(c.Dir, c.Admin.StateFile)
	return nil
}

// LoadAdminState reads the overlay file. A missing file is not an error: it just
// means nothing has been created through the API yet.
func LoadAdminState(path string) (*AdminState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &AdminState{}, nil
		}
		return nil, fmt.Errorf("read admin state %s: %w", path, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return &AdminState{}, nil
	}
	st := &AdminState{}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(st); err != nil {
		if errors.Is(err, io.EOF) {
			return &AdminState{}, nil
		}
		return nil, fmt.Errorf("parse admin state %s: %w", path, err)
	}
	return st, nil
}

// MergeAdminState appends API-managed resources to a config.
//
// Overlay entries may not reuse an id from the main file: the admin API manages
// its own resources, and a silent override would make "delete" ambiguous.
func MergeAdminState(c *Config, st *AdminState) error {
	if st == nil {
		return nil
	}
	providerIDs := make(map[string]bool, len(c.Providers))
	for _, p := range c.Providers {
		providerIDs[p.ID] = true
	}
	connIDs := make(map[string]bool, len(c.Connections))
	for _, cn := range c.Connections {
		connIDs[cn.ID] = true
	}
	comboNames := make(map[string]bool, len(c.Combos))
	for _, cb := range c.Combos {
		comboNames[cb.Name] = true
	}

	for _, p := range st.Providers {
		if providerIDs[p.ID] {
			return fmt.Errorf("admin state: provider %q already exists in the main config", p.ID)
		}
		providerIDs[p.ID] = true
		c.Providers = append(c.Providers, p)
	}
	for _, cn := range st.Connections {
		if connIDs[cn.ID] {
			return fmt.Errorf("admin state: connection %q already exists in the main config", cn.ID)
		}
		connIDs[cn.ID] = true
		c.Connections = append(c.Connections, cn)
	}
	for _, cb := range st.Combos {
		if comboNames[cb.Name] {
			return fmt.Errorf("admin state: combo %q already exists in the main config", cb.Name)
		}
		comboNames[cb.Name] = true
		c.Combos = append(c.Combos, cb)
	}
	seenKeys := make(map[string]bool, len(c.Auth.Keys))
	for _, k := range c.Auth.Keys {
		seenKeys[k] = true
	}
	for _, k := range st.Keys {
		if !seenKeys[k] {
			seenKeys[k] = true
			c.Auth.Keys = append(c.Auth.Keys, k)
		}
	}
	return nil
}

func mergeAdminStateFile(c *Config, path string) error {
	st, err := LoadAdminState(path)
	if err != nil {
		return err
	}
	return MergeAdminState(c, st)
}

// SaveAdminState writes the overlay atomically with 0600 permissions.
//
// When a master key is configured, connection keys, admin client keys and
// provider header values are encrypted before they touch the disk. Existing
// ciphertext is left as-is so an already encrypted state is not double wrapped.
func SaveAdminState(path string, st *AdminState, box *secret.Box) error {
	cp := st.Clone()
	if box != nil {
		encrypt := func(v string) (string, error) {
			if v == "" || secret.IsEncrypted(v) {
				return v, nil
			}
			return box.Encrypt(v)
		}
		for i := range cp.Connections {
			v, err := encrypt(cp.Connections[i].APIKey)
			if err != nil {
				return err
			}
			cp.Connections[i].APIKey = v
		}
		for i := range cp.Keys {
			v, err := encrypt(cp.Keys[i])
			if err != nil {
				return err
			}
			cp.Keys[i] = v
		}
		for i := range cp.Providers {
			for k, v := range cp.Providers[i].Headers {
				ev, err := encrypt(v)
				if err != nil {
					return err
				}
				cp.Providers[i].Headers[k] = ev
			}
		}
	}

	data, err := yaml.Marshal(cp)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".admin-*.yaml")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
