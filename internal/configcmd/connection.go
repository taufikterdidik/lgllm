package configcmd

import (
	"fmt"
	"io"
	"strings"
	"time"

	"lgllm/internal/config"
	"lgllm/internal/registry"
)

func cmdConnection(args []string, g globals, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: lgllm config connection list|add|set|remove|test")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list", "ls":
		return connectionList(rest, g, stdout)
	case "add":
		return connectionAdd(rest, g, stdin, stdout)
	case "set", "update":
		return connectionSet(rest, g, stdout)
	case "remove", "rm", "delete":
		return connectionRemove(rest, g, stdout)
	case "test", "check":
		return connectionTest(rest, g, stdout)
	default:
		return fmt.Errorf("unknown connection command %q (want list|add|set|remove|test)", sub)
	}
}

func connectionList(args []string, g globals, stdout io.Writer) error {
	fs := newFlagSet("config connection list")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("connection list takes no positional arguments")
	}
	doc, err := loadDocument(g.configPath)
	if err != nil {
		return err
	}
	cfg, err := doc.rawConfig()
	if err != nil {
		return err
	}
	if len(cfg.Connections) == 0 {
		fmt.Fprintln(stdout, "no connections yet; add one with `lgllm config connection add`")
		return nil
	}
	fmt.Fprintf(stdout, "%-18s %-16s %-24s %-6s %s\n", "ID", "PROVIDER", "API KEY", "STATE", "BASE URL")
	for _, c := range cfg.Connections {
		state := "on"
		if c.Disabled {
			state = "off"
		}
		base := c.BaseURL
		if base == "" {
			base = "(provider default)"
		}
		fmt.Fprintf(stdout, "%-18s %-16s %-24s %-6s %s\n",
			c.ID, c.Provider, describeSecret(c.APIKey), state, base)
	}
	return nil
}

func connectionAdd(args []string, g globals, stdin io.Reader, stdout io.Writer) error {
	fs := newFlagSet("config connection add")
	var (
		provider = fs.String("provider", "", "provider id or alias")
		id       = fs.String("id", "", "connection id (default PROVIDER-N)")
		apiKey   = fs.String("api-key", "", "credential; use '${VAR}' to keep it in the environment")
		baseURL  = fs.String("base-url", "", "override the provider base URL for this credential")
		disabled = fs.Bool("disabled", false, "add the credential disabled")
	)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("connection add takes no positional arguments")
	}
	*provider = strings.TrimSpace(*provider)
	if *provider == "" && isTerminal(stdin) {
		answer, err := newSession(stdin, stdout).ask("Provider id or alias", "")
		if err != nil {
			return err
		}
		*provider = answer
	}
	if *provider == "" {
		return fmt.Errorf("connection add needs --provider (try `lgllm config provider list`)")
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
	p := resolveProvider(effective, *provider)
	if p == nil {
		return fmt.Errorf("provider %q is not configured; add it first with `provider add`", *provider)
	}
	if *id == "" {
		*id = nextConnectionID(cfg, p.ID)
	}
	if existing, _ := findConnection(cfg, *id); existing != nil {
		return fmt.Errorf("connection %q already exists (use `connection set`)", *id)
	}
	if st, err := loadAdminOverlay(g.configPath); err != nil {
		return err
	} else {
		for _, c := range st.Connections {
			if c.ID == *id {
				return fmt.Errorf("connection %q is managed in the admin state file; use a different id", *id)
			}
		}
	}

	if !flagProvided(changedFlags(fs), "api-key") && p.Auth != "none" {
		if b, ok := registry.CatalogByRef(p.ID); ok && b.KeyEnv != "" {
			*apiKey = "${" + b.KeyEnv + "}"
		}
	}
	*apiKey = strings.TrimSpace(*apiKey)
	if *apiKey == "" && p.Auth != "none" {
		return fmt.Errorf("provider %q uses %q auth; pass --api-key (or '${ENV_VAR}')", p.ID, p.Auth)
	}
	if *apiKey != "" && !strings.HasPrefix(*apiKey, "enc:v1:") && !strings.HasPrefix(*apiKey, "${") {
		// A secret on the command line is visible in shell history. It is still
		// allowed for non-interactive use, but the warning nudges toward env refs.
		fmt.Fprintln(stdout, "note: prefer --api-key '${ENV_VAR}' or pipe through `lgllm -encrypt` to keep secrets out of shell history")
	}

	entry := newMap()
	mappingSet(entry, "id", scalar(*id))
	mappingSet(entry, "provider", scalar(p.ID))
	if *apiKey != "" {
		mappingSet(entry, "api_key", scalar(*apiKey))
	}
	if *baseURL != "" {
		mappingSet(entry, "base_url", scalar(*baseURL))
	}
	if *disabled {
		mappingSet(entry, "disabled", boolNode(true))
	}
	seq, err := ensureSequence(doc.root(), "connections")
	if err != nil {
		return err
	}
	seq.Content = append(seq.Content, entry)

	warnings, err := doc.save(false, stdout)
	if err != nil {
		return err
	}
	printWarnings(stdout, warnings)
	fmt.Fprintf(stdout, "added connection %s for %s to %s\n", *id, p.ID, g.configPath)
	fmt.Fprintf(stdout, "next: lgllm config models sync --provider %s\n", p.ID)
	return nil
}

func nextConnectionID(cfg *config.Config, provider string) string {
	used := map[string]bool{}
	for _, c := range cfg.Connections {
		used[c.ID] = true
	}
	base := strings.ReplaceAll(provider, " ", "-")
	for n := 1; ; n++ {
		candidate := fmt.Sprintf("%s-%d", base, n)
		if !used[candidate] {
			return candidate
		}
	}
}

func connectionSet(args []string, g globals, stdout io.Writer) error {
	fs := newFlagSet("config connection set")
	var (
		id       = fs.String("id", "", "connection id")
		provider = fs.String("provider", "", "provider id or alias")
		apiKey   = fs.String("api-key", "", "credential (empty removes it)")
		baseURL  = fs.String("base-url", "", "override base URL (empty removes it)")
		disabled = fs.Bool("disabled", false, "disable the credential")
		enabled  = fs.Bool("enabled", false, "enable the credential")
	)
	positional, err := parsePositional(fs, args)
	if err != nil {
		return err
	}
	if *id == "" {
		*id = positional
	}
	if strings.TrimSpace(*id) == "" {
		return fmt.Errorf("connection set needs --id")
	}
	if *disabled && *enabled {
		return fmt.Errorf("use only one of --disabled and --enabled")
	}

	doc, err := loadDocument(g.configPath)
	if err != nil {
		return err
	}
	seq := mappingGet(doc.root(), "connections")
	entry, _ := sequenceFind(seq, "id", strings.TrimSpace(*id))
	if entry == nil {
		return fmt.Errorf("connection %q does not exist", *id)
	}
	changed := changedFlags(fs)
	if flagProvided(changed, "provider") {
		if strings.TrimSpace(*provider) == "" {
			return fmt.Errorf("--provider must not be empty")
		}
		cfg, err := doc.rawConfig()
		if err != nil {
			return err
		}
		p := resolveProvider(registry.Materialize(cfg.Providers, cfg.Connections), *provider)
		if p == nil {
			return fmt.Errorf("provider %q is not configured", *provider)
		}
		mappingSet(entry, "provider", scalar(p.ID))
	}
	if flagProvided(changed, "api-key") {
		setOrDeleteString(entry, "api_key", *apiKey)
	}
	if flagProvided(changed, "base-url") {
		setOrDeleteString(entry, "base_url", *baseURL)
	}
	if flagProvided(changed, "disabled") {
		if *disabled {
			mappingSet(entry, "disabled", boolNode(true))
		} else {
			mappingDelete(entry, "disabled")
		}
	}
	if flagProvided(changed, "enabled") {
		if *enabled {
			mappingDelete(entry, "disabled")
		} else {
			mappingSet(entry, "disabled", boolNode(true))
		}
	}

	warnings, err := doc.save(false, stdout)
	if err != nil {
		return err
	}
	printWarnings(stdout, warnings)
	fmt.Fprintf(stdout, "updated connection %s in %s\n", *id, g.configPath)
	return nil
}

func connectionRemove(args []string, g globals, stdout io.Writer) error {
	fs := newFlagSet("config connection remove")
	id := fs.String("id", "", "connection id")
	positional, err := parsePositional(fs, args)
	if err != nil {
		return err
	}
	if *id == "" {
		*id = positional
	}
	if strings.TrimSpace(*id) == "" {
		return fmt.Errorf("connection remove needs an id")
	}
	doc, err := loadDocument(g.configPath)
	if err != nil {
		return err
	}
	seq := mappingGet(doc.root(), "connections")
	_, index := sequenceFind(seq, "id", strings.TrimSpace(*id))
	if index < 0 {
		return fmt.Errorf("connection %q does not exist", *id)
	}
	sequenceDelete(seq, index)
	warnings, err := doc.save(false, stdout)
	if err != nil {
		return err
	}
	printWarnings(stdout, warnings)
	fmt.Fprintf(stdout, "removed connection %s from %s\n", *id, g.configPath)
	return nil
}

func connectionTest(args []string, g globals, stdout io.Writer) error {
	fs := newFlagSet("config connection test")
	var (
		provider = fs.String("provider", "", "provider id or alias")
		id       = fs.String("id", "", "connection id (default: first enabled for the provider)")
		url      = fs.String("url", "", "override the models endpoint URL")
		timeout  = fs.Duration("timeout", 20*time.Second, "request timeout")
	)
	positional, err := parsePositional(fs, args)
	if err != nil {
		return err
	}
	if *provider == "" {
		*provider = positional
	}
	if strings.TrimSpace(*provider) == "" && *id == "" {
		return fmt.Errorf("connection test needs --provider or --id")
	}
	models, detail, err := discoverFromConfig(g.configPath, g.masterKey, *provider, *id, *url, *timeout)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "OK: %s returned %d model(s)\n", detail, len(models))
	for i, m := range models {
		if i == 8 {
			fmt.Fprintf(stdout, "  ... and %d more\n", len(models)-i)
			break
		}
		fmt.Fprintf(stdout, "  %s\n", m)
	}
	return nil
}
