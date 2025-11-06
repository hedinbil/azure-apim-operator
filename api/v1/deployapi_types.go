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

// DeployAPISpec defines the desired state of DeployAPI.
// This spec orchestrates the deployment of an API to Azure APIM by creating
// an ImportAPI resource that will handle the actual import process.
type DeployAPISpec struct {
	// Host is the hostname where the backend service is accessible.
	Host string `json:"host"`
	// RoutePrefix is the base route path in APIM (e.g., "/myapi").
	RoutePrefix string `json:"routePrefix"`
	// OpenAPIDefinitionURL is the URL where the OpenAPI/Swagger definition can be fetched.
	OpenAPIDefinitionURL string `json:"openApiDefinitionUrl"`
	// APIMService is the name of the APIMService custom resource.
	APIMService string `json:"apimService"`
	// Subscription is the Azure subscription ID where the APIM service is deployed.
	Subscription string `json:"subscription"`
	// ResourceGroup is the Azure resource group where the APIM service is located.
	ResourceGroup string `json:"resourceGroup"`
	// APIID is the unique identifier for the API in Azure APIM.
	APIID string `json:"APIID"`
	// Revision is an optional API revision number. If specified, a new revision will be created.
	Revision string `json:"revision,omitempty"`
}

// DeployAPIStatus defines the observed state of DeployAPI.
type DeployAPIStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// DeployAPI is the Schema for the deployapis API.
type DeployAPI struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DeployAPISpec   `json:"spec,omitempty"`
	Status DeployAPIStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// DeployAPIList contains a list of DeployAPI.
type DeployAPIList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DeployAPI `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DeployAPI{}, &DeployAPIList{})
}
