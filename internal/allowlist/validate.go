package allowlist

import "fmt"

// CheckAttribute validates that value conforms to def's declared type and
// bounds (max length, max items, enum membership). It operates on plain Go
// values (string, int64, bool, float64, []string) so this package stays free
// of any OTLP/pdata dependency — callers (internal/otlp) are responsible for
// converting pcommon.Value into these native forms first.
func CheckAttribute(def AttrDef, value any) error {
	switch def.Type {
	case AttrString:
		s, ok := value.(string)
		if !ok {
			return fmt.Errorf("expected string, got %T", value)
		}
		if def.MaxLength > 0 && len(s) > def.MaxLength {
			return fmt.Errorf("string exceeds max_length %d (got %d)", def.MaxLength, len(s))
		}
		if len(def.Enum) > 0 && !contains(def.Enum, s) {
			return fmt.Errorf("value %q not in allowed enum %v", s, def.Enum)
		}
		return nil
	case AttrInt:
		switch value.(type) {
		case int64, int:
			return nil
		default:
			return fmt.Errorf("expected int, got %T", value)
		}
	case AttrBool:
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("expected bool, got %T", value)
		}
		return nil
	case AttrDouble:
		switch value.(type) {
		case float64, float32:
			return nil
		default:
			return fmt.Errorf("expected double, got %T", value)
		}
	case AttrStringArray:
		arr, ok := value.([]string)
		if !ok {
			return fmt.Errorf("expected string_array, got %T", value)
		}
		if def.MaxItems > 0 && len(arr) > def.MaxItems {
			return fmt.Errorf("array exceeds max_items %d (got %d)", def.MaxItems, len(arr))
		}
		if def.MaxLength > 0 {
			for _, s := range arr {
				if len(s) > def.MaxLength {
					return fmt.Errorf("array item exceeds max_length %d (got %d)", def.MaxLength, len(s))
				}
			}
		}
		return nil
	default:
		return fmt.Errorf("attribute %q has unknown declared type %q", def.Key, def.Type)
	}
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// AllowedSet turns an allowed_attributes list into a set for O(1) membership
// checks.
func AllowedSet(keys []string) map[string]struct{} {
	set := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		set[k] = struct{}{}
	}
	return set
}
