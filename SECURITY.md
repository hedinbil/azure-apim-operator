# Security Policy

## Supported Versions

We provide security updates for the following versions:

| Version | Supported          |
| ------- | ------------------ |
| Latest  | :white_check_mark: |
| < Latest | :x:                |

## Reporting a Vulnerability

We take security vulnerabilities seriously. If you discover a security vulnerability, please follow these steps:

### 1. **Do NOT** open a public issue

Security vulnerabilities should be reported privately to prevent exploitation.

### 2. Report the vulnerability

Please email security concerns to: **security@hedinit.com**

Include the following information:
- Description of the vulnerability
- Steps to reproduce
- Potential impact
- Suggested fix (if any)
- Your contact information

### 3. Response timeline

- **Initial Response**: Within 48 hours
- **Status Update**: Within 7 days
- **Resolution**: Depends on severity and complexity

### 4. Disclosure policy

- We will acknowledge receipt of your report
- We will work with you to understand and resolve the issue
- We will notify you when the vulnerability is fixed
- We will credit you in the security advisory (if desired)
- Public disclosure will be coordinated with you

## Security Best Practices

When using this operator:

1. **Authentication**: Use Azure Workload Identity or secure Service Principal credentials
2. **RBAC**: Follow least-privilege principles for operator permissions
3. **Network Policies**: Restrict operator network access where possible
4. **Secrets Management**: Never commit secrets or credentials to version control
5. **Updates**: Keep the operator and dependencies up to date
6. **Monitoring**: Monitor operator logs and metrics for suspicious activity

## Known Security Considerations

- The operator requires permissions to manage Azure API Management resources
- The operator may fetch OpenAPI specifications from application endpoints
- Telemetry data may be sent to external services (configurable)

## Security Updates

Security updates will be:
- Released as patch versions
- Documented in release notes
- Tagged with the `security` label in releases

## Thank You

We appreciate your help in keeping the Azure APIM Operator secure!

