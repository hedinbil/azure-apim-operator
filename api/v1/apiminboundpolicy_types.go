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

// APIMInboundPolicySpec defines the desired state of APIMInboundPolicy.
type APIMInboundPolicySpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// APIMService is the name of the APIMService custom resource
	APIMService string `json:"apimService"`

	// APIID is the unique identifier for the API in APIM where the policy will be applied
	APIID string `json:"apiId"`

	// OperationID is the unique identifier for the operation (endpoint) within the API.
	// If specified, the policy will be applied to this specific operation.
	// If not specified, the policy will be applied to the entire API.
	OperationID string `json:"operationId,omitempty"`

	// PolicyContent is the XML content of the policy to be applied.
	// This should be a complete policy XML document including all sections (inbound, backend, outbound, on-error).
	PolicyContent string `json:"policyContent"`
}

// APIMInboundPolicyStatus defines the observed state of APIMInboundPolicy.
type APIMInboundPolicyStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// Phase indicates lifecycle state like "Created" or "Error"
	Phase string `json:"phase,omitempty"`

	// Message contains error details or status context
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// APIMInboundPolicy is the Schema for the apiminboundpolicies API.
type APIMInboundPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   APIMInboundPolicySpec   `json:"spec,omitempty"`
	Status APIMInboundPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// APIMInboundPolicyList contains a list of APIMInboundPolicy.
type APIMInboundPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []APIMInboundPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&APIMInboundPolicy{}, &APIMInboundPolicyList{})
}
