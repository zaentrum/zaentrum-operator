package controller

import (
	"context"
	"os"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
	"github.com/zaentrum/zaentrum-operator/operator/internal/digest"
)

// This file implements the controller's report on ITSELF: status.controller.
//
// The operator publishes the platform's version on every pass and said nothing
// at all about the thing doing the publishing, so "which operator is this
// cluster running, and is it current?" was answerable only by someone with
// kubectl and the operator's namespace memorised. The portal and the CLI could
// only say "not covered here".
//
// Reporting is the whole feature. The product must never upgrade its own
// control plane — swapping the controller out is a cluster-admin / OLM /
// GitOps job, and an operator that can replace itself mid-reconcile is a
// failure mode, not a feature. Flux, Argo CD, cert-manager and every
// OLM-managed operator draw the line in the same place: report inside, act
// outside.
//
// Everything here is best-effort. An unreadable pod, an unreachable registry
// or a channel document that will not parse costs the reading, never the
// reconcile.

const (
	// envPodName and envPodNamespace are injected by the downward API in the
	// manager Deployment (config/manager, deploy/operator-install.yaml, the
	// OLM bundle and the appliance render). The image is NEVER baked in at
	// build time: a version stamped into the binary is a claim about the past
	// that goes stale the moment someone repoints the tag, and the one thing
	// this field must be is true.
	envPodName      = "POD_NAME"
	envPodNamespace = "POD_NAMESPACE"

	// envInstallSource is the one install source the cluster cannot be asked
	// about. The appliance image bakes the same manifests a cluster-admin
	// would apply by hand, so nothing distinguishes them in the API; that
	// image stamps this env instead. Everything else is derived.
	envInstallSource = "ZAENTRUM_INSTALL_SOURCE"

	// managerContainer is the controller container's name in every install
	// bundle we ship.
	managerContainer = "manager"

	// csvKind / csvGroup identify the OLM object that owns what OLM installs.
	csvKind  = "ClusterServiceVersion"
	csvGroup = "operators.coreos.com"

	// replicaSetKind is the pod's own owner; the Deployment sits one hop above
	// it (see selfDeployment).
	replicaSetKind = "ReplicaSet"

	// versionUnknown is reported when the image yields neither tag nor digest.
	versionUnknown = "unknown"

	// shortDigestLen is how much of a digest's hex stands in for a version,
	// matching the 12-character short form docker, podman and git all use.
	shortDigestLen = 12
)

// controllerReport reads the controller's own pod and returns what to publish
// in status.controller. channelTarget is the tag the CR's channel resolved to
// this pass ("" when discovery failed or the CR pins spec.version).
//
// It never returns nil and never returns an error: a reading it cannot take
// degrades to "unknown"/"" so the field always says something honest.
func (r *ZaentrumReconciler) controllerReport(
	ctx context.Context, z *zaentrumv1alpha1.Zaentrum, channelTarget string,
) *zaentrumv1alpha1.ControllerStatus {
	cur := &zaentrumv1alpha1.ControllerStatus{
		Version: versionUnknown,
		Source:  zaentrumv1alpha1.InstallSourceUnknown,
	}

	if pod, ok := r.selfPod(ctx); ok {
		cur.Image = managerImage(pod)
		cur.Version = imageVersion(cur.Image)
		cur.Source = r.installSource(ctx, pod)
		cur.AvailableUpdate = r.controllerUpdate(ctx, cur.Image, channelTarget)
	}

	// Hold the previous timestamp while the reading is unchanged. Moving it
	// every pass would turn every status write into a real object change, and
	// the CR watch would re-enqueue the reconcile that just wrote it — a loop
	// driven entirely by a clock. The conditions below do the same thing with
	// LastTransitionTime, for the same reason.
	cur.ObservedAt = metav1.Now()
	if prev := z.Status.Controller; prev != nil && sameReading(prev, cur) {
		cur.ObservedAt = prev.ObservedAt
	}
	return cur
}

// sameReading reports whether two readings say the same thing, ignoring when
// they were taken.
func sameReading(a, b *zaentrumv1alpha1.ControllerStatus) bool {
	return a.Image == b.Image &&
		a.Version == b.Version &&
		a.Source == b.Source &&
		a.AvailableUpdate == b.AvailableUpdate
}

// selfPod reads the pod the controller runs in, named by the downward API env
// the manager Deployment injects. Absent env (a local `go run`, an install
// bundle that predates this field) or an unreadable pod means no reading —
// reported as "unknown", never guessed at.
func (r *ZaentrumReconciler) selfPod(ctx context.Context) (*corev1.Pod, bool) {
	name := strings.TrimSpace(os.Getenv(envPodName))
	ns := strings.TrimSpace(os.Getenv(envPodNamespace))
	if name == "" || ns == "" {
		return nil, false
	}
	var pod corev1.Pod
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &pod); err != nil {
		log.FromContext(ctx).Info("controller self-report: own pod not readable",
			"pod", ns+"/"+name, "error", err.Error())
		return nil, false
	}
	return &pod, true
}

// managerImage returns the manager container's image as the pod spec writes it
// — not the kubelet's resolved status image, because the point of the field is
// what this install ASKED for, tag and all.
func managerImage(pod *corev1.Pod) string {
	for _, c := range pod.Spec.Containers {
		if c.Name == managerContainer {
			return c.Image
		}
	}
	// A repackaged install may name the container something else; a
	// single-container pod is unambiguous either way.
	if len(pod.Spec.Containers) == 1 {
		return pod.Spec.Containers[0].Image
	}
	return ""
}

// imageVersion is the image's tag, else its short digest, else "unknown".
func imageVersion(image string) string {
	if strings.TrimSpace(image) == "" {
		return versionUnknown
	}
	ref := digest.Parse(image)
	if ref.Tag != "" {
		return ref.Tag
	}
	if hex, ok := strings.CutPrefix(ref.Digest, "sha256:"); ok && len(hex) >= shortDigestLen {
		return hex[:shortDigestLen]
	}
	if ref.Digest != "" {
		return ref.Digest
	}
	return versionUnknown
}

// installSource derives how this controller was installed.
//
// OLM first, and not as a formality: when a ClusterServiceVersion owns the
// controller, OLM owns its upgrade path, and that outranks anything an image
// baked into itself. The appliance env is consulted only where no CSV owns us,
// and an unrecognised value is ignored rather than published — the enum is the
// contract the portal and the CLI render against.
func (r *ZaentrumReconciler) installSource(ctx context.Context, pod *corev1.Pod) zaentrumv1alpha1.InstallSource {
	if ownedByCSV(pod.OwnerReferences) {
		return zaentrumv1alpha1.InstallSourceOLM
	}
	if dep, ok := r.selfDeployment(ctx, pod); ok && ownedByCSV(dep.OwnerReferences) {
		return zaentrumv1alpha1.InstallSourceOLM
	}

	switch v := zaentrumv1alpha1.InstallSource(strings.TrimSpace(os.Getenv(envInstallSource))); v {
	case zaentrumv1alpha1.InstallSourceOLM,
		zaentrumv1alpha1.InstallSourceManifest,
		zaentrumv1alpha1.InstallSourceAppliance,
		zaentrumv1alpha1.InstallSourceUnknown:
		return v
	case "":
	default:
		log.FromContext(ctx).Info("controller self-report: ignoring unknown "+envInstallSource,
			"value", string(v))
	}

	// Nothing owns us and nothing declared itself: somebody applied the
	// manifests.
	return zaentrumv1alpha1.InstallSourceManifest
}

// ownedByCSV reports whether any owner reference is an OLM ClusterServiceVersion.
func ownedByCSV(refs []metav1.OwnerReference) bool {
	for _, o := range refs {
		if o.Kind == csvKind && strings.HasPrefix(o.APIVersion, csvGroup+"/") {
			return true
		}
	}
	return false
}

// selfDeployment reads the Deployment behind the controller pod — the object
// OLM actually owns (a CSV owns the Deployment; the pod's own owner is the
// ReplicaSet in between).
//
// The name comes from the ReplicaSet's, which Kubernetes forms as
// "<deployment>-<pod-template-hash>", rather than from reading the ReplicaSet:
// that would need an `apps/replicasets` RBAC rule the operator does not hold
// and has no other use for, and this report is not worth widening the
// operator's reach over.
func (r *ZaentrumReconciler) selfDeployment(ctx context.Context, pod *corev1.Pod) (*appsv1.Deployment, bool) {
	name := ""
	for _, o := range pod.OwnerReferences {
		if o.Kind != replicaSetKind {
			continue
		}
		if i := strings.LastIndex(o.Name, "-"); i > 0 {
			name = o.Name[:i]
		}
		break
	}
	if name == "" {
		return nil, false
	}
	var dep appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: name}, &dep); err != nil {
		log.FromContext(ctx).Info("controller self-report: own deployment not readable",
			"deployment", pod.Namespace+"/"+name, "error", err.Error())
		return nil, false
	}
	return &dep, true
}

// controllerUpdate reports the channel's tag when the controller is not
// already running it.
//
// The channel document names a release tag, and the operator image ships to
// the same ghcr.io/zaentrum namespace under the same tags — so the candidate
// is that tag on the controller's OWN repository, resolved through the same
// digest cache the platform render uses (one HEAD per tag per TTL, not one per
// reconcile).
//
// Where the registry answers, the comparison is by digest, because comparing
// names gets this wrong in both directions: an operator pinned to
// `:sha-<commit>` that IS the channel's current image would be told to update
// forever, and a digest-pinned operator has no tag to compare at all. Where it
// does not answer, a tag comparison is still better than silence, and a
// digest-pinned image reports nothing rather than guessing.
func (r *ZaentrumReconciler) controllerUpdate(ctx context.Context, image, channelTarget string) string {
	target := strings.TrimSpace(channelTarget)
	if target == "" || strings.TrimSpace(image) == "" {
		return ""
	}
	ref := digest.Parse(image)
	if ref.Repo == "" {
		return ""
	}
	// Running the channel's tag by name, with nothing else to check.
	if ref.Tag == target && ref.Digest == "" {
		return ""
	}

	if r.Digest != nil {
		if pinned, err := r.Digest.Resolve(ctx, ref.Registry+"/"+ref.Repo+":"+target); err == nil {
			want := digest.Parse(pinned).Digest
			have := ref.Digest
			if have == "" {
				if p, rerr := r.Digest.Resolve(ctx, image); rerr == nil {
					have = digest.Parse(p).Digest
				}
			}
			if want != "" && have != "" {
				if have == want {
					// A different name for the bytes already running.
					return ""
				}
				return target
			}
		} else {
			log.FromContext(ctx).Info("controller self-report: channel image not resolvable",
				"target", target, "error", err.Error())
		}
	}

	// The registry could not be reached (or there is no resolver).
	if ref.Digest != "" {
		// Pinned by digest: without the registry there is nothing to compare,
		// and a tag is not evidence of anything newer.
		return ""
	}
	if ref.Tag != target {
		return target
	}
	return ""
}
