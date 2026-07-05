// Package controller implements the Stube platform reconciler.
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

	stubev1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
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

// StubeReconciler reconciles a Stube object into the full platform.
type StubeReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// ReleasesURL is the channel document the reconciler consults for Stage-2
	// auto-update discovery. Empty falls back to updates.DefaultReleasesURL.
	ReleasesURL string
	// Updates fetches/parses the channel document. The zero value is usable.
	Updates updates.Client
}

// +kubebuilder:rbac:groups=stube.io,resources=stubes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=stube.io,resources=stubes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=stube.io,resources=stubes/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=configmaps;secrets;services;serviceaccounts;persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete

// Reconcile renders the embedded templates for the Stube CR and applies every
// object via server-side apply, then refreshes status.
func (r *StubeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var stube stubev1alpha1.Stube
	if err := r.Get(ctx, req.NamespacedName, &stube); err != nil {
		// Deleted: owner references garbage-collect the managed resources.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Stage 2 — release-channel discovery. Resolve spec.channel to its target
	// tag from the published releases.json, then decide the tag to render this
	// pass. Discovery is best-effort: any failure leaves status.availableUpdate
	// unchanged and falls back to the spec/"latest" render so reconcile never
	// blocks on the network.
	decision := r.resolveUpdate(ctx, &stube)

	// Render the platform from the embedded templates with CR-driven values.
	// The decision's render tag overrides spec.version so auto-mode rolls the
	// channel target in this very pass.
	vals := templates.NewValues(&stube)
	vals.Version = decision.RenderTag
	objs, err := templates.Render(vals)
	if err != nil {
		r.setApplied(&stube, metav1.ConditionFalse, "RenderFailed", err.Error())
		stube.Status.Phase = "Error"
		_ = r.patchStatus(ctx, &stube)
		return ctrl.Result{}, fmt.Errorf("render templates: %w", err)
	}

	stube.Status.Phase = "Reconciling"

	// Apply each object via server-side apply with our field manager. Set the
	// Stube as owner on namespaced resources so they GC with the CR (the
	// cluster-scoped Namespace cannot carry a namespaced owner ref, so skip it).
	if err := r.applyAll(ctx, &stube, objs); err != nil {
		r.setApplied(&stube, metav1.ConditionFalse, "ApplyFailed", err.Error())
		stube.Status.Phase = "Error"
		_ = r.patchStatus(ctx, &stube)
		return ctrl.Result{}, err
	}
	r.setApplied(&stube, metav1.ConditionTrue, "Applied",
		fmt.Sprintf("applied %d objects via server-side apply", len(objs)))

	// Refresh component readiness from the live Deployments and roll status up.
	allReady, err := r.refreshComponents(ctx, &stube, objs)
	if err != nil {
		return ctrl.Result{}, err
	}

	// currentVersion is the tag actually applied this pass; availableUpdate is
	// the channel target when it differs (manual mode surfaces it; auto mode
	// already rolled it into RenderTag so it collapses to "").
	stube.Status.CurrentVersion = vals.Version
	stube.Status.AvailableUpdate = decision.AvailableUpdate
	stube.Status.ObservedGeneration = stube.Generation

	if allReady {
		stube.Status.Phase = "Ready"
		r.setReady(&stube, metav1.ConditionTrue, "AllComponentsReady", "all components are ready")
	} else {
		stube.Status.Phase = "Progressing"
		r.setReady(&stube, metav1.ConditionFalse, "ComponentsNotReady", "waiting for components to become ready")
	}

	if err := r.patchStatus(ctx, &stube); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("reconciled stube", "objects", len(objs), "phase", stube.Status.Phase, "version", vals.Version)
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// resolveUpdate performs Stage-2 channel discovery for one reconcile pass and
// returns the render/availableUpdate decision. It never returns an error:
// network or parse failures are logged and degrade to a spec/"latest" render
// with no surfaced update, so a flaky channel endpoint can never block a
// reconcile.
func (r *StubeReconciler) resolveUpdate(ctx context.Context, stube *stubev1alpha1.Stube) updates.Decision {
	logger := log.FromContext(ctx)

	spec := stube.Spec
	auto := spec.Update.Mode == stubev1alpha1.UpdateAuto

	// A pinned spec.version opts out of channel tracking; skip the network.
	if updates.IsPinned(spec.Version) {
		return updates.Decide(spec.Version, auto, "")
	}

	channel := string(spec.Channel)
	if channel == "" {
		channel = string(stubev1alpha1.ChannelStable)
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
func (r *StubeReconciler) applyAll(ctx context.Context, stube *stubev1alpha1.Stube, objs []*unstructured.Unstructured) error {
	for _, obj := range objs {
		// Own namespaced resources so they cascade-delete with the CR.
		if obj.GetNamespace() != "" {
			if err := controllerutil.SetControllerReference(stube, obj, r.Scheme); err != nil {
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
func (r *StubeReconciler) refreshComponents(ctx context.Context, stube *stubev1alpha1.Stube, objs []*unstructured.Unstructured) (bool, error) {
	var comps []stubev1alpha1.ComponentStatus
	allReady := true

	for _, obj := range objs {
		if obj.GetKind() != "Deployment" {
			continue
		}
		name := obj.GetName()
		var dep appsv1.Deployment
		err := r.Get(ctx, types.NamespacedName{Namespace: obj.GetNamespace(), Name: name}, &dep)
		if errors.IsNotFound(err) {
			comps = append(comps, stubev1alpha1.ComponentStatus{Name: name, Ready: false})
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
		comps = append(comps, stubev1alpha1.ComponentStatus{Name: name, Ready: ready, Image: image})
	}

	stube.Status.Components = comps
	return allReady, nil
}

func (r *StubeReconciler) patchStatus(ctx context.Context, stube *stubev1alpha1.Stube) error {
	return r.Status().Update(ctx, stube)
}

func (r *StubeReconciler) setReady(stube *stubev1alpha1.Stube, status metav1.ConditionStatus, reason, msg string) {
	setCondition(stube, condTypeReady, status, reason, msg)
}

func (r *StubeReconciler) setApplied(stube *stubev1alpha1.Stube, status metav1.ConditionStatus, reason, msg string) {
	setCondition(stube, condTypeApplied, status, reason, msg)
}

func setCondition(stube *stubev1alpha1.Stube, condType string, status metav1.ConditionStatus, reason, msg string) {
	cond := metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: stube.Generation,
		LastTransitionTime: metav1.Now(),
	}
	for i := range stube.Status.Conditions {
		if stube.Status.Conditions[i].Type == condType {
			// Preserve transition time when status is unchanged.
			if stube.Status.Conditions[i].Status == status {
				cond.LastTransitionTime = stube.Status.Conditions[i].LastTransitionTime
			}
			stube.Status.Conditions[i] = cond
			return
		}
	}
	stube.Status.Conditions = append(stube.Status.Conditions, cond)
}

// SetupWithManager wires the reconciler to watch Stube CRs and the Deployments
// it owns (so readiness changes re-trigger a reconcile).
func (r *StubeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&stubev1alpha1.Stube{}).
		Owns(&appsv1.Deployment{}).
		Complete(r)
}
