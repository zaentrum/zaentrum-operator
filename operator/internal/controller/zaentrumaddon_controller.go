package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
	"github.com/zaentrum/zaentrum-operator/operator/internal/addon"
	"github.com/zaentrum/zaentrum-operator/operator/internal/templates"
)

const (
	// addonRequeueSettled re-plans a Ready, Planned or Degraded addon now and
	// then: a moving chart tag, a collision that cleared.
	addonRequeueSettled = 5 * time.Minute
	// addonRequeueRollout backs up the Deployment watch while pods roll.
	addonRequeueRollout = 30 * time.Second
	// addonRequeueRetry retries what the next reconcile may fix by itself:
	// a chart that failed to fetch, a platform that is not there yet.
	addonRequeueRetry = time.Minute
	// addonAPIPollInterval is how often the operator checks whether the
	// ZaentrumAddon API became usable.
	addonAPIPollInterval = time.Minute

	// valuesSecretGrace is how old an unreferenced values Secret must be before
	// it is collected: the portal creates a Secret first and points the addon
	// at it second, and must never lose that race.
	valuesSecretGrace = 10 * time.Minute
	// orphanValuesGrace is how old a values Secret of an addon that does not
	// exist must be before the sweep deletes it.
	orphanValuesGrace = time.Hour
	// valuesSweepInterval is how often orphaned values Secrets are swept.
	valuesSweepInterval = 30 * time.Minute

	condTypePlanned = "Planned"
)

// ChartSource fetches addon chart archives; *addon.Fetcher is the real one.
type ChartSource interface {
	Fetch(ctx context.Context, chart zaentrumv1alpha1.AddonChart, fresh bool) (*addon.Archive, error)
}

// ZaentrumAddonReconciler installs addon Helm charts next to the platform.
//
// It is a controller of its own on purpose: nothing an addon does — a chart
// that does not fetch or render, a refused object, a failed apply — ever
// touches the Zaentrum resource or its phase.
type ZaentrumAddonReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// APIReader reads straight from the API server. Values objects, generated
	// values, name collisions, the applied inventory and pods are read through
	// it, so installing addons does not make the operator cache every Secret,
	// ConfigMap and Pod in the cluster. Nil falls back to the client.
	APIReader client.Reader

	// Charts fetches chart archives. A shared instance keeps its cache warm
	// across reconciles; nil uses one owned by the reconciler.
	Charts ChartSource

	// Now is the clock values Secret collection measures age with; nil is
	// time.Now. Tests set it.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=zaentrum.io,resources=zaentrumaddons,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=zaentrum.io,resources=zaentrumaddons/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=zaentrum.io,resources=zaentrumaddons/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;patch;delete

// Reconcile plans the addon — fetch, values, render, guardrails — and, unless
// it is suspended, applies the plan with server-side apply, prunes what the
// chart no longer renders and rolls component readiness up into the phase.
func (r *ZaentrumAddonReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var a zaentrumv1alpha1.ZaentrumAddon
	if err := r.Get(ctx, req.NamespacedName, &a); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !a.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &a)
	}
	if !controllerutil.ContainsFinalizer(&a, addon.FinalizerValues) {
		// First, so an addon removed right after it was created still gets the
		// chance to keep its values. The patch refreshes a's resourceVersion for
		// the status update below.
		base := a.DeepCopy()
		controllerutil.AddFinalizer(&a, addon.FinalizerValues)
		if err := r.Patch(ctx, &a, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
	}

	var collectAgain time.Duration
	if addon.ValidateName(a.Name) == nil {
		var err error
		if collectAgain, err = r.collectValuesSecrets(ctx, &a); err != nil {
			// Housekeeping: a failed collection never blocks the plan.
			log.FromContext(ctx).Info("values secret collection failed; trying again", "addon", a.Name, "error", err.Error())
			collectAgain = addonRequeueRetry
		}
	}

	result, err := r.reconcileAddon(ctx, &a)
	if serr := r.Status().Update(ctx, &a); serr != nil && err == nil {
		return ctrl.Result{}, serr
	}
	result.RequeueAfter = sooner(result.RequeueAfter, collectAgain)
	return result, err
}

// collectValuesSecrets deletes the addon's values Secrets nothing reads any
// more. The portal never deletes a Secret: each secret write creates a new,
// immutable one and repoints valuesFrom at it, leaving the previous one
// behind. A Secret is collected only when it is labelled for this addon, named
// as its values, owned by this addon alone or by nothing, referenced by no
// valuesFrom entry, not the generated Secret, not kept, and older than the
// grace the portal needs between creating it and pointing the addon at it.
// Returns when to look again for a Secret still inside that grace (0 = none).
// Only metadata is read.
func (r *ZaentrumAddonReconciler) collectValuesSecrets(ctx context.Context, a *zaentrumv1alpha1.ZaentrumAddon) (time.Duration, error) {
	referenced := map[string]bool{}
	for _, ref := range a.Spec.ValuesFrom {
		if ref.Kind == "Secret" {
			referenced[ref.Name] = true
		}
	}
	secrets := &metav1.PartialObjectMetadataList{}
	secrets.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("SecretList"))
	if err := r.reader().List(ctx, secrets, client.InNamespace(a.Namespace), client.MatchingLabels{addon.LabelAddon: a.Name}); err != nil {
		return 0, fmt.Errorf("list values secrets: %w", err)
	}
	now := r.now()
	var again time.Duration
	for i := range secrets.Items {
		s := &secrets.Items[i]
		if !addon.IsValuesSecretName(a.Name, s.Name) || s.Name == addon.GeneratedSecretName(a.Name) ||
			referenced[s.Name] || s.Labels[addon.LabelKeep] == "true" || !ownedOnlyBy(s, a.UID) {
			continue
		}
		if age := now.Sub(s.CreationTimestamp.Time); age < valuesSecretGrace {
			again = sooner(again, valuesSecretGrace-age+time.Second)
			continue
		}
		if err := r.deleteSecret(ctx, s); err != nil {
			return again, err
		}
		log.FromContext(ctx).Info("deleted an unreferenced values secret", "addon", a.Name, "secret", s.Name)
	}
	return again, nil
}

// SweepOrphanedValues deletes values Secrets whose ZaentrumAddon does not exist
// at all — left by a portal write for an addon that was never created, or that
// failed before pointing the addon at the Secret. A Secret goes only when it is
// labelled zaentrum.io/addon=<name> and named as that addon's values, no
// ZaentrumAddon <name> exists in its namespace, it is not kept, it has no owner
// but a ZaentrumAddon, and it is older than an hour. Only metadata is read.
func (r *ZaentrumAddonReconciler) SweepOrphanedValues(ctx context.Context) error {
	var addons zaentrumv1alpha1.ZaentrumAddonList
	if err := r.reader().List(ctx, &addons); err != nil {
		return fmt.Errorf("list addons: %w", err)
	}
	exists := map[types.NamespacedName]bool{}
	for _, a := range addons.Items {
		exists[types.NamespacedName{Namespace: a.Namespace, Name: a.Name}] = true
	}
	secrets := &metav1.PartialObjectMetadataList{}
	secrets.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("SecretList"))
	if err := r.reader().List(ctx, secrets, client.HasLabels{addon.LabelAddon}); err != nil {
		return fmt.Errorf("list values secrets: %w", err)
	}
	now := r.now()
	for i := range secrets.Items {
		s := &secrets.Items[i]
		name := s.Labels[addon.LabelAddon]
		if name == "" || !addon.IsValuesSecretName(name, s.Name) || s.Labels[addon.LabelKeep] == "true" ||
			exists[types.NamespacedName{Namespace: s.Namespace, Name: name}] ||
			now.Sub(s.CreationTimestamp.Time) < orphanValuesGrace || ownedByOtherThanAddons(s) {
			continue
		}
		if err := r.deleteSecret(ctx, s); err != nil {
			return err
		}
		log.FromContext(ctx).Info("deleted an orphaned values secret", "namespace", s.Namespace, "secret", s.Name)
	}
	return nil
}

// valuesSweep runs SweepOrphanedValues now and every valuesSweepInterval.
func (r *ZaentrumAddonReconciler) valuesSweep() manager.Runnable {
	return manager.RunnableFunc(func(ctx context.Context) error {
		logger := ctrl.Log.WithName("addons")
		for {
			if err := r.SweepOrphanedValues(ctx); err != nil {
				logger.Info("orphaned values sweep failed; trying again next interval", "error", err.Error())
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(valuesSweepInterval):
			}
		}
	})
}

// deleteSecret deletes the listed Secret, and only that very object (UID
// precondition). One already gone is fine.
func (r *ZaentrumAddonReconciler) deleteSecret(ctx context.Context, s *metav1.PartialObjectMetadata) error {
	s.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
	uid := s.UID
	if err := r.Delete(ctx, s, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete values secret %s/%s: %w", s.Namespace, s.Name, err)
	}
	return nil
}

// ownedOnlyBy reports whether an object has no owner references, or only ones
// to the given owner.
func ownedOnlyBy(o metav1.Object, uid types.UID) bool {
	for _, ref := range o.GetOwnerReferences() {
		if ref.UID != uid {
			return false
		}
	}
	return true
}

// ownedByOtherThanAddons reports whether an object has an owner that is not a
// ZaentrumAddon — something else claims it.
func ownedByOtherThanAddons(o metav1.Object) bool {
	for _, ref := range o.GetOwnerReferences() {
		if ref.Kind != "ZaentrumAddon" || !strings.HasPrefix(ref.APIVersion, zaentrumv1alpha1.GroupVersion.Group+"/") {
			return true
		}
	}
	return false
}

// sooner is the earlier of two requeue delays, where 0 means none.
func sooner(a, b time.Duration) time.Duration {
	switch {
	case a == 0:
		return b
	case b == 0 || a < b:
		return a
	default:
		return b
	}
}

func (r *ZaentrumAddonReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *ZaentrumAddonReconciler) reconcileAddon(ctx context.Context, a *zaentrumv1alpha1.ZaentrumAddon) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	// A changed spec fetches its chart fresh instead of from the ref cache.
	fresh := a.Status.ObservedGeneration != a.Generation

	if err := addon.ValidateName(a.Name); err != nil {
		// Only a new resource can fix its name.
		notPlanned(a, zaentrumv1alpha1.AddonFailed, "InvalidName", err.Error())
		return ctrl.Result{}, nil
	}

	reserved := reservedObjects(a)
	applied, err := r.inventory(ctx, a, reserved)
	if err != nil {
		return r.errored(a, err)
	}

	z, problem, err := r.findPlatform(ctx, a.Namespace)
	if err != nil {
		return r.errored(a, err)
	}
	if problem != "" {
		notPlanned(a, zaentrumv1alpha1.AddonFailed, "NoPlatform", problem)
		return r.withoutApply(ctx, a, applied, addonRequeueRetry)
	}
	platform, err := templates.AddonPlatformValues(z)
	if err != nil {
		notPlanned(a, zaentrumv1alpha1.AddonFailed, "PlatformValues", "platform values: "+err.Error())
		return r.withoutApply(ctx, a, applied, addonRequeueRetry)
	}

	if err := addon.ValidateChart(a.Spec.Chart); err != nil {
		notPlanned(a, failedPhase(a), "InvalidChart", err.Error())
		return r.withoutApply(ctx, a, applied, 0)
	}
	archive, err := r.charts().Fetch(ctx, a.Spec.Chart, fresh)
	if err != nil {
		notPlanned(a, failedPhase(a), "FetchFailed", err.Error())
		return r.withoutApply(ctx, a, applied, addonRequeueRetry)
	}
	chrt, err := addon.LoadChart(archive.Data)
	if err != nil {
		notPlanned(a, failedPhase(a), "InvalidChart", err.Error())
		return r.withoutApply(ctx, a, applied, addonRequeueRetry)
	}

	plan := &zaentrumv1alpha1.AddonPlan{Chart: addon.ChartInfo(chrt, archive.Digest)}
	var errs []string
	plan.ValuesSchema, errs = addon.SchemaForStatus(chrt)
	plan.ValuesErrors = append(plan.ValuesErrors, errs...)
	plan.Violations = addon.CheckConventions(chrt)

	sources, valuesObjects, err := r.valuesSources(ctx, a)
	if err != nil {
		return r.errored(a, err)
	}
	user, errs := addon.UserValues(a.Spec.Values, sources)
	plan.ValuesErrors = append(plan.ValuesErrors, errs...)

	// Generated values exist before the schema is checked, so a generated
	// property may be required.
	fields, err := addon.GeneratedFields(chrt.Schema)
	if err != nil {
		plan.ValuesErrors = append(plan.ValuesErrors, err.Error())
	}
	generated, errs, err := r.generatedValues(ctx, a, fields, user)
	if err != nil {
		return r.errored(a, err)
	}
	plan.ValuesErrors = append(plan.ValuesErrors, errs...)
	if err := r.adoptValues(ctx, a, valuesObjects); err != nil {
		return r.errored(a, err)
	}

	rendered, errs := addon.Render(addon.RenderInput{
		Chart:     chrt,
		Name:      a.Name,
		Namespace: a.Namespace,
		Values:    addon.LayerValues(generated, user, platform),
	})
	plan.ValuesErrors = append(plan.ValuesErrors, errs...)
	if rendered != nil {
		plan.Objects, plan.Workloads = addon.Summarize(rendered.Objects)
		plan.Violations = append(plan.Violations, addon.Violations(rendered.Objects, addon.GuardInput{
			Namespace:       a.Namespace,
			MediaClaim:      platformString(platform, "media", "claimName"),
			Primary:         chrt.Metadata.Annotations[addon.ChartAnnotationPrimary],
			Reserved:        reserved,
			EventsTLSSecret: platformString(platform, "events", "tlsSecret"),
			PullSecrets:     platformStrings(platform, "imagePullSecrets"),
		})...)
		collisions, err := addon.Collisions(rendered.Objects, a.UID, r.ownerLookup(ctx, a.Namespace))
		if err != nil {
			return r.errored(a, err)
		}
		plan.Violations = append(plan.Violations, collisions...)
		plan.Changes = addon.Changes(rendered.Objects, liveObjects(applied))
	}
	recordPlan(a, plan)

	if problems := len(plan.ValuesErrors) + len(plan.Violations); problems > 0 {
		msg := planProblem(plan, problems)
		a.Status.Phase = failedPhase(a)
		a.Status.Message = msg
		setAddonCondition(a, condTypePlanned, metav1.ConditionFalse, "PlanFailed", msg)
		setAddonCondition(a, condTypeReady, metav1.ConditionFalse, "PlanFailed", "nothing applied: "+msg)
		return r.withoutApply(ctx, a, applied, addonRequeueRetry)
	}
	setAddonCondition(a, condTypePlanned, metav1.ConditionTrue, "Planned",
		fmt.Sprintf("chart %s %s renders %d objects", plan.Chart.Name, plan.Chart.Version, len(plan.Objects)))

	if a.Spec.Suspend {
		a.Status.Phase = zaentrumv1alpha1.AddonPlanned
		a.Status.Message = "planned; suspended, so nothing is applied"
		setAddonCondition(a, condTypeReady, metav1.ConditionFalse, "Suspended", a.Status.Message)
		return r.withoutApply(ctx, a, applied, addonRequeueSettled)
	}

	addon.Decorate(rendered.Objects, addon.Decoration{
		Name:      a.Name,
		Namespace: a.Namespace,
		PartOf:    platformString(platform, "partOf"),
		Checksum:  rendered.Checksum,
	})
	addon.ApplyOrder(rendered.Objects)
	if err := r.apply(ctx, a, rendered.Objects); err != nil {
		return r.errored(a, err)
	}
	if err := r.prune(ctx, applied, rendered.Objects); err != nil {
		return r.errored(a, err)
	}
	a.Status.LastAppliedChart = &zaentrumv1alpha1.AddonChart{
		Ref:     a.Spec.Chart.Ref,
		Version: a.Spec.Chart.Version,
		Digest:  archive.Digest,
	}

	var deployments []string
	for _, o := range rendered.Objects {
		if o.GetKind() == "Deployment" {
			deployments = append(deployments, o.GetName())
		}
	}
	state, err := r.refreshComponents(ctx, a, deployments)
	if err != nil {
		return r.errored(a, err)
	}
	requeue := addonRequeueSettled
	switch {
	case state.allReady:
		a.Status.Phase = zaentrumv1alpha1.AddonReady
		a.Status.Message = fmt.Sprintf("applied %d objects; all %d components ready", len(rendered.Objects), len(deployments))
		setAddonCondition(a, condTypeReady, metav1.ConditionTrue, "AllComponentsReady", a.Status.Message)
	case len(state.stalled) > 0:
		// Like the platform: a rollout Kubernetes gave up on is not "still
		// installing", even though the previous pods may keep serving.
		a.Status.Phase = zaentrumv1alpha1.AddonDegraded
		a.Status.Message = "rollout stalled: " + strings.Join(state.stalled, ", ")
		setAddonCondition(a, condTypeReady, metav1.ConditionFalse, "RolloutStalled", a.Status.Message)
	case state.progressing:
		a.Status.Phase = zaentrumv1alpha1.AddonInstalling
		a.Status.Message = "waiting for " + strings.Join(state.waiting, ", ")
		setAddonCondition(a, condTypeReady, metav1.ConditionFalse, "ComponentsNotReady", a.Status.Message)
		requeue = addonRequeueRollout
	default:
		a.Status.Phase = zaentrumv1alpha1.AddonDegraded
		a.Status.Message = "not ready: " + strings.Join(state.waiting, ", ")
		setAddonCondition(a, condTypeReady, metav1.ConditionFalse, "ComponentsNotReady", a.Status.Message)
		requeue = addonRequeueRollout
	}
	logger.Info("reconciled addon", "phase", a.Status.Phase, "chart", plan.Chart.Name, "version", plan.Chart.Version,
		"objects", len(rendered.Objects))
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// finalize lets a ZaentrumAddon being removed go. Owner references take care of
// what the addon applied and the values Secrets it adopted; the finalizer only
// exists so an addon removed with zaentrum.io/keep-values: "true" can hand its
// values and generated Secrets back first. It never holds a removal for good:
// with the platform gone or the namespace terminating it just lets go.
func (r *ZaentrumAddonReconciler) finalize(ctx context.Context, a *zaentrumv1alpha1.ZaentrumAddon) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(a, addon.FinalizerValues) {
		return ctrl.Result{}, nil
	}
	logger := log.FromContext(ctx)
	if a.Annotations[addon.AnnotationKeepValues] == "true" {
		reason, err := r.cannotKeepValues(ctx, a)
		if err != nil {
			return ctrl.Result{}, err
		}
		if reason != "" {
			logger.Info("addon removed without keeping its values: "+reason, "addon", a.Name)
		} else if err := r.keepValues(ctx, a); err != nil {
			return ctrl.Result{}, err
		}
	}
	base := a.DeepCopy()
	controllerutil.RemoveFinalizer(a, addon.FinalizerValues)
	if err := r.Patch(ctx, a, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	return ctrl.Result{}, nil
}

// cannotKeepValues says why an addon's values cannot be kept on removal — its
// namespace is going away (and everything in it), or there is no platform left
// — or returns "" when they can.
func (r *ZaentrumAddonReconciler) cannotKeepValues(ctx context.Context, a *zaentrumv1alpha1.ZaentrumAddon) (string, error) {
	var ns corev1.Namespace
	switch err := r.reader().Get(ctx, types.NamespacedName{Name: a.Namespace}, &ns); {
	case apierrors.IsNotFound(err):
		return "the namespace is gone", nil
	case err != nil:
		return "", fmt.Errorf("read namespace: %w", err)
	case !ns.DeletionTimestamp.IsZero() || ns.Status.Phase == corev1.NamespaceTerminating:
		return "the namespace is terminating", nil
	}
	var platforms zaentrumv1alpha1.ZaentrumList
	if err := r.List(ctx, &platforms, client.InNamespace(a.Namespace)); err != nil {
		return "", fmt.Errorf("list platforms: %w", err)
	}
	if len(platforms.Items) == 0 {
		return "no platform in this namespace", nil
	}
	return "", nil
}

// keepValues hands an addon's values and generated Secrets back before the
// addon goes: this addon's owner references come off, so garbage collection
// leaves them, and zaentrum.io/keep=true makes the operator's own collection
// skip them. A Secret that disappeared meanwhile has nothing left to keep.
// Only metadata is read and written — never a secret value.
func (r *ZaentrumAddonReconciler) keepValues(ctx context.Context, a *zaentrumv1alpha1.ZaentrumAddon) error {
	logger := log.FromContext(ctx)
	secrets := &metav1.PartialObjectMetadataList{}
	secrets.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("SecretList"))
	if err := r.reader().List(ctx, secrets, client.InNamespace(a.Namespace), client.MatchingLabels{addon.LabelAddon: a.Name}); err != nil {
		return fmt.Errorf("list values secrets: %w", err)
	}
	for i := range secrets.Items {
		s := &secrets.Items[i]
		if !addon.IsValuesSecretName(a.Name, s.Name) && s.Name != addon.GeneratedSecretName(a.Name) {
			continue
		}
		s.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
		base := s.DeepCopy()
		var owners []metav1.OwnerReference
		for _, ref := range s.OwnerReferences {
			if ref.UID != a.UID {
				owners = append(owners, ref)
			}
		}
		s.OwnerReferences = owners
		if s.Labels == nil {
			s.Labels = map[string]string{}
		}
		s.Labels[addon.LabelKeep] = "true"
		switch err := r.Patch(ctx, s, client.MergeFrom(base)); {
		case apierrors.IsNotFound(err):
			logger.Info("values secret disappeared before it could be kept", "addon", a.Name, "secret", s.Name)
		case err != nil:
			return fmt.Errorf("keep values secret %s: %w", s.Name, err)
		}
	}
	return nil
}

func (r *ZaentrumAddonReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *ZaentrumAddonReconciler) charts() ChartSource {
	if r.Charts == nil {
		r.Charts = &addon.Fetcher{}
	}
	return r.Charts
}

// failedPhase is the phase of an addon whose plan cannot go ahead: PlanFailed
// while it is only being planned, Failed when it was meant to be installed.
func failedPhase(a *zaentrumv1alpha1.ZaentrumAddon) string {
	if a.Spec.Suspend {
		return zaentrumv1alpha1.AddonPlanFailed
	}
	return zaentrumv1alpha1.AddonFailed
}

// recordPlan stores the plan made for the addon's current generation — the
// pairing a client checks before it lets someone install a plan.
func recordPlan(a *zaentrumv1alpha1.ZaentrumAddon, plan *zaentrumv1alpha1.AddonPlan) {
	a.Status.Plan = plan
	a.Status.ObservedGeneration = a.Generation
}

// notPlanned records that no plan could be made for the current generation.
func notPlanned(a *zaentrumv1alpha1.ZaentrumAddon, phase, reason, msg string) {
	recordPlan(a, nil)
	a.Status.Phase = phase
	a.Status.Message = msg
	setAddonCondition(a, condTypePlanned, metav1.ConditionFalse, reason, msg)
	setAddonCondition(a, condTypeReady, metav1.ConditionFalse, reason, msg)
}

// errored records an unexpected error and returns it for a retry with backoff.
// The plan is left as it was: its observedGeneration still says which spec it
// belongs to.
func (r *ZaentrumAddonReconciler) errored(a *zaentrumv1alpha1.ZaentrumAddon, err error) (ctrl.Result, error) {
	a.Status.Phase = failedPhase(a)
	a.Status.Message = err.Error()
	setAddonCondition(a, condTypeReady, metav1.ConditionFalse, "ReconcileError", err.Error())
	return ctrl.Result{}, err
}

// withoutApply finishes a pass that applied nothing: components still report
// what is running from an earlier apply.
func (r *ZaentrumAddonReconciler) withoutApply(ctx context.Context, a *zaentrumv1alpha1.ZaentrumAddon, applied []*unstructured.Unstructured, requeue time.Duration) (ctrl.Result, error) {
	var deployments []string
	for _, o := range applied {
		if o.GetKind() == "Deployment" {
			deployments = append(deployments, o.GetName())
		}
	}
	if _, err := r.refreshComponents(ctx, a, deployments); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

func planProblem(plan *zaentrumv1alpha1.AddonPlan, problems int) string {
	first := ""
	if len(plan.Violations) > 0 {
		first = plan.Violations[0]
	} else {
		first = plan.ValuesErrors[0]
	}
	if problems == 1 {
		return first
	}
	return fmt.Sprintf("%s (and %d more)", first, problems-1)
}

func setAddonCondition(a *zaentrumv1alpha1.ZaentrumAddon, condType string, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&a.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: a.Generation,
	})
}

// platformString reads a string from the reserved platform values.
func platformString(platform map[string]interface{}, keys ...string) string {
	v, _ := addon.GetPath(platform, keys)
	s, _ := v.(string)
	return s
}

// platformStrings reads a string list from the reserved platform values.
func platformStrings(platform map[string]interface{}, keys ...string) []string {
	v, _ := addon.GetPath(platform, keys)
	list, _ := v.([]interface{})
	out := make([]string, 0, len(list))
	for _, e := range list {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// findPlatform returns the one Zaentrum in the namespace, or why there is not
// exactly one.
func (r *ZaentrumAddonReconciler) findPlatform(ctx context.Context, namespace string) (*zaentrumv1alpha1.Zaentrum, string, error) {
	var list zaentrumv1alpha1.ZaentrumList
	if err := r.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, "", fmt.Errorf("list platforms: %w", err)
	}
	switch len(list.Items) {
	case 0:
		return nil, "no platform in this namespace", nil
	case 1:
		return &list.Items[0], "", nil
	default:
		return nil, fmt.Sprintf("%d platforms in this namespace; an addon needs exactly one", len(list.Items)), nil
	}
}

// reservedObjects are the addon's own values objects, which a chart may not
// render and prune never touches.
func reservedObjects(a *zaentrumv1alpha1.ZaentrumAddon) map[string]bool {
	reserved := map[string]bool{"Secret/" + addon.GeneratedSecretName(a.Name): true}
	for _, ref := range a.Spec.ValuesFrom {
		reserved[ref.Kind+"/"+ref.Name] = true
	}
	return reserved
}

// inventory lists what is applied for the addon: objects of the allowed kinds
// labelled for it and managed by the operator, controlled by this very
// ZaentrumAddon, minus its values objects.
func (r *ZaentrumAddonReconciler) inventory(ctx context.Context, a *zaentrumv1alpha1.ZaentrumAddon, reserved map[string]bool) ([]*unstructured.Unstructured, error) {
	var out []*unstructured.Unstructured
	for _, gvk := range addon.AllowedKinds {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
		if err := r.reader().List(ctx, list, client.InNamespace(a.Namespace), client.MatchingLabels{
			addon.LabelAddon:     a.Name,
			addon.LabelManagedBy: addon.ManagedBy,
		}); err != nil {
			return nil, fmt.Errorf("list applied %s objects: %w", gvk.Kind, err)
		}
		for i := range list.Items {
			o := &list.Items[i]
			o.SetGroupVersionKind(gvk)
			owner := metav1.GetControllerOfNoCopy(o)
			if reserved[addon.Key(o)] || owner == nil || owner.UID != a.UID {
				continue
			}
			out = append(out, o)
		}
	}
	return out, nil
}

func liveObjects(objs []*unstructured.Unstructured) []addon.LiveObject {
	out := make([]addon.LiveObject, 0, len(objs))
	for _, o := range objs {
		out = append(out, addon.Live(o))
	}
	return out
}

// valuesReadDenied is the reason a valuesFrom object outside the addon's own
// values namespace is refused.
const valuesReadDenied = "only the addon's own zaentrum-addon-<name>-* Secrets/ConfigMaps labelled zaentrum.io/addon=<name> may be referenced"

// valuesSources reads the valuesFrom objects, each once, and returns the sources
// plus the objects it actually adopted. A ref that names anything but the
// addon's own values objects (name prefix + label) is refused WITHOUT reading it
// — the operator is a cluster-wide secret reader, and this stops an addon from
// making it pipe a platform secret into chart values (confused deputy).
func (r *ZaentrumAddonReconciler) valuesSources(ctx context.Context, a *zaentrumv1alpha1.ZaentrumAddon) ([]addon.ValuesSource, []client.Object, error) {
	read := map[string]client.Object{}
	rejected := map[string]bool{}
	var objects []client.Object
	var sources []addon.ValuesSource
	for _, ref := range a.Spec.ValuesFrom {
		src := addon.ValuesSource{Ref: ref}
		// The name prefix is checked before any read, so a foreign object is
		// never fetched.
		if !strings.HasPrefix(ref.Name, addon.ValuesObjectPrefix(a.Name)) {
			src.Reject = valuesReadDenied
			sources = append(sources, src)
			continue
		}
		key := ref.Kind + "/" + ref.Name
		obj, done := read[key]
		if !done {
			switch ref.Kind {
			case "Secret":
				obj = &corev1.Secret{}
			case "ConfigMap":
				obj = &corev1.ConfigMap{}
			}
			if obj != nil {
				err := r.reader().Get(ctx, types.NamespacedName{Namespace: a.Namespace, Name: ref.Name}, obj)
				switch {
				case apierrors.IsNotFound(err):
					obj = nil
				case err != nil:
					return nil, nil, fmt.Errorf("read valuesFrom %s: %w", key, err)
				case !addon.OwnsValuesObject(a.Name, obj.GetName(), obj.GetLabels()):
					// Right name, wrong (or missing) label: still not the addon's.
					rejected[key] = true
					obj = nil
				default:
					objects = append(objects, obj)
				}
			}
			read[key] = obj
		}
		if rejected[key] {
			src.Reject = valuesReadDenied
			sources = append(sources, src)
			continue
		}
		switch o := obj.(type) {
		case *corev1.Secret:
			src.Found, src.Data = true, o.Data
		case *corev1.ConfigMap:
			src.Found, src.Data = true, map[string][]byte{}
			for k, v := range o.BinaryData {
				src.Data[k] = v
			}
			for k, v := range o.Data {
				src.Data[k] = []byte(v)
			}
		}
		sources = append(sources, src)
	}
	return sources, objects, nil
}

// generatedValues makes sure every generated field has a value stored in the
// addon's generated values Secret — creating the Secret or adding keys, never
// changing one — and returns the values to layer in.
func (r *ZaentrumAddonReconciler) generatedValues(ctx context.Context, a *zaentrumv1alpha1.ZaentrumAddon, fields []addon.GeneratedField, user map[string]interface{}) (map[string]string, []string, error) {
	if len(fields) == 0 {
		return nil, nil, nil
	}
	var sec corev1.Secret
	key := types.NamespacedName{Namespace: a.Namespace, Name: addon.GeneratedSecretName(a.Name)}
	err := r.reader().Get(ctx, key, &sec)
	exists := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, nil, fmt.Errorf("read generated values: %w", err)
	}
	missing, errs, err := addon.GenerateMissing(fields, user, sec.Data)
	if err != nil {
		return nil, nil, err
	}
	if len(missing) > 0 {
		if exists {
			// The optimistic lock turns a concurrent writer into a conflict
			// and a retry, instead of a key overwritten.
			base := sec.DeepCopy()
			if sec.Data == nil {
				sec.Data = map[string][]byte{}
			}
			for k, v := range missing {
				sec.Data[k] = []byte(v)
			}
			if err := r.Patch(ctx, &sec, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				return nil, nil, fmt.Errorf("store generated values: %w", err)
			}
		} else {
			sec = corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      key.Name,
					Namespace: key.Namespace,
					Labels:    map[string]string{addon.LabelAddon: a.Name},
				},
				Type: corev1.SecretTypeOpaque,
				Data: map[string][]byte{},
			}
			for k, v := range missing {
				sec.Data[k] = []byte(v)
			}
			if err := controllerutil.SetControllerReference(a, &sec, r.Scheme); err != nil {
				return nil, nil, err
			}
			if err := r.Create(ctx, &sec); err != nil {
				return nil, nil, fmt.Errorf("store generated values: %w", err)
			}
		}
	}
	return addon.GeneratedValues(fields, sec.Data), errs, nil
}

// adoptValues ties the addon's values objects to it, so removing the addon
// removes them: a valuesFrom Secret/ConfigMap labelled for this addon gets an
// owner reference — and loses it again once labelled zaentrum.io/keep=true.
func (r *ZaentrumAddonReconciler) adoptValues(ctx context.Context, a *zaentrumv1alpha1.ZaentrumAddon, objects []client.Object) error {
	for _, obj := range objects {
		labels := obj.GetLabels()
		// Never take ownership of anything but the addon's own values objects,
		// so a foreign object referenced in valuesFrom can never be
		// garbage-collected with the addon.
		if !addon.OwnsValuesObject(a.Name, obj.GetName(), labels) {
			continue
		}
		keep := labels[addon.LabelKeep] == "true"
		owned := false
		for _, ref := range obj.GetOwnerReferences() {
			owned = owned || ref.UID == a.UID
		}
		if keep != owned {
			continue
		}
		base := obj.DeepCopyObject().(client.Object)
		var err error
		if keep {
			err = controllerutil.RemoveOwnerReference(a, obj, r.Scheme)
		} else {
			err = controllerutil.SetOwnerReference(a, obj, r.Scheme)
		}
		if err != nil {
			return err
		}
		if err := r.Patch(ctx, obj, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return fmt.Errorf("adopt values %s: %w", obj.GetName(), err)
		}
	}
	return nil
}

// ownerLookup reads the metadata of an object named like a rendered one, for
// the collision check.
func (r *ZaentrumAddonReconciler) ownerLookup(ctx context.Context, namespace string) addon.OwnerLookup {
	return func(obj *unstructured.Unstructured) (bool, *metav1.OwnerReference, error) {
		live := &metav1.PartialObjectMetadata{}
		live.SetGroupVersionKind(obj.GroupVersionKind())
		err := r.reader().Get(ctx, types.NamespacedName{Namespace: namespace, Name: obj.GetName()}, live)
		if apierrors.IsNotFound(err) {
			return false, nil, nil
		}
		if err != nil {
			return false, nil, fmt.Errorf("check %s: %w", addon.Key(obj), err)
		}
		return true, metav1.GetControllerOf(live), nil
	}
}

// apply server-side applies every object, owned by the addon. Only objects no
// one else controls get here: the collision check refused the rest.
func (r *ZaentrumAddonReconciler) apply(ctx context.Context, a *zaentrumv1alpha1.ZaentrumAddon, objs []*unstructured.Unstructured) error {
	for _, obj := range objs {
		if err := controllerutil.SetControllerReference(a, obj, r.Scheme); err != nil {
			return fmt.Errorf("set owner reference on %s: %w", addon.Key(obj), err)
		}
		err := r.Patch(ctx, obj, client.Apply, client.FieldOwner(addon.FieldManager), client.ForceOwnership)
		if err != nil && obj.GetKind() == "Job" && apierrors.IsInvalid(err) && strings.Contains(err.Error(), "field is immutable") {
			// A Job's pod template is immutable. A Job the chart changed — a
			// new image, new values — is replaced, which runs it again.
			stale := &unstructured.Unstructured{}
			stale.SetGroupVersionKind(obj.GroupVersionKind())
			stale.SetNamespace(obj.GetNamespace())
			stale.SetName(obj.GetName())
			if derr := r.Delete(ctx, stale, client.PropagationPolicy(metav1.DeletePropagationBackground)); derr != nil && !apierrors.IsNotFound(derr) {
				return fmt.Errorf("replace %s: %w", addon.Key(obj), derr)
			}
			err = r.Patch(ctx, obj, client.Apply, client.FieldOwner(addon.FieldManager), client.ForceOwnership)
		}
		if err != nil {
			return fmt.Errorf("apply %s: %w", addon.Key(obj), err)
		}
	}
	return nil
}

// prune deletes the applied objects the chart no longer renders.
func (r *ZaentrumAddonReconciler) prune(ctx context.Context, applied, rendered []*unstructured.Unstructured) error {
	stale := map[string]bool{}
	for _, l := range addon.Prune(rendered, liveObjects(applied)) {
		stale[l.Kind+"/"+l.Name] = true
	}
	for _, obj := range applied {
		if !stale[addon.Key(obj)] {
			continue
		}
		uid := obj.GetUID()
		err := r.Delete(ctx, obj, client.PropagationPolicy(metav1.DeletePropagationBackground), client.Preconditions{UID: &uid})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("prune %s: %w", addon.Key(obj), err)
		}
	}
	return nil
}

// componentState is the readiness roll-up of an addon's Deployments.
type componentState struct {
	allReady    bool
	progressing bool
	stalled     []string
	waiting     []string
}

// refreshComponents records the named Deployments' live readiness in
// status.components.
func (r *ZaentrumAddonReconciler) refreshComponents(ctx context.Context, a *zaentrumv1alpha1.ZaentrumAddon, names []string) (componentState, error) {
	state := componentState{allReady: true}
	comps := make([]zaentrumv1alpha1.AddonComponentStatus, 0, len(names))
	for _, name := range names {
		comp := zaentrumv1alpha1.AddonComponentStatus{Name: name, Kind: "Deployment", Desired: 1}
		var dep appsv1.Deployment
		err := r.Get(ctx, types.NamespacedName{Namespace: a.Namespace, Name: name}, &dep)
		switch {
		case apierrors.IsNotFound(err):
			comp.Reason = "not created yet"
			state.allReady = false
			state.progressing = true
			state.waiting = append(state.waiting, name+" 0/1")
			comps = append(comps, comp)
			continue
		case err != nil:
			return state, fmt.Errorf("get deployment %s: %w", name, err)
		}

		if dep.Spec.Replicas != nil {
			comp.Desired = *dep.Spec.Replicas
		}
		// A rollout is complete once no pod of an older ReplicaSet is left —
		// available replicas alone also count the pods being replaced.
		complete := dep.Status.ObservedGeneration >= dep.Generation &&
			dep.Status.UpdatedReplicas >= comp.Desired &&
			dep.Status.Replicas == dep.Status.UpdatedReplicas
		comp.Ready = dep.Status.AvailableReplicas
		if !complete && dep.Status.UpdatedReplicas < comp.Ready {
			comp.Ready = dep.Status.UpdatedReplicas
		}
		ready := complete && comp.Desired > 0 && dep.Status.AvailableReplicas >= comp.Desired

		if stalled := rolloutStalled(&dep); stalled != "" {
			comp.Reason = stalled
			state.stalled = append(state.stalled, fmt.Sprintf("%s (%s)", name, stalled))
			ready = false
		} else if !ready {
			if !complete {
				state.progressing = true
			}
			comp.Reason = r.podProblem(ctx, &dep)
			if comp.Reason == "" && comp.Desired == 0 {
				comp.Reason = "scaled to zero"
			} else if comp.Reason == "" {
				comp.Reason = "rollout in progress"
			}
		}
		if !ready {
			state.allReady = false
			state.waiting = append(state.waiting, fmt.Sprintf("%s %d/%d (%s)", name, comp.Ready, comp.Desired, comp.Reason))
		}
		comps = append(comps, comp)
	}
	a.Status.Components = comps
	return state, nil
}

// podProblem names what keeps a Deployment's pods from running — an image that
// does not pull, a container refused for running as root — or "".
func (r *ZaentrumAddonReconciler) podProblem(ctx context.Context, dep *appsv1.Deployment) string {
	selector, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
	if err != nil || selector.Empty() {
		return ""
	}
	var pods corev1.PodList
	if err := r.reader().List(ctx, &pods, client.InNamespace(dep.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return ""
	}
	for _, pod := range pods.Items {
		statuses := append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...)
		for _, cs := range statuses {
			if w := cs.State.Waiting; w != nil && w.Reason != "" && w.Reason != "ContainerCreating" && w.Reason != "PodInitializing" {
				return truncate(strings.TrimSuffix(w.Reason+": "+w.Message, ": "), 256)
			}
		}
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason != "" {
				return truncate(strings.TrimSuffix(c.Reason+": "+c.Message, ": "), 256)
			}
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// SetupWithManager watches ZaentrumAddons, the Deployments they own, the
// values Secrets/ConfigMaps they read (metadata only) and the platform they
// install next to.
func (r *ZaentrumAddonReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		// Status writes do not bump the generation, so the addon's own status
		// updates do not re-trigger it.
		For(&zaentrumv1alpha1.ZaentrumAddon{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&appsv1.Deployment{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.addonsReading("Secret")), builder.OnlyMetadata).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.addonsReading("ConfigMap")), builder.OnlyMetadata).
		Watches(&zaentrumv1alpha1.Zaentrum{}, handler.EnqueueRequestsFromMapFunc(r.addonsInNamespace)).
		Complete(r)
}

// addonsReading maps a Secret/ConfigMap event to the addons in its namespace
// that read it through valuesFrom or keep their generated values in it.
func (r *ZaentrumAddonReconciler) addonsReading(kind string) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		var list zaentrumv1alpha1.ZaentrumAddonList
		if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
			return nil
		}
		var reqs []reconcile.Request
		for i := range list.Items {
			a := &list.Items[i]
			if readsObject(a, kind, obj.GetName()) {
				reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: a.Namespace, Name: a.Name}})
			}
		}
		return reqs
	}
}

func readsObject(a *zaentrumv1alpha1.ZaentrumAddon, kind, name string) bool {
	if kind == "Secret" && name == addon.GeneratedSecretName(a.Name) {
		return true
	}
	for _, ref := range a.Spec.ValuesFrom {
		if ref.Kind == kind && ref.Name == name {
			return true
		}
	}
	return false
}

// addonsInNamespace maps a platform event to every addon next to it: the
// platform values they render with may have changed.
func (r *ZaentrumAddonReconciler) addonsInNamespace(ctx context.Context, obj client.Object) []reconcile.Request {
	var list zaentrumv1alpha1.ZaentrumAddonList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for _, a := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: a.Namespace, Name: a.Name}})
	}
	return reqs
}

// AddonControllerWhenServed starts the addon controller once the ZaentrumAddon
// API can be listed: its CRD installed and the operator allowed to read it.
//
// Applying that CRD and ClusterRole is a separate cluster-admin step. Watching
// a kind the API server does not serve fails the manager — the platform
// reconciler with it — so an operator rolled out ahead of that step keeps
// reconciling the platform and picks addons up once the API is there.
func AddonControllerWhenServed(mgr ctrl.Manager, r *ZaentrumAddonReconciler) manager.Runnable {
	return manager.RunnableFunc(func(ctx context.Context) error {
		logger := ctrl.Log.WithName("addons")
		waiting := false
		for {
			err := mgr.GetAPIReader().List(ctx, &zaentrumv1alpha1.ZaentrumAddonList{}, client.Limit(1))
			if err == nil {
				if err := r.SetupWithManager(mgr); err != nil {
					logger.Error(err, "unable to start the addon controller")
					return nil
				}
				logger.Info("addon controller started")
				if err := mgr.Add(r.valuesSweep()); err != nil {
					logger.Error(err, "unable to start the orphaned values sweep")
				}
				return nil
			}
			if !waiting {
				logger.Info("addon controller waits for the ZaentrumAddon API (CRD and RBAC)", "error", err.Error())
				waiting = true
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(addonAPIPollInterval):
			}
		}
	})
}
