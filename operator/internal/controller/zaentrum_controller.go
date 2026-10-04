// Package controller implements the Zaentrum platform reconciler and the
// ZaentrumAddon reconciler that installs addon charts next to it.
package controller

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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
	"github.com/zaentrum/zaentrum-operator/operator/internal/digest"
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

	// stalled collects components whose rollout Kubernetes has given up on,
	// gathered during refreshComponents and consumed when the phase is set.
	// Reset every reconcile.
	stalled []string

	// ReleasesURL is the channel document the reconciler consults for Stage-2
	// auto-update discovery. Empty falls back to updates.DefaultReleasesURL.
	ReleasesURL string
	// Updates fetches/parses the channel document. The zero value is usable.
	Updates updates.Client

	// PinDigests resolves each ghcr.io/zaentrum/* image on a moving tag to its
	// current digest before apply, so a newly-pushed image actually rolls. A
	// re-rendered `:latest` is otherwise byte-identical and server-side apply is
	// a no-op, so nothing restarts. Off leaves the tag in place.
	PinDigests bool
	// Digest is the registry resolver. A shared instance keeps its digest cache
	// warm across the 30s reconciles.
	Digest *digest.Resolver

	// APIReader reads straight from the API server. Verification reads its
	// Jobs, their pods and the test account's Secret through it, so the
	// operator does not cache every Job and Pod in the cluster to do so. Nil
	// falls back to the client.
	APIReader client.Reader

	// Now is the clock verification timestamps come from; nil is time.Now.
	// Tests set it.
	Now func() time.Time

	// OpenShift says whether the cluster is OpenShift (serves its SCC API). Nil
	// until discovered, then kept (openshift.go); a test sets it.
	OpenShift *bool
	clusterMu sync.Mutex
}

// +kubebuilder:rbac:groups=zaentrum.io,resources=zaentrums,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=zaentrum.io,resources=zaentrums/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=zaentrum.io,resources=zaentrums/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=configmaps;secrets;services;serviceaccounts;persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete
// The controller reads its OWN pod (and the Deployment behind it) for
// status.controller. Both verbs are already in the ClusterRole, held so the
// operator may grant them to portal-api — this is the first use that reads
// with them rather than handing them on.
// Verification (verify.go) reads the pod of each run's Job to collect its
// report — the first use of pods/list here, also held for portal-api — and
// creates and deletes the run's Job and the test account's Secret with the jobs
// and secrets verbs above.
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list

// Reconcile renders the embedded templates for the Zaentrum CR and applies every
// object via server-side apply, then refreshes status.
func (r *ZaentrumReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// The Zaentrum is read from the API server, not the cache. This pass starts
	// Jobs from what status says about the last ones — a verification run, a
	// realm run, a database copy — and a cache that has not seen the status the
	// pass before wrote would have it start them again: one did, and replaced a
	// verification run in flight. Its status write then failed on the stale
	// resourceVersion, but the Jobs were already made. One GET a pass is cheap.
	var z zaentrumv1alpha1.Zaentrum
	if err := r.reader().Get(ctx, req.NamespacedName, &z); err != nil {
		// Deleted: owner references garbage-collect the managed resources.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Stage 2 — release-channel discovery. Resolve spec.channel to its target
	// tag from the published releases.json, then decide the tag to render this
	// pass. Discovery is best-effort: any failure leaves status.availableUpdate
	// unchanged and falls back to the spec/"latest" render so reconcile never
	// blocks on the network. The resolved target is reused for the controller's
	// own report below, so one pass consults the channel once.
	decision, channelTarget := r.resolveUpdate(ctx, &z)

	// Render the platform from the embedded templates with CR-driven values.
	// The decision's render tag overrides spec.version so auto-mode rolls the
	// channel target in this very pass.
	// Whether the cluster is OpenShift decides the user the chart names for its
	// pods, so the render waits for an answer rather than guess one.
	openShift, err := r.openShift()
	if err != nil {
		logger.Info("render deferred", "reason", err.Error())
		return ctrl.Result{RequeueAfter: verifyRequeueAfter}, nil
	}
	// Where the bundled Postgres keeps its data is read from the running one
	// before anything is rendered, so that no render moves it by itself: a
	// Postgres started on a new volume starts empty (database.go).
	db, err := r.planDatabase(ctx, &z)
	if err != nil {
		r.setApplied(&z, metav1.ConditionFalse, "DatabaseUnknown", err.Error())
		z.Status.Phase = "Error"
		_ = r.patchStatus(ctx, &z)
		return ctrl.Result{}, err
	}
	// A restore of the bundled Postgres stops every client of the database for
	// as long as it runs; whether one is in flight, or starts, is decided
	// before the render for the same reason (restore.go).
	restoreDump := r.planRestore(ctx, &z, db.copying)
	if restoreDump != "" {
		db.start = false
	}
	// The platform's own certificate (spec.tls): cert-manager issues it where it
	// is there, and the Routes, which cannot name a Secret, carry it inline
	// (tls.go). A Secret that cannot be read defers the render rather than
	// strip the Routes of the certificate they have.
	tlsSecret, issuing, err := r.platformCertificate(ctx, &z)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("read the platform's certificate: %w", err)
	}
	vals := templates.NewValues(&z)
	vals.OpenShift = openShift
	vals.Version = decision.RenderTag
	vals.PostgresVolume = db.volume
	vals.PostgresMigrate = db.migrate
	vals.RestoreDump = restoreDump
	certificateValues(&vals, &z, tlsSecret, issuing)
	rendered, err := templates.Render(vals)
	if err != nil {
		r.setApplied(&z, metav1.ConditionFalse, "RenderFailed", err.Error())
		z.Status.Phase = "Error"
		_ = r.patchStatus(ctx, &z)
		return ctrl.Result{}, fmt.Errorf("render templates: %w", err)
	}

	// The chart's hooks are not the platform. They are never applied with it;
	// the operator starts the ones it knows on its own, as Jobs of their own —
	// the verification Job among them (verify.go).
	objs, tests := templates.SplitHooks(rendered)

	z.Status.Phase = "Reconciling"

	// Pin our own images to digests so a fresh push on a moving tag actually
	// rolls. Applied unconditionally: pinning a concrete tag to its digest is a
	// harmless no-op flap-wise (that digest never moves), while a moving tag —
	// "latest" OR a channel tag like "edge" — is exactly the case that needs it.
	// Best-effort: an unresolvable image keeps its tag, so a registry hiccup
	// degrades to today's behaviour instead of blocking the reconcile. The
	// hooks are pinned in the same pass, so a run uses the images of its time.
	r.pinDigests(ctx, &z, append(objs[:len(objs):len(objs)], tests...))

	// The platform's Secrets, unless someone else provides them, before
	// anything that reads them is applied: made once, never rotated
	// (secrets.go). The chart renders none of them for the operator.
	if err := r.ensureSecrets(ctx, &z); err != nil {
		z.Status.Phase = "Error"
		_ = r.patchStatus(ctx, &z)
		return ctrl.Result{}, err
	}

	// A copy of the bundled Postgres has succeeded: move it onto its claim,
	// which the apply below could not do alone where kubectl co-owns the
	// emptyDir (database.go).
	if db.switching {
		if err := r.switchPostgres(ctx, &z, vals.PostgresVolume); err != nil {
			r.setApplied(&z, metav1.ConditionFalse, "ApplyFailed", "switch the bundled Postgres onto its claim: "+err.Error())
			z.Status.Phase = "Error"
			_ = r.patchStatus(ctx, &z)
			return ctrl.Result{}, err
		}
	}

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

	// A copy of the bundled Postgres onto its claim, which was just applied.
	if db.start {
		if err := r.startMigration(ctx, &z, tests); err != nil {
			setCondition(&z, condTypeDatabasePersistent, metav1.ConditionFalse, "MigrationFailed",
				clip("could not start the copy of the bundled Postgres: "+err.Error(), maxVerifyMessage))
		}
	}

	// The restore in flight takes its step now that its clients were applied
	// stopped, and the backups are read.
	restoreInFlight := r.stepRestore(ctx, &z, objs, tests)
	r.reportBackups(ctx, &z, objs)
	r.reportTLS(ctx, &z, tlsSecret, issuing)

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

	// The controller's report on ITSELF sits beside those and touches nothing
	// else — not the phase, not the conditions, not a component. The platform
	// version above is what this operator rolls out; status.controller is the
	// operator, which it reports and never upgrades. See internal/controller/self.go.
	z.Status.Controller = r.controllerReport(ctx, &z, channelTarget)

	switch {
	case restoreInFlight:
		// The database's clients are stopped on purpose: not Ready, and not
		// "still working on it" either.
		z.Status.Phase = "Restoring"
		msg := "restoring the bundled Postgres"
		if rs := restoreStatus(&z); rs != nil && rs.Message != "" {
			msg = rs.Message
		}
		r.setReady(&z, metav1.ConditionFalse, "Restoring", msg)
	case allReady:
		z.Status.Phase = "Ready"
		r.setReady(&z, metav1.ConditionTrue, "AllComponentsReady", "all components are ready")
	case len(r.stalled) > 0:
		// Distinct from Progressing on purpose. A rollout that has given up is
		// not "still working on it", and calling both the same is what makes a
		// wedged platform invisible: the old pods keep serving, so nothing
		// looks wrong anywhere.
		z.Status.Phase = "Degraded"
		msg := "rollout stalled: " + strings.Join(r.stalled, ", ")
		r.setReady(&z, metav1.ConditionFalse, "RolloutStalled", msg)
		logger.Info("rollout stalled", "components", strings.Join(r.stalled, ", "))
	default:
		z.Status.Phase = "Progressing"
		r.setReady(&z, metav1.ConditionFalse, "ComponentsNotReady", "waiting for components to become ready")
	}

	// The bundled realm holds what the chart decides about it — the sign-in
	// redirects — however old the realm is; see realm.go. It writes only the
	// RealmConfigured condition.
	hold := ""
	if db.copying {
		hold = "the bundled Postgres is being copied onto its claim"
	}
	if restoreInFlight {
		hold = "the bundled Postgres is being restored"
	}
	configuring := r.configureRealm(ctx, &z, objs, tests, hold)

	// The platform's self-test, after every update that leaves it Ready and on
	// request. It reads the readiness just computed and writes only
	// status.verification and the Verified condition; see verify.go. A run
	// waits for a realm run in flight, as a realm run waits for it: the realm
	// Job may end the master token the check's account preparation holds.
	// Nor does it start while the database is being copied: what it wrote
	// would not be carried across, and the Postgres is about to restart.
	verifying := r.verify(ctx, &z, vals.Version, objs, tests, allReady && !configuring && !db.copying && !restoreInFlight)

	if err := r.patchStatus(ctx, &z); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("reconciled zaentrum", "objects", len(objs), "phase", z.Status.Phase, "version", vals.Version)
	if verifying || configuring || db.copying || restoreInFlight {
		return ctrl.Result{RequeueAfter: verifyRequeueAfter}, nil
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// pinDigests rewrites the rendered ghcr.io/zaentrum/* images to their current
// digests. It authenticates the registry with the CR's pull secret so private
// repositories resolve too, and never fails the reconcile — an image it cannot
// resolve simply keeps its tag.
func (r *ZaentrumReconciler) pinDigests(ctx context.Context, z *zaentrumv1alpha1.Zaentrum, objs []*unstructured.Unstructured) {
	if !r.PinDigests {
		return
	}
	logger := log.FromContext(ctx)
	resolver := r.Digest
	if resolver == nil {
		resolver = digest.New(r.pullCreds(ctx, z))
	} else {
		resolver.SetCreds(r.pullCreds(ctx, z))
	}
	// The checker's image first: a platform tag the zae image lacks becomes
	// zae:latest, which the pass below then pins like every other image.
	if note := checkerFallback(ctx, resolver, objs); note != "" {
		logger.Info("verification: " + note)
	}
	n, errs := resolver.PinImages(ctx, objs, digest.ZaentrumImages)
	for _, err := range errs {
		logger.Info("digest pin skipped for an image (kept its tag)", "error", err.Error())
	}
	if n > 0 {
		logger.Info("pinned images to digests", "count", n)
	}
}

// pullCreds harvests registry credentials from the CR's imagePullSecrets so the
// digest resolver can read private manifests. Missing/unreadable secrets are not
// fatal — resolution just falls back to anonymous, and any private lookup then
// degrades to keeping the tag.
func (r *ZaentrumReconciler) pullCreds(ctx context.Context, z *zaentrumv1alpha1.Zaentrum) map[string]string {
	creds := map[string]string{}
	for _, name := range z.Spec.ImagePullSecrets {
		var sec corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Namespace: z.Namespace, Name: name}, &sec); err != nil {
			continue
		}
		body := sec.Data[corev1.DockerConfigJsonKey]
		if len(body) == 0 {
			continue
		}
		if c, err := digest.CredsFromDockerConfig(body); err == nil {
			for host, cred := range c {
				creds[host] = cred
			}
		}
	}
	return creds
}

// resolveUpdate performs Stage-2 channel discovery for one reconcile pass and
// returns the render/availableUpdate decision plus the raw channel target the
// controller's own report reuses (see self.go). It never returns an error:
// network or parse failures are logged and degrade to a spec/"latest" render
// with no surfaced update, so a flaky channel endpoint can never block a
// reconcile — and the empty target then tells the self-report it has nothing
// to compare against either.
func (r *ZaentrumReconciler) resolveUpdate(ctx context.Context, z *zaentrumv1alpha1.Zaentrum) (updates.Decision, string) {
	logger := log.FromContext(ctx)

	spec := z.Spec
	auto := spec.Update.Mode == zaentrumv1alpha1.UpdateAuto

	// A pinned spec.version opts out of channel tracking; skip the network.
	// The self-report inherits that: an air-gapped, pinned install makes no
	// outbound call for the platform, and must make none for the operator.
	if updates.IsPinned(spec.Version) {
		return updates.Decide(spec.Version, auto, ""), ""
	}

	channel := string(spec.Channel)
	if channel == "" {
		channel = string(zaentrumv1alpha1.ChannelStable)
	}

	rel, err := r.Updates.Fetch(ctx, r.ReleasesURL)
	if err != nil {
		logger.Info("release-channel discovery skipped (fetch failed)",
			"channel", channel, "error", err.Error())
		return updates.Decide(spec.Version, auto, ""), ""
	}

	target, err := rel.Resolve(channel)
	if err != nil {
		logger.Info("release-channel discovery skipped (resolve failed)",
			"channel", channel, "error", err.Error())
		return updates.Decide(spec.Version, auto, ""), ""
	}

	return updates.Decide(spec.Version, auto, target), target
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
		// Own namespaced resources so they cascade-delete with the CR — but
		// what the chart marks to be kept when the platform goes (the claim
		// with the backups), which Helm keeps on an uninstall too.
		if obj.GetNamespace() != "" && obj.GetAnnotations()["helm.sh/resource-policy"] != "keep" {
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
	r.stalled = nil

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
		// NOTE: AvailableReplicas counts the OLD ReplicaSet too. A deployment
		// whose new pods cannot start still reports its previous pods as
		// available, so this alone says "fine" while nothing new can roll. That
		// is exactly how an expired registry credential froze every component
		// for nine hours while the site kept serving and the CR kept saying
		// "Progressing" — a string that reads the same after 30 seconds as
		// after half a day.
		ready := dep.Status.AvailableReplicas >= desired && desired > 0
		if !ready {
			allReady = false
		}

		// Kubernetes already decides when a rollout has given up: the
		// Progressing condition flips to False with ProgressDeadlineExceeded
		// after progressDeadlineSeconds. Surface that rather than re-deriving
		// it, and name the component so the reason is actionable.
		if stalledReason := rolloutStalled(&dep); stalledReason != "" {
			allReady = false
			r.stalled = append(r.stalled, fmt.Sprintf("%s (%s)", name, stalledReason))
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

// rolloutStalled reports why a Deployment's rollout has given up, or "" if it
// is healthy or still legitimately in progress.
//
// It reads Kubernetes' own verdict (Progressing=False, normally
// ProgressDeadlineExceeded) rather than inventing a timer. The extra
// UpdatedReplicas check catches the case this was written for: the new
// ReplicaSet cannot produce a single pod — an unpullable image, an unschedulable
// node — while the previous pods keep every readiness signal green.
func rolloutStalled(dep *appsv1.Deployment) string {
	desired := int32(1)
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}
	for _, c := range dep.Status.Conditions {
		if c.Type == appsv1.DeploymentProgressing && c.Status == corev1.ConditionFalse {
			reason := c.Reason
			if reason == "" {
				reason = "ProgressDeadlineExceeded"
			}
			if dep.Status.UpdatedReplicas < desired {
				return fmt.Sprintf("%s, %d/%d updated replicas",
					reason, dep.Status.UpdatedReplicas, desired)
			}
			return reason
		}
	}
	return ""
}
