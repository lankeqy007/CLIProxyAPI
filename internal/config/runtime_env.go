package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

var managedContainerEnvKeys = []string{
	"SPACE_ID",
	"SPACE_HOST",
	"RENDER",
	"RENDER_SERVICE_ID",
	"RAILWAY_ENVIRONMENT",
	"FLY_APP_NAME",
	"KOYEB_SERVICE_ID",
}

// IsCloudDeployEnv reports whether the current process is running in cloud deploy mode.
// DEPLOY=cloud remains the explicit override and common container platforms are detected automatically.
func IsCloudDeployEnv() bool {
	if value, ok := lookupEnvAnyCase("DEPLOY"); ok && strings.EqualFold(value, "cloud") {
		return true
	}
	for _, key := range managedContainerEnvKeys {
		if _, ok := lookupEnvAnyCase(key); ok {
			return true
		}
	}
	return false
}

// ApplyRuntimeEnv overrides config values from runtime environment variables.
// This keeps a single image portable across container platforms that inject HOST/PORT at runtime.
func ApplyRuntimeEnv(cfg *Config) error {
	if cfg == nil {
		return nil
	}

	if value, ok := lookupEnvAnyCase("HOST", "APP_HOST", "CLI_PROXY_HOST"); ok {
		cfg.Host = value
	}

	if value, ok := lookupEnvAnyCase("PORT", "APP_PORT", "CLI_PROXY_PORT"); ok {
		port, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid runtime port %q: %w", value, err)
		}
		if port <= 0 || port > 65535 {
			return fmt.Errorf("runtime port %d is out of range", port)
		}
		cfg.Port = port
	}

	if value, ok := lookupEnvAnyCase("CLIENT_API_KEYS"); ok {
		cfg.APIKeys = mergeUniqueStrings(cfg.APIKeys, splitCommaSeparated(value))
	} else if value, ok := lookupEnvAnyCase("CLIENT_API_KEY"); ok {
		cfg.APIKeys = mergeUniqueStrings(cfg.APIKeys, []string{value})
	}

	return nil
}

func lookupEnvAnyCase(keys ...string) (string, bool) {
	for _, key := range keys {
		for _, candidate := range []string{key, strings.ToLower(key)} {
			if value, ok := os.LookupEnv(candidate); ok {
				if trimmed := strings.TrimSpace(value); trimmed != "" {
					return trimmed, true
				}
			}
		}
	}
	return "", false
}

func splitCommaSeparated(value string) []string {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		out = append(out, trimmed)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func mergeUniqueStrings(existing []string, additions []string) []string {
	if len(existing) == 0 && len(additions) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(existing)+len(additions))
	out := make([]string, 0, len(existing)+len(additions))
	for _, value := range existing {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	for _, value := range additions {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
