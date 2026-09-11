package apim

import (
	"net/url"
	"strings"
)

// apiVersion is the Azure API Management control-plane API version every
// request in this package targets.
const apiVersion = "2021-08-01"

// armHost is the Azure Resource Manager endpoint.
const armHost = "https://management.azure.com"

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
