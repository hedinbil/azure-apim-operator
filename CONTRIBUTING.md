# Contributing to Azure APIM Operator

Thank you for your interest in contributing to the Azure APIM Operator! This document provides guidelines and instructions for contributing.

## Code of Conduct

This project adheres to a Code of Conduct that all contributors are expected to follow. Please read [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) before contributing.

## How to Contribute

### Reporting Issues

- **Bug Reports**: Use the GitHub issue tracker to report bugs. Please include:
  - A clear description of the issue
  - Steps to reproduce
  - Expected vs. actual behavior
  - Kubernetes and operator versions
  - Relevant logs or error messages

- **Feature Requests**: Open an issue with the `enhancement` label describing:
  - The use case and motivation
  - Proposed solution or design
  - Any alternatives considered

### Submitting Changes

1. **Fork the Repository**: Create your own fork of the repository.

2. **Create a Branch**: Create a feature branch from `main`:
   ```bash
   git checkout -b feature/your-feature-name
   ```

3. **Make Changes**: 
   - Follow the existing code style and conventions
   - Add tests for new functionality
   - Update documentation as needed
   - Ensure all tests pass locally

4. **Commit Your Changes**: Write clear, descriptive commit messages:
   ```bash
   git commit -m "Add feature: brief description"
   ```

5. **Push and Create Pull Request**: 
   - Push your branch to your fork
   - Open a Pull Request against the `main` branch
   - Fill out the PR template with a clear description

### Development Setup

#### Prerequisites

- Go 1.21 or later
- Kubernetes cluster (local or remote)
- kubectl configured
- Docker (for building images)
- Kubebuilder (for CRD generation)

#### Building from Source

```bash
# Clone the repository
git clone https://github.com/hedinit/azure-apim-operator.git
cd azure-apim-operator

# Install dependencies
go mod download

# Build the operator
make build

# Run tests
make test

# Generate manifests
make manifests
```

#### Running Locally

```bash
# Install CRDs
make install

# Run the operator locally
make run
```

### Code Standards

- **Go Style**: Follow standard Go formatting (`gofmt`, `golint`)
- **Comments**: Document exported functions and types
- **Error Handling**: Always handle errors explicitly
- **Testing**: Write unit tests for new functionality
- **Logging**: Use structured logging with appropriate levels

### Testing

- Run unit tests: `make test`
- Run integration tests: `make test-integration`
- Run e2e tests: `make test-e2e`

### Documentation

- Update README.md for user-facing changes
- Add code comments for complex logic
- Update API documentation for CRD changes
- Include examples in PR descriptions

### Review Process

- All PRs require at least one approval
- CI checks must pass
- Code review feedback should be addressed
- Maintainers may request changes before merging

## Questions?

If you have questions about contributing, please:
- Open a GitHub Discussion
- Check existing issues for similar questions
- Review the documentation in the README

Thank you for contributing! 🎉

