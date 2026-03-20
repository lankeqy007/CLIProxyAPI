package management

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
const codexAutoRefillDefaultDownloadConcurrency = 4
const codexAutoRefillProviderQuotaCacheTTL = time.Minute

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
	HourlyClaimLimit      int
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
			h.setCodexAutoRefillNextCheck(time.Now().Add(interval))
			<-timer.C
			h.runCodexAutoRefillWithTrigger("scheduled")
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
	if refill.HourlyClaimLimit < 0 {
		refill.HourlyClaimLimit = 0
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
		HourlyClaimLimit:      refill.HourlyClaimLimit,
		RequireConsecutiveLow: refill.RequireConsecutiveLow,
		VerifyAfterImport:     refill.VerifyAfterImport,
		Priority:              refill.Priority,
		Note:                  strings.TrimSpace(refill.Note),
		AuthDir:               strings.TrimSpace(h.cfg.AuthDir),
		SDKConfig:             h.cfg.SDKConfig,
	}
}

func (h *Handler) codexAutoRefillProviderQuotaStatus(ctx context.Context, runtimeCfg codexAutoRefillRuntimeConfig, now time.Time, forceRefresh bool) codexAutoRefillProviderQuotaSnapshot {
	cacheKey := codexAutoRefillProviderQuotaCacheKey(runtimeCfg)
	if !forceRefresh {
		if snapshot, ok := h.cachedCodexAutoRefillProviderQuota(cacheKey, now); ok {
			return snapshot
		}
	}

	snapshot := h.fetchCodexAutoRefillProviderQuota(ctx, runtimeCfg, now)
	h.storeCodexAutoRefillProviderQuota(cacheKey, snapshot)
	return snapshot
}

func (h *Handler) cachedCodexAutoRefillProviderQuota(cacheKey string, now time.Time) (codexAutoRefillProviderQuotaSnapshot, bool) {
	if h == nil {
		return codexAutoRefillProviderQuotaSnapshot{}, false
	}

	h.codexAutoRefillMu.Lock()
	defer h.codexAutoRefillMu.Unlock()

	snapshot := h.codexAutoRefill.providerQuota
	if cacheKey == "" || h.codexAutoRefill.providerQuotaKey != cacheKey || snapshot.FetchedAt.IsZero() {
		return codexAutoRefillProviderQuotaSnapshot{}, false
	}
	if now.Sub(snapshot.FetchedAt) >= codexAutoRefillProviderQuotaCacheTTL {
		return codexAutoRefillProviderQuotaSnapshot{}, false
	}
	return snapshot, true
}

func (h *Handler) storeCodexAutoRefillProviderQuota(cacheKey string, snapshot codexAutoRefillProviderQuotaSnapshot) {
	if h == nil {
		return
	}
	h.codexAutoRefillMu.Lock()
	h.codexAutoRefill.providerQuota = snapshot
	h.codexAutoRefill.providerQuotaKey = cacheKey
	h.codexAutoRefillMu.Unlock()
}

func (h *Handler) fetchCodexAutoRefillProviderQuota(ctx context.Context, runtimeCfg codexAutoRefillRuntimeConfig, now time.Time) codexAutoRefillProviderQuotaSnapshot {
	session, err := resolveCodexAutoRefillProviderQuotaSession(runtimeCfg)
	snapshot := codexAutoRefillProviderQuotaSnapshot{
		Supported: err == nil,
		Endpoint:  "/me",
		AuthMode:  "session",
		FetchedAt: now.UTC(),
	}
	if err != nil {
		snapshot.Error = "GET /me requires token_atlas_session; keep refill mode as api-key if you want, but configure session-value or session-env separately"
		return snapshot
	}

	responseBody, _, err := h.codexAutoRefillRequest(ctx, runtimeCfg, http.MethodGet, "/me", nil, func(req *http.Request) {
		req.AddCookie(&http.Cookie{Name: "token_atlas_session", Value: session})
	})
	if err != nil {
		snapshot.Error = err.Error()
		return snapshot
	}

	payload, err := decodeCodexAutoRefillProviderQuota(responseBody)
	if err != nil {
		snapshot.Error = err.Error()
		return snapshot
	}

	snapshot.Raw = payload
	populateCodexAutoRefillProviderQuota(&snapshot, payload)
	return snapshot
}

func codexAutoRefillProviderQuotaCacheKey(runtimeCfg codexAutoRefillRuntimeConfig) string {
	baseURL := strings.TrimRight(strings.TrimSpace(runtimeCfg.ProviderURL), "/")
	session, err := resolveCodexAutoRefillProviderQuotaSession(runtimeCfg)
	if err != nil {
		return baseURL + "|provider-me|missing"
	}
	return baseURL + "|provider-me|" + codexAutoRefillSecretHash(session)
}

func codexAutoRefillSecretHash(value string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(value)))
	return hex.EncodeToString(sum[:8])
}

func resolveCodexAutoRefillProviderQuotaSession(runtimeCfg codexAutoRefillRuntimeConfig) (string, error) {
	return resolveCodexAutoRefillSecret(
		runtimeCfg.SessionValue,
		runtimeCfg.SessionEnv,
		codexAutoRefillDefaultSessionEnv,
		"codex auto-refill session",
		"quota-exceeded.codex-auto-refill.session-value",
	)
}

func codexAutoRefillProviderRemaining(snapshot codexAutoRefillProviderQuotaSnapshot) (int, bool) {
	if snapshot.QuotaRemaining == nil {
		return 0, false
	}
	if *snapshot.QuotaRemaining <= 0 {
		return 0, true
	}
	if *snapshot.QuotaRemaining > int64(^uint(0)>>1) {
		return int(^uint(0) >> 1), true
	}
	return int(*snapshot.QuotaRemaining), true
}

func decodeCodexAutoRefillProviderQuota(raw []byte) (map[string]any, error) {
	var payload any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("decode provider /me response: %w", err)
	}
	object, ok := payload.(map[string]any)
	if ok {
		return object, nil
	}
	return map[string]any{"value": payload}, nil
}

func populateCodexAutoRefillProviderQuota(snapshot *codexAutoRefillProviderQuotaSnapshot, payload map[string]any) {
	if snapshot == nil || payload == nil {
		return
	}

	userMap := codexAutoRefillFirstMap(payload, "user", "me", "profile", "account")
	quotaMap := codexAutoRefillFirstMap(payload, "quota", "allowance", "limits")
	claimsMap := codexAutoRefillFirstMap(payload, "claims", "claim_stats", "statistics", "stats", "usage")

	snapshot.UserID = codexAutoRefillFirstString([]map[string]any{userMap, payload}, "id", "user_id", "uid")
	snapshot.Username = codexAutoRefillFirstString([]map[string]any{userMap, payload}, "name", "username", "login", "nickname", "display_name")
	snapshot.Email = codexAutoRefillFirstString([]map[string]any{userMap, payload}, "email", "mail")
	snapshot.QuotaRemaining = codexAutoRefillFirstInt64([]map[string]any{quotaMap, claimsMap, payload}, "remaining", "remaining_count", "quota_remaining", "available", "available_count", "hourly_remaining")
	snapshot.QuotaUsed = codexAutoRefillFirstInt64([]map[string]any{quotaMap, claimsMap, payload}, "used", "used_count", "quota_used", "claimed", "claim_count", "hourly_used")
	snapshot.QuotaLimit = codexAutoRefillFirstInt64([]map[string]any{quotaMap, claimsMap, payload}, "limit", "total", "max", "quota", "quota_limit", "hourly_limit")
	snapshot.ClaimCount = codexAutoRefillFirstInt64([]map[string]any{claimsMap, payload}, "count", "claimed", "claimed_count", "issued", "received")
	snapshot.ClaimLimit = codexAutoRefillFirstInt64([]map[string]any{claimsMap, quotaMap, payload}, "limit", "max", "total", "claim_limit", "hourly_limit")

	if snapshot.ClaimCount == nil {
		if list, ok := payload["claims"].([]any); ok {
			count := int64(len(list))
			snapshot.ClaimCount = &count
		}
	}
}

func codexAutoRefillFirstMap(payload map[string]any, keys ...string) map[string]any {
	for _, key := range keys {
		value, ok := payload[key]
		if !ok {
			continue
		}
		if typed, ok := value.(map[string]any); ok {
			return typed
		}
	}
	return nil
}

func codexAutoRefillFirstString(candidates []map[string]any, keys ...string) string {
	for _, candidate := range candidates {
		if candidate == nil {
			continue
		}
		for _, key := range keys {
			value, ok := candidate[key]
			if !ok {
				continue
			}
			switch typed := value.(type) {
			case string:
				if text := strings.TrimSpace(typed); text != "" {
					return text
				}
			case fmt.Stringer:
				if text := strings.TrimSpace(typed.String()); text != "" {
					return text
				}
			}
		}
	}
	return ""
}

func codexAutoRefillFirstInt64(candidates []map[string]any, keys ...string) *int64 {
	for _, candidate := range candidates {
		if candidate == nil {
			continue
		}
		for _, key := range keys {
			value, ok := candidate[key]
			if !ok {
				continue
			}
			if parsed, ok := codexAutoRefillToInt64(value); ok {
				result := parsed
				return &result
			}
		}
	}
	return nil
}

func codexAutoRefillToInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int8:
		return int64(typed), true
	case int16:
		return int64(typed), true
	case int32:
		return int64(typed), true
	case int64:
		return typed, true
	case uint:
		return int64(typed), true
	case uint8:
		return int64(typed), true
	case uint16:
		return int64(typed), true
	case uint32:
		return int64(typed), true
	case uint64:
		if typed > uint64(^uint64(0)>>1) {
			return 0, false
		}
		return int64(typed), true
	case float32:
		return int64(typed), true
	case float64:
		return int64(typed), true
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return 0, false
		}
		return parsed, true
	case string:
		text := strings.TrimSpace(typed)
		if text == "" {
			return 0, false
		}
		var parsed int64
		if _, err := fmt.Sscan(text, &parsed); err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

func (h *Handler) runCodexAutoRefill() {
	h.runCodexAutoRefillWithTrigger("scheduled")
}

func (h *Handler) runCodexAutoRefillWithTrigger(trigger string) {
	runtimeCfg := h.currentCodexAutoRefillRuntimeConfig()
	now := time.Now()
	if !runtimeCfg.Enable || runtimeCfg.AuthDir == "" {
		h.observeCodexAutoRefill(now, trigger, codexAutoRefillPoolSnapshot{}, "auto-refill disabled or auth-dir is empty")
		if trigger == "manual" {
			h.codexAutoRefillLog("info", "skip", "manual refill skipped because auto-refill is disabled or auth-dir is empty", func(entry *codexAutoRefillLogEntry) {
				entry.Trigger = trigger
			})
		}
		h.resetCodexAutoRefillLow()
		return
	}
	manager := h.currentAuthManager()
	if manager == nil {
		h.observeCodexAutoRefill(now, trigger, codexAutoRefillPoolSnapshot{}, "core auth manager unavailable")
		if trigger == "manual" {
			h.codexAutoRefillLog("warn", "skip", "manual refill skipped because core auth manager is unavailable", func(entry *codexAutoRefillLogEntry) {
				entry.Trigger = trigger
			})
		}
		return
	}
	if !h.beginCodexAutoRefillRun() {
		h.observeCodexAutoRefill(now, trigger, codexAutoRefillPoolSnapshot{}, "another refill run is already in progress")
		if trigger == "manual" {
			h.codexAutoRefillLog("info", "skip", "manual refill skipped because another run is already in progress", func(entry *codexAutoRefillLogEntry) {
				entry.Trigger = trigger
			})
		}
		return
	}
	defer func() {
		h.endCodexAutoRefillRun(time.Now())
	}()

	pool := h.codexAutoRefillPool(now)
	h.observeCodexAutoRefill(now, trigger, pool, "")
	h.markCodexAutoRefillRunStarted(now, trigger, pool)
	if pool.ReadyCount >= runtimeCfg.LowWatermark {
		skipReason := fmt.Sprintf("ready pool is healthy (%d >= %d)", pool.ReadyCount, runtimeCfg.LowWatermark)
		h.observeCodexAutoRefill(now, trigger, pool, skipReason)
		if trigger == "manual" {
			h.codexAutoRefillLog("info", "skip", "manual refill skipped because ready pool is already healthy", func(entry *codexAutoRefillLogEntry) {
				entry.Trigger = trigger
				entry.ReadyCount = pool.ReadyCount
				entry.CoolingCount = pool.CoolingCount
				entry.UnavailableCount = pool.UnavailableCount
				entry.DisabledCount = pool.DisabledCount
				entry.TotalCount = pool.TotalCount
			})
		}
		h.resetCodexAutoRefillLow()
		return
	}

	lowCount := h.incrementCodexAutoRefillLow()
	if lowCount < runtimeCfg.RequireConsecutiveLow {
		skipReason := fmt.Sprintf("ready=%d below low-watermark=%d but only %d/%d consecutive low checks observed", pool.ReadyCount, runtimeCfg.LowWatermark, lowCount, runtimeCfg.RequireConsecutiveLow)
		h.observeCodexAutoRefill(now, trigger, pool, skipReason)
		h.codexAutoRefillLog("info", "low-watermark", skipReason, func(entry *codexAutoRefillLogEntry) {
			entry.Trigger = trigger
			entry.ReadyCount = pool.ReadyCount
			entry.CoolingCount = pool.CoolingCount
			entry.UnavailableCount = pool.UnavailableCount
			entry.DisabledCount = pool.DisabledCount
			entry.TotalCount = pool.TotalCount
		})
		log.Debugf("management codex auto-refill: ready=%d below low-watermark=%d (%d/%d)", pool.ReadyCount, runtimeCfg.LowWatermark, lowCount, runtimeCfg.RequireConsecutiveLow)
		return
	}
	if !h.codexAutoRefillCanClaim(now, runtimeCfg.MinClaimInterval) {
		skipReason := fmt.Sprintf("minimum claim interval not reached (%s)", runtimeCfg.MinClaimInterval)
		h.observeCodexAutoRefill(now, trigger, pool, skipReason)
		h.codexAutoRefillLog("info", "skip", skipReason, func(entry *codexAutoRefillLogEntry) {
			entry.Trigger = trigger
			entry.ReadyCount = pool.ReadyCount
			entry.CoolingCount = pool.CoolingCount
			entry.UnavailableCount = pool.UnavailableCount
			entry.DisabledCount = pool.DisabledCount
			entry.TotalCount = pool.TotalCount
		})
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
		skipReason := fmt.Sprintf("no refill needed after applying target and max-claim rules (target=%d ready=%d)", runtimeCfg.TargetReady, pool.ReadyCount)
		h.observeCodexAutoRefill(now, trigger, pool, skipReason)
		h.codexAutoRefillLog("info", "skip", skipReason, func(entry *codexAutoRefillLogEntry) {
			entry.Trigger = trigger
			entry.ReadyCount = pool.ReadyCount
			entry.CoolingCount = pool.CoolingCount
			entry.UnavailableCount = pool.UnavailableCount
			entry.DisabledCount = pool.DisabledCount
			entry.TotalCount = pool.TotalCount
		})
		return
	}

	if runtimeCfg.HourlyClaimLimit > 0 {
		claimedThisHour, hourlyRemaining, _, _ := h.codexAutoRefillHourlyAllowance(now, runtimeCfg.HourlyClaimLimit)
		if hourlyRemaining <= 0 {
			skipReason := fmt.Sprintf("hourly claim limit reached (%d/%d used in the last hour)", claimedThisHour, runtimeCfg.HourlyClaimLimit)
			h.observeCodexAutoRefill(now, trigger, pool, skipReason)
			h.codexAutoRefillLog("warn", "hourly-limit", skipReason, func(entry *codexAutoRefillLogEntry) {
				entry.Trigger = trigger
				entry.ReadyCount = pool.ReadyCount
				entry.CoolingCount = pool.CoolingCount
				entry.UnavailableCount = pool.UnavailableCount
				entry.DisabledCount = pool.DisabledCount
				entry.TotalCount = pool.TotalCount
			})
			return
		}
		if missing > hourlyRemaining {
			h.codexAutoRefillLog("info", "hourly-limit", fmt.Sprintf("refill request capped by hourly limit: requested=%d allowed=%d", missing, hourlyRemaining), func(entry *codexAutoRefillLogEntry) {
				entry.Trigger = trigger
				entry.RequestedCount = missing
			})
			missing = hourlyRemaining
		}
	}
	if providerQuota := h.codexAutoRefillProviderQuotaStatus(context.Background(), runtimeCfg, now, true); providerQuota.Supported {
		if strings.TrimSpace(providerQuota.Error) != "" {
			skipReason := fmt.Sprintf("provider quota refresh failed: %s", strings.TrimSpace(providerQuota.Error))
			h.observeCodexAutoRefill(now, trigger, pool, skipReason)
			h.codexAutoRefillLog("warn", "provider-quota", skipReason, func(entry *codexAutoRefillLogEntry) {
				entry.Trigger = trigger
				entry.ReadyCount = pool.ReadyCount
				entry.CoolingCount = pool.CoolingCount
				entry.UnavailableCount = pool.UnavailableCount
				entry.DisabledCount = pool.DisabledCount
				entry.TotalCount = pool.TotalCount
				entry.Error = strings.TrimSpace(providerQuota.Error)
			})
			return
		}
		if providerRemaining, ok := codexAutoRefillProviderRemaining(providerQuota); ok {
			if providerRemaining <= 0 {
				skipReason := "provider quota exhausted (remaining=0)"
				h.observeCodexAutoRefill(now, trigger, pool, skipReason)
				h.codexAutoRefillLog("warn", "provider-quota", skipReason, func(entry *codexAutoRefillLogEntry) {
					entry.Trigger = trigger
					entry.ReadyCount = pool.ReadyCount
					entry.CoolingCount = pool.CoolingCount
					entry.UnavailableCount = pool.UnavailableCount
					entry.DisabledCount = pool.DisabledCount
					entry.TotalCount = pool.TotalCount
				})
				return
			}
			if missing > providerRemaining {
				h.codexAutoRefillLog("info", "provider-quota", fmt.Sprintf("refill request capped by provider quota: requested=%d allowed=%d", missing, providerRemaining), func(entry *codexAutoRefillLogEntry) {
					entry.Trigger = trigger
					entry.RequestedCount = missing
				})
				missing = providerRemaining
			}
		}
	}
	if missing <= 0 {
		return
	}

	h.codexAutoRefillLog("info", "run-start", fmt.Sprintf("starting codex auto-refill (%s)", formatCodexAutoRefillPoolMessage(pool)), func(entry *codexAutoRefillLogEntry) {
		entry.Trigger = trigger
		entry.RequestedCount = missing
		entry.ReadyCount = pool.ReadyCount
		entry.CoolingCount = pool.CoolingCount
		entry.UnavailableCount = pool.UnavailableCount
		entry.DisabledCount = pool.DisabledCount
		entry.TotalCount = pool.TotalCount
	})

	imported, err := h.codexAutoRefillClaimAndImport(context.Background(), manager, runtimeCfg, missing)
	if err != nil {
		finishedAt := time.Now()
		h.markCodexAutoRefillFailure(finishedAt, "", err, missing)
		h.codexAutoRefillLog("warn", "run-failed", fmt.Sprintf("claim/import failed: %v", err), func(entry *codexAutoRefillLogEntry) {
			entry.Trigger = trigger
			entry.RequestedCount = missing
			entry.ReadyCount = pool.ReadyCount
			entry.CoolingCount = pool.CoolingCount
			entry.UnavailableCount = pool.UnavailableCount
			entry.DisabledCount = pool.DisabledCount
			entry.TotalCount = pool.TotalCount
			entry.Error = err.Error()
		})
		log.WithError(err).Warnf("management codex auto-refill: claim/import failed ready=%d cooling=%d unavailable=%d total=%d", pool.ReadyCount, pool.CoolingCount, pool.UnavailableCount, pool.TotalCount)
		return
	}

	finishedAt := time.Now()
	h.markCodexAutoRefillClaimed(finishedAt, missing)
	h.markCodexAutoRefillSuccess(finishedAt, missing, imported)
	h.codexAutoRefillLog("info", "run-success", fmt.Sprintf("refill completed: imported=%d requested=%d", imported, missing), func(entry *codexAutoRefillLogEntry) {
		entry.Trigger = trigger
		entry.RequestedCount = missing
		entry.ImportedCount = imported
		entry.ReadyCount = pool.ReadyCount
		entry.CoolingCount = pool.CoolingCount
		entry.UnavailableCount = pool.UnavailableCount
		entry.DisabledCount = pool.DisabledCount
		entry.TotalCount = pool.TotalCount
	})
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

func (h *Handler) endCodexAutoRefillRun(finishedAt time.Time) {
	if h == nil {
		return
	}
	h.codexAutoRefillMu.Lock()
	h.codexAutoRefill.running = false
	h.codexAutoRefill.lastRunFinishedAt = finishedAt
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

func (h *Handler) markCodexAutoRefillClaimed(now time.Time, claimedCount int) {
	if h == nil {
		return
	}
	h.codexAutoRefillMu.Lock()
	if claimedCount > 0 {
		if h.codexAutoRefill.hourlyWindowStart.IsZero() || now.Sub(h.codexAutoRefill.hourlyWindowStart) >= time.Hour {
			h.codexAutoRefill.hourlyWindowStart = now
			h.codexAutoRefill.hourlyClaimed = 0
		}
		h.codexAutoRefill.hourlyClaimed += claimedCount
	}
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
			h.codexAutoRefillLog("warn", "import-failed", fmt.Sprintf("failed to import %s: %v", strings.TrimSpace(file.Name), errImport), func(entry *codexAutoRefillLogEntry) {
				entry.Error = errImport.Error()
			})
			log.WithError(errImport).Warnf("management codex auto-refill: failed to import %s", strings.TrimSpace(file.Name))
			continue
		}
		if !importedOne {
			h.codexAutoRefillLog("info", "import-skipped", fmt.Sprintf("skipped duplicate or already-known auth file %s", strings.TrimSpace(file.Name)), nil)
			continue
		}
		imported++
		h.codexAutoRefillLog("info", "import-success", fmt.Sprintf("imported auth file %s", strings.TrimSpace(file.Name)), nil)
		if runtimeCfg.VerifyAfterImport && auth != nil {
			if decision, shouldDelete := codexSweepDeleteDecision(auth, time.Now()); shouldDelete {
				_ = h.deleteCodexAuthFile(context.Background(), auth, h.codexSweepTargetPath(auth))
				h.codexAutoRefillLog("warn", "verify-drop", fmt.Sprintf("dropped imported auth %s after verification (%s)", strings.TrimSpace(file.Name), decision.reason), func(entry *codexAutoRefillLogEntry) {
					entry.Error = decision.reason
				})
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
	h.codexAutoRefillLog("info", "claim-response", fmt.Sprintf("provider returned %d token ids", len(tokenIDs)), func(entry *codexAutoRefillLogEntry) {
		entry.RequestedCount = len(tokenIDs)
		entry.Path = "/api/claim"
		entry.Method = http.MethodPost
	})
	concurrency := codexAutoRefillDownloadConcurrency(len(tokenIDs))
	h.codexAutoRefillLog("info", "download-start", fmt.Sprintf("downloading %d auth files from provider with concurrency=%d", len(tokenIDs), concurrency), func(entry *codexAutoRefillLogEntry) {
		entry.Method = http.MethodGet
		entry.Path = "/api/download/:token_id"
		entry.RequestedCount = len(tokenIDs)
	})
	files, err := h.codexAutoRefillDownloadFiles(ctx, runtimeCfg, tokenIDs, concurrency, func(req *http.Request) {
		req.Header.Set("X-API-Key", apiKey)
	})
	if err != nil {
		return nil, err
	}
	h.codexAutoRefillLog("info", "download-complete", fmt.Sprintf("downloaded %d auth files from provider", len(files)), func(entry *codexAutoRefillLogEntry) {
		entry.Method = http.MethodGet
		entry.Path = "/api/download/:token_id"
		entry.RequestedCount = len(files)
	})
	return files, nil
}

func codexAutoRefillDownloadConcurrency(total int) int {
	if total <= 1 {
		return 1
	}
	if total < codexAutoRefillDefaultDownloadConcurrency {
		return total
	}
	return codexAutoRefillDefaultDownloadConcurrency
}

func (h *Handler) codexAutoRefillDownloadFiles(ctx context.Context, runtimeCfg codexAutoRefillRuntimeConfig, tokenIDs []string, concurrency int, decorate func(*http.Request)) ([]codexAutoRefillDownloadedFile, error) {
	if len(tokenIDs) == 0 {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if concurrency <= 1 {
		files := make([]codexAutoRefillDownloadedFile, 0, len(tokenIDs))
		for _, tokenID := range tokenIDs {
			file, err := h.codexAutoRefillDownloadFile(ctx, runtimeCfg, tokenID, decorate)
			if err != nil {
				return nil, err
			}
			files = append(files, file)
		}
		return files, nil
	}
	if concurrency > len(tokenIDs) {
		concurrency = len(tokenIDs)
	}

	type downloadResult struct {
		index int
		file  codexAutoRefillDownloadedFile
	}

	downloadCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan int, len(tokenIDs))
	results := make(chan downloadResult, len(tokenIDs))
	for index := range tokenIDs {
		jobs <- index
	}
	close(jobs)

	var (
		wg       sync.WaitGroup
		firstErr error
		errOnce  sync.Once
	)

	worker := func() {
		defer wg.Done()
		for index := range jobs {
			if downloadCtx.Err() != nil {
				return
			}
			file, err := h.codexAutoRefillDownloadFile(downloadCtx, runtimeCfg, tokenIDs[index], decorate)
			if err != nil {
				errOnce.Do(func() {
					firstErr = err
					cancel()
				})
				return
			}
			results <- downloadResult{index: index, file: file}
		}
	}

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go worker()
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	files := make([]codexAutoRefillDownloadedFile, len(tokenIDs))
	for result := range results {
		files[result.index] = result.file
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return files, nil
}

func (h *Handler) codexAutoRefillDownloadFile(ctx context.Context, runtimeCfg codexAutoRefillRuntimeConfig, tokenID string, decorate func(*http.Request)) (codexAutoRefillDownloadedFile, error) {
	body, headers, err := h.codexAutoRefillRequest(ctx, runtimeCfg, http.MethodGet, "/api/download/"+tokenID, nil, decorate)
	if err != nil {
		return codexAutoRefillDownloadedFile{}, err
	}
	name := fileNameFromContentDisposition(headers.Get("Content-Disposition"))
	if name == "" {
		name = "token-" + tokenID + ".json"
	}
	return codexAutoRefillDownloadedFile{Name: name, Data: body}, nil
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
	files, err := unzipCodexAutoRefillArchive(archiveBody)
	if err != nil {
		return nil, err
	}
	h.codexAutoRefillLog("info", "download-complete", fmt.Sprintf("downloaded %d auth files from session archive", len(files)), func(entry *codexAutoRefillLogEntry) {
		entry.Method = http.MethodGet
		entry.Path = "/me/claims/archive"
		entry.RequestedCount = len(files)
	})
	return files, nil
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
	requestCtx := ctx
	if requestCtx == nil {
		requestCtx = context.Background()
	}
	var cancel context.CancelFunc
	if runtimeCfg.Timeout > 0 {
		requestCtx, cancel = context.WithTimeout(requestCtx, runtimeCfg.Timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(requestCtx, method, targetURL, reader)
	if err != nil {
		return nil, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if decorate != nil {
		decorate(req)
	}
	startedAt := time.Now()
	h.codexAutoRefillLog("info", "provider-request", fmt.Sprintf("requesting refill provider %s %s", method, path), func(entry *codexAutoRefillLogEntry) {
		entry.Method = method
		entry.Path = path
	})

	clientTimeout := runtimeCfg.Timeout
	if clientTimeout <= 0 {
		clientTimeout = 20 * time.Second
	}
	client := util.SetProxy(&runtimeCfg.SDKConfig, &http.Client{Timeout: clientTimeout})
	resp, err := client.Do(req)
	if err != nil {
		elapsedMs := time.Since(startedAt).Milliseconds()
		h.codexAutoRefillLog("warn", "provider-request-failed", fmt.Sprintf("refill provider request failed for %s %s", method, path), func(entry *codexAutoRefillLogEntry) {
			entry.Method = method
			entry.Path = path
			entry.DurationMs = elapsedMs
			entry.Error = err.Error()
		})
		return nil, nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		elapsedMs := time.Since(startedAt).Milliseconds()
		h.codexAutoRefillLog("warn", "provider-response-read-failed", fmt.Sprintf("failed to read refill provider response body for %s %s", method, path), func(entry *codexAutoRefillLogEntry) {
			entry.Method = method
			entry.Path = path
			entry.StatusCode = resp.StatusCode
			entry.DurationMs = elapsedMs
			entry.Error = err.Error()
		})
		return nil, nil, err
	}
	elapsedMs := time.Since(startedAt).Milliseconds()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		h.codexAutoRefillLog("warn", "provider-response", fmt.Sprintf("refill provider responded with status %d for %s %s", resp.StatusCode, method, path), func(entry *codexAutoRefillLogEntry) {
			entry.Method = method
			entry.Path = path
			entry.StatusCode = resp.StatusCode
			entry.DurationMs = elapsedMs
			entry.Error = strings.TrimSpace(string(responseBody))
		})
		return nil, resp.Header.Clone(), fmt.Errorf("refill request %s %s failed with status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	h.codexAutoRefillLog("info", "provider-response", fmt.Sprintf("refill provider responded with status %d for %s %s", resp.StatusCode, method, path), func(entry *codexAutoRefillLogEntry) {
		entry.Method = method
		entry.Path = path
		entry.StatusCode = resp.StatusCode
		entry.DurationMs = elapsedMs
	})
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
