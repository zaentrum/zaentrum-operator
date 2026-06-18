package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// IdentityMode selects whether Stube ships its own bundled OIDC provider
// (Keycloak) or federates to an external one.
// +kubebuilder:validation:Enum=bundled;external
type IdentityMode string

const (
	// IdentityBundled deploys the in-cluster Keycloak + stube realm import.
	IdentityBundled IdentityMode = "bundled"
	// IdentityExternal points every service at an external issuer; the
	// bundled Keycloak resources are not rendered.
	IdentityExternal IdentityMode = "external"
)

// Channel selects the auto-update train consulted by Stage-2 update logic.
// +kubebuilder:validation:Enum=stable;edge
type Channel string

const (
	// ChannelStable is the default, slower-moving release train.
	ChannelStable Channel = "stable"
	// ChannelEdge tracks pre-release builds.
	ChannelEdge Channel = "edge"
)

// UpdateMode selects whether updates are applied automatically.
// +kubebuilder:validation:Enum=manual;auto
type UpdateMode string

const (
	// UpdateManual never bumps spec.version on its own (default).
	UpdateManual UpdateMode = "manual"
	// UpdateAuto lets the Stage-2 reconciler bump to the latest in-channel tag.
	UpdateAuto UpdateMode = "auto"
)

// IdentitySpec configures the OIDC provider for the platform.
type IdentitySpec struct {
	// Mode is "bundled" (ship Keycloak) or "external" (federate).
	// +kubebuilder:default=bundled
	// +optional
	Mode IdentityMode `json:"mode,omitempty"`

	// Issuer is the public OIDC issuer URL. When empty in bundled mode the
	// operator derives it from Hostname (http://<hostname>/auth/realms/stube).
	// +optional
	Issuer string `json:"issuer,omitempty"`

	// ClientID is the public OIDC client id the web SPA authenticates as.
	// +kubebuilder:default=chino-web
	// +optional
	ClientID string `json:"clientId,omitempty"`

	// Audience is the expected token audience services validate against.
	// +kubebuilder:default=chino
	// +optional
	Audience string `json:"audience,omitempty"`
}

// StorageSpec configures persistent storage for the media library.
type StorageSpec struct {
	// MediaSize is the size of the media library PVC.
	// +kubebuilder:default="50Gi"
	// +optional
	MediaSize resource.Quantity `json:"mediaSize,omitempty"`

	// ClassName is an optional StorageClass for all platform PVCs.
	// +optional
	ClassName string `json:"className,omitempty"`
}

// FeaturesSpec toggles optional platform capabilities.
type FeaturesSpec struct {
	// GPU enables hardware (NVENC) transcoding on the stream plane.
	// +kubebuilder:default=false
	// +optional
	GPU bool `json:"gpu,omitempty"`

	// Kafka enables the bundled single-node event-stream broker.
	// +kubebuilder:default=true
	// +optional
	Kafka bool `json:"kafka,omitempty"`
}

// UpdateSpec configures the Stage-2 auto-update behaviour.
type UpdateSpec struct {
	// Mode is "manual" (default) or "auto".
	// +kubebuilder:default=manual
	// +optional
	Mode UpdateMode `json:"mode,omitempty"`
}

// StubeSpec defines the desired state of a Stube platform instance.
type StubeSpec struct {
	// Channel selects the release train (consumed by Stage-2 auto-update).
	// +kubebuilder:default=stable
	// +optional
	Channel Channel `json:"channel,omitempty"`

	// Version is the image tag applied to every ghcr.io/zaentrum/stube/* image.
	// +kubebuilder:default=latest
	// +optional
	Version string `json:"version,omitempty"`

	// Hostname is the public host: issuer host + ingress host + KC_HOSTNAME.
	// +kubebuilder:default=stube.localhost
	// +optional
	Hostname string `json:"hostname,omitempty"`

	// Identity configures the OIDC provider.
	// +optional
	Identity IdentitySpec `json:"identity,omitempty"`

	// Storage configures persistent storage.
	// +optional
	Storage StorageSpec `json:"storage,omitempty"`

	// Features toggles optional capabilities.
	// +optional
	Features FeaturesSpec `json:"features,omitempty"`

	// Update configures Stage-2 auto-update.
	// +optional
	Update UpdateSpec `json:"update,omitempty"`
}

// ComponentStatus reports the readiness of one managed Deployment.
type ComponentStatus struct {
	// Name is the Deployment name.
	Name string `json:"name"`
	// Ready is true when all replicas of the Deployment are available.
	Ready bool `json:"ready"`
	// Image is the primary container image (with tag) currently applied.
	Image string `json:"image,omitempty"`
}

// StubeStatus reports the observed state of a Stube platform instance.
type StubeStatus struct {
	// Phase is a coarse human-facing lifecycle string.
	// +optional
	Phase string `json:"phase,omitempty"`

	// CurrentVersion mirrors the version most recently applied to the cluster.
	// +optional
	CurrentVersion string `json:"currentVersion,omitempty"`

	// AvailableUpdate is the newest in-channel tag discovered by Stage-2
	// auto-update logic, if any.
	// +optional
	AvailableUpdate string `json:"availableUpdate,omitempty"`

	// ObservedGeneration is the .metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions follow the standard Kubernetes condition convention.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Components reports per-Deployment readiness.
	// +optional
	Components []ComponentStatus `json:"components,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=stb
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.currentVersion`
// +kubebuilder:printcolumn:name="Host",type=string,JSONPath=`.spec.hostname`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Stube is the Schema for the stubes API; one CR drives the whole platform.
type Stube struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StubeSpec   `json:"spec,omitempty"`
	Status StubeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// StubeList contains a list of Stube.
type StubeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Stube `json:"items"`
}
