package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type DevEnvironmentPhase string
type InterceptPhase string

const (
	PhaseProvisioning DevEnvironmentPhase = "Provisioning"
	PhaseReady        DevEnvironmentPhase = "Ready"
	PhaseSleeping     DevEnvironmentPhase = "Sleeping"
	PhaseTerminating  DevEnvironmentPhase = "Terminating"
	PhaseFailed       DevEnvironmentPhase = "Failed"

	InterceptPending InterceptPhase = "Pending"
	InterceptReady   InterceptPhase = "Ready"
	InterceptError   InterceptPhase = "Error"
)

type Intercept struct {
	Service   string `json:"service"`
	LocalPort int    `json:"localPort"`
}

type InterceptStatus struct {
	Service   string         `json:"service"`
	LocalPort int            `json:"localPort"`
	Phase     InterceptPhase `json:"phase"`
	Message   string         `json:"message,omitempty"`
}

type DevEnvironmentSpec struct {
	Developer         string      `json:"developer"`
	Branch            string      `json:"branch"`
	Intercepts        []Intercept `json:"intercepts"`
	TTL               string      `json:"ttl,omitempty"`
	DeveloperTunnelIP string      `json:"developerTunnelIP"`
}

type DevEnvironmentStatus struct {
	Phase             DevEnvironmentPhase `json:"phase,omitempty"`
	Namespace         string              `json:"namespace,omitempty"`
	DeveloperTunnelIP string              `json:"developerTunnelIP,omitempty"`
	Message           string              `json:"message,omitempty"`
	LastHeartbeat     metav1.Time         `json:"lastHeartbeat,omitempty"`
	Intercepts        []InterceptStatus   `json:"intercepts,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
//+kubebuilder:printcolumn:name="Developer",type="string",JSONPath=".spec.developer"
//+kubebuilder:printcolumn:name="Branch",type="string",JSONPath=".spec.branch"
//+kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

type DevEnvironment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DevEnvironmentSpec   `json:"spec,omitempty"`
	Status DevEnvironmentStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

type DevEnvironmentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DevEnvironment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DevEnvironment{}, &DevEnvironmentList{})
}
