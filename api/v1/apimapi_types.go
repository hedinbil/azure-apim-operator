package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// APIMAPISpec defines the desired state of APIMAPI.
// This spec contains the configuration needed to import and manage an API in Azure API Management.
type APIMAPISpec struct {
	// ServiceURL is the backend service URL that APIM will proxy requests to.
	ServiceURL string `json:"serviceUrl"`
	// RoutePrefix is the base route path in APIM (e.g., "/myapi").
	RoutePrefix string `json:"routePrefix"`
	// OpenAPIDefinitionURL is the URL where the OpenAPI/Swagger definition can be fetched.
	OpenAPIDefinitionURL string `json:"openApiDefinitionUrl"`
	// ProductIDs is a list of product IDs to associate this API with in APIM.
	// Products are used to group APIs and require subscriptions.
	ProductIDs []string `json:"productIds,omitempty"`
	// TagIDs is a list of tag IDs to apply to this API in APIM.
	// Tags are used for categorization and organization.
	TagIDs []string `json:"tagIds,omitempty"`
	// APIMService is the name of the APIMService custom resource that references
	// the Azure API Management service instance.
	APIMService string `json:"apimService"`
	// APIID is the unique identifier for the API in Azure APIM.
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
