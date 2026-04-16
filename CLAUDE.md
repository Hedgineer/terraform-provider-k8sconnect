# terraform-provider-k8sconnect (Hedgineer Fork)

Fork of [jmorris0x0/terraform-provider-k8sconnect](https://github.com/jmorris0x0/terraform-provider-k8sconnect) for internal distribution.

## Why This Fork Exists

We are switching AKS clusters from private API + `az aks command invoke` to public API + Entra protection. k8sconnect solves three problems simultaneously:

1. **Single-pass creation** — create AKS cluster + bootstrap K8s resources in one `tofu apply` (no two-phase orchestration)
2. **Real plan output** — Server-Side Apply with dry-run gives actual diffs, replacing `terraform_data` + shell scripts where plan just says "script will run"
3. **Escape invoke pain** — no more exit-code-0-on-failure, 200KB upload limits, flaky empty JSON, 7s latency per call

We are entering a fast client rampup phase where new environments get deployed regularly. Seamless single-apply provisioning is an operational multiplier.

## Fork Strategy

**Minimal divergence. Distribution wrapper only.**

- Fork is pinned to upstream tagged releases (currently v0.3.7)
- Only Hedgineer additions: CI/CD workflow, this CLAUDE.md
- Upstream code is untouched — no patches, no behavior changes
- Bug fixes go upstream first (PR to jmorris0x0); only carry patches temporarily if upstream is unresponsive
- Track upstream releases: when a new version is tagged, rebase our CI commits on top

### Syncing with upstream

```bash
git fetch upstream
git rebase upstream/v0.X.Y  # rebase onto new release tag
# or if divergence grows:
git merge upstream/v0.X.Y
```

If we start making substantive changes (unlikely), switch from rebase to merge.

## Distribution

Published as an **OCI provider mirror** to ACR (`hedgineercicdacr`), served to prod/clients via pls-proxy.

OpenTofu supports OCI provider mirrors natively: https://opentofu.org/docs/cli/oci_registries/provider-mirror/

### How it works

1. GoReleaser builds platform ZIPs (darwin/linux/windows amd64)
2. `hedgineer-release.yml` uses `oras` to push each platform ZIP as OCI artifacts with correct artifact types
3. Creates OCI index manifest combining all platforms
4. Pushes to ACR: `hedgineercicdacr.azurecr.io/opentofu-providers/hedgineer/k8sconnect:<version>`
5. pls-proxy serves it at `modules.hub.hedgineer.ai/opentofu-providers/hedgineer/k8sconnect:<version>`

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

Then in module code:

```hcl
required_providers {
  k8sconnect = {
    source  = "hedgineer/k8sconnect"
    version = "~> 0.3"
  }
}
```

**Pre-release patch versions**: When we carry a downstream-only fix on top of upstream (e.g., `hed-v0.3.8-p1` = "our patch before upstream 0.3.8 lands"), the OCI tag is `0.3.8-p1`. OpenTofu's resolver excludes pre-release versions from range constraints like `~> 0.3`, so consumers must pin explicitly:

```hcl
version = "0.3.8-p1"   # or "~> 0.3.8-p1"
```

Once upstream ships `v0.3.8` with the fix, consumers can drop the pin back to `~> 0.3` and `0.3.8` will supersede `0.3.8-p1` naturally.

## CI/CD

### Workflow files

| File | Trigger | Purpose |
|---|---|---|
| `hedgineer-release.yml` | GitHub Release published (`hed-v*` tags) | Build + push OCI to ACR |
| `release.yml` | `workflow_dispatch` only (tag trigger removed) | Upstream's GPG-signed release — disabled on this fork |
| `test.yml` | Push to main, PRs | Unit tests, build, acceptance tests, lint, coverage |
| `security.yml` | Push to main, PRs | gosec, govulncheck |

### Tag namespace

We use `hed-v*` tags (e.g. `hed-v0.3.7`) to avoid clashes with upstream `v*` tags on sync. The `hedgineer-release.yml` workflow ignores any release not tagged `hed-v*`.

### How to release

```bash
gh release create hed-v0.3.7 --generate-notes
```

This creates the tag + release on GitHub, which triggers `hedgineer-release.yml` → GoReleaser build → `oras` push to ACR.

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
- **Build:** GoReleaser v2
- **Provider framework:** Terraform Plugin Framework (protocol 6.0)
- **K8s interaction:** Server-Side Apply with dry-run for plan, SSA for apply
- **Minimum K8s version:** 1.28

## Validated (Apr 2026)

PR #673 on hedgineer-bizops proved end-to-end:
- Provider loads from OCI mirror (`hedgineercicdacr`) — `Installed hedgineer/k8sconnect v0.3.7 (verified checksum)`
- Entra auth via kubelogin exec works — `data.k8sconnect_object.kube_system[0]: Read complete after 2s`
- SSA dry-run reads real K8s state during plan
- CA cert TLS verification works (`cluster_ca_certificate` from AKS module output)

### Auth gotchas discovered

- **ACR OAuth2 + OpenTofu:** `docker login` credentials from `az acr login` do NOT work — OpenTofu ignores Docker credential helpers and doesn't present config.json credentials in ACR's OAuth2 token exchange. Must use `oci_credentials` block in generated `.tfrc`. This is staging-only; prod/clients use pls-proxy where simple `docker login` works.
- **kubelogin required:** k8sconnect talks to K8s API during plan (SSA dry-run). CI needs `az aks install-cli` to install kubelogin.
- **Inline connection requires CA cert or insecure=true:** Added `cluster_ca_certificate` output to aks-cluster module.

## Upstream Resources

- Repo: https://github.com/jmorris0x0/terraform-provider-k8sconnect
- Docs: `docs/` directory and `examples/`
- Current version: v0.3.7 (Feb 2025)

## What NOT to Change

- Do not modify provider source code without discussing first
- Do not rename the binary or change the provider address
- Do not add Hedgineer-specific provider features — contribute upstream instead
- Do not remove upstream's `.goreleaser.yml` — we reuse it for the build step
