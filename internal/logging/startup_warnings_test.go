package logging

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
)

// useStartupTestLogger sends the standard logger to a buffer standing in for
// the console and restores the logger afterwards.
func useStartupTestLogger(t *testing.T) *bytes.Buffer {
	t.Helper()
	SetupBaseLogger()
	logger := log.StandardLogger()
	origOut, origFormatter, origLevel := logger.Out, logger.Formatter, logger.GetLevel()
	origHooks := logger.ReplaceHooks(make(log.LevelHooks))
	var console bytes.Buffer
	logger.SetOutput(&console)
	logger.SetFormatter(&LogFormatter{})
	logger.SetLevel(log.InfoLevel)
	t.Cleanup(func() {
		ReplayStartupWarnings()
		closeLogOutputs()
		logger.ReplaceHooks(origHooks)
		logger.SetOutput(origOut)
		logger.SetFormatter(origFormatter)
		logger.SetLevel(origLevel)
	})
	return &console
}

func TestReplayStartupWarningsWritesConfigWarningsToLogFile(t *testing.T) {
	console := useStartupTestLogger(t)
	dir := t.TempDir()
	t.Setenv("WRITABLE_PATH", dir)
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\ndebgu: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	HoldStartupWarnings()
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	log.Info("startup info line")
	cfg.LoggingToFile = true
	if err = ConfigureLogOutput(cfg); err != nil {
		t.Fatal(err)
	}
	ReplayStartupWarnings()
	log.Warn("warning after startup")
	closeLogOutputs()

	warning := `config: unknown key "debgu" (line 2) is ignored`
	if got := strings.Count(console.String(), warning); got != 1 {
		t.Fatalf("console has the warning %d times, want 1:\n%s", got, console.String())
	}
	file, err := os.ReadFile(filepath.Join(dir, "logs", "main.log"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(file), warning); got != 1 {
		t.Fatalf("log file has the warning %d times, want 1:\n%s", got, file)
	}
	// The replayed line keeps its source, not the replay's.
	if !strings.Contains(string(file), "[config_unknown_keys.go:") {
		t.Fatalf("replayed warning lost its source:\n%s", file)
	}
	if strings.Contains(string(file), "startup info line") {
		t.Fatalf("log file replays info lines:\n%s", file)
	}
	if got := strings.Count(string(file), "warning after startup"); got != 1 {
		t.Fatalf("log file has the later warning %d times, want 1:\n%s", got, file)
	}
}

func TestReplayStartupWarningsWithoutLogFileWritesNothingAgain(t *testing.T) {
	console := useStartupTestLogger(t)
	closeLogOutputs()

	HoldStartupWarnings()
	log.Warn("held warning")
	ReplayStartupWarnings()
	log.Warn("later warning")

	for _, message := range []string{"held warning", "later warning"} {
		if got := strings.Count(console.String(), message); got != 1 {
			t.Fatalf("console has %q %d times, want 1:\n%s", message, got, console.String())
		}
	}
	for _, levelHooks := range log.StandardLogger().Hooks {
		for _, hook := range levelHooks {
			if _, ok := hook.(*startupWarningHook); ok {
				t.Fatal("startup warning hook is still installed after replay")
			}
		}
	}
}
