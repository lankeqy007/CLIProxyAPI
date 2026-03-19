package management

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	codexauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const codexAutoRefillWorkerFallbackInterval = 2 * time.Minute
const codexAutoRefillDefaultAPIKeyEnv = "TOKEN_ATLAS_API_KEY"
const codexAutoRefillDefaultSessionEnv = "TOKEN_ATLAS_SESSION"

type codexAutoRefillRuntimeConfig struct {
	Enable                bool
	ProviderURL           string
	AuthMode              string
	APIKey                string
	APIKeyEnv             string
	SessionValue          string
	SessionEnv            string
	CheckInterval         time.Duration
	MinClaimInterval      time.Duration
	Timeout               time.Duration
	LowWatermark          int
	TargetReady           int
	MaxClaimPerRun        int
	RequireConsecutiveLow int
	VerifyAfterImport     bool
	Priority              int
	Note                  string
	AuthDir               string
	SDKConfig             config.SDKConfig
}

type codexAutoRefillPoolSnapshot struct {
	ReadyCount       int
	CoolingCount     int
	UnavailableCount int
	DisabledCount    int
	TotalCount       int
}

type codexAutoRefillDownloadedFile struct {
	Name string
	Data []byte
}

type codexAutoRefillIdentity struct {
	Email     string
	AccountID string
}

func (h *Handler) startCodexAutoRefill() {
	if h == nil {
		return
	}
	go func() {
		for {
			runtimeCfg := h.currentCodexAutoRefillRuntimeConfig()
			interval := runtimeCfg.CheckInterval
			if interval <= 0 {
				interval = codexAutoRefillWorkerFallbackInterval
			}
			timer := time.NewTimer(interval)
			<-timer.C
			h.runCodexAutoRefill()
		}
	}()
}

func (h *Handler) currentCodexAutoRefillRuntimeConfig() codexAutoRefillRuntimeConfig {
	if h == nil {
		return codexAutoRefillRuntimeConfig{
			CheckInterval: codexAutoRefillWorkerFallbackInterval,
			Timeout:       20 * time.Second,
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil {
		return codexAutoRefillRuntimeConfig{
			CheckInterval: codexAutoRefillWorkerFallbackInterval,
			Timeout:       20 * time.Second,
		}
	}
	refill := h.cfg.QuotaExceeded.CodexAutoRefill
	if strings.TrimSpace(refill.ProviderURL) == "" {
		refill.ProviderURL = "https://gptfreetoken.pony.indevs.in"
	}
	if strings.TrimSpace(refill.AuthMode) == "" {
		refill.AuthMode = "api-key"
	}
	if refill.CheckIntervalSeconds <= 0 {
		refill.CheckIntervalSeconds = 120
	}
	if refill.MinClaimIntervalSeconds <= 0 {
		refill.MinClaimIntervalSeconds = 900
	}
	if refill.TimeoutSeconds <= 0 {
		refill.TimeoutSeconds = 20
	}
	if refill.LowWatermark < 0 {
		refill.LowWatermark = 0
	}
	if refill.TargetReady <= 0 {
		refill.TargetReady = 3
	}
	if refill.TargetReady < refill.LowWatermark {
		refill.TargetReady = refill.LowWatermark
	}
	if refill.MaxClaimPerRun <= 0 {
		refill.MaxClaimPerRun = 2
	}
	if refill.RequireConsecutiveLow <= 0 {
		refill.RequireConsecutiveLow = 2
	}
	if strings.TrimSpace(refill.Note) == "" {
		refill.Note = "auto-refill"
	}
	if refill.Priority == 0 {
		refill.Priority = -10
	}
	return codexAutoRefillRuntimeConfig{
		Enable:                refill.Enable,
		ProviderURL:           strings.TrimSpace(refill.ProviderURL),
		AuthMode:              strings.ToLower(strings.TrimSpace(refill.AuthMode)),
		APIKey:                strings.TrimSpace(refill.APIKey),
		APIKeyEnv:             strings.TrimSpace(refill.APIKeyEnv),
		SessionValue:          strings.TrimSpace(refill.SessionValue),
		SessionEnv:            strings.TrimSpace(refill.SessionEnv),
		CheckInterval:         time.Duration(refill.CheckIntervalSeconds) * time.Second,
		MinClaimInterval:      time.Duration(refill.MinClaimIntervalSeconds) * time.Second,
		Timeout:               time.Duration(refill.TimeoutSeconds) * time.Second,
		LowWatermark:          refill.LowWatermark,
		TargetReady:           refill.TargetReady,
		MaxClaimPerRun:        refill.MaxClaimPerRun,
		RequireConsecutiveLow: refill.RequireConsecutiveLow,
		VerifyAfterImport:     refill.VerifyAfterImport,
		Priority:              refill.Priority,
		Note:                  strings.TrimSpace(refill.Note),
		AuthDir:               strings.TrimSpace(h.cfg.AuthDir),
		SDKConfig:             h.cfg.SDKConfig,
	}
}

func (h *Handler) runCodexAutoRefill() {
	runtimeCfg := h.currentCodexAutoRefillRuntimeConfig()
	if !runtimeCfg.Enable || runtimeCfg.AuthDir == "" {
		h.resetCodexAutoRefillLow()
		return
	}
	manager := h.currentAuthManager()
	if manager == nil {
		return
	}
	if !h.beginCodexAutoRefillRun() {
		return
	}
	defer h.endCodexAutoRefillRun()

	now := time.Now()
	pool := h.codexAutoRefillPool(now)
	if pool.ReadyCount >= runtimeCfg.LowWatermark {
		h.resetCodexAutoRefillLow()
		return
	}

	lowCount := h.incrementCodexAutoRefillLow()
	if lowCount < runtimeCfg.RequireConsecutiveLow {
		log.Debugf("management codex auto-refill: ready=%d below low-watermark=%d (%d/%d)", pool.ReadyCount, runtimeCfg.LowWatermark, lowCount, runtimeCfg.RequireConsecutiveLow)
		return
	}
	if !h.codexAutoRefillCanClaim(now, runtimeCfg.MinClaimInterval) {
		return
	}

	missing := runtimeCfg.TargetReady - pool.ReadyCount
	if missing <= 0 {
		missing = runtimeCfg.MaxClaimPerRun
	}
	if missing > runtimeCfg.MaxClaimPerRun {
		missing = runtimeCfg.MaxClaimPerRun
	}
	if missing <= 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), runtimeCfg.Timeout)
	imported, err := h.codexAutoRefillClaimAndImport(ctx, manager, runtimeCfg, missing)
	cancel()
	if err != nil {
		log.WithError(err).Warnf("management codex auto-refill: claim/import failed ready=%d cooling=%d unavailable=%d total=%d", pool.ReadyCount, pool.CoolingCount, pool.UnavailableCount, pool.TotalCount)
		return
	}

	h.markCodexAutoRefillClaimed(now)
	log.Infof("management codex auto-refill: imported=%d requested=%d ready=%d cooling=%d unavailable=%d total=%d", imported, missing, pool.ReadyCount, pool.CoolingCount, pool.UnavailableCount, pool.TotalCount)
}

func (h *Handler) beginCodexAutoRefillRun() bool {
	if h == nil {
		return false
	}
	h.codexAutoRefillMu.Lock()
	defer h.codexAutoRefillMu.Unlock()
	if h.codexAutoRefill.running {
		return false
	}
	h.codexAutoRefill.running = true
	return true
}

func (h *Handler) endCodexAutoRefillRun() {
	if h == nil {
		return
	}
	h.codexAutoRefillMu.Lock()
	h.codexAutoRefill.running = false
	h.codexAutoRefillMu.Unlock()
}

func (h *Handler) incrementCodexAutoRefillLow() int {
	if h == nil {
		return 0
	}
	h.codexAutoRefillMu.Lock()
	defer h.codexAutoRefillMu.Unlock()
	h.codexAutoRefill.consecutiveLow++
	return h.codexAutoRefill.consecutiveLow
}

func (h *Handler) resetCodexAutoRefillLow() {
	if h == nil {
		return
	}
	h.codexAutoRefillMu.Lock()
	h.codexAutoRefill.consecutiveLow = 0
	h.codexAutoRefillMu.Unlock()
}

func (h *Handler) codexAutoRefillCanClaim(now time.Time, minInterval time.Duration) bool {
	if h == nil {
		return false
	}
	h.codexAutoRefillMu.Lock()
	defer h.codexAutoRefillMu.Unlock()
	if minInterval <= 0 || h.codexAutoRefill.lastClaimAt.IsZero() {
		return true
	}
	return now.Sub(h.codexAutoRefill.lastClaimAt) >= minInterval
}

func (h *Handler) markCodexAutoRefillClaimed(now time.Time) {
	if h == nil {
		return
	}
	h.codexAutoRefillMu.Lock()
	h.codexAutoRefill.lastClaimAt = now
	h.codexAutoRefill.consecutiveLow = 0
	h.codexAutoRefillMu.Unlock()
}

func (h *Handler) codexAutoRefillPool(now time.Time) codexAutoRefillPoolSnapshot {
	manager := h.currentAuthManager()
	if manager == nil {
		return codexAutoRefillPoolSnapshot{}
	}
	snapshot := codexAutoRefillPoolSnapshot{}
	for _, auth := range manager.List() {
		path := h.codexSweepTargetPath(auth)
		if path == "" {
			continue
		}
		if _, err := os.Stat(path); err != nil {
			continue
		}
		snapshot.TotalCount++
		if auth.Disabled || auth.Status == coreauth.StatusDisabled {
			snapshot.DisabledCount++
			continue
		}
		if auth.Quota.Exceeded && !auth.Quota.NextRecoverAt.IsZero() && auth.Quota.NextRecoverAt.After(now) {
			snapshot.CoolingCount++
			continue
		}
		if auth.Unavailable && auth.NextRetryAfter.After(now) {
			snapshot.UnavailableCount++
			continue
		}
		snapshot.ReadyCount++
	}
	return snapshot
}

func (h *Handler) codexAutoRefillClaimAndImport(ctx context.Context, manager *coreauth.Manager, runtimeCfg codexAutoRefillRuntimeConfig, count int) (int, error) {
	if count <= 0 {
		return 0, nil
	}
	if err := os.MkdirAll(runtimeCfg.AuthDir, 0o700); err != nil {
		return 0, fmt.Errorf("create auth dir: %w", err)
	}

	var (
		files []codexAutoRefillDownloadedFile
		err   error
	)
	switch runtimeCfg.AuthMode {
	case "", "api-key":
		files, err = h.codexAutoRefillClaimWithAPIKey(ctx, runtimeCfg, count)
	case "session":
		files, err = h.codexAutoRefillClaimWithSession(ctx, runtimeCfg, count)
	default:
		return 0, fmt.Errorf("unsupported auth mode %q", runtimeCfg.AuthMode)
	}
	if err != nil {
		return 0, err
	}
	if len(files) == 0 {
		return 0, fmt.Errorf("refill provider returned no auth files")
	}

	imported := 0
	for _, file := range files {
		auth, importedOne, errImport := h.importCodexAutoRefillFile(ctx, manager, runtimeCfg, file.Name, file.Data)
		if errImport != nil {
			log.WithError(errImport).Warnf("management codex auto-refill: failed to import %s", strings.TrimSpace(file.Name))
			continue
		}
		if !importedOne {
			continue
		}
		imported++
		if runtimeCfg.VerifyAfterImport && auth != nil {
			if decision, shouldDelete := codexSweepDeleteDecision(auth, time.Now()); shouldDelete {
				_ = h.deleteCodexAuthFile(context.Background(), auth, h.codexSweepTargetPath(auth))
				log.Warnf("management codex auto-refill: dropped imported auth=%s status=%d reason=%s", strings.TrimSpace(auth.ID), decision.status, decision.reason)
				imported--
			}
		}
	}
	return imported, nil
}

func resolveCodexAutoRefillSecret(directValue string, envName string, fallbackEnvName string, label string, configKey string) (string, error) {
	if value := strings.TrimSpace(directValue); value != "" {
		return value, nil
	}

	targetEnvName := strings.TrimSpace(envName)
	if targetEnvName == "" {
		targetEnvName = fallbackEnvName
	}
	if value := strings.TrimSpace(os.Getenv(targetEnvName)); value != "" {
		return value, nil
	}

	return "", fmt.Errorf("missing %s: set %s or env %s", label, configKey, targetEnvName)
}

func (h *Handler) codexAutoRefillClaimWithAPIKey(ctx context.Context, runtimeCfg codexAutoRefillRuntimeConfig, count int) ([]codexAutoRefillDownloadedFile, error) {
	apiKey, err := resolveCodexAutoRefillSecret(
		runtimeCfg.APIKey,
		runtimeCfg.APIKeyEnv,
		codexAutoRefillDefaultAPIKeyEnv,
		"codex auto-refill api key",
		"quota-exceeded.codex-auto-refill.api-key",
	)
	if err != nil {
		return nil, err
	}

	requestBody, _ := json.Marshal(map[string]int{"count": count})
	responseBody, _, err := h.codexAutoRefillRequest(ctx, runtimeCfg, http.MethodPost, "/api/claim", requestBody, func(req *http.Request) {
		req.Header.Set("X-API-Key", apiKey)
	})
	if err != nil {
		return nil, err
	}

	tokenIDs := extractTokenIDs(responseBody)
	if len(tokenIDs) == 0 {
		return nil, fmt.Errorf("claim response contained no token ids")
	}

	files := make([]codexAutoRefillDownloadedFile, 0, len(tokenIDs))
	for _, tokenID := range tokenIDs {
		body, headers, errDownload := h.codexAutoRefillRequest(ctx, runtimeCfg, http.MethodGet, "/api/download/"+tokenID, nil, func(req *http.Request) {
			req.Header.Set("X-API-Key", apiKey)
		})
		if errDownload != nil {
			return nil, errDownload
		}
		name := fileNameFromContentDisposition(headers.Get("Content-Disposition"))
		if name == "" {
			name = "token-" + tokenID + ".json"
		}
		files = append(files, codexAutoRefillDownloadedFile{Name: name, Data: body})
	}
	return files, nil
}

func (h *Handler) codexAutoRefillClaimWithSession(ctx context.Context, runtimeCfg codexAutoRefillRuntimeConfig, count int) ([]codexAutoRefillDownloadedFile, error) {
	session, err := resolveCodexAutoRefillSecret(
		runtimeCfg.SessionValue,
		runtimeCfg.SessionEnv,
		codexAutoRefillDefaultSessionEnv,
		"codex auto-refill session",
		"quota-exceeded.codex-auto-refill.session-value",
	)
	if err != nil {
		return nil, err
	}

	requestBody, _ := json.Marshal(map[string]int{"count": count})
	if _, _, err := h.codexAutoRefillRequest(ctx, runtimeCfg, http.MethodPost, "/me/claim", requestBody, func(req *http.Request) {
		req.AddCookie(&http.Cookie{Name: "token_atlas_session", Value: session})
	}); err != nil {
		return nil, err
	}

	archiveBody, _, err := h.codexAutoRefillRequest(ctx, runtimeCfg, http.MethodGet, "/me/claims/archive", nil, func(req *http.Request) {
		req.AddCookie(&http.Cookie{Name: "token_atlas_session", Value: session})
	})
	if err != nil {
		return nil, err
	}
	return unzipCodexAutoRefillArchive(archiveBody)
}

func (h *Handler) codexAutoRefillRequest(ctx context.Context, runtimeCfg codexAutoRefillRuntimeConfig, method string, path string, body []byte, decorate func(*http.Request)) ([]byte, http.Header, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(runtimeCfg.ProviderURL), "/")
	if baseURL == "" {
		return nil, nil, fmt.Errorf("provider url is empty")
	}
	targetURL := baseURL + path
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, targetURL, reader)
	if err != nil {
		return nil, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if decorate != nil {
		decorate(req)
	}

	client := util.SetProxy(&runtimeCfg.SDKConfig, &http.Client{Timeout: runtimeCfg.Timeout})
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, resp.Header.Clone(), fmt.Errorf("refill request %s %s failed with status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	return responseBody, resp.Header.Clone(), nil
}

func unzipCodexAutoRefillArchive(raw []byte) ([]codexAutoRefillDownloadedFile, error) {
	reader, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, fmt.Errorf("open refill archive: %w", err)
	}
	files := make([]codexAutoRefillDownloadedFile, 0, len(reader.File))
	for _, file := range reader.File {
		if file == nil || file.FileInfo().IsDir() {
			continue
		}
		name := filepath.Base(strings.TrimSpace(file.Name))
		if !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		rc, errOpen := file.Open()
		if errOpen != nil {
			return nil, fmt.Errorf("open archive entry %s: %w", name, errOpen)
		}
		data, errRead := io.ReadAll(rc)
		_ = rc.Close()
		if errRead != nil {
			return nil, fmt.Errorf("read archive entry %s: %w", name, errRead)
		}
		files = append(files, codexAutoRefillDownloadedFile{Name: name, Data: data})
	}
	return files, nil
}

func extractTokenIDs(raw []byte) []string {
	var payload any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}
	seen := make(map[string]struct{})
	ids := make([]string, 0)
	var walk func(any)
	add := func(rawID any) {
		switch typed := rawID.(type) {
		case string:
			id := strings.TrimSpace(typed)
			if id == "" {
				return
			}
			if _, exists := seen[id]; exists {
				return
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		case float64:
			id := strings.TrimSpace(fmt.Sprintf("%.0f", typed))
			if id == "" {
				return
			}
			if _, exists := seen[id]; exists {
				return
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	walk = func(node any) {
		switch typed := node.(type) {
		case map[string]any:
			if val, ok := typed["token_id"]; ok {
				add(val)
			}
			if val, ok := typed["tokenId"]; ok {
				add(val)
			}
			for _, child := range typed {
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(payload)
	return ids
}

func fileNameFromContentDisposition(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(raw)
	if err != nil {
		return ""
	}
	name := filepath.Base(strings.TrimSpace(params["filename"]))
	if name == "" {
		name = filepath.Base(strings.TrimSpace(params["filename*"]))
	}
	return name
}

func (h *Handler) importCodexAutoRefillFile(ctx context.Context, manager *coreauth.Manager, runtimeCfg codexAutoRefillRuntimeConfig, name string, data []byte) (*coreauth.Auth, bool, error) {
	normalized, identity, err := normalizeCodexAutoRefillJSON(data, runtimeCfg)
	if err != nil {
		return nil, false, err
	}
	if duplicate := h.findExistingCodexAutoRefillAuth(manager, identity); duplicate != nil {
		return duplicate, false, nil
	}

	fileName := codexAutoRefillFileName(name, identity)
	dst, err := uniqueCodexAutoRefillPath(runtimeCfg.AuthDir, fileName)
	if err != nil {
		return nil, false, err
	}
	if errWrite := os.WriteFile(dst, normalized, 0o600); errWrite != nil {
		return nil, false, fmt.Errorf("write auth file: %w", errWrite)
	}
	if errReg := h.registerAuthFromFile(ctx, dst, normalized); errReg != nil {
		_ = os.Remove(dst)
		return nil, false, errReg
	}

	if auth, ok := manager.GetByID(h.authIDForPath(dst)); ok {
		return auth, true, nil
	}
	if auth := h.findAuthForDelete(filepath.Base(dst)); auth != nil {
		return auth, true, nil
	}
	return nil, true, nil
}

func normalizeCodexAutoRefillJSON(data []byte, runtimeCfg codexAutoRefillRuntimeConfig) ([]byte, codexAutoRefillIdentity, error) {
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, codexAutoRefillIdentity{}, fmt.Errorf("invalid refill auth json: %w", err)
	}
	if len(meta) == 0 {
		return nil, codexAutoRefillIdentity{}, fmt.Errorf("empty refill auth json")
	}

	typ, _ := meta["type"].(string)
	typ = strings.TrimSpace(typ)
	if typ == "" {
		meta["type"] = "codex"
	} else if !strings.EqualFold(typ, "codex") {
		return nil, codexAutoRefillIdentity{}, fmt.Errorf("unsupported auth type %q", typ)
	}

	identity := codexAutoRefillIdentityFromMetadata(meta)
	if identity.Email != "" {
		meta["email"] = identity.Email
	}
	if identity.AccountID != "" {
		meta["account_id"] = identity.AccountID
	}
	if runtimeCfg.Note != "" {
		meta["note"] = runtimeCfg.Note
	}
	meta["priority"] = runtimeCfg.Priority
	meta["auto_refill"] = true
	meta["auto_refill_provider"] = "pony"
	meta["claimed_at"] = time.Now().UTC().Format(time.RFC3339)

	normalized, err := json.Marshal(meta)
	if err != nil {
		return nil, codexAutoRefillIdentity{}, fmt.Errorf("marshal refill auth json: %w", err)
	}
	return normalized, identity, nil
}

func (h *Handler) findExistingCodexAutoRefillAuth(manager *coreauth.Manager, identity codexAutoRefillIdentity) *coreauth.Auth {
	if manager == nil || identity.empty() {
		return nil
	}
	for _, auth := range manager.List() {
		path := h.codexSweepTargetPath(auth)
		if path == "" {
			continue
		}
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if codexAutoRefillIdentityFromAuth(auth).matches(identity) {
			return auth
		}
	}
	return nil
}

func codexAutoRefillIdentityFromAuth(auth *coreauth.Auth) codexAutoRefillIdentity {
	if auth == nil {
		return codexAutoRefillIdentity{}
	}
	identity := codexAutoRefillIdentity{
		Email: strings.ToLower(strings.TrimSpace(authEmail(auth))),
	}
	if auth.Metadata != nil {
		if raw, ok := auth.Metadata["account_id"].(string); ok {
			identity.AccountID = strings.TrimSpace(raw)
		}
	}
	if identity.AccountID == "" || identity.Email == "" {
		if auth.Metadata != nil {
			if raw, ok := auth.Metadata["id_token"].(string); ok {
				claims, err := codexauth.ParseJWTToken(strings.TrimSpace(raw))
				if err == nil && claims != nil {
					if identity.AccountID == "" {
						identity.AccountID = strings.TrimSpace(claims.GetAccountID())
					}
					if identity.Email == "" {
						identity.Email = strings.ToLower(strings.TrimSpace(claims.GetUserEmail()))
					}
				}
			}
		}
	}
	return identity
}

func codexAutoRefillIdentityFromMetadata(meta map[string]any) codexAutoRefillIdentity {
	identity := codexAutoRefillIdentity{}
	if rawEmail, ok := meta["email"].(string); ok {
		identity.Email = strings.ToLower(strings.TrimSpace(rawEmail))
	}
	if rawAccountID, ok := meta["account_id"].(string); ok {
		identity.AccountID = strings.TrimSpace(rawAccountID)
	}
	if identity.AccountID == "" || identity.Email == "" {
		if rawIDToken, ok := meta["id_token"].(string); ok {
			claims, err := codexauth.ParseJWTToken(strings.TrimSpace(rawIDToken))
			if err == nil && claims != nil {
				if identity.AccountID == "" {
					identity.AccountID = strings.TrimSpace(claims.GetAccountID())
				}
				if identity.Email == "" {
					identity.Email = strings.ToLower(strings.TrimSpace(claims.GetUserEmail()))
				}
			}
		}
	}
	return identity
}

func (i codexAutoRefillIdentity) empty() bool {
	return i.Email == "" && i.AccountID == ""
}

func (i codexAutoRefillIdentity) matches(other codexAutoRefillIdentity) bool {
	if i.empty() || other.empty() {
		return false
	}
	if i.AccountID != "" && other.AccountID != "" && i.AccountID == other.AccountID {
		return true
	}
	return i.Email != "" && other.Email != "" && strings.EqualFold(i.Email, other.Email)
}

func codexAutoRefillFileName(original string, identity codexAutoRefillIdentity) string {
	base := filepath.Base(strings.TrimSpace(original))
	if strings.HasSuffix(strings.ToLower(base), ".json") {
		return sanitizeCodexAutoRefillFileName(base)
	}
	label := identity.AccountID
	if label == "" {
		label = identity.Email
	}
	if label == "" {
		label = "auth"
	}
	return sanitizeCodexAutoRefillFileName("codex-auto-refill-" + label + ".json")
}

func uniqueCodexAutoRefillPath(dir string, fileName string) (string, error) {
	fileName = sanitizeCodexAutoRefillFileName(fileName)
	if fileName == "" {
		fileName = fmt.Sprintf("codex-auto-refill-%d.json", time.Now().UnixNano())
	}
	base := strings.TrimSuffix(fileName, filepath.Ext(fileName))
	ext := filepath.Ext(fileName)
	if ext == "" {
		ext = ".json"
	}
	for idx := 0; idx < 1000; idx++ {
		candidate := fileName
		if idx > 0 {
			candidate = fmt.Sprintf("%s-%d%s", base, idx, ext)
		}
		full := filepath.Join(dir, candidate)
		if _, err := os.Stat(full); os.IsNotExist(err) {
			if abs, errAbs := filepath.Abs(full); errAbs == nil {
				return abs, nil
			}
			return full, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("unable to allocate refill auth file name for %s", fileName)
}

func sanitizeCodexAutoRefillFileName(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	if name == "" {
		return ""
	}
	if !strings.HasSuffix(strings.ToLower(name), ".json") {
		name += ".json"
	}
	name = strings.TrimSuffix(name, ".json")
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.' || r == '-' || r == '_' || r == '@':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	clean := strings.Trim(strings.ReplaceAll(b.String(), "--", "-"), "-")
	if clean == "" {
		clean = "codex-auto-refill"
	}
	return clean + ".json"
}
