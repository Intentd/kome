package rehearse

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/intentd/kome"
	"github.com/intentd/kome/pkg/engine"
	"github.com/intentd/tools/structured"
)

// kind rehearses a kind migration:
//
//  1. apply both schemas, seed from.objects and to.objects;
//  2. up --dry-run: every source would migrate, nothing changes;
//  3. up (no prune): each source has a destination with the same spec;
//  4. edit one destination, down: it is drifted and kept, the rest restored;
//  5. up with prune: sources are gone, each destination records its source;
//  6. down: pruned sources are recreated, destinations not created kept;
//  7. up, clean: the records are gone, and down has nothing to restore.
func (mr *migrationRun) kind(ctx context.Context) error {
	d := mr.decl()
	from, err := mr.objects(d.From)
	if err != nil {
		return mr.check(1, "seed the objects", err, "")
	}
	to, err := mr.objects(d.To.Shape)
	if err != nil {
		return mr.check(1, "seed the objects", err, "")
	}
	noPrune, prune := mr.with(false), mr.with(true)

	// 1.
	err = mr.applySchema(ctx, d.From.Schema)
	if err == nil {
		err = mr.applySchema(ctx, d.To.Schema)
	}
	if err == nil {
		err = mr.Cluster.Apply(ctx, append(append([]*unstructured.Unstructured{}, from...), to...))
	}
	if err := mr.check(1, "apply both schemas and seed the objects", err, fmt.Sprintf("%d source(s), %d destination(s)", len(from), len(to))); err != nil {
		return err
	}
	all := append(append([]*unstructured.Unstructured{}, from...), to...)
	before, err := mr.specs(ctx, all)
	if err != nil {
		return mr.check(1, "read the objects", err, "")
	}
	existing := map[string]bool{}
	for _, o := range to {
		existing[engine.Ref(o.GetNamespace(), o.GetName())] = true
	}

	// 2.
	res, err := mr.Engine.Up(ctx, noPrune, kome.Options{DryRun: true})
	if err == nil {
		mr.absorb(2, res)
	}
	if err == nil {
		after, rerr := mr.specs(ctx, all)
		if err = rerr; err == nil {
			err = unchanged(before, after)
		}
	}
	// From here on the sources are the objects the selection takes, less
	// those with a known issue.
	var unselected []*unstructured.Unstructured
	if err == nil {
		from, unselected = selected(res, from)
		from = mr.keep(from)
	}
	if err := mr.check(2, "up --dry-run", err, fmt.Sprintf("%d would migrate, %d not selected", len(from), len(unselected))); err != nil {
		return err
	}

	// 3.
	res, err = mr.Engine.Up(ctx, noPrune, kome.Options{})
	if err == nil {
		mr.absorb(3, res)
		from = mr.keep(from)
	}
	var drifted map[string]bool
	if err == nil {
		drifted, err = mr.destinationsMatch(ctx, res, from)
	}
	if err == nil {
		// Sources outside the selection get no destination.
		for _, d := range mr.destinations(unselected) {
			if live, _ := mr.Cluster.Get(ctx, d); live != nil && !existing[engine.Ref(d.GetNamespace(), d.GetName())] {
				err = fmt.Errorf("%s was not selected but has a destination", ref(d))
				break
			}
		}
	}
	if err := mr.check(3, "up", err, fmt.Sprintf("%d drifted", len(drifted))); err != nil {
		return err
	}

	// 4.
	var edited string
	edited, err = mr.editDestinationAndDown(ctx, from, existing, drifted)
	if err := mr.check(4, "edit one destination, then down", err, edited); err != nil {
		return err
	}

	// 5.
	res, err = mr.Engine.Up(ctx, prune, kome.Options{})
	if err == nil {
		mr.absorb(5, res)
		from = mr.keep(from)
	}
	if err == nil {
		err = mr.prunedAndRecorded(ctx, res, from)
	}
	if err := mr.check(5, "up with prune", err, ""); err != nil {
		return err
	}

	// 6.
	res, err = mr.Engine.Down(ctx, prune, kome.Options{})
	if err == nil {
		mr.absorb(6, res)
		from = mr.keep(from)
	}
	if err == nil {
		err = mr.sourcesBack(ctx, from, existing)
	}
	if err := mr.check(6, "down after prune", err, ""); err != nil {
		return err
	}

	// 7.
	res, err = mr.Engine.Up(ctx, noPrune, kome.Options{})
	if err == nil {
		mr.absorb(7, res)
		from = mr.keep(from)
		dsts := mr.destinations(from)
		err = mr.cleanThenDown(ctx, dsts)
	}
	return mr.check(7, "up, clean, then down", err, "")
}

// with returns the migration with prune set.
func (mr *migrationRun) with(prune bool) kome.Migrations {
	m := mr.m
	m.Kind.Prune = prune
	return kome.Migrations{m}
}

// destinations returns each source's destination object (identity only),
// with no namespace when the destination is cluster-scoped.
func (mr *migrationRun) destinations(from []*unstructured.Unstructured) []*unstructured.Unstructured {
	clusterScoped := false
	if m, err := mr.Engine.Kube.Mapping(mr.m.Kind.To); err == nil {
		clusterScoped = m.Scope.Name() == meta.RESTScopeNameRoot
	}
	out := make([]*unstructured.Unstructured, 0, len(from))
	for _, src := range from {
		dst := &unstructured.Unstructured{Object: map[string]any{}}
		dst.SetGroupVersionKind(mr.m.Kind.To)
		dst.SetName(src.GetName())
		ns := src.GetNamespace()
		if ns == "" {
			ns = mr.m.Kind.Namespace
		}
		if clusterScoped {
			ns = ""
		}
		dst.SetNamespace(ns)
		out = append(out, dst)
	}
	return out
}

// destinationsMatch checks each source not reported drifted has a
// destination holding its spec; it returns the drifted ones.
func (mr *migrationRun) destinationsMatch(ctx context.Context, res []*engine.Result, from []*unstructured.Unstructured) (map[string]bool, error) {
	got := objectsOf(res)
	drifted := map[string]bool{}
	srcSpecs, err := mr.specs(ctx, from)
	if err != nil {
		return nil, err
	}
	dsts := mr.destinations(from)
	dstSpecs, err := mr.specs(ctx, dsts)
	if err != nil {
		return nil, err
	}
	var problems []string
	for i, src := range from {
		r := got[engine.Ref(src.GetNamespace(), src.GetName())]
		if r.Drifted {
			drifted[engine.Ref(src.GetNamespace(), src.GetName())] = true
			continue
		}
		have := dstSpecs[ref(dsts[i])]
		if have == nil {
			problems = append(problems, ref(src)+" has no destination")
			continue
		}
		if missing := structured.MissingPaths(srcSpecs[ref(src)], have); len(missing) > 0 {
			problems = append(problems, fmt.Sprintf("%s's destination lacks %s", ref(src), strings.Join(missing, ", ")))
		}
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return drifted, nil
}

// editDestinationAndDown edits the first destination the migration created,
// runs down, and checks it is drifted and kept while the other sources'
// destinations the migration created are gone.
func (mr *migrationRun) editDestinationAndDown(ctx context.Context, from []*unstructured.Unstructured, existing, drifted map[string]bool) (string, error) {
	dsts := mr.destinations(from)
	var target, targetSrc *unstructured.Unstructured
	for i, d := range dsts {
		key := engine.Ref(d.GetNamespace(), d.GetName())
		srcKey := engine.Ref(from[i].GetNamespace(), from[i].GetName())
		if !existing[key] && !drifted[srcKey] {
			target, targetSrc = d, from[i]
			break
		}
	}
	if target == nil {
		return "no destination the migration created to edit", nil
	}
	live, err := mr.Cluster.Get(ctx, target)
	if err != nil || live == nil {
		return "", fmt.Errorf("read %s: %v", ref(target), err)
	}
	spec, _, _ := unstructured.NestedMap(live.Object, "spec")
	key := firstLeaf(spec)
	if key == "" {
		return "the destination has no spec field to edit", nil
	}
	path := append([]string{"spec"}, strings.Split(key, ".")...)
	v, _, _ := unstructured.NestedFieldNoCopy(live.Object, path...)
	if err := unstructured.SetNestedField(live.Object, edited(v, mr.liveSchema(ctx, mr.m.Kind.To, key)), path...); err != nil {
		return "", err
	}
	if err := mr.Cluster.Update(ctx, live); err != nil {
		return "", fmt.Errorf("edit %s: %w", ref(target), err)
	}

	res, err := mr.Engine.Down(ctx, mr.with(false), kome.Options{})
	if err != nil {
		return "", err
	}
	mr.absorb(4, res)
	// down reports each object by the source it restores.
	if r := objectsOf(res)[engine.Ref(targetSrc.GetNamespace(), targetSrc.GetName())]; !r.AllDrifted {
		return "", fmt.Errorf("%s was edited but down did not report it drifted", ref(target))
	}
	if kept, _ := mr.Cluster.Get(ctx, target); kept == nil {
		return "", fmt.Errorf("the edited %s was deleted", ref(target))
	}
	for i, d := range dsts {
		k := engine.Ref(d.GetNamespace(), d.GetName())
		if d == target || existing[k] || drifted[engine.Ref(from[i].GetNamespace(), from[i].GetName())] {
			continue
		}
		if live, _ := mr.Cluster.Get(ctx, d); live != nil {
			return "", fmt.Errorf("%s is still there after down", ref(d))
		}
	}
	return "edited " + ref(target) + " spec." + key, nil
}

// prunedAndRecorded checks each pruned source is gone and its destination
// records it.
func (mr *migrationRun) prunedAndRecorded(ctx context.Context, res []*engine.Result, from []*unstructured.Unstructured) error {
	got := objectsOf(res)
	dsts := mr.destinations(from)
	for i, src := range from {
		if !got[engine.Ref(src.GetNamespace(), src.GetName())].Pruned {
			continue
		}
		if live, _ := mr.Cluster.Get(ctx, src); live != nil {
			return fmt.Errorf("%s was reported pruned but is still there", ref(src))
		}
		dst, err := mr.Cluster.Get(ctx, dsts[i])
		if err != nil || dst == nil {
			return fmt.Errorf("%s was pruned but its destination is missing", ref(src))
		}
		if _, ok := dst.GetAnnotations()[mr.ann.KindSnapshot]; !ok {
			return fmt.Errorf("%s does not record its source", ref(dst))
		}
	}
	return nil
}

// sourcesBack checks every source exists again, and that destinations seeded
// before the migration are still there.
func (mr *migrationRun) sourcesBack(ctx context.Context, from []*unstructured.Unstructured, existing map[string]bool) error {
	for _, src := range from {
		if live, err := mr.Cluster.Get(ctx, src); err != nil || live == nil {
			return fmt.Errorf("%s was not recreated", ref(src))
		}
	}
	for _, d := range mr.destinations(from) {
		if existing[engine.Ref(d.GetNamespace(), d.GetName())] {
			if live, _ := mr.Cluster.Get(ctx, d); live == nil {
				return fmt.Errorf("%s existed before the migration and was deleted", ref(d))
			}
		}
	}
	return nil
}

// firstLeaf returns the first scalar path in spec, in sorted order.
func firstLeaf(spec map[string]any) string {
	keys := make([]string, 0, len(spec))
	for k := range spec {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, k := range keys {
		switch v := spec[k].(type) {
		case map[string]any:
			if sub := firstLeaf(v); sub != "" {
				return k + "." + sub
			}
		case []any:
		default:
			return k
		}
	}
	return ""
}
