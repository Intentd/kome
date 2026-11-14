package declaration

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/intentd/tools/kube"
)

// Identity is which resource a shape is: read from its schema file, or
// declared.
type Identity struct {
	GVK    schema.GroupVersionKind
	Plural string
	// Namespaced is known only from a schema file; a declared identity's
	// scope is read from the cluster.
	Namespaced bool
	// FromFile is true when the identity was read from a schema file.
	FromFile bool
	// PluralInferred is true when the plural was guessed from the kind.
	PluralInferred bool
}

// GVR returns the identity's resource.
func (id Identity) GVR() schema.GroupVersionResource {
	return id.GVK.GroupVersion().WithResource(id.Plural)
}

// SameKind reports whether a and b are the same kind: group and kind, any
// version.
func (id Identity) SameKind(other Identity) bool {
	return id.GVK.Group == other.GVK.Group && id.GVK.Kind == other.GVK.Kind
}

// Identify reads s's identity: from its schema file when it has one, else
// from its apiVersion, kind and plural.
func (f *File) Identify(s Shape) (Identity, error) {
	if s.Schema != "" {
		h, err := kube.ReadCRDFileHeader(f.Path(s.Schema))
		if err != nil {
			return Identity{}, fmt.Errorf("schema %s: %w", s.Schema, err)
		}
		namespaced, _ := h.Namespaced()
		return Identity{
			GVK:        schema.GroupVersionKind{Group: h.Spec.Group, Version: h.Version(), Kind: h.Spec.Names.Kind},
			Plural:     h.Spec.Names.Plural,
			Namespaced: namespaced,
			FromFile:   true,
		}, nil
	}
	if s.APIVersion == "" || s.Kind == "" {
		return Identity{}, fmt.Errorf("needs a schema, or apiVersion and kind")
	}
	gvk, err := kube.GVKFromFields(s.APIVersion, s.Kind)
	if err != nil {
		return Identity{}, err
	}
	id := Identity{GVK: gvk, Plural: s.Plural}
	if id.Plural == "" {
		id.Plural, id.PluralInferred = kube.PluralForKind(s.Kind), true
	}
	return id, nil
}

// Level is how a migration moves objects.
type Level string

const (
	// LevelField keeps the kind; values move between paths.
	LevelField Level = "field"
	// LevelKind moves objects to another kind, group or scope.
	LevelKind Level = "kind"
)

// Level returns m's level: field when it declares spec moves, else kind.
// Validate checks that the identities agree with it.
func (m Migration) Level() Level {
	if len(m.Spec) > 0 {
		return LevelField
	}
	return LevelKind
}
