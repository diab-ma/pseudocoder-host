package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// parseFlexibleTimestampSeconds accepts either legacy numeric Unix seconds or
// current RFC3339 string timestamps and normalizes them to Unix seconds.
func parseFlexibleTimestampSeconds(raw json.RawMessage) (float64, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return 0, nil
	}

	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return 0, err
		}
		text = strings.TrimSpace(text)
		if text == "" {
			return 0, nil
		}
		if ts, err := time.Parse(time.RFC3339Nano, text); err == nil {
			return float64(ts.UnixNano()) / float64(time.Second), nil
		}
		value, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return 0, fmt.Errorf("parse timestamp %q: %w", text, err)
		}
		return normalizeUnixSeconds(value), nil
	}

	value, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return 0, fmt.Errorf("parse numeric timestamp %q: %w", trimmed, err)
	}
	return normalizeUnixSeconds(value), nil
}

func normalizeUnixSeconds(value float64) float64 {
	if math.Abs(value) >= 1e12 {
		return value / 1000.0
	}
	return value
}

// parseTextContent extracts display text from either a plain JSON string or
// the newer array-of-parts Gemini message shape.
func parseTextContent(raw json.RawMessage) (string, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", nil
	}

	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return "", err
		}
		return text, nil
	}

	var payload any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", err
	}

	var parts []string
	collectTextContent(payload, &parts)
	return strings.Join(parts, "\n"), nil
}

func collectTextContent(value any, parts *[]string) {
	switch typed := value.(type) {
	case string:
		if typed != "" {
			*parts = append(*parts, typed)
		}
	case []any:
		for _, item := range typed {
			collectTextContent(item, parts)
		}
	case map[string]any:
		if text, ok := typed["text"].(string); ok && text != "" {
			*parts = append(*parts, text)
			return
		}
		if content, ok := typed["content"]; ok {
			collectTextContent(content, parts)
		}
	}
}

// compactJSONExcerpt returns a stable string representation for raw JSON data.
// Strings are unquoted; structured values are compacted JSON.
func compactJSONExcerpt(raw json.RawMessage) (string, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", nil
	}

	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return "", err
		}
		return text, nil
	}

	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return "", err
	}
	return buf.String(), nil
}
