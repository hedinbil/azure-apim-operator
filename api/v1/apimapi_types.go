package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// API types an APIMAPI can declare. They map to the "type" property of an
// API in Azure API Management.
const (
	// APITypeHTTP is an HTTP API whose operations are imported from an OpenAPI document.
	APITypeHTTP = "http"
	// APITypeWebSocket is a WebSocket API. APIM models it as its own API type with a
	// single onHandshake operation and no OpenAPI document, which is what SignalR and
	// other socket servers need.
	APITypeWebSocket = "websocket"
)

// APIMAPIWebSocket holds the settings that only a websocket API has. It sits under
// spec.websocket and is only allowed when spec.type is "websocket", so an http API
// cannot set fields that would be silently ignored.
type APIMAPIWebSocket struct {
	// DisplayName is the name shown for the API in the Azure portal and developer portal.
	// An http API takes its name from the OpenAPI document instead. Defaults to APIID.
	// +kubebuilder:validation:MaxLength=300
	DisplayName string `json:"displayName,omitempty"`
	// Protocols lists the protocols the API is exposed on in APIM. Defaults to ["wss"].
	// +kubebuilder:validation:MaxItems=2
	// +kubebuilder:validation:items:Enum=ws;wss
	Protocols []string `json:"protocols,omitempty"`
}

// APIMAPITarget defines how an APIMAPI maps to workloads in the cluster.
// When Selector is omitted, the legacy behavior is used and the APIMAPI name
// must match the ReplicaSet app.kubernetes.io/name label.
type APIMAPITarget struct {
	// Selector matches ReplicaSets whose readiness events should trigger this API import.
	Selector *metav1.LabelSelector `json:"selector,omitempty"`
}

// APIMAPISpec defines the desired state of APIMAPI.
// This spec contains the configuration needed to import and manage an API in Azure API Management.
// +kubebuilder:validation:XValidation:rule="(has(self.type) && self.type == 'websocket') || (has(self.openApiDefinitionUrl) && size(self.openApiDefinitionUrl) > 0)",message="openApiDefinitionUrl is required unless type is websocket"
// +kubebuilder:validation:XValidation:rule="!(has(self.type) && self.type == 'websocket') || self.serviceUrl.startsWith('ws://') || self.serviceUrl.startsWith('wss://')",message="a websocket API needs a ws:// or wss:// serviceUrl"
// +kubebuilder:validation:XValidation:rule="(has(self.type) && self.type == 'websocket') || self.serviceUrl.startsWith('http://') || self.serviceUrl.startsWith('https://')",message="an http API needs an http:// or https:// serviceUrl"
// +kubebuilder:validation:XValidation:rule="(has(self.type) && self.type == 'websocket') || !has(self.websocket)",message="the websocket block is only allowed when type is websocket"
type APIMAPISpec struct {
	// Type selects the kind of API to create in APIM. "http" (the default) imports the
	// OpenAPI document at OpenAPIDefinitionURL. "websocket" creates a WebSocket API from
	// ServiceURL alone; APIM adds the onHandshake operation itself and no OpenAPI document
	// is fetched. Settings that only apply to one type live in a block named after it.
	// +kubebuilder:validation:Enum=http;websocket
	// +kubebuilder:default=http
	Type string `json:"type,omitempty"`
	// WebSocket holds websocket-only settings. Optional even for websocket APIs; only
	// allowed when Type is "websocket".
	WebSocket *APIMAPIWebSocket `json:"websocket,omitempty"`
	// ServiceURL is the backend service URL that APIM will proxy requests to.
	// http(s) for HTTP APIs, ws(s) for websocket APIs.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^(https?|wss?)://`
	// +kubebuilder:validation:MaxLength=2048
	ServiceURL string `json:"serviceUrl"`
	// RoutePrefix is the base route path in APIM (e.g., "/myapi").
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9._~/-]*$`
	// +kubebuilder:validation:MaxLength=400
	RoutePrefix string `json:"routePrefix"`
	// OpenAPIDefinitionURL is the URL where the OpenAPI/Swagger definition can be fetched.
	// Required for HTTP APIs, ignored for websocket APIs.
	// +kubebuilder:validation:Pattern=`^https?://`
	// +kubebuilder:validation:MaxLength=2048
	OpenAPIDefinitionURL string `json:"openApiDefinitionUrl,omitempty"`
	// Target optionally selects which ReplicaSets should trigger imports for this API.
	// If omitted, the operator falls back to matching metadata.name with the
	// ReplicaSet app.kubernetes.io/name label.
	Target *APIMAPITarget `json:"target,omitempty"`
	// ProductIDs is a list of product IDs to associate this API with in APIM.
	// Products are used to group APIs and require subscriptions.
	ProductIDs []string `json:"productIds,omitempty"`
	// TagIDs is a list of tag IDs to apply to this API in APIM.
	// Tags are used for categorization and organization.
	TagIDs []string `json:"tagIds,omitempty"`
	// APIMService is the name of the APIMService custom resource that references
	// the Azure API Management service instance.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9][a-zA-Z0-9-]{0,48}[a-zA-Z0-9]$`
	// +kubebuilder:validation:MaxLength=50
	APIMService string `json:"apimService"`
	// APIID is the unique identifier for the API in Azure APIM.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,78}[a-zA-Z0-9]$|^[a-zA-Z0-9]$`
	// +kubebuilder:validation:MaxLength=80
	APIID string `json:"APIID"`
	// SubscriptionRequired controls whether a subscription key is required to access the API.
	// If set to false, the API can be accessed without a subscription key.
	// If not specified, defaults to true (subscription required).
	// +kubebuilder:default=true
	SubscriptionRequired bool `json:"subscriptionRequired"`
}

// APIMAPIStatus defines the observed state of APIMAPI.
// This status reflects the current state of the API in Azure APIM.
type APIMAPIStatus struct {
	// ImportedAt is the timestamp when the API was successfully imported into APIM.
	ImportedAt string `json:"importedAt,omitempty"`
	// Status indicates the current status of the API (e.g., "OK", "Error").
	Status string `json:"status,omitempty"`
	// ApiHost is the full URL to access the API through APIM (e.g., "https://api.example.com/myapi").
	ApiHost string `json:"apiHost"`
	// DeveloperPortalHost is the URL of the APIM developer portal.
	DeveloperPortalHost string `json:"developerPortalHost"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// APIMAPI is the Schema for the apimapis API.
// APIMAPI is a Kubernetes custom resource that represents an API in Azure API Management.
// When created, it triggers the import of an OpenAPI definition into APIM and configures
// the API with the specified route prefix, service URL, products, and tags.
type APIMAPI struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec defines the desired state of the API in APIM.
	Spec APIMAPISpec `json:"spec,omitempty"`
	// Status reflects the observed state of the API in APIM.
	Status APIMAPIStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// APIMAPIList contains a list of APIMAPI resources.
// This is used by kubectl to list all APIMAPI instances in a namespace.
type APIMAPIList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	// Items is the list of APIMAPI resources.
	Items []APIMAPI `json:"items"`
}

// init registers the APIMAPI and APIMAPIList types with the Kubernetes scheme.
// This is required for the Kubernetes API server to recognize these custom resources.
func init() {
	SchemeBuilder.Register(&APIMAPI{}, &APIMAPIList{})
}
