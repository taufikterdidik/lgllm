package configcmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"lgllm/internal/config"
	"lgllm/internal/registry"
	"lgllm/internal/secret"
)

// globals are flags accepted anywhere on the command line and removed before a
// subcommand parses its own flags, so both `config --config x provider list` and
// `config provider list --config x` work.
type globals struct {
	configPath string
	masterKey  string
}

func defaultGlobals() globals {
	return globals{
		configPath: "config.yaml",
		masterKey:  os.Getenv("LGLLM_MASTER_KEY"),
	}
}

func parseGlobals(args []string) ([]string, globals, error) {
	g := defaultGlobals()
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func(name string) (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", name)
			}
			i++
			return args[i], nil
		}
		switch {
		case a == "-config" || a == "--config":
			v, err := next(a)
			if err != nil {
				return nil, g, err
			}
			g.configPath = v
		case strings.HasPrefix(a, "-config="):
			g.configPath = strings.TrimPrefix(a, "-config=")
		case strings.HasPrefix(a, "--config="):
			g.configPath = strings.TrimPrefix(a, "--config=")
		case a == "-master-key" || a == "--master-key":
			v, err := next(a)
			if err != nil {
				return nil, g, err
			}
			g.masterKey = v
		case strings.HasPrefix(a, "-master-key="):
			g.masterKey = strings.TrimPrefix(a, "-master-key=")
		case strings.HasPrefix(a, "--master-key="):
			g.masterKey = strings.TrimPrefix(a, "--master-key=")
		default:
			rest = append(rest, a)
		}
	}
	if strings.TrimSpace(g.configPath) == "" {
		return nil, g, errors.New("--config must not be empty")
	}
	return rest, g, nil
}

// Run executes `lgllm config ...`. It never starts the server.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	rest, g, err := parseGlobals(args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		if isTerminal(stdin) {
			return runWizard(g, stdin, stdout)
		}
		usage(stdout)
		return nil
	}

	cmd, sub := rest[0], rest[1:]
	switch cmd {
	case "help", "-h", "--help":
		usage(stdout)
		return nil
	case "init":
		return cmdInit(sub, g, stdout)
	case "show":
		return cmdShow(sub, g, stdout)
	case "validate", "check":
		return cmdValidate(sub, g, stdout)
	case "provider", "providers":
		return cmdProvider(sub, g, stdout)
	case "connection", "connections":
		return cmdConnection(sub, g, stdin, stdout)
	case "models", "model":
		return cmdModels(sub, g, stdout)
	case "alias", "aliases":
		return cmdAlias(sub, g, stdout)
	case "combo", "combos":
		return cmdCombo(sub, g, stdout)
	case "wizard", "interactive":
		return runWizard(g, stdin, stdout)
	default:
		return fmt.Errorf("unknown config command %q (try `lgllm config help`)", cmd)
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `Usage: lgllm config [global flags] <command> [flags]

Edit config.yaml without a running server. Every change is validated with the
same parser the server uses and the file is replaced atomically, so comments and
${ENV} placeholders survive. With no command on a terminal, an interactive
wizard starts.

Global flags (accepted before or after the command):
  --config PATH       config file (default config.yaml)
  --master-key KEY    decrypt enc:v1: secrets (default $LGLLM_MASTER_KEY)

Commands:
  init                create a starter config (--force overwrites)
  show                print the effective providers, connections and shortcuts
  validate            validate the file and provider cross-references
  provider list       list effective providers
  provider add        add a catalog or custom provider
  provider set        update a provider entry
  provider remove     remove a provider override from the config
  connection list     list credentials
  connection add      add a credential for a provider
  connection set      update a credential
  connection remove   delete a credential
  connection test     call the provider's /models endpoint with one credential
  models list         list configured models (--upstream queries the provider)
  models sync         refresh models from the provider's /models endpoint
  alias list|set|remove
  combo list|add|remove
  wizard              interactive menu

Examples:
  lgllm config init
  lgllm config provider add openai
  lgllm config connection add --provider openai --id work --api-key '${OPENAI_API_KEY}'
  lgllm config models sync --provider openai
  lgllm config connection test --provider openai
  lgllm config alias set fast ds/deepseek-chat
`)
}

// --- init -------------------------------------------------------------------

const starterConfig = `# lgllm configuration.
#
# Add providers and credentials without editing this file by hand:
#   lgllm config wizard
# or non-interactively:
#   lgllm config connection add --provider openai --id work --api-key '${OPENAI_API_KEY}'
#   lgllm config models sync --provider openai
#
# The file reloads automatically when it changes (SIGHUP also works).
server:
  listen: %s

auth:
  # off | localhost | required
  mode: localhost
  keys: []

log:
  level: info
  format: text

metrics:
  enabled: true
  usage_log_size: 500
  performance_window: 10

admin:
  enabled: false

# providers overrides or extends the built-in catalog. A catalog provider only
# needs a connection below; declare it here to override base_url or models.
providers: []

# connections are credentials. A provider becomes usable once one is enabled.
connections: []
`

func cmdInit(args []string, g globals, stdout io.Writer) error {
	fs := newFlagSet("config init")
	force := fs.Bool("force", false, "overwrite an existing file")
	listen := fs.String("listen", "127.0.0.1:20128", "value for server.listen")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("config init takes no positional arguments")
	}
	if info, err := os.Stat(g.configPath); err == nil && !info.IsDir() && !*force {
		return fmt.Errorf("%s already exists (use --force to overwrite)", g.configPath)
	}
	data := []byte(fmt.Sprintf(starterConfig, *listen))
	if _, err := validateConfig(data); err != nil {
		return err
	}
	abs, _ := filepath.Abs(g.configPath)
	if err := writeAtomic(g.configPath, data, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "wrote %s\n", abs)
	fmt.Fprintln(stdout, "next: lgllm config wizard   (or provider/connection add)")
	return nil
}

// --- validate ---------------------------------------------------------------

func cmdValidate(args []string, g globals, stdout io.Writer) error {
	fs := newFlagSet("config validate")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("config validate takes no positional arguments")
	}
	data, err := os.ReadFile(g.configPath)
	if err != nil {
		return fmt.Errorf("read config %s: %w", g.configPath, err)
	}
	warnings, err := validateConfig(data)
	if err != nil {
		return err
	}
	abs, _ := filepath.Abs(g.configPath)
	if len(warnings) > 0 {
		fmt.Fprintf(stdout, "config OK (partially validated): %s\n", abs)
		for _, w := range warnings {
			fmt.Fprintf(stdout, "warning: %s\n", w)
		}
		return nil
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return err
	}
	reg, err := registry.New(cfg)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "config OK: %s (%d provider(s) usable)\n", abs, reg.Len())
	return nil
}

// --- show -------------------------------------------------------------------

func cmdShow(args []string, g globals, stdout io.Writer) error {
	fs := newFlagSet("config show")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("config show takes no positional arguments")
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
	abs, _ := filepath.Abs(g.configPath)

	fmt.Fprintf(stdout, "config: %s\n", abs)
	fmt.Fprintf(stdout, "note: admin.yaml is not shown; `models` and `connection test` use the full runtime view\n")
	fmt.Fprintf(stdout, "server.listen: %s\n", cfg.Server.Listen)
	fmt.Fprintf(stdout, "auth.mode: %s\n", cfg.Auth.Mode)
	if cfg.DefaultProvider != "" {
		fmt.Fprintf(stdout, "default_provider: %s\n", cfg.DefaultProvider)
	}

	fmt.Fprintf(stdout, "\nproviders (%d effective):\n", len(effective))
	if len(effective) == 0 {
		fmt.Fprintln(stdout, "  (none; add a connection to activate a catalog provider)")
	}
	for _, p := range effective {
		alias := p.Alias
		if alias == "" {
			alias = "-"
		}
		declared := configProviderEntry(doc.root(), p.ID) != nil
		source := "catalog"
		if declared {
			source = "config"
		}
		fmt.Fprintf(stdout, "  %-14s alias=%-4s format=%-17s auth=%-9s base=%s models=%d (%s)\n",
			p.ID, alias, p.Format, p.Auth, p.BaseURL, len(p.Models), source)
	}

	fmt.Fprintf(stdout, "\nconnections (%d):\n", len(cfg.Connections))
	if len(cfg.Connections) == 0 {
		fmt.Fprintln(stdout, "  (none)")
	}
	for _, c := range cfg.Connections {
		state := "enabled"
		if c.Disabled {
			state = "disabled"
		}
		base := c.BaseURL
		if base == "" {
			base = "(provider default)"
		}
		fmt.Fprintf(stdout, "  %-16s provider=%-12s key=%-22s base=%s (%s)\n",
			c.ID, c.Provider, describeSecret(c.APIKey), base, state)
	}

	if len(cfg.Aliases) > 0 {
		fmt.Fprintf(stdout, "\naliases (%d):\n", len(cfg.Aliases))
		names := make([]string, 0, len(cfg.Aliases))
		for name := range cfg.Aliases {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(stdout, "  %-16s -> %s\n", name, cfg.Aliases[name])
		}
	}

	if len(cfg.Combos) > 0 {
		fmt.Fprintf(stdout, "\ncombos (%d):\n", len(cfg.Combos))
		for _, c := range cfg.Combos {
			strategy := c.Strategy
			if strategy == "" {
				strategy = "priority"
			}
			fmt.Fprintf(stdout, "  %-16s strategy=%-12s members=%s\n",
				c.Name, strategy, strings.Join(c.Members, ", "))
		}
	}
	return nil
}

// describeSecret never prints a credential. Placeholders and ciphertext are
// already safe, so they are shown as-is to make it obvious what is configured.
func describeSecret(v string) string {
	switch {
	case v == "":
		return "(none)"
	case strings.HasPrefix(v, "${") || secret.IsEncrypted(v):
		return v
	default:
		return fmt.Sprintf("(set, %d chars)", len(v))
	}
}

// --- shared lookups ---------------------------------------------------------

// canonicalProvider resolves a catalog id or alias to its catalog id.
func canonicalProvider(ref string) string {
	ref = strings.TrimSpace(ref)
	if b, ok := registry.CatalogByRef(ref); ok {
		return b.ID
	}
	return ref
}

// catalogProvider turns a built-in entry into the provider shape the rest of
// the command uses. It is the fallback for "just add a connection": Materialize
// only includes a catalog provider once a credential references it, but that is
// exactly the connection being created.
func catalogProvider(ref string) *config.Provider {
	b, ok := registry.CatalogByRef(canonicalProvider(ref))
	if !ok {
		return nil
	}
	p := &config.Provider{
		ID: b.ID, Alias: b.Alias, BaseURL: b.BaseURL,
		Format: b.Format, Auth: b.Auth, AuthParam: b.AuthParam,
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

// resolveProvider looks in the effective list first, then in the catalog.
func resolveProvider(list []config.Provider, ref string) *config.Provider {
	if p := findEffectiveProvider(list, ref); p != nil {
		return p
	}
	return catalogProvider(ref)
}

// findEffectiveProvider matches an id or alias in a materialized provider list.
func findEffectiveProvider(list []config.Provider, ref string) *config.Provider {
	ref = strings.TrimSpace(ref)
	for i := range list {
		if list[i].ID == ref || (list[i].Alias != "" && list[i].Alias == ref) {
			return &list[i]
		}
	}
	return nil
}

// configProviderEntry finds a `providers:` entry by id or alias, resolving
// catalog aliases so "oa" matches an entry written as "id: openai".
func configProviderEntry(root *yaml.Node, ref string) *yaml.Node {
	seq := mappingGet(root, "providers")
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return nil
	}
	want := canonicalProvider(ref)
	for _, item := range seq.Content {
		if item.Kind != yaml.MappingNode {
			continue
		}
		id := mappingString(item, "id")
		alias := mappingString(item, "alias")
		if id == ref || id == want || alias == ref {
			return item
		}
		if canonicalProvider(id) == want {
			return item
		}
	}
	return nil
}

// ensureProviderEntry returns the config entry for id, creating `providers:` and
// the mapping when needed.
func ensureProviderEntry(root *yaml.Node, id string) (*yaml.Node, error) {
	if entry := configProviderEntry(root, id); entry != nil {
		return entry, nil
	}
	seq, err := ensureSequence(root, "providers")
	if err != nil {
		return nil, err
	}
	entry := newMap()
	mappingSet(entry, "id", scalar(id))
	seq.Content = append(seq.Content, entry)
	return entry, nil
}

// loadAdminOverlay reads the API-managed resources that live next to the main
// file. An edit that would duplicate an id from that overlay is refused before
// it is written, because the server rejects such a config on load.
func loadAdminOverlay(path string) (*config.AdminState, error) {
	doc, err := loadDocument(path)
	if err != nil {
		return nil, err
	}
	cfg, err := doc.rawConfig()
	if err != nil {
		return nil, err
	}
	cfg.Dir = filepath.Dir(mustAbs(path))
	statePath := cfg.Admin.StateFile
	if statePath == "" {
		statePath = filepath.Join(cfg.Dir, "admin.yaml")
	} else if !filepath.IsAbs(statePath) {
		statePath = filepath.Join(cfg.Dir, statePath)
	}
	return config.LoadAdminState(statePath)
}

func mustAbs(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
}

func findConnection(cfg *config.Config, id string) (*config.Connection, int) {
	for i := range cfg.Connections {
		if cfg.Connections[i].ID == id {
			return &cfg.Connections[i], i
		}
	}
	return nil, -1
}

func providerConnection(cfg *config.Config, providerRef string) *config.Connection {
	providerRef = canonicalProvider(providerRef)
	for i := range cfg.Connections {
		c := &cfg.Connections[i]
		if c.Disabled {
			continue
		}
		if canonicalProvider(c.Provider) == providerRef {
			return c
		}
	}
	return nil
}

func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// flagProvided reports whether a string flag was explicitly set.
func flagProvided(changed map[string]bool, names ...string) bool {
	for _, name := range names {
		if changed[name] {
			return true
		}
	}
	return false
}

var validFormats = []string{
	registry.FormatChat,
	registry.FormatAnthropic,
	registry.FormatGemini,
	registry.FormatResponses,
	registry.FormatOllama,
}

func validateFormat(format string) error {
	for _, f := range validFormats {
		if format == f {
			return nil
		}
	}
	return fmt.Errorf("format %q is not one of %s", format, strings.Join(validFormats, "|"))
}
