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
	"sync/atomic"
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
		Models: []string{
			"claude-sonnet-4-5", "claude-opus-4-1", "claude-haiku-4-5",
			"claude-3-7-sonnet-latest", "claude-3-5-haiku-latest",
		},
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
	for _, b := range catalog {
		// Catalog names are reserved even when no connection uses them yet, so
		// an alias must not shadow "cc" or "oa".
		known[b.ID] = true
		if b.Alias != "" {
			known[b.Alias] = true
		}
	}
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

	validateShortcuts(cfg, known, add)

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

// validateShortcuts checks that every alias and combo target exists.
//
// A shortcut that names a provider nobody configured is not an error here:
// whether a credential is present is reported by the connection checks, and a
// shortcut may legitimately sit ready for a provider that is added later. A
// typo in the provider name, or an alias that points at itself, is an error
// because no credential can ever fix it.
func validateShortcuts(cfg *config.Config, known map[string]bool, add func(string, ...any)) {
	effective := Materialize(cfg.Providers, cfg.Connections)

	names := make([]string, 0, len(cfg.Aliases))
	for name := range cfg.Aliases {
		names = append(names, name)
	}
	sort.Strings(names) // stable error order: map iteration is randomized

	comboNames := make(map[string]bool, len(cfg.Combos))
	for _, cb := range cfg.Combos {
		comboNames[cb.Name] = true
	}
	aliasNames := make(map[string]bool, len(cfg.Aliases))
	for name := range cfg.Aliases {
		aliasNames[name] = true
	}

	for _, name := range names {
		where := fmt.Sprintf("aliases[%q]", name)
		// config validation already rejects a collision with a declared provider
		// id or alias; this one also covers catalog aliases the config never
		// mentions ("cc"), which only this package knows about.
		if known[name] {
			add("%s: the name collides with a provider id or alias", where)
		}
		if comboNames[name] {
			add("%s: the name collides with a combo", where)
		}
		if target := strings.TrimSpace(cfg.Aliases[name]); target == "" ||
			strings.Contains(name, "/") {
			continue // reported by config validation already
		}
		if _, _, err := resolveConfigRef(effective, cfg.Aliases, name, map[string]bool{}); err != nil {
			add("%s: %v", where, err)
		}
	}

	for i := range cfg.Combos {
		cb := &cfg.Combos[i]
		where := fmt.Sprintf("combos[%d] (%q)", i, cb.Name)
		if known[cb.Name] {
			add("%s: the name collides with a provider id or alias", where)
		}
		if aliasNames[cb.Name] {
			add("%s: the name collides with an alias", where)
		}
		for j, member := range cb.Members {
			member = strings.TrimSpace(member)
			if member == "" {
				continue // reported by config validation already
			}
			if _, isCombo := comboByName(cfg, member); isCombo {
				add("%s: members[%d] %q names a combo; nesting is not supported", where, j, member)
				continue
			}
			if _, _, err := resolveConfigRef(effective, cfg.Aliases, member, map[string]bool{}); err != nil {
				add("%s: members[%d]: %v", where, j, err)
			}
		}
	}
}

func comboByName(cfg *config.Config, name string) (config.Combo, bool) {
	for _, cb := range cfg.Combos {
		if cb.Name == name {
			return cb, true
		}
	}
	return config.Combo{}, false
}

// resolveConfigRef resolves a routing target against the materialized provider
// set, without requiring a usable credential.
//
// Accepted: "provider/model", "provider/" (the provider's first advertised
// model) and an alias name, recursively. A bare model name is refused: it would
// depend on default_provider, which a configured target must never do silently.
func resolveConfigRef(providers []config.Provider, aliases map[string]string, ref string, seen map[string]bool) (provider, model string, err error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", "", errors.New("target is empty")
	}

	if prefix, rest, ok := strings.Cut(ref, "/"); ok {
		if p := findProvider(providers, prefix); p != nil {
			if rest != "" {
				return p.ID, rest, nil
			}
			if len(p.Models) == 0 {
				return "", "", fmt.Errorf("provider %q advertises no model to default to", p.ID)
			}
			return p.ID, p.Models[0].ID, nil
		}
		// A catalog provider is a valid reference even before its connection is
		// written: the shortcut then sits ready instead of failing the boot.
		b, found := CatalogByRef(prefix)
		if !found {
			return "", "", fmt.Errorf("unknown provider %q", prefix)
		}
		if rest != "" {
			return b.ID, rest, nil
		}
		if len(b.Models) == 0 {
			return "", "", fmt.Errorf("provider %q advertises no model to default to", b.ID)
		}
		return b.ID, b.Models[0], nil
	}

	target, ok := aliases[ref]
	if !ok {
		return "", "", fmt.Errorf("%q is not \"provider/model\" and is not an alias", ref)
	}
	if seen[ref] {
		return "", "", fmt.Errorf("alias cycle at %q", ref)
	}
	seen[ref] = true
	provider, model, err = resolveConfigRef(providers, aliases, target, seen)
	if err != nil {
		return "", "", fmt.Errorf("alias %q: %w", ref, err)
	}
	return provider, model, nil
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

	// aliases and combos are the config's routing shortcuts, kept unresolved:
	// a target is only meaningful at request time, when the set of usable
	// providers is already fixed in this snapshot.
	aliases map[string]string
	combos  map[string]*Combo
}

// Combo strategies.
const (
	// StrategyPriority tries members in declaration order.
	StrategyPriority = "priority"
	// StrategyRoundRobin rotates the starting member per request.
	StrategyRoundRobin = "round-robin"
)

// Combo is a named group of routing targets tried in order.
type Combo struct {
	Name     string
	Members  []string
	Strategy string
	// next is the round-robin cursor. Atomic because one snapshot serves
	// concurrent requests; a reload starts again at member zero.
	next atomic.Uint64
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

	r.compileShortcuts(cfg)
	return r, nil
}

// compileShortcuts copies the routing shortcuts into the snapshot.
//
// They are stored as declared and resolved per request: a provider without a
// credential today may get one on the next reload, and resolving lazily lets a
// request distinguish "unknown model" from "provider has no connection".
func (r *Registry) compileShortcuts(cfg *config.Config) {
	r.aliases = make(map[string]string, len(cfg.Aliases))
	for name, target := range cfg.Aliases {
		r.aliases[name] = target
	}

	r.combos = make(map[string]*Combo, len(cfg.Combos))
	for _, cb := range cfg.Combos {
		r.combos[cb.Name] = &Combo{
			Name:     cb.Name,
			Members:  append([]string(nil), cb.Members...),
			Strategy: cb.Strategy,
		}
	}
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

// Target is a routing decision: which provider answers, with which model id.
type Target struct {
	Provider *Provider
	Model    string
	// Alias names the alias that produced this target, if any.
	Alias string
	// Combo names the combo that produced this target, if any.
	Combo string
}

// Resolve maps a client model string to a provider and upstream model id.
func (r *Registry) Resolve(requested string) (*Provider, string, error) {
	t, err := r.Lookup(requested)
	if err != nil {
		return nil, "", err
	}
	return t.Provider, t.Model, nil
}

// Lookup resolves a client model string to the first attempt, reporting which
// alias or combo matched so a caller can log or echo the client-facing name.
//
// Accepted forms, most specific first:
//   - "provider/model" or "alias/model"
//   - a combo name from config.combos
//   - an alias name from config.aliases
//   - "provider/" for that provider's first advertised model
//   - a bare model whose prefix identifies a provider ("gpt-*" -> openai)
//   - a bare model routed to default_provider
func (r *Registry) Lookup(requested string) (Target, error) {
	plan, err := r.Plan(requested)
	if err != nil {
		return Target{}, err
	}
	first := plan.Attempts[0]
	return Target{
		Provider: first.Provider,
		Model:    first.Model,
		Alias:    plan.Alias,
		Combo:    plan.Combo,
	}, nil
}

// Attempt is one provider/model pair a request may try.
type Attempt struct {
	Provider *Provider
	Model    string
}

// Plan is the ordered list of attempts for one request. A plain model has one
// attempt; a combo lists every usable member so the caller can retry after a
// failure. The order is already rotated for a round-robin combo.
type Plan struct {
	Requested string
	Alias     string
	Combo     string
	Attempts  []Attempt
}

// Plan resolves a client model string without committing to a single provider.
func (r *Registry) Plan(requested string) (Plan, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return Plan{}, errors.New("model is required")
	}
	return r.planModel(requested, make(map[string]bool, 2))
}

func (r *Registry) planModel(requested string, seen map[string]bool) (Plan, error) {
	single := func(t Target, alias, combo string) (Plan, error) {
		return Plan{
			Requested: requested,
			Alias:     alias,
			Combo:     combo,
			Attempts:  []Attempt{{Provider: t.Provider, Model: t.Model}},
		}, nil
	}

	if prefix, rest, found := strings.Cut(requested, "/"); found {
		if p, ok := r.Provider(prefix); ok {
			t, err := modelTarget(p, rest, prefix)
			if err != nil {
				return Plan{}, err
			}
			return single(t, "", "")
		}
		// The prefix is not a provider, so the slash belongs to the model id:
		// openrouter-style ids look exactly like "vendor/model". Only the
		// default provider can take it; prefix inference must not guess here,
		// or "deepseek-ai/DeepSeek-V3" behind an OpenRouter default would be
		// sent to DeepSeek with its vendor prefix doubled.
		t, err := r.defaultTarget(requested)
		if err != nil {
			return Plan{}, err
		}
		return single(t, "", "")
	}

	if target, ok := r.aliases[requested]; ok {
		t, err := r.resolveTarget(target, seen)
		if err != nil {
			return Plan{}, fmt.Errorf("alias %q -> %q: %w", requested, target, err)
		}
		return single(t, requested, "")
	}

	if cb, ok := r.combos[requested]; ok {
		return r.planCombo(cb)
	}

	if p, ok := r.inferProvider(requested); ok {
		return single(Target{Provider: p, Model: requested}, "", "")
	}

	t, err := r.defaultTarget(requested)
	if err != nil {
		return Plan{}, err
	}
	return single(t, "", "")
}

// defaultTarget routes a bare model name to default_provider.
func (r *Registry) defaultTarget(model string) (Target, error) {
	p, ok := r.Default()
	if !ok {
		return Target{}, fmt.Errorf("%w (use provider/model, e.g. %s)", ErrNoDefaultProvider, r.exampleRef())
	}
	return Target{Provider: p, Model: model}, nil
}

// planCombo lists the members that can serve this request, in the order they
// should be tried.
//
// Only members whose provider has a usable credential are candidates, so a
// combo keeps working when one provider is unconfigured. A round-robin combo
// rotates the starting point per request; the caller retries the rest in order.
func (r *Registry) planCombo(cb *Combo) (Plan, error) {
	usable := make([]Attempt, 0, len(cb.Members))
	var lastErr error
	for _, member := range cb.Members {
		t, err := r.resolveTarget(member, make(map[string]bool, 2))
		if err != nil {
			lastErr = err
			continue
		}
		usable = append(usable, Attempt{Provider: t.Provider, Model: t.Model})
	}
	if len(usable) == 0 {
		if lastErr == nil {
			lastErr = errors.New("it lists no members")
		}
		return Plan{}, fmt.Errorf("combo %q has no usable member: %v", cb.Name, lastErr)
	}

	if cb.Strategy == StrategyRoundRobin {
		// Add(1)-1 makes the first request after a reload use member zero.
		idx := int((cb.next.Add(1) - 1) % uint64(len(usable)))
		usable = append(usable[idx:], usable[:idx]...)
	}
	return Plan{Requested: cb.Name, Combo: cb.Name, Attempts: usable}, nil
}

// resolveTarget resolves a configured target: it must reach a usable provider,
// directly or through another alias. Unlike lookupModel it never falls back to
// prefix inference or default_provider, because a configured target that cannot
// be honoured should fail loudly rather than silently pick another provider.
func (r *Registry) resolveTarget(ref string, seen map[string]bool) (Target, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Target{}, errors.New("target is empty")
	}

	if prefix, rest, found := strings.Cut(ref, "/"); found {
		p, ok := r.Provider(prefix)
		if !ok {
			return Target{}, fmt.Errorf("provider %q has no enabled connection", prefix)
		}
		return modelTarget(p, rest, prefix)
	}

	if target, ok := r.aliases[ref]; ok {
		if seen[ref] {
			return Target{}, fmt.Errorf("alias cycle at %q", ref)
		}
		seen[ref] = true
		t, err := r.resolveTarget(target, seen)
		if err != nil {
			return Target{}, fmt.Errorf("alias %q: %w", ref, err)
		}
		return t, nil
	}

	return Target{}, fmt.Errorf("%q is not provider/model and is not a known alias", ref)
}

// modelTarget pairs a provider with a model id. An empty id means "the first
// model this provider advertises", which is how "anthropic/" names a target.
func modelTarget(p *Provider, model, prefix string) (Target, error) {
	if model == "" {
		id, ok := firstModel(p)
		if !ok {
			return Target{}, fmt.Errorf("provider %q advertises no models; use %s/<model>", p.ID, prefix)
		}
		model = id
	}
	return Target{Provider: p, Model: model}, nil
}

// inferenceRules map a bare model name to the provider that serves it.
//
// The prefixes identify the vendor unambiguously ("gpt-4o" is only served by
// [OI]), so a client that speaks a vendor dialect works without a config
// alias. A rule only applies when that provider is usable: with no [OI]
// credential "gpt-4o" falls through to default_provider exactly as before.
//
// Deliberately absent: llama/qwen/mistral prefixes and anything else several
// providers serve under the same name. Guessing there would send a local Ollama
// model to a cloud provider and fail in a confusing way. A name containing "/"
// is never inferred either: that shape is an OpenRouter-style model id.
var inferenceRules = []struct {
	provider string
	prefixes []string
}{
	{provider: "openai", prefixes: []string{
		"gpt-", "chatgpt-", "o1-", "o3-", "o4-", "text-embedding-",
	}},
	{provider: "anthropic", prefixes: []string{"claude-"}},
	{provider: "gemini", prefixes: []string{"gemini-"}},
	{provider: "deepseek", prefixes: []string{"deepseek-"}},
}

func (r *Registry) inferProvider(model string) (*Provider, bool) {
	for _, rule := range inferenceRules {
		for _, prefix := range rule.prefixes {
			if strings.HasPrefix(model, prefix) {
				return r.Provider(rule.provider)
			}
		}
	}
	return nil, false
}

// firstModel returns the first advertised model id of a provider.
func firstModel(p *Provider) (string, bool) {
	for _, m := range p.Models {
		if m.ID != "" {
			return m.ID, true
		}
	}
	return "", false
}

// AliasInfo is a configured alias with its resolved target.
type AliasInfo struct {
	Name   string
	Target Target
}

// Aliases reports the aliases that resolve with the current credentials, in
// name order. An alias pointing at a provider with no connection is left out:
// advertising it would offer a model the proxy cannot serve.
func (r *Registry) Aliases() []AliasInfo {
	out := make([]AliasInfo, 0, len(r.aliases))
	for name := range r.aliases {
		t, err := r.Lookup(name)
		if err != nil {
			continue
		}
		out = append(out, AliasInfo{Name: name, Target: t})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ComboInfo is a configured combo with its usable members.
type ComboInfo struct {
	Name     string
	Strategy string
	Members  []Target
}

// Combos reports the combos with at least one usable member, in name order.
func (r *Registry) Combos() []ComboInfo {
	out := make([]ComboInfo, 0, len(r.combos))
	for _, c := range r.combos {
		info := ComboInfo{Name: c.Name, Strategy: c.Strategy}
		for _, member := range c.Members {
			t, err := r.resolveTarget(member, make(map[string]bool, 2))
			if err != nil {
				continue
			}
			info.Members = append(info.Members, t)
		}
		if len(info.Members) > 0 {
			out = append(out, info)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Shortcut resolves a name only if it is a configured alias or combo, never
// falling back to inference or the default provider. "usable" is false for a
// shortcut that exists but currently has no provider to route to.
func (r *Registry) Shortcut(name string) (t Target, usable bool) {
	_, isAlias := r.aliases[name]
	_, isCombo := r.combos[name]
	if !isAlias && !isCombo {
		return Target{}, false
	}
	t, err := r.Lookup(name)
	return t, err == nil
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
