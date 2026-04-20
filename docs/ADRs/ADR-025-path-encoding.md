# ADR-025: Path Encoding for Map Keys Containing Dots

**Status:** Implemented
**Date:** 2026-04-20
**Related ADRs:** ADR-001 (Managed State Projection), ADR-020 (Managed Fields Display Strategy), ADR-024 (Managed Fields Relationship Scoping)

## Context

Field paths in this provider are dot-joined: `spec.replicas`, `metadata.name`. The format breaks for Kubernetes map keys that legitimately contain dots:

- ConfigMap data keys: `config.yaml`, `application.properties`
- Annotation keys: `app.kubernetes.io/name`, `kubectl.kubernetes.io/last-applied-configuration`
- Label keys: `app.kubernetes.io/part-of`
- ArgoCD `argocd-cm` entries: `resource.customizations.ignoreDifferences.admissionregistration.k8s.io_ValidatingWebhookConfiguration` (one key)

The old path builder did `prefix + "." + key` regardless of key contents, producing:

```
data.resource.customizations.ignoreDifferences.admissionregistration.k8s.io_ValidatingWebhookConfiguration
```

…which was indistinguishable from a seven-level nested structure. The old path consumer (`parsePath`, `getFieldByPath`) did `strings.Split(path, ".")` and tried to navigate seven levels deep. For a flat-dotted source it failed, returned "not found," and `projectFields` silently skipped the entry.

### The drift-collapse bug

With silent skipping, two projections computed from different source structures ended up equal:

```
state.managed_state_projection  = {/* dotted keys silently dropped */}
planned.managed_state_projection = {/* dotted keys silently dropped */}
```

`baselineProjection.Equal(plannedData.ManagedStateProjection)` returned `true` → `checkDriftAndPreserveState` reverted `yaml_body` to state's old value → plan reported "no changes" even when the user had changed a dotted-key value. Two Hedgineer staging clients hit this with the ArgoCD `argocd-cm` patch pattern (six dotted keys, including `ui.bannercontent`, `ui.bannerpos`, and four `resource.customizations.ignoreDifferences.<group>_<kind>` entries).

`extractFieldPaths`, `extractPathsFromFieldsV1`, `extractPathsFromFieldsV1Simple`, and `parseOwnedFields` all had the same path-build bug. `getFieldByPath`, `setFieldByPath`, and `parsePath` all had the matching consumer bug. A separate function, `getFieldValue` (used only for drift-warning value extraction), had a greedy fallback fix — but none of the projection-side call sites did.

## Decision

**Encode paths with a quote-delimited syntax. Keys containing `.`, `"`, or `\` are wrapped in `"..."` with `\"` and `\\` escapes inside; keys without those characters are bare. Dots outside quotes and outside `[...]` are the only separators.**

Examples:

| Key | Encoded path segment |
|---|---|
| `replicas` | `replicas` |
| `config.yaml` | `"config.yaml"` |
| `app.kubernetes.io/name` | `"app.kubernetes.io/name"` |
| `resource.customizations.ignoreDifferences.admissionregistration.k8s.io_ValidatingWebhookConfiguration` | `"resource.customizations.ignoreDifferences.admissionregistration.k8s.io_ValidatingWebhookConfiguration"` |

Full paths:

| Nested | Flat-dotted key |
|---|---|
| `spec.replicas` | `data."config.yaml"` |
| `spec.containers[name=nginx].image` | `metadata.annotations."app.kubernetes.io/name"` |

### Scope of the change

- **Emit sites** (convert unescaped K8s key → encoded path segment): `extractFieldPaths`, `parseOwnedFields`, `extractPathsFromFieldsV1`, `extractPathsFromFieldsV1Simple`.
- **Consume sites** (parse encoded path → navigate/manipulate): `parsePath`, `getFieldByPath`, `setFieldByPath`, `navigateToPath`, `flattenProjectionToMap`, `removeParentFieldsFromOwnership`.
- **Shared helpers** in `internal/k8sconnect/common/fieldmanagement/paths.go`: `EncodePathKey`, `DecodePathKey`, `JoinPath`, `SplitPath`, `FindSelectorStart`.
- **Value extraction** (`getFieldValue` in `plan_modifier.go`) — simplified from greedy fallback to deterministic `getFieldByPath`, since all callers now receive encoded paths.

### User-facing surfaces

- **`managed_state_projection` map keys**: encoded. Users see `data."config.yaml"` in state/plan output. One-time diff on next plan for existing resources; idempotent SSA re-apply stabilizes state.
- **`managed_fields` map keys**: encoded, same as above.
- **`ignore_fields` user input**: back-compat preserved. Users may write either form:
  - Authoritative quoted: `data."config.yaml"`
  - Unescaped, for convenience: `data.config.yaml`
  
  `pathMatchesIgnorePattern` first tries a direct segment match. If that fails, it falls back to joining consecutive no-selector pattern segments with `.` and matching against a single encoded path segment. This lets users keep existing configs unchanged while encouraging explicit-quote usage going forward.

## Rationale

### Why not greedy fallback at the reader?

Initial proposal was to make `getFieldByPath` mirror `getFieldValue`'s greedy "try nested, then try flat" logic. Pros: zero state churn. Cons: the stored `managed_state_projection` format remained ambiguous forever, the edge case "both nested and flat exist with the same path" had undefined resolution, and each new reader of stored state would have to replicate the greedy logic correctly. The contract stayed broken.

### Why not escape-based encoding (`data.config\.yaml`)?

Also considered. Same implementation cost, same correctness properties. Rejected for readability: `data."resource.customizations.ignoreDifferences.admissionregistration.k8s.io_ValidatingWebhookConfiguration"` is legible; `data.resource\.customizations\.ignoreDifferences\.admissionregistration\.k8s\.io_ValidatingWebhookConfiguration` is not. On ArgoCD-style configs where dotted keys dominate, the quoted form is qualitatively easier to read.

### Why accept user input without quotes?

Existing consumer configs predate this ADR. Requiring them to rewrite `ignore_fields` entries as a provider upgrade side-effect would be a hostile break. The back-compat path (greedy segment joining inside the matcher) imposes no runtime cost on the common case (non-dotted keys match on the fast path) and localizes the ambiguity to pattern matching only — the internal path representation stays unambiguous.

## Blast Radius

Accepted at decision time: on the first plan after upgrading the provider, any resource with dotted keys in labels / annotations / ConfigMap data will show a diff because stored state uses the old unencoded format. The apply is a no-op at the Kubernetes level (SSA sees the same data), but pipelines see "N resources will be modified." One-time churn, then state stabilizes.

Hedgineer context at decision time: two staging clients, no production. Accepted.

## Implementation

- `internal/k8sconnect/common/fieldmanagement/paths.go`: new file with encode/decode/split/join helpers.
- `internal/k8sconnect/common/fieldmanagement/paths_test.go`: unit tests for the helpers including edge cases (quotes inside quoted segments, brackets inside quoted segments, escape round-trips).
- `internal/k8sconnect/common/fieldmanagement/ownership.go`: `extractPathsFromFieldsV1` and `extractPathsFromFieldsV1Simple` use `JoinPath(prefix, EncodePathKey(fieldName))`.
- `internal/k8sconnect/resource/object/projection.go`: `extractFieldPaths`, `parseOwnedFields`, `parsePath`, `navigateToPath`, `pathMatchesIgnorePattern` updated.
- `internal/k8sconnect/resource/object/plan_modifier.go`: `removeParentFieldsFromOwnership` uses `SplitPath`; `getFieldValue` / `getNestedFieldValue` collapsed into the new deterministic traversal.
- `internal/k8sconnect/resource/object/projection_dotted_keys_test.go`: regression tests for the extract→project→flatten round-trip, drift detection on dotted keys, and ignore_fields back-compat.
- `internal/k8sconnect/resource/object/projection_dotted_keys_acc_test.go`: acceptance test mirroring the ArgoCD `argocd-cm` patch pattern.
- `internal/k8sconnect/resource/object/plan_modifier_value_test.go`: updated to exercise the new encoded-path contract.

## Non-Goals

- No change to the array selector syntax (`[name=nginx]`, `[0]`, `[?(@.field=='value')]`) — brackets already disambiguate.
- No change to `ignore_fields` JSONPath-predicate resolution.
- No state upgrader. The one-time diff is the migration.
- No retrofit of the `patch` resource's path handling — it filters to its own field manager and did not exhibit the drift-collapse bug.

## Related Documentation

- ADR-001: Managed State Projection (overall projection model)
- ADR-024: Managed Fields Relationship Scoping (separate fix shipped in `hed-v0.3.8-p1`)
