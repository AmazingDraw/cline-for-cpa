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

// excludedModels is the exact-id / exact-leaf blacklist. A namespaced entry
// hides only that id; a bare leaf hides the leaf in every namespace.
var excludedModels []string

// excludedLeafPrefixes hides every leaf that starts with one of these strings
// (ASCII, case-insensitive), in every namespace. New qwen* / minimax* rows
// from a catalog refresh stay hidden without another code change.
var excludedLeafPrefixes = []string{
	"qwen",
	"minimax",
}

// servedVendorAliases maps a feed id that is not in a served namespace onto a
// client-facing served id. Chat still sends the feed id upstream. Added only
// after a live probe: nvidia/nemotron-3.5-lightning-free streams with cost 0
// (non-stream returns 500 empty content because the model emits reasoning
// first). The paid catalog id nvidia/nemotron-3.5-lightning is not aliased.
var servedVendorAliases = map[string]string{
	"nvidia/nemotron-3.5-lightning-free": "cline-free/nemotron-3.5-lightning",
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
	"deepseek-v4.1-flash-1017",
	"deepseek-v4-pro",
	"glm-5.3",
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

	for src, dest := range servedVendorAliases {
		if seen[dest] || isExcludedModel(dest) || !isServedNamespace(dest) {
			continue
		}
		meta, ok := activeCatalog[src]
		if !ok {
			meta, ok = generatedCatalog[src]
		}
		if !ok {
			continue
		}
		seen[dest] = true
		models = append(models, Model{ID: dest, Upstream: src, Meta: meta})
	}
	return models
}

func isServedModel(id string) bool {
	return isServedModelWithCatalog(id, currentCatalogMap())
}

// isServedModelWithCatalog decides exposure by namespace only.
//
// It used to also pass any catalog row whose Tier was free/clinePass, whatever
// its namespace. On 2026-10-21 the feed started listing free models as
// vendor/<slug>-free (reflection/beam-free, …): that rule advertised them, but
// NormalizeModel has no route for a foreign namespace and rewrote them to
// cline-pass/reflection/beam-free — an advertised model that cannot work and
// would be sent to the subscription pool. Until such ids get a verified route
// they stay in modelmeta (feed record) but are not served.
func isServedModelWithCatalog(id string, _ map[string]modelMeta) bool {
	return isServedNamespace(id)
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
	lower := strings.ToLower(leaf)
	for _, prefix := range excludedLeafPrefixes {
		prefix = strings.ToLower(strings.TrimSpace(prefix))
		if prefix != "" && strings.HasPrefix(lower, prefix) {
			return true
		}
	}
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
