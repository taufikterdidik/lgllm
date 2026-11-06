package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"lgllm/internal/registry"
	"lgllm/internal/version"
)

// This file implements the compatibility endpoints.
//
// Several clients probe a server for its model list before using it, each in
// its own dialect: Ollama asks /api/tags and /api/show, llama.cpp asks /props.
// lgllm is neither, but answering in their shape lets such a client discover
// the models this proxy routes. None of these handlers contacts a provider;
// generation still goes through /v1/chat/completions (the /api/chat translator
// belongs to phase 4).

// --- Ollama: GET /api/tags ---------------------------------------------

// ollamaDetails mirrors api.ModelDetails from ollama/api/types.go.
type ollamaDetails struct {
	ParentModel       string   `json:"parent_model"`
	Format            string   `json:"format"`
	Family            string   `json:"family"`
	Families          []string `json:"families"`
	ParameterSize     string   `json:"parameter_size"`
	QuantizationLevel string   `json:"quantization_level"`
}

// ollamaModel mirrors api.ListModelResponse, one entry of GET /api/tags.
type ollamaModel struct {
	Name         string        `json:"name"`
	Model        string        `json:"model"`
	ModifiedAt   time.Time     `json:"modified_at"`
	Size         int64         `json:"size"`
	Digest       string        `json:"digest"`
	Details      ollamaDetails `json:"details"`
	Capabilities []string      `json:"capabilities"`
}

// handleAPITags serves GET /api/tags.
//
// Names are provider-qualified ("ak/deepseek-v4.1-flash") because that is
// exactly what /v1/chat/completions accepts, so a client can take a name from
// this list and send it back unchanged. Detail fields that describe a local
// weight file (parameter_size, quantization_level) stay empty: guessing them
// from a model name would be worse than admitting a proxy does not know.
//
// An empty registry yields {"models":[]}, which is what a real Ollama server
// with no models returns: a client shows an empty list rather than an error.
func (s *Server) handleAPITags(w http.ResponseWriter, r *http.Request) {
	reg := s.Registry()
	modified := s.modelModifiedAt()

	models := make([]ollamaModel, 0, 8)
	for _, p := range reg.Providers() {
		for _, m := range p.Models {
			ref := modelRef(p, m)
			models = append(models, ollamaModel{
				Name:         ref,
				Model:        ref,
				ModifiedAt:   modified,
				Size:         0, // a remote model has no local file to measure
				Digest:       modelDigest(p, m),
				Details:      ollamaDetailsFor(p),
				Capabilities: capabilitiesFor(m),
			})
		}
	}

	// Aliases and combos are routable names, so they belong in a list a client
	// picks from. They have no upstream model of their own, so the target's
	// capabilities are reported and size stays zero.
	for _, a := range reg.Aliases() {
		models = append(models, ollamaModel{
			Name:         a.Name,
			Model:        a.Name,
			ModifiedAt:   modified,
			Digest:       digestFor(a.Name),
			Details:      ollamaDetailsFor(a.Target.Provider),
			Capabilities: capabilitiesFor(advertisedOrEmpty(a.Target)),
		})
	}
	for _, c := range reg.Combos() {
		models = append(models, ollamaModel{
			Name:         c.Name,
			Model:        c.Name,
			ModifiedAt:   modified,
			Digest:       digestFor(c.Name),
			Details:      ollamaDetailsFor(c.Members[0].Provider),
			Capabilities: capabilitiesFor(advertisedOrEmpty(c.Members[0])),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

// digestFor fingerprints a routable name that has no single upstream model.
func digestFor(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:])
}

// advertisedOrEmpty returns the advertised model behind a resolved target, so a
// client sees the capabilities declared for it. A target naming a model the
// provider does not advertise yields an empty model: capabilities are then just
// "completion", which is true of anything this proxy routes.
func advertisedOrEmpty(t registry.Target) registry.Model {
	if t.Provider == nil {
		return registry.Model{}
	}
	if m, ok := advertisedModel(t.Provider, t.Model); ok {
		return m
	}
	return registry.Model{}
}

// modelDigest is a stable fingerprint of the routing target.
//
// It hashes "provider/model", not weights: those live upstream and are not
// readable here. Clients key caches and de-duplication on this field, so it must
// be non-empty and stable across restarts, and must change when the routing
// target changes. Ollama's own digests are bare lowercase hex, so this matches
// the expected shape without pretending to be a content hash.
func modelDigest(p *registry.Provider, m registry.Model) string {
	sum := sha256.Sum256([]byte(p.ID + "/" + m.ID))
	return hex.EncodeToString(sum[:])
}

// --- Ollama: GET,POST /api/show ----------------------------------------

// ollamaShow mirrors api.ShowResponse.
//
// license, modelfile, parameters, template and system describe a local model
// file. A proxy has no such file, so they are omitted rather than filled with
// invented text; the fields that can be answered truthfully are all present.
type ollamaShow struct {
	Details      ollamaDetails  `json:"details"`
	ModelInfo    map[string]any `json:"model_info"`
	Capabilities []string       `json:"capabilities"`
	ModifiedAt   time.Time      `json:"modified_at"`
}

// handleAPIShow serves GET and POST /api/show.
//
// Ollama's own client posts {"model": "..."} ("name" is the deprecated
// spelling). A GET with ?model= is accepted as well, which makes the endpoint
// easy to probe by hand.
func (s *Server) handleAPIShow(w http.ResponseWriter, r *http.Request) {
	requested, err := showTarget(r)
	if err != nil {
		var maxErr *http.MaxBytesError
		status := http.StatusBadRequest
		if errors.As(err, &maxErr) {
			status = http.StatusRequestEntityTooLarge
		}
		writeOllamaError(w, status, err.Error())
		return
	}

	reg := s.Registry()
	p, m, ok := findModel(reg, requested)
	if !ok {
		// An empty registry lands here too: with nothing configured there is no
		// model to describe, which is what an Ollama server without models says.
		writeOllamaError(w, http.StatusNotFound, fmt.Sprintf("model %q not found", requested))
		return
	}

	info := map[string]any{
		"lgllm.provider": p.ID,
		"lgllm.format":   p.Format,
		"lgllm.model":    m.ID,
	}
	if m.Context > 0 {
		info["lgllm.context_length"] = m.Context
	}

	writeJSON(w, http.StatusOK, ollamaShow{
		Details:      ollamaDetailsFor(p),
		ModelInfo:    info,
		Capabilities: capabilitiesFor(m),
		ModifiedAt:   s.modelModifiedAt(),
	})
}

// showTarget extracts the model name from an /api/show request.
//
// The query string is read first so a POST with an empty body still works; a
// non-empty field in the body wins, because that is what Ollama's client sends.
func showTarget(r *http.Request) (string, error) {
	requested := strings.TrimSpace(r.URL.Query().Get("model"))
	if requested == "" {
		requested = strings.TrimSpace(r.URL.Query().Get("name"))
	}

	if r.Method == http.MethodPost && r.Body != nil {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return "", err
		}
		if len(bytes.TrimSpace(body)) > 0 {
			// Only the fields that select a model are decoded; the rest of
			// Ollama's request (verbose, options) has no meaning for a proxy.
			var req struct {
				Model string `json:"model"`
				Name  string `json:"name"`
			}
			if err := json.Unmarshal(body, &req); err != nil {
				return "", fmt.Errorf("invalid request body: %w", err)
			}
			if v := strings.TrimSpace(req.Model); v != "" {
				requested = v
			} else if v := strings.TrimSpace(req.Name); v != "" {
				requested = v
			}
		}
	}

	if requested == "" {
		return "", errors.New("model is required")
	}
	return requested, nil
}

// --- llama.cpp: GET /v1/props ------------------------------------------

// propsResponse mirrors the part of llama.cpp's GET /props that describes a
// model rather than a local GGUF file or live slot state.
type propsResponse struct {
	DefaultGenerationSettings propsGenerationSettings `json:"default_generation_settings"`
	TotalSlots                int                     `json:"total_slots"`
	ModelAlias                string                  `json:"model_alias"`
	ModelPath                 string                  `json:"model_path"`
	Modalities                propsModalities         `json:"modalities"`
	ChatTemplate              string                  `json:"chat_template"`
	ChatTemplateCaps          map[string]any          `json:"chat_template_caps"`
	BuildInfo                 string                  `json:"build_info"`
	IsSleeping                bool                    `json:"is_sleeping"`
}

type propsGenerationSettings struct {
	Params map[string]any `json:"params"`
	NCtx   int            `json:"n_ctx"`
}

type propsModalities struct {
	Vision bool `json:"vision"`
	Video  bool `json:"video"`
	Audio  bool `json:"audio"`
}

// handleProps serves GET /v1/props (and /props).
//
// llama.cpp loads one model, so /props describes "the" model. A proxy routes
// many: ?model= chooses one, otherwise the default provider's first advertised
// model is described. Only defaults are reported: params.n_predict is -1
// because this server imposes no cap (the client's max_tokens and the provider
// decide), n_ctx comes from the config, and is_sleeping is always false because
// there is no local KV cache to offload.
func (s *Server) handleProps(w http.ResponseWriter, r *http.Request) {
	reg := s.Registry()
	requested := strings.TrimSpace(r.URL.Query().Get("model"))

	var (
		alias  string
		nCtx   int
		vision bool
	)
	if requested != "" {
		p, m, ok := findModel(reg, requested)
		if !ok {
			writeJSONError(w, http.StatusNotFound,
				fmt.Sprintf("model %q not found", requested), "invalid_request_error")
			return
		}
		alias, nCtx, vision = modelRef(p, m), m.Context, m.Capabilities["vision"]
	} else if p, m, ok := defaultModel(reg); ok {
		alias, nCtx, vision = modelRef(p, m), m.Context, m.Capabilities["vision"]
	}

	writeJSON(w, http.StatusOK, propsResponse{
		DefaultGenerationSettings: propsGenerationSettings{
			Params: map[string]any{"n_predict": -1},
			NCtx:   nCtx,
		},
		TotalSlots:       1,
		ModelAlias:       alias,
		ModelPath:        "", // no local file; ModelAlias is the routable id
		Modalities:       propsModalities{Vision: vision},
		ChatTemplate:     "",
		ChatTemplateCaps: map[string]any{},
		BuildInfo:        "lgllm " + version.String(),
		IsSleeping:       false,
	})
}

// --- shared helpers -----------------------------------------------------

// findModel maps a client-supplied name to an advertised model.
//
// Accepted forms, in order:
//   - the prefixed id this server advertises ("ak/model"),
//   - a bare model id; the default provider wins when several advertise it,
//   - an Ollama-style ":latest" suffix, which is ignored.
func findModel(reg *registry.Registry, requested string) (*registry.Provider, registry.Model, bool) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return nil, registry.Model{}, false
	}
	name := strings.TrimSuffix(requested, ":latest")

	if prefix, rest, ok := strings.Cut(name, "/"); ok && rest != "" {
		if p, found := reg.Provider(prefix); found {
			if m, found := advertisedModel(p, rest); found {
				return p, m, true
			}
		}
	}

	// A configured alias or combo is a known name even when the model behind it
	// is not in the advertised list.
	if t, ok := reg.Shortcut(name); ok && t.Provider != nil {
		if m, found := advertisedModel(t.Provider, t.Model); found {
			return t.Provider, m, true
		}
		return t.Provider, registry.Model{ID: t.Model}, true
	}

	var (
		fallback *registry.Provider
		model    registry.Model
		found    bool
	)
	for _, p := range reg.Providers() {
		m, ok := advertisedModel(p, name)
		if !ok {
			continue
		}
		if def, ok := reg.Default(); ok && def.ID == p.ID {
			return p, m, true
		}
		if !found {
			fallback, model, found = p, m, true
		}
	}
	if found {
		return fallback, model, true
	}
	return nil, registry.Model{}, false
}

// defaultModel picks the model described when no name was requested: the
// default provider's first model, else the first model of the first provider.
func defaultModel(reg *registry.Registry) (*registry.Provider, registry.Model, bool) {
	if p, ok := reg.Default(); ok && len(p.Models) > 0 {
		return p, p.Models[0], true
	}
	for _, p := range reg.Providers() {
		if len(p.Models) > 0 {
			return p, p.Models[0], true
		}
	}
	return nil, registry.Model{}, false
}

// advertisedModel finds one advertised model by its upstream id.
func advertisedModel(p *registry.Provider, id string) (registry.Model, bool) {
	for _, m := range p.Models {
		if m.ID == id {
			return m, true
		}
	}
	return registry.Model{}, false
}

// ollamaDetailsFor fills Ollama's details block for a provider's models.
//
// parent_model carries the provider id, which is the only provenance a proxy
// has. format is "api" rather than "gguf" because the model is remote, and
// family is left empty: it names a weight architecture, which is unknown here.
func ollamaDetailsFor(p *registry.Provider) ollamaDetails {
	return ollamaDetails{
		ParentModel: p.ID,
		Format:      "api",
		Families:    []string{},
	}
}

// ollamaCapabilities is the capability set an Ollama client understands, in a
// fixed order so repeated calls do not shuffle the list.
var ollamaCapabilities = []string{
	"completion", "tools", "thinking", "vision", "embedding", "insert",
}

// capabilitiesFor reports what a client may ask this model to do.
//
// "completion" is always advertised: every listed model answers a chat request.
// Anything else comes from config (models[].capabilities) because a proxy
// cannot introspect a remote model; unknown keys are dropped so the response
// only ever names capabilities a client understands.
func capabilitiesFor(m registry.Model) []string {
	caps := make([]string, 0, 3)
	for _, name := range ollamaCapabilities {
		if name == "completion" || m.Capabilities[name] {
			caps = append(caps, name)
		}
	}
	return caps
}

// modelModifiedAt is "when did the model list last change": the config file's
// modification time, falling back to process start. Ollama emits a timestamp in
// this field, so a zero value would decode as year 1 and look broken.
func (s *Server) modelModifiedAt() time.Time {
	if src := s.Config().Source; src != "" {
		if st, err := os.Stat(src); err == nil {
			return st.ModTime().UTC()
		}
	}
	return s.started.UTC().Truncate(time.Second)
}

// writeOllamaError reports an error in Ollama's shape: a flat "error" string,
// not the [OI] {"error":{...}} envelope. An Ollama client decodes the former,
// and the latter would surface as an opaque decode failure.
func writeOllamaError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
