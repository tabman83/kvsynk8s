/*
Copyright 2026.

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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// VaultSpec identifies the Azure Key Vault secret that is the source of truth
// for this SecretSync. Never carries a secret value (constitution principle I).
type VaultSpec struct {
	// name is the name of the Azure Key Vault (not the full URI).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:MaxLength=24
	// +kubebuilder:validation:Pattern=`^[a-zA-Z][a-zA-Z0-9-]+$`
	Name string `json:"name"`

	// secret is the name of the secret inside the vault.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=127
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9-]+$`
	Secret string `json:"secret"`
}

// TargetSpec describes the Kubernetes Secret produced from the vault secret.
// Both fields are optional; the controller (not the CRD schema, which cannot
// reference metadata.name) applies the documented defaults at reconcile time.
type TargetSpec struct {
	// secretName is the name of the Kubernetes Secret to create in this
	// namespace. Defaults to the SecretSync's own name when empty. This
	// default is applied by the controller at reconcile time, not by the CRD
	// schema.
	// The pattern is apimachinery's dns1123SubdomainFmt verbatim, so it plus
	// MaxLength=253 accept exactly what IsDNS1123Subdomain accepts — the rule
	// the API server applies to the managed Secret's metadata.name. Same
	// reasoning as the dataKey rule below: a name the API server will always
	// reject (an empty label, "my..secret"; a label edged with a hyphen,
	// "a.-b") would only produce a SecretSync whose Secret can never be
	// created, and that Invalid is not ErrTargetConflict, so Reconcile returns
	// it before writing any status — the CR would sit at Pending with an empty
	// reason forever. Reject it at admission instead.
	// There is deliberately no per-label length rule: IsDNS1123Subdomain
	// enforces only the 253-char total and this regex. The 63-char limit
	// belongs to IsDNS1123Label, which Secret names are not checked against.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	SecretName string `json:"secretName,omitempty"`

	// dataKey is the key under .data to store the value in. Defaults to the
	// vault secret name when empty. This default is applied by the
	// controller at reconcile time, not by the CRD schema.
	// The XValidation rule mirrors apimachinery's IsConfigMapKey: "." , ".."
	// and any ".."-prefixed key are rejected by the API server on the Secret
	// write, so accepting them here would only produce a SecretSync whose
	// Secret can never be created. Reject them at admission instead.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z0-9]+$`
	// +kubebuilder:validation:XValidation:rule="self != '.' && !self.startsWith('..')",message="dataKey must not be '.' or start with '..' (not a valid Secret data key)"
	DataKey string `json:"dataKey,omitempty"`
}

// SecretSyncSpec defines the desired state of SecretSync
type SecretSyncSpec struct {
	// vault identifies the source Key Vault secret.
	// +kubebuilder:validation:Required
	Vault VaultSpec `json:"vault"`

	// target describes the Kubernetes Secret produced from the vault secret.
	// +optional
	Target TargetSpec `json:"target,omitempty"`
}

// SecretSyncState is the high level sync state of a SecretSync, per the state
// machine in data-model.md.
// +kubebuilder:validation:Enum=Pending;InSync;Failing
type SecretSyncState string

const (
	// SecretSyncStatePending means the controller has not completed a sync
	// attempt yet.
	SecretSyncStatePending SecretSyncState = "Pending"
	// SecretSyncStateInSync means the managed Secret reflects the latest
	// Key Vault version.
	SecretSyncStateInSync SecretSyncState = "InSync"
	// SecretSyncStateFailing means the last sync attempt failed; see reason
	// and message for details (never a secret value).
	SecretSyncStateFailing SecretSyncState = "Failing"
)

// SecretSyncStatus defines the observed state of SecretSync.
type SecretSyncStatus struct {
	// state is the current high level sync state.
	// +optional
	State SecretSyncState `json:"state,omitempty"`

	// lastSyncTime is the time of the last successful write/verify.
	// +optional
	LastSyncTime metav1.Time `json:"lastSyncTime,omitzero"`

	// syncedVersion is the Key Vault secret version currently reflected by
	// the managed Secret. It is an identifier, never a value, and is safe
	// to expose (constitution principle I).
	// +optional
	SyncedVersion string `json:"syncedVersion,omitempty"`

	// reason is a machine-readable failure reason. Known values: SecretNotFound,
	// AccessDenied, AuthenticationFailed, TargetConflict, TargetImmutable,
	// SourceDeleted, SourceDisabled, SecretWriteFailed, TransientError.
	// +optional
	Reason string `json:"reason,omitempty"`

	// message is a human-readable detail. It MUST never contain a secret
	// value (constitution principle I, FR-010).
	// +optional
	Message string `json:"message,omitempty"`

	// observedGeneration is the generation of the spec last processed by the
	// controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions holds the standard Kubernetes conditions for this SecretSync.
	// Exactly one type is set today, "Ready": True while the managed Secret
	// reflects the current vault value, False with the failure reason
	// otherwise, Unknown before the first sync attempt completes.
	//
	// It exists because nothing standard can key on status.state: this field
	// is what makes `kubectl wait --for=condition=Ready secretsync/<name>`
	// work, and what Argo CD and Flux health checks read.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// ConditionReady is the only condition type this operator sets. Its Reason is
// the same vocabulary as status.reason, plus "Synced" for the success case and
// "Pending" before the first attempt completes — metav1.Condition.Reason is
// required and must be non-empty, so the success case cannot simply carry the
// empty status.reason.
const ConditionReady = "Ready"

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ss
// +kubebuilder:printcolumn:name="Vault",type=string,JSONPath=".spec.vault.name"
// +kubebuilder:printcolumn:name="Secret",type=string,JSONPath=".spec.vault.secret",priority=1
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=".status.state"
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=".status.reason"
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=".status.syncedVersion"
// +kubebuilder:printcolumn:name="Last Sync",type=date,JSONPath=".status.lastSyncTime"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// SecretSync is the Schema for the secretsyncs API
type SecretSync struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of SecretSync
	// +required
	Spec SecretSyncSpec `json:"spec"`

	// status defines the observed state of SecretSync
	// +optional
	Status SecretSyncStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SecretSyncList contains a list of SecretSync
type SecretSyncList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SecretSync `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &SecretSync{}, &SecretSyncList{})
		return nil
	})
}
