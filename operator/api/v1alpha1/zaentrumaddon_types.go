package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Addon lifecycle phases reported in ZaentrumAddonStatus.Phase. An empty phase
// (no status yet) means pending — not reconciled; the portal and zae render it
// as such, so the operator never writes a "Pending" string of its own.
const (
	// AddonPlanned: suspended (plan only) and the plan is installable.
	AddonPlanned = "Planned"
	// AddonPlanFailed: suspended and the chart cannot be installed as planned.
	AddonPlanFailed = "PlanFailed"
	// AddonInstalling: applied; workloads are still rolling out.
	AddonInstalling = "Installing"
	// AddonReady: applied and every workload is ready.
	AddonReady = "Ready"
	// AddonDegraded: applied, but a workload is not ready and no rollout is in progress.
	AddonDegraded = "Degraded"
	// AddonFailed: not suspended and the chart could not be planned or applied.
	AddonFailed = "Failed"
)

// AddonChart references the Helm chart an addon installs.
type AddonChart struct {
	// Ref is an OCI chart reference (oci://registry/path/chart) or a direct
	// https link to a chart archive (https://host/path/chart-1.2.0.tgz).
	// +kubebuilder:validation:Pattern=`^(oci|https)://.+`
	Ref string `json:"ref"`

	// Version is the chart version — the tag of an oci ref, where it is
	// required. Ignored for direct https archives.
	// +optional
	Version string `json:"version,omitempty"`

	// Digest pins the exact chart archive; a fetched archive that does not
	// match is refused.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	// +optional
	Digest string `json:"digest,omitempty"`
}

// AddonValuesReference merges chart values from a Secret or ConfigMap in the
// addon's namespace (HelmRelease valuesFrom semantics).
type AddonValuesReference struct {
	// Kind of the values object.
	// +kubebuilder:validation:Enum=Secret;ConfigMap
	Kind string `json:"kind"`

	// Name of the values object.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// ValuesKey is the data key to read. Default "values.yaml": a whole
	// YAML/JSON values document merged over the values before it.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[\-._a-zA-Z0-9]+$`
	// +optional
	ValuesKey string `json:"valuesKey,omitempty"`

	// TargetPath sets this dotted values path (e.g. database.url) to the key's
	// raw string value instead of merging a document.
	// +kubebuilder:validation:MaxLength=250
	// +optional
	TargetPath string `json:"targetPath,omitempty"`

	// Optional tolerates a missing object or key.
	// +optional
	Optional bool `json:"optional,omitempty"`
}

// ZaentrumAddonSpec defines the desired state of an addon: a Helm chart the
// operator renders and applies next to the platform in the same namespace.
type ZaentrumAddonSpec struct {
	// Chart is the Helm chart to install.
	Chart AddonChart `json:"chart"`

	// Values are non-secret chart values. Precedence: chart defaults <
	// generated values < values < valuesFrom (in order) < the reserved
	// zaentrum block the operator sets.
	// +optional
	Values *apiextensionsv1.JSON `json:"values,omitempty"`

	// ValuesFrom merges values from Secrets/ConfigMaps, applied in order.
	// +optional
	ValuesFrom []AddonValuesReference `json:"valuesFrom,omitempty"`

	// Suspend makes the addon plan-only: the chart is fetched, rendered and
	// validated and status.plan is reported, but nothing is applied or pruned.
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// InputsRevision changes whenever the secret inputs this addon reads are
	// written, so the plan is recomputed; the portal sets a random token on
	// every secret write. It carries no meaning to the operator beyond bumping
	// metadata.generation, so observedGeneration tells clients the plan is fresh.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	InputsRevision string `json:"inputsRevision,omitempty"`
}

// AddonChartInfo describes a fetched chart.
type AddonChartInfo struct {
	// +optional
	Name string `json:"name,omitempty"`
	// +optional
	Version string `json:"version,omitempty"`
	// +optional
	AppVersion string `json:"appVersion,omitempty"`
	// +optional
	Description string `json:"description,omitempty"`
	// Digest is the sha256 of the chart archive.
	// +optional
	Digest string `json:"digest,omitempty"`
	// Annotations are the Chart.yaml annotations (zaentrum.io/primary, …).
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// AddonObject names one rendered object.
type AddonObject struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// AddonWorkload summarises a rendered workload for review before install.
type AddonWorkload struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Images are the container and init container images.
	// +optional
	Images []string `json:"images,omitempty"`
	// Ports are the container ports.
	// +optional
	Ports []int32 `json:"ports,omitempty"`
}

// AddonPlanChanges compares a plan with the objects currently applied.
type AddonPlanChanges struct {
	// Added lists objects the plan would create ("Kind/name").
	// +optional
	Added []string `json:"added,omitempty"`
	// Removed lists applied objects the plan would prune ("Kind/name").
	// +optional
	Removed []string `json:"removed,omitempty"`
	// Images lists container image changes ("Deployment/x: old → new").
	// +optional
	Images []string `json:"images,omitempty"`
}

// AddonPlan is the outcome of fetching, rendering and validating the chart.
type AddonPlan struct {
	// Chart describes the fetched chart.
	Chart AddonChartInfo `json:"chart"`

	// ValuesSchema is the chart's raw values.schema.json ("" if none).
	// +optional
	ValuesSchema string `json:"valuesSchema,omitempty"`

	// ValuesErrors are schema and render errors, e.g. a missing required input.
	// +optional
	ValuesErrors []string `json:"valuesErrors,omitempty"`

	// Violations are guardrail refusals; any violation blocks the install.
	// +optional
	Violations []string `json:"violations,omitempty"`

	// Objects lists every rendered object.
	// +optional
	Objects []AddonObject `json:"objects,omitempty"`

	// Workloads summarises the rendered Deployments and Jobs.
	// +optional
	Workloads []AddonWorkload `json:"workloads,omitempty"`

	// Changes compares the plan with what is applied.
	// +optional
	Changes *AddonPlanChanges `json:"changes,omitempty"`
}

// AddonComponentStatus reports the live readiness of one applied workload.
type AddonComponentStatus struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	// Ready counts the ready replicas of the current rollout.
	Ready int32 `json:"ready"`
	// Desired is the desired replica count.
	Desired int32 `json:"desired"`
	// Reason says why the component is not ready ("" when it is).
	// +optional
	Reason string `json:"reason,omitempty"`
}

// ZaentrumAddonStatus reports the observed state of an addon.
type ZaentrumAddonStatus struct {
	// Phase is Planned, PlanFailed, Installing, Ready, Degraded or Failed. Empty
	// means pending (not reconciled yet).
	// +optional
	Phase string `json:"phase,omitempty"`

	// Message is one human-readable line about the phase.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the .metadata.generation the plan was made for.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// LastAppliedChart is the chart currently applied (digest of its archive).
	// +optional
	LastAppliedChart *AddonChart `json:"lastAppliedChart,omitempty"`

	// Plan is refreshed on every reconcile, also while suspended.
	// +optional
	Plan *AddonPlan `json:"plan,omitempty"`

	// Components reports the applied Deployments' live readiness.
	// +optional
	Components []AddonComponentStatus `json:"components,omitempty"`

	// Conditions are Ready and Planned.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=zaddon,path=zaentrumaddons,singular=zaentrumaddon
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Chart",type=string,JSONPath=`.spec.chart.ref`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.chart.version`
// +kubebuilder:printcolumn:name="Suspended",type=boolean,JSONPath=`.spec.suspend`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ZaentrumAddon is the Schema for the zaentrumaddons API: one Helm chart the
// operator installs next to the platform, in the platform's namespace.
type ZaentrumAddon struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ZaentrumAddonSpec   `json:"spec,omitempty"`
	Status ZaentrumAddonStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ZaentrumAddonList contains a list of ZaentrumAddon.
type ZaentrumAddonList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ZaentrumAddon `json:"items"`
}
