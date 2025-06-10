package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// APIMAPIDeploymentSpec defines the desired state of APIMAPIDeployment
type APIMAPIDeploymentSpec struct {
	ServiceURL           string `json:"serviceUrl"`
	RoutePrefix          string `json:"routePrefix"`
	OpenAPIDefinitionURL string `json:"openAPIDefinitionURL"`
	ProductID            string `json:"productId"`
	APIMService          string `json:"apimService"`
	Subscription         string `json:"subscription"`
	ResourceGroup        string `json:"resourceGroup"`
	APIID                string `json:"APIID"`
	Revision             string `json:"revision,omitempty"`
}

type APIMAPIDeploymentStatus struct {
	ImportedAt string `json:"importedAt,omitempty"`
	Status     string `json:"status,omitempty"`
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

//+kubebuilder:object:root=true

// APIMAPIDeploymentList contains a list of APIMAPIDeployment
type APIMAPIDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []APIMAPIDeployment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&APIMAPIDeployment{}, &APIMAPIDeploymentList{})
}
