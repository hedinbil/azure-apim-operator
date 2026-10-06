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
	"context"

	"github.com/hedinit/azure-apim-operator/internal/identity"
)

// managementTokenFunc obtains a bearer token for the Azure Management API. Every
// reconciler that writes to APIM carries one as a test seam, so envtest can run the
// whole write path against a fake ARM; nil means identity.GetManagementToken.
type managementTokenFunc func(ctx context.Context, clientID, tenantID string) (string, error)

// get calls f, or identity.GetManagementToken when f is nil.
func (f managementTokenFunc) get(ctx context.Context, clientID, tenantID string) (string, error) {
	if f == nil {
		return identity.GetManagementToken(ctx, clientID, tenantID)
	}
	return f(ctx, clientID, tenantID)
}
