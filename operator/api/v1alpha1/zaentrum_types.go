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

	// ExposeAdminConsole publishes the bundled Keycloak's admin console and its
	// admin API on the public host, under /auth/admin. Off by default: the
	// public host then routes only what people sign in through — /auth/realms
	// (the login and account pages, the OIDC endpoints) and /auth/resources —
	// and the console answers only through a port-forward, at
	// http://localhost:8080/auth/admin/ after
	// `kubectl port-forward svc/keycloak 8080:80`.
	// +optional
	ExposeAdminConsole bool `json:"exposeAdminConsole,omitempty"`
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

	// MediaAccessMode is the access mode of the media PVC the chart creates
	// (provisionMedia). ReadWriteOnce, the default, suits a single node and
	// node-local storage such as k3s's local-path, where every pod that mounts
	// the library runs on the volume's node; ReadWriteMany is for a cluster
	// whose storage class shares one volume across nodes (NFS, CephFS).
	// +kubebuilder:validation:Enum=ReadWriteOnce;ReadWriteMany
	// +optional
	MediaAccessMode string `json:"mediaAccessMode,omitempty"`

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

	// Postgres is the volume of the bundled Postgres (databases.mode perApp or
	// single): a PersistentVolumeClaim, so users, watch state and the catalog
	// outlive the pod. An install whose Postgres already runs on emptyDir stays
	// there until Migrate copies its databases over.
	// +optional
	Postgres PostgresStorageSpec `json:"postgres,omitempty"`
}

// PostgresStorageSpec is where the bundled Postgres keeps its data.
type PostgresStorageSpec struct {
	// Size of the claim created for it, postgres-data.
	// +kubebuilder:default="10Gi"
	// +optional
	Size resource.Quantity `json:"size,omitempty"`

	// ClassName is the StorageClass of that claim. Empty: storage.className,
	// else the cluster's default.
	// +optional
	ClassName string `json:"className,omitempty"`

	// ClaimName names an existing PersistentVolumeClaim to keep the data on
	// instead; none is created then.
	// +optional
	ClaimName string `json:"claimName,omitempty"`

	// Migrate moves a database that lives anywhere else — on emptyDir, where
	// the bundled Postgres kept it before it was persistent, or on another
	// claim — onto the claim above: a Job copies every database into it from
	// the running Postgres, and only once the copy has succeeded does the
	// Postgres switch over. Without it the operator never moves the database,
	// since a Postgres started on a new volume starts empty.
	// +optional
	Migrate bool `json:"migrate,omitempty"`
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
	// Chino is the host serving the chino SPA at "/", e.g. chino.example.com.
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
	// External means the platform's Secrets are pre-created (e.g. by CI) and
	// left alone. Default false: the operator generates each one it needs once,
	// from crypto/rand, owns it and never rotates it — zaentrum-db,
	// zaentrum-stream-signing and, with bundled identity, zaentrum-keycloak,
	// zaentrum-keycloak-admin and zaentrum-demo-user.
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
	// Host of the shared cluster, e.g. postgres.example.com.
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

// VerifyRequestAnnotation asks the operator for a verification run: any value
// it has not answered yet (status.verification.request) starts one once the
// platform is Ready. Set a fresh token per request.
const VerifyRequestAnnotation = "zaentrum.io/verify-request"

// VerificationSpec configures the platform's self-test: the chart's test hook,
// which the operator runs after every update and on request and reports in
// status.verification.
type VerificationSpec struct {
	// Enabled runs the checks after every update that leaves the platform
	// Ready, and whenever the zaentrum.io/verify-request annotation changes.
	// Default true.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
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

	// Verification configures the platform's self-test after each update.
	// +optional
	Verification VerificationSpec `json:"verification,omitempty"`

	// Network configures split-horizon / hostAliases.
	// +optional
	Network NetworkSpec `json:"network,omitempty"`

	// Routing selects Ingress vs OpenShift Routes.
	// +optional
	Routing RoutingSpec `json:"routing,omitempty"`

	// Secrets says whether the platform's Secrets are the operator's to make or
	// provided from outside.
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

// InstallSource says how the operator's OWN controller was installed. It is
// derived from the cluster, never configured by a field on the CR — an
// installer that lies about itself is worse than one that says "unknown".
// +kubebuilder:validation:Enum=olm;manifest;appliance;unknown
type InstallSource string

const (
	// InstallSourceOLM means the controller's Deployment (or its pod) is owned
	// by a ClusterServiceVersion: OLM/OperatorHub owns its lifecycle.
	InstallSourceOLM InstallSource = "olm"
	// InstallSourceManifest means a plain apply of deploy/operator-install.yaml
	// or `kubectl apply -k operator/config` — a cluster-admin or a GitOps job.
	InstallSourceManifest InstallSource = "manifest"
	// InstallSourceAppliance means the controller was baked into the all-in-one
	// image, which is the one case the cluster cannot be asked about: it looks
	// exactly like a manifest apply, so that image stamps
	// ZAENTRUM_INSTALL_SOURCE=appliance onto the manager container.
	InstallSourceAppliance InstallSource = "appliance"
	// InstallSourceUnknown means the controller could not read its own pod.
	InstallSourceUnknown InstallSource = "unknown"
)

// ControllerStatus reports the operator's OWN control plane: the image the
// controller pod runs, how it was installed, and whether the channel carries
// something newer.
//
// It is a reading, not a lever. status.currentVersion and spec.version are the
// PLATFORM; this is the operator itself, and the operator must never upgrade
// it — replacing a control plane is a cluster-admin / OLM / GitOps job. What
// the product owes its operator is the fact, so "which operator am I running,
// and is it current?" stops being a question only kubectl can answer.
type ControllerStatus struct {
	// Image is the image the controller pod actually runs, exactly as written
	// (a tag, a digest, or both). Empty when the pod could not be read.
	Image string `json:"image"`

	// Version is the image's tag when it has one, else its short (12 hex
	// character) digest, else "unknown".
	Version string `json:"version"`

	// Source is how this controller was installed.
	Source InstallSource `json:"source"`

	// AvailableUpdate is a newer controller version discovered on the CR's
	// channel. Empty when there is none, when spec.version pins a tag (the CR
	// has opted out of channel tracking entirely), or when discovery failed.
	AvailableUpdate string `json:"availableUpdate"`

	// ObservedAt is when this reading last CHANGED — not when it was last
	// taken. The reading is retaken every pass; the timestamp moves only when
	// something about it does (see internal/controller/self.go for why).
	ObservedAt metav1.Time `json:"observedAt"`
}

// VerificationResult is the verdict of the platform's latest self-test.
// +kubebuilder:validation:Enum=Passed;Failed;Running;Skipped;Error
type VerificationResult string

const (
	// VerificationPassed means every check passed (warnings and skips allowed).
	VerificationPassed VerificationResult = "Passed"
	// VerificationFailed means at least one check failed; checks says which.
	VerificationFailed VerificationResult = "Failed"
	// VerificationRunning means a run is in flight.
	VerificationRunning VerificationResult = "Running"
	// VerificationSkipped means verification is disabled (spec.verification).
	VerificationSkipped VerificationResult = "Skipped"
	// VerificationError means no verdict could be reached — the run could not
	// start, or ended without a readable report (an image that does not pull,
	// the deadline, an account that could not be prepared). message says why.
	VerificationError VerificationResult = "Error"
)

// VerificationTrigger says what started a verification run.
// +kubebuilder:validation:Enum=update;request
type VerificationTrigger string

const (
	// VerificationTriggerUpdate is a run for a platform whose images changed.
	VerificationTriggerUpdate VerificationTrigger = "update"
	// VerificationTriggerRequest is a run the zaentrum.io/verify-request
	// annotation asked for.
	VerificationTriggerRequest VerificationTrigger = "request"
)

// VerificationCheckStatus is one check's outcome.
// +kubebuilder:validation:Enum=ok;warn;fail;skip
type VerificationCheckStatus string

const (
	// VerificationCheckOK is a check that passed.
	VerificationCheckOK VerificationCheckStatus = "ok"
	// VerificationCheckWarn is a check that passed with a warning.
	VerificationCheckWarn VerificationCheckStatus = "warn"
	// VerificationCheckFail is a check that failed.
	VerificationCheckFail VerificationCheckStatus = "fail"
	// VerificationCheckSkip is a check that did not run (e.g. sign-in without
	// an account to sign in with).
	VerificationCheckSkip VerificationCheckStatus = "skip"
)

// VerificationCheck is one line of the run's report.
type VerificationCheck struct {
	// Name of the check, as the check runner names it.
	Name string `json:"name"`
	// Status is ok, warn, fail or skip.
	Status VerificationCheckStatus `json:"status"`
	// Detail is what the check saw, at most 200 characters.
	// +optional
	Detail string `json:"detail,omitempty"`
}

// VerificationStatus reports the platform's latest self-test: the chart's test
// hook (`zae doctor`, outside-in against the public URL, with a real sign-in),
// which the operator runs after every update and on request.
//
// It is a reading beside the platform's status, never a gate: it does not move
// the phase or the Ready condition, and a failed run is not retried by itself —
// only a new update or a new request starts another.
type VerificationStatus struct {
	// Result is Passed, Failed, Running, Skipped or Error.
	Result VerificationResult `json:"result"`

	// Trigger is what started the run: update or request.
	// +optional
	Trigger VerificationTrigger `json:"trigger,omitempty"`

	// Request is the zaentrum.io/verify-request value the run answered.
	// +optional
	Request string `json:"request,omitempty"`

	// Fingerprint identifies the platform the run verified: the first 12 hex
	// characters of the sha256 over the sorted "deployment/container=image"
	// lines of every platform Deployment (init containers included), images as
	// applied after digest pinning. A run starts whenever it moves.
	// +optional
	Fingerprint string `json:"fingerprint,omitempty"`

	// Version is status.currentVersion when the run started.
	// +optional
	Version string `json:"version,omitempty"`

	// StartedAt is when the run started.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// FinishedAt is when the run ended.
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`

	// Job is the name of the run's Job, in the platform's namespace.
	// +optional
	Job string `json:"job,omitempty"`

	// Passed counts the report's checks that passed.
	Passed int32 `json:"passed"`
	// Failed counts the report's checks that failed.
	Failed int32 `json:"failed"`
	// Warned counts the report's checks that passed with a warning.
	Warned int32 `json:"warned"`
	// Skipped counts the report's checks that did not run.
	Skipped int32 `json:"skipped"`

	// Checks are the report's lines, at most 40; when there are more, failures
	// and warnings are kept first and message says how many are not shown.
	// +optional
	Checks []VerificationCheck `json:"checks,omitempty"`

	// Message is one line about the result: a summary, or why there is none.
	// +optional
	Message string `json:"message,omitempty"`
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

	// Controller reports the operator's own controller — what it runs, how it
	// was installed and whether something newer is out. Reported, never acted
	// on. Absent until a controller that knows how to read itself reconciles
	// the CR.
	// +optional
	Controller *ControllerStatus `json:"controller,omitempty"`

	// Verification reports the platform's latest self-test. Absent until the
	// first run; see VerificationStatus.
	// +optional
	Verification *VerificationStatus `json:"verification,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=stb,path=zaentrums,singular=zaentrum
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.currentVersion`
// +kubebuilder:printcolumn:name="Verified",type=string,JSONPath=`.status.verification.result`
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
