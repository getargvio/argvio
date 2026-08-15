package otlp

import "go.opentelemetry.io/collector/pdata/pcommon"

// extractAttrs converts a pcommon.Map into a native Go map, applying the
// Layer 1 structural bounds. It never guesses at unsupported OTel value
// kinds (Map/Bytes, or a Slice with non-string elements) — those are simply
// unrepresentable under our taxonomy (internal/allowlist's AttrType only
// covers string/int/bool/double/string_array), so the attribute is dropped
// and reported in dropped, same as any other Layer 2 rejection.
//
// tooMany is true if the map itself exceeds maxCount — the caller rejects
// the whole record in that case (there's no principled way to pick which
// attributes to keep from an oversized/abusive set).
func extractAttrs(m pcommon.Map, maxCount, maxKeyLen, maxValLen int) (kept map[string]any, dropped []string, tooMany bool) {
	if m.Len() > maxCount {
		return nil, nil, true
	}
	kept = make(map[string]any, m.Len())
	m.Range(func(k string, v pcommon.Value) bool {
		if len(k) > maxKeyLen {
			dropped = append(dropped, k)
			return true
		}
		switch v.Type() {
		case pcommon.ValueTypeStr:
			s := v.Str()
			if len(s) > maxValLen {
				dropped = append(dropped, k)
				return true
			}
			kept[k] = s
		case pcommon.ValueTypeInt:
			kept[k] = v.Int()
		case pcommon.ValueTypeDouble:
			kept[k] = v.Double()
		case pcommon.ValueTypeBool:
			kept[k] = v.Bool()
		case pcommon.ValueTypeSlice:
			raw := v.Slice().AsRaw()
			strs := make([]string, 0, len(raw))
			ok := true
			for _, item := range raw {
				s, isStr := item.(string)
				if !isStr || len(s) > maxValLen {
					ok = false
					break
				}
				strs = append(strs, s)
			}
			if !ok {
				dropped = append(dropped, k)
				return true
			}
			kept[k] = strs
		default:
			// ValueTypeEmpty, ValueTypeMap, ValueTypeBytes: no corresponding
			// allowlist.AttrType, so there is nothing valid to keep.
			dropped = append(dropped, k)
		}
		return true
	})
	return kept, dropped, false
}

// mergeAttrs layers record-scope attributes over resource-scope ones
// (record wins on key collision — e.g. a per-span override of a
// resource-level default) into a single map for allowlist/tier evaluation.
// The inputs are never mutated.
func mergeAttrs(resourceAttrs, recordAttrs map[string]any) map[string]any {
	merged := make(map[string]any, len(resourceAttrs)+len(recordAttrs))
	for k, v := range resourceAttrs {
		merged[k] = v
	}
	for k, v := range recordAttrs {
		merged[k] = v
	}
	return merged
}
