package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// APIMAPIDeploymentSpec defines the desired state of APIMAPIDeployment.
// This spec contains all the information needed to deploy an API to Azure API Management,
// including the OpenAPI definition, service URL, route configuration, and associations.
type APIMAPIDeploymentSpec struct {
	// ServiceURL is the backend service URL that APIM will proxy requests to.
	ServiceURL string `json:"serviceUrl"`
	// RoutePrefix is the base route path in APIM (e.g., "/myapi").
	RoutePrefix string `json:"routePrefix"`
	// OpenAPIDefinitionURL is the URL where the OpenAPI/Swagger definition can be fetched.
	OpenAPIDefinitionURL string `json:"openApiDefinitionUrl"`
	// ProductIDs is a list of product IDs to associate this API with in APIM.
	ProductIDs []string `json:"productIds,omitempty"`
	// TagIDs is a list of tag IDs to apply to this API in APIM.
	TagIDs []string `json:"tagIds,omitempty"`
	// APIMService is the name of the APIMService custom resource.
	APIMService string `json:"apimService"`
	// Subscription is the Azure subscription ID where the APIM service is deployed.
	Subscription string `json:"subscription"`
	// ResourceGroup is the Azure resource group where the APIM service is located.
	ResourceGroup string `json:"resourceGroup"`
	// APIID is the unique identifier for the API in Azure APIM.
	APIID string `json:"APIID"`
	// Revision is an optional API revision number. If specified, a new revision will be created.
	Revision string `json:"revision,omitempty"`
	// SubscriptionRequired controls whether a subscription key is required to access the API.
	// If set to false, the API can be accessed without a subscription key.
	// If not specified, defaults to true (subscription required).
	// +kubebuilder:default=true
	SubscriptionRequired bool `json:"subscriptionRequired"`
}

// APIMAPIDeploymentStatus defines the observed state of APIMAPIDeployment.
// This status tracks the deployment progress and result.
type APIMAPIDeploymentStatus struct {
	// ImportedAt is the timestamp when the API was successfully imported into APIM.
	ImportedAt string `json:"importedAt,omitempty"`
	// Status indicates the current deployment status (e.g., "OK", "Error").
	Status string `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// APIMAPIDeployment is the Schema for the APIMAPIDeployments API
type APIMAPIDeployment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   APIMAPIDeploymentSpec   `json:"spec,omitempty"`
	Status APIMAPIDeploymentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// APIMAPIDeploymentList contains a list of APIMAPIDeployment
type APIMAPIDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []APIMAPIDeployment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&APIMAPIDeployment{}, &APIMAPIDeploymentList{})
}
