package management

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestListAuthFiles_IncludesOrganizationNameFromManager(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	authDir := t.TempDir()
	fileName := "claude-0a1b2c3d-user@example.com.json"
	filePath := filepath.Join(authDir, fileName)
	if errWrite := os.WriteFile(filePath, []byte(`{"type":"claude","email":"user@example.com","organization_name":"Team A"}`), 0o600); errWrite != nil {
		t.Fatalf("failed to write auth file: %v", errWrite)
	}

	manager := coreauth.NewManager(nil, nil, nil)
	record := &coreauth.Auth{
		ID:       fileName,
		FileName: fileName,
		Provider: "claude",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path": filePath,
		},
		Metadata: map[string]any{
			"type":              "claude",
			"email":             "user@example.com",
			"organization_name": " Team A ",
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = &memoryAuthStore{}

	entry := firstAuthFileEntry(t, h)
	if got := entry["organization_name"]; got != "Team A" {
		t.Fatalf("expected organization_name %q, got %#v", "Team A", got)
	}
}

func TestListAuthFiles_OmitsEmptyOrganizationName(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	authDir := t.TempDir()
	fileName := "claude-user@example.com.json"
	filePath := filepath.Join(authDir, fileName)
	if errWrite := os.WriteFile(filePath, []byte(`{"type":"claude","email":"user@example.com"}`), 0o600); errWrite != nil {
		t.Fatalf("failed to write auth file: %v", errWrite)
	}

	manager := coreauth.NewManager(nil, nil, nil)
	record := &coreauth.Auth{
		ID:         fileName,
		FileName:   fileName,
		Provider:   "claude",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"path": filePath},
		Metadata:   map[string]any{"type": "claude", "email": "user@example.com", "organization_name": "  "},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = &memoryAuthStore{}

	entry := firstAuthFileEntry(t, h)
	if _, ok := entry["organization_name"]; ok {
		t.Fatalf("expected organization_name to be omitted, got %#v", entry["organization_name"])
	}
}

func TestListAuthFilesFromDisk_IncludesOrganizationName(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	authDir := t.TempDir()
	filePath := filepath.Join(authDir, "claude-0a1b2c3d-user@example.com.json")
	if errWrite := os.WriteFile(filePath, []byte(`{"type":"claude","email":"user@example.com","organization_name":"Team A"}`), 0o600); errWrite != nil {
		t.Fatalf("failed to write auth file: %v", errWrite)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)

	entry := firstAuthFileEntry(t, h)
	if got := entry["organization_name"]; got != "Team A" {
		t.Fatalf("expected organization_name %q, got %#v", "Team A", got)
	}
}
