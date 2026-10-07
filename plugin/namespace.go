package plugin

import (
	"fmt"
	"strings"
)

// Cline exposes models under several namespaces. The prefix decides which upstream
// pool the request is billed against, so it must survive routing untouched:
//
//	cline-pass/…   the ClinePass subscription — usage counts against the plan
//	cline-free/…   the free tier — per-model rate limit, no plan usage
//	cline-cloud/…  Cline Cloud usage-based pool (served selectively)
//
// Until 0.3.0 every id was rewritten to cline-pass/, which silently moved free
// models into the subscription pool.
const (
	namespacePass    = "cline-pass/"
	namespaceFree    = "cline-free/"
	namespaceStealth = "stealth/"
	namespaceCloud   = "cline-cloud/"
)

// NormalizeModel resolves a client-supplied model id into
// (clientFacingID, upstreamModelID, error).
//
// Rules:
//   - a known namespace prefix is preserved; repeated prefixes are collapsed;
//   - no prefix → cline-pass, so existing clients keep working unchanged;
//   - a vendor prefix in front of a model we publish is dropped
//     (deepseek/deepseek-v4.1-flash → cline-pass/deepseek-v4.1-flash); without
//     that lookup the leaf would go upstream verbatim and fail as model_not_found.
//
// Unknown leaves pass through on purpose: in this deployment the host filters
// models, and a bad id should surface as the upstream's own model_not_found
// rather than a plugin-side guess.
func NormalizeModel(raw string) (string, string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", "", fmt.Errorf("model id is required")
	}

	ns, leaf := splitNamespace(name)
	leaf = strings.TrimSpace(leaf)
	if leaf == "" {
		return "", "", fmt.Errorf("empty model id after alias resolution")
	}
	if i := strings.LastIndex(leaf, "/"); i >= 0 {
		if tail := leaf[i+1:]; tail != "" && knownLeaf(ns, tail) {
			leaf = tail
		}
	}
	if ns == "" {
		ns = namespacePass
	}

	clientID := ns + leaf
	upstream := clientID
	if m, ok := lookupModel(clientID); ok && strings.TrimSpace(m.Upstream) != "" {
		upstream = m.Upstream
	}
	return clientID, upstream, nil
}

// splitNamespace peels every known namespace prefix off the id and reports the
// first one seen — that is the namespace the caller asked for.
func splitNamespace(name string) (namespace, leaf string) {
	for {
		switch {
		case strings.HasPrefix(name, namespacePass):
			if namespace == "" {
				namespace = namespacePass
			}
			name = name[len(namespacePass):]
		case strings.HasPrefix(name, namespaceFree):
			if namespace == "" {
				namespace = namespaceFree
			}
			name = name[len(namespaceFree):]
		case strings.HasPrefix(name, namespaceStealth):
			if namespace == "" {
				namespace = namespaceStealth
			}
			name = name[len(namespaceStealth):]
		case strings.HasPrefix(name, namespaceCloud):
			if namespace == "" {
				namespace = namespaceCloud
			}
			name = name[len(namespaceCloud):]
		default:
			return namespace, name
		}
	}
}

// knownLeaf reports whether the leaf is one we publish — in the requested
// namespace, or in either namespace when none was requested.
func knownLeaf(namespace, leaf string) bool {
	if namespace != "" {
		_, ok := lookupModel(namespace + leaf)
		return ok
	}
	if _, ok := lookupModel(namespacePass + leaf); ok {
		return true
	}
	if _, ok := lookupModel(namespaceFree + leaf); ok {
		return true
	}
	if _, ok := lookupModel(namespaceStealth + leaf); ok {
		return true
	}
	_, ok := lookupModel(namespaceCloud + leaf)
	return ok
}
