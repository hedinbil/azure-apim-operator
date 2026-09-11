package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DeletionPolicy decides what happens to the product in APIM when its APIMProduct is deleted.
// +kubebuilder:validation:Enum=Delete;Retain
type DeletionPolicy string

const (
	// DeletionPolicyDelete removes the product from APIM before the resource goes away.
	DeletionPolicyDelete DeletionPolicy = "Delete"
	// DeletionPolicyRetain leaves the product in APIM and only removes the resource.
	DeletionPolicyRetain DeletionPolicy = "Retain"
)

// APIMProductSpec defines the desired state
type APIMProductSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,78}[a-zA-Z0-9]$|^[a-zA-Z0-9]$`
	// +kubebuilder:validation:MaxLength=80
	ProductID string `json:"productId"` // Required unique product ID in APIM
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=300
	DisplayName string `json:"displayName"`           // Friendly display name
	Description string `json:"description,omitempty"` // Optional description
	Published   bool   `json:"published,omitempty"`   // Whether the product should be published
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9][a-zA-Z0-9-]{0,48}[a-zA-Z0-9]$`
	// +kubebuilder:validation:MaxLength=50
	APIMService string `json:"apimService"` // API Management service name
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,78}[a-zA-Z0-9]$|^[a-zA-Z0-9]$`
	// +kubebuilder:validation:MaxLength=80
	APIID string `json:"apiID,omitempty"` // Optional API to associate with the product
	// DeletionPolicy decides whether deleting this resource also deletes the product in APIM.
	// Delete (the default) removes it; Retain leaves it in place.
	// +kubebuilder:default=Delete
	// +optional
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`
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
