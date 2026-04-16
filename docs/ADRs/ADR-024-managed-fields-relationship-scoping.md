# ADR-024: Managed Fields Relationship Scoping

**Status:** Implemented
**Date:** 2026-04-16
**Related ADRs:** ADR-005 (Managed Fields Strategy), ADR-020 (Managed Fields Display Strategy), ADR-021 (Ownership Transition Messaging)

## Context

ADR-020 framed `managed_fields` as "bounded by managed_state_projection" (i.e., scoped to the user's `yaml_body`), but the actual implementation went broader: `updateManagedFieldsData` (and the dry-run prediction block in `plan_modifier.go`) included ownership for every field manager on the object, filtered only by `status.*` and Kubernetes system-annotation patterns.

That broader scope produced a class of breakages observed in the wild:

> Provider produced inconsistent result after apply

The most common trigger was an ArgoCD `Application` resource. ArgoCD's `argocd-application-controller` writes spec-level fields like `operation.sync.revision` and `operation.sync.prune` on the object as part of normal reconciliation. These paths are not in the user's `yaml_body` and k8sconnect never owned them — they are controller internals. But because `managed_fields` was unfiltered, those foreign paths:

1. Were absent from the plan prediction (the dry-run response didn't include them, since we were not sending them).
2. Appeared in the post-apply `managed_fields` value (the controller wrote them in between plan and apply).

Terraform's plugin framework then failed the apply with "inconsistent result" because the provider produced a `managed_fields` map that differed from the plan.

The same failure mode was reachable with other controllers (Flux, operators writing their own status-adjacent spec fields, admission webhooks adding fields, etc.). Anything that touches a path we never referenced was risk.

## Decision

**Scope `managed_fields` to fields k8sconnect has a relationship with.** Specifically, the value is computed from the union of:

1. **Currently owned by k8sconnect** — paths where `k8sconnect` appears in the live object's `managedFields`.
2. **Previously owned by k8sconnect** — paths present in the ADR-021 `ownership_baseline` blob in private state with manager `k8sconnect`. This preserves visibility through ownership transitions (e.g., a field we owned last apply that HPA has now taken).
3. **Referenced in `yaml_body`** — paths extracted from the user's YAML. The user explicitly referenced these; they deserve visibility on who owns them (including takeovers by external controllers).

Paths outside this union are controller internals. We have no managed relationship with them; tracking them produces noise and the inconsistent-result failures above.

The existing `status.*` and K8s system-annotation filters continue to apply **within** this scope — those fields are excluded regardless of ownership because they change unpredictably and aren't actionable.

### What still works

- **Ownership transitions on yaml_body paths** — a field in yaml_body that an external controller takes over (HPA on `spec.replicas`) remains visible. `managed_fields[spec.replicas]` flips to `hpa-controller`, the plan diff shows the transition, and ADR-021 warnings still fire.
- **Transitions away from k8sconnect** — a field we owned at the last apply but no longer have in yaml_body stays visible via the baseline path, so the user sees the ownership handoff on the next plan.
- **Force-take visibility** — when we re-apply and force=true reclaims a field, the transition back to `k8sconnect` appears as before.

### What is excluded

- **Fields we never referenced and never owned** — foreign controller internals like `operation.*` on ArgoCD Applications.
- **Status fields** — unchanged from prior behavior.
- **K8s system annotations** (`kubectl.kubernetes.io/*`, `deployment.kubernetes.io/*`, etc.) — unchanged.

## Rationale

SSA's mental model is "you own what you write." A provider's view of field ownership should match that mental model: show the fields I manage, plus the ones I used to manage, plus the ones my config explicitly touches. Anything else is someone else's problem.

The previous broader scope was reaching for a generality that had no users. Nothing in the product relied on tracking foreign-controller ownership for paths outside the user's config, and the cost (inconsistent-result failures on otherwise valid CRDs) was borne every time a real-world controller wrote a spec field on one of its own resources.

This scoping is also consistent with ADR-020's original intent ("bounded by managed_state_projection"), which the implementation had drifted from.

## Implementation

- `internal/k8sconnect/resource/object/managed_fields.go`:
  - New helper `computeRelevantManagedFieldPaths(yamlBody, baseline, ownership)` returns the relevant-path set.
  - `updateManagedFieldsData` intersects the live ownership with the relevant set before flattening.
  - New helper `readOwnershipBaseline` deserializes the ADR-021 private-state blob.
- `internal/k8sconnect/resource/object/plan_modifier.go`:
  - The dry-run managed_fields prediction block applies the same scoping via `computeRelevantManagedFieldPaths`.
  - `applyProjection` receives the baseline from `calculateProjection`, which reads it from `req.Private`.
- `internal/k8sconnect/resource/object/crud.go`:
  - Create/Read/Update callers pass `yaml_body` and (for Read/Update) the baseline from `req.Private`.
- Tests:
  - Unit tests in `managed_fields_scope_test.go` cover the three relevant-path sources and verify foreign paths are excluded while yaml_body paths with external ownership remain visible.
  - Acceptance test `managed_fields_foreign_scope_test.go` reproduces the ArgoCD-style scenario with a ConfigMap (foreign manager writes a `data.external-key` not in yaml_body) and verifies no inconsistent-result error and that the foreign path doesn't appear in `managed_fields`.

## Non-Goals

- No change to `ignore_fields` semantics for yaml_body / projection / SSA (separate concern — ignored fields are still in scope for managed_fields display, since they're still in yaml_body).
- No change to the status/system-annotation filters.
- No change to the ADR-021 `ownership_baseline` private-state mechanism.
- No change to the patch resource's `managed_fields` (the patch implementation already filtered to its own field manager; the inconsistent-result class didn't apply there).

## Trade-offs

**Benefits**
- Eliminates inconsistent-result errors on CRDs with active controllers (ArgoCD Applications, Flux resources, operator CRs).
- `managed_fields` matches the user's mental model: "the fields I care about."
- Smaller, more actionable diff output — no noise from fields we were never going to manage.

**Limitations**
- If a user wants visibility into foreign-controller ownership on fields they haven't configured, they won't see it here. The existing approach was not reliably providing this anyway (it was fragile to the exact timing of controller writes vs. plan/apply). Users who need that view can query `kubectl get <resource> -o yaml` directly.
- `computeRelevantManagedFieldPaths` parses `yaml_body` once per plan/update; in hot paths this is a microseconds-level overhead. Not material.

## Related Documentation

- ADR-005: Managed Fields Strategy
- ADR-020: Managed Fields Display Strategy (this ADR supersedes the implicit broader scope the implementation had drifted into)
- ADR-021: Ownership Transition Messaging (baseline source this ADR consumes)
