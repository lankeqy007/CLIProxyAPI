package management

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestRunCodexAutoRefill_ClaimsAndImportsAPIKeyFiles(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	t.Setenv("TOKEN_ATLAS_API_KEY", "secret-token")

	var claimCalls atomic.Int32
	var downloadCalls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/claim":
			claimCalls.Add(1)
			if got := r.Header.Get("X-API-Key"); got != "secret-token" {
				t.Fatalf("claim api key = %q, want %q", got, "secret-token")
			}
			body, _ := io.ReadAll(r.Body)
			var payload map[string]int
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatalf("claim body decode error: %v", err)
			}
			if payload["count"] != 2 {
				t.Fatalf("claim count = %d, want 2", payload["count"])
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"claims":[{"token_id":"101"},{"token_id":"102"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/download/101":
			downloadCalls.Add(1)
			w.Header().Set("Content-Disposition", `attachment; filename="alpha.json"`)
			_, _ = w.Write([]byte(`{"type":"codex","email":"alpha@example.com","account_id":"acc-alpha","refresh_token":"r1"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/download/102":
			downloadCalls.Add(1)
			w.Header().Set("Content-Disposition", `attachment; filename="beta.json"`)
			_, _ = w.Write([]byte(`{"type":"codex","email":"beta@example.com","account_id":"acc-beta","refresh_token":"r2"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	authDir := t.TempDir()
	cfg := &config.Config{
		AuthDir: authDir,
		QuotaExceeded: config.QuotaExceeded{
			CodexAutoRefill: config.CodexAutoRefill{
				Enable:                  true,
				ProviderURL:             server.URL,
				AuthMode:                "api-key",
				APIKeyEnv:               "TOKEN_ATLAS_API_KEY",
				CheckIntervalSeconds:    1,
				MinClaimIntervalSeconds: 1,
				TimeoutSeconds:          5,
				LowWatermark:            1,
				TargetReady:             2,
				MaxClaimPerRun:          2,
				RequireConsecutiveLow:   1,
				VerifyAfterImport:       false,
				Priority:                -10,
				Note:                    "auto-refill",
			},
		},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(cfg, manager)

	h.runCodexAutoRefill()

	if claimCalls.Load() != 1 {
		t.Fatalf("claim calls = %d, want 1", claimCalls.Load())
	}
	if downloadCalls.Load() != 2 {
		t.Fatalf("download calls = %d, want 2", downloadCalls.Load())
	}

	auths := manager.List()
	if len(auths) != 2 {
		t.Fatalf("imported auth count = %d, want 2", len(auths))
	}
	for _, auth := range auths {
		if auth.Provider != "codex" {
			t.Fatalf("provider = %q, want codex", auth.Provider)
		}
		if auth.Metadata["note"] != "auto-refill" {
			t.Fatalf("note = %#v, want auto-refill", auth.Metadata["note"])
		}
		if auth.Metadata["auto_refill"] != true {
			t.Fatalf("auto_refill = %#v, want true", auth.Metadata["auto_refill"])
		}
		if priority, ok := auth.Metadata["priority"].(float64); !ok || int(priority) != -10 {
			t.Fatalf("priority = %#v, want -10", auth.Metadata["priority"])
		}
	}

	for _, name := range []string{"alpha.json", "beta.json"} {
		if _, err := os.Stat(filepath.Join(authDir, name)); err != nil {
			t.Fatalf("expected imported file %s to exist: %v", name, err)
		}
	}
}

func TestRunCodexAutoRefill_SkipsWhenReadyEnough(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	t.Setenv("TOKEN_ATLAS_API_KEY", "secret-token")

	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		http.NotFound(w, r)
	}))
	defer server.Close()

	authDir := t.TempDir()
	filePath := filepath.Join(authDir, "ready.json")
	if err := os.WriteFile(filePath, []byte(`{"type":"codex","email":"ready@example.com"}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       "ready-auth",
		Provider: "codex",
		FileName: filepath.Base(filePath),
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path":   filePath,
			"source": filePath,
		},
		Metadata: map[string]any{
			"type":  "codex",
			"email": "ready@example.com",
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	cfg := &config.Config{
		AuthDir: authDir,
		QuotaExceeded: config.QuotaExceeded{
			CodexAutoRefill: config.CodexAutoRefill{
				Enable:                  true,
				ProviderURL:             server.URL,
				AuthMode:                "api-key",
				APIKeyEnv:               "TOKEN_ATLAS_API_KEY",
				CheckIntervalSeconds:    1,
				MinClaimIntervalSeconds: 1,
				TimeoutSeconds:          5,
				LowWatermark:            1,
				TargetReady:             3,
				MaxClaimPerRun:          2,
				RequireConsecutiveLow:   1,
			},
		},
	}
	h := NewHandlerWithoutConfigFilePath(cfg, manager)

	h.runCodexAutoRefill()

	if requestCount.Load() != 0 {
		t.Fatalf("unexpected refill requests: %d", requestCount.Load())
	}
}

func TestRunCodexAutoRefill_UsesDirectAPIKeyWithoutEnv(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	var claimCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/claim":
			claimCalls.Add(1)
			if got := r.Header.Get("X-API-Key"); got != "direct-token" {
				t.Fatalf("claim api key = %q, want %q", got, "direct-token")
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"claims":[{"token_id":"201"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/download/201":
			w.Header().Set("Content-Disposition", `attachment; filename="direct.json"`)
			_, _ = w.Write([]byte(`{"type":"codex","email":"direct@example.com","account_id":"acc-direct","refresh_token":"r1"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	authDir := t.TempDir()
	cfg := &config.Config{
		AuthDir: authDir,
		QuotaExceeded: config.QuotaExceeded{
			CodexAutoRefill: config.CodexAutoRefill{
				Enable:                  true,
				ProviderURL:             server.URL,
				AuthMode:                "api-key",
				APIKey:                  "direct-token",
				CheckIntervalSeconds:    1,
				MinClaimIntervalSeconds: 1,
				TimeoutSeconds:          5,
				LowWatermark:            1,
				TargetReady:             1,
				MaxClaimPerRun:          1,
				RequireConsecutiveLow:   1,
			},
		},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(cfg, manager)

	h.runCodexAutoRefill()

	if claimCalls.Load() != 1 {
		t.Fatalf("claim calls = %d, want 1", claimCalls.Load())
	}
	if len(manager.List()) != 1 {
		t.Fatalf("imported auth count = %d, want 1", len(manager.List()))
	}
}

func TestRunCodexAutoRefill_UsesDirectSessionWithoutEnv(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	archiveBody := buildCodexAutoRefillArchive(t, map[string]string{
		"session.json": `{"type":"codex","email":"session@example.com","account_id":"acc-session","refresh_token":"r1"}`,
	})

	var claimCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("token_atlas_session")
		if err != nil {
			t.Fatalf("expected session cookie: %v", err)
		}
		if cookie.Value != "session-token" {
			t.Fatalf("session cookie = %q, want %q", cookie.Value, "session-token")
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/me/claim":
			claimCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		case r.Method == http.MethodGet && r.URL.Path == "/me/claims/archive":
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write(archiveBody)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	authDir := t.TempDir()
	cfg := &config.Config{
		AuthDir: authDir,
		QuotaExceeded: config.QuotaExceeded{
			CodexAutoRefill: config.CodexAutoRefill{
				Enable:                  true,
				ProviderURL:             server.URL,
				AuthMode:                "session",
				SessionValue:            "session-token",
				CheckIntervalSeconds:    1,
				MinClaimIntervalSeconds: 1,
				TimeoutSeconds:          5,
				LowWatermark:            1,
				TargetReady:             1,
				MaxClaimPerRun:          1,
				RequireConsecutiveLow:   1,
			},
		},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(cfg, manager)

	h.runCodexAutoRefill()

	if claimCalls.Load() != 1 {
		t.Fatalf("claim calls = %d, want 1", claimCalls.Load())
	}
	if len(manager.List()) != 1 {
		t.Fatalf("imported auth count = %d, want 1", len(manager.List()))
	}
}

func buildCodexAutoRefillArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()

	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, body := range files {
		fileWriter, err := writer.Create(name)
		if err != nil {
			t.Fatalf("create zip entry %s: %v", name, err)
		}
		if _, err := fileWriter.Write([]byte(body)); err != nil {
			t.Fatalf("write zip entry %s: %v", name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close zip writer: %v", err)
	}
	return buffer.Bytes()
}
