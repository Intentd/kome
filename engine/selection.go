package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8slabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/dynamic"

	"github.com/intentd/tools/condition"
	"github.com/intentd/tools/kubeclient"
	"github.com/intentd/tools/structured"
)

// Selection is which objects a run migrates.
type Selection struct {
	Namespaces []string
	Name       string
	Selector   string
	When, Or   condition.Conditions
}

// Narrow returns the migration's selection s narrowed by the caller's scope:
// the scope can narrow the namespaces, never widen them; names must agree;
// label selectors are both applied. Conditions are the migration's.
func (s Selection) Narrow(scope Selection) (Selection, error) {
	out := s
	if len(scope.Namespaces) > 0 {
		if len(s.Namespaces) == 0 {
			out.Namespaces = scope.Namespaces
		} else if out.Namespaces = structured.Intersect(scope.Namespaces, s.Namespaces); len(out.Namespaces) == 0 {
			return out, fmt.Errorf("namespaces %s are outside the migration's selection %s",
				strings.Join(scope.Namespaces, ","), strings.Join(s.Namespaces, ","))
		}
	}
	switch {
	case s.Name != "" && scope.Name != "" && s.Name != scope.Name:
		return out, fmt.Errorf("the migration selects name %q; %q was requested", s.Name, scope.Name)
	case scope.Name != "":
		out.Name = scope.Name
	}
	switch {
	case s.Selector == "":
		out.Selector = scope.Selector
	case scope.Selector != "":
		out.Selector = scope.Selector + "," + s.Selector
	}
	return out, nil
}

// IsEmpty reports whether s selects everything.
func (s Selection) IsEmpty() bool {
	return len(s.Namespaces) == 0 && s.Name == "" && s.Selector == "" && !s.HasConditions()
}

// HasConditions reports whether when or or is declared.
func (s Selection) HasConditions() bool { return len(s.When) > 0 || len(s.Or) > 0 }

// Includes reports whether an object with this identity and labels is in s,
// for where the API server cannot filter, such as sources recorded in a
// destination's record. Conditions are not checked.
func (s Selection) Includes(name, namespace string, lbls map[string]string) (bool, error) {
	if s.Name != "" && name != s.Name {
		return false, nil
	}
	if len(s.Namespaces) > 0 && namespace != "" && !slices.Contains(s.Namespaces, namespace) {
		return false, nil
	}
	matcher, err := k8slabels.Parse(s.Selector)
	if err != nil {
		return false, fmt.Errorf("parse selector %q: %w", s.Selector, err)
	}
	return matcher.Matches(k8slabels.Set(lbls)), nil
}

// Admits reports whether obj passes s's when/or conditions, evaluated with
// r's functions. Objects pass when none are declared.
func (r *Runner) Admits(s Selection, obj *unstructured.Unstructured) bool {
	if !s.HasConditions() {
		return true
	}
	return r.Resolver(obj.Object).Evaluate(s.When, s.Or)
}

// List lists the objects of resource in s. A cluster-scoped resource is
// listed whole; the namespaces are ignored, with a warning.
func (r *Runner) List(ctx context.Context, s Selection, kind string, resource dynamic.NamespaceableResourceInterface, clusterScoped bool, warn func(string)) ([]unstructured.Unstructured, error) {
	if clusterScoped && len(s.Namespaces) > 0 {
		warn(fmt.Sprintf("namespaces %s ignored: %s is cluster-scoped", strings.Join(s.Namespaces, ","), kind))
	}
	opts := metav1.ListOptions{Limit: r.PageSize, LabelSelector: s.Selector}
	if s.Name != "" {
		opts.FieldSelector = "metadata.name=" + s.Name
	}
	items, err := kubeclient.ListAllForNamespaces(ctx, resource, clusterScoped, s.Namespaces, opts)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", kind, err)
	}
	return items, nil
}
