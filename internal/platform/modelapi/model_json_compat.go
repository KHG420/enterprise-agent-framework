package modelapi

import (
	"bytes"
	"encoding/json"
	"log"
	"strings"

	agentcontract "github.com/KHG420/agenstra/internal/contract/agent"
)

// Empty tool arguments denote no parameters only when the current declared
// function's schema accepts an empty object. Required fields still fail.
func normalizeModelToolJSON(body []byte, schema agentcontract.JSON) ([]byte, bool) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		empty := []byte("{}")
		if validateNativeDecisionOutput(empty, schema) == "" {
			log.Printf("model JSON normalized: bytes=%d normalized_bytes=2 reason=empty_parameters", len(body))
			return empty, true
		}
		return body, false
	}
	return normalizeModelJSON(body)
}

// Normalize only formatting around a complete object. Every field, scalar and
// numeric representation then passes the original duplicate-key, schema and
// runtime checks. Incomplete values retain the original recovery path.
func normalizeModelJSON(body []byte) ([]byte, bool) {
	if json.Valid(body) {
		return body, false
	}
	candidate := bytes.Trim(body, " \t\r\n")
	reasons := []string{}
	if bytes.HasPrefix(candidate, []byte("\xef\xbb\xbf")) {
		candidate = bytes.TrimSpace(candidate[3:])
		reasons = append(reasons, "utf8_bom")
	}
	if unwrapped := unwrapModelJSON(candidate); len(unwrapped) != len(candidate) {
		candidate = unwrapped
		reasons = append(reasons, "markdown_fence")
	}
	if len(candidate) == 0 || candidate[0] != '{' {
		return body, false
	}
	if fixed, changed := normalizeModelJSONPunctuation(candidate); changed {
		candidate = fixed
		reasons = append(reasons, "json_punctuation")
	}
	if !json.Valid(candidate) {
		decoder := json.NewDecoder(bytes.NewReader(candidate))
		var object json.RawMessage
		if decoder.Decode(&object) != nil {
			return body, false
		}
		end := decoder.InputOffset()
		suffix := candidate[end:]
		if len(suffix) == 0 || len(suffix) > 32 || len(bytes.Trim(suffix, "}] \t\r\n")) != 0 {
			return body, false
		}
		candidate = candidate[:end]
		reasons = append(reasons, "extra_closing_delimiters")
	}
	log.Printf("model JSON normalized: bytes=%d normalized_bytes=%d reason=%s", len(body), len(candidate), strings.Join(reasons, ","))
	return candidate, true
}

// This single pass preserves quoted bytes and escapes. It removes a trailing
// comma only after a value and escapes literal control characters in strings.
// The full JSON parser determines whether the resulting object is complete.
func normalizeModelJSONPunctuation(body []byte) ([]byte, bool) {
	var output []byte
	var stack []byte
	redundant := 0
	inside, escaped := false, false
	last := byte(0)
	start := 0
	for i, current := range body {
		if inside {
			if escaped {
				escaped = false
				continue
			}
			switch current {
			case '\\':
				escaped = true
			case '"':
				inside = false
				last = '"'
			default:
				if current < 0x20 {
					if len(output)+i-start+6 > 8<<20 {
						return body, false
					}
					output = append(output, body[start:i]...)
					const hex = "0123456789abcdef"
					output = append(output, '\\', 'u', '0', '0', hex[current>>4], hex[current&15])
					start = i + 1
				}
			}
			continue
		}
		if current == '"' {
			inside = true
		} else if current == '{' || current == '[' {
			stack = append(stack, current)
		} else if (current == '}' || current == ']') && len(stack) > 0 {
			opening := stack[len(stack)-1]
			if opening == '{' && current == '}' || opening == '[' && current == ']' {
				stack = stack[:len(stack)-1]
			} else {
				redundant++
				if redundant > 32 {
					return body, false
				}
				output = append(output, body[start:i]...)
				start = i + 1
				continue
			}
		} else if current == ',' && strings.ContainsRune(`"}]0123456789el`, rune(last)) {
			next := i + 1
			for next < len(body) && strings.ContainsRune(" \t\r\n", rune(body[next])) {
				next++
			}
			if next < len(body) && (body[next] == '}' || body[next] == ']') {
				output = append(output, body[start:i]...)
				start = i + 1
			}
		}
		if !strings.ContainsRune(" \t\r\n", rune(current)) {
			last = current
		}
	}
	if output == nil && start == 0 {
		return body, false
	}
	if len(output)+len(body)-start > 8<<20 {
		return body, false
	}
	return append(output, body[start:]...), true
}
