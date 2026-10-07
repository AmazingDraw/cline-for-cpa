package plugin

import (
	"strings"
	"sync"
)

// modelMeta is the per-model metadata Cline publishes for a model.
type modelMeta struct {
	// Tier is the Cline tier the id belongs to: recommended / free / clinePass / clineCloud.
	Tier string
	// DisplayName is the human-readable name Cline shows in its own picker.
	DisplayName string
	// Description is Cline's short model blurb.
	Description string
	// Underlying is the vendor catalog id the model resolves to.
	Underlying string
	// ContextLength is the maximum combined context window.
	ContextLength int64
	// MaxOutputTokens is the maximum completion token count.
	MaxOutputTokens int64
	// InputModalities / OutputModalities are the accepted/produced modality names.
	InputModalities  []string
	OutputModalities []string
	// Parameters lists the request parameters the model supports.
	Parameters []string
}

// Model is one model this plugin advertises to the host.
type Model struct {
	ID       string
	Upstream string
	Meta     modelMeta
}

// servedNamespaces lists the tiers this plugin routes. Only namespaces whose
// billing behaviour is verified end to end are exposed: cline-pass/ (subscription),
// cline-free/ (free pool), and stealth/ (experimental free pool).
// cline-cloud/ stays in modelmeta (upstream feed) but is not served.
var servedNamespaces = []string{namespacePass, namespaceFree, namespaceStealth}

// excludedModels is the blacklist. Everything in a served namespace is exposed
// by default. (Removed solar-pro4 since it was never in the official free tier).
var excludedModels = []string{
	"cline-pass/qwen3.8-max",
	"cline-pass/qwen3.7-max",
	"cline-pass/qwen3.7-plus",
}

// fallbackPassLeaves keeps the plugin functional when the generated catalog is
// absent (a cold start with no modelmeta_gen.go and no reachable upstream).
//
// Kept in sync with the current ClinePass lineup: the four leaves below are the
// ones that survived every upstream reshuffle so far. They are a last-resort
// floor, not the served list — the generated catalog and the live feed decide
// what is actually exposed, so this list deliberately stays small and stable
// rather than tracking every new release.
var fallbackPassLeaves = []string{
	"deepseek-v4.1-flash",
	"mimo-v2.6-flash",
	"mimo-v2.6-pro",
	"kimi-k3",
}

var (
	indexMu    sync.RWMutex
	modelIndex = make(map[string]Model)
)

func init() {
	rebuildModelIndex()
}

func rebuildModelIndex() {
	models := exposedModels()
	newIdx := make(map[string]Model, len(models))
	for _, m := range models {
		newIdx[m.ID] = m
	}

	indexMu.Lock()
	modelIndex = newIdx
	indexMu.Unlock()
}

// exposedModels returns the model list, combining dynamic discovery with the static catalog fallback.
func exposedModels() []Model {
	modelsDiskLoadedOnce.Do(loadModelsSnapshotFromDisk)

	modelsUpdaterMu.RLock()
	hasDynamic := len(dynamicCatalog) > 0
	dynCatalog := dynamicCatalog
	dynOrder := dynamicOrder
	modelsUpdaterMu.RUnlock()

	activeCatalog := generatedCatalog
	activeOrder := generatedOrder
	if hasDynamic {
		activeCatalog = dynCatalog
		activeOrder = dynOrder
	}

	models := make([]Model, 0, len(activeOrder))
	seen := make(map[string]bool)

	for _, id := range activeOrder {
		if seen[id] || !isServedModelWithCatalog(id, activeCatalog) || isExcludedModel(id) {
			continue
		}
		seen[id] = true
		models = append(models, Model{ID: id, Upstream: id, Meta: activeCatalog[id]})
	}

	// Always ensure static models are included if dynamic missed any
	for _, id := range generatedOrder {
		if seen[id] || !isServedModelWithCatalog(id, generatedCatalog) || isExcludedModel(id) {
			continue
		}
		seen[id] = true
		models = append(models, Model{ID: id, Upstream: id, Meta: generatedCatalog[id]})
	}

	if len(models) == 0 {
		for _, leaf := range fallbackPassLeaves {
			id := namespacePass + leaf
			models = append(models, Model{ID: id, Upstream: id, Meta: generatedCatalog[id]})
		}
	}
	return models
}

func isServedModel(id string) bool {
	return isServedModelWithCatalog(id, currentCatalogMap())
}

func isServedModelWithCatalog(id string, catalog map[string]modelMeta) bool {
	if isServedNamespace(id) {
		return true
	}
	if meta, ok := catalog[id]; ok {
		if meta.Tier == "free" || meta.Tier == "clinePass" {
			return true
		}
	}
	return false
}

func currentCatalogMap() map[string]modelMeta {
	modelsUpdaterMu.RLock()
	defer modelsUpdaterMu.RUnlock()
	if len(dynamicCatalog) > 0 {
		return dynamicCatalog
	}
	return generatedCatalog
}

func isServedNamespace(id string) bool {
	for _, ns := range servedNamespaces {
		if strings.HasPrefix(id, ns) {
			return true
		}
	}
	return false
}

func isExcludedModel(id string) bool {
	leaf := leafOfModel(id)
	for _, entry := range excludedModels {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if entry == id || entry == leaf {
			return true
		}
	}
	return false
}

func lookupModel(id string) (Model, bool) {
	indexMu.RLock()
	m, ok := modelIndex[id]
	indexMu.RUnlock()
	if ok {
		return m, true
	}
	// Fallback check against exposed models directly if index hasn't caught up
	cat := currentCatalogMap()
	if meta, exists := cat[id]; exists && isServedModelWithCatalog(id, cat) && !isExcludedModel(id) {
		return Model{ID: id, Upstream: id, Meta: meta}, true
	}
	return Model{}, false
}

// StaticModelIDs returns advertised model ids for model.static / model.for_auth.
func StaticModelIDs() []string {
	models := exposedModels()
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.ID)
	}
	return out
}

func leafOfModel(id string) string {
	if i := strings.LastIndex(id, "/"); i >= 0 {
		return id[i+1:]
	}
	return id
}
