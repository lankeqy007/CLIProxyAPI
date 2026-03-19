package management

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestRecordCodexQuotaRefreshResult_RemovesFatalAuths(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	t.Setenv(codexSweepEnabledEnv, "false")

	cases := []struct {
		name   string
		status int
		body   string
	}{
		{
			name:   "unauthorized",
			status: http.StatusUnauthorized,
			body:   `{"error":{"message":"Session expired","code":"invalid_token"}}`,
		},
		{
			name:   "invalid grant",
			status: http.StatusBadRequest,
			body:   `{"error":{"message":"oauth: invalid_grant","code":"invalid_grant"}}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			authDir := t.TempDir()
			authPath := filepath.Join(authDir, "fatal.json")
			if err := os.WriteFile(authPath, []byte(`{"type":"codex"}`), 0o600); err != nil {
				t.Fatalf("write auth file: %v", err)
			}

			manager := coreauth.NewManager(nil, nil, nil)
			auth := newCodexSweepTestAuth("codex-fatal", authPath, nil)
			if _, err := manager.Register(context.Background(), auth); err != nil {
				t.Fatalf("register auth: %v", err)
			}

			store := &memoryAuthStore{items: map[string]*coreauth.Auth{authPath: {ID: authPath}}}
			h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
			h.tokenStore = store

			h.recordCodexQuotaRefreshResult(
				auth,
				http.MethodGet,
				mustParseCodexQuotaURL(t, "https://chatgpt.com/backend-api/wham/usage"),
				tc.status,
				http.Header{},
				[]byte(tc.body),
			)

			if _, err := os.Stat(authPath); !os.IsNotExist(err) {
				t.Fatalf("expected auth file to be removed, stat err: %v", err)
			}
			if _, exists := store.items[authPath]; exists {
				t.Fatalf("expected token record to be removed")
			}
			current, ok := manager.GetByID(auth.ID)
			if !ok {
				t.Fatalf("expected auth to remain addressable after delete")
			}
			if !current.Disabled || current.Status != coreauth.StatusDisabled {
				t.Fatalf("expected auth to be disabled after delete, got %#v", current)
			}
		})
	}
}

func TestRecordCodexQuotaRefreshResult_KeepsRetryableAuths(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	t.Setenv(codexSweepEnabledEnv, "false")

	cases := []struct {
		name         string
		status       int
		header       http.Header
		body         string
		wantStatus   string
		wantQuota    bool
		wantRetrySet bool
	}{
		{
			name:         "rate limited",
			status:       http.StatusTooManyRequests,
			header:       http.Header{"Retry-After": []string{"120"}},
			body:         `{"error":{"message":"quota exceeded","code":"rate_limit"}}`,
			wantStatus:   "quota exhausted",
			wantQuota:    true,
			wantRetrySet: true,
		},
		{
			name:         "upstream bad gateway",
			status:       http.StatusBadGateway,
			body:         `{"error":{"message":"upstream unavailable","code":"bad_gateway"}}`,
			wantStatus:   "transient upstream error",
			wantQuota:    false,
			wantRetrySet: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			authDir := t.TempDir()
			authPath := filepath.Join(authDir, "keep.json")
			if err := os.WriteFile(authPath, []byte(`{"type":"codex"}`), 0o600); err != nil {
				t.Fatalf("write auth file: %v", err)
			}

			manager := coreauth.NewManager(nil, nil, nil)
			auth := newCodexSweepTestAuth("codex-keep", authPath, nil)
			if _, err := manager.Register(context.Background(), auth); err != nil {
				t.Fatalf("register auth: %v", err)
			}

			store := &memoryAuthStore{items: map[string]*coreauth.Auth{authPath: {ID: authPath}}}
			h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
			h.tokenStore = store

			before := time.Now()
			h.recordCodexQuotaRefreshResult(
				auth,
				http.MethodGet,
				mustParseCodexQuotaURL(t, "https://chatgpt.com/backend-api/wham/usage"),
				tc.status,
				tc.header,
				[]byte(tc.body),
			)

			if _, err := os.Stat(authPath); err != nil {
				t.Fatalf("expected auth file to remain, stat err: %v", err)
			}
			current, ok := manager.GetByID(auth.ID)
			if !ok {
				t.Fatalf("expected auth to remain registered")
			}
			if current.Disabled {
				t.Fatalf("expected auth to remain enabled, got %#v", current)
			}
			if current.Status != coreauth.StatusError {
				t.Fatalf("expected auth status error, got %q", current.Status)
			}
			if current.StatusMessage != tc.wantStatus {
				t.Fatalf("status message = %q, want %q", current.StatusMessage, tc.wantStatus)
			}
			if current.Quota.Exceeded != tc.wantQuota {
				t.Fatalf("quota exceeded = %v, want %v", current.Quota.Exceeded, tc.wantQuota)
			}
			if tc.wantRetrySet && !current.NextRetryAfter.After(before) {
				t.Fatalf("expected next retry after to be set, got %v", current.NextRetryAfter)
			}
		})
	}
}

func TestRecordCodexQuotaRefreshResult_ClearsStateOnSuccessAndIgnoresOtherRequests(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	t.Setenv(codexSweepEnabledEnv, "false")

	authDir := t.TempDir()
	authPath := filepath.Join(authDir, "success.json")
	if err := os.WriteFile(authPath, []byte(`{"type":"codex"}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	manager := coreauth.NewManager(nil, nil, nil)
	auth := newCodexSweepTestAuth("codex-success", authPath, func(a *coreauth.Auth) {
		a.Unavailable = true
		a.Status = coreauth.StatusError
		a.StatusMessage = "quota exhausted"
		a.LastError = &coreauth.Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"}
		a.Quota = coreauth.QuotaState{
			Exceeded:      true,
			Reason:        "quota",
			NextRecoverAt: time.Now().Add(10 * time.Minute),
			BackoffLevel:  2,
		}
		a.NextRetryAfter = time.Now().Add(10 * time.Minute)
	})
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	h.recordCodexQuotaRefreshResult(
		auth,
		http.MethodGet,
		mustParseCodexQuotaURL(t, "https://chatgpt.com/backend-api/wham/usage"),
		http.StatusOK,
		http.Header{},
		[]byte(`{"plan_type":"pro"}`),
	)

	current, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatalf("expected auth to remain registered")
	}
	if current.Unavailable || current.Status != coreauth.StatusActive || current.LastError != nil {
		t.Fatalf("expected auth state to be cleared on success, got %#v", current)
	}
	if current.Quota.Exceeded || !current.NextRetryAfter.IsZero() {
		t.Fatalf("expected quota cooldown to be cleared, got %#v", current.Quota)
	}

	geminiAuth := &coreauth.Auth{ID: "gemini", Provider: "gemini", Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), geminiAuth); err != nil {
		t.Fatalf("register gemini auth: %v", err)
	}
	h.recordCodexQuotaRefreshResult(
		geminiAuth,
		http.MethodGet,
		mustParseCodexQuotaURL(t, "https://chatgpt.com/backend-api/wham/usage"),
		http.StatusUnauthorized,
		http.Header{},
		[]byte(`{"error":{"message":"should be ignored"}}`),
	)
	ignored, ok := manager.GetByID(geminiAuth.ID)
	if !ok {
		t.Fatalf("expected gemini auth to remain registered")
	}
	if ignored.Status != coreauth.StatusActive || ignored.LastError != nil || ignored.Unavailable {
		t.Fatalf("expected non-codex auth to remain unchanged, got %#v", ignored)
	}
}

func mustParseCodexQuotaURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url %q: %v", raw, err)
	}
	return parsed
}
