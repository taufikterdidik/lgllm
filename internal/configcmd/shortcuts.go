package configcmd

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

func cmdAlias(args []string, g globals, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: lgllm config alias list|set|remove")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list", "ls":
		return aliasList(rest, g, stdout)
	case "set", "add":
		return aliasSet(rest, g, stdout)
	case "remove", "rm", "delete", "unset":
		return aliasRemove(rest, g, stdout)
	default:
		return fmt.Errorf("unknown alias command %q (want list|set|remove)", sub)
	}
}

func aliasList(args []string, g globals, stdout io.Writer) error {
	fs := newFlagSet("config alias list")
	if err := parseFlags(fs, args); err != nil {
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
	if len(cfg.Aliases) == 0 {
		fmt.Fprintln(stdout, "no aliases")
		return nil
	}
	names := make([]string, 0, len(cfg.Aliases))
	for name := range cfg.Aliases {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(stdout, "%-20s -> %s\n", name, cfg.Aliases[name])
	}
	return nil
}

func aliasSet(args []string, g globals, stdout io.Writer) error {
	fs := newFlagSet("config alias set")
	name := fs.String("name", "", "client-facing alias")
	target := fs.String("target", "", "routing target, e.g. provider/model")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	positional := fs.Args()
	if *name == "" && len(positional) > 0 {
		*name = positional[0]
		positional = positional[1:]
	}
	if *target == "" && len(positional) > 0 {
		*target = positional[0]
		positional = positional[1:]
	}
	if len(positional) > 0 {
		return fmt.Errorf("alias set takes exactly two arguments: NAME TARGET")
	}
	*name = strings.TrimSpace(*name)
	*target = strings.TrimSpace(*target)
	if *name == "" || *target == "" {
		return fmt.Errorf("alias set needs NAME and TARGET, e.g. `alias set fast ds/deepseek-chat`")
	}
	if strings.Contains(*name, "/") {
		return fmt.Errorf("alias %q must not contain \"/\" (that is the provider prefix)", *name)
	}

	doc, err := loadDocument(g.configPath)
	if err != nil {
		return err
	}
	aliases, err := ensureMapping(doc.root(), "aliases")
	if err != nil {
		return err
	}
	mappingSet(aliases, *name, scalar(*target))
	warnings, err := doc.save(false, stdout)
	if err != nil {
		return err
	}
	printWarnings(stdout, warnings)
	fmt.Fprintf(stdout, "set alias %s -> %s in %s\n", *name, *target, g.configPath)
	return nil
}

func aliasRemove(args []string, g globals, stdout io.Writer) error {
	fs := newFlagSet("config alias remove")
	name := fs.String("name", "", "alias to remove")
	positional, err := parsePositional(fs, args)
	if err != nil {
		return err
	}
	if *name == "" {
		*name = positional
	}
	if strings.TrimSpace(*name) == "" {
		return fmt.Errorf("alias remove needs a name")
	}
	doc, err := loadDocument(g.configPath)
	if err != nil {
		return err
	}
	aliases := mappingGet(doc.root(), "aliases")
	if aliases == nil || !mappingDelete(aliases, *name) {
		return fmt.Errorf("alias %q does not exist", *name)
	}
	if len(aliases.Content) == 0 {
		mappingDelete(doc.root(), "aliases")
	}
	warnings, err := doc.save(false, stdout)
	if err != nil {
		return err
	}
	printWarnings(stdout, warnings)
	fmt.Fprintf(stdout, "removed alias %s from %s\n", *name, g.configPath)
	return nil
}

func cmdCombo(args []string, g globals, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: lgllm config combo list|add|set|remove")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list", "ls":
		return comboList(rest, g, stdout)
	case "add":
		return comboAdd(rest, g, false, stdout)
	case "set", "update":
		return comboAdd(rest, g, true, stdout)
	case "remove", "rm", "delete":
		return comboRemove(rest, g, stdout)
	default:
		return fmt.Errorf("unknown combo command %q (want list|add|set|remove)", sub)
	}
}

func comboList(args []string, g globals, stdout io.Writer) error {
	fs := newFlagSet("config combo list")
	if err := parseFlags(fs, args); err != nil {
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
	if len(cfg.Combos) == 0 {
		fmt.Fprintln(stdout, "no combos")
		return nil
	}
	for _, c := range cfg.Combos {
		strategy := c.Strategy
		if strategy == "" {
			strategy = "priority"
		}
		fmt.Fprintf(stdout, "%-20s strategy=%-12s members=%s\n", c.Name, strategy, strings.Join(c.Members, ", "))
	}
	return nil
}

func comboAdd(args []string, g globals, update bool, stdout io.Writer) error {
	fs := newFlagSet("config combo add")
	name := fs.String("name", "", "combo name")
	strategy := fs.String("strategy", "", "priority|round-robin")
	tokenSaver := fs.String("token-saver", "", "true|false|inherit")
	var members csvList
	fs.Var(&members, "member", "routing target in priority order (repeatable, comma list accepted)")
	positional, err := parsePositional(fs, args)
	if err != nil {
		return err
	}
	if *name == "" {
		*name = positional
	}
	*name = strings.TrimSpace(*name)
	if *name == "" {
		return fmt.Errorf("combo add/set needs a name")
	}
	if strings.Contains(*name, "/") {
		return fmt.Errorf("combo %q must not contain \"/\" (that is the provider prefix)", *name)
	}

	doc, err := loadDocument(g.configPath)
	if err != nil {
		return err
	}
	seq, err := ensureSequence(doc.root(), "combos")
	if err != nil {
		return err
	}
	entry, index := sequenceFind(seq, "name", *name)
	if entry != nil && !update {
		return fmt.Errorf("combo %q already exists (use `combo set`)", *name)
	}
	if st, err := loadAdminOverlay(g.configPath); err != nil {
		return err
	} else {
		for _, cb := range st.Combos {
			if cb.Name == *name {
				return fmt.Errorf("combo %q is managed in the admin state file; use the admin API", *name)
			}
		}
	}
	if entry == nil && update {
		return fmt.Errorf("combo %q does not exist", *name)
	}
	if entry == nil {
		entry = newMap()
		mappingSet(entry, "name", scalar(*name))
	}
	changed := changedFlags(fs)
	if flagProvided(changed, "member") {
		if len(members) == 0 {
			return fmt.Errorf("--member needs at least one target")
		}
		m := newSeq()
		for _, target := range members {
			m.Content = append(m.Content, scalar(target))
		}
		mappingSet(entry, "members", m)
	} else if entry != nil && mappingGet(entry, "members") == nil {
		return fmt.Errorf("combo %q needs at least one --member", *name)
	}
	if flagProvided(changed, "strategy") {
		s := strings.ToLower(strings.TrimSpace(*strategy))
		switch s {
		case "":
			mappingDelete(entry, "strategy")
		case "priority", "round-robin":
			mappingSet(entry, "strategy", scalar(s))
		default:
			return fmt.Errorf("strategy %q is not one of priority|round-robin", s)
		}
	}
	if flagProvided(changed, "token-saver") {
		switch strings.ToLower(strings.TrimSpace(*tokenSaver)) {
		case "", "inherit":
			mappingDelete(entry, "token_saver")
		case "true", "yes", "on":
			mappingSet(entry, "token_saver", boolNode(true))
		case "false", "no", "off":
			mappingSet(entry, "token_saver", boolNode(false))
		default:
			return fmt.Errorf("--token-saver must be true, false, or inherit")
		}
	}
	if index < 0 {
		seq.Content = append(seq.Content, entry)
	}

	warnings, err := doc.save(false, stdout)
	if err != nil {
		return err
	}
	printWarnings(stdout, warnings)
	fmt.Fprintf(stdout, "saved combo %s in %s\n", *name, g.configPath)
	return nil
}

func comboRemove(args []string, g globals, stdout io.Writer) error {
	fs := newFlagSet("config combo remove")
	name := fs.String("name", "", "combo name")
	positional, err := parsePositional(fs, args)
	if err != nil {
		return err
	}
	if *name == "" {
		*name = positional
	}
	if strings.TrimSpace(*name) == "" {
		return fmt.Errorf("combo remove needs a name")
	}
	doc, err := loadDocument(g.configPath)
	if err != nil {
		return err
	}
	seq := mappingGet(doc.root(), "combos")
	_, index := sequenceFind(seq, "name", *name)
	if index < 0 {
		return fmt.Errorf("combo %q does not exist", *name)
	}
	sequenceDelete(seq, index)
	warnings, err := doc.save(false, stdout)
	if err != nil {
		return err
	}
	printWarnings(stdout, warnings)
	fmt.Fprintf(stdout, "removed combo %s from %s\n", *name, g.configPath)
	return nil
}
