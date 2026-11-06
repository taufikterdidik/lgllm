// Package registry compiles provider configuration into an immutable routing
// index.
//
// Two layers:
//
//   - Catalog: a small built-in list of providers with a stable public HTTP API
//     (base URL, wire format, auth scheme, display models). It lets a config be
//     as short as one connection entry.
//   - Index: the catalog merged with config overrides, then reduced to providers
//     that actually have an enabled credential.
//
// The index is rebuilt on every config reload and swapped atomically, so a
// request always sees one consistent view of providers and credentials.
package registry

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"lgllm/internal/config"
)

// Wire formats a provider can speak.
const (
	FormatChat      = "openai"           // chat completions (the pivot format)
	FormatAnthropic = "anthropic"        // messages API
	FormatGemini    = "gemini"           // generateContent
	FormatResponses = "openai-responses" // responses API
	FormatOllama    = "ollama"           // native /api/chat
)

var (
	// ErrUnknownProvider means the model prefix matched no provider.
	ErrUnknownProvider = errors.New("unknown provider")
	// ErrNoDefaultProvider means a bare model name was used but no provider
	// could be inferred.
	ErrNoDefaultProvider = errors.New("no default provider configured")
)

// Builtin is one catalog entry. BaseURL is the API root (not the final
// endpoint); provider-specific paths are appended by the adapter.
type Builtin struct {
	ID        string
	Alias     string
	Name      string
	BaseURL   string
	Format    string
	Auth      string
	AuthParam string
	Headers   map[string]string
	Models    []string
	// KeyEnv is the conventional environment variable for the API key.
	KeyEnv string
	Site   string
}

// catalog is deliberately short. An entry needs a stable, documented HTTP API;
// providers that require browser automation or captured cookies are out of
// scope because they break silently and cannot be tested here.
//
// Model lists are hints for GET /v1/models, never an allow-list: any model id is
// forwarded, because provider catalogs change weekly.
var catalog = []Builtin{
	{
		ID: "anthropic", Alias: "cc", Name: "Anthropic",
		BaseURL: "https://api.anthropic.com/v1",
		Format:  FormatAnthropic, Auth: "x-api-key",
		Headers: map[string]string{"anthropic-version": "2023-06-01"},
		// Model hints are added in phase 4: this provider is not reachable via
		// the phase-2 passthrough, so advertising models here would mislead.
		KeyEnv: "ANTHROPIC_API_KEY", Site: "https://docs.anthropic.com",
	},
	{
		ID: "deepseek", Alias: "ds", Name: "DeepSeek",
		BaseURL: "https://api.deepseek.com/v1",
		Format:  FormatChat, Auth: "bearer",
		Models: []string{"deepseek-chat", "deepseek-reasoner"},
		KeyEnv: "DEEPSEEK_API_KEY", Site: "https://platform.deepseek.com",
	},
	{
		// Gemini exposes an OpenAI-compatible surface at /v1beta/openai, which
		// lets it work through the phase-2 passthrough with no translator.
		ID: "gemini", Alias: "gem", Name: "Google Gemini",
		BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai",
		Format:  FormatChat, Auth: "bearer",
		Models: []string{"gemini-2.5-pro", "gemini-2.5-flash", "gemini-2.5-flash-lite"},
		KeyEnv: "GEMINI_API_KEY", Site: "https://ai.google.dev",
	},
	{
		ID: "groq", Alias: "gq", Name: "Groq",
		BaseURL: "https://api.groq.com/openai/v1",
		Format:  FormatChat, Auth: "bearer",
		Models: []string{"llama-3.3-70b-versatile", "llama-3.1-8b-instant"},
		KeyEnv: "GROQ_API_KEY", Site: "https://console.groq.com",
	},
	{
		ID: "mistral", Alias: "ms", Name: "Mistral",
		BaseURL: "https://api.mistral.ai/v1",
		Format:  FormatChat, Auth: "bearer",
		Models: []string{"mistral-large-latest", "mistral-small-latest"},
		KeyEnv: "MISTRAL_API_KEY", Site: "https://console.mistral.ai",
	},
	{
		ID: "ollama", Name: "Ollama (local)",
		BaseURL: "http://127.0.0.1:11434/v1",
		Format:  FormatChat, Auth: "none",
		Models: []string{"llama3.2", "qwen2.5-coder", "deepseek-r1"},
		Site:   "https://ollama.com",
	},
	{
		ID: "openrouter", Alias: "or", Name: "OpenRouter",
		BaseURL: "https://openrouter.ai/api/v1",
		Format:  FormatChat, Auth: "bearer",
		Models: []string{"deepseek/deepseek-chat", "google/gemini-2.5-pro"},
		KeyEnv: "OPENROUTER_API_KEY", Site: "https://openrouter.ai",
	},
	{
		ID: "openai", Alias: "oa", Name: "openai",
		BaseURL: "https://api.openai.com/v1",
		Format:  FormatChat, Auth: "bearer",
		Models: []string{"gpt-4o", "gpt-4o-mini", "gpt-4.1", "gpt-4.1-mini", "o3", "o4-mini"},
		KeyEnv: "OPENAI_API_KEY", Site: "https://platform.openai.com",
	},
}

// Catalog returns the built-in providers, sorted by id.
func Catalog() []Builtin {
	out := append([]Builtin(nil), catalog...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// CatalogByRef finds a catalog entry by id or alias.
func CatalogByRef(ref string) (Builtin, bool) {
	for _, b := range catalog {
		if b.ID == ref || (b.Alias != "" && b.Alias == ref) {
			return b, true
		}
	}
	return Builtin{}, false
}

// Materialize merges the catalog with config overrides.
//
// Every provider declared in config.providers becomes effective, as does every
// catalog entry referenced by a connection, so "add a connection" alone is
// enough. Non-empty config fields override catalog fields; empty ones inherit.
// A provider unknown to the catalog is fully user-defined.
func Materialize(cfg []config.Provider, conns []config.Connection) []config.Provider {
	// Resolve every reference to a canonical catalog id first, so "id: oa" and
	// "id: openai" land on the same entry instead of creating two providers.
	canonical := func(ref string) string {
		if b, ok := CatalogByRef(ref); ok {
			return b.ID
		}
		return ref
	}

	needed := make([]string, 0, len(cfg)+len(conns))
	seen := make(map[string]bool, len(cfg)+len(conns))
	addRef := func(ref string) {
		if ref == "" {
			return
		}
		id := canonical(ref)
		if seen[id] {
			return
		}
		seen[id] = true
		needed = append(needed, id)
	}
	for _, p := range cfg {
		addRef(p.ID)
	}
	// A connection may reference a catalog provider without declaring it, but it
	// cannot conjure a provider that exists nowhere: that is a config error and
	// is reported by Validate.
	for _, c := range conns {
		if _, ok := CatalogByRef(c.Provider); ok {
			addRef(c.Provider)
		}
	}

	overrides := make(map[string]*config.Provider, len(cfg))
	for i := range cfg {
		overrides[canonical(cfg[i].ID)] = &cfg[i]
	}

	out := make([]config.Provider, 0, len(needed))
	for _, id := range needed {
		base := config.Provider{ID: id}
		if b, ok := CatalogByRef(id); ok {
			base = fromCatalog(b)
		}
		if ov, ok := overrides[id]; ok {
			mergeOverride(&base, ov)
		}
		if base.Format == "" {
			base.Format = FormatChat
		}
		if base.Auth == "" {
			base.Auth = "bearer"
		}
		out = append(out, base)
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func fromCatalog(b Builtin) config.Provider {
	p := config.Provider{
		ID:        b.ID,
		Alias:     b.Alias,
		BaseURL:   b.BaseURL,
		Format:    b.Format,
		Auth:      b.Auth,
		AuthParam: b.AuthParam,
	}
	if len(b.Headers) > 0 {
		p.Headers = make(map[string]string, len(b.Headers))
		for k, v := range b.Headers {
			p.Headers[k] = v
		}
	}
	for _, m := range b.Models {
		p.Models = append(p.Models, config.Model{ID: m, Name: m})
	}
	return p
}

func mergeOverride(dst, src *config.Provider) {
	if src.Alias != "" {
		dst.Alias = src.Alias
	}
	if src.BaseURL != "" {
		dst.BaseURL = src.BaseURL
	}
	if src.Format != "" {
		dst.Format = src.Format
	}
	if src.Auth != "" {
		dst.Auth = src.Auth
	}
	if src.AuthParam != "" {
		dst.AuthParam = src.AuthParam
	}
	if len(src.Headers) > 0 {
		if dst.Headers == nil {
			dst.Headers = make(map[string]string, len(src.Headers))
		}
		for k, v := range src.Headers {
			dst.Headers[k] = v
		}
	}
	if src.Timeout != 0 {
		dst.Timeout = src.Timeout
	}
	if len(src.Models) > 0 {
		dst.Models = src.Models
	}
	dst.Disabled = src.Disabled
}

// Validate checks the provider set after materialization.
//
// It lives here rather than in config because only this package knows the
// catalog: a config may name "deepseek" and inherit its base URL, but a custom
// provider must spell one out.
func Validate(cfg *config.Config) error {
	effective := Materialize(cfg.Providers, cfg.Connections)

	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	known := make(map[string]bool, len(effective))
	for i := range effective {
		p := &effective[i]
		known[p.ID] = true
		if p.Alias != "" {
			known[p.Alias] = true
		}

		if p.BaseURL == "" {
			add("provider %q has no base_url; set it or use a catalog id", p.ID)
		} else if err := validateEndpoint(p.BaseURL); err != nil {
			add("provider %q base_url: %v", p.ID, err)
		}
		switch p.Format {
		case FormatChat, FormatAnthropic, FormatGemini, FormatResponses, FormatOllama:
		default:
			add("provider %q format %q is not one of %s|%s|%s|%s|%s",
				p.ID, p.Format, FormatChat, FormatAnthropic, FormatGemini, FormatResponses, FormatOllama)
		}
		switch p.Auth {
		case "bearer", "x-api-key", "none":
		case "query":
			if p.AuthParam == "" {
				add("provider %q auth_param is required when auth is \"query\"", p.ID)
			}
		default:
			add("provider %q auth %q is not one of bearer|x-api-key|query|none", p.ID, p.Auth)
		}
		for j, m := range p.Models {
			if strings.TrimSpace(m.ID) == "" {
				add("provider %q models[%d].id is empty", p.ID, j)
			}
		}
	}

	for i := range cfg.Connections {
		c := &cfg.Connections[i]
		if c.Provider == "" {
			continue // reported by config validation already
		}
		if !known[c.Provider] {
			add("connections[%d] (%q) references unknown provider %q", i, c.ID, c.Provider)
			continue
		}
		if c.Disabled {
			continue
		}
		p := findProvider(effective, c.Provider)
		if p != nil && p.Auth != "none" && c.APIKey == "" {
			add("connections[%d] (%q) has no api_key (provider %q uses %q auth)",
				i, c.ID, p.ID, p.Auth)
		}
	}

	if cfg.DefaultProvider != "" && !known[cfg.DefaultProvider] {
		add("default_provider %q does not match any provider", cfg.DefaultProvider)
	}

	switch len(problems) {
	case 0:
		return nil
	case 1:
		return errors.New("invalid config: " + problems[0])
	default:
		return errors.New("invalid config:\n  - " + strings.Join(problems, "\n  - "))
	}
}

func findProvider(list []config.Provider, ref string) *config.Provider {
	for i := range list {
		if list[i].ID == ref || (list[i].Alias != "" && list[i].Alias == ref) {
			return &list[i]
		}
	}
	return nil
}

func validateEndpoint(raw string) error {
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

// Provider is a provider plus its credentials, ready to serve a request.
type Provider struct {
	ID        string
	Alias     string
	BaseURL   string
	Format    string
	Auth      string
	AuthParam string
	Headers   map[string]string
	Timeout   time.Duration
	Models    []Model

	// connections holds enabled credentials in config order. Phase 5 rotates
	// across them; phase 2 uses the first.
	connections []Connection
}

// Connection is one usable credential.
type Connection struct {
	ID      string
	APIKey  string
	BaseURL string
}

// Model is an advertised model.
type Model struct {
	ID      string
	Name    string
	Context int
	// Capabilities are optional features the provider declares (tools, vision,
	// thinking, ...). They are only used by the compatibility endpoints; the
	// chat proxy forwards whatever the client asks for regardless.
	Capabilities map[string]bool
}

// Registry is an immutable snapshot of usable providers.
type Registry struct {
	providers map[string]*Provider
	byAlias   map[string]string
	order     []string
	fallback  string
}

// New validates the config and compiles it into a registry snapshot.
//
// A provider with no enabled credential is left out: routing then fails with
// "unknown provider" instead of sending a keyless request upstream.
func New(cfg *config.Config) (*Registry, error) {
	if err := Validate(cfg); err != nil {
		return nil, err
	}

	effective := Materialize(cfg.Providers, cfg.Connections)

	r := &Registry{
		providers: make(map[string]*Provider, len(effective)),
		byAlias:   make(map[string]string, len(effective)),
	}

	for _, pc := range effective {
		if pc.Disabled {
			continue
		}

		p := &Provider{
			ID:        pc.ID,
			Alias:     pc.Alias,
			BaseURL:   strings.TrimRight(pc.BaseURL, "/"),
			Format:    pc.Format,
			Auth:      pc.Auth,
			AuthParam: pc.AuthParam,
			Headers:   copyMap(pc.Headers),
			Timeout:   pc.Timeout.Std(),
		}
		for _, m := range pc.Models {
			p.Models = append(p.Models, Model{
				ID:           m.ID,
				Name:         m.Name,
				Context:      m.Context,
				Capabilities: copyBoolMap(m.Capabilities),
			})
		}

		for _, cc := range cfg.Connections {
			if cc.Disabled || !refersTo(cc.Provider, pc) {
				continue
			}
			p.connections = append(p.connections, Connection{
				ID:      cc.ID,
				APIKey:  cc.APIKey,
				BaseURL: strings.TrimRight(cc.BaseURL, "/"),
			})
		}

		if len(p.connections) == 0 {
			continue
		}

		r.providers[p.ID] = p
		r.order = append(r.order, p.ID)
		if p.Alias != "" {
			r.byAlias[p.Alias] = p.ID
		}
	}

	sort.Strings(r.order)

	switch {
	case cfg.DefaultProvider != "":
		if id, ok := r.lookup(cfg.DefaultProvider); ok {
			r.fallback = id
		}
	case len(r.order) == 1:
		r.fallback = r.order[0]
	}

	return r, nil
}

func refersTo(ref string, p config.Provider) bool {
	return ref == p.ID || (p.Alias != "" && ref == p.Alias)
}

func copyMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func copyBoolMap(in map[string]bool) map[string]bool {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]bool, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (r *Registry) lookup(ref string) (string, bool) {
	if _, ok := r.providers[ref]; ok {
		return ref, true
	}
	if id, ok := r.byAlias[ref]; ok {
		return id, true
	}
	return "", false
}

// Provider returns a provider by id or alias.
func (r *Registry) Provider(ref string) (*Provider, bool) {
	id, ok := r.lookup(ref)
	if !ok {
		return nil, false
	}
	return r.providers[id], true
}

// Providers lists every usable provider in id order.
func (r *Registry) Providers() []*Provider {
	out := make([]*Provider, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.providers[id])
	}
	return out
}

// Default returns the provider used for bare model names, if any.
func (r *Registry) Default() (*Provider, bool) {
	if r.fallback == "" {
		return nil, false
	}
	p, ok := r.providers[r.fallback]
	return p, ok
}

// Len reports how many providers are usable.
func (r *Registry) Len() int { return len(r.order) }

// Resolve maps a client model string to a provider and upstream model id.
//
// Accepted forms:
//   - "provider/model" or "alias/model"
//   - "model" (resolved against the default provider)
func (r *Registry) Resolve(requested string) (*Provider, string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return nil, "", errors.New("model is required")
	}

	if prefix, rest, found := strings.Cut(requested, "/"); found && rest != "" {
		if p, ok := r.Provider(prefix); ok {
			return p, rest, nil
		}
		// No provider matched, so the slash belongs to the model id:
		// openrouter-style ids look exactly like "vendor/model".
	}

	p, ok := r.Default()
	if !ok {
		return nil, "", fmt.Errorf("%w (use provider/model, e.g. %s)", ErrNoDefaultProvider, r.exampleRef())
	}
	return p, requested, nil
}

func (r *Registry) exampleRef() string {
	if len(r.order) == 0 {
		return "provider/model"
	}
	id := r.order[0]
	return id + "/" + modelExample(r.providers[id])
}

func modelExample(p *Provider) string {
	for _, m := range p.Models {
		if m.ID != "" {
			return m.ID
		}
	}
	return "model"
}

// Connection returns the credential used for the next request.
//
// Phase 2 always uses the first enabled credential; phase 5 adds rotation.
func (p *Provider) Connection() (Connection, error) {
	if len(p.connections) == 0 {
		return Connection{}, fmt.Errorf("provider %q has no credentials", p.ID)
	}
	return p.connections[0], nil
}

// Connections exposes the credential list (phase 5 needs it).
func (p *Provider) Connections() []Connection {
	return p.connections
}

// Endpoint resolves the URL a request is sent to for the given provider.
//
// The connection may override the host (self-hosted gateways), so it is
// assembled per request rather than cached.
func (p *Provider) Endpoint(c Connection) (string, error) {
	base := p.BaseURL
	if c.BaseURL != "" {
		base = c.BaseURL
	}
	if base == "" {
		return "", fmt.Errorf("provider %q has no base_url", p.ID)
	}
	if err := validateEndpoint(base); err != nil {
		return "", fmt.Errorf("provider %q base_url: %w", p.ID, err)
	}
	return base, nil
}

// SupportsPassthrough reports whether a chat-completions body can be forwarded
// unchanged. Only same-format providers qualify; translation arrives in phase 4.
func (p *Provider) SupportsPassthrough() bool {
	return p.Format == FormatChat
}
