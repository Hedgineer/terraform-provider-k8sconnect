# terraform-provider-k8sconnect (Hedgineer Fork)

Fork of [jmorris0x0/terraform-provider-k8sconnect](https://github.com/jmorris0x0/terraform-provider-k8sconnect) for internal distribution.

## Why This Fork Exists

We are switching AKS clusters from private API + `az aks command invoke` to public API + Entra protection. k8sconnect solves three problems simultaneously:

1. **Single-pass creation** — create AKS cluster + bootstrap K8s resources in one `tofu apply` (no two-phase orchestration)
2. **Real plan output** — Server-Side Apply with dry-run gives actual diffs, replacing `terraform_data` + shell scripts where plan just says "script will run"
3. **Escape invoke pain** — no more exit-code-0-on-failure, 200KB upload limits, flaky empty JSON, 7s latency per call

We are entering a fast client rampup phase where new environments get deployed regularly. Seamless single-apply provisioning is an operational multiplier.

## Fork Strategy

**Upstream `main` plus a small patch stack, released on our own version line.**

- Based on upstream `main` f59fe83 (`v0.3.9` plus the ADR-023 docs commit), rebased 2026-11-10 in fork PR #3.
- The patch stack carries fixes upstream doesn't have:
  - object fixes, documented as HED-ADR-001..003 in `docs/ADRs/`
  - Deployment rollout waits until old replicas are gone, like `kubectl rollout status`
  - JSON arrays whose elements differ in shape convert to tuples, so `data.k8sconnect_object` can read real Deployments and DaemonSets (`TestAccObjectDataSource_mixedShapeList`)
  - the Hedgineer release workflow, darwin/arm64 builds, and this file
- Keep Hedgineer CI and release logic in separate files (`hedgineer-release.yml`), so syncs don't conflict.
- Upstream is public. Sending a fix upstream is a per-fix decision and needs explicit sign-off first.

### ADR numbering

Fork ADRs are `HED-ADR-NNN` in `docs/ADRs/`, so they never collide with upstream's `ADR-NNN` sequence. The next free number is HED-ADR-004.

### Syncing with upstream

The org ruleset on `main` blocks force-pushes and merge commits, and requires a PR with one approval given after the last push, merged by squash or rebase. Upstream merges with merge commits, so a rebase plus force-push, a plain merge, `gh repo sync` and the "Sync fork" button are all rejected. Don't use the admin bypass.

Squash-merging means upstream is **not** an ancestor of `main`. The rebased patch stack lives on the last sync PR's head instead:

| Sync point (2026-11-10) | Value |
|---|---|
| Upstream base | f59fe83 |
| Patch stack top | 2c845b8 = `refs/pull/3/head^1` |
| Squash commit on `main` | ce68b21 |

Recipe (the SHAs and PR number come from the table):

```bash
git fetch upstream --tags            # our tags are hed-v*, so upstream's v* tags can't collide
git fetch origin main 'refs/pull/3/head:refs/remotes/origin/pr/3'
OLD_BASE=f59fe83
STACK=$(git rev-parse origin/pr/3^1)

git switch -c sync/upstream-$(git rev-parse --short upstream/main) "$STACK"
git cherry-pick ce68b21..origin/main # main's commits since the last sync, if any
git diff --stat HEAD origin/main     # must print nothing: the branch has everything main has
PRE=$(git rev-parse HEAD)

git rebase --onto upstream/main "$OLD_BASE"
git range-diff "$OLD_BASE..$PRE" upstream/main..HEAD   # every patch unchanged, or changed on purpose

git merge -s ours origin/main -m "merge: supersede squashed main with rebased history"
```

Then:

1. Push the branch and open a PR. Same-repo PRs run the full acceptance suite.
2. Get one teammate's approval.
3. Squash-merge with a custom one-line subject. The default body would list every upstream commit.
4. Check the squash commit's tree equals the branch tree: `git diff <squash> <branch>` prints nothing.
5. Update the sync point table above with the new base, stack top (`refs/pull/<N>/head^1`) and squash commit.

`-s ours` silently drops anything the branch lacks, so never skip the `git diff` and `range-diff` checks.

### Pushing

The `origin` remote is SSH, and pushes over it fail with "The 'Hedgineer' organization has enabled or enforced SAML SSO" unless your key is SSO-authorized. Push over HTTPS instead, which uses the `gh` credential helper:

```bash
git push https://github.com/Hedgineer/terraform-provider-k8sconnect.git <branch>
```

## Distribution

Published as an **OCI provider mirror** to ACR (`hedgineercicdacr`), served to prod/clients via pls-proxy.

OpenTofu supports OCI provider mirrors natively: https://opentofu.org/docs/cli/oci_registries/provider-mirror/

### How it works

1. `hedgineer-release.yml` builds platform ZIPs with plain `go build` (darwin amd64/arm64, linux amd64, windows amd64)
2. It uses `oras` to push each platform ZIP as OCI artifacts with correct artifact types
3. Creates OCI index manifest combining all platforms
4. Pushes to ACR: `hedgineercicdacr.azurecr.io/opentofu-providers/hedgineer/k8sconnect:<version>`
5. pls-proxy serves it at `modules.hub.hedgineer.ai/opentofu-providers/hedgineer/k8sconnect:<version>`

### Versioning

- Releases are plain semver from `0.3.100` up, staying below `0.4.0` because published modules cap there.
- Trial builds are `-rc.N` prereleases, e.g. `0.3.100-rc.1`. OpenTofu picks a prerelease only through an exact pin (`version = "0.3.100-rc.1"`).
- A final release is built from the same commit as the rc it promotes.
- The older `0.3.8-pN` and `0.3.9-p1` tags stay in ACR for roots that still pin them.
- Which versions each client sees is gated per client in pls-proxy. The plan lives in hedgineer-bizops, `openspec/changes/k8sconnect-release-versioning/`.

### Consumer configuration

In `.terraformrc` or CLI config:

```hcl
provider_installation {
  oci_mirror {
    repository_template = "modules.hub.hedgineer.ai/opentofu-providers/${namespace}/${type}"
    include             = ["registry.opentofu.org/hedgineer/*"]
  }
  direct {
    exclude = ["registry.opentofu.org/hedgineer/*"]
  }
}
```

Roots pin an exact version:

```hcl
required_providers {
  k8sconnect = {
    source  = "hedgineer/k8sconnect"
    version = "0.3.100"
  }
}
```

Modules state a minimum only, e.g. `version = ">= 0.3.9-p1"`. Raise a module's minimum only after every client's proxy map grants that version.

## CI/CD

### Workflow files

| File | Trigger | Purpose |
|---|---|---|
| `hedgineer-release.yml` | GitHub Release published (`hed-v*` tags) | Build + push OCI to ACR |
| `release.yml` | `workflow_dispatch` only (tag trigger removed) | Upstream's GPG-signed release — disabled on this fork |
| `test.yml` | Push to main, PRs, `workflow_dispatch` | Unit tests, build, acceptance tests, lint, coverage |
| `security.yml` | Push to main, PRs | gosec, govulncheck |

Acceptance tests run only on pushes to `main` and same-repo PRs. A `workflow_dispatch` run of Test, or a PR from another fork, skips them.

### Tag namespace

Tags are `hed-v<version>` (e.g. `hed-v0.3.100` publishes OCI tag `0.3.100`). Upstream's `v*` tags never collide with ours, so upstream can be fetched with tags. `hedgineer-release.yml` ignores any release not tagged `hed-v*`.

### How to release

Always pass `--target` with a full commit SHA. The workflow builds whatever commit the tag points at, and without `--target` the tag lands on the current tip of `main`, which breaks "final on the same commit as the rc".

```bash
gh release create hed-v0.3.101-rc.1 --target "$(git rev-parse origin/main)" --prerelease --generate-notes
# after the rc is proven on staging, release the rc's commit, not the current main:
git fetch origin --tags
gh release create hed-v0.3.101 --target "$(git rev-parse 'hed-v0.3.101-rc.1^{commit}')" --generate-notes
```

This creates the tag + release on GitHub, which triggers `hedgineer-release.yml` → `go build` → `oras` push to ACR.

### Identity & Authentication

- Uses shared `hed-module-publisher` managed identity in `hedgineer-shared-rg`
- `AcrPush` on both `hedgineermodules` (pre-existing) and `hedgineercicdacr`
- GitHub OIDC federation: `repo:Hedgineer/terraform-provider-k8sconnect:environment:release`
- Identity is bootstrapped manually (not managed by bizops TF), same pattern as auth-proxy
- GitHub environment `release` with vars: `AZURE_CLIENT_ID`, `AZURE_TENANT_ID`, `AZURE_SUBSCRIPTION_ID`

### Upstream workflow changes

The upstream `release.yml` uses GPG signing for the Terraform Registry protocol. We don't need GPG signing since we're distributing via OCI mirror (integrity is handled by OCI content-addressable digests). The upstream tag trigger has been removed (one-line diff — reapply after upstream syncs). Our release workflow (`hedgineer-release.yml`) is a separate file that never conflicts on merge.

## Technology

- **Language:** Go (see `go.mod` for version)
- **Build:** plain `go build` + zip in `hedgineer-release.yml` (upstream's `release.yml` uses GoReleaser)
- **Provider framework:** Terraform Plugin Framework (protocol 6.0)
- **K8s interaction:** Server-Side Apply with dry-run for plan, SSA for apply
- **Minimum K8s version:** 1.28

## Validated

**Apr 2026:** PR #673 on hedgineer-bizops proved end-to-end:
- Provider loads from OCI mirror (`hedgineercicdacr`) — `Installed hedgineer/k8sconnect v0.3.7 (verified checksum)`
- Entra auth via kubelogin exec works — `data.k8sconnect_object.kube_system[0]: Read complete after 2s`
- SSA dry-run reads real K8s state during plan
- CA cert TLS verification works (`cluster_ca_certificate` from AKS module output)

**Nov 2026:** `0.3.100` deployed to bizops staging (hedgineer-bizops #8878). All k8sconnect resources refreshed against the live cluster, and the apply made no changes.

### Auth gotchas discovered

- **ACR OAuth2 + OpenTofu:** `docker login` credentials from `az acr login` do NOT work — OpenTofu ignores Docker credential helpers and doesn't present config.json credentials in ACR's OAuth2 token exchange. Must use `oci_credentials` block in generated `.tfrc`. This is staging-only; prod/clients use pls-proxy where simple `docker login` works.
- **kubelogin required:** k8sconnect talks to K8s API during plan (SSA dry-run). CI needs `az aks install-cli` to install kubelogin.
- **Inline connection requires CA cert or insecure=true:** Added `cluster_ca_certificate` output to aks-cluster module.

## Upstream Resources

- Repo: https://github.com/jmorris0x0/terraform-provider-k8sconnect
- Docs: `docs/` directory and `examples/`
- Fork base: upstream `main` f59fe83 (`v0.3.9` plus ADR-023)

## What NOT to Change

- Do not modify provider source code without discussing first
- Do not rename the binary or change the provider address
- Do not add Hedgineer-specific provider features — contribute upstream instead
- Do not remove upstream's `.goreleaser.yml` or `release.yml`. Keeping them, with only the `release.yml` tag-trigger change, avoids sync conflicts
