/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"os"
	"strings"
)

// Phase constants for status tracking across controllers.
const (
	phaseError   = "Error"   // Indicates an error occurred during resource creation/update.
	phaseCreated = "Created" // Indicates the resource was successfully created or updated.
)

// Error message constants shared across controllers.
const (
	errMsgFailedToGetAzureToken = "Failed to get Azure token"
)

// getOperatorNamespace returns the namespace where the operator is running.
// It first tries to read from the service account namespace file (production),
// then falls back to the OPERATOR_NAMESPACE environment variable (for testing),
// and finally defaults to "default" if neither is available.
func getOperatorNamespace() (string, error) {
	// First, try to read from the service account namespace file (production)
	nsBytes, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err == nil {
		return strings.TrimSpace(string(nsBytes)), nil
	}

	// Fall back to environment variable (useful for testing)
	if ns := os.Getenv("OPERATOR_NAMESPACE"); ns != "" {
		return ns, nil
	}

	// Default to "default" namespace if neither is available
	// This allows tests to work without setting up the service account file
	return "default", nil
}
