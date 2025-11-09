/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// APIMAPIPatchSpec defines the desired state of APIMAPIPatch.
// This spec contains the information needed to patch/update an existing API in APIM.
type APIMAPIPatchSpec struct {
	// APIID is the unique identifier for the API in Azure APIM that should be patched.
	APIID string `json:"APIID"`
	// ServiceURL is the new backend service URL that APIM will proxy requests to.
	ServiceURL string `json:"serviceUrl"`
}

// APIMAPIPatchStatus defines the observed state of APIMAPIPatch.
type APIMAPIPatchStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// APIMAPIPatch is the Schema for the apimapipatches API.
type APIMAPIPatch struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   APIMAPIPatchSpec   `json:"spec,omitempty"`
	Status APIMAPIPatchStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// APIMAPIPatchList contains a list of APIMAPIPatch.
type APIMAPIPatchList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []APIMAPIPatch `json:"items"`
}

func init() {
	SchemeBuilder.Register(&APIMAPIPatch{}, &APIMAPIPatchList{})
}
