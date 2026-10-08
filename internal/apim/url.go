package apim

import (
	"net/http"
	"net/url"
	"strings"
	"time"
)

// armResponseHeaderTimeout bounds the wait for ARM to start answering one request, and
// armRequestTimeout the whole request including the body. Without them a half-open
// connection blocks a reconcile worker for good; with four workers per controller, four such
// requests stop every APIM write. No ARM call this package makes waits for an operation: a
// write that takes longer is answered with 202 at once (see startAPIWrite).
const (
	armResponseHeaderTimeout = 90 * time.Second
	armRequestTimeout        = 2 * time.Minute
)

// httpClient is the client every ARM call in this package goes through. A package variable
// so a test can swap the transport and see the exact request without a network.
var httpClient = newARMClient()

// newARMClient builds the ARM client: bounded in time, and never following a redirect. ARM
// does not redirect, and Go forwards the Authorization header on a redirect to the same
// host name on another port or scheme, past the IsOperationURL check.
func newARMClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = armResponseHeaderTimeout
	// Every worker of every controller talks to the same host; the default of 2 idle
	// connections per host would repeat the TLS handshake for most calls.
	transport.MaxIdleConnsPerHost = 16
	return &http.Client{
		Transport: transport,
		Timeout:   armRequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// apiVersion is the Azure API Management control-plane API version every
// request in this package targets.
const apiVersion = "2021-08-01"

// armHost is the Azure Resource Manager endpoint. A variable so a test can point the
// package at an httptest server standing in for ARM (see UseEndpoint).
var armHost = "https://management.azure.com"

// UseEndpoint points every ARM call in this package at host (scheme and authority, no
// trailing slash) through client, and returns a func that restores the previous values.
// It exists for tests in other packages, which run the whole write path against an
// httptest server; production code never calls it. Not safe to call while requests are
// in flight.
func UseEndpoint(host string, client *http.Client) (restore func()) {
	previousHost, previousClient := armHost, httpClient
	armHost, httpClient = host, client
	return func() { armHost, httpClient = previousHost, previousClient }
}

// serviceScope identifies one API Management instance. Every config type in
// this package satisfies it, which is what lets the URL builders below be
// shared instead of each call site formatting its own string.
type serviceScope interface {
	scope() (subscriptionID, resourceGroup, serviceName string)
}

func (c APIMDeploymentConfig) scope() (string, string, string) {
	return c.SubscriptionID, c.ResourceGroup, c.ServiceName
}

func (c APIMProductConfig) scope() (string, string, string) {
	return c.SubscriptionID, c.ResourceGroup, c.ServiceName
}

func (c APIMTagConfig) scope() (string, string, string) {
	return c.SubscriptionID, c.ResourceGroup, c.ServiceName
}

func (c APIMInboundPolicyConfig) scope() (string, string, string) {
	return c.SubscriptionID, c.ResourceGroup, c.ServiceName
}

// serviceURL builds an ARM URL for a path under an API Management instance.
//
// Every segment is percent-escaped, including the ones that come straight from
// a custom resource: an apiId, productId, tagId or operationId containing "/",
// "?", "#" or ".." used to change which resource the request targeted, because
// http.NewRequestWithContext parses whatever string it is handed. CRD patterns
// now reject those characters as well, so this is the second of two layers
// (APIM-16).
//
// Pass segments in path order, e.g. serviceURL(cfg, "apis", apiID, "policies",
// "policy"). Fixed path words are escaped too, which is a no-op for them.
func serviceURL(cfg serviceScope, segments ...string) string {
	escaped := make([]string, len(segments))
	for i, segment := range segments {
		escaped[i] = url.PathEscape(segment)
	}
	return serviceURLEscaped(cfg, escaped...)
}

// serviceURLEscaped is serviceURL for segments that are already escaped. Only
// revisionURL needs it; everything else goes through serviceURL.
func serviceURLEscaped(cfg serviceScope, segments ...string) string {
	subscriptionID, resourceGroup, serviceName := cfg.scope()
	var b strings.Builder
	b.WriteString(armHost)
	b.WriteString("/subscriptions/")
	b.WriteString(url.PathEscape(subscriptionID))
	b.WriteString("/resourceGroups/")
	b.WriteString(url.PathEscape(resourceGroup))
	b.WriteString("/providers/Microsoft.ApiManagement/service/")
	b.WriteString(url.PathEscape(serviceName))
	for _, segment := range segments {
		b.WriteString("/")
		b.WriteString(segment)
	}
	b.WriteString("?api-version=")
	b.WriteString(apiVersion)
	return b.String()
}

// withQuery appends one query parameter to a URL built by serviceURL. Every
// such URL already carries ?api-version=, so the parameter is joined with "&";
// both halves are query-escaped because the value may come from a resource.
func withQuery(u, key, value string) string {
	return u + "&" + url.QueryEscape(key) + "=" + url.QueryEscape(value)
}

// revisionURL builds the URL of one API, optionally a specific revision. APIM
// addresses a revision as "{apiId};rev={n}" and ARM needs that ";" literally,
// so the two halves are escaped separately and joined here; passing the whole
// thing through serviceURL would escape the separator into %3B.
func revisionURL(cfg serviceScope, apiID, revision string) string {
	segment := url.PathEscape(apiID)
	if revision != "" {
		segment += ";rev=" + url.PathEscape(revision)
	}
	return serviceURLEscaped(cfg, url.PathEscape("apis"), segment)
}
