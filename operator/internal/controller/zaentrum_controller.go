// Package controller implements the Zaentrum platform reconciler.
package controller

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
	"github.com/zaentrum/zaentrum-operator/operator/internal/templates"
	"github.com/zaentrum/zaentrum-operator/operator/internal/updates"
)

const (
	// requeueAfter drives periodic re-reconciliation so component readiness
	// stays fresh even without watch events.
	requeueAfter = 30 * time.Second

	condTypeReady   = "Ready"
	condTypeApplied = "ResourcesApplied"
)

// ZaentrumReconciler reconciles a Zaentrum object into the full platform.
type ZaentrumReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// ReleasesURL is the channel document the reconciler consults for Stage-2
	// auto-update discovery. Empty falls back to updates.DefaultReleasesURL.
	ReleasesURL string
	// Updates fetches/parses the channel document. The zero value is usable.
	Updates updates.Client
}

// +kubebuilder:rbac:groups=zaentrum.io,resources=zaentrums,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=zaentrum.io,resources=zaentrums/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=zaentrum.io,resources=zaentrums/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=configmaps;secrets;services;serviceaccounts;persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete

// Reconcile renders the embedded templates for the Zaentrum CR and applies every
// object via server-side apply, then refreshes status.
func (r *ZaentrumReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var z zaentrumv1alpha1.Zaentrum
	if err := r.Get(ctx, req.NamespacedName, &z); err != nil {
		// Deleted: owner references garbage-collect the managed resources.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Stage 2 — release-channel discovery. Resolve spec.channel to its target
	// tag from the published releases.json, then decide the tag to render this
	// pass. Discovery is best-effort: any failure leaves status.availableUpdate
	// unchanged and falls back to the spec/"latest" render so reconcile never
	// blocks on the network.
	decision := r.resolveUpdate(ctx, &z)

	// Render the platform from the embedded templates with CR-driven values.
	// The decision's render tag overrides spec.version so auto-mode rolls the
	// channel target in this very pass.
	vals := templates.NewValues(&z)
	vals.Version = decision.RenderTag
	objs, err := templates.Render(vals)
	if err != nil {
		r.setApplied(&z, metav1.ConditionFalse, "RenderFailed", err.Error())
		z.Status.Phase = "Error"
		_ = r.patchStatus(ctx, &z)
		return ctrl.Result{}, fmt.Errorf("render templates: %w", err)
	}

	z.Status.Phase = "Reconciling"

	// Apply each object via server-side apply with our field manager. Set the
	// Zaentrum as owner on namespaced resources so they GC with the CR (the
	// cluster-scoped Namespace cannot carry a namespaced owner ref, so skip it).
	if err := r.applyAll(ctx, &z, objs); err != nil {
		r.setApplied(&z, metav1.ConditionFalse, "ApplyFailed", err.Error())
		z.Status.Phase = "Error"
		_ = r.patchStatus(ctx, &z)
		return ctrl.Result{}, err
	}
	r.setApplied(&z, metav1.ConditionTrue, "Applied",
		fmt.Sprintf("applied %d objects via server-side apply", len(objs)))

	// Refresh component readiness from the live Deployments and roll status up.
	allReady, err := r.refreshComponents(ctx, &z, objs)
	if err != nil {
		return ctrl.Result{}, err
	}

	// currentVersion is the tag actually applied this pass; availableUpdate is
	// the channel target when it differs (manual mode surfaces it; auto mode
	// already rolled it into RenderTag so it collapses to "").
	z.Status.CurrentVersion = vals.Version
	z.Status.AvailableUpdate = decision.AvailableUpdate
	z.Status.ObservedGeneration = z.Generation

	if allReady {
		z.Status.Phase = "Ready"
		r.setReady(&z, metav1.ConditionTrue, "AllComponentsReady", "all components are ready")
	} else {
		z.Status.Phase = "Progressing"
		r.setReady(&z, metav1.ConditionFalse, "ComponentsNotReady", "waiting for components to become ready")
	}

	if err := r.patchStatus(ctx, &z); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("reconciled zaentrum", "objects", len(objs), "phase", z.Status.Phase, "version", vals.Version)
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// resolveUpdate performs Stage-2 channel discovery for one reconcile pass and
// returns the render/availableUpdate decision. It never returns an error:
// network or parse failures are logged and degrade to a spec/"latest" render
// with no surfaced update, so a flaky channel endpoint can never block a
// reconcile.
func (r *ZaentrumReconciler) resolveUpdate(ctx context.Context, z *zaentrumv1alpha1.Zaentrum) updates.Decision {
	logger := log.FromContext(ctx)

	spec := z.Spec
	auto := spec.Update.Mode == zaentrumv1alpha1.UpdateAuto

	// A pinned spec.version opts out of channel tracking; skip the network.
	if updates.IsPinned(spec.Version) {
		return updates.Decide(spec.Version, auto, "")
	}

	channel := string(spec.Channel)
	if channel == "" {
		channel = string(zaentrumv1alpha1.ChannelStable)
	}

	rel, err := r.Updates.Fetch(ctx, r.ReleasesURL)
	if err != nil {
		logger.Info("release-channel discovery skipped (fetch failed)",
			"channel", channel, "error", err.Error())
		return updates.Decide(spec.Version, auto, "")
	}

	target, err := rel.Resolve(channel)
	if err != nil {
		logger.Info("release-channel discovery skipped (resolve failed)",
			"channel", channel, "error", err.Error())
		return updates.Decide(spec.Version, auto, "")
	}

	return updates.Decide(spec.Version, auto, target)
}

// applyAll applies every rendered object via server-side apply. Server-side
// apply (Patch with types.ApplyPatchType + a FieldManager) makes the operator
// the declarative owner of exactly the fields it sets: the API server merges
// our intent with other managers' fields and prunes anything we previously set
// but no longer do. It is idempotent — re-applying identical objects is a
// no-op — and avoids read-modify-write conflicts. We set Force so the operator
// reclaims ownership of fields a prior manager (e.g. kubectl) touched.
func (r *ZaentrumReconciler) applyAll(ctx context.Context, z *zaentrumv1alpha1.Zaentrum, objs []*unstructured.Unstructured) error {
	for _, obj := range objs {
		// Own namespaced resources so they cascade-delete with the CR.
		if obj.GetNamespace() != "" {
			if err := controllerutil.SetControllerReference(z, obj, r.Scheme); err != nil {
				return fmt.Errorf("set owner ref on %s/%s: %w", obj.GetKind(), obj.GetName(), err)
			}
		}
		if err := r.Patch(ctx, obj, client.Apply,
			client.FieldOwner(templates.FieldManager),
			client.ForceOwnership,
		); err != nil {
			return fmt.Errorf("server-side apply %s/%s: %w", obj.GetKind(), obj.GetName(), err)
		}
	}
	return nil
}

// refreshComponents reads each managed Deployment and records its readiness +
// applied image into status.components. Returns true when every Deployment has
// all desired replicas available.
func (r *ZaentrumReconciler) refreshComponents(ctx context.Context, z *zaentrumv1alpha1.Zaentrum, objs []*unstructured.Unstructured) (bool, error) {
	var comps []zaentrumv1alpha1.ComponentStatus
	allReady := true

	for _, obj := range objs {
		if obj.GetKind() != "Deployment" {
			continue
		}
		name := obj.GetName()
		var dep appsv1.Deployment
		err := r.Get(ctx, types.NamespacedName{Namespace: obj.GetNamespace(), Name: name}, &dep)
		if errors.IsNotFound(err) {
			comps = append(comps, zaentrumv1alpha1.ComponentStatus{Name: name, Ready: false})
			allReady = false
			continue
		}
		if err != nil {
			return false, fmt.Errorf("get deployment %s: %w", name, err)
		}

		image := ""
		if len(dep.Spec.Template.Spec.Containers) > 0 {
			image = dep.Spec.Template.Spec.Containers[0].Image
		}
		desired := int32(1)
		if dep.Spec.Replicas != nil {
			desired = *dep.Spec.Replicas
		}
		ready := dep.Status.AvailableReplicas >= desired && desired > 0
		if !ready {
			allReady = false
		}
		comps = append(comps, zaentrumv1alpha1.ComponentStatus{Name: name, Ready: ready, Image: image})
	}

	z.Status.Components = comps
	return allReady, nil
}

func (r *ZaentrumReconciler) patchStatus(ctx context.Context, z *zaentrumv1alpha1.Zaentrum) error {
	return r.Status().Update(ctx, z)
}

func (r *ZaentrumReconciler) setReady(z *zaentrumv1alpha1.Zaentrum, status metav1.ConditionStatus, reason, msg string) {
	setCondition(z, condTypeReady, status, reason, msg)
}

func (r *ZaentrumReconciler) setApplied(z *zaentrumv1alpha1.Zaentrum, status metav1.ConditionStatus, reason, msg string) {
	setCondition(z, condTypeApplied, status, reason, msg)
}

func setCondition(z *zaentrumv1alpha1.Zaentrum, condType string, status metav1.ConditionStatus, reason, msg string) {
	cond := metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: z.Generation,
		LastTransitionTime: metav1.Now(),
	}
	for i := range z.Status.Conditions {
		if z.Status.Conditions[i].Type == condType {
			// Preserve transition time when status is unchanged.
			if z.Status.Conditions[i].Status == status {
				cond.LastTransitionTime = z.Status.Conditions[i].LastTransitionTime
			}
			z.Status.Conditions[i] = cond
			return
		}
	}
	z.Status.Conditions = append(z.Status.Conditions, cond)
}

// SetupWithManager wires the reconciler to watch Zaentrum CRs and the Deployments
// it owns (so readiness changes re-trigger a reconcile).
func (r *ZaentrumReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&zaentrumv1alpha1.Zaentrum{}).
		Owns(&appsv1.Deployment{}).
		Complete(r)
}
