package management

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestShouldDeleteCodexAuthStatus(t *testing.T) {
	t.Parallel()

	cases := []struct {
		status int
		want   bool
	}{
		{status: 0, want: false},
		{status: http.StatusOK, want: false},
		{status: http.StatusNoContent, want: false},
		{status: http.StatusBadRequest, want: true},
		{status: http.StatusUnauthorized, want: true},
		{status: http.StatusPaymentRequired, want: true},
		{status: http.StatusForbidden, want: true},
		{status: http.StatusNotFound, want: true},
		{status: http.StatusRequestTimeout, want: false},
		{status: http.StatusTooManyRequests, want: false},
		{status: http.StatusBadGateway, want: false},
	}

	for _, tc := range cases {
		if got := shouldDeleteCodexAuthStatus(tc.status); got != tc.want {
			t.Fatalf("shouldDeleteCodexAuthStatus(%d) = %v, want %v", tc.status, got, tc.want)
		}
	}
}

func TestRunCodexCredentialSweep_RemovesOnlyInvalidFileBackedAuths(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	t.Setenv(codexSweepEnabledEnv, "false")

	authDir := t.TempDir()
	removePath := filepath.Join(authDir, "remove.json")
	keepQuotaPath := filepath.Join(authDir, "keep-quota.json")
	keepNetworkPath := filepath.Join(authDir, "keep-network.json")
	for _, item := range []string{removePath, keepQuotaPath, keepNetworkPath} {
		if err := os.WriteFile(item, []byte(`{"type":"codex"}`), 0o600); err != nil {
			t.Fatalf("failed to write auth file %s: %v", item, err)
		}
	}

	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(&codexSweepTestExecutor{
		results: map[string]error{
			"codex-remove":     &coreauth.Error{HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"},
			"codex-keep-quota": &coreauth.Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"},
			"codex-keep-net":   errors.New("dial tcp timeout"),
		},
	})

	for _, auth := range []*coreauth.Auth{
		newCodexSweepTestAuth("codex-remove", removePath),
		newCodexSweepTestAuth("codex-keep-quota", keepQuotaPath),
		newCodexSweepTestAuth("codex-keep-net", keepNetworkPath),
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
			removePath:      &coreauth.Auth{ID: removePath},
			keepQuotaPath:   &coreauth.Auth{ID: keepQuotaPath},
			keepNetworkPath: &coreauth.Auth{ID: keepNetworkPath},
		},
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = store
	h.runCodexCredentialSweep(time.Second)

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
	if _, err := os.Stat(keepNetworkPath); err != nil {
		t.Fatalf("expected network-error auth file to remain, stat err: %v", err)
	}
	if auth, ok := manager.GetByID("codex-keep-quota"); !ok || auth.Disabled {
		t.Fatalf("expected 429 auth to remain enabled, got %#v", auth)
	}
	if auth, ok := manager.GetByID("codex-keep-net"); !ok || auth.Disabled {
		t.Fatalf("expected network-error auth to remain enabled, got %#v", auth)
	}
	if auth, ok := manager.GetByID("codex-config"); !ok || auth.Disabled {
		t.Fatalf("expected config-backed auth to be skipped, got %#v", auth)
	}
}

func newCodexSweepTestAuth(id string, path string) *coreauth.Auth {
	return &coreauth.Auth{
		ID:       id,
		Provider: "codex",
		FileName: filepath.Base(path),
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path":   path,
			"source": path,
		},
	}
}

type codexSweepTestExecutor struct {
	results map[string]error
}

func (e *codexSweepTestExecutor) Identifier() string { return "codex" }

func (e *codexSweepTestExecutor) Execute(_ context.Context, auth *coreauth.Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if e == nil || auth == nil {
		return cliproxyexecutor.Response{}, nil
	}
	if err := e.results[auth.ID]; err != nil {
		return cliproxyexecutor.Response{}, err
	}
	return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *codexSweepTestExecutor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, errors.New("not implemented")
}

func (e *codexSweepTestExecutor) Refresh(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
	return nil, errors.New("not implemented")
}

func (e *codexSweepTestExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("not implemented")
}

func (e *codexSweepTestExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}
