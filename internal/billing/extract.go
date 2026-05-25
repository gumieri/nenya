package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type QuotaExtractionConfig struct {
	Mode string `json:"mode"`

	BalancePath string `json:"balance_path,omitempty"`

	ArrayPath     string `json:"array_path,omitempty"`
	ValueField    string `json:"value_field,omitempty"`
	ValueDivideBy int    `json:"value_divide_by,omitempty"`
	ResetField    string `json:"reset_field,omitempty"`
	ResetUnit     string `json:"reset_unit,omitempty"`
	LevelField    string `json:"level_field,omitempty"`

	RemainingHeader string `json:"remaining_header,omitempty"`
	LimitHeader     string `json:"limit_header,omitempty"`
	ResetHeader     string `json:"reset_header,omitempty"`
}

type QuotaInfo struct {
	BalanceUSD  float64
	LimitUSD    float64
	ResetAt     time.Time
	IsExhausted bool
	Level       string
}

func ExtractQuotaFromResponse(ctx context.Context, body []byte, config QuotaExtractionConfig, logger *slog.Logger) (*QuotaInfo, error) {
	if logger != nil {
		logger.DebugContext(ctx, "extracting quota from response", "body_length", len(body))
	}

	if len(body) == 0 {
		return nil, fmt.Errorf("empty response body")
	}

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	info := &QuotaInfo{}

	switch config.Mode {
	case "simple_json":
		if err := extractSimpleJSON(raw, config, info); err != nil {
			return nil, err
		}
	case "max_from_array":
		if err := extractMaxFromArray(raw, config, info); err != nil {
			return nil, err
		}
	case "headers":
		return nil, fmt.Errorf("headers mode requires HTTP response, not JSON body")
	default:
		return nil, fmt.Errorf("unknown quota extraction mode: %s", config.Mode)
	}

	determineExhausted(ctx, info, logger)
	return info, nil
}

func ExtractQuotaFromHeaders(ctx context.Context, headers http.Header, config QuotaExtractionConfig, logger *slog.Logger) (*QuotaInfo, error) {
	if logger != nil {
		logger.DebugContext(ctx, "extracting quota from headers")
	}

	if config.Mode != "headers" {
		return nil, fmt.Errorf("config mode is %q, expected 'headers'", config.Mode)
	}

	info := &QuotaInfo{}
	parseFloatHeader(&info.BalanceUSD, config.RemainingHeader, headers, ctx, logger)
	parseFloatHeader(&info.LimitUSD, config.LimitHeader, headers, ctx, logger)

	resetHeader := config.ResetHeader
	if resetHeader != "" {
		resetStr := headers.Get(resetHeader)
		if resetStr != "" {
			parseResetHeader(info, resetStr)
		}
	}

	determineExhausted(ctx, info, logger)
	return info, nil
}

func parseFloatHeader(field *float64, headerName string, headers http.Header, ctx context.Context, logger *slog.Logger) {
	if headerName == "" {
		return
	}
	headerVal := headers.Get(headerName)
	if headerVal == "" {
		return
	}
	val, err := parseHeaderValue(headerVal)
	if err != nil {
		if logger != nil {
			logger.WarnContext(ctx, "failed to parse header", "header", headerName, "value", headerVal, "error", err)
		}
		return
	}
	*field = val
}

func parseResetHeader(info *QuotaInfo, value string) {
	resetUnix, err := strconv.ParseInt(value, 10, 64)
	if err == nil {
		info.ResetAt = time.Unix(resetUnix, 0)
		return
	}
	t, err := time.Parse(time.RFC3339, value)
	if err == nil {
		info.ResetAt = t
	}
}

func extractSimpleJSON(raw map[string]any, config QuotaExtractionConfig, info *QuotaInfo) error {
	balancePath := strings.Split(config.BalancePath, ".")

	val, err := getNestedValue(raw, balancePath)
	if err != nil {
		return fmt.Errorf("failed to get balance at path %s: %w", config.BalancePath, err)
	}

	balance, ok := val.(float64)
	if !ok {
		return fmt.Errorf("balance value is not a number, got %T", val)
	}
	info.BalanceUSD = balance

	extractResetTime(info, raw, config.ResetField, config.ResetUnit)
	return nil
}

func extractMaxFromArray(raw map[string]any, config QuotaExtractionConfig, info *QuotaInfo) error {
	arrayPath := strings.Split(config.ArrayPath, ".")
	valuePath := strings.Split(config.ValueField, ".")

	val, err := getNestedValue(raw, arrayPath)
	if err != nil {
		return fmt.Errorf("failed to get array at path %s: %w", config.ArrayPath, err)
	}

	arr, ok := val.([]any)
	if !ok {
		return fmt.Errorf("value at path %s is not an array, got %T", config.ArrayPath, val)
	}

	if len(arr) == 0 {
		return fmt.Errorf("array at path %s is empty", config.ArrayPath)
	}

	maxBalance := 0.0
	for _, item := range arr {
		balance, ok := extractItemBalance(item, valuePath, config.ValueDivideBy)
		if !ok {
			continue
		}
		if balance > maxBalance {
			maxBalance = balance
			extractLevel(info, raw, config.LevelField)
			extractResetTime(info, raw, config.ResetField, config.ResetUnit)
		}
	}

	if maxBalance == 0 {
		return fmt.Errorf("no valid balance found in array")
	}

	info.BalanceUSD = maxBalance
	return nil
}

func extractLevel(info *QuotaInfo, raw map[string]any, levelField string) {
	if levelField == "" {
		return
	}
	levelPath := strings.Split(levelField, ".")
	if level, err := getNestedValue(raw, levelPath); err == nil {
		if l, ok := level.(string); ok && l != "" {
			info.Level = l
		}
	}
}

func extractResetTime(info *QuotaInfo, raw map[string]any, resetField, resetUnit string) {
	if resetField == "" {
		return
	}
	resetPath := strings.Split(resetField, ".")
	if resetVal, err := getNestedValue(raw, resetPath); err == nil {
		parseResetFromJSON(info, resetVal, resetUnit)
	}
}

func extractItemBalance(item any, valuePath []string, divideBy int) (float64, bool) {
	itemMap, ok := item.(map[string]any)
	if !ok {
		return 0, false
	}

	val, err := getNestedValue(itemMap, valuePath)
	if err != nil {
		return 0, false
	}

	balance, ok := val.(float64)
	if !ok {
		return 0, false
	}

	adjustedBalance := balance
	if divideBy > 0 {
		adjustedBalance = balance / float64(divideBy)
	}

	return adjustedBalance, true
}

func getNestedValue(raw map[string]any, path []string) (any, error) {
	current := any(raw)

	for i, key := range path {
		var next any
		var err error
		if idx := parseArrayIndex(key); idx >= 0 {
			next, err = getArrayElement(current, key, i, idx)
		} else {
			next, err = getMapElement(current, key, i)
		}
		if err != nil {
			return nil, err
		}
		current = next
	}

	return current, nil
}

func getArrayElement(current any, key string, depth, idx int) (any, error) {
	baseKey := key[:len(key)-len(fmt.Sprintf("[%d]", idx))]
	arr, err := getMapKey(current, baseKey)
	if err != nil {
		return nil, fmt.Errorf("path segment %q at depth %d: %w", key, depth, err)
	}
	arrVal, ok := arr.([]any)
	if !ok {
		return nil, fmt.Errorf("path segment %q at depth %d is not an array, got %T", key, depth, arr)
	}
	if idx >= len(arrVal) {
		return nil, fmt.Errorf("array index %d out of bounds (len=%d) at depth %d", idx, len(arrVal), depth)
	}
	return arrVal[idx], nil
}

func getMapElement(current any, key string, depth int) (any, error) {
	m, ok := current.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("path segment %q at depth %d is not a map", key, depth)
	}
	val, exists := m[key]
	if !exists {
		return nil, fmt.Errorf("key %q not found at depth %d", key, depth)
	}
	return val, nil
}

func parseArrayIndex(key string) int {
	start := strings.Index(key, "[")
	if start < 0 {
		return -1
	}
	end := strings.Index(key, "]")
	if end < 0 || end <= start+1 {
		return -1
	}
	idx, err := strconv.Atoi(key[start+1 : end])
	if err != nil {
		return -1
	}
	return idx
}

func getMapKey(current any, key string) (any, error) {
	m, ok := current.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("not a map")
	}
	val, exists := m[key]
	if !exists {
		return nil, fmt.Errorf("key %q not found", key)
	}
	return val, nil
}

func parseResetFromJSON(info *QuotaInfo, val any, unit string) {
	switch unit {
	case "unix_ms":
		if ms, ok := val.(float64); ok {
			info.ResetAt = time.UnixMicro(int64(ms))
		}
	case "unix_s":
		if s, ok := val.(float64); ok {
			info.ResetAt = time.Unix(int64(s), 0)
		}
	case "rfc3339":
		if str, ok := val.(string); ok {
			t, err := time.Parse(time.RFC3339, str)
			if err == nil {
				info.ResetAt = t
			}
		}
	}
}

func parseHeaderValue(value string) (float64, error) {
	parts := strings.Split(value, "/")
	if len(parts) > 1 {
		value = parts[0]
	}

	return strconv.ParseFloat(strings.TrimSpace(value), 64)
}

func determineExhausted(ctx context.Context, info *QuotaInfo, logger *slog.Logger) {
	if info.BalanceUSD <= 0 {
		info.IsExhausted = true
		return
	}

	if info.LimitUSD > 0 && info.BalanceUSD <= (info.LimitUSD*0.01) {
		info.IsExhausted = true
		if logger != nil {
			logger.WarnContext(ctx, "quota near exhausted (less than or equal to 1% of limit)",
				"balance_usd", info.BalanceUSD,
				"limit_usd", info.LimitUSD,
			)
		}
		return
	}

	info.IsExhausted = false
}

func (qc QuotaExtractionConfig) IsValid() bool {
	switch qc.Mode {
	case "simple_json", "max_from_array", "headers":
		return true
	default:
		return false
	}
}
