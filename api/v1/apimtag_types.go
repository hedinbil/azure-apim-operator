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

// APIMTagSpec defines the desired state of APIMTag.
type APIMTagSpec struct {
	// APIMService is the name of the APIMService custom resource
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9][a-zA-Z0-9-]{0,48}[a-zA-Z0-9]$`
	// +kubebuilder:validation:MaxLength=50
	APIMService string `json:"apimService"`

	// TagID is the unique identifier for the tag in APIM
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,78}[a-zA-Z0-9]$|^[a-zA-Z0-9]$`
	// +kubebuilder:validation:MaxLength=80
	TagID string `json:"tagId"`

	// DisplayName is the name shown in the APIM UI
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=300
	DisplayName string `json:"displayName"`
	// DeletionPolicy decides whether deleting this resource also deletes the tag in APIM.
	// Retain (the default) leaves it in place. Delete is not implemented for this kind yet
	// and behaves like Retain.
	// +kubebuilder:default=Retain
	// +optional
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`
}

// APIMTagStatus defines the observed state of APIMTag.
type APIMTagStatus struct {
	// Phase indicates lifecycle state like "Created", "Error", "Backoff", "Stalled" or "Invalid"
	Phase string `json:"phase,omitempty"`

	// Message contains error details or status context
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the metadata.generation the last APIM write was for. A spec
	// change makes it differ, which clears the retry state.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// RetryStatus tracks backing off from failed APIM writes.
	RetryStatus `json:",inline"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// APIMTag is the Schema for the apimtags API.
type APIMTag struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   APIMTagSpec   `json:"spec,omitempty"`
	Status APIMTagStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// APIMTagList contains a list of APIMTag.
type APIMTagList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []APIMTag `json:"items"`
}

func init() {
	SchemeBuilder.Register(&APIMTag{}, &APIMTagList{})
}
