package server

import (
	"fmt"
	"strings"

	"lgllm/internal/registry"
)

// modelContextInfo is the effective context window for a routable name.
//
// A combo is only as large as its smallest member. If any member's context is
// unknown we report Known=false instead of guessing a number: a client that
// trusts an optimistic limit would send a request that one fallback member
// cannot accept.
type modelContextInfo struct {
	Context int
	Known   bool
	Combo   bool
	Members []modelContextMember
}

type modelContextMember struct {
	ID      string `json:"id"`
	Context int    `json:"context_length,omitempty"`
	Known   bool   `json:"context_known"`
}

func (s *Server) modelContextInfoFor(reg *registry.Registry, name string) (modelContextInfo, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return modelContextInfo{}, fmt.Errorf("model is required")
	}

	for _, combo := range reg.Combos() {
		if combo.Name == name {
			return contextForCombo(combo), nil
		}
	}

	target, err := reg.Lookup(name)
	if err != nil {
		return modelContextInfo{}, err
	}
	if target.Combo != "" {
		for _, combo := range reg.Combos() {
			if combo.Name == target.Combo {
				return contextForCombo(combo), nil
			}
		}
	}
	context, known := contextForTarget(target)
	return modelContextInfo{Context: context, Known: known}, nil
}

func contextForCombo(combo registry.ComboInfo) modelContextInfo {
	out := modelContextInfo{Combo: true, Members: make([]modelContextMember, 0, len(combo.Members))}
	allKnown := len(combo.Members) > 0
	minimum := 0
	for _, target := range combo.Members {
		context, known := contextForTarget(target)
		member := modelContextMember{Context: context, Known: known}
		if target.Provider != nil {
			member.ID = modelRef(target.Provider, registry.Model{ID: target.Model})
		}
		out.Members = append(out.Members, member)
		if !known {
			allKnown = false
			continue
		}
		if minimum == 0 || context < minimum {
			minimum = context
		}
	}
	if !allKnown {
		return out
	}
	out.Context = minimum
	out.Known = true
	return out
}

func contextForTarget(target registry.Target) (int, bool) {
	if target.Provider == nil || target.Model == "" {
		return 0, false
	}
	model, ok := advertisedModel(target.Provider, target.Model)
	if !ok || model.Context <= 0 {
		return 0, false
	}
	return model.Context, true
}
