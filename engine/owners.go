package engine

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Owners returns the field managers that own path (a dot path, such as
// "spec.replicas") or anything under it on obj, other than skip: the writers
// that will set it again. Only the object itself counts; a status or scale
// subresource write is not a claim on the spec.
func Owners(obj *unstructured.Unstructured, path, skip string) []string {
	seen := map[string]bool{}
	var out []string
	for _, mf := range obj.GetManagedFields() {
		if mf.Subresource != "" || mf.Manager == skip || mf.Manager == "" || seen[mf.Manager] {
			continue
		}
		var fields map[string]any
		if err := json.NewDecoder(mf.FieldsV1.GetRawReader()).Decode(&fields); err != nil {
			continue
		}
		if owns(fields, strings.Split(path, ".")) {
			seen[mf.Manager] = true
			out = append(out, mf.Manager)
		}
	}
	sort.Strings(out)
	return out
}

// owns walks managedFields' "f:<field>" keys down path.
func owns(fields map[string]any, path []string) bool {
	if len(path) == 0 {
		return true
	}
	next, ok := fields["f:"+path[0]].(map[string]any)
	if !ok {
		return false
	}
	return owns(next, path[1:])
}

// OwnerTally counts, per path, the objects each other manager owns it on.
type OwnerTally map[string]map[string]int

// Add counts obj's owners of path.
func (t OwnerTally) Add(obj *unstructured.Unstructured, path, skip string) {
	for _, m := range Owners(obj, path, skip) {
		if t[path] == nil {
			t[path] = map[string]int{}
		}
		t[path][m]++
	}
}

// Each calls fn for every path and manager, sorted, with the object count.
func (t OwnerTally) Each(fn func(path, manager string, objects int)) {
	paths := make([]string, 0, len(t))
	for p := range t {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		managers := make([]string, 0, len(t[p]))
		for m := range t[p] {
			managers = append(managers, m)
		}
		sort.Strings(managers)
		for _, m := range managers {
			fn(p, m, t[p][m])
		}
	}
}

// Objects is "1 object" or "n objects".
func Objects(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
