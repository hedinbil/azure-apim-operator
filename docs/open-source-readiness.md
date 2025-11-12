# Open Source Readiness Assessment

## Executive Summary
- The repository lacks essential legal artifacts (e.g., LICENSE file) while claiming Apache 2.0 in `README.md:959` and showing `**© 2025 Hedin IT - All rights reserved**` in `README.md:973`, creating conflicting terms that must be resolved before publication.
- Sensitive data is present in `scripts/test.sh:6` and `scripts/test.sh:9` (customer API URL plus live subscription key); these must be revoked and scrubbed.
- Core identifiers (Go module, CRD group, chart defaults, automation scripts, documentation) hardcode Hedin-specific domains (`hedinit.io`, `hedinit.com`) and private infrastructure (Azure Container Registry, Azure subscriptions), blocking external adoption.
- CI/CD workflows and helper scripts assume access to Hedin Azure resources and credentials (`.github/workflows/build-docker-image.yml:25-65`, `.github/workflows/build-helm-chart.yml:30-70`, `scripts/docker.sh:6-33`, `scripts/helm.sh:5-40`, `scripts/helm2.sh:5-48`); these must be generalized or guarded for community use.
- Significant scaffolding and TODOs remain (sample manifests, controller tests, monitoring setup), and there is no contributor or security policy, so governance and quality baselines are undefined.

## Licensing & Legal
- **Missing LICENSE artifact**: No `LICENSE` file exists in the repository despite Apache 2.0 notices (e.g., `cmd/main.go:1-14`, `hack/boilerplate.go.txt:1-15`). Publish a definitive license file that matches the intended terms.
- **Conflicting statements**: `README.md:959` claims Apache 2.0, but `README.md:973` asserts “All rights reserved,” which contradicts open-source licensing. Remove or reword company-exclusive language.
- **Copyright and branding**: Review whether the boxed shields/logos in `README.md:6-14` require attribution.
- **Dependency licensing**: The project pulls numerous third-party modules (see `go.mod:7-109`). Perform a proper license audit (SPDX bill of materials, e.g., `github.com/Azure/azure-sdk-for-go`, `go.opentelemetry.io/otel`, `k8s.io/*`, `github.com/onsi/*`). Confirm compatibility with the chosen OSS license and document any obligations.

## Sensitive Data & Security
- **Hardcoded secret**: `scripts/test.sh:9` contains `SUBSCRIPTION_KEY="a41a90fea9314469a885e13fb3ac7023"`. Treat it as compromised, rotate upstream, purge from history if necessary, and replace with placeholders. Also sanitize the associated endpoint in `scripts/test.sh:6`.
- **Default endpoints**: `scripts/test.sh:6` targets an internal environment (`https://api-dev.hedinit.com/...`). Replace with mock/demo values or externalize configuration.
- **Insecure telemetry defaults**: `internal/logger/logger.go:26-60` dials OTLP with `grpc.WithTransportCredentials(insecure.NewCredentials())`. Either allow TLS configuration or document the security implications.
- **Secret handling**: Ensure Azure credentials referenced in workflows and scripts are injected via GitHub secrets/variables (currently assumed), document requirements, and guard against accidental leakage.
- **Security process**: Provide a public vulnerability disclosure policy (e.g., `SECURITY.md`) with triage contacts.

## Proprietary Branding & Hardcoded Identifiers
- **Go module path**: `go.mod:1` uses `github.com/hedinit/azure-apim-operator`. Decide on the public namespace and adjust imports.
- **Kubebuilder domain**: CRD group names use `apim.operator.io` throughout (`api/v1/groupversion_info.go:19`, `config/crd/bases/...`, `config/samples/*`). For open source, adopt a neutral domain you control (e.g., `apim.azure-operator.io`) and regenerate CRDs/code.
- **Leader election ID**: `cmd/main.go:195` references `50287eb5.hedinit.io`; update to match the new domain to avoid collisions.
- **Helm chart defaults**: `charts/azure-apim-operator/values.yaml:10` pins the image repository to `hedinit.azurecr.io/azure-apim-operator`; `charts/azure-apim-operator/values.yaml:167` defines `hedinit.io/openapi-export`. Replace with public-friendly defaults and document optional overrides.
- **Project metadata**: `PROJECT:5-105` hardcodes `domain: hedinit.io` and repository path. Update and re-run Kubebuilder scaffolding as needed.
- **Documentation**: `README.md` references Hedin-branded resources (`README.md:89`, `README.md:129`, `README.md:975`). Replace links or add notes for community forks.
- **Automation scripts**: `scripts/docker.sh:6-33`, `scripts/helm.sh:5-40`, and `scripts/helm2.sh:5-50` all assume Hedin’s Azure Container Registry. Parameterize via environment variables or provide public registry examples.

## Documentation & Governance
- **Missing foundational docs**: Add `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, `SECURITY.md`, onboarding docs, and release guidelines. Align with the License once finalized.
- **Docs folder**: Prior to this assessment, `/docs` did not exist; consider a comprehensive user/developer guide and API reference.
- **README accuracy**: Remove references to private issue trackers (`README.md:975`) if the project moves to a different organization. Clarify support expectations and compatibility guarantees.
- **Commented TODOs**: Numerous `TODO(user)` markers remain (see `config/samples/*.yaml`, `internal/controller/*_test.go`, `Makefile:64`, `cmd/main.go:166-174`). Plan either to resolve them or clearly mark features as experimental.
- **Language consistency**: `.github/workflows/agent-renova.yml:25-88` mixes Swedish comments and emoji; provide English documentation for external contributors.

## Build & Release Infrastructure
- **GitHub Actions**: Workflows such as `.github/workflows/build-docker-image.yml:25-95`, `.github/workflows/build-helm-chart.yml:30-119`, `.github/workflows/test-e2e.yml:1-45` depend on Hedin Azure credentials and registries. For OSS, add guard rails (conditional execution, public registry options, or separate community workflows).
- **Container publishing**: Provide instructions for building/pushing to a public registry (Docker Hub, GHCR). Parameterize `IMG` defaults in `Makefile:1-13`.
- **Versioning**: Establish semantic versioning guidelines and automated tagging/release notes.
- **Local tooling**: `.devcontainer/post-install.sh:1-25` downloads binaries over the public internet with `curl`; document supply-chain considerations or pin versions/checksums.
- **Telemetry defaults**: `internal/logger/logger.go:18-60` assumes Datadog; make telemetry optional behind build flags or configuration to avoid surprising community users.

## Configuration & Distribution Artifacts
- **Helm chart**: The chart bundles CRDs copied from `config/crd/bases/*.yaml` via `scripts/helm2.sh:25-47`. Once the group/domain change is made, regenerate the packaged CRDs and ensure licensing notices accompany them.
- **Chart values**: Duplicate `serviceAccount` blocks in `charts/azure-apim-operator/values.yaml:23-33` and `charts/azure-apim-operator/values.yaml:126-150` indicate drift—clean up defaults and document Workload Identity requirements.
- **Swagger annotation key**: `charts/azure-apim-operator/values.yaml:167` should be generalized.
- **E2E tests**: `test/e2e/e2e_suite_test.go:29-48` expects Docker/Kind; document prerequisites and optionally add containerized test harnesses.
- **Configuration samples**: `config/samples/*.yaml` contain placeholders (`# TODO(user): Add fields here`). Provide realistic community-ready examples with sanitized values.

## Dependencies & Technical Baseline
- **Go toolchain**: `go.mod:3-5` targets Go 1.23 with `godebug default=go1.23`. Verify that Go 1.23 is publicly available or downgrade to the latest stable release (1.22 as of Q1 2025) for broader compatibility.
- **Kubernetes stack**: Dependencies lock to `k8s.io/* v0.32.1`. Document the minimum supported Kubernetes version (likely 1.32) and consider adding compatibility tests for older clusters.
- **Azure Management API**: `internal/apim/apim.go:18-214` hardcodes API version `2021-08-01`. Confirm this is still current and configurable.
- **Observability**: Add guidance on integrating with other tracing backends or provide no-op default (current code requires Datadog env vars).
- **Security scanning**: Integrate `gosec`, `trivy`, or similar into CI to catch issues like the hardcoded key earlier.

## Testing & Quality
- **Unit tests**: Generated controller tests (e.g., `internal/controller/apimservice_controller_test.go:41-82`) still contain scaffolding comments and no assertions. Flesh these out or remove to avoid false confidence.
- **E2E coverage**: `test/e2e/e2e_test.go:43-210` focuses on operator availability/metrics but not API import flows. Expand scenarios (OpenAPI ingestion, product/tag assignment, error handling).
- **Automation robustness**: Scripts rely on `curl` and `az` CLI without verifying tool presence (`scripts/docker.sh`, `scripts/helm.sh`). Add pre-flight checks or document prerequisites.
- **TODO debt**: Track unresolved `TODO` markers to ensure they don’t ship in production code without a follow-up issue.

## Recommended Next Steps
1. **Legal cleanup**: Publish the definitive LICENSE file, remove conflicting “All rights reserved” language, and audit third-party licenses.
2. **Secret hygiene**: Revoke the leaked subscription key, scrub it from history, and replace all customer-specific URLs/values with placeholders or configuration.
3. **Namespace restructuring**: Decide on the public project domain/repository, update `go.mod`, Kubebuilder domain settings, Helm chart defaults, annotations, and regenerate CRDs/controllers.
4. **CI/CD rework**: Parameterize registry/auth settings, add conditional gating for private workflows, and supply community-friendly build paths.
5. **Documentation & governance**: Add CONTRIBUTING, CODE_OF_CONDUCT, SECURITY, release, and support docs; revise the README for an external audience.
6. **Testing hardening**: Replace scaffolded tests with meaningful coverage and expand e2e scenarios; integrate security scanning.
7. **Telemetry & config**: Make tracing optional or configurable with secure defaults, clean up chart values/service account duplication.
8. **Release process**: Define how versions are tagged, packages published, and changelog maintained once the project is public.

## Open Questions / Assumptions
- Does Hedin intend to maintain ownership of the CRD group domain after open sourcing? If not, reserve an alternative domain before release.
- Are there additional private dependencies (Azure subscriptions, petstore demo) that need sanitizing, or can equivalent public resources be stood up?
- Should community builds include optional features (Datadog, Azure Workload Identity), or will those remain Hedin-specific extensions?
- Confirm whether any customer data was ever stored in this repository beyond the exposed subscription key.
