package management

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
)

const (
	codexSweepEnabledEnv      = "CPA_CODEX_SWEEP_ENABLED"
	codexSweepIntervalEnv     = "CPA_CODEX_SWEEP_INTERVAL"
	codexSweepTimeoutEnv      = "CPA_CODEX_SWEEP_TIMEOUT"
	codexSweepInitialDelayEnv = "CPA_CODEX_SWEEP_INITIAL_DELAY"

	codexSweepDefaultInterval     = 15 * time.Minute
	codexSweepDefaultTimeout      = 20 * time.Second
	codexSweepDefaultInitialDelay = 2 * time.Minute
	codexSweepProbeModel          = "gpt-5"
)

var codexSweepProbePayload = []byte(`{"model":"gpt-5","input":[{"role":"user","content":"ping"}]}`)

func (h *Handler) startCodexCredentialSweep() {
	if h == nil || !codexSweepEnabled() {
		return
	}

	interval := codexSweepDurationFromEnv(codexSweepIntervalEnv, codexSweepDefaultInterval)
	timeout := codexSweepDurationFromEnv(codexSweepTimeoutEnv, codexSweepDefaultTimeout)
	initialDelay := codexSweepDurationFromEnv(codexSweepInitialDelayEnv, codexSweepDefaultInitialDelay)
	if interval <= 0 || timeout <= 0 {
		return
	}

	go func() {
		if initialDelay > 0 {
			timer := time.NewTimer(initialDelay)
			defer timer.Stop()
			<-timer.C
		}

		h.runCodexCredentialSweep(timeout)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			h.runCodexCredentialSweep(timeout)
		}
	}()
}

func codexSweepEnabled() bool {
	raw, ok := os.LookupEnv(codexSweepEnabledEnv)
	if !ok {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		log.Warnf("management codex sweep: invalid %s=%q, keeping default enabled state", codexSweepEnabledEnv, raw)
		return true
	}
}

func codexSweepDurationFromEnv(name string, fallback time.Duration) time.Duration {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return fallback
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		log.Warnf("management codex sweep: invalid %s=%q: %v", name, raw, err)
		return fallback
	}
	return parsed
}

func (h *Handler) runCodexCredentialSweep(timeout time.Duration) {
	manager := h.currentAuthManager()
	if manager == nil {
		return
	}
	if _, ok := manager.Executor("codex"); !ok {
		return
	}

	for _, auth := range manager.List() {
		path := h.codexSweepTargetPath(auth)
		if path == "" {
			continue
		}

		probeCtx, cancel := context.WithTimeout(context.Background(), timeout)
		status, err := h.probeCodexAuth(probeCtx, manager, auth)
		cancel()

		if !shouldDeleteCodexAuthStatus(status) {
			if err != nil && status == 0 {
				log.WithError(err).Debugf("management codex sweep: keeping auth %s due to non-http probe error", strings.TrimSpace(auth.ID))
			}
			continue
		}

		if errDelete := h.deleteCodexAuthFile(context.Background(), auth, path); errDelete != nil {
			log.WithError(errDelete).Warnf("management codex sweep: failed to remove auth=%s status=%d file=%s", strings.TrimSpace(auth.ID), status, path)
			continue
		}
		log.Warnf("management codex sweep: removed auth=%s status=%d file=%s", strings.TrimSpace(auth.ID), status, path)
	}
}

func (h *Handler) currentAuthManager() *coreauth.Manager {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.authManager
}

func (h *Handler) currentAuthDir() string {
	if h == nil {
		return ""
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil {
		return ""
	}
	return strings.TrimSpace(h.cfg.AuthDir)
}

func (h *Handler) codexSweepTargetPath(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") || auth.Disabled || isRuntimeOnlyAuth(auth) {
		return ""
	}

	if path := strings.TrimSpace(authAttribute(auth, "path")); path != "" {
		return absolutePath(path)
	}

	source := strings.TrimSpace(authAttribute(auth, "source"))
	if source != "" && !strings.HasPrefix(strings.ToLower(source), "config:") {
		return absolutePath(source)
	}

	fileName := strings.TrimSpace(auth.FileName)
	if fileName == "" {
		return ""
	}
	authDir := h.currentAuthDir()
	if authDir == "" {
		return ""
	}
	return absolutePath(filepath.Join(authDir, filepath.Base(fileName)))
}

func absolutePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if filepath.IsAbs(path) {
		return path
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

func (h *Handler) probeCodexAuth(ctx context.Context, manager *coreauth.Manager, auth *coreauth.Auth) (int, error) {
	if manager == nil || auth == nil {
		return 0, nil
	}
	executor, ok := manager.Executor("codex")
	if !ok || executor == nil {
		return 0, nil
	}

	_, err := executor.Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   codexSweepProbeModel,
		Payload: append([]byte(nil), codexSweepProbePayload...),
		Format:  sdktranslator.FromString("openai-response"),
	}, cliproxyexecutor.Options{
		Alt:          "responses/compact",
		SourceFormat: sdktranslator.FromString("openai-response"),
	})
	if err == nil {
		return http.StatusOK, nil
	}
	if statusProvider, ok := err.(interface{ StatusCode() int }); ok {
		return statusProvider.StatusCode(), err
	}
	return 0, err
}

func shouldDeleteCodexAuthStatus(status int) bool {
	if status <= 0 {
		return false
	}
	if status >= http.StatusOK && status < http.StatusMultipleChoices {
		return false
	}
	if status == http.StatusRequestTimeout || status == http.StatusTooManyRequests {
		return false
	}
	if status >= http.StatusInternalServerError && status < 600 {
		return false
	}
	return true
}

func (h *Handler) deleteCodexAuthFile(ctx context.Context, auth *coreauth.Auth, path string) error {
	if ctx == nil {
		ctx = context.Background()
	}

	var errs []error
	if path == "" {
		errs = append(errs, fmt.Errorf("empty auth file path"))
	} else {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("remove auth file: %w", err))
		}
		if err := h.deleteTokenRecord(ctx, path); err != nil {
			errs = append(errs, fmt.Errorf("delete token record: %w", err))
		}
	}

	if auth != nil && strings.TrimSpace(auth.ID) != "" {
		h.disableAuth(ctx, auth.ID)
	} else if path != "" {
		h.disableAuth(ctx, path)
	}
	return errors.Join(errs...)
}
