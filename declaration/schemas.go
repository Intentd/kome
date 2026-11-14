package declaration

import (
	"fmt"
	"sort"
	"strings"

	"github.com/intentd/tools/kube"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// checkSchemas runs the checks that read the schema files: what the
// destination can hold, which paths each schema declares, the declared
// transitional schema, and the objects. A side without a schema file is
// checked against the cluster instead, before up.
func (f *File) checkSchemas(c checker, m Migration, from, to Identity) {
	fromSpec, fromOK := f.specSchema(c, "from", m.From.Schema)
	toSpec, toOK := f.specSchema(c, "to", m.To.Schema)

	switch m.Level() {
	case LevelKind:
		if fromOK && toOK {
			if problems := kube.CompareSchemas(fromSpec, toSpec); len(problems) > 0 {
				c.err("to", "%s cannot hold every source field:\n  spec.%s", to.GVK.Kind, strings.Join(problems, "\n  spec."))
			}
		}
	case LevelField:
		f.checkFieldPaths(c, m, fromSpec, fromOK, toSpec, toOK)
		if m.Transitional != nil {
			f.checkTransitional(c, m, fromSpec, fromOK, toSpec, toOK)
		}
	}

	f.checkObjects(c, "from", m.From, from)
	f.checkObjects(c, "to", m.To.Shape, to)
}

// specSchema reads the spec schema of a schema file; ok is false without
// one.
func (f *File) specSchema(c checker, side, file string) (*apiextensions.JSONSchemaProps, bool) {
	if file == "" {
		return nil, false
	}
	spec, err := kube.ReadSchemaFromCRDFile(f.Path(file))
	if err != nil {
		c.err(side, "schema %s: %v", file, err)
		return nil, false
	}
	if spec == nil {
		return nil, false // no schema in the file: nothing to check against
	}
	return spec, true
}

func (f *File) checkFieldPaths(c checker, m Migration, fromSpec *apiextensions.JSONSchemaProps, fromOK bool, toSpec *apiextensions.JSONSchemaProps, toOK bool) {
	var sources []string
	for _, key := range sortedKeys(m.Spec) {
		fl := m.Spec[key]
		at := "spec." + key
		src := fl.SourcePath(key)
		sources = append(sources, src)
		if fromOK {
			if _, found := kube.SchemaTypeAt(fromSpec, src); !found {
				c.err(at, "from path spec.%s is not in %s", src, m.From.Schema)
			}
		}
		if toOK {
			for _, dst := range fl.Destinations(key) {
				if _, found := kube.SchemaTypeAt(toSpec, dst); !found {
					c.err(at, "to path spec.%s is not in %s", dst, m.To.Schema)
				}
			}
		}
	}
	if fromOK && toOK {
		var dropped []string
		for _, problem := range kube.CompareSchemas(fromSpec, toSpec) {
			path, _, _ := strings.Cut(problem, ":")
			if !covered(path, sources) {
				dropped = append(dropped, "spec."+problem)
			}
		}
		if len(dropped) > 0 {
			c.err("spec", "%s drops fields no move covers; add a move for each, or they are lost:\n  %s", m.To.Schema, strings.Join(dropped, "\n  "))
		}
	}
}

// checkTransitional checks a declared transitional schema: it holds every
// from path with from's type, every to path with to's type, and is at to's
// storage version.
func (f *File) checkTransitional(c checker, m Migration, fromSpec *apiextensions.JSONSchemaProps, fromOK bool, toSpec *apiextensions.JSONSchemaProps, toOK bool) {
	file := m.Transitional.Schema
	if file == "" {
		c.err("transitional.schema", "is required when transitional is declared; leave transitional out to have it generated")
		return
	}
	tSpec, ok := f.specSchema(c, "transitional", file)
	if !ok {
		return
	}
	check := func(path string, want *apiextensions.JSONSchemaProps, wantOK bool, side string) {
		got, found := kube.SchemaTypeAt(tSpec, path)
		if !found {
			c.err("transitional", "%s does not hold %s path spec.%s", file, side, path)
			return
		}
		if !wantOK || got == "" {
			return
		}
		if typ, found := kube.SchemaTypeAt(want, path); found && typ != "" && !kube.TypeFits(typ, got) {
			c.err("transitional", "spec.%s is %s in %s but %s in the %s schema", path, got, file, typ, side)
		}
	}
	for _, key := range sortedKeys(m.Spec) {
		fl := m.Spec[key]
		check(fl.SourcePath(key), fromSpec, fromOK, "from")
		for _, dst := range fl.Destinations(key) {
			check(dst, toSpec, toOK, "to")
		}
	}
	if m.To.Schema != "" {
		th, err1 := kube.ReadCRDFileHeader(f.Path(file))
		toh, err2 := kube.ReadCRDFileHeader(f.Path(m.To.Schema))
		if err1 == nil && err2 == nil && th.Version() != toh.Version() {
			c.err("transitional", "%s stores %s but %s stores %s: they must be the same version", file, th.Version(), m.To.Schema, toh.Version())
		}
	}
}

// checkObjects checks each object file of a shape: its objects are the
// shape's kind, hold only fields its schema declares, and appear once.
func (f *File) checkObjects(c checker, side string, s Shape, id Identity) {
	seen := map[string]string{}
	for _, file := range s.Objects {
		objs, err := kube.ReadObjects(f.Path(file))
		if err != nil {
			c.err(side+".objects", "%s: %v", file, err)
			continue
		}
		for i, obj := range objs {
			name, ns := metaString(obj, "name"), metaString(obj, "namespace")
			ref := fmt.Sprintf("%s object %d", file, i+1)
			if name != "" {
				ref = fmt.Sprintf("%s %s", file, strings.TrimPrefix(ns+"/"+name, "/"))
			}
			gv, _ := schema.ParseGroupVersion(fmt.Sprint(obj["apiVersion"]))
			if kind := fmt.Sprint(obj["kind"]); id.GVK.Kind != "" && (kind != id.GVK.Kind || gv.Group != id.GVK.Group) {
				c.err(side+".objects", "%s is %s %s, not %s", ref, obj["apiVersion"], kind, id.GVK.GroupKind())
				continue
			}
			if name == "" {
				c.err(side+".objects", "%s has no metadata.name", ref)
			} else if prev, dup := seen[ns+"/"+name]; dup {
				c.err(side+".objects", "%s is also in %s", ref, prev)
			} else {
				seen[ns+"/"+name] = file
			}
			if s.Schema == "" {
				continue
			}
			spec, _ := obj["spec"].(map[string]any)
			unknown, err := kube.UnknownSpecFields(f.Path(s.Schema), spec, nil)
			if err != nil {
				c.err(side+".objects", "%s: %v", ref, err)
			} else if len(unknown) > 0 {
				c.err(side+".objects", "%s has fields %s does not declare: %s", ref, s.Schema, strings.Join(unknown, ", "))
			}
		}
	}
}

func metaString(obj map[string]any, key string) string {
	meta, _ := obj["metadata"].(map[string]any)
	s, _ := meta[key].(string)
	return s
}

// covered reports whether path is a source path, or under or above one.
func covered(path string, sources []string) bool {
	for _, src := range sources {
		if path == src || strings.HasPrefix(path, src+".") || strings.HasPrefix(src, path+".") {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]Field) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
