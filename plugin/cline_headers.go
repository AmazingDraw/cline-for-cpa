package plugin

import (
	"net/http"
	"strings"
)

// Client identity headers, aligned with the official builder
//
//	@cline/llms  src/providers/cline-client-headers.ts  → buildClineClientHeaders()
//
// which emits (bundled form in dist/providers.js, function cC):
//
//	HTTP-Referer, X-Title              : static
//	User-Agent                         : "Cline/<client.version>"
//	X-CLIENT-TYPE                      : client.name ?? "cline-<source>"   (cline-desktop|cline-cli|cline-sdk)
//	X-CLIENT-VERSION                   : client.version
//	X-PLATFORM                         : client.platform ?? source         (desktop|cli|vscode)
//	X-PLATFORM-VERSION                 : client.platformVersion ?? version
//	X-CORE-VERSION                     : core version
//	X-IS-MULTIROOT                     : client.isMultiRoot ? "true" : "false"
//	X-Task-ID                          : client session id
//
// Earlier revisions of this plugin sent only the first four, which made the
// upstream see an incomplete client identity.
const (
	defaultClientType    = "cline-desktop"
	defaultClientVersion = "0.0.36" // desktop UA fallback; bump when releasing the plugin
	defaultHTTPReferer   = "https://cline.bot"
	defaultXTitle        = "Cline"
	defaultMultiRoot     = "false"
)

// clineHeaders is the resolved client-identity header set.
type clineHeaders struct {
	ClientType  string
	Version     string // bare version, e.g. "0.0.33"
	Referer     string
	Title       string
	Platform    string
	PlatformVer string
	CoreVersion string
	IsMultiRoot string
	TaskID      string
}

// bareVersion tolerates both "3.0.64" and "Cline/3.0.64" in configuration.
func bareVersion(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "Cline/")
	return strings.TrimSpace(v)
}

// resolveClineHeaders fills every official header from config + defaults + dynamic version.
func resolveClineHeaders(cfg pluginConfig) clineHeaders {
	clientType := firstNonEmpty(cfg.ClientType, defaultClientType)

	// A profile sets the coherent identity triple in one go (client type,
	// platform); explicit per-field config always wins.
	profilePlatform := ""
	switch strings.ToLower(strings.TrimSpace(cfg.Profile)) {
	case "cli":
		profilePlatform = "cli"
		if strings.TrimSpace(cfg.ClientType) == "" {
			clientType = "cline-cli"
		}
	case "desktop":
		profilePlatform = "desktop"
	}

	// Dynamic version auto-detection: if user explicitly configured client_version,
	// that wins. Otherwise detect latest live version from npm/cache, falling back
	// safely to defaultClientVersion (0.0.34).
	baseVersion := bareVersion(cfg.ClientVersion)
	if baseVersion == "" {
		baseVersion = resolveLiveClientVersion(clientType, defaultClientVersion)
	}

	h := clineHeaders{
		ClientType: clientType,
		Version:    baseVersion,
		Referer:    firstNonEmpty(cfg.HTTPReferer, defaultHTTPReferer),
		Title:      firstNonEmpty(cfg.XTitle, defaultXTitle),
		TaskID:     strings.TrimSpace(cfg.TaskID),
	}

	h.Platform = firstNonEmpty(cfg.Platform, profilePlatform, platformFromClientType(h.ClientType))
	h.PlatformVer = bareVersion(firstNonEmpty(cfg.PlatformVersion, h.Version))
	h.CoreVersion = bareVersion(firstNonEmpty(cfg.CoreVersion, h.Version))
	h.IsMultiRoot = firstNonEmpty(cfg.IsMultiRoot, defaultMultiRoot)
	return h
}

// platformFromClientType mirrors the official `cline-<source>` convention.
func platformFromClientType(clientType string) string {
	ct := strings.ToLower(strings.TrimSpace(clientType))
	if strings.HasPrefix(ct, "cline-") {
		return strings.TrimPrefix(ct, "cline-")
	}
	if ct == "" {
		return "desktop"
	}
	return ct
}

// applyClineHeaders sets the full official client-identity header set.
func applyClineHeaders(req *http.Request, cfg pluginConfig) {
	if req == nil {
		return
	}
	h := resolveClineHeaders(cfg)
	req.Header.Set("HTTP-Referer", h.Referer)
	req.Header.Set("X-Title", h.Title)
	req.Header.Set("User-Agent", "Cline/"+h.Version)
	req.Header.Set("X-Client-Type", h.ClientType)
	req.Header.Set("X-Client-Version", h.Version)
	req.Header.Set("X-Platform", h.Platform)
	req.Header.Set("X-Platform-Version", h.PlatformVer)
	req.Header.Set("X-Core-Version", h.CoreVersion)
	req.Header.Set("X-IS-MULTIROOT", h.IsMultiRoot)
	if h.TaskID != "" {
		// The official client sends its session id here; we only send it when a
		// task id is configured, since a fabricated one would be misleading.
		req.Header.Set("X-Task-ID", h.TaskID)
	}
}
