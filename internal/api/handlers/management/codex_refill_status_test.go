package management

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestGetCodexAutoRefillStatus_DisabledDoesNotRefreshProviderQuota(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	var providerCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls++
		if r.URL.Path != "/me" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"quota":{"remaining":9,"limit":15,"used":6}}`))
	}))
	defer server.Close()

	cfg := &config.Config{
		AuthDir: t.TempDir(),
		QuotaExceeded: config.QuotaExceeded{
			CodexAutoRefill: config.CodexAutoRefill{
				Enable:               false,
				ProviderURL:          server.URL,
				SessionValue:         "session-token",
				CheckIntervalSeconds: 120,
				TimeoutSeconds:       5,
			},
		},
	}
	h := NewHandlerWithoutConfigFilePath(cfg, coreauth.NewManager(nil, nil, nil))

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/quota-exceeded/codex-auto-refill/status", nil)

	h.GetCodexAutoRefillStatus(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if providerCalls != 0 {
		t.Fatalf("provider /me calls = %d, want 0 when refill is disabled", providerCalls)
	}
}

func TestTriggerCodexAutoRefill_DisabledReturnsConflict(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	cfg := &config.Config{
		AuthDir: t.TempDir(),
		QuotaExceeded: config.QuotaExceeded{
			CodexAutoRefill: config.CodexAutoRefill{
				Enable: false,
			},
		},
	}
	h := NewHandlerWithoutConfigFilePath(cfg, coreauth.NewManager(nil, nil, nil))

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/quota-exceeded/codex-auto-refill/run", nil)

	h.TriggerCodexAutoRefill(ctx)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status code = %d, want %d, body=%s", rec.Code, http.StatusConflict, rec.Body.String())
	}
}

func TestRunCodexAutoRefill_RecordsFinishedAtOnExit(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	t.Setenv("TOKEN_ATLAS_API_KEY", "secret-token")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/claim":
			time.Sleep(120 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"claims":[{"token_id":"701"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/download/701":
			time.Sleep(120 * time.Millisecond)
			w.Header().Set("Content-Disposition", `attachment; filename="timed.json"`)
			_, _ = w.Write([]byte(`{"type":"codex","email":"timed@example.com","account_id":"acc-timed","refresh_token":"r1"}`))
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
				TargetReady:             1,
				MaxClaimPerRun:          1,
				RequireConsecutiveLow:   1,
			},
		},
	}
	h := NewHandlerWithoutConfigFilePath(cfg, coreauth.NewManager(nil, nil, nil))

	h.runCodexAutoRefill()

	status := h.codexAutoRefillStatusSnapshot(
		h.currentCodexAutoRefillRuntimeConfig(),
		h.codexAutoRefillPool(time.Now()),
		codexAutoRefillProviderQuotaSnapshot{},
		time.Now(),
	)
	if status.LastRunStartedAt.IsZero() || status.LastRunFinishedAt.IsZero() {
		t.Fatalf("expected start and finish timestamps, got start=%v finish=%v", status.LastRunStartedAt, status.LastRunFinishedAt)
	}
	if got := status.LastRunFinishedAt.Sub(status.LastRunStartedAt); got < 200*time.Millisecond {
		t.Fatalf("run duration = %s, want at least 200ms", got)
	}
}

func TestPutCodexAutoRefill_PersistsEnableFalse(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &config.Config{
		AuthDir: t.TempDir(),
		QuotaExceeded: config.QuotaExceeded{
			CodexAutoRefill: config.CodexAutoRefill{
				Enable:                  true,
				ProviderURL:             "https://example.com",
				CheckIntervalSeconds:    120,
				MinClaimIntervalSeconds: 900,
				TimeoutSeconds:          20,
				LowWatermark:            10,
				TargetReady:             20,
				MaxClaimPerRun:          5,
				RequireConsecutiveLow:   2,
			},
		},
	}
	if err := os.WriteFile(cfgPath, []byte("quota-exceeded:\n  codex-auto-refill:\n    enable: true\n"), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	h := NewHandler(cfg, cfgPath, coreauth.NewManager(nil, nil, nil))

	payload := map[string]any{
		"enable":                    false,
		"provider-url":              "https://example.com",
		"auth-mode":                 "api-key",
		"check-interval-seconds":    120,
		"min-claim-interval-seconds": 900,
		"timeout-seconds":           20,
		"low-watermark":             10,
		"target-ready":              20,
		"max-claim-per-run":         5,
		"hourly-claim-limit":        0,
		"require-consecutive-low":   2,
		"verify-after-import":       false,
		"priority":                  -10,
		"note":                      "auto-refill",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPut, "/v0/management/quota-exceeded/codex-auto-refill", io.NopCloser(bytes.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req

	h.PutCodexAutoRefill(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if h.cfg.QuotaExceeded.CodexAutoRefill.Enable {
		t.Fatal("expected in-memory enable=false after PUT")
	}

	reloaded, err := config.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if reloaded.QuotaExceeded.CodexAutoRefill.Enable {
		t.Fatal("expected persisted enable=false after PUT")
	}
}
