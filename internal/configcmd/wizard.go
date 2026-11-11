package configcmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"lgllm/internal/registry"
)

const wizardMenu = `
What next?
  1) add a provider from the built-in catalog
  2) add a custom provider
  3) add a connection (credential)
  4) sync models from the provider
  5) show the effective config
  6) set an alias
  7) remove a provider or connection
  8) validate the config
  9) quit
`

// session is a small line-oriented prompt helper. It works on any io.Reader, so
// the wizard can be scripted in tests.
type session struct {
	in  *bufio.Reader
	out io.Writer
}

func newSession(in io.Reader, out io.Writer) *session {
	return &session{in: bufio.NewReader(in), out: out}
}

func (s *session) readLine() (string, error) {
	line, err := s.in.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && line != "" {
			return line, nil
		}
		return "", err
	}
	return line, nil
}

func (s *session) ask(label, def string) (string, error) {
	if def != "" {
		fmt.Fprintf(s.out, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(s.out, "%s: ", label)
	}
	line, err := s.readLine()
	if err != nil {
		fmt.Fprintln(s.out)
		return "", err
	}
	value := strings.TrimSpace(line)
	if value == "" {
		return def, nil
	}
	return value, nil
}

func (s *session) confirm(label string, def bool) (bool, error) {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	value, err := s.ask(fmt.Sprintf("%s (%s)", label, hint), "")
	if err != nil {
		return false, err
	}
	switch strings.ToLower(value) {
	case "":
		return def, nil
	case "y", "yes", "ya":
		return true, nil
	case "n", "no", "tidak":
		return false, nil
	default:
		return false, fmt.Errorf("answer y or n")
	}
}

func runWizard(g globals, in io.Reader, out io.Writer) error {
	s := newSession(in, out)
	if _, err := os.Stat(g.configPath); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(out, "%s does not exist.\n", g.configPath)
		create, err := s.confirm("Create a starter config there", true)
		if err != nil {
			return nil
		}
		if !create {
			return nil
		}
		if err := cmdInit(nil, g, out); err != nil {
			return err
		}
	}

	fmt.Fprintf(out, "lgllm config wizard — %s\n", g.configPath)
	for {
		fmt.Fprint(out, wizardMenu)
		choice, err := s.ask("Choice", "9")
		if err != nil {
			if errors.Is(err, io.EOF) {
				fmt.Fprintln(out)
				return nil
			}
			return err
		}
		switch choice {
		case "1":
			wizardAddCatalogProvider(s, g, out)
		case "2":
			wizardAddCustomProvider(s, g, out)
		case "3":
			wizardAddConnection(s, g, out)
		case "4":
			wizardSyncModels(s, g, out)
		case "5":
			if err := cmdShow(nil, g, out); err != nil {
				fmt.Fprintf(out, "error: %v\n", err)
			}
		case "6":
			wizardAlias(s, g, out)
		case "7":
			wizardRemove(s, g, out)
		case "8":
			if err := cmdValidate(nil, g, out); err != nil {
				fmt.Fprintf(out, "error: %v\n", err)
			}
		case "9", "q", "quit", "exit":
			fmt.Fprintln(out, "bye")
			return nil
		default:
			fmt.Fprintln(out, "pick 1-9")
		}
	}
}

func wizardAddCatalogProvider(s *session, g globals, out io.Writer) {
	fmt.Fprintln(out, "Catalog:")
	for _, b := range registry.Catalog() {
		alias := b.Alias
		if alias == "" {
			alias = "-"
		}
		hint := b.KeyEnv
		if hint == "" {
			hint = "no key"
		}
		fmt.Fprintf(out, "  %-12s alias=%-4s %-18s key=%s\n", b.ID, alias, b.Name, hint)
	}
	id, err := s.ask("Provider id or alias", "openai")
	if err != nil || id == "" {
		return
	}
	if err := providerAdd([]string{"--id", id}, g, out); err != nil {
		fmt.Fprintf(out, "error: %v\n", err)
		return
	}
	yes, err := s.confirm("Add a connection for it now", true)
	if err == nil && yes {
		wizardAddConnectionFor(s, g, out, id)
	}
}

func wizardAddCustomProvider(s *session, g globals, out io.Writer) {
	id, err := s.ask("Provider id (e.g. mylab)", "")
	if err != nil || id == "" {
		return
	}
	base, err := s.ask("Base URL including the version segment", "")
	if err != nil || base == "" {
		fmt.Fprintln(out, "a custom provider needs a base URL")
		return
	}
	format, err := s.ask("Format (openai|anthropic|gemini|openai-responses|ollama)", "openai")
	if err != nil {
		return
	}
	auth, err := s.ask("Auth (bearer|x-api-key|query|none)", "bearer")
	if err != nil {
		return
	}
	models, err := s.ask("Model ids, comma separated (optional)", "")
	if err != nil {
		return
	}
	args := []string{"--id", id, "--base-url", base, "--format", format, "--auth", auth}
	for _, model := range strings.Split(models, ",") {
		if model = strings.TrimSpace(model); model != "" {
			args = append(args, "--model", model)
		}
	}
	if err := providerAdd(args, g, out); err != nil {
		fmt.Fprintf(out, "error: %v\n", err)
		return
	}
	yes, err := s.confirm("Add a connection for it now", true)
	if err == nil && yes {
		wizardAddConnectionFor(s, g, out, id)
	}
}

func wizardAddConnection(s *session, g globals, out io.Writer) {
	provider, err := s.ask("Provider id or alias", "openai")
	if err != nil || provider == "" {
		return
	}
	wizardAddConnectionFor(s, g, out, provider)
}

func wizardAddConnectionFor(s *session, g globals, out io.Writer, provider string) {
	keyHint := ""
	if b, ok := registry.CatalogByRef(provider); ok && b.KeyEnv != "" {
		keyHint = "${" + b.KeyEnv + "}"
	}
	key, err := s.ask("API key, or ${ENV_VAR} reference", keyHint)
	if err != nil {
		return
	}
	id, err := s.ask("Connection id (empty for automatic)", "")
	if err != nil {
		return
	}
	base, err := s.ask("Base URL override (empty for the provider default)", "")
	if err != nil {
		return
	}
	args := []string{"--provider", provider}
	if key != "" {
		args = append(args, "--api-key", key)
	}
	if id != "" {
		args = append(args, "--id", id)
	}
	if base != "" {
		args = append(args, "--base-url", base)
	}
	if err := connectionAdd(args, g, s.in, out); err != nil {
		fmt.Fprintf(out, "error: %v\n", err)
	}
}

func wizardSyncModels(s *session, g globals, out io.Writer) {
	provider, err := s.ask("Provider id or alias", "")
	if err != nil || provider == "" {
		return
	}
	conn, err := s.ask("Connection id (empty for the first enabled)", "")
	if err != nil {
		return
	}
	replace, err := s.confirm("Drop models the provider no longer lists", false)
	if err != nil {
		return
	}
	args := []string{"--provider", provider}
	if conn != "" {
		args = append(args, "--connection", conn)
	}
	if replace {
		args = append(args, "--replace")
	}
	if err := modelsSync(args, g, out); err != nil {
		fmt.Fprintf(out, "error: %v\n", err)
	}
}

func wizardAlias(s *session, g globals, out io.Writer) {
	name, err := s.ask("Alias name", "")
	if err != nil || name == "" {
		return
	}
	target, err := s.ask("Target (provider/model)", "")
	if err != nil || target == "" {
		return
	}
	if err := aliasSet([]string{name, target}, g, out); err != nil {
		fmt.Fprintf(out, "error: %v\n", err)
	}
}

func wizardRemove(s *session, g globals, out io.Writer) {
	kind, err := s.ask("Remove provider or connection", "connection")
	if err != nil {
		return
	}
	id, err := s.ask("ID to remove", "")
	if err != nil || id == "" {
		return
	}
	switch strings.ToLower(kind) {
	case "p", "provider":
		if err := providerRemove([]string{"--id", id}, g, out); err != nil {
			fmt.Fprintf(out, "error: %v\n", err)
		}
	case "c", "connection":
		if err := connectionRemove([]string{"--id", id}, g, out); err != nil {
			fmt.Fprintf(out, "error: %v\n", err)
		}
	default:
		fmt.Fprintln(out, "say provider or connection")
	}
}
