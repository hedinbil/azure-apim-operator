# Open Source Migration Checklist

This document tracks the changes made to prepare the repository for open source release.

## ✅ Completed Changes

### Critical Security & Legal
- [x] **Removed hardcoded secrets** from `scripts/test.sh` (subscription key and customer API URL)
- [x] **Created LICENSE file** (Apache 2.0) to match copyright notices
- [x] **Fixed conflicting copyright statements** in README.md (removed "All rights reserved")

### Domain & Branding Changes
- [x] **Updated CRD group domain** from `apim.operator.io` to `apim.operator.io` in:
  - `api/v1/groupversion_info.go`
  - `cmd/main.go` (leader election ID)
  - `PROJECT` (Kubebuilder configuration)
- [x] **Updated Helm chart defaults**:
  - Image repository changed from `hedinit.azurecr.io` to `ghcr.io/hedinit/azure-apim-operator`
  - Swagger annotation key changed from `hedinit.io/openapi-export` to `operator.io/openapi-export`
- [x] **Updated README.md** to use new domain in all examples

### Documentation & Governance
- [x] **Created CONTRIBUTING.md** with contribution guidelines
- [x] **Created SECURITY.md** with vulnerability disclosure policy
- [x] **Created CODE_OF_CONDUCT.md** with community standards

### Code Improvements
- [x] **Made telemetry optional** in `internal/logger/logger.go`:
  - Telemetry disabled if `OTEL_EXPORTER_OTLP_ENDPOINT` is not set
  - Added TLS support with secure defaults
  - Added `OTEL_EXPORTER_OTLP_INSECURE` environment variable for local development

## ⚠️ Required Next Steps

### 1. Regenerate CRDs (CRITICAL)
After the domain change, you **must** regenerate all CRDs:

```bash
make manifests
```

This will update:
- `config/crd/bases/*.yaml` files
- Helm chart CRDs in `charts/azure-apim-operator/crds/*.yaml`

**Note**: This is a **breaking change**. Existing installations using `apim.operator.io` will need to migrate.

### 2. Domain Ownership Decision
The domain `apim.operator.io` is used as a placeholder. You should:
- Decide on the final domain you control (e.g., `apim.azure-operator.io`)
- Update all references if different from `apim.operator.io`
- Ensure the domain is registered or use a domain you own

### 3. Secret Rotation
**IMPORTANT**: The subscription key in `scripts/test.sh` was previously exposed. You must:
- Rotate/revoke the exposed subscription key in Azure
- Consider scrubbing git history if the key was committed

### 4. Update Sample Files
Update sample manifests in `config/samples/` to use the new domain:
- `apim_v1_apimapi.yaml`
- `apim_v1_apimservice.yaml`
- `apim_v1_apimproduct.yaml`
- `apim_v1_apimtag.yaml`
- `apim_v1_apiminboundpolicy.yaml`

### 5. Update RBAC Files
RBAC files in `config/rbac/` reference the old domain. After regenerating manifests, these should be updated automatically, but verify:
- All `*_admin_role.yaml`
- All `*_editor_role.yaml`
- All `*_viewer_role.yaml`

### 6. CI/CD Updates
Review and update GitHub Actions workflows:
- Parameterize registry/auth settings
- Add conditional gating for private workflows
- Provide community-friendly build paths

### 7. Additional Considerations
- [ ] Review and update any remaining `hedinit.io` references in code comments
- [ ] Update `go.mod` module path if repository moves to a different organization
- [ ] Verify all sample configurations work with the new domain
- [ ] Test CRD installation and migration path
- [ ] Update any external documentation or blog posts

## Migration Guide for Existing Users

If you have existing installations using `apim.operator.io`:

1. **Backup existing resources**: Export all CRDs and custom resources
2. **Uninstall old operator**: Remove the old CRDs and operator
3. **Install new operator**: Deploy with new CRDs using `apim.operator.io`
4. **Recreate resources**: Apply your custom resources with updated `apiVersion`

## Testing Checklist

Before releasing:
- [ ] All tests pass with new domain
- [ ] CRDs install correctly
- [ ] Operator starts and reconciles resources
- [ ] Sample manifests work end-to-end
- [ ] Helm chart installs successfully
- [ ] Telemetry is optional and works when enabled
- [ ] No hardcoded secrets remain in codebase

## Notes

- The domain change is **breaking** and requires CRD regeneration
- Consider versioning this as a major release (v2.0.0)
- Document the migration path for existing users
- The new domain should be one you control or a neutral community domain

