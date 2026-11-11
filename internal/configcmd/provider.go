package configcmd

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"lgllm/internal/registry"
)

func cmdProvider(args []string, g globals, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: lgllm config provider list|add|set|remove")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list", "ls":
		return providerList(rest, g, stdout)
	case "add":
		return providerAdd(rest, g, stdout)
	case "set", "update":
		return providerSet(rest, g, stdout)
	case "remove", "rm", "delete":
		return providerRemove(rest, g, stdout)
	default:
		return fmt.Errorf("unknown provider command %q (want list|add|set|remove)", sub)
	}
}

func providerList(args []string, g globals, stdout io.Writer) error {
	fs := newFlagSet("config provider list")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("provider list takes no positional arguments")
	}
	doc, err := loadDocument(g.configPath)
	if err != nil {
		return err
	}
	cfg, err := doc.rawConfig()
	if err != nil {
		return err
	}
	list := registry.Materialize(cfg.Providers, cfg.Connections)
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	if len(list) == 0 {
		fmt.Fprintln(stdout, "no providers yet; add a connection or a provider")
		return nil
	}
	fmt.Fprintf(stdout, "%-16s %-6s %-18s %-10s %-6s %s\n", "ID", "ALIAS", "FORMAT", "AUTH", "MODELS", "BASE URL")
	for _, p := range list {
		alias := p.Alias
		if alias == "" {
			alias = "-"
		}
		fmt.Fprintf(stdout, "%-16s %-6s %-18s %-10s %-6d %s\n",
			p.ID, alias, p.Format, p.Auth, len(p.Models), p.BaseURL)
	}
	return nil
}

type providerOptions struct {
	alias        string
	baseURL      string
	format       string
	auth         string
	authParam    string
	timeout      string
	headers      []string
	models       []string
	disabled     bool
	enabled      bool
	clearHeaders bool
}

func providerAdd(args []string, g globals, stdout io.Writer) error {
	fs := newFlagSet("config provider add")
	var (
		id        = fs.String("id", "", "provider id (catalog id or a new custom id)")
		alias     = fs.String("alias", "", "short prefix used in model names")
		baseURL   = fs.String("base-url", "", "API root including the version segment")
		format    = fs.String("format", "", "openai|anthropic|gemini|openai-responses|ollama")
		auth      = fs.String("auth", "", "bearer|x-api-key|query|none")
		authParam = fs.String("auth-param", "", "query parameter for auth=query")
		timeout   = fs.String("timeout", "", "per-provider response header timeout, e.g. 60s")
		models    csvList
		headers   stringList
	)
	fs.Var(&models, "model", "advertised model id (repeatable, comma list accepted)")
	fs.Var(&headers, "header", "extra upstream header Name=Value (repeatable)")
	positional, err := parsePositional(fs, args)
	if err != nil {
		return err
	}
	if *id == "" {
		*id = positional
	} else if positional != "" {
		return fmt.Errorf("provider add takes either --id or one positional id, not both")
	}
	*id = strings.TrimSpace(*id)
	if *id == "" {
		return fmt.Errorf(`provider add needs an id: lgllm config provider add openai`)
	}

	canonical := canonicalProvider(*id)
	_, cataloged := registry.CatalogByRef(canonical)

	doc, err := loadDocument(g.configPath)
	if err != nil {
		return err
	}
	root := doc.root()
	if configProviderEntry(root, canonical) != nil {
		return fmt.Errorf("provider %q already has a config entry (use `provider set`)", canonical)
	}
	if st, err := loadAdminOverlay(g.configPath); err != nil {
		return err
	} else {
		for _, p := range st.Providers {
			if canonicalProvider(p.ID) == canonical {
				return fmt.Errorf("provider %q is managed in the admin state file; edit it through the admin API", canonical)
			}
		}
	}
	if !cataloged && strings.TrimSpace(*baseURL) == "" {
		return fmt.Errorf("provider %q is not in the built-in catalog; --base-url is required", canonical)
	}

	entry := newMap()
	mappingSet(entry, "id", scalar(canonical))
	opts := providerOptions{
		alias: *alias, baseURL: *baseURL, format: *format, auth: *auth,
		authParam: *authParam, timeout: *timeout, headers: headers, models: models,
	}
	if err := applyProviderOptions(entry, opts, changedFlags(fs)); err != nil {
		return err
	}
	seq, err := ensureSequence(root, "providers")
	if err != nil {
		return err
	}
	seq.Content = append(seq.Content, entry)

	warnings, err := doc.save(false, stdout)
	if err != nil {
		return err
	}
	printWarnings(stdout, warnings)
	fmt.Fprintf(stdout, "added provider %s to %s\n", canonical, g.configPath)
	if !cataloged {
		fmt.Fprintf(stdout, "next: lgllm config connection add --provider %s --api-key '${%s_API_KEY}'\n",
			canonical, strings.ToUpper(strings.ReplaceAll(canonical, "-", "_")))
	}
	return nil
}

func providerSet(args []string, g globals, stdout io.Writer) error {
	fs := newFlagSet("config provider set")
	var (
		id        = fs.String("id", "", "provider id or alias")
		alias     = fs.String("alias", "", "short prefix (empty removes it)")
		baseURL   = fs.String("base-url", "", "API root including the version segment (empty removes it)")
		format    = fs.String("format", "", "openai|anthropic|gemini|openai-responses|ollama")
		auth      = fs.String("auth", "", "bearer|x-api-key|query|none")
		authParam = fs.String("auth-param", "", "query parameter for auth=query")
		timeout   = fs.String("timeout", "", "per-provider response header timeout (empty removes it)")
		models    csvList
		headers   stringList
		disabled  = fs.Bool("disabled", false, "disable the provider")
		enabled   = fs.Bool("enabled", false, "enable the provider")
		clear     = fs.Bool("clear-headers", false, "remove every custom header")
	)
	fs.Var(&models, "model", "replace the advertised models (repeatable, comma list accepted)")
	fs.Var(&headers, "header", "set an extra upstream header Name=Value (repeatable)")
	positional, err := parsePositional(fs, args)
	if err != nil {
		return err
	}
	if *id == "" {
		*id = positional
	}
	if strings.TrimSpace(*id) == "" {
		return fmt.Errorf("provider set needs --id (or a positional provider id)")
	}
	if *disabled && *enabled {
		return fmt.Errorf("use only one of --disabled and --enabled")
	}

	doc, err := loadDocument(g.configPath)
	if err != nil {
		return err
	}
	root := doc.root()
	cfg, err := doc.rawConfig()
	if err != nil {
		return err
	}
	if st, err := loadAdminOverlay(g.configPath); err != nil {
		return err
	} else {
		for _, p := range st.Providers {
			if canonicalProvider(p.ID) == canonicalProvider(*id) {
				return fmt.Errorf("provider %q is managed in the admin state file; edit it through the admin API", *id)
			}
		}
	}
	if resolveProvider(registry.Materialize(cfg.Providers, cfg.Connections), *id) == nil {
		return fmt.Errorf("provider %q is not configured (use `provider add`)", *id)
	}
	entry, err := ensureProviderEntry(root, canonicalProvider(*id))
	if err != nil {
		return err
	}
	opts := providerOptions{
		alias: *alias, baseURL: *baseURL, format: *format, auth: *auth,
		authParam: *authParam, timeout: *timeout, headers: headers, models: models,
		disabled: *disabled, enabled: *enabled, clearHeaders: *clear,
	}
	if err := applyProviderOptions(entry, opts, changedFlags(fs)); err != nil {
		return err
	}

	warnings, err := doc.save(false, stdout)
	if err != nil {
		return err
	}
	printWarnings(stdout, warnings)
	fmt.Fprintf(stdout, "updated provider %s in %s\n", canonicalProvider(*id), g.configPath)
	return nil
}

func providerRemove(args []string, g globals, stdout io.Writer) error {
	fs := newFlagSet("config provider remove")
	id := fs.String("id", "", "provider id or alias")
	force := fs.Bool("force", false, "remove even when credentials still reference the provider")
	positional, err := parsePositional(fs, args)
	if err != nil {
		return err
	}
	if *id == "" {
		*id = positional
	}
	if strings.TrimSpace(*id) == "" {
		return fmt.Errorf("provider remove needs an id")
	}

	doc, err := loadDocument(g.configPath)
	if err != nil {
		return err
	}
	root := doc.root()
	seq := mappingGet(root, "providers")
	entry, index := sequenceFind(seq, "id", *id)
	if entry == nil {
		entry, index = sequenceFind(seq, "id", canonicalProvider(*id))
	}
	if entry == nil {
		// Also match an alias, which may differ from the catalog alias.
		if seq != nil {
			for i, item := range seq.Content {
				if item.Kind == yaml.MappingNode && mappingString(item, "alias") == strings.TrimSpace(*id) {
					entry, index = item, i
					break
				}
			}
		}
	}
	if entry == nil {
		if _, ok := registry.CatalogByRef(*id); ok {
			return fmt.Errorf("provider %q is built in and has no config entry to remove", *id)
		}
		return fmt.Errorf("provider %q has no config entry", *id)
	}

	if !*force {
		cfg, err := doc.rawConfig()
		if err != nil {
			return err
		}
		providerID := mappingString(entry, "id")
		for _, c := range cfg.Connections {
			if canonicalProvider(c.Provider) == canonicalProvider(providerID) {
				return fmt.Errorf("provider %q still has connection %q (remove it first or use --force)", providerID, c.ID)
			}
		}
	}
	sequenceDelete(seq, index)
	warnings, err := doc.save(false, stdout)
	if err != nil {
		return err
	}
	printWarnings(stdout, warnings)
	fmt.Fprintf(stdout, "removed provider entry %s from %s\n", *id, g.configPath)
	return nil
}

func applyProviderOptions(entry *yaml.Node, o providerOptions, changed map[string]bool) error {
	if flagProvided(changed, "alias") {
		setOrDeleteString(entry, "alias", o.alias)
	}
	if flagProvided(changed, "base-url") {
		setOrDeleteString(entry, "base_url", o.baseURL)
	}
	if flagProvided(changed, "format") {
		f := strings.ToLower(strings.TrimSpace(o.format))
		if f == "" {
			mappingDelete(entry, "format")
		} else {
			if err := validateFormat(f); err != nil {
				return err
			}
			mappingSet(entry, "format", scalar(f))
		}
	}
	if flagProvided(changed, "auth") {
		a := strings.ToLower(strings.TrimSpace(o.auth))
		switch a {
		case "":
			mappingDelete(entry, "auth")
		case "bearer", "x-api-key", "query", "none":
			mappingSet(entry, "auth", scalar(a))
		default:
			return fmt.Errorf("auth %q is not one of bearer|x-api-key|query|none", a)
		}
	}
	if flagProvided(changed, "auth-param") {
		setOrDeleteString(entry, "auth_param", o.authParam)
	}
	if flagProvided(changed, "timeout") {
		setOrDeleteString(entry, "timeout", o.timeout)
	}
	if o.clearHeaders {
		mappingDelete(entry, "headers")
	}
	if flagProvided(changed, "header") {
		headers, err := ensureMapping(entry, "headers")
		if err != nil {
			return err
		}
		for _, raw := range o.headers {
			name, value, err := splitHeader(raw)
			if err != nil {
				return err
			}
			mappingSet(headers, name, scalar(value))
		}
	}
	if flagProvided(changed, "model") {
		seq := newSeq()
		for _, id := range o.models {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			item := newMap()
			mappingSet(item, "id", scalar(id))
			seq.Content = append(seq.Content, item)
		}
		mappingSet(entry, "models", seq)
	}
	if flagProvided(changed, "disabled") {
		if o.disabled {
			mappingSet(entry, "disabled", boolNode(true))
		} else {
			mappingDelete(entry, "disabled")
		}
	}
	if flagProvided(changed, "enabled") {
		if o.enabled {
			mappingDelete(entry, "disabled")
		} else {
			mappingSet(entry, "disabled", boolNode(true))
		}
	}
	return nil
}

func printWarnings(w io.Writer, warnings []string) {
	for _, warning := range warnings {
		fmt.Fprintf(w, "warning: %s\n", warning)
	}
}
