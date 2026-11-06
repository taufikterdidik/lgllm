// Package config defines the lgllm configuration schema, its defaults and
// validation rules.
//
// One YAML file is the single source of truth. Secrets can be injected with
// ${ENV_VAR} references so a config file can be committed without credentials.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root document.
type Config struct {
	Server     Server     `yaml:"server"`
	Auth       Auth       `yaml:"auth"`
	Log        Log        `yaml:"log"`
	TokenSaver TokenSaver `yaml:"tokensaver"`
	Upstream   Upstream   `yaml:"upstream"`
	Metrics    Metrics    `yaml:"metrics"`

	// Providers overrides or extends the built-in provider catalog. Only fields
	// that are set take effect; an entry may be just "id: openai".
	Providers []Provider `yaml:"providers"`

	// Connections are credentials. A provider becomes usable only once at least
	// one enabled connection references it.
	Connections []Connection `yaml:"connections"`

	// DefaultProvider is used for bare model names that cannot be inferred from
	// a prefix, e.g. "my-finetune". Defaults to the only configured provider.
	DefaultProvider string `yaml:"default_provider"`

	// Aliases map a client-facing name to a routing target, e.g.
	// "fast: ds/deepseek-chat". A target is "provider/model" (provider id or
	// alias) or another alias; a bare model name is refused because it would
	// silently depend on default_provider.
	Aliases map[string]string `yaml:"aliases"`

	// Combos are named groups of routing targets tried in order. Phase 3 uses
	// the first member that can serve; retry, rotation and cooldown on a failed
	// attempt arrive in phase 5.
	Combos []Combo `yaml:"combos"`

	// Source is the absolute path this config was loaded from.
	Source string `yaml:"-"`
	// Dir is the directory of Source, used to resolve relative paths.
	Dir string `yaml:"-"`
}

// Upstream tunes outbound HTTP behaviour.
//
// ResponseHeaderTimeout is the only timeout that covers "how long until the
// upstream answers", and it stops at the response headers. A whole-request
// deadline would abort long streams, so body reads are bounded by StreamIdle
// instead.
type Upstream struct {
	// ConnectTimeout bounds TCP connect plus TLS handshake.
	ConnectTimeout Duration `yaml:"connect_timeout"`
	// ResponseHeaderTimeout bounds the wait for status line and headers.
	ResponseHeaderTimeout Duration `yaml:"response_header_timeout"`
	// StreamIdleTimeout aborts a response body that produces no bytes.
	StreamIdleTimeout Duration `yaml:"stream_idle_timeout"`
	// StreamWriteTimeout aborts when a client stops reading; without it a slow
	// client would pin a goroutine and an upstream connection indefinitely.
	StreamWriteTimeout Duration `yaml:"stream_write_timeout"`
	// MaxIdleConnsPerHost sizes the connection pool per upstream host.
	MaxIdleConnsPerHost int `yaml:"max_idle_conns_per_host"`
}

// Provider describes an upstream service.
type Provider struct {
	ID string `yaml:"id"`
	// Alias is the short prefix used in model strings, e.g. "cc" for anthropic.
	Alias string `yaml:"alias"`
	// BaseURL is the API root including the version segment,
	// e.g. "https://api.openai.com/v1". Endpoint paths are appended to it.
	BaseURL string `yaml:"base_url"`
	// Format is the wire protocol: openai|anthropic|gemini|openai-responses|ollama.
	Format string `yaml:"format"`
	// Auth is the outbound scheme: bearer|x-api-key|query|none.
	Auth string `yaml:"auth"`
	// AuthParam names the query parameter when Auth is "query".
	AuthParam string `yaml:"auth_param"`
	// Headers are sent on every request to this provider (e.g. anthropic-version).
	Headers map[string]string `yaml:"headers"`
	// Timeout overrides upstream.response_header_timeout for this provider.
	Timeout Duration `yaml:"timeout"`
	// Models is display-only: it feeds GET /v1/models. Any model id is accepted
	// for proxying, because provider catalogs change weekly.
	Models   []Model `yaml:"models"`
	Disabled bool    `yaml:"disabled"`
}

// Model is one entry in a provider's advertised model list.
type Model struct {
	ID           string          `yaml:"id"`
	Name         string          `yaml:"name"`
	Context      int             `yaml:"context"`
	Capabilities map[string]bool `yaml:"capabilities"`
}

// Combo is a named routing group.
//
// Members are routing targets in priority order: "provider/model", a bare model
// name resolved like any request, or another alias. Phase 3 picks the first
// member that can serve the request (its provider has a usable credential);
// phase 5 adds retry/rotation when an attempt fails.
type Combo struct {
	Name     string   `yaml:"name"`
	Members  []string `yaml:"members"`
	Strategy string   `yaml:"strategy"`
	// TokenSaver overrides the global tokensaver.enabled for this combo when
	// set. A pointer distinguishes "not specified" from "explicitly off".
	TokenSaver *bool `yaml:"token_saver"`
}

// Metrics controls the observability surface.
type Metrics struct {
	// Enabled serves GET /metrics and GET /usage.
	Enabled bool `yaml:"enabled"`
	// PprofListen starts a separate pprof listener when non-empty. It must be a
	// loopback address: pprof has no authentication.
	PprofListen string `yaml:"pprof_listen"`
	// UsageLogSize is the number of recent requests kept in memory. Zero
	// disables the ring buffer while leaving counters active.
	UsageLogSize int `yaml:"usage_log_size"`
}

// Connection is one credential for a provider.
type Connection struct {
	ID string `yaml:"id"`
	// Provider is a provider id; the alias is accepted too.
	Provider string `yaml:"provider"`
	// APIKey may reference an environment variable: ${OPENAI_API_KEY}.
	APIKey string `yaml:"api_key"`
	// BaseURL overrides the provider base URL, for gateways and self-hosting.
	BaseURL  string `yaml:"base_url"`
	Disabled bool   `yaml:"disabled"`
}

// Server holds listener, timeout and transport limits.
//
// ReadHeaderTimeout and IdleTimeout are the only single-value timeouts. There is
// deliberately no global read/write timeout: a chat completion can stream for
// minutes, so deadlines belong to individual request phases (phase 2).
type Server struct {
	// Listen is the TCP address, e.g. "127.0.0.1:20128".
	Listen string `yaml:"listen"`

	ReadHeaderTimeout Duration `yaml:"read_header_timeout"`
	// IdleTimeout bounds keep-alive connections between requests.
	IdleTimeout Duration `yaml:"idle_timeout"`
	// ShutdownTimeout bounds how long in-flight requests may drain on SIGTERM.
	ShutdownTimeout Duration `yaml:"shutdown_timeout"`

	// MaxBodyBytes caps an individual request body.
	MaxBodyBytes int64 `yaml:"max_body_bytes"`

	// CORSOrigins is the allow-list for browser clients ("*" allows all).
	CORSOrigins []string `yaml:"cors_origins"`
}

// Auth controls inbound API key verification.
type Auth struct {
	// Mode is one of:
	//   off       - never check (only sane behind another authenticating proxy)
	//   localhost - check only when the client is not on loopback (default)
	//   required  - always require a key
	Mode string `yaml:"mode"`

	// Keys accepts plaintext keys or "sha256:<hex>" digests.
	Keys []string `yaml:"keys"`
}

// Log configures the slog handler.
type Log struct {
	Level  string `yaml:"level"`  // debug|info|warn|error
	Format string `yaml:"format"` // text|json
}

// TokenSaver is parsed now and consumed in phase 6.
type TokenSaver struct {
	Enabled  bool `yaml:"enabled"`
	MinBytes int  `yaml:"min_bytes"`
	MaxBytes int  `yaml:"max_bytes"`
	// Prompt is an optional system instruction appended when the saver is on
	// (for example a terse-answer instruction). Empty leaves it untouched.
	Prompt string `yaml:"prompt"`
}

// Default returns the configuration used when a key is absent.
func Default() Config {
	return Config{
		Server: Server{
			Listen:            "127.0.0.1:20128",
			ReadHeaderTimeout: Duration(10 * time.Second),
			IdleTimeout:       Duration(120 * time.Second),
			ShutdownTimeout:   Duration(10 * time.Second),
			MaxBodyBytes:      32 << 20, // 32 MiB: large diffs and images fit
			CORSOrigins:       []string{"*"},
		},
		Auth: Auth{Mode: "localhost"},
		Log:  Log{Level: "info", Format: "text"},
		Upstream: Upstream{
			ConnectTimeout:        Duration(10 * time.Second),
			ResponseHeaderTimeout: Duration(120 * time.Second),
			StreamIdleTimeout:     Duration(90 * time.Second),
			StreamWriteTimeout:    Duration(30 * time.Second),
			MaxIdleConnsPerHost:   64,
		},
		Metrics: Metrics{
			Enabled:      true,
			UsageLogSize: 500,
		},
		TokenSaver: TokenSaver{
			Enabled:  true,
			MinBytes: 2048,
			MaxBytes: 512 << 10,
		},
	}
}

// Duration is a time.Duration that reads YAML duration strings ("30s", "2m")
// and plain integers as seconds.
//
// It is a plain int64 kind on purpose: environment expansion re-marshals the
// document, and a struct-typed wrapper would lose its custom encoding there.
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("duration must be a scalar, got %s", kindName(node.Kind))
	}
	// After environment expansion the value may arrive as a plain scalar
	// without the !!str tag, so accept integers explicitly.
	return d.set(node.Value)
}

// set parses a duration literal.
func (d *Duration) set(raw string) error {
	raw = strings.TrimSpace(raw)
	if secs, err := time.ParseDuration(raw); err == nil {
		*d = Duration(secs)
		return nil
	}
	var n int64
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil || raw != strconv.FormatInt(n, 10) {
		return fmt.Errorf("invalid duration %q (want e.g. \"30s\" or a number of seconds)", raw)
	}
	*d = Duration(time.Duration(n) * time.Second)
	return nil
}

// MarshalYAML implements yaml.Marshaler.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// Std converts to time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

func kindName(k yaml.Kind) string {
	switch k {
	case yaml.DocumentNode:
		return "document"
	case yaml.SequenceNode:
		return "sequence"
	case yaml.MappingNode:
		return "mapping"
	case yaml.ScalarNode:
		return "scalar"
	case yaml.AliasNode:
		return "alias"
	}
	return "unknown"
}

// EnvRef describes an unresolved ${VAR} reference in the config file.
type EnvRef struct {
	Var  string
	Line int
}

// ResolveEnv expands ${VAR} and ${VAR:-default} references in a raw config
// document, before type decoding.
//
// Expansion is textual but restricted to scalar values, never keys or comments,
// so a variable can never inject structure into the document. A missing variable
// without a default is a hard error: silently substituting "" into an auth key
// or upstream URL would fail open.
func ResolveEnv(data []byte) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("config is not valid YAML: %w", err)
	}
	if root.Kind == 0 {
		return data, nil // empty document
	}
	missing, err := expandNode(&root)
	if err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		names := make([]string, 0, len(missing))
		for _, m := range missing {
			names = append(names, fmt.Sprintf("${%s} (line %d)", m.Var, m.Line))
		}
		return nil, fmt.Errorf("unresolved environment reference: %s", strings.Join(names, ", "))
	}
	return yaml.Marshal(&root)
}

func expandNode(n *yaml.Node) ([]EnvRef, error) {
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		var missing []EnvRef
		for _, c := range n.Content {
			m, err := expandNode(c)
			if err != nil {
				return nil, err
			}
			missing = append(missing, m...)
		}
		return missing, nil
	case yaml.MappingNode:
		var missing []EnvRef
		// Content is [key, value, key, value, ...]; only values are expanded.
		for i := 0; i < len(n.Content); i += 2 {
			m, err := expandNode(n.Content[i+1])
			if err != nil {
				return nil, err
			}
			missing = append(missing, m...)
		}
		return missing, nil
	case yaml.ScalarNode:
		if n.Tag != "!!str" {
			return nil, nil
		}
		expanded, refs, changed := expandString(n.Value)
		if !changed {
			return nil, nil
		}
		n.Value = expanded
		// Re-quote: an interpolated value may contain characters that are not
		// valid in YAML plain style (an API key containing '#' or ':').
		n.Style = yaml.DoubleQuotedStyle
		for i := range refs {
			refs[i].Line = n.Line
		}
		return refs, nil
	}
	return nil, nil
}

// expandString substitutes references in s. It returns the substituted string,
// any unresolved references, and whether at least one reference was found.
func expandString(s string) (string, []EnvRef, bool) {
	var b strings.Builder
	var missing []EnvRef
	changed := false
	for {
		start := strings.Index(s, "${")
		if start < 0 {
			b.WriteString(s)
			break
		}
		end := strings.Index(s[start:], "}")
		if end < 0 {
			// Not a reference; keep the literal text.
			b.WriteString(s)
			break
		}
		end += start
		changed = true
		b.WriteString(s[:start])

		expr := s[start+2 : end]
		name, fallback, hasDefault := strings.Cut(expr, ":-")
		name = strings.TrimSpace(name)
		if v, ok := os.LookupEnv(name); ok && v != "" {
			b.WriteString(v)
		} else if hasDefault {
			b.WriteString(fallback)
		} else {
			missing = append(missing, EnvRef{Var: name})
		}
		s = s[end+1:]
	}
	return b.String(), missing, changed
}
