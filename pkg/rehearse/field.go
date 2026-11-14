package rehearse

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/intentd/kome"
	"github.com/intentd/kome/pkg/engine"
	"github.com/intentd/kome/pkg/field"
	"github.com/intentd/tools/fileio"
	"github.com/intentd/tools/structured"
)

// field rehearses a field migration:
//
//  1. apply from.schema, seed from.objects;
//  2. up refuses: the live schema lacks the new paths;
//  3. apply the transitional schema, seed to.objects;
//  4. up --dry-run: every source would migrate, nothing changes;
//  5. up: sources hold their values at the new paths, to.objects unchanged;
//  6. edit one migrated field, down: it is drifted and kept, the rest restored;
//  7. up again: everything is back in the new shape;
//  8. apply to.schema: every object reads back, nothing dropped;
//  9. clean: the records are gone, and down has nothing to restore.
func (mr *migrationRun) field(ctx context.Context) error {
	d := mr.decl()
	from, err := mr.objects(d.From)
	if err != nil {
		return mr.check(1, "seed the source objects", err, "")
	}
	to, err := mr.objects(d.To.Shape)
	if err != nil {
		return mr.check(3, "seed the destination objects", err, "")
	}
	moves := mr.m.Field.Moves

	// 1.
	err = mr.applySchema(ctx, d.From.Schema)
	if err == nil {
		err = mr.Cluster.Apply(ctx, from)
	}
	if err := mr.check(1, "apply from.schema and seed from.objects", err, fmt.Sprintf("%d object(s)", len(from))); err != nil {
		return err
	}
	seeded, err := mr.specs(ctx, from)
	if err != nil {
		return mr.check(1, "read the seeded objects", err, "")
	}

	// 2.
	_, err = mr.Engine.Up(ctx, mr.ms(), kome.Options{DryRun: true})
	detail := "up refuses until the transitional schema is applied"
	switch {
	case err == nil:
		detail, err = "from.schema already declares the new paths", nil
	case strings.Contains(err.Error(), "apply the transitional schema first"):
		err = nil
	}
	if err := mr.check(2, "up without the transitional schema", err, detail); err != nil {
		return err
	}

	// 3.
	t, err := mr.transitional()
	if err == nil {
		err = mr.Cluster.ApplyCRD(ctx, t)
	}
	if err == nil {
		err = mr.Cluster.Apply(ctx, to)
	}
	if err := mr.check(3, "apply the transitional schema and seed to.objects", err, ""); err != nil {
		return err
	}
	all := append(append([]*unstructured.Unstructured{}, from...), to...)
	before, err := mr.specs(ctx, all)
	if err != nil {
		return mr.check(3, "read the objects", err, "")
	}

	// 4.
	res, err := mr.Engine.Up(ctx, mr.ms(), kome.Options{DryRun: true})
	// From here on the sources are the objects the selection takes, less
	// those with a known issue; the others must be left as they are.
	var unselected []*unstructured.Unstructured
	if err == nil {
		mr.absorb(4, res)
		from, unselected = selected(res, from)
		from = mr.keep(from)
		err = migratesAll(res, from)
	}
	if err == nil {
		after, rerr := mr.specs(ctx, all)
		if err = rerr; err == nil {
			err = unchanged(before, after)
		}
	}
	if err := mr.check(4, "up --dry-run", err, fmt.Sprintf("%d would migrate, %d not selected", len(from), len(unselected))); err != nil {
		return err
	}

	// 5.
	res, err = mr.Engine.Up(ctx, mr.ms(), kome.Options{})
	if err == nil {
		mr.absorb(5, res)
		from = mr.keep(from)
		err = mr.inNewShape(ctx, from, moves, seeded)
	}
	if err == nil {
		// to.objects and objects outside the selection are not touched.
		keep := append(append([]*unstructured.Unstructured{}, to...), unselected...)
		var after map[string]map[string]any
		if after, err = mr.specs(ctx, keep); err == nil {
			err = unchanged(pick(before, keep), after)
		}
	}
	if err := mr.check(5, "up", err, ""); err != nil {
		return err
	}

	// 6.
	err = mr.editAndDown(ctx, from, moves, seeded)
	if err := mr.check(6, "edit one field, then down", err, "the edit is drifted and kept, the rest restored"); err != nil {
		return err
	}

	// 7.
	res, err = mr.Engine.Up(ctx, mr.ms(), kome.Options{})
	if err == nil {
		mr.absorb(7, res)
		from = mr.keep(from)
		err = mr.inNewShape(ctx, from, moves, seeded)
	}
	if err := mr.check(7, "up again", err, ""); err != nil {
		return err
	}

	// 8. An object with a known issue was never migrated, so to.schema drops
	// its old fields: that is the issue already reported, not checked again.
	checked := append(append(append([]*unstructured.Unstructured{}, from...), unselected...), to...)
	migrated, err := mr.specs(ctx, checked)
	if err == nil {
		err = mr.applySchema(ctx, d.To.Schema)
	}
	if err == nil {
		final, rerr := mr.specs(ctx, checked)
		if err = rerr; err == nil {
			err = nothingDropped(migrated, final)
		}
	}
	if err := mr.check(8, "apply to.schema", err, "nothing dropped"); err != nil {
		return err
	}

	// 9.
	err = mr.cleanThenDown(ctx, from)
	return mr.check(9, "clean, then down", err, "")
}

func (mr *migrationRun) applySchema(ctx context.Context, file string) error {
	data, err := mr.schema(file)
	if err != nil {
		return err
	}
	return mr.Cluster.ApplyCRD(ctx, data)
}

// transitional is the declared transitional schema, or the generated one.
func (mr *migrationRun) transitional() ([]byte, error) {
	if t := mr.decl().Transitional; t != nil {
		return fileio.ReadLocal(mr.File.Path(t.Schema))
	}
	return kome.Transitional(mr.File, mr.m.Name)
}

// migratesAll checks every source would migrate, or is excluded.
func migratesAll(res []*engine.Result, from []*unstructured.Unstructured) error {
	got := objectsOf(res)
	var missing []string
	for _, o := range from {
		r := got[engine.Ref(o.GetNamespace(), o.GetName())]
		if len(r.Fields) == 0 && !r.Excluded {
			missing = append(missing, ref(o))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("not migrated: %s", strings.Join(missing, ", "))
	}
	return nil
}

// inNewShape checks each source holds the new paths of every move whose old
// value it was seeded with, and no longer holds the old path unless the move
// writes it. A move whose value an object never had leaves it alone.
func (mr *migrationRun) inNewShape(ctx context.Context, from []*unstructured.Unstructured, moves []field.Move, seeded map[string]map[string]any) error {
	specs, err := mr.specs(ctx, from)
	if err != nil {
		return err
	}
	var problems []string
	for _, o := range from {
		s := specs[ref(o)]
		for _, m := range moves {
			if _, had := structured.GetNestedPath(seeded[ref(o)], m.From); !had {
				continue
			}
			for _, dst := range m.Destinations() {
				if _, ok := structured.GetNestedPath(s, dst); !ok {
					problems = append(problems, fmt.Sprintf("%s lacks spec.%s", ref(o), dst))
				}
			}
			if !writes(m) {
				if _, ok := structured.GetNestedPath(s, m.From); ok {
					problems = append(problems, fmt.Sprintf("%s still has spec.%s", ref(o), m.From))
				}
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return nil
}

// writes reports whether m writes its old path, or below it.
func writes(m field.Move) bool {
	for _, d := range m.Destinations() {
		if d == m.From || strings.HasPrefix(d, m.From+".") {
			return true
		}
	}
	return false
}

// editAndDown edits one migrated field of the first source, runs down, and
// checks the edit is drifted and kept while the other sources are restored.
func (mr *migrationRun) editAndDown(ctx context.Context, from []*unstructured.Unstructured, moves []field.Move, seeded map[string]map[string]any) error {
	if len(moves) == 0 {
		return nil
	}
	// The first object seeded with the first move's old value.
	m := moves[0]
	var target *unstructured.Unstructured
	var others []*unstructured.Unstructured
	for _, o := range from {
		if _, had := structured.GetNestedPath(seeded[ref(o)], m.From); had && target == nil {
			target = o
			continue
		}
		others = append(others, o)
	}
	if target == nil {
		return nil
	}
	dst := m.Destinations()[0]
	live, err := mr.Cluster.Get(ctx, target)
	if err != nil || live == nil {
		return fmt.Errorf("read %s: %v", ref(target), err)
	}
	path := append([]string{"spec"}, strings.Split(dst, ".")...)
	v, _, _ := unstructured.NestedFieldNoCopy(live.Object, path...)
	if err := unstructured.SetNestedField(live.Object, edited(v, mr.liveSchema(ctx, mr.m.Field.GVK, dst)), path...); err != nil {
		return err
	}
	if err := mr.Cluster.Update(ctx, live); err != nil {
		return fmt.Errorf("edit %s: %w", ref(target), err)
	}

	res, err := mr.Engine.Down(ctx, mr.ms(), kome.Options{})
	if err != nil {
		return err
	}
	mr.absorb(6, res)
	others = mr.keep(others)
	got := objectsOf(res)
	r := got[engine.Ref(target.GetNamespace(), target.GetName())]
	if r.Err == nil && !contains(r.DriftedFields, m.Key) {
		return fmt.Errorf("%s: %s was edited but not reported drifted", ref(target), m.Key)
	}
	specs, err := mr.specs(ctx, others)
	if err != nil {
		return err
	}
	for _, o := range others {
		want, _ := structured.GetNestedPath(seeded[ref(o)], m.From)
		have, _ := structured.GetNestedPath(specs[ref(o)], m.From)
		if !reflect.DeepEqual(structured.JSONNormalize(want), structured.JSONNormalize(have)) {
			return fmt.Errorf("%s: spec.%s is %v after down, want %v", ref(o), m.From, have, want)
		}
	}
	return nil
}

// cleanThenDown runs clean, checks no record is left, and that down then
// has nothing to restore.
func (mr *migrationRun) cleanThenDown(ctx context.Context, objs []*unstructured.Unstructured) error {
	if _, err := mr.Engine.Clean(ctx, mr.ms(), kome.Options{}); err != nil {
		return err
	}
	for _, o := range objs {
		live, err := mr.Cluster.Get(ctx, o)
		if err != nil || live == nil {
			continue
		}
		for _, key := range []string{mr.ann.FieldSnapshot, mr.ann.KindSnapshot} {
			if _, ok := live.GetAnnotations()[key]; ok {
				return fmt.Errorf("%s still carries %s", ref(o), key)
			}
		}
	}
	res, err := mr.Engine.Down(ctx, mr.ms(), kome.Options{})
	if err != nil {
		return err
	}
	for _, o := range objectsOf(res) {
		if len(o.Fields) > 0 {
			return fmt.Errorf("down restored %s after clean", o.Ref())
		}
	}
	return nil
}

// nothingDropped checks every path before is still in after.
func nothingDropped(before, after map[string]map[string]any) error {
	var dropped []string
	for k, b := range before {
		for _, p := range structured.MissingPaths(b, after[k]) {
			dropped = append(dropped, k+" spec."+p)
		}
	}
	if len(dropped) > 0 {
		return fmt.Errorf("dropped: %s", strings.Join(dropped, ", "))
	}
	return nil
}

func pick(all map[string]map[string]any, objs []*unstructured.Unstructured) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, o := range objs {
		out[ref(o)] = all[ref(o)]
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
