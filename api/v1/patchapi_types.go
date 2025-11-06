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

// PatchAPISpec defines the desired state of PatchAPI.
// This spec contains the information needed to update an existing API's service URL in APIM.
type PatchAPISpec struct {
	// APIID is the unique identifier for the API in Azure APIM that should be patched.
	APIID string `json:"APIID"`
	// ServiceURL is the new backend service URL that APIM will proxy requests to.
	ServiceURL string `json:"serviceUrl"`
}

// PatchAPIStatus defines the observed state of PatchAPI.
type PatchAPIStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// PatchAPI is the Schema for the patchapis API.
type PatchAPI struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PatchAPISpec   `json:"spec,omitempty"`
	Status PatchAPIStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// PatchAPIList contains a list of PatchAPI.
type PatchAPIList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PatchAPI `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PatchAPI{}, &PatchAPIList{})
}
