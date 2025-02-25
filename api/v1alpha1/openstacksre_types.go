package v1alpha1

import (
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// OpenStackSRESpec defines the desired state of OpenStackSRE
type OpenStackSRESpec struct {
    // If you want some configuration toggles, place them here.
    // E.g. we can specify a "BalancingMode" or "EvacuationEnabled", etc.
    EvacuationEnabled bool `json:"evacuationEnabled,omitempty"`
    BalancingEnabled  bool `json:"balancingEnabled,omitempty"`
}

// OpenStackSREStatus defines the observed state of OpenStackSRE
type OpenStackSREStatus struct {
    // A list of hypervisors that were evacuated, time-stamped events, etc.
    LastAction string `json:"lastAction,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// OpenStackSRE is the Schema for the openstacksres API
type OpenStackSRE struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`

    Spec   OpenStackSRESpec   `json:"spec,omitempty"`
    Status OpenStackSREStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// OpenStackSREList contains a list of OpenStackSRE
type OpenStackSREList struct {
    metav1.TypeMeta `json:",inline"`
    metav1.ListMeta `json:"metadata,omitempty"`
    Items           []OpenStackSRE `json:"items"`
}
