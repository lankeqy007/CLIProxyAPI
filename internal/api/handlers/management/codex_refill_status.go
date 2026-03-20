package management

import (
	"fmt"
	"strings"
	"time"
)

const codexAutoRefillMaxLogEntries = 200

type codexAutoRefillLogEntry struct {
	Timestamp        time.Time `json:"timestamp"`
	Level            string    `json:"level"`
	Event            string    `json:"event"`
	Message          string    `json:"message"`
	Trigger          string    `json:"trigger,omitempty"`
	Method           string    `json:"method,omitempty"`
	Path             string    `json:"path,omitempty"`
	StatusCode       int       `json:"statusCode,omitempty"`
	DurationMs       int64     `json:"durationMs,omitempty"`
	RequestedCount   int       `json:"requestedCount,omitempty"`
	ImportedCount    int       `json:"importedCount,omitempty"`
	ReadyCount       int       `json:"readyCount,omitempty"`
	CoolingCount     int       `json:"coolingCount,omitempty"`
	UnavailableCount int       `json:"unavailableCount,omitempty"`
	DisabledCount    int       `json:"disabledCount,omitempty"`
	TotalCount       int       `json:"totalCount,omitempty"`
	Error            string    `json:"error,omitempty"`
}

type codexAutoRefillProviderQuotaSnapshot struct {
	Supported      bool           `json:"supported"`
	Endpoint       string         `json:"endpoint,omitempty"`
	AuthMode       string         `json:"authMode,omitempty"`
	FetchedAt      time.Time      `json:"fetchedAt,omitempty"`
	Error          string         `json:"error,omitempty"`
	UserID         string         `json:"userId,omitempty"`
	Username       string         `json:"username,omitempty"`
	Email          string         `json:"email,omitempty"`
	QuotaRemaining *int64         `json:"quotaRemaining,omitempty"`
	QuotaUsed      *int64         `json:"quotaUsed,omitempty"`
	QuotaLimit     *int64         `json:"quotaLimit,omitempty"`
	ClaimCount     *int64         `json:"claimCount,omitempty"`
	ClaimLimit     *int64         `json:"claimLimit,omitempty"`
	Raw            map[string]any `json:"raw,omitempty"`
}

type codexAutoRefillStatusSnapshot struct {
	Running           bool                                 `json:"running"`
	ConsecutiveLow    int                                  `json:"consecutiveLow"`
	LastClaimAt       time.Time                            `json:"lastClaimAt,omitempty"`
	LastCheckAt       time.Time                            `json:"lastCheckAt,omitempty"`
	NextCheckAt       time.Time                            `json:"nextCheckAt,omitempty"`
	LastRunStartedAt  time.Time                            `json:"lastRunStartedAt,omitempty"`
	LastRunFinishedAt time.Time                            `json:"lastRunFinishedAt,omitempty"`
	LastSuccessAt     time.Time                            `json:"lastSuccessAt,omitempty"`
	LastErrorAt       time.Time                            `json:"lastErrorAt,omitempty"`
	LastError         string                               `json:"lastError,omitempty"`
	LastSkipReason    string                               `json:"lastSkipReason,omitempty"`
	LastTrigger       string                               `json:"lastTrigger,omitempty"`
	LastRequested     int                                  `json:"lastRequested"`
	LastImported      int                                  `json:"lastImported"`
	CurrentPool       codexAutoRefillPoolSnapshot          `json:"currentPool"`
	LastObservedPool  codexAutoRefillPoolSnapshot          `json:"lastObservedPool"`
	HourlyClaimed     int                                  `json:"hourlyClaimed"`
	HourlyRemaining   int                                  `json:"hourlyRemaining"`
	HourlyWindowStart time.Time                            `json:"hourlyWindowStart,omitempty"`
	HourlyResetAt     time.Time                            `json:"hourlyResetAt,omitempty"`
	ProviderQuota     codexAutoRefillProviderQuotaSnapshot `json:"providerQuota"`
	Logs              []codexAutoRefillLogEntry            `json:"logs"`
}

func (h *Handler) setCodexAutoRefillNextCheck(nextCheckAt time.Time) {
	if h == nil {
		return
	}
	h.codexAutoRefillMu.Lock()
	h.codexAutoRefill.nextCheckAt = nextCheckAt
	h.codexAutoRefillMu.Unlock()
}

func (h *Handler) currentCodexAutoRefillProviderQuotaSnapshot() codexAutoRefillProviderQuotaSnapshot {
	if h == nil {
		return codexAutoRefillProviderQuotaSnapshot{}
	}
	h.codexAutoRefillMu.Lock()
	defer h.codexAutoRefillMu.Unlock()
	return h.codexAutoRefill.providerQuota
}

func (h *Handler) codexAutoRefillLog(level string, event string, message string, enrich func(*codexAutoRefillLogEntry)) {
	if h == nil {
		return
	}
	entry := codexAutoRefillLogEntry{
		Timestamp: time.Now().UTC(),
		Level:     strings.ToLower(strings.TrimSpace(level)),
		Event:     strings.TrimSpace(event),
		Message:   strings.TrimSpace(message),
	}
	if entry.Level == "" {
		entry.Level = "info"
	}
	if enrich != nil {
		enrich(&entry)
	}

	h.codexAutoRefillMu.Lock()
	defer h.codexAutoRefillMu.Unlock()
	h.codexAutoRefill.logs = append(h.codexAutoRefill.logs, entry)
	if len(h.codexAutoRefill.logs) > codexAutoRefillMaxLogEntries {
		h.codexAutoRefill.logs = append([]codexAutoRefillLogEntry(nil), h.codexAutoRefill.logs[len(h.codexAutoRefill.logs)-codexAutoRefillMaxLogEntries:]...)
	}
}

func (h *Handler) observeCodexAutoRefill(now time.Time, trigger string, pool codexAutoRefillPoolSnapshot, skipReason string) {
	if h == nil {
		return
	}
	h.codexAutoRefillMu.Lock()
	h.codexAutoRefill.lastCheckAt = now
	h.codexAutoRefill.lastTrigger = strings.TrimSpace(trigger)
	h.codexAutoRefill.lastObservedPool = pool
	h.codexAutoRefill.lastSkipReason = strings.TrimSpace(skipReason)
	h.codexAutoRefillMu.Unlock()
}

func (h *Handler) markCodexAutoRefillRunStarted(now time.Time, trigger string, pool codexAutoRefillPoolSnapshot) {
	if h == nil {
		return
	}
	h.codexAutoRefillMu.Lock()
	h.codexAutoRefill.lastCheckAt = now
	h.codexAutoRefill.lastRunStartedAt = now
	h.codexAutoRefill.lastTrigger = strings.TrimSpace(trigger)
	h.codexAutoRefill.lastSkipReason = ""
	h.codexAutoRefill.lastObservedPool = pool
	h.codexAutoRefill.lastRequested = 0
	h.codexAutoRefill.lastImported = 0
	h.codexAutoRefillMu.Unlock()
}

func (h *Handler) markCodexAutoRefillRunFinished(now time.Time) {
	if h == nil {
		return
	}
	h.codexAutoRefillMu.Lock()
	h.codexAutoRefill.lastRunFinishedAt = now
	h.codexAutoRefillMu.Unlock()
}

func (h *Handler) markCodexAutoRefillFailure(now time.Time, skipReason string, err error, requested int) {
	if h == nil {
		return
	}
	message := strings.TrimSpace(skipReason)
	if err != nil {
		message = strings.TrimSpace(err.Error())
	}
	h.codexAutoRefillMu.Lock()
	h.codexAutoRefill.lastErrorAt = now
	h.codexAutoRefill.lastError = message
	h.codexAutoRefill.lastSkipReason = message
	h.codexAutoRefill.lastRequested = requested
	h.codexAutoRefill.lastImported = 0
	h.codexAutoRefillMu.Unlock()
}

func (h *Handler) markCodexAutoRefillSuccess(now time.Time, requested int, imported int) {
	if h == nil {
		return
	}
	h.codexAutoRefillMu.Lock()
	h.codexAutoRefill.lastSuccessAt = now
	h.codexAutoRefill.lastError = ""
	h.codexAutoRefill.lastSkipReason = ""
	h.codexAutoRefill.lastRequested = requested
	h.codexAutoRefill.lastImported = imported
	h.codexAutoRefillMu.Unlock()
}

func (h *Handler) codexAutoRefillHourlyAllowance(now time.Time, limit int) (claimed int, remaining int, windowStart time.Time, resetAt time.Time) {
	if h == nil {
		if limit > 0 {
			return 0, limit, time.Time{}, time.Time{}
		}
		return 0, 0, time.Time{}, time.Time{}
	}

	h.codexAutoRefillMu.Lock()
	defer h.codexAutoRefillMu.Unlock()

	if limit <= 0 {
		return 0, 0, time.Time{}, time.Time{}
	}
	if !h.codexAutoRefill.hourlyWindowStart.IsZero() && now.Sub(h.codexAutoRefill.hourlyWindowStart) >= time.Hour {
		h.codexAutoRefill.hourlyWindowStart = time.Time{}
		h.codexAutoRefill.hourlyClaimed = 0
	}
	if h.codexAutoRefill.hourlyWindowStart.IsZero() {
		return 0, limit, time.Time{}, time.Time{}
	}

	claimed = h.codexAutoRefill.hourlyClaimed
	remaining = limit - claimed
	if remaining < 0 {
		remaining = 0
	}
	windowStart = h.codexAutoRefill.hourlyWindowStart
	resetAt = windowStart.Add(time.Hour)
	return claimed, remaining, windowStart, resetAt
}

func (h *Handler) codexAutoRefillStatusSnapshot(runtimeCfg codexAutoRefillRuntimeConfig, currentPool codexAutoRefillPoolSnapshot, providerQuota codexAutoRefillProviderQuotaSnapshot, now time.Time) codexAutoRefillStatusSnapshot {
	if h == nil {
		return codexAutoRefillStatusSnapshot{CurrentPool: currentPool, ProviderQuota: providerQuota}
	}

	h.codexAutoRefillMu.Lock()
	if runtimeCfg.HourlyClaimLimit > 0 && !h.codexAutoRefill.hourlyWindowStart.IsZero() && now.Sub(h.codexAutoRefill.hourlyWindowStart) >= time.Hour {
		h.codexAutoRefill.hourlyWindowStart = time.Time{}
		h.codexAutoRefill.hourlyClaimed = 0
	}

	snapshot := codexAutoRefillStatusSnapshot{
		Running:           h.codexAutoRefill.running,
		ConsecutiveLow:    h.codexAutoRefill.consecutiveLow,
		LastClaimAt:       h.codexAutoRefill.lastClaimAt,
		LastCheckAt:       h.codexAutoRefill.lastCheckAt,
		NextCheckAt:       h.codexAutoRefill.nextCheckAt,
		LastRunStartedAt:  h.codexAutoRefill.lastRunStartedAt,
		LastRunFinishedAt: h.codexAutoRefill.lastRunFinishedAt,
		LastSuccessAt:     h.codexAutoRefill.lastSuccessAt,
		LastErrorAt:       h.codexAutoRefill.lastErrorAt,
		LastError:         h.codexAutoRefill.lastError,
		LastSkipReason:    h.codexAutoRefill.lastSkipReason,
		LastTrigger:       h.codexAutoRefill.lastTrigger,
		LastRequested:     h.codexAutoRefill.lastRequested,
		LastImported:      h.codexAutoRefill.lastImported,
		CurrentPool:       currentPool,
		LastObservedPool:  h.codexAutoRefill.lastObservedPool,
		ProviderQuota:     providerQuota,
		Logs:              append([]codexAutoRefillLogEntry(nil), h.codexAutoRefill.logs...),
	}
	if runtimeCfg.HourlyClaimLimit > 0 {
		snapshot.HourlyClaimed = h.codexAutoRefill.hourlyClaimed
		snapshot.HourlyRemaining = runtimeCfg.HourlyClaimLimit - h.codexAutoRefill.hourlyClaimed
		if snapshot.HourlyRemaining < 0 {
			snapshot.HourlyRemaining = 0
		}
		snapshot.HourlyWindowStart = h.codexAutoRefill.hourlyWindowStart
		if !h.codexAutoRefill.hourlyWindowStart.IsZero() {
			snapshot.HourlyResetAt = h.codexAutoRefill.hourlyWindowStart.Add(time.Hour)
		}
	}
	h.codexAutoRefillMu.Unlock()
	return snapshot
}

func formatCodexAutoRefillPoolMessage(pool codexAutoRefillPoolSnapshot) string {
	return fmt.Sprintf("ready=%d cooling=%d unavailable=%d disabled=%d total=%d", pool.ReadyCount, pool.CoolingCount, pool.UnavailableCount, pool.DisabledCount, pool.TotalCount)
}
