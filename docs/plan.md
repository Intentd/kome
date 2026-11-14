# Plan

Work that is decided in direction but not built yet.

## Where to look for a source's children

Before pruning a source, KOME looks for the objects it owns (by
ownerReference): it lists every listable resource type discovery reports,
cluster-wide for a cluster-scoped source, in the source's namespace otherwise.

That is right for KOME's CLI, which runs with the user's own credentials. It is
wrong for a consumer that runs with a narrow role, such as Inrun's gateway: it
can't list every type, so the search fails and nothing is pruned. It also
lists far more than a migration usually needs.

KOME does not own RBAC, and it should not guess which types matter. When a
consumer needs it, KOME takes what to search from the caller:

- **Types**: the resource types a source may own. Only these are listed.
- **Namespaces**: where those children live, when not with the source, so a
  cluster-scoped source does not list every namespace.

Left unset, KOME keeps searching everything, as it does now.

### Open when building it

- A child of a type outside the list is never seen. Pruning with the default
  background propagation would let the garbage collector delete it. Options:
  delete the source with `propagationPolicy: Orphan` whenever the search is
  narrowed (except `children: delete`), or refuse to narrow without it. The
  first keeps unseen children alive, ownerless.
- How the list is declared in the KOME file (under `lifecycle`), or only
  through the Go API.

### On the consumer side (Inrun)

- `migrate.lifecycle.managedResources`, shaped like the reconcilers'
  `managedResources`, feeds both the list KOME searches and the RBAC Inrun
  generates (`list` on each, `patch` for `children: reparent`).
- Validation fails when `prune` is on and no `managedResources` are declared:
  assuming `*` would grant list on every type. An empty list states "owns
  nothing" and is allowed.
- Whether the gateway prunes at startup at all, or leaves the lifecycle to
  `inrun migrate` run by a person, is undecided.
