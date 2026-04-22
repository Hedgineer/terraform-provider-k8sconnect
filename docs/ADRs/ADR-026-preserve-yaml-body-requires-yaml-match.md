# ADR-026: Preserve-YAMLBody Drift Guard Requires yaml_body Equality

**Status:** Implemented
**Date:** 2026-04-22
**Related ADRs:** ADR-001 (Managed State Projection), ADR-024 (Managed Fields Relationship Scoping)

## Context

`checkDriftAndPreserveState` in `plan_modifier.go` suppresses cosmetic
`yaml_body` diffs in the plan output when the managed-state projection is
unchanged. The heuristic: if the projection's values haven't changed, the
user-visible effect on Kubernetes is zero, so overwrite `plannedData.YAMLBody`
with `stateData.YAMLBody` to avoid showing a pointless diff.

The pre-ADR-026 condition was:

```go
if baselineProjection.Equal(plannedData.ManagedStateProjection) {
    plannedData.YAMLBody = stateData.YAMLBody
    // …preserve projection, object_ref, managed_fields
}
```

This works fine in the steady-state flow where `state.yaml_body` and
`planned.yaml_body` both came from the same user config (they're equal, so
the preservation is a no-op on `yaml_body`).

## The bug

It breaks on any flow where `state.yaml_body` is not the user's config:

| Flow | state.yaml_body source | Behavior under old code |
|---|---|---|
| Normal apply | user's config | no-op preserve, fine |
| `tofu import` | live object serialized to YAML (includes every server-set default) | preservation *replaces* plan's yaml_body with the bloated imported yaml |

The import path of the bug:

1. `tofu state rm` + `tofu import` on an existing resource.
2. `import.go` serializes the live object into `state.yaml_body`. The live
   object has all the fields Kubernetes admission set (for a Service:
   `spec.clusterIP`, `spec.clusterIPs`, `spec.internalTrafficPolicy`,
   `spec.ipFamilies`, `spec.ipFamilyPolicy`, `spec.sessionAffinity`,
   `spec.type`, `spec.ports[0].protocol` — none of which were in the user's
   yaml).
3. `tofu plan`. ModifyPlan runs dry-run using the user's minimal yaml.
   Dry-run's managedFields attributes ports+selector to k8sconnect. Extraction
   yields 8 projection paths. Values read from dry-run result.
4. `baselineProjection` (state's projection, 8 entries from import-time
   `extractOwnedPaths` + core-field merge) equals `plannedData.ManagedStateProjection`
   (8 entries, same paths, same values — cluster state hasn't moved).
5. Preservation fires. `plannedData.YAMLBody = stateData.YAMLBody` — the
   bloated one.
6. Apply. SSA sends the bloated yaml with `force=true`. Kubernetes gives
   k8sconnect ownership of every field in the bloated yaml — including the
   server-set defaults. Post-apply k8sconnect owns 15 paths.
7. `updateProjection` in `context.go` runs `extractOwnedPaths` against the
   post-apply `currentObj.managedFields`. Since k8sconnect now owns 15 paths,
   the projection has 12 + 4-core = 16 entries.
8. Terraform Plugin Framework compares `plan.managed_state_projection` (8)
   against `apply.managed_state_projection` (16) and produces:

   ```
   Error: Provider produced inconsistent result after apply
   .managed_state_projection: new element "spec.clusterIP" has appeared
   ```

   (Plus the other 7 server-set defaults.)

## Why this hid for so long

Three conditions are needed:

1. `state.yaml_body` differs from user config (import, or some other path
   that writes the raw live object into yaml_body).
2. Projection values still match between state and plan (same cluster state;
   same user-owned fields).
3. The object's schema has admission-defaulted fields the user didn't write
   (common: Service, StatefulSet, Deployment, many CRDs).

Clients that go from creation → applies never hit (1). Clients that import
into a config that already reflects the live object also don't hit (1) in a
harmful way. The bug only fires when a user imports and expects to converge
the resource to a *narrower* user config — exactly what client-jhc's staging
deploy was doing.

It appeared to surface only after ADR-024 (`managed_fields` scoping, shipped
in `hed-v0.3.8-p1`), which made the scope mismatch between plan and apply
louder. The actual underlying bug predates ADR-024 by a wide margin.

## Decision

Preserve only when the two yaml_body strings **define the same field set**
(semantic comparison, not textual):

```go
yamlBodySameFields := yamlBodiesHaveSameFieldSet(
    stateData.YAMLBody.ValueString(),
    plannedData.YAMLBody.ValueString(),
)
if baselineProjection.Equal(plannedData.ManagedStateProjection) && yamlBodySameFields {
    // safe to preserve: same projection values AND same declared fields
    plannedData.YAMLBody = stateData.YAMLBody
    plannedData.ManagedStateProjection = stateData.ManagedStateProjection
    plannedData.ObjectRef = stateData.ObjectRef
    // …preserve managed_fields when eligible
}
```

`yamlBodiesHaveSameFieldSet` parses both strings and compares the sorted
lists of field paths produced by `extractFieldPaths`. This deliberately
ignores value differences (handled by the projection equality check) and
cosmetic textual differences (whitespace, ordering, quoting).

Three outcomes:

| state.yaml_body | plannedData.yaml_body | Preservation? |
|---|---|---|
| Same field set (cosmetic diff: whitespace, ordering) | Same field set | ✓ preserve — suppress spurious yaml_body diff |
| Bloated from import (has fields user's config omits) | User's minimal yaml | ✗ skip — apply the user's narrower yaml |
| User removed a field | User's narrower yaml | ✗ skip — honor the removal |

The first case is the original motivation (matches
`TestAccObjectResource_NoUpdateOnFormattingChanges`). The second case is the
client-jhc bug. The third case was arguably already buggy under the old
preservation logic and is now correctly handled.

## Consequences

### Positive

- Fixes the "new element has appeared" class of errors after `tofu import`.
- Preserves the original intent of the heuristic: suppress cosmetic
  yaml_body diffs when state was written from the same config path.
- No state migration, no schema change, no behavior change for the normal
  user-edit flow.

### Negative / trade-off

- Adds a YAML parse per plan. Negligible (milliseconds) — `extractFieldPaths`
  walks a typical manifest in microseconds.
- If `yamlBodiesHaveSameFieldSet` fails to parse either YAML, we fall through
  to the non-preserving branch (safer failure mode: shows a diff instead of
  silently preserving potentially stale state).

### Non-goals

- This does not fix `import.go`'s yaml_body bloat at import time. The live
  object is still serialized in full into `state.yaml_body`. A cleaner
  long-term direction is to have import store a narrower yaml_body, or to
  prompt users to edit their config to match the imported shape. Deferred
  to a follow-up ADR.
- This does not change ADR-024's `managed_fields` scoping. ADR-024 continues
  to filter managed_fields to (owned ∪ baseline ∪ yaml_body). The scope
  check (1) "currently owned by k8sconnect" was suspected during triage
  but was not the cause; it still evaluates consistently between plan and
  apply for fields we actually send.

## Regression test

`TestAccObjectResource_ImportPreservesUserYAMLBody` reproduces the exact
path: apply minimal Service → `ImportStatePersist` → re-apply → assert no
inconsistency error, assert managed_fields and managed_state_projection
do not contain server-set defaults. Pre-fix this test fails with the
framework's "new element has appeared" error; post-fix it converges.

## Related Documentation

- ADR-001: Managed State Projection (what the projection is)
- ADR-024: Managed Fields Relationship Scoping (what scoping ADR-024 adds)
- PR: `hed-v0.3.8-p3` release notes
