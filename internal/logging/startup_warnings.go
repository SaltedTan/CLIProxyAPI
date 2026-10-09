package logging

import (
	"io"
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
// ConfigureLogOutput first sets the log output. They are still logged as usual.
func HoldStartupWarnings() {
	startupWarningsMu.Lock()
	defer startupWarningsMu.Unlock()
	if startupWarnings != nil {
		return
	}
	startupWarnings = &startupWarningHook{}
	log.AddHook(startupWarnings)
}

// takeStartupWarnings stops keeping warnings and returns the kept ones. It
// returns nil when nothing is held, so only the first ConfigureLogOutput sees
// them.
func takeStartupWarnings() [][]byte {
	startupWarningsMu.Lock()
	hook := startupWarnings
	startupWarnings = nil
	startupWarningsMu.Unlock()
	if hook == nil {
		return nil
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
	defer hook.mu.Unlock()
	lines := hook.lines
	hook.lines = nil
	return lines
}

// writeStartupWarnings copies the kept warnings into a new log file. They were
// logged before the file existed, so they only reached the console. It runs
// before the logger switches to the file, so no warning reaches the file both
// directly and as a copy.
func writeStartupWarnings(w io.Writer) {
	for _, line := range takeStartupWarnings() {
		if _, errWrite := w.Write(line); errWrite != nil {
			return
		}
	}
}
