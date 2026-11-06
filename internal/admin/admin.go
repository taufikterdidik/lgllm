// Package admin owns the resources created through the management API.
//
// The main config file is never rewritten: API-created providers, connections,
// combos and client keys live in a separate overlay (admin.yaml) that the config
// loader merges at startup. Every mutation is persisted atomically and then
// validated against the full config, including the provider catalog, before it is
// applied to the running server.
package admin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"lgllm/internal/config"
	"lgllm/internal/secret"
)

// redacted is what GET /api/connections returns in place of a key. Sending it
// back keeps the stored value.
const redacted = "***"

// Options configure the manager.
type Options struct {
	// BasePath is the main config file.
	BasePath string
	// StatePath is the overlay. Empty derives admin.state_file or
	// "admin.yaml" next to BasePath.
	StatePath string
	// Overrides are re-applied on every reload (listen, log, master key).
	Overrides config.Overrides
	// Apply publishes a validated config. It must rebuild the registry and swap
	// the live snapshot; returning an error rolls the mutation back.
	Apply func(*config.Config) error
}

// Manager serializes reads and writes of the overlay.
type Manager struct {
	opts  Options
	box   *secret.Box
	mu    sync.Mutex
	state *config.AdminState
	cfg   *config.Config
}

// New loads the overlay and the merged config once.
func New(opts Options) (*Manager, error) {
	if opts.BasePath == "" {
		return nil, errors.New("admin manager: base config path is required")
	}
	if opts.StatePath == "" {
		opts.StatePath = filepath.Join(filepath.Dir(opts.BasePath), "admin.yaml")
	}
	var box *secret.Box
	if opts.Overrides.MasterKey != "" {
		var err error
		box, err = secret.New(opts.Overrides.MasterKey)
		if err != nil {
			return nil, err
		}
	}

	state, err := config.LoadAdminState(opts.StatePath)
	if err != nil {
		return nil, err
	}
	if opts.Overrides.MasterKey != "" {
		if err := decryptState(state, opts.Overrides.MasterKey); err != nil {
			return nil, err
		}
	}
	cfg, err := loadMerged(opts)
	if err != nil {
		return nil, err
	}
	return &Manager{opts: opts, box: box, state: state, cfg: cfg}, nil
}

func loadMerged(opts Options) (*config.Config, error) {
	ov := opts.Overrides
	ov.AdminStatePath = opts.StatePath
	return config.LoadWithOverrides(opts.BasePath, ov)
}

// State returns a copy of the overlay.
func (m *Manager) State() *config.AdminState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state.Clone()
}

// Config returns the last validated merged config.
func (m *Manager) Config() *config.Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg
}

// Mutate applies fn to a copy of the overlay, persists it, validates the merged
// config and publishes it. On any error the previous file is restored.
func (m *Manager) Mutate(fn func(*config.AdminState) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	next := m.state.Clone()
	if err := fn(next); err != nil {
		return err
	}
	prev, readErr := os.ReadFile(m.opts.StatePath)
	existed := readErr == nil
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}

	if err := config.SaveAdminState(m.opts.StatePath, next, m.box); err != nil {
		return err
	}
	cfg, err := loadMerged(m.opts)
	if err != nil {
		restore(m.opts.StatePath, prev, existed)
		return err
	}
	if m.opts.Apply != nil {
		if err := m.opts.Apply(cfg); err != nil {
			restore(m.opts.StatePath, prev, existed)
			return err
		}
	}
	m.state = next
	m.cfg = cfg
	return nil
}

// restore puts the previous overlay back after a failed mutation.
func restore(path string, prev []byte, existed bool) {
	if !existed {
		_ = os.Remove(path)
		return
	}
	_ = os.WriteFile(path, prev, 0o600)
}

// Admin-managed lookups used by the HTTP layer to decide what is editable.

// HasProvider reports whether id exists in the overlay.
func (m *Manager) HasProvider(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.state.Providers {
		if p.ID == id {
			return true
		}
	}
	return false
}

// HasConnection reports whether id exists in the overlay.
func (m *Manager) HasConnection(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, cn := range m.state.Connections {
		if cn.ID == id {
			return true
		}
	}
	return false
}

// HasCombo reports whether name exists in the overlay.
func (m *Manager) HasCombo(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, cb := range m.state.Combos {
		if cb.Name == name {
			return true
		}
	}
	return false
}

// AddProvider appends an admin-managed provider.
func (m *Manager) AddProvider(p config.Provider) error {
	return m.Mutate(func(st *config.AdminState) error {
		if len(p.ID) == 0 {
			return errors.New("provider id is required")
		}
		for _, existing := range st.Providers {
			if existing.ID == p.ID {
				return fmt.Errorf("provider %q already exists", p.ID)
			}
		}
		st.Providers = append(st.Providers, p)
		return nil
	})
}

// UpdateProvider replaces an admin-managed provider.
func (m *Manager) UpdateProvider(id string, p config.Provider) error {
	return m.Mutate(func(st *config.AdminState) error {
		for i := range st.Providers {
			if st.Providers[i].ID == id {
				// GET redacts secret header values; sending "***" back keeps the
				// stored value instead of replacing a key with a literal.
				for k, v := range p.Headers {
					if v == redacted {
						p.Headers[k] = st.Providers[i].Headers[k]
					}
				}
				p.ID = id
				st.Providers[i] = p
				return nil
			}
		}
		return fmt.Errorf("provider %q is not managed by the admin API", id)
	})
}

// DeleteProvider removes an admin-managed provider.
func (m *Manager) DeleteProvider(id string) error {
	return m.Mutate(func(st *config.AdminState) error {
		idx := -1
		for i := range st.Providers {
			if st.Providers[i].ID == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			return fmt.Errorf("provider %q is not managed by the admin API", id)
		}
		for _, cn := range st.Connections {
			if cn.Provider == id {
				return fmt.Errorf("provider %q still has connection %q; delete it first", id, cn.ID)
			}
		}
		st.Providers = append(st.Providers[:idx], st.Providers[idx+1:]...)
		return nil
	})
}

// AddConnection appends an admin-managed connection.
func (m *Manager) AddConnection(cn config.Connection) error {
	return m.Mutate(func(st *config.AdminState) error {
		if cn.ID == "" {
			return errors.New("connection id is required")
		}
		for _, existing := range st.Connections {
			if existing.ID == cn.ID {
				return fmt.Errorf("connection %q already exists", cn.ID)
			}
		}
		st.Connections = append(st.Connections, cn)
		return nil
	})
}

// UpdateConnection replaces an admin-managed connection. An empty or "***"
// api_key keeps the stored one, so GET (which redacts it) can round-trip.
func (m *Manager) UpdateConnection(id string, cn config.Connection) error {
	return m.Mutate(func(st *config.AdminState) error {
		for i := range st.Connections {
			if st.Connections[i].ID == id {
				if cn.APIKey == "" || cn.APIKey == redacted {
					cn.APIKey = st.Connections[i].APIKey
				}
				cn.ID = id
				st.Connections[i] = cn
				return nil
			}
		}
		return fmt.Errorf("connection %q is not managed by the admin API", id)
	})
}

// DeleteConnection removes an admin-managed connection.
func (m *Manager) DeleteConnection(id string) error {
	return m.Mutate(func(st *config.AdminState) error {
		for i := range st.Connections {
			if st.Connections[i].ID == id {
				st.Connections = append(st.Connections[:i], st.Connections[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("connection %q is not managed by the admin API", id)
	})
}

// AddCombo appends an admin-managed combo.
func (m *Manager) AddCombo(cb config.Combo) error {
	return m.Mutate(func(st *config.AdminState) error {
		if cb.Name == "" {
			return errors.New("combo name is required")
		}
		for _, existing := range st.Combos {
			if existing.Name == cb.Name {
				return fmt.Errorf("combo %q already exists", cb.Name)
			}
		}
		st.Combos = append(st.Combos, cb)
		return nil
	})
}

// UpdateCombo replaces an admin-managed combo.
func (m *Manager) UpdateCombo(name string, cb config.Combo) error {
	return m.Mutate(func(st *config.AdminState) error {
		for i := range st.Combos {
			if st.Combos[i].Name == name {
				cb.Name = name
				st.Combos[i] = cb
				return nil
			}
		}
		return fmt.Errorf("combo %q is not managed by the admin API", name)
	})
}

// DeleteCombo removes an admin-managed combo.
func (m *Manager) DeleteCombo(name string) error {
	return m.Mutate(func(st *config.AdminState) error {
		for i := range st.Combos {
			if st.Combos[i].Name == name {
				st.Combos = append(st.Combos[:i], st.Combos[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("combo %q is not managed by the admin API", name)
	})
}

// AddKey appends a client key to the overlay.
func (m *Manager) AddKey(key string) error {
	if len(key) < 8 {
		return errors.New("api key must be at least 8 characters")
	}
	return m.Mutate(func(st *config.AdminState) error {
		st.Keys = append(st.Keys, key)
		return nil
	})
}

// DeleteKey removes an admin key by its id (a hash prefix returned by ListKeys).
func (m *Manager) DeleteKey(id string) error {
	return m.Mutate(func(st *config.AdminState) error {
		for i, k := range st.Keys {
			if KeyID(k) == id {
				st.Keys = append(st.Keys[:i], st.Keys[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("key %q is not managed by the admin API", id)
	})
}
