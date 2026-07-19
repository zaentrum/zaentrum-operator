package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// IdentityMode selects whether Zaentrum ships its own bundled OIDC provider
// (Keycloak) or federates to an external one.
// +kubebuilder:validation:Enum=bundled;external
type IdentityMode string

const (
	// IdentityBundled deploys the in-cluster Keycloak + zaentrum realm import.
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
	// operator derives it from Hostname (http://<hostname>/auth/realms/zaentrum).
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

	// IssuerScheme is http or https — the scheme of the derived issuer + the
	// bundled Keycloak KC_HOSTNAME. Use https when TLS is terminated at the edge.
	// +kubebuilder:validation:Enum=http;https
	// +kubebuilder:default=http
	// +optional
	IssuerScheme string `json:"issuerScheme,omitempty"`

	// LoginTheme is the bundled Keycloak login theme name (empty = Keycloak default).
	// +optional
	LoginTheme string `json:"loginTheme,omitempty"`
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

	// ProvisionMedia controls whether the chart creates the media PVC. Set false
	// when an external PV backs it (e.g. the demo's NFS export). Default true.
	// +optional
	ProvisionMedia *bool `json:"provisionMedia,omitempty"`

	// KafkaPVC names a pre-created PVC to back the bundled Kafka broker's log dir
	// so topics survive a pod restart/reschedule. Empty (default) → ephemeral
	// emptyDir (topics are recreated by the demo's topics Job + producer retry).
	// +optional
	KafkaPVC string `json:"kafkaPvc,omitempty"`

	// KafkaNode pins the bundled Kafka broker to a node (kubernetes.io/hostname),
	// required when KafkaPVC is a node-local volume. Empty → no node pinning.
	// +optional
	KafkaNode string `json:"kafkaNode,omitempty"`
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

	// Pipeline enables the media pipeline (analyzer/packager/transcoder/katalog-ingest).
	// +kubebuilder:default=false
	// +optional
	Pipeline bool `json:"pipeline,omitempty"`
}

// NetworkSpec configures network-level platform behaviour.
type NetworkSpec struct {
	// IssuerHostAliasIP adds a hostAliases entry (this IP → the public host) to
	// the OIDC validators so in-cluster token validation reaches an edge-terminated
	// HTTPS issuer (split-horizon). Empty = no hostAliases.
	// +optional
	IssuerHostAliasIP string `json:"issuerHostAliasIP,omitempty"`
}

// RoutingSpec selects how the platform is exposed.
type RoutingSpec struct {
	// ProvisionIngress renders a plain-Kubernetes Ingress. Default true.
	// +optional
	ProvisionIngress *bool `json:"provisionIngress,omitempty"`

	// ProvisionRoutes renders OpenShift Routes (single-origin paths). Default false.
	// +optional
	ProvisionRoutes *bool `json:"provisionRoutes,omitempty"`

	// Mode is "pathRouted" (everything on Hostname — profile B1, the default) or
	// "subdomains" (chino gets its own host serving the SPA at /; additive — the
	// path-routed surfaces on Hostname remain).
	// +kubebuilder:validation:Enum=pathRouted;subdomains
	// +kubebuilder:default=pathRouted
	// +optional
	Mode string `json:"mode,omitempty"`

	// Hosts names the per-product public hosts used in subdomains mode.
	// +optional
	Hosts RoutingHosts `json:"hosts,omitempty"`
}

// RoutingHosts are the per-product public hosts (subdomains mode).
type RoutingHosts struct {
	// Chino is the host serving the chino SPA at "/", e.g. chino.beta.nalet.cloud.
	// +optional
	Chino string `json:"chino,omitempty"`
}

// EventStreamingSpec selects the Kafka the platform rides: the bundled
// single-node broker, or a shared external cluster (per-tenant topic prefix +
// mTLS) — e.g. the platform Strimzi at platform-event-streaming.
type EventStreamingSpec struct {
	// Mode is "bundled" (default) or "external".
	// +kubebuilder:validation:Enum=bundled;external
	// +kubebuilder:default=bundled
	// +optional
	Mode string `json:"mode,omitempty"`

	// Bootstrap is the external bootstrap server list, e.g.
	// platform-kafka-kafka-bootstrap.platform-event-streaming.svc:9093.
	// +optional
	Bootstrap string `json:"bootstrap,omitempty"`

	// CertSecret names a secret IN THIS NAMESPACE holding user.crt/user.key/
	// ca.crt (a copied KafkaUser secret) for mTLS; empty = plaintext.
	// +optional
	CertSecret string `json:"certSecret,omitempty"`

	// TopicPrefix is the per-tenant topic namespace on a shared cluster
	// (e.g. "zaentrum-beta."). Default "stube.".
	// +kubebuilder:default="stube."
	// +optional
	TopicPrefix string `json:"topicPrefix,omitempty"`
}

// SecretsSpec controls secret provisioning.
type SecretsSpec struct {
	// External means the platform secrets are pre-created (e.g. by CI); the chart
	// does not render them. Default false (bundled dev-default secrets).
	// +optional
	External bool `json:"external,omitempty"`
}

// DatabasesSpec configures the per-app database layout.
type DatabasesSpec struct {
	// Mode is "perApp" (a DB per service), "single", or "external" (shared cluster).
	// +kubebuilder:default=perApp
	// +optional
	Mode string `json:"mode,omitempty"`
	// +kubebuilder:default=chino
	// +optional
	Chino string `json:"chino,omitempty"`
	// +kubebuilder:default=katalog
	// +optional
	Katalog string `json:"katalog,omitempty"`
	// +kubebuilder:default=keycloak
	// +optional
	Keycloak string `json:"keycloak,omitempty"`
	// +kubebuilder:default=portal
	// +optional
	Portal string `json:"portal,omitempty"`

	// External configures the shared database cluster used when Mode is
	// "external": the chart skips the bundled postgres + create-db init
	// containers (DB provisioning is the tenant's job) and the names above then
	// address databases ON the shared cluster (e.g. chino_beta).
	// +optional
	External DatabaseExternalSpec `json:"external,omitempty"`
}

// DatabaseExternalSpec points the platform at a shared Postgres.
type DatabaseExternalSpec struct {
	// Host of the shared cluster, e.g. postgres.nalet.cloud.
	// +optional
	Host string `json:"host,omitempty"`
	// +kubebuilder:default=5432
	// +optional
	Port int32 `json:"port,omitempty"`
	// +kubebuilder:default=require
	// +optional
	SSLMode string `json:"sslmode,omitempty"`
}

// KeycloakSpec configures the bundled Keycloak image.
type KeycloakSpec struct {
	// Image is the bundled Keycloak container image.
	// +kubebuilder:default="quay.io/keycloak/keycloak:26.0.7"
	// +optional
	Image string `json:"image,omitempty"`
}

// UpdateSpec configures the Stage-2 auto-update behaviour.
type UpdateSpec struct {
	// Mode is "manual" (default) or "auto".
	// +kubebuilder:default=manual
	// +optional
	Mode UpdateMode `json:"mode,omitempty"`
}

// ZaentrumSpec defines the desired state of a Zaentrum platform instance.
type ZaentrumSpec struct {
	// Channel selects the release train (consumed by Stage-2 auto-update).
	// +kubebuilder:default=stable
	// +optional
	Channel Channel `json:"channel,omitempty"`

	// Version is the image tag applied to every ghcr.io/zaentrum/* image.
	// +kubebuilder:default=latest
	// +optional
	Version string `json:"version,omitempty"`

	// Hostname is the public host: issuer host + ingress host + KC_HOSTNAME.
	// +kubebuilder:default=zaentrum.localhost
	// +optional
	Hostname string `json:"hostname,omitempty"`

	// Identity configures the OIDC provider.
	// +optional
	Identity IdentitySpec `json:"identity,omitempty"`

	// EventStreaming selects the bundled broker or a shared external Kafka.
	// +optional
	EventStreaming EventStreamingSpec `json:"eventStreaming,omitempty"`

	// Storage configures persistent storage.
	// +optional
	Storage StorageSpec `json:"storage,omitempty"`

	// Features toggles optional capabilities.
	// +optional
	Features FeaturesSpec `json:"features,omitempty"`

	// Update configures Stage-2 auto-update.
	// +optional
	Update UpdateSpec `json:"update,omitempty"`

	// Network configures split-horizon / hostAliases.
	// +optional
	Network NetworkSpec `json:"network,omitempty"`

	// Routing selects Ingress vs OpenShift Routes.
	// +optional
	Routing RoutingSpec `json:"routing,omitempty"`

	// Secrets controls whether platform secrets are rendered or external.
	// +optional
	Secrets SecretsSpec `json:"secrets,omitempty"`

	// Databases configures the per-app database layout.
	// +optional
	Databases DatabasesSpec `json:"databases,omitempty"`

	// Keycloak configures the bundled Keycloak image.
	// +optional
	Keycloak KeycloakSpec `json:"keycloak,omitempty"`

	// ImagePullSecrets are added to every workload (private registries).
	// +optional
	ImagePullSecrets []string `json:"imagePullSecrets,omitempty"`

	// PartOf sets the app.kubernetes.io/part-of label value (default: the namespace).
	// +optional
	PartOf string `json:"partOf,omitempty"`

	// Replicas overrides the replica count of individual app-tier Deployments by
	// name, e.g. {"chino-api": 2, "katalog-api": 3}. Unlisted services stay at 1.
	// Stateful backers (postgres/valkey/kafka/keycloak) are NOT scalable this way.
	// Set from the portal operator console; the operator reconciles it so the
	// change persists (a raw Deployment edit would be reverted on the next pass).
	// +optional
	Replicas map[string]int32 `json:"replicas,omitempty"`
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

// ZaentrumStatus reports the observed state of a Zaentrum platform instance.
type ZaentrumStatus struct {
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
// +kubebuilder:resource:shortName=stb,path=zaentrums,singular=zaentrum
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.currentVersion`
// +kubebuilder:printcolumn:name="Host",type=string,JSONPath=`.spec.hostname`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Zaentrum is the Schema for the zaentrums API; one CR drives the whole platform.
type Zaentrum struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ZaentrumSpec   `json:"spec,omitempty"`
	Status ZaentrumStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ZaentrumList contains a list of Zaentrum.
type ZaentrumList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Zaentrum `json:"items"`
}
