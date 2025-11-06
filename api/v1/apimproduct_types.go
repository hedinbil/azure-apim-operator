package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// APIMProductSpec defines the desired state
type APIMProductSpec struct {
	ProductID   string `json:"productId"`             // Required unique product ID in APIM
	DisplayName string `json:"displayName"`           // Friendly display name
	Description string `json:"description,omitempty"` // Optional description
	Published   bool   `json:"published,omitempty"`   // Whether the product should be published
	APIMService string `json:"apimService"`           // API Management service name
	APIID       string `json:"apiID,omitempty"`       // Optional API to associate with the product
}

// APIMProductStatus defines the observed state
type APIMProductStatus struct {
	Phase   string `json:"phase,omitempty"`   // Status phase (e.g. Created, Error)
	Message string `json:"message,omitempty"` // Status message or error description
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// APIMProduct is the Schema for the apimproducts API
type APIMProduct struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   APIMProductSpec   `json:"spec,omitempty"`
	Status APIMProductStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// APIMProductList contains a list of APIMProduct
type APIMProductList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []APIMProduct `json:"items"`
}

func init() {
	SchemeBuilder.Register(&APIMProduct{}, &APIMProductList{})
}
