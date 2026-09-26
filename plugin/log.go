package plugin

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

var (
	logMu       sync.Mutex
	debugLogsOn = strings.TrimSpace(os.Getenv("CLINE_FOR_CPA_DEBUG")) != ""
)

// pluginLogf writes a diagnostic line to stderr.
func pluginLogf(format string, args ...any) {
	logMu.Lock()
	defer logMu.Unlock()
	fmt.Fprintf(os.Stderr, "[cline-for-cpa] "+format+"\n", args...)
}

// pluginDebugf logs only when CLINE_FOR_CPA_DEBUG is set (official sdkDebug).
func pluginDebugf(format string, args ...any) {
	if !debugLogsOn {
		return
	}
	pluginLogf(format, args...)
}
