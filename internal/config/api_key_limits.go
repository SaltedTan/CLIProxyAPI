package config

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// normalizeAPIKeyLimits trims the client key entries of cfg.APIKeyLimits, drops
// empty keys and rejects negative, NaN or infinite allowances. Zero entries are
// kept because they mean "no limit" and the usage tracker ignores them.
func normalizeAPIKeyLimits(cfg *Config) error {
	if cfg == nil {
		return nil
	}
	cfg.APIKeyLimits = normalizeAPIKeyLimitKeys(cfg.APIKeyLimits)
	return validateAPIKeyLimits(cfg.APIKeyLimits)
}

// normalizeAPIKeyLimitKeys returns limits with trimmed keys and without empty
// keys. A nil or empty map stays nil so the field is omitted when saved. When
// two raw keys trim to the same key, the already trimmed spelling wins and ties
// are broken by sorted order so the result does not depend on map iteration.
func normalizeAPIKeyLimitKeys(limits map[string]float64) map[string]float64 {
	if len(limits) == 0 {
		return nil
	}
	rawKeys := make([]string, 0, len(limits))
	for key := range limits {
		rawKeys = append(rawKeys, key)
	}
	sort.Strings(rawKeys)
	out := make(map[string]float64, len(limits))
	for _, rawKey := range rawKeys {
		key := strings.TrimSpace(rawKey)
		if key == "" {
			continue
		}
		if _, exists := out[key]; exists && rawKey != key {
			continue
		}
		out[key] = limits[rawKey]
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// validateAPIKeyLimits rejects allowances that are negative, NaN or infinite.
// The error identifies the entry by a masked key so raw client keys never reach
// logs or management API responses.
func validateAPIKeyLimits(limits map[string]float64) error {
	keys := make([]string, 0, len(limits))
	for key := range limits {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := limits[key]
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return fmt.Errorf("api-key-limits entry %s: allowance must be a finite number >= 0, got %v", maskClientKey(key), value)
		}
	}
	return nil
}

// maskClientKey obscures a client API key for error messages. It follows the
// same shape as util.HideAPIKey, which this package cannot import.
func maskClientKey(key string) string {
	key = strings.TrimSpace(key)
	switch {
	case len(key) > 8:
		return key[:4] + "..." + key[len(key)-4:]
	case len(key) > 4:
		return key[:2] + "..." + key[len(key)-2:]
	case len(key) > 2:
		return key[:1] + "..." + key[len(key)-1:]
	default:
		return key
	}
}
