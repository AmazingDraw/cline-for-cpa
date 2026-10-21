package plugin

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
)

// Client identity headers, aligned with the official builder
//
//	cline/cline sdk/packages/llms/src/providers/request-headers.ts
//	  → buildClineRequestHeaders()   (called from @cline/core local-runtime-bootstrap.ts
//	                                   and hub-runtime-host.ts with coreVersion = @cline/core package.json version)
//
//	HTTP-Referer, X-Title   : static
//	User-Agent              : "Cline/<client.version>"
//	X-CLIENT-TYPE           : client.name ?? "cline-<source>"
//	X-CLIENT-VERSION        : client.version
//	X-PLATFORM              : client.platform ?? source
//	X-PLATFORM-VERSION      : client.platformVersion ?? client.version
//	X-CORE-VERSION          : @cline/core package version   ← independent of the client version
//	X-IS-MULTIROOT          : client.isMultiRoot ? "true" : "false"
//	X-Task-ID               : session id (omitted when there is none)
//
// Who the "client" is (apps/*/client identity):
//
//	Desktop  apps/examples/desktop-app/sidecar/client-context.ts
//	         name "cline-desktop", platform "Cline Desktop",
//	         version = platformVersion = desktop app package.json (git tag desktop-v<ver>)
//	CLI      apps/cli/src/utils/cline-client-identity.ts
//	         name "cline-cli", platform "cli", version = CLI build (npm `cline`, tag cli-v<ver>)
//
// So there are THREE independent versions, each with its own fallback, cache
// key and probe (see version_updater.go): desktop app, CLI, and @cline/core.
const (
	defaultClientType = "cline-desktop"
	// defaultClientVersion is the Cline Desktop app version (GitHub tag desktop-v*).
	// It is NOT the @cline/core npm version. Bump to live latest on every release.
	defaultClientVersion = "0.0.48"
	// defaultCLIClientVersion is the Cline CLI version (npm `cline`, tag cli-v*).
	defaultCLIClientVersion = "3.0.73"
	// defaultCoreVersion is the @cline/core npm version, sent as X-Core-Version.
	defaultCoreVersion = "0.0.95"
	// desktopPlatform is the X-Platform value the official Desktop sends.
	desktopPlatform    = "Cline Desktop"
	defaultHTTPReferer = "https://cline.bot"
	defaultXTitle      = "Cline"
	defaultMultiRoot   = "false"
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
		profilePlatform = desktopPlatform
	}

	// Client version: explicit client_version wins; otherwise the newer of the
	// live cache and the compiled-in fallback for this client kind (desktop app
	// or CLI). A stale cache can therefore never downgrade a fresh build.
	baseVersion := bareVersion(cfg.ClientVersion)
	if baseVersion == "" {
		key, fallback := targetKeyDesktop, defaultClientVersion
		if isCLIClientType(clientType) {
			key, fallback = targetKeyCLI, defaultCLIClientVersion
		}
		baseVersion = resolveLiveVersion(key, fallback)
	}
	// Core version is independent of the client version (official X-CORE-VERSION
	// is the @cline/core package version for both Desktop and CLI).
	coreVersion := bareVersion(cfg.CoreVersion)
	if coreVersion == "" {
		coreVersion = resolveLiveVersion(targetKeyCore, defaultCoreVersion)
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
	h.CoreVersion = coreVersion
	h.IsMultiRoot = firstNonEmpty(cfg.IsMultiRoot, defaultMultiRoot)
	return h
}

// isCLIClientType reports whether the client type names the Cline CLI.
func isCLIClientType(clientType string) bool {
	ct := strings.ToLower(strings.TrimSpace(clientType))
	return strings.HasSuffix(ct, "-cli") || ct == "cli" || strings.Contains(ct, "cline-cli")
}

// platformFromClientType mirrors the official client identities: Desktop sends
// "Cline Desktop", the CLI sends "cli"; anything else follows the SDK's
// `cline-<source>` → `<source>` convention.
func platformFromClientType(clientType string) string {
	ct := strings.ToLower(strings.TrimSpace(clientType))
	switch ct {
	case "", "cline-desktop", "desktop":
		return desktopPlatform
	}
	if strings.HasPrefix(ct, "cline-") {
		return strings.TrimPrefix(ct, "cline-")
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
	logIdentityOnChange(h)
}

var (
	identityLogMu   sync.Mutex
	lastIdentityLog string
)

// logIdentityOnChange writes the (non-secret) identity header set to the plugin
// log once per distinct value, so the versions actually sent upstream can be
// verified after a deploy without packet capture.
func logIdentityOnChange(h clineHeaders) {
	line := fmt.Sprintf("client identity User-Agent=Cline/%s X-Client-Type=%s X-Client-Version=%s X-Platform=%q X-Platform-Version=%s X-Core-Version=%s X-IS-MULTIROOT=%s",
		h.Version, h.ClientType, h.Version, h.Platform, h.PlatformVer, h.CoreVersion, h.IsMultiRoot)
	identityLogMu.Lock()
	changed := line != lastIdentityLog
	if changed {
		lastIdentityLog = line
	}
	identityLogMu.Unlock()
	if changed {
		pluginLogf("%s", line)
	}
}
