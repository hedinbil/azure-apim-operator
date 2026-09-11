package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// APIMAPIDeploymentSpec defines the desired state of APIMAPIDeployment.
// This spec contains all the information needed to deploy an API to Azure API Management,
// including the OpenAPI definition, service URL, route configuration, and associations.
type APIMAPIDeploymentSpec struct {
	// ServiceURL is the backend service URL that APIM will proxy requests to.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^https?://`
	// +kubebuilder:validation:MaxLength=2048
	ServiceURL string `json:"serviceUrl"`
	// RoutePrefix is the base route path in APIM (e.g., "/myapi").
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9._~/-]*$`
	// +kubebuilder:validation:MaxLength=400
	RoutePrefix string `json:"routePrefix"`
	// OpenAPIDefinitionURL is the URL where the OpenAPI/Swagger definition can be fetched.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^https?://`
	// +kubebuilder:validation:MaxLength=2048
	OpenAPIDefinitionURL string `json:"openApiDefinitionUrl"`
	// ProductIDs is a list of product IDs to associate this API with in APIM.
	ProductIDs []string `json:"productIds,omitempty"`
	// TagIDs is a list of tag IDs to apply to this API in APIM.
	TagIDs []string `json:"tagIds,omitempty"`
	// APIMService is the name of the APIMService custom resource.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9][a-zA-Z0-9-]{0,48}[a-zA-Z0-9]$`
	// +kubebuilder:validation:MaxLength=50
	APIMService string `json:"apimService"`
	// APIMAPIName is the name of the APIMAPI resource that produced this deployment.
	// When omitted, legacy behavior falls back to using the deployment name.
	APIMAPIName string `json:"apimApiName,omitempty"`
	// Subscription is the Azure subscription ID where the APIM service is deployed.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`
	Subscription string `json:"subscription"`
	// ResourceGroup is the Azure resource group where the APIM service is located.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9][a-zA-Z0-9._()-]{0,88}[a-zA-Z0-9_()-]$`
	// +kubebuilder:validation:MaxLength=90
	ResourceGroup string `json:"resourceGroup"`
	// APIID is the unique identifier for the API in Azure APIM.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,78}[a-zA-Z0-9]$|^[a-zA-Z0-9]$`
	// +kubebuilder:validation:MaxLength=80
	APIID string `json:"APIID"`
	// Revision is an optional API revision number. If specified, a new revision will be created.
	// +kubebuilder:validation:Pattern=`^[0-9]{1,9}$`
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
	// Phase indicates the current reconciliation phase.
	// Typical values are WaitingForMatch, WaitingForReadyPod, Importing, Succeeded, and Error.
	Phase string `json:"phase,omitempty"`
	// Message describes the current reconciliation state in a human-readable way.
	Message string `json:"message,omitempty"`
	// LastError contains the most recent reconciliation error, if any.
	LastError string `json:"lastError,omitempty"`
	// LastAttemptAt is the timestamp of the most recent reconciliation attempt.
	LastAttemptAt string `json:"lastAttemptAt,omitempty"`
	// ObservedGeneration is the APIMAPI generation that this deployment status reflects.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// MatchedReplicaSets lists the ReplicaSets currently matched to the source APIMAPI.
	MatchedReplicaSets []string `json:"matchedReplicaSets,omitempty"`
	// OpenAPIHash is the hash of the latest successfully fetched OpenAPI document.
	OpenAPIHash string `json:"openApiHash,omitempty"`
	// DesiredHash is the hash of the desired APIM state derived from the deployment inputs.
	DesiredHash string `json:"desiredHash,omitempty"`
	// AppliedHash is the desired hash that was last successfully reconciled in APIM.
	AppliedHash string `json:"appliedHash,omitempty"`
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
