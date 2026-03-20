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
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

func TestRunCodexAutoRefill_UsesPerRequestTimeoutInsteadOfWholeRunDeadline(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	t.Setenv("TOKEN_ATLAS_API_KEY", "secret-token")

	var claimCalls atomic.Int32
	var downloadCalls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/claim":
			claimCalls.Add(1)
			time.Sleep(450 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"claims":[{"token_id":"301"},{"token_id":"302"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/download/301":
			downloadCalls.Add(1)
			time.Sleep(450 * time.Millisecond)
			w.Header().Set("Content-Disposition", `attachment; filename="slow-alpha.json"`)
			_, _ = w.Write([]byte(`{"type":"codex","email":"slow-alpha@example.com","account_id":"acc-slow-alpha","refresh_token":"r1"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/download/302":
			downloadCalls.Add(1)
			time.Sleep(450 * time.Millisecond)
			w.Header().Set("Content-Disposition", `attachment; filename="slow-beta.json"`)
			_, _ = w.Write([]byte(`{"type":"codex","email":"slow-beta@example.com","account_id":"acc-slow-beta","refresh_token":"r2"}`))
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
				TimeoutSeconds:          1,
				LowWatermark:            1,
				TargetReady:             2,
				MaxClaimPerRun:          2,
				RequireConsecutiveLow:   1,
				VerifyAfterImport:       false,
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
	if len(manager.List()) != 2 {
		t.Fatalf("imported auth count = %d, want 2", len(manager.List()))
	}
}

func TestRunCodexAutoRefill_DownloadsWithBoundedConcurrencyAndLogsDuration(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	t.Setenv("TOKEN_ATLAS_API_KEY", "secret-token")

	var (
		activeDownloads atomic.Int32
		maxDownloads    atomic.Int32
		downloadCalls   atomic.Int32
	)

	updateMax := func(current int32) {
		for {
			existing := maxDownloads.Load()
			if current <= existing {
				return
			}
			if maxDownloads.CompareAndSwap(existing, current) {
				return
			}
		}
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/claim":
			time.Sleep(40 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"claims":[{"token_id":"401"},{"token_id":"402"},{"token_id":"403"},{"token_id":"404"},{"token_id":"405"},{"token_id":"406"}]}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/download/"):
			current := activeDownloads.Add(1)
			updateMax(current)
			downloadCalls.Add(1)
			time.Sleep(80 * time.Millisecond)
			activeDownloads.Add(-1)
			tokenID := filepath.Base(r.URL.Path)
			w.Header().Set("Content-Disposition", `attachment; filename="`+tokenID+`.json"`)
			_, _ = w.Write([]byte(`{"type":"codex","email":"` + tokenID + `@example.com","account_id":"acc-` + tokenID + `","refresh_token":"r-` + tokenID + `"}`))
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
				TimeoutSeconds:          1,
				LowWatermark:            1,
				TargetReady:             6,
				MaxClaimPerRun:          6,
				RequireConsecutiveLow:   1,
				VerifyAfterImport:       false,
			},
		},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(cfg, manager)

	h.runCodexAutoRefill()

	if downloadCalls.Load() != 6 {
		t.Fatalf("download calls = %d, want 6", downloadCalls.Load())
	}
	if got := maxDownloads.Load(); got <= 1 {
		t.Fatalf("max concurrent downloads = %d, want > 1", got)
	} else if got > codexAutoRefillDefaultDownloadConcurrency {
		t.Fatalf("max concurrent downloads = %d, want <= %d", got, codexAutoRefillDefaultDownloadConcurrency)
	}

	status := h.codexAutoRefillStatusSnapshot(h.currentCodexAutoRefillRuntimeConfig(), h.codexAutoRefillPool(time.Now()), codexAutoRefillProviderQuotaSnapshot{}, time.Now())
	durationsByPath := map[string]int64{}
	for _, entry := range status.Logs {
		if entry.Event == "provider-response" && entry.Path != "" {
			durationsByPath[entry.Path] = entry.DurationMs
		}
	}
	loggedPaths := make([]string, 0, len(durationsByPath))
	for path := range durationsByPath {
		loggedPaths = append(loggedPaths, path)
	}

	if got := durationsByPath["/api/claim"]; got <= 0 {
		t.Fatalf("claim duration = %d, want > 0 (logs=%v)", got, loggedPaths)
	}
	for _, path := range []string{"/api/download/401", "/api/download/402"} {
		if got := durationsByPath[path]; got <= 0 {
			t.Fatalf("download duration for %s = %d, want > 0 (logs=%v)", path, got, loggedPaths)
		}
	}
}

func TestCodexAutoRefillProviderQuotaStatus_FetchesAndCachesSessionSnapshot(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	var meCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/me" {
			http.NotFound(w, r)
			return
		}
		meCalls.Add(1)
		cookie, err := r.Cookie("token_atlas_session")
		if err != nil {
			t.Fatalf("expected session cookie: %v", err)
		}
		if cookie.Value != "session-token" {
			t.Fatalf("session cookie = %q, want %q", cookie.Value, "session-token")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user":{"id":"u-1","name":"linuxdo","email":"linuxdo@example.com"},"quota":{"remaining":11,"used":4,"limit":15},"claims":{"count":4,"limit":15}}`))
	}))
	defer server.Close()

	cfg := &config.Config{
		QuotaExceeded: config.QuotaExceeded{
			CodexAutoRefill: config.CodexAutoRefill{
				ProviderURL:  server.URL,
				AuthMode:     "session",
				SessionValue: "session-token",
			},
		},
	}
	h := NewHandlerWithoutConfigFilePath(cfg, coreauth.NewManager(nil, nil, nil))
	runtimeCfg := h.currentCodexAutoRefillRuntimeConfig()
	startedAt := time.Now()

	first := h.codexAutoRefillProviderQuotaStatus(context.Background(), runtimeCfg, startedAt)
	second := h.codexAutoRefillProviderQuotaStatus(context.Background(), runtimeCfg, startedAt.Add(30*time.Second))
	third := h.codexAutoRefillProviderQuotaStatus(context.Background(), runtimeCfg, startedAt.Add(61*time.Second))

	if meCalls.Load() != 2 {
		t.Fatalf("/me calls = %d, want 2", meCalls.Load())
	}
	if !first.Supported {
		t.Fatalf("first snapshot supported = false, want true")
	}
	if first.Error != "" {
		t.Fatalf("first snapshot error = %q, want empty", first.Error)
	}
	if first.Username != "linuxdo" {
		t.Fatalf("username = %q, want linuxdo", first.Username)
	}
	if first.Email != "linuxdo@example.com" {
		t.Fatalf("email = %q, want linuxdo@example.com", first.Email)
	}
	if first.QuotaRemaining == nil || *first.QuotaRemaining != 11 {
		t.Fatalf("quotaRemaining = %#v, want 11", first.QuotaRemaining)
	}
	if first.QuotaUsed == nil || *first.QuotaUsed != 4 {
		t.Fatalf("quotaUsed = %#v, want 4", first.QuotaUsed)
	}
	if first.QuotaLimit == nil || *first.QuotaLimit != 15 {
		t.Fatalf("quotaLimit = %#v, want 15", first.QuotaLimit)
	}
	if first.ClaimCount == nil || *first.ClaimCount != 4 {
		t.Fatalf("claimCount = %#v, want 4", first.ClaimCount)
	}
	if first.ClaimLimit == nil || *first.ClaimLimit != 15 {
		t.Fatalf("claimLimit = %#v, want 15", first.ClaimLimit)
	}
	if second.FetchedAt.IsZero() || !second.FetchedAt.Equal(first.FetchedAt) {
		t.Fatalf("cached fetchedAt = %v, want %v", second.FetchedAt, first.FetchedAt)
	}
	if third.FetchedAt.Equal(first.FetchedAt) {
		t.Fatalf("third fetchedAt = %v, want refresh after cache ttl", third.FetchedAt)
	}
}

func TestCodexAutoRefillProviderQuotaStatus_UsesSessionEvenWhenRefillModeIsAPIKey(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	var meCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/me" {
			http.NotFound(w, r)
			return
		}
		meCalls.Add(1)
		cookie, err := r.Cookie("token_atlas_session")
		if err != nil {
			t.Fatalf("expected session cookie: %v", err)
		}
		if cookie.Value != "session-token" {
			t.Fatalf("session cookie = %q, want %q", cookie.Value, "session-token")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"quota":{"remaining":9}}`))
	}))
	defer server.Close()

	cfg := &config.Config{
		QuotaExceeded: config.QuotaExceeded{
			CodexAutoRefill: config.CodexAutoRefill{
				ProviderURL:  server.URL,
				AuthMode:     "api-key",
				APIKey:       "api-key-token",
				SessionValue: "session-token",
			},
		},
	}
	h := NewHandlerWithoutConfigFilePath(cfg, coreauth.NewManager(nil, nil, nil))

	snapshot := h.codexAutoRefillProviderQuotaStatus(context.Background(), h.currentCodexAutoRefillRuntimeConfig(), time.Now())

	if meCalls.Load() != 1 {
		t.Fatalf("/me calls = %d, want 1", meCalls.Load())
	}
	if !snapshot.Supported {
		t.Fatalf("supported = false, want true")
	}
	if snapshot.AuthMode != "session" {
		t.Fatalf("authMode = %q, want session", snapshot.AuthMode)
	}
	if snapshot.Error != "" {
		t.Fatalf("error = %q, want empty", snapshot.Error)
	}
	if snapshot.QuotaRemaining == nil || *snapshot.QuotaRemaining != 9 {
		t.Fatalf("quotaRemaining = %#v, want 9", snapshot.QuotaRemaining)
	}
}

func TestCodexAutoRefillProviderQuotaStatus_UnsupportedWithoutSession(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	t.Setenv("TOKEN_ATLAS_API_KEY", "secret-token")

	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		http.NotFound(w, r)
	}))
	defer server.Close()

	cfg := &config.Config{
		QuotaExceeded: config.QuotaExceeded{
			CodexAutoRefill: config.CodexAutoRefill{
				ProviderURL: server.URL,
				AuthMode:    "api-key",
				APIKeyEnv:   "TOKEN_ATLAS_API_KEY",
			},
		},
	}
	h := NewHandlerWithoutConfigFilePath(cfg, coreauth.NewManager(nil, nil, nil))

	snapshot := h.codexAutoRefillProviderQuotaStatus(context.Background(), h.currentCodexAutoRefillRuntimeConfig(), time.Now())

	if snapshot.Supported {
		t.Fatalf("supported = true, want false")
	}
	if !strings.Contains(snapshot.Error, "token_atlas_session") {
		t.Fatalf("error = %q, want token_atlas_session hint", snapshot.Error)
	}
	if requestCount.Load() != 0 {
		t.Fatalf("request count = %d, want 0", requestCount.Load())
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
