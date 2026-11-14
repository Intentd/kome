package field

import (
	"reflect"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/intentd/kome/engine"
	"github.com/intentd/tools/structured"
)

// record decodes obj's field record; empty when there is none. A record
// that cannot be decoded fails the object rather than being overwritten.
func (mg migrator) record(obj *unstructured.Unstructured) (map[string]any, error) {
	out := map[string]any{}
	if raw := obj.GetAnnotations()[mg.Annotations.FieldSnapshot]; raw != "" {
		if err := engine.DecodeRecord(raw, &out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// parseEntry decodes a record entry into its before and after maps.
func parseEntry(raw any) (before, after map[string]any, ok bool) {
	m, isMap := raw.(map[string]any)
	if !isMap {
		return nil, nil, false
	}
	before, bOK := m["before"].(map[string]any)
	after, aOK := m["after"].(map[string]any)
	return before, after, bOK && aOK
}

// drifted reports whether a value the migration wrote changed or went
// missing. Paths it cleared (nil) are expected to be absent.
func drifted(spec, after map[string]any) bool {
	for path, want := range after {
		if want == nil {
			continue
		}
		have, found := structured.GetNestedPath(spec, path)
		// JSON-normalise: the API returns int64 where decoded JSON has float64.
		if !found || !reflect.DeepEqual(structured.JSONNormalize(have), structured.JSONNormalize(want)) {
			return true
		}
	}
	return false
}
