package identity

import (
	"fmt"
	"sync"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

// None of these tests call GetToken. NewWorkloadIdentityCredential only validates its
// options; it reads the token file and talks to Azure AD on the first GetToken, so the
// credentials built here never touch the projected token path or the network.

const (
	clientA = "00000000-0000-0000-0000-00000000000a"
	clientB = "00000000-0000-0000-0000-00000000000b"
	tenantA = "11111111-1111-1111-1111-11111111111a"
	tenantB = "11111111-1111-1111-1111-11111111111b"
)

// resetCredentials empties the package cache now and again when the test ends, so each
// test starts from a cold cache and leaves nothing behind for the next one.
func resetCredentials(t *testing.T) {
	t.Helper()
	reset := func() {
		credentialsMu.Lock()
		credentials = map[credentialKey]*azidentity.WorkloadIdentityCredential{}
		credentialsMu.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

func cachedCount() int {
	credentialsMu.Lock()
	defer credentialsMu.Unlock()
	return len(credentials)
}

func mustCredential(t *testing.T, clientID, tenantID string) *azidentity.WorkloadIdentityCredential {
	t.Helper()
	cred, err := credential(clientID, tenantID)
	if err != nil {
		t.Fatalf("credential(%q, %q) returned error: %v", clientID, tenantID, err)
	}
	if cred == nil {
		t.Fatalf("credential(%q, %q) returned a nil credential without an error", clientID, tenantID)
	}
	return cred
}

func TestCredentialReturnsTheSameCredentialForTheSameIdentity(t *testing.T) {
	resetCredentials(t)

	first := mustCredential(t, clientA, tenantA)
	second := mustCredential(t, clientA, tenantA)

	if first != second {
		t.Fatalf("expected the cached credential %p on the second call, got a new one %p", first, second)
	}
	if got := cachedCount(); got != 1 {
		t.Fatalf("expected 1 cached credential, got %d", got)
	}
}

func TestCredentialKeepsIdentitiesApart(t *testing.T) {
	resetCredentials(t)

	base := mustCredential(t, clientA, tenantA)
	otherClient := mustCredential(t, clientB, tenantA)
	otherTenant := mustCredential(t, clientA, tenantB)
	otherBoth := mustCredential(t, clientB, tenantB)

	creds := map[string]*azidentity.WorkloadIdentityCredential{
		"clientA/tenantA": base,
		"clientB/tenantA": otherClient,
		"clientA/tenantB": otherTenant,
		"clientB/tenantB": otherBoth,
	}
	seen := map[*azidentity.WorkloadIdentityCredential]string{}
	for name, cred := range creds {
		if prev, ok := seen[cred]; ok {
			t.Fatalf("%s and %s share credential %p", prev, name, cred)
		}
		seen[cred] = name
	}
	if got := cachedCount(); got != len(creds) {
		t.Fatalf("expected %d cached credentials, got %d", len(creds), got)
	}

	// Each identity still resolves to its own cached credential afterwards.
	if again := mustCredential(t, clientB, tenantA); again != otherClient {
		t.Fatalf("clientB/tenantA: expected cached %p, got %p", otherClient, again)
	}
	if again := mustCredential(t, clientA, tenantB); again != otherTenant {
		t.Fatalf("clientA/tenantB: expected cached %p, got %p", otherTenant, again)
	}
}

func TestCredentialBuildsOneCredentialPerIdentityUnderConcurrency(t *testing.T) {
	resetCredentials(t)

	type identity struct{ clientID, tenantID string }
	identities := []identity{
		{clientA, tenantA},
		{clientB, tenantA},
		{clientA, tenantB},
		{clientB, tenantB},
		{"00000000-0000-0000-0000-00000000000c", tenantA},
	}

	const goroutines = 50
	results := make([]*azidentity.WorkloadIdentityCredential, goroutines)
	errs := make([]error, goroutines)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range goroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			id := identities[i%len(identities)]
			results[i], errs[i] = credential(id.clientID, id.tenantID)
		}(i)
	}
	close(start)
	wg.Wait()

	perIdentity := map[identity]*azidentity.WorkloadIdentityCredential{}
	for i := range goroutines {
		id := identities[i%len(identities)]
		if errs[i] != nil {
			t.Fatalf("goroutine %d (%v): unexpected error: %v", i, id, errs[i])
		}
		if results[i] == nil {
			t.Fatalf("goroutine %d (%v): nil credential", i, id)
		}
		if want, ok := perIdentity[id]; ok && want != results[i] {
			t.Fatalf("goroutine %d (%v): got credential %p, another goroutine got %p for the same identity",
				i, id, results[i], want)
		}
		perIdentity[id] = results[i]
	}

	if len(perIdentity) != len(identities) {
		t.Fatalf("expected %d distinct identities, got %d", len(identities), len(perIdentity))
	}
	distinct := map[*azidentity.WorkloadIdentityCredential]bool{}
	for _, cred := range perIdentity {
		distinct[cred] = true
	}
	if len(distinct) != len(identities) {
		t.Fatalf("expected %d distinct credentials, got %d", len(identities), len(distinct))
	}
	if got := cachedCount(); got != len(identities) {
		t.Fatalf("expected %d cached credentials, got %d", len(identities), got)
	}
}

func TestCredentialDoesNotCacheARejectedIdentity(t *testing.T) {
	// An empty tenant ID makes azidentity fall back to AZURE_TENANT_ID. Setting it to the
	// empty string keeps that fallback empty, so the result does not depend on the
	// environment the tests run in.
	t.Setenv("AZURE_TENANT_ID", "")

	cases := []struct {
		name     string
		tenantID string
	}{
		{"empty tenant ID", ""},
		{"tenant ID with characters Azure AD rejects", "not a tenant!"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetCredentials(t)

			for attempt := 1; attempt <= 2; attempt++ {
				cred, err := credential(clientA, tc.tenantID)
				if err == nil {
					t.Fatalf("attempt %d: expected an error for tenant ID %q, got credential %p",
						attempt, tc.tenantID, cred)
				}
				if cred != nil {
					t.Fatalf("attempt %d: expected a nil credential alongside the error, got %p", attempt, cred)
				}
				if got := cachedCount(); got != 0 {
					t.Fatalf("attempt %d: expected nothing cached after a rejected identity, got %d entries",
						attempt, got)
				}
			}

			// A valid identity for the same client is still built and cached normally.
			mustCredential(t, clientA, tenantA)
			if got := cachedCount(); got != 1 {
				t.Fatalf("expected 1 cached credential after a valid call, got %d", got)
			}
		})
	}
}

func TestResetCredentialsIsolatesTests(t *testing.T) {
	t.Run("populate", func(t *testing.T) {
		resetCredentials(t)
		for i := range 3 {
			mustCredential(t, fmt.Sprintf("00000000-0000-0000-0000-00000000010%d", i), tenantA)
		}
		if got := cachedCount(); got != 3 {
			t.Fatalf("expected 3 cached credentials, got %d", got)
		}
	})
	t.Run("starts empty", func(t *testing.T) {
		if got := cachedCount(); got != 0 {
			t.Fatalf("expected the previous test's cleanup to empty the cache, got %d entries", got)
		}
	})
}
