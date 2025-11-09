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

// APIMServiceSpec defines the desired state of APIMService.
// This spec contains the Azure subscription and resource group information needed
// to identify and connect to an Azure API Management service instance.
type APIMServiceSpec struct {
	// Name is the name of the Azure API Management service instance in Azure.
	Name string `json:"name"`
	// ResourceGroup is the Azure resource group where the APIM service is located.
	ResourceGroup string `json:"resourceGroup"`
	// Subscription is the Azure subscription ID where the APIM service is deployed.
	Subscription string `json:"subscription"`
}

// APIMServiceStatus defines the observed state of APIMService.
// This status reflects information about the APIM service that was retrieved from Azure.
type APIMServiceStatus struct {
	// Host is the hostname of the APIM service (e.g., "myapim.azure-api.net").
	Host string `json:"host,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// APIMService is the Schema for the apimservices API.
type APIMService struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   APIMServiceSpec   `json:"spec,omitempty"`
	Status APIMServiceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// APIMServiceList contains a list of APIMService.
type APIMServiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []APIMService `json:"items"`
}

func init() {
	SchemeBuilder.Register(&APIMService{}, &APIMServiceList{})
}
