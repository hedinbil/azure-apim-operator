package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// APIMAPISpec defines the desired state of APIMAPI
type APIMAPISpec struct {
	ServiceURL           string   `json:"serviceUrl"`
	RoutePrefix          string   `json:"routePrefix"`
	OpenAPIDefinitionURL string   `json:"openApiDefinitionUrl"`
	ProductIDs           []string `json:"productIds,omitempty"`
	TagIDs               []string `json:"tagIds,omitempty"`
	APIMService          string   `json:"apimService"`
	APIID                string   `json:"APIID"`
}

type APIMAPIStatus struct {
	ImportedAt          string `json:"importedAt,omitempty"`
	Status              string `json:"status,omitempty"`
	ApiHost             string `json:"apiHost"`
	DeveloperPortalHost string `json:"developerPortalHost"`
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

// +kubebuilder:object:root=true

// APIMAPIList contains a list of APIMAPI
type APIMAPIList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []APIMAPI `json:"items"`
}

func init() {
	SchemeBuilder.Register(&APIMAPI{}, &APIMAPIList{})
}
