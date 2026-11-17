package engine

import (
	"fmt"
	"strings"
)

// PruneBlocked is why a source was not pruned: what the file must say before
// it can be. It is an object's error, and the report gathers them per
// migration into the lines to add.
type PruneBlocked struct {
	// UndeclaredFinalizers are on the source and not in lifecycle.finalizers.
	UndeclaredFinalizers []string
	// Children are objects the source owns while lifecycle.children is unset.
	Children []string
	// CannotOwn are children the destination cannot own.
	CannotOwn []string
	// DeclaredFinalizers is lifecycle.finalizers as the file has it, for the
	// report's suggestion: what to add joins what is already there.
	DeclaredFinalizers []string
}

func (b *PruneBlocked) Error() string {
	var parts []string
	if len(b.UndeclaredFinalizers) > 0 {
		parts = append(parts, fmt.Sprintf("finalizers %s are not in lifecycle.finalizers; list them there to let KOME remove them first",
			strings.Join(b.UndeclaredFinalizers, ", ")))
	}
	if len(b.Children) > 0 {
		parts = append(parts, fmt.Sprintf("it owns %s; set lifecycle.children to reparent, orphan or delete", Names(b.Children)))
	}
	if len(b.CannotOwn) > 0 {
		parts = append(parts, fmt.Sprintf("the destination can't own %s (a namespaced owner owns only objects in its own namespace)", Names(b.CannotOwn)))
	}
	return "not pruned: " + strings.Join(parts, "; ")
}

// Empty reports whether nothing blocks the prune.
func (b *PruneBlocked) Empty() bool {
	return len(b.UndeclaredFinalizers) == 0 && len(b.Children) == 0 && len(b.CannotOwn) == 0
}

// Names is names for a message: all of them up to five, then a count.
func Names(names []string) string {
	if len(names) <= 5 {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:5], ", "), len(names)-5)
}
