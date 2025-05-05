package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// APIMAPISpec defines the desired state of APIMAPI
type APIMAPISpec struct {
	Host          string `json:"host"`
	RoutePrefix   string `json:"routePrefix"`
	SwaggerPath   string `json:"swaggerPath"`
	APIMService   string `json:"apimService"`
	Subscription  string `json:"subscription"`
	ResourceGroup string `json:"resourceGroup"`
	Revision      string `json:"revision,omitempty"`
}

type APIMAPIStatus struct {
	ImportedAt    string `json:"importedAt,omitempty"`
	SwaggerStatus string `json:"swaggerStatus,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// APIMAPI is the Schema for the apimapis API
type APIMAPI struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   APIMAPISpec   `json:"spec,omitempty"`
	Status APIMAPIStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// APIMAPIList contains a list of APIMAPI
type APIMAPIList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []APIMAPI `json:"items"`
}

func init() {
	SchemeBuilder.Register(&APIMAPI{}, &APIMAPIList{})
}
