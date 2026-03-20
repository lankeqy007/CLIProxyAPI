package management

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// Quota exceeded toggles
func (h *Handler) GetSwitchProject(c *gin.Context) {
	c.JSON(200, gin.H{"switch-project": h.cfg.QuotaExceeded.SwitchProject})
}
func (h *Handler) PutSwitchProject(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.QuotaExceeded.SwitchProject = v })
}

func (h *Handler) GetSwitchPreviewModel(c *gin.Context) {
	c.JSON(200, gin.H{"switch-preview-model": h.cfg.QuotaExceeded.SwitchPreviewModel})
}
func (h *Handler) PutSwitchPreviewModel(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.QuotaExceeded.SwitchPreviewModel = v })
}

func (h *Handler) GetCodexAutoRefill(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"codex-auto-refill": h.cfg.QuotaExceeded.CodexAutoRefill})
}

func (h *Handler) PutCodexAutoRefill(c *gin.Context) {
	var body config.CodexAutoRefill
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	h.mu.Lock()
	h.cfg.QuotaExceeded.CodexAutoRefill = body
	h.cfg.SanitizeCodexAutoRefill()
	h.mu.Unlock()
	h.persist(c)
}

func (h *Handler) GetCodexAutoRefillStatus(c *gin.Context) {
	now := time.Now()
	runtimeCfg := h.currentCodexAutoRefillRuntimeConfig()
	providerQuota := h.currentCodexAutoRefillProviderQuotaSnapshot()
	if runtimeCfg.Enable {
		providerQuota = h.codexAutoRefillProviderQuotaStatus(c.Request.Context(), runtimeCfg, now, false)
	}
	status := h.codexAutoRefillStatusSnapshot(runtimeCfg, h.codexAutoRefillPool(now), providerQuota, now)
	c.JSON(http.StatusOK, gin.H{"status": status})
}

func (h *Handler) TriggerCodexAutoRefill(c *gin.Context) {
	runtimeCfg := h.currentCodexAutoRefillRuntimeConfig()
	if !runtimeCfg.Enable || runtimeCfg.AuthDir == "" {
		c.JSON(http.StatusConflict, gin.H{"error": "codex auto-refill is disabled"})
		return
	}
	go h.runCodexAutoRefillWithTrigger("manual")
	c.JSON(http.StatusAccepted, gin.H{"status": "accepted"})
}
