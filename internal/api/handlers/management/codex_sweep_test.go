package management

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestShouldDeleteCodexAuthRuntimeError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		err  *coreauth.Error
		want bool
	}{
		{err: nil, want: false},
		{err: &coreauth.Error{HTTPStatus: http.StatusUnauthorized}, want: true},
		{err: &coreauth.Error{HTTPStatus: http.StatusPaymentRequired}, want: true},
		{err: &coreauth.Error{HTTPStatus: http.StatusForbidden}, want: true},
		{err: &coreauth.Error{HTTPStatus: http.StatusBadRequest, Message: "invalid_request_error"}, want: false},
		{err: &coreauth.Error{HTTPStatus: http.StatusBadRequest, Message: "oauth: invalid_grant"}, want: true},
		{err: &coreauth.Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"}, want: false},
		{err: &coreauth.Error{HTTPStatus: http.StatusBadGateway, Message: "upstream"}, want: false},
	}

	for _, tc := range cases {
		if got := shouldDeleteCodexAuthRuntimeError(tc.err); got != tc.want {
			t.Fatalf("shouldDeleteCodexAuthRuntimeError(%#v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestRunCodexCredentialSweep_RemovesOnlyUnavailableFatalFileBackedAuths(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	t.Setenv(codexSweepEnabledEnv, "false")

	authDir := t.TempDir()
	removePath := filepath.Join(authDir, "remove.json")
	keepQuotaPath := filepath.Join(authDir, "keep-quota.json")
	keepTransientPath := filepath.Join(authDir, "keep-transient.json")
	keepRequestPath := filepath.Join(authDir, "keep-request.json")
	keepPartialPath := filepath.Join(authDir, "keep-partial.json")
	for _, item := range []string{removePath, keepQuotaPath, keepTransientPath, keepRequestPath, keepPartialPath} {
		if err := os.WriteFile(item, []byte(`{"type":"codex"}`), 0o600); err != nil {
			t.Fatalf("failed to write auth file %s: %v", item, err)
		}
	}

	manager := coreauth.NewManager(nil, nil, nil)
	now := time.Now()
	for _, auth := range []*coreauth.Auth{
		newCodexSweepTestAuth("codex-remove", removePath, func(a *coreauth.Auth) {
			a.Unavailable = true
			a.LastError = &coreauth.Error{HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"}
			a.NextRetryAfter = now.Add(30 * time.Minute)
		}),
		newCodexSweepTestAuth("codex-keep-quota", keepQuotaPath, func(a *coreauth.Auth) {
			a.Unavailable = true
			a.LastError = &coreauth.Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"}
			a.NextRetryAfter = now.Add(5 * time.Minute)
			a.Quota = coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: now.Add(5 * time.Minute)}
		}),
		newCodexSweepTestAuth("codex-keep-transient", keepTransientPath, func(a *coreauth.Auth) {
			a.Unavailable = true
			a.LastError = &coreauth.Error{HTTPStatus: http.StatusBadGateway, Message: "upstream"}
			a.NextRetryAfter = now.Add(1 * time.Minute)
		}),
		newCodexSweepTestAuth("codex-keep-request", keepRequestPath, func(a *coreauth.Auth) {
			a.Unavailable = true
			a.LastError = &coreauth.Error{HTTPStatus: http.StatusBadRequest, Message: "invalid_request_error"}
		}),
		newCodexSweepTestAuth("codex-keep-partial", keepPartialPath, func(a *coreauth.Auth) {
			a.Unavailable = false
			a.LastError = &coreauth.Error{HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"}
		}),
		{
			ID:       "codex-config",
			Provider: "codex",
			Status:   coreauth.StatusActive,
			Attributes: map[string]string{
				"source": "config:codex[abc123]",
			},
		},
	} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("failed to register auth %s: %v", auth.ID, err)
		}
	}

	store := &memoryAuthStore{
		items: map[string]*coreauth.Auth{
			removePath:        {ID: removePath},
			keepQuotaPath:     {ID: keepQuotaPath},
			keepTransientPath: {ID: keepTransientPath},
			keepRequestPath:   {ID: keepRequestPath},
			keepPartialPath:   {ID: keepPartialPath},
		},
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = store
	h.runCodexCredentialSweep()

	if _, err := os.Stat(removePath); !os.IsNotExist(err) {
		t.Fatalf("expected invalid auth file to be removed, stat err: %v", err)
	}
	if _, exists := store.items[removePath]; exists {
		t.Fatalf("expected invalid auth token store record to be removed")
	}
	if auth, ok := manager.GetByID("codex-remove"); !ok || !auth.Disabled || auth.Status != coreauth.StatusDisabled {
		t.Fatalf("expected invalid auth to be disabled, got %#v", auth)
	}

	if _, err := os.Stat(keepQuotaPath); err != nil {
		t.Fatalf("expected 429 auth file to remain, stat err: %v", err)
	}
	if _, err := os.Stat(keepTransientPath); err != nil {
		t.Fatalf("expected transient auth file to remain, stat err: %v", err)
	}
	if _, err := os.Stat(keepRequestPath); err != nil {
		t.Fatalf("expected request-error auth file to remain, stat err: %v", err)
	}
	if _, err := os.Stat(keepPartialPath); err != nil {
		t.Fatalf("expected partially available auth file to remain, stat err: %v", err)
	}
	if auth, ok := manager.GetByID("codex-keep-quota"); !ok || auth.Disabled {
		t.Fatalf("expected 429 auth to remain enabled, got %#v", auth)
	}
	if auth, ok := manager.GetByID("codex-keep-transient"); !ok || auth.Disabled {
		t.Fatalf("expected transient auth to remain enabled, got %#v", auth)
	}
	if auth, ok := manager.GetByID("codex-keep-request"); !ok || auth.Disabled {
		t.Fatalf("expected request-error auth to remain enabled, got %#v", auth)
	}
	if auth, ok := manager.GetByID("codex-keep-partial"); !ok || auth.Disabled {
		t.Fatalf("expected partially available auth to remain enabled, got %#v", auth)
	}
	if auth, ok := manager.GetByID("codex-config"); !ok || auth.Disabled {
		t.Fatalf("expected config-backed auth to be skipped, got %#v", auth)
	}
}

func newCodexSweepTestAuth(id string, path string, mutate func(*coreauth.Auth)) *coreauth.Auth {
	auth := &coreauth.Auth{
		ID:       id,
		Provider: "codex",
		FileName: filepath.Base(path),
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path":   path,
			"source": path,
		},
	}
	if mutate != nil {
		mutate(auth)
	}
	return auth
}
