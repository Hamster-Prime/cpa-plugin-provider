package executor

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const maxErrorBodyBytes = 1 << 20

const redactedSensitiveValue = "[REDACTED]"

type statusError struct {
	statusCode    int
	body          []byte
	requestScoped bool
}

func (e statusError) Error() string {
	if message := upstreamErrorMessage(e.body); message != "" {
		return message
	}
	return fmt.Sprintf("upstream request failed with status %d", e.statusCode)
}

func (e statusError) StatusCode() int { return e.statusCode }

func (e statusError) IsRequestScoped() bool {
	return e.requestScoped
}

func newRequestScopedStatusError(statusCode int, body []byte) statusError {
	return statusError{statusCode: statusCode, body: boundedBody(body), requestScoped: true}
}

func newUpstreamStatusError(statusCode int, body []byte, sensitiveValues ...string) statusError {
	rawBody := boundedBody(body)
	exposedBody := []byte(nil)
	if len(body) < maxErrorBodyBytes {
		exposedBody = redactSensitiveValues(rawBody, sensitiveValues...)
	}
	return statusError{
		statusCode:    statusCode,
		body:          exposedBody,
		requestScoped: upstreamRequestScoped(statusCode, rawBody),
	}
}

func boundedBody(body []byte) []byte {
	if len(body) <= maxErrorBodyBytes {
		return append([]byte(nil), body...)
	}
	return append([]byte(nil), body[:maxErrorBodyBytes]...)
}

func redactSensitiveValues(body []byte, sensitiveValues ...string) []byte {
	values := make([]string, 0, len(sensitiveValues)*2)
	seen := make(map[string]struct{}, len(sensitiveValues)*2)
	addValue := func(value string) {
		if value == "" {
			return
		}
		if _, exists := seen[value]; exists {
			return
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	for _, value := range sensitiveValues {
		addValue(value)
		if encoded, err := json.Marshal(value); err == nil && len(encoded) >= 2 {
			addValue(string(encoded[1 : len(encoded)-1]))
		}
	}
	if len(values) == 0 {
		return append([]byte(nil), body...)
	}
	sort.Slice(values, func(i, j int) bool {
		return len(values[i]) > len(values[j])
	})
	replacements := make([]string, 0, len(values)*2)
	for _, value := range values {
		replacements = append(replacements, value, redactedSensitiveValue)
	}
	return []byte(strings.NewReplacer(replacements...).Replace(string(body)))
}

func upstreamErrorMessage(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return ""
	}
	var decoded struct {
		Message string          `json:"message"`
		Error   json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal([]byte(trimmed), &decoded); err == nil {
		if len(decoded.Error) > 0 {
			var object struct {
				Message string `json:"message"`
			}
			if errObject := json.Unmarshal(decoded.Error, &object); errObject == nil {
				if message := strings.TrimSpace(object.Message); message != "" {
					return message
				}
			}
			var message string
			if errString := json.Unmarshal(decoded.Error, &message); errString == nil {
				if message = strings.TrimSpace(message); message != "" {
					return message
				}
			}
		}
		if message := strings.TrimSpace(decoded.Message); message != "" {
			return message
		}
	}
	return trimmed
}

func upstreamRequestScoped(statusCode int, body []byte) bool {
	if statusCode != 400 {
		return false
	}
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return false
	}
	markers := make([]string, 0, 8)
	collectErrorMarkers(payload, &markers)
	raw := strings.ToLower(string(body))
	if hasAnyMarker(markers, []string{
		"authentication_error",
		"invalid_api_key",
		"unauthenticated",
		"permission_denied",
		"resource_exhausted",
		"api_key_invalid",
		"api_key_expired",
		"rate_limit_error",
		"rate_limit_exceeded",
		"quota_exceeded",
		"insufficient_quota",
		"service_disabled",
		"invalid_grant",
	}) || containsAny(raw, []string{
		"invalid api key",
		"api key invalid",
		"api key expired",
		"rate limit",
		"quota exceeded",
		"not available for your plan",
		"not available for your account",
	}) {
		return false
	}
	if hasAnyMarker(markers, []string{"model_not_supported", "model_not_found"}) ||
		containsAny(raw, []string{
			"requested model is not supported",
			"requested model is unsupported",
			"requested model is unavailable",
			"model is not supported",
			"model not supported",
			"unsupported model",
			"model unavailable",
		}) ||
		(strings.Contains(raw, "model") && (strings.Contains(raw, "model not found") || strings.Contains(raw, "model does not exist"))) {
		return false
	}
	return hasAnyMarker(markers, []string{
		"invalid_request_error",
		"bad_request_error",
		"invalid_argument",
		"failed_precondition",
	})
}

func collectErrorMarkers(value any, markers *[]string) {
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "type", "code", "status", "reason":
				if marker, ok := nested.(string); ok {
					*markers = append(*markers, strings.ToLower(strings.TrimSpace(marker)))
				}
			}
			collectErrorMarkers(nested, markers)
		}
	case []any:
		for _, nested := range typed {
			collectErrorMarkers(nested, markers)
		}
	}
}

func hasAnyMarker(markers, candidates []string) bool {
	for _, marker := range markers {
		for _, candidate := range candidates {
			if marker == candidate {
				return true
			}
		}
	}
	return false
}

func containsAny(value string, candidates []string) bool {
	for _, candidate := range candidates {
		if strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}
