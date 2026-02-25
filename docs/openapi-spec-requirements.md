# OpenAPI Spec Requirements for Azure APIM

This document explains what Azure API Management expects from your OpenAPI specification, with a focus on `operationId` -- the single most important field for reliable, repeatable API imports.

## How the Operator Imports Specs

The operator fetches your application's OpenAPI/Swagger JSON and sends it **as-is** to the Azure APIM Management REST API. There is no parsing, validation, or transformation. Whatever your application produces is exactly what APIM receives.

This means **your application is responsible for producing a complete, APIM-compatible OpenAPI spec**. If the spec is missing required fields, APIM may silently generate defaults on first import but fail on subsequent imports.

## The operationId Problem

### What Is operationId?

In the OpenAPI specification, `operationId` is an optional string on each operation (endpoint) that serves as a unique identifier:

```json
{
  "paths": {
    "/v1/payments/{customer}/FI/nordea/": {
      "post": {
        "operationId": "UploadPayment_FI_Nordea",
        "summary": "Upload a payment file",
        ...
      }
    }
  }
}
```

While OpenAPI treats it as optional, **Azure APIM relies on it for idempotent re-imports**.

### Why APIM Needs operationId

When you import an OpenAPI spec into APIM, each operation becomes an Azure resource with a resource name. The import process works like this:

1. **First import:** APIM creates operations. If `operationId` is present, it becomes the Azure resource name. If absent, APIM auto-generates one.
2. **Re-import:** APIM compares the `operationId` in the new spec against existing operation resource names.
   - **Match found:** Updates the operation in place.
   - **No match:** Tries to create a new operation.
   - **But the method + URL already exists** from the previous import: **ValidationError**.

### What Happens Without operationId

Without `operationId`, APIM auto-generates resource names on first import (e.g., `65a8e7d5b2c3f4a1`). On re-import, it generates **different** names, can't match them to existing operations, and attempts to create duplicates. This produces the following error:

```json
{
  "status": "Failed",
  "error": {
    "code": "ValidationError",
    "message": "One or more fields contain incorrect values:",
    "details": [
      {
        "code": "ValidationError",
        "message": "Operation with the same method and URL template already exists: POST, /v1/payments/bankconnect/{countryCode}/{customerName}/{bankRegistrationNumber}/payment",
        "target": "urlTemplate"
      }
    ]
  }
}
```

This breaks CI/CD pipelines that re-deploy API specs on every application update -- which is exactly what this operator does.

### The Rule

> Every operation in your OpenAPI spec **must** have a unique, stable `operationId`.

Once an `operationId` is deployed to APIM, do **not** rename it unless you also delete the old operation in APIM first. Renaming causes APIM to see it as a new operation while the old one still exists.

## Setting operationId by Framework

### ASP.NET Core Minimal APIs

Use `.WithName()` in the endpoint's fluent chain. This sets both the endpoint name and the `operationId` in the generated OpenAPI spec.

```csharp
app.MapPost("/v1/payments/{customer}/FI/nordea/", handler)
    .RequireAuthorization(ApiPolicies.OAuth2)
    .Produces<PaymentResponseDto>(200)
    .Produces(400)
    .WithName("UploadPayment_FI_Nordea")
    .AddOpenApiOperationTransformer((operation, context, ct) =>
    {
        operation.Description = "Upload a payment file for Finland Nordea";
        return Task.CompletedTask;
    });
```

**Placement:** `.WithName()` should be placed after `.Produces()` / `.RequireAuthorization()` and before `.AddOpenApiOperationTransformer()` (if present).

### ASP.NET Core Controllers

Use the `Name` parameter on the HTTP method attribute:

```csharp
[HttpGet("paymentfeedback", Name = "GetPaymentFeedback_FI_Nordea")]
public async Task<IActionResult> GetPaymentFeedback(
    [FromRoute] string customer,
    CancellationToken cancellationToken)
{
    // ...
}
```

### NestJS

Use the `@ApiOperation` decorator:

```typescript
@Post('payments')
@ApiOperation({ operationId: 'UploadPayment_FI_Nordea' })
async uploadPayment(@Body() dto: UploadPaymentDto) {
  // ...
}
```

### FastAPI (Python)

Set `operation_id` on the route decorator:

```python
@app.post("/v1/payments/{customer}/FI/nordea/", operation_id="UploadPayment_FI_Nordea")
async def upload_payment(customer: str, request: PaymentRequest):
    ...
```

### Spring Boot

Use `@Operation` from Swagger annotations:

```java
@PostMapping("/v1/payments/{customer}/FI/nordea/")
@Operation(operationId = "UploadPayment_FI_Nordea")
public ResponseEntity<PaymentResponse> uploadPayment(
    @PathVariable String customer,
    @RequestBody PaymentRequest request) {
    // ...
}
```

## Naming Conventions

Choose a consistent naming convention and apply it across all endpoints. Recommended pattern:

```
{Action}_{CountryCode}_{Provider}
```

Examples:

| Endpoint | operationId |
|----------|-------------|
| `POST /v1/payments/{customer}/FI/nordea/` | `UploadPayment_FI_Nordea` |
| `GET /v1/payments/{customer}/FI/nordea/paymentfeedback` | `GetPaymentFeedback_FI_Nordea` |
| `GET /v1/payments/{customer}/FI/nordea/accountstatement` | `GetAccountStatement_FI_Nordea` |
| `GET /v1/payments/{customer}/DK/danskebank/incomingreferencepayment` | `GetIncomingReferencePayment_DK_DanskeBank` |
| `GET /v1/payments/bankconnect/{cc}/{cn}/{brn}/status` | `GetBankStatus_BankConnect` |
| `POST /v1/payments/bankconnect/{cc}/{cn}/{brn}/payment` | `TransferPayment_BankConnect` |
| `POST /v1/payments/bankconnect/{cc}/{cn}/{brn}/activate` | `ActivateBank_BankConnect` |

For APIs without country/provider context, use:

```
{Action}_{Module}
```

Examples: `ListUsers_Admin`, `CreateOrder_Checkout`, `GetHealthCheck_System`

**Rules:**

- Names must be **unique** across all endpoints in the API
- Use PascalCase for readability
- Derive the name from the HTTP method, resource, and distinguishing context
- Keep names stable -- renaming after deployment requires manual APIM cleanup

## Validating Your Spec

Before deploying, verify that your OpenAPI spec contains `operationId` on every operation.

### Check the live spec

```bash
curl -s http://localhost:5000/swagger/v1/swagger.json | jq '.paths | to_entries[] | .value | to_entries[] | {method: .key, operationId: .value.operationId}'
```

### Count operations vs operationIds

```bash
# Total operations
curl -s http://localhost:5000/swagger/v1/swagger.json | jq '[.paths | to_entries[] | .value | to_entries[] | select(.key != "parameters")] | length'

# Operations with operationId
curl -s http://localhost:5000/swagger/v1/swagger.json | jq '[.paths | to_entries[] | .value | to_entries[] | select(.value.operationId != null)] | length'
```

Both numbers should be equal. If they differ, some operations are missing `operationId`.

### Check for duplicates

```bash
curl -s http://localhost:5000/swagger/v1/swagger.json | jq '[.paths | to_entries[] | .value | to_entries[] | .value.operationId] | group_by(.) | map(select(length > 1)) | .[]'
```

This should produce no output. If it does, you have duplicate `operationId` values.

## Migration Guide: Adding operationId to an Existing API

If you already have APIs in APIM that were imported **without** `operationId`, adding `operationId` to your spec and re-importing will fail. APIM can't match the new `operationId`-based names to the old auto-generated names.

### Option A: Clean Slate (Recommended)

1. Add `.WithName()` (or equivalent) to all endpoints in your application
2. Delete the existing API from APIM (via Azure Portal, CLI, or the APIM REST API)
3. Re-import with the operator -- the API will be created fresh with proper operation resource names
4. All subsequent re-imports will be idempotent

```bash
# Delete the API from APIM
az apim api delete \
  --resource-group <rg> \
  --service-name <apim-name> \
  --api-id <api-id> \
  --yes
```

### Option B: Delete Individual Operations

If you can't delete the entire API (e.g., it has policies or subscriptions you need to preserve):

1. List the existing operations and their resource names
2. Delete the operations that will conflict
3. Re-import with `operationId` values in the spec

```bash
# List operations
az apim api operation list \
  --resource-group <rg> \
  --service-name <apim-name> \
  --api-id <api-id> \
  --output table

# Delete a specific operation
az apim api operation delete \
  --resource-group <rg> \
  --service-name <apim-name> \
  --api-id <api-id> \
  --operation-id <auto-generated-name> \
  --yes
```

### Option C: Match Existing Names

If you want zero downtime, you can set your `operationId` values to match the auto-generated resource names already in APIM. List the current operations, note their resource names, and use those as your `operationId` values. This is fragile and not recommended for long-term use.

## Other OpenAPI Best Practices for APIM

Beyond `operationId`, consider these practices for a clean APIM integration:

| Field | Recommendation |
|-------|---------------|
| `info.title` | Set a clear API title -- it becomes the display name in APIM |
| `info.version` | Use semantic versioning -- APIM displays this |
| `servers` | Not used by the operator (it sets the service URL separately) |
| `description` on operations | Displayed in the APIM developer portal |
| `parameters` descriptions | Displayed in the developer portal and used for testing |
| `produces`/`consumes` (OAS 2) or `content` (OAS 3) | Defines request/response content types in APIM |
| `security` definitions | APIM imports these but the operator manages auth separately |

## Interaction with APIMInboundPolicy

The `operationId` in your OpenAPI spec is also used when applying operation-level policies via the `APIMInboundPolicy` CRD. The `spec.operationId` field on the policy resource must match the `operationId` from the imported spec.

Without `operationId` in the spec, you cannot reliably target specific operations with policies, since APIM's auto-generated names are unpredictable and change on re-import.

## Summary

1. Add `operationId` to every operation in your OpenAPI spec
2. Use stable, unique, descriptive names following a consistent convention
3. Never rename an `operationId` after deployment without cleaning up APIM first
4. Validate your spec before deploying (check count and uniqueness)
5. For existing APIs without `operationId`, delete from APIM and re-import cleanly
