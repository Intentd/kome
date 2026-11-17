package field

import (
	"reflect"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/intentd/kome/engine"
	"github.com/intentd/tools/structured"
)

// record decodes obj's field record: every field migration's entries, keyed
// by migration name, then by the moved path. mine is this migration's,
// empty when it has none. Keying by migration lets two migrations move the
// same path (A → B, then B → C) and be undone one at a time. A record that
// cannot be decoded fails the object rather than being overwritten.
func (mg migrator) record(obj *unstructured.Unstructured) (all, mine map[string]any, err error) {
	all = map[string]any{}
	if raw := obj.GetAnnotations()[mg.Annotations.FieldSnapshot]; raw != "" {
		if err := engine.DecodeRecord(raw, &all); err != nil {
			return nil, nil, err
		}
	}
	mine, _ = all[mg.name].(map[string]any)
	if mine == nil {
		mine = map[string]any{}
	}
	return all, mine, nil
}

// withMine is all with this migration's entries set to mine, or removed when
// mine is empty.
func (mg migrator) withMine(all, mine map[string]any) map[string]any {
	out := make(map[string]any, len(all)+1)
	for k, v := range all {
		out[k] = v
	}
	if len(mine) == 0 {
		delete(out, mg.name)
	} else {
		out[mg.name] = mine
	}
	return out
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
