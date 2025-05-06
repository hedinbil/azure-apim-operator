package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// APIMAPIRevisionSpec defines the desired state of APIMAPIRevision
type APIMAPIRevisionSpec struct {
	Host          string `json:"host"`
	RoutePrefix   string `json:"routePrefix"`
	SwaggerPath   string `json:"swaggerPath"`
	APIMService   string `json:"apimService"`
	Subscription  string `json:"subscription"`
	ResourceGroup string `json:"resourceGroup"`
	APIID         string `json:"APIID"`
	Revision      string `json:"revision,omitempty"`
}

type APIMAPIRevisionStatus struct {
	ImportedAt    string `json:"importedAt,omitempty"`
	SwaggerStatus string `json:"swaggerStatus,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// APIMAPIRevision is the Schema for the APIMAPIRevisions API
type APIMAPIRevision struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   APIMAPIRevisionSpec   `json:"spec,omitempty"`
	Status APIMAPIRevisionStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// APIMAPIRevisionList contains a list of APIMAPIRevision
type APIMAPIRevisionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []APIMAPIRevision `json:"items"`
}

func init() {
	SchemeBuilder.Register(&APIMAPIRevision{}, &APIMAPIRevisionList{})
}
