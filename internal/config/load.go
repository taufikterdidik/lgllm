package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Overrides are values supplied on the command line. They are applied after the
// file is parsed, on every load, so a reload cannot silently drop them.
type Overrides struct {
	Listen    string
	LogLevel  string
	LogFormat string
}

// apply installs the non-empty overrides.
func (o Overrides) apply(c *Config) {
	if o.Listen != "" {
		c.Server.Listen = o.Listen
	}
	if o.LogLevel != "" {
		c.Log.Level = strings.ToLower(strings.TrimSpace(o.LogLevel))
	}
	if o.LogFormat != "" {
		c.Log.Format = strings.ToLower(strings.TrimSpace(o.LogFormat))
	}
}

// Load reads and validates the config file at path.
func Load(path string) (*Config, error) {
	return LoadWithOverrides(path, Overrides{})
}

// LoadWithOverrides reads the file and then applies command-line overrides.
//
// Provider cross-references are not checked here: they need the built-in
// catalog, which lives in the registry package. main calls registry.Validate
// after loading, and the watcher does the same on reload, so a bad connection
// reference is rejected in both paths.
func LoadWithOverrides(path string, ov Overrides) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", abs, err)
	}
	cfg.Source = abs
	cfg.Dir = filepath.Dir(abs)

	ov.apply(cfg)
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", abs, err)
	}
	return cfg, nil
}

// Parse decodes, defaults and validates config bytes.
//
// Parsing is strict: unknown keys are rejected, so a typo such as
// "read_header_timout" fails at startup instead of silently disabling a limit.
func Parse(data []byte) (*Config, error) {
	// Catch a second YAML document before environment expansion re-marshals the
	// tree, which would silently collapse it into one.
	if err := checkSingleDocument(data); err != nil {
		return nil, err
	}

	resolved, err := ResolveEnv(data)
	if err != nil {
		return nil, err
	}

	cfg := Default()
	dec := yaml.NewDecoder(bytes.NewReader(resolved))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("config is empty")
		}
		return nil, fmt.Errorf("parse: %w", err)
	}

	// A second YAML document is almost certainly a mistake (e.g. a stray "---"),
	// and silently ignoring it would hide a whole config block.
	var extra yaml.Node
	switch err := dec.Decode(&extra); {
	case err == nil:
		return nil, errors.New("parse: multiple YAML documents are not supported, keep one document")
	case !errors.Is(err, io.EOF):
		return nil, fmt.Errorf("parse: %w", err)
	}

	cfg.normalize()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// checkSingleDocument rejects input containing more than one YAML document.
func checkSingleDocument(data []byte) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil // emptiness is reported later with a clearer message
		}
		return fmt.Errorf("parse: %w", err)
	}
	var extra yaml.Node
	switch err := dec.Decode(&extra); {
	case err == nil:
		return errors.New("parse: multiple YAML documents are not supported, keep one document")
	case errors.Is(err, io.EOF):
		return nil
	default:
		return fmt.Errorf("parse: %w", err)
	}
}

// normalize trims and fills values that are safe to infer.
func (c *Config) normalize() {
	c.Server.Listen = strings.TrimSpace(c.Server.Listen)
	c.Auth.Mode = strings.ToLower(strings.TrimSpace(c.Auth.Mode))
	c.Log.Level = strings.ToLower(strings.TrimSpace(c.Log.Level))
	c.Log.Format = strings.ToLower(strings.TrimSpace(c.Log.Format))

	if c.Auth.Mode == "" {
		c.Auth.Mode = "localhost"
	}
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	if c.Log.Format == "" {
		c.Log.Format = "text"
	}

	keys := make([]string, 0, len(c.Auth.Keys))
	for _, k := range c.Auth.Keys {
		if k = strings.TrimSpace(k); k != "" {
			keys = append(keys, k)
		}
	}
	c.Auth.Keys = keys

	origins := make([]string, 0, len(c.Server.CORSOrigins))
	for _, o := range c.Server.CORSOrigins {
		if o = strings.TrimSpace(o); o != "" {
			origins = append(origins, o)
		}
	}
	c.Server.CORSOrigins = origins

	// --- providers and connections ---
	c.DefaultProvider = strings.TrimSpace(c.DefaultProvider)
	for i := range c.Providers {
		p := &c.Providers[i]
		p.ID = strings.TrimSpace(p.ID)
		p.Alias = strings.TrimSpace(p.Alias)
		p.BaseURL = strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
		p.Format = strings.ToLower(strings.TrimSpace(p.Format))
		p.Auth = strings.ToLower(strings.TrimSpace(p.Auth))
		p.AuthParam = strings.TrimSpace(p.AuthParam)
		// Format and auth are deliberately left empty when unset: an empty field
		// means "inherit from the catalog entry" (see registry.Materialize).
		for j := range p.Models {
			m := &p.Models[j]
			m.ID = strings.TrimSpace(m.ID)
			if m.Name = strings.TrimSpace(m.Name); m.Name == "" {
				m.Name = m.ID
			}
		}
	}
	for i := range c.Connections {
		cn := &c.Connections[i]
		cn.ID = strings.TrimSpace(cn.ID)
		cn.Provider = strings.TrimSpace(cn.Provider)
		cn.BaseURL = strings.TrimRight(strings.TrimSpace(cn.BaseURL), "/")
	}
}

// providers returns the normalized provider list with config-level checks.
//
// Cross-references (a connection naming a provider that exists only in the
// built-in catalog, credentials matching the auth scheme) are validated by the
// registry package, which owns that knowledge. This keeps config free of any
// dependency on the catalog.
func (c *Config) validateProviders(add func(string, ...any)) {
	ids := make(map[string]bool, len(c.Providers))
	aliases := make(map[string]string, len(c.Providers))

	for i := range c.Providers {
		p := &c.Providers[i]
		where := fmt.Sprintf("providers[%d]", i)
		if p.ID == "" {
			add("%s.id is required", where)
			continue
		}
		if ids[p.ID] {
			add("%s.id %q is duplicated", where, p.ID)
		}
		ids[p.ID] = true

		if p.Alias != "" {
			if ids[p.Alias] {
				add("%s.alias %q collides with a provider id", where, p.Alias)
			}
			if prev, ok := aliases[p.Alias]; ok {
				add("%s.alias %q is already used by %q", where, p.Alias, prev)
			}
			aliases[p.Alias] = p.ID
		}

		// base_url may be inherited from the provider catalog, so an empty value
		// is only an error once the catalog has been consulted (registry.Validate).
		if p.BaseURL != "" {
			if err := validateURL(p.BaseURL); err != nil {
				add("%s.base_url: %v", where, err)
			}
		}
		// Empty format/auth inherit from the provider catalog, so they are only
		// validated when explicitly set.
		if p.Format != "" && !validFormat(p.Format) {
			add("%s.format %q is not one of openai|anthropic|gemini|openai-responses|ollama", where, p.Format)
		}
		switch p.Auth {
		case "", "bearer", "x-api-key", "none":
		case "query":
			if p.AuthParam == "" {
				add("%s.auth_param is required when auth is \"query\"", where)
			}
		default:
			add("%s.auth %q is not one of bearer|x-api-key|query|none", where, p.Auth)
		}

		seen := make(map[string]bool, len(p.Models))
		for j := range p.Models {
			m := &p.Models[j]
			if m.ID == "" {
				add("%s.models[%d].id is required", where, j)
				continue
			}
			if seen[m.ID] {
				add("%s.models[%d].id %q is duplicated", where, j, m.ID)
			}
			seen[m.ID] = true
		}
	}

	connIDs := make(map[string]bool, len(c.Connections))
	for i := range c.Connections {
		cn := &c.Connections[i]
		where := fmt.Sprintf("connections[%d]", i)
		if cn.ID == "" {
			add("%s.id is required", where)
		} else if connIDs[cn.ID] {
			add("%s.id %q is duplicated", where, cn.ID)
		}
		connIDs[cn.ID] = true

		if cn.Provider == "" {
			add("%s.provider is required", where)
			continue
		}
		if cn.BaseURL != "" {
			if err := validateURL(cn.BaseURL); err != nil {
				add("%s.base_url: %v", where, err)
			}
		}
	}
}

// ValidateProviders re-runs the provider section checks on their own.
func (c *Config) ValidateProviders() error {
	var problems []string
	c.validateProviders(func(f string, a ...any) {
		problems = append(problems, fmt.Sprintf(f, a...))
	})
	return formatProblems(problems)
}

// Validate enforces the invariants the rest of the program relies on.
func (c *Config) Validate() error {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	// --- server ---
	host, port, err := net.SplitHostPort(c.Server.Listen)
	if err != nil {
		add("server.listen %q is not host:port: %v", c.Server.Listen, err)
	} else {
		if host == "" {
			add("server.listen %q has no host (use 0.0.0.0 to bind every interface)", c.Server.Listen)
		}
		// Port 0 asks the kernel for a free port, which tests rely on.
		if p, err := strconv.Atoi(port); err != nil || p < 0 || p > 65535 {
			add("server.listen %q has an invalid port", c.Server.Listen)
		}
	}

	if c.Server.ReadHeaderTimeout <= 0 {
		add("server.read_header_timeout must be positive")
	}
	if c.Server.IdleTimeout <= 0 {
		add("server.idle_timeout must be positive")
	}
	if c.Server.ShutdownTimeout <= 0 {
		add("server.shutdown_timeout must be positive")
	}
	if c.Server.MaxBodyBytes < 1024 {
		add("server.max_body_bytes must be at least 1024")
	}
	for i, o := range c.Server.CORSOrigins {
		if o == "*" {
			continue
		}
		if !strings.HasPrefix(o, "http://") && !strings.HasPrefix(o, "https://") {
			add("server.cors_origins[%d] %q must start with http:// or https://, or be \"*\"", i, o)
		}
	}

	// --- auth ---
	switch c.Auth.Mode {
	case "off":
		// Turning auth off on a public interface turns lgllm into an open relay
		// that spends the owner's provider credits. Refuse the combination.
		if !isLoopbackHost(host) {
			add("auth.mode \"off\" requires server.listen on loopback, host is %q", host)
		}
	case "localhost", "required":
	default:
		add("auth.mode %q is not one of off|localhost|required", c.Auth.Mode)
	}
	if c.Auth.Mode == "required" && len(c.Auth.Keys) == 0 {
		add("auth.mode \"required\" needs at least one key in auth.keys")
	}
	for i, k := range c.Auth.Keys {
		if strings.HasPrefix(k, "sha256:") {
			raw, err := hex.DecodeString(strings.TrimPrefix(k, "sha256:"))
			if err != nil || len(raw) != sha256.Size {
				add("auth.keys[%d] is not a valid sha256:<hex> digest", i)
			}
			continue
		}
		if len(k) < 8 {
			add("auth.keys[%d] is shorter than 8 characters (use a random token of 32+ chars)", i)
		}
	}

	// --- log ---
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		add("log.level %q is not one of debug|info|warn|error", c.Log.Level)
	}
	switch c.Log.Format {
	case "text", "json":
	default:
		add("log.format %q is not one of text|json", c.Log.Format)
	}

	// --- token saver (reserved for phase 6) ---
	if c.TokenSaver.Enabled {
		if c.TokenSaver.MinBytes < 0 {
			add("tokensaver.min_bytes must not be negative")
		}
		if c.TokenSaver.MaxBytes > 0 && c.TokenSaver.MaxBytes < c.TokenSaver.MinBytes {
			add("tokensaver.max_bytes must be >= min_bytes")
		}
	}

	// --- upstream transport ---
	if c.Upstream.ConnectTimeout <= 0 {
		add("upstream.connect_timeout must be positive")
	}
	if c.Upstream.ResponseHeaderTimeout <= 0 {
		add("upstream.response_header_timeout must be positive")
	}
	if c.Upstream.StreamIdleTimeout <= 0 {
		add("upstream.stream_idle_timeout must be positive")
	}
	if c.Upstream.StreamWriteTimeout < 0 {
		add("upstream.stream_write_timeout must not be negative (0 disables it)")
	}
	if c.Upstream.MaxIdleConnsPerHost < 0 {
		add("upstream.max_idle_conns_per_host must not be negative")
	}

	// --- providers and connections ---
	c.validateProviders(add)

	return formatProblems(problems)
}

// formatProblems renders a validation failure with every problem visible, so a
// user fixes the whole file in one pass.
func formatProblems(problems []string) error {
	switch len(problems) {
	case 0:
		return nil
	case 1:
		return errors.New("invalid config: " + problems[0])
	default:
		return errors.New("invalid config:\n  - " + strings.Join(problems, "\n  - "))
	}
}

func validFormat(f string) bool {
	switch f {
	case "openai", "anthropic", "gemini", "openai-responses", "ollama":
		return true
	}
	return false
}

func validateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("missing host")
	}
	return nil
}

// Fingerprint hashes the parsed config so a reload can tell whether anything
// meaningful changed. Secrets are included: rotating a key must count as a
// change even though it does not alter behaviour otherwise.
func (c *Config) Fingerprint() string {
	clone := *c
	clone.Source, clone.Dir = "", ""
	clone.Server.CORSOrigins = append([]string(nil), c.Server.CORSOrigins...)
	clone.Auth.Keys = append([]string(nil), c.Auth.Keys...)

	out, err := yaml.Marshal(&clone)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(out)
	return hex.EncodeToString(sum[:8])
}

// Redacted renders the config for logging with credentials masked.
func (c *Config) Redacted() string {
	clone := *c
	clone.Source, clone.Dir = "", ""
	clone.Auth.Keys = make([]string, len(c.Auth.Keys))
	for i, k := range c.Auth.Keys {
		clone.Auth.Keys[i] = maskKey(k)
	}
	out, err := yaml.Marshal(&clone)
	if err != nil {
		return "<unrenderable>"
	}
	return string(bytes.TrimSpace(out))
}

func maskKey(k string) string {
	if strings.HasPrefix(k, "sha256:") {
		return k[:len("sha256:")+6] + "…"
	}
	if len(k) <= 8 {
		return "****"
	}
	return k[:3] + "…" + k[len(k)-3:]
}

func isLoopbackHost(host string) bool {
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
