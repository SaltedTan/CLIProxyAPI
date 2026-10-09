package logging

import (
	"sync"

	log "github.com/sirupsen/logrus"
)

// maxHeldStartupWarnings bounds the startup warnings kept for the log file.
const maxHeldStartupWarnings = 256

// startupWarningHook keeps a formatted copy of each warning and error logged
// before the log output is configured, such as config load diagnostics.
type startupWarningHook struct {
	mu    sync.Mutex
	lines [][]byte
}

var (
	startupWarningsMu sync.Mutex
	startupWarnings   *startupWarningHook
)

// Levels returns the levels the hook keeps.
func (h *startupWarningHook) Levels() []log.Level {
	return []log.Level{log.PanicLevel, log.FatalLevel, log.ErrorLevel, log.WarnLevel}
}

// Fire keeps the entry as the configured formatter renders it, with its
// original time and source.
func (h *startupWarningHook) Fire(entry *log.Entry) error {
	formatted, errFormat := entry.Logger.Formatter.Format(entry)
	if errFormat != nil {
		return errFormat
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.lines) < maxHeldStartupWarnings {
		h.lines = append(h.lines, append([]byte(nil), formatted...))
	}
	return nil
}

// HoldStartupWarnings starts keeping the warnings and errors logged until
// ReplayStartupWarnings. They are still logged as usual.
func HoldStartupWarnings() {
	startupWarningsMu.Lock()
	defer startupWarningsMu.Unlock()
	if startupWarnings != nil {
		return
	}
	startupWarnings = &startupWarningHook{}
	log.AddHook(startupWarnings)
}

// ReplayStartupWarnings stops keeping warnings and writes the kept ones to the
// log file when ConfigureLogOutput enabled one. They were logged before the
// file existed, so they only reached the console. Without a log file the
// console already shows them and nothing is written again.
func ReplayStartupWarnings() {
	startupWarningsMu.Lock()
	hook := startupWarnings
	startupWarnings = nil
	startupWarningsMu.Unlock()
	if hook == nil {
		return
	}

	logger := log.StandardLogger()
	hooks := make(log.LevelHooks)
	for level, levelHooks := range logger.Hooks {
		for _, levelHook := range levelHooks {
			if levelHook != hook {
				hooks[level] = append(hooks[level], levelHook)
			}
		}
	}
	logger.ReplaceHooks(hooks)

	hook.mu.Lock()
	lines := hook.lines
	hook.lines = nil
	hook.mu.Unlock()

	writerMu.RLock()
	defer writerMu.RUnlock()
	if logWriter == nil {
		return
	}
	for _, line := range lines {
		if _, errWrite := logWriter.Write(line); errWrite != nil {
			return
		}
	}
}
