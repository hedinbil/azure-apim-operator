package v1

// DeletionPolicy decides what happens to the object in APIM when the resource that
// manages it is deleted. Retain is the default on every kind: deleting a resource -
// by hand or through a GitOps prune - never removes anything from APIM unless the
// owner opted in with Delete. A leftover in APIM is visible and recoverable; a
// deleted product with live subscriptions is not.
// +kubebuilder:validation:Enum=Delete;Retain
type DeletionPolicy string

const (
	// DeletionPolicyDelete removes the object from APIM before the resource goes away.
	DeletionPolicyDelete DeletionPolicy = "Delete"
	// DeletionPolicyRetain leaves the object in APIM and only removes the resource.
	DeletionPolicyRetain DeletionPolicy = "Retain"
)
