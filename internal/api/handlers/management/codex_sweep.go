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
	log "github.com/sirupsen/logrus"
)

const (
	codexSweepEnabledEnv      = "CPA_CODEX_SWEEP_ENABLED"
	codexSweepIntervalEnv     = "CPA_CODEX_SWEEP_INTERVAL"
	codexSweepInitialDelayEnv = "CPA_CODEX_SWEEP_INITIAL_DELAY"

	codexSweepDefaultInterval     = 15 * time.Minute
	codexSweepDefaultInitialDelay = 2 * time.Minute
)

func (h *Handler) startCodexCredentialSweep() {
	if h == nil || !codexSweepEnabled() {
		return
	}

	interval := codexSweepDurationFromEnv(codexSweepIntervalEnv, codexSweepDefaultInterval)
	initialDelay := codexSweepDurationFromEnv(codexSweepInitialDelayEnv, codexSweepDefaultInitialDelay)
	if interval <= 0 {
		return
	}

	go func() {
		if initialDelay > 0 {
			timer := time.NewTimer(initialDelay)
			defer timer.Stop()
			<-timer.C
		}

		h.runCodexCredentialSweep()

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			h.runCodexCredentialSweep()
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

type codexSweepDecision struct {
	status int
	reason string
}

func (h *Handler) runCodexCredentialSweep() {
	manager := h.currentAuthManager()
	if manager == nil {
		return
	}
	now := time.Now()
	for _, auth := range manager.List() {
		path := h.codexSweepTargetPath(auth)
		if path == "" {
			continue
		}

		decision, shouldDelete := codexSweepDeleteDecision(auth, now)
		if !shouldDelete {
			continue
		}

		if errDelete := h.deleteCodexAuthFile(context.Background(), auth, path); errDelete != nil {
			log.WithError(errDelete).Warnf("management codex sweep: failed to remove auth=%s status=%d reason=%s file=%s", strings.TrimSpace(auth.ID), decision.status, decision.reason, path)
			continue
		}
		log.Warnf("management codex sweep: removed auth=%s status=%d reason=%s file=%s", strings.TrimSpace(auth.ID), decision.status, decision.reason, path)
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

func codexSweepDeleteDecision(auth *coreauth.Auth, now time.Time) (codexSweepDecision, bool) {
	if auth == nil {
		return codexSweepDecision{}, false
	}
	if auth.Quota.Exceeded && !auth.Quota.NextRecoverAt.IsZero() && auth.Quota.NextRecoverAt.After(now) {
		return codexSweepDecision{}, false
	}
	if !auth.Unavailable || auth.LastError == nil {
		return codexSweepDecision{}, false
	}
	if !shouldDeleteCodexAuthRuntimeError(auth.LastError) {
		return codexSweepDecision{}, false
	}
	status := auth.LastError.StatusCode()
	return codexSweepDecision{
		status: status,
		reason: codexSweepErrorReason(auth.LastError),
	}, true
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

func shouldDeleteCodexAuthRuntimeError(err *coreauth.Error) bool {
	if err == nil {
		return false
	}
	switch err.StatusCode() {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden:
		return true
	case http.StatusBadRequest:
		return codexAuthErrorLooksInvalid(err)
	default:
		return false
	}
}

func codexAuthErrorLooksInvalid(err *coreauth.Error) bool {
	if err == nil {
		return false
	}
	raw := strings.ToLower(strings.TrimSpace(strings.Join([]string{err.Code, err.Message}, " ")))
	if raw == "" {
		return false
	}
	if strings.Contains(raw, "invalid_request_error") || strings.Contains(raw, "unsupported_parameter") {
		return false
	}
	for _, needle := range []string{
		"invalid_grant",
		"invalid_token",
		"expired_token",
		"token expired",
		"invalid refresh",
		"refresh token",
		"session expired",
		"account deactivated",
	} {
		if strings.Contains(raw, needle) {
			return true
		}
	}
	return false
}

func codexSweepErrorReason(err *coreauth.Error) string {
	if err == nil {
		return ""
	}
	message := strings.TrimSpace(err.Message)
	if message == "" {
		message = strings.TrimSpace(err.Code)
	}
	if message == "" {
		return "runtime error"
	}
	return message
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
