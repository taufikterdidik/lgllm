package configcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"lgllm/internal/config"
	"lgllm/internal/registry"
	"lgllm/internal/sessionid"
)

// modelItem is the union of the shapes providers use for a model listing.
type modelItem struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Model string `json:"model"`
}

func (m modelItem) ref() string {
	switch {
	case strings.TrimSpace(m.ID) != "":
		return strings.TrimSpace(m.ID)
	case strings.TrimSpace(m.Model) != "":
		return strings.TrimSpace(m.Model)
	default:
		return strings.TrimSpace(m.Name)
	}
}

// fetchModels calls a /models endpoint and returns unique ids in provider order.
func fetchModels(ctx context.Context, endpoint, auth, authParam, apiKey string,
	headers map[string]string, sessionHeader, session string, timeout time.Duration) ([]string, error) {

	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("models URL %q: %w", endpoint, err)
	}
	if auth == "query" {
		if authParam == "" {
			return nil, fmt.Errorf("provider uses query auth but has no auth_param")
		}
		q := u.Query()
		q.Set(authParam, apiKey)
		u.RawQuery = q.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "lgllm-config")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if sessionHeader != "" && session != "" {
		req.Header.Set(sessionHeader, sessionid.Sanitize(session))
	}
	switch auth {
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+apiKey)
	case "x-api-key":
		req.Header.Set("x-api-key", apiKey)
	}

	client := &http.Client{Timeout: timeout}
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", safeURL(u), err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", safeURL(u), err)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		snippet := strings.TrimSpace(string(body))
		if len(snippet) > 300 {
			snippet = snippet[:300] + "..."
		}
		return nil, fmt.Errorf("GET %s: status %d: %s", safeURL(u), res.StatusCode, snippet)
	}
	models, err := decodeModelList(body)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", safeURL(u), err)
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("GET %s returned no models", safeURL(u))
	}
	return models, nil
}

// safeURL redacts every query value: query auth puts the credential there.
func safeURL(u *url.URL) string {
	cp := *u
	if cp.RawQuery != "" {
		q := cp.Query()
		for key := range q {
			q.Set(key, "REDACTED")
		}
		cp.RawQuery = q.Encode()
	}
	return cp.Redacted()
}

// decodeModelList accepts the OpenAI shape ({"data":[...]}), the Ollama shape
// ({"models":[...]}) and a bare array.
func decodeModelList(body []byte) ([]string, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("empty response body")
	}
	var items []modelItem
	if trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, fmt.Errorf("decode model list: %w", err)
		}
	} else {
		var payload struct {
			Data   []modelItem `json:"data"`
			Models []modelItem `json:"models"`
		}
		if err := json.Unmarshal(trimmed, &payload); err != nil {
			return nil, fmt.Errorf("decode model list: %w", err)
		}
		items = payload.Data
		if len(items) == 0 {
			items = payload.Models
		}
	}

	seen := make(map[string]bool, len(items))
	out := make([]string, 0, len(items))
	for _, item := range items {
		id := item.ref()
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out, nil
}

// discoverFromConfig loads the runtime view (including admin.yaml and decrypted
// secrets), picks a connection, and asks the provider for its model list.
func discoverFromConfig(path, masterKey, providerRef, connectionID, explicitURL string,
	timeout time.Duration) ([]string, string, error) {

	cfg, err := config.LoadWithOverrides(path, config.Overrides{MasterKey: masterKey})
	if err != nil {
		return nil, "", err
	}
	effective := registry.Materialize(cfg.Providers, cfg.Connections)

	// With only --id, derive the provider from the connection.
	if strings.TrimSpace(providerRef) == "" && connectionID != "" {
		conn, _ := findConnection(cfg, connectionID)
		if conn == nil {
			return nil, "", fmt.Errorf("connection %q does not exist", connectionID)
		}
		providerRef = conn.Provider
	}
	p := findEffectiveProvider(effective, providerRef)
	if p == nil {
		return nil, "", fmt.Errorf("provider %q is not configured", providerRef)
	}

	var conn *config.Connection
	if connectionID != "" {
		conn, _ = findConnection(cfg, connectionID)
		if conn == nil {
			return nil, "", fmt.Errorf("connection %q does not exist", connectionID)
		}
		if canonicalProvider(conn.Provider) != p.ID {
			return nil, "", fmt.Errorf("connection %q belongs to provider %q, not %q", connectionID, conn.Provider, p.ID)
		}
	} else {
		conn = providerConnection(cfg, p.ID)
		if conn == nil {
			return nil, "", fmt.Errorf("provider %q has no enabled connection; add one first", p.ID)
		}
	}

	base := strings.TrimRight(conn.BaseURL, "/")
	if base == "" {
		base = strings.TrimRight(p.BaseURL, "/")
	}
	endpoint := explicitURL
	if endpoint == "" {
		if base == "" {
			return nil, "", fmt.Errorf("provider %q has no base_url", p.ID)
		}
		endpoint = base + "/models"
	}
	if p.Auth != "none" && conn.APIKey == "" {
		return nil, "", fmt.Errorf("connection %q has no api_key", conn.ID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	session := ""
	if p.SessionHeader != "" {
		session = sessionid.Fingerprint("config:" + p.ID + ":" + conn.ID)
	}
	models, err := fetchModels(ctx, endpoint, p.Auth, p.AuthParam, conn.APIKey,
		p.Headers, p.SessionHeader, session, timeout)
	if err != nil {
		return nil, "", err
	}
	return models, fmt.Sprintf("%s via connection %s", endpoint, conn.ID), nil
}

func cmdModels(args []string, g globals, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: lgllm config models list|sync")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list", "ls":
		return modelsList(rest, g, stdout)
	case "sync", "update", "refresh":
		return modelsSync(rest, g, stdout)
	default:
		return fmt.Errorf("unknown models command %q (want list|sync)", sub)
	}
}

func modelsList(args []string, g globals, stdout io.Writer) error {
	fs := newFlagSet("config models list")
	var (
		provider = fs.String("provider", "", "provider id or alias")
		connID   = fs.String("connection", "", "connection id for --upstream")
		upstream = fs.Bool("upstream", false, "query the provider instead of the config")
		url      = fs.String("url", "", "override the models endpoint URL")
		timeout  = fs.Duration("timeout", 20*time.Second, "request timeout for --upstream")
	)
	positional, err := parsePositional(fs, args)
	if err != nil {
		return err
	}
	if *provider == "" {
		*provider = positional
	}

	if *upstream {
		if strings.TrimSpace(*provider) == "" && *connID == "" {
			return fmt.Errorf("models list --upstream needs --provider or --connection")
		}
		models, detail, err := discoverFromConfig(g.configPath, g.masterKey, *provider, *connID, *url, *timeout)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "upstream %s advertised %d model(s):\n", detail, len(models))
		for _, id := range models {
			fmt.Fprintln(stdout, "  "+id)
		}
		return nil
	}

	doc, err := loadDocument(g.configPath)
	if err != nil {
		return err
	}
	cfg, err := doc.rawConfig()
	if err != nil {
		return err
	}
	effective := registry.Materialize(cfg.Providers, cfg.Connections)
	var chosen []config.Provider
	if strings.TrimSpace(*provider) == "" {
		chosen = effective
	} else {
		p := findEffectiveProvider(effective, *provider)
		if p == nil {
			return fmt.Errorf("provider %q is not configured", *provider)
		}
		chosen = []config.Provider{*p}
	}
	sort.Slice(chosen, func(i, j int) bool { return chosen[i].ID < chosen[j].ID })
	if len(chosen) == 0 {
		fmt.Fprintln(stdout, "no providers yet")
		return nil
	}
	for _, p := range chosen {
		fmt.Fprintf(stdout, "%s (%d):\n", p.ID, len(p.Models))
		for _, m := range p.Models {
			var extra []string
			if m.Context > 0 {
				extra = append(extra, fmt.Sprintf("context=%d", m.Context))
			}
			if len(m.Capabilities) > 0 {
				keys := make([]string, 0, len(m.Capabilities))
				for k, on := range m.Capabilities {
					if on {
						keys = append(keys, k)
					}
				}
				sort.Strings(keys)
				if len(keys) > 0 {
					extra = append(extra, "capabilities="+strings.Join(keys, ","))
				}
			}
			if len(extra) > 0 {
				fmt.Fprintf(stdout, "  %-40s %s\n", m.ID, strings.Join(extra, " "))
			} else {
				fmt.Fprintf(stdout, "  %s\n", m.ID)
			}
		}
	}
	return nil
}

func modelsSync(args []string, g globals, stdout io.Writer) error {
	fs := newFlagSet("config models sync")
	var (
		provider = fs.String("provider", "", "provider id or alias")
		connID   = fs.String("connection", "", "connection id (default: first enabled for the provider)")
		url      = fs.String("url", "", "override the models endpoint URL")
		timeout  = fs.Duration("timeout", 20*time.Second, "request timeout")
		replace  = fs.Bool("replace", false, "drop configured models the provider no longer advertises")
		dryRun   = fs.Bool("dry-run", false, "print the change without writing the file")
	)
	positional, err := parsePositional(fs, args)
	if err != nil {
		return err
	}
	if *provider == "" {
		*provider = positional
	}
	if strings.TrimSpace(*provider) == "" {
		return fmt.Errorf("models sync needs --provider")
	}

	models, detail, err := discoverFromConfig(g.configPath, g.masterKey, *provider, *connID, *url, *timeout)
	if err != nil {
		return err
	}

	doc, err := loadDocument(g.configPath)
	if err != nil {
		return err
	}
	cfg, err := doc.rawConfig()
	if err != nil {
		return err
	}
	p := findEffectiveProvider(registry.Materialize(cfg.Providers, cfg.Connections), *provider)
	wantID := canonicalProvider(*provider)
	if p != nil {
		wantID = p.ID
	}
	if st, err := loadAdminOverlay(g.configPath); err != nil {
		return err
	} else {
		for _, ap := range st.Providers {
			if canonicalProvider(ap.ID) == wantID {
				return fmt.Errorf("provider %q is managed in the admin state file; sync its models through the admin API", wantID)
			}
		}
	}
	if p == nil {
		return fmt.Errorf("provider %q is not configured", *provider)
	}

	entry := configProviderEntry(doc.root(), p.ID)
	var existing *yaml.Node
	if entry != nil {
		existing = mappingGet(entry, "models")
	}
	seq, added, kept, removed := mergeModels(existing, models, *replace)

	if *dryRun {
		fmt.Fprintf(stdout, "would sync %d model(s) for %s from %s (%d new, %d kept, %d removed)\n",
			len(seq.Content), p.ID, detail, added, kept, removed)
		for _, item := range seq.Content {
			fmt.Fprintf(stdout, "  %s\n", mappingString(item, "id"))
		}
		return nil
	}

	if entry == nil {
		entry, err = ensureProviderEntry(doc.root(), p.ID)
		if err != nil {
			return err
		}
	}
	mappingSet(entry, "models", seq)
	warnings, err := doc.save(false, stdout)
	if err != nil {
		return err
	}
	printWarnings(stdout, warnings)
	fmt.Fprintf(stdout, "synced %d model(s) for %s from %s (%d new, %d kept, %d removed) into %s\n",
		len(seq.Content), p.ID, detail, added, kept, removed, g.configPath)
	fmt.Fprintln(stdout, "the running server applies the change on the next config poll (or send SIGHUP)")
	return nil
}

// mergeModels rebuilds a models sequence. Merge keeps configured entries and
// their metadata (name, context, capabilities) and appends new upstream ids;
// replace drops ids the provider no longer lists.
func mergeModels(existing *yaml.Node, upstream []string, replace bool) (seq *yaml.Node, added, kept, removed int) {
	old := map[string]*yaml.Node{}
	var oldOrder []string
	if existing != nil && existing.Kind == yaml.SequenceNode {
		for _, item := range existing.Content {
			if item.Kind != yaml.MappingNode {
				continue
			}
			id := mappingString(item, "id")
			if id == "" || old[id] != nil {
				continue
			}
			old[id] = item
			oldOrder = append(oldOrder, id)
		}
	}
	upstreamSet := make(map[string]bool, len(upstream))
	for _, id := range upstream {
		upstreamSet[id] = true
	}

	var order []string
	if replace {
		order = append(order, upstream...)
	} else {
		order = append(order, oldOrder...)
		for _, id := range upstream {
			if old[id] == nil {
				order = append(order, id)
			}
		}
	}

	seq = newSeq()
	for _, id := range order {
		if prev := old[id]; prev != nil {
			item := cloneNode(prev)
			mappingSet(item, "id", scalar(id))
			seq.Content = append(seq.Content, item)
			kept++
			continue
		}
		item := newMap()
		mappingSet(item, "id", scalar(id))
		seq.Content = append(seq.Content, item)
		added++
	}
	if replace {
		for _, id := range oldOrder {
			if !upstreamSet[id] {
				removed++
			}
		}
	}
	return seq, added, kept, removed
}
