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
	APIMService string `json:"apimService"`

	// TagID is the unique identifier for the tag in APIM
	TagID string `json:"tagId"`

	// DisplayName is the name shown in the APIM UI
	DisplayName string `json:"displayName"`
}

// APIMTagStatus defines the observed state of APIMTag.
type APIMTagStatus struct {
	// Phase indicates lifecycle state like "Created" or "Error"
	Phase string `json:"phase,omitempty"`

	// Message contains error details or status context
	Message string `json:"message,omitempty"`
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
