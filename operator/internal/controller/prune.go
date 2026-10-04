package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
	"github.com/zaentrum/zaentrum-operator/operator/internal/templates"
)

// This file removes what the operator applied for the platform and its render
// no longer carries.
//
// Server-side apply owns the fields of what the operator applies, never what
// it stops applying: an object a render no longer carries stays as the last
// pass left it — the pipeline's workers running after features.pipeline went
// off, a Route the routing no longer has. Every object the operator applies is
// labelled zaentrum.io/platform with its Zaentrum's name (applyAll), and after
// every apply the operator removes each object that
//
//   - is of a kind the chart renders and the operator removes
//     (templates.PruneKinds: Deployments, Services, ServiceAccounts, Routes,
//     Ingresses, CronJobs, Roles, RoleBindings, Certificates),
//   - is in the Zaentrum's namespace, carries the label for it, and is
//     controlled by it,
//   - and is not in this pass's render,
//
// logs each, and names them in the Pruned condition. It never removes anything
// without the label — what the operator applied before it labelled stays for a
// person to remove; anything another controls — an addon's, one someone took
// over; a claim, a Secret, a ConfigMap or a Job (templates.KeptKinds), which
// hold the databases, the backups, the library and the credentials, or are
// runs; what the chart marks helm.sh/resource-policy: keep; nor the bundled
// Postgres's Deployment, whose data lives in its pod while it runs on an
// emptyDir: a database is not removed because a render stopped naming it.
//
// A render is the spec and what the reconciler found out before it, and a
// lookup that gives no answer defers the render instead of guessing
// (openshift.go, tls.go, database.go): no pass removes what the next would
// render again.

const (
	condTypePruned = "Pruned"

	// keepPolicy is the annotation the chart marks what is kept with, as
	// Helm keeps it on an uninstall.
	keepPolicy = "helm.sh/resource-policy"

	// unservedRecheck is how long a kind the cluster does not serve — Routes
	// off OpenShift, Certificates without cert-manager — goes unlisted before
	// it is asked about again: each such list costs a discovery of the API.
	unservedRecheck = 10 * time.Minute
)

// prunePlatform takes this pass's step: it removes what the operator applied
// for z and the render no longer carries, and writes the Pruned condition. It
// never fails the reconcile: what it could not list or remove, it tries again
// on the next pass. platform is the pass's render, without its hooks.
func (r *ZaentrumReconciler) prunePlatform(ctx context.Context, z *zaentrumv1alpha1.Zaentrum, platform []*unstructured.Unstructured) {
	removed, err := r.prune(ctx, z, platform, false)
	when := r.now().UTC().Format("2006-01-02 15:04 MST")
	switch {
	case err != nil:
		msg := "could not remove all the platform no longer renders: " + strings.ReplaceAll(err.Error(), "\n", "; ")
		if len(removed) > 0 {
			msg = fmt.Sprintf("%s: removed %s; %s", when, strings.Join(removed, ", "), msg)
		}
		setCondition(z, condTypePruned, metav1.ConditionFalse, "Failed", clip(msg, maxVerifyMessage))
	case len(removed) > 0:
		setCondition(z, condTypePruned, metav1.ConditionTrue, "Removed", clip(fmt.Sprintf(
			"%s: removed %s, which the platform no longer renders", when, strings.Join(removed, ", ")), maxVerifyMessage))
	default:
		// The last removal stays named until there is another.
		if c := meta.FindStatusCondition(z.Status.Conditions, condTypePruned); c == nil || c.Status != metav1.ConditionTrue {
			setCondition(z, condTypePruned, metav1.ConditionTrue, "NothingToRemove",
				"nothing the operator applied is left that the platform no longer renders")
		}
	}
}

// prune removes the objects stale finds among those the operator applied for
// z, as Kind/name: each with background propagation, so a Deployment's pods
// go with it, and a precondition on its UID, so that one made anew since it
// was listed stays. dryRun lists and logs what it would remove, and removes
// nothing. The error joins every list and removal that failed; what it
// removed before one did is returned with it.
func (r *ZaentrumReconciler) prune(ctx context.Context, z *zaentrumv1alpha1.Zaentrum, platform []*unstructured.Unstructured, dryRun bool) ([]string, error) {
	logger := log.FromContext(ctx)
	rendered := map[schema.GroupKind]bool{}
	for _, o := range platform {
		rendered[o.GroupVersionKind().GroupKind()] = true
	}
	var live []metav1.PartialObjectMetadata
	var errs []error
	for _, gvk := range templates.PruneKinds {
		gk := gvk.GroupKind()
		if !rendered[gk] && r.unservedLately(gk) {
			continue
		}
		list := &metav1.PartialObjectMetadataList{}
		list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
		err := r.reader().List(ctx, list, client.InNamespace(z.Namespace),
			client.MatchingLabels{templates.LabelPlatform: platformLabel(z)})
		switch {
		case meta.IsNoMatchError(err):
			// The cluster does not serve the kind: there is nothing of it.
			r.served(gk, false)
			continue
		case err != nil:
			errs = append(errs, fmt.Errorf("list %s: %w", gvk.Kind, err))
			continue
		}
		r.served(gk, true)
		for i := range list.Items {
			item := list.Items[i]
			item.SetGroupVersionKind(gvk)
			live = append(live, item)
		}
	}

	var removed []string
	for _, o := range stale(z, live, platform) {
		name := o.Kind + "/" + o.Name
		if dryRun {
			logger.Info("prune: would remove an object the platform no longer renders (dry run)", "kind", o.Kind, "name", o.Name)
			removed = append(removed, name)
			continue
		}
		uid := o.UID
		err := r.Delete(ctx, o.DeepCopy(), client.PropagationPolicy(metav1.DeletePropagationBackground),
			client.Preconditions{UID: &uid})
		switch {
		case apierrors.IsNotFound(err):
			continue // gone since it was listed
		case err != nil:
			errs = append(errs, fmt.Errorf("remove %s: %w", name, err))
			continue
		}
		logger.Info("prune: removed an object the platform no longer renders", "kind", o.Kind, "name", o.Name)
		removed = append(removed, name)
	}
	return removed, errors.Join(errs...)
}

// stale is what of live the operator removes for z: objects of one of
// templates.PruneKinds in z's namespace, labelled for z and controlled by it,
// not on their way out, not kept, and not in platform, the pass's render —
// sorted by kind, then name.
func stale(z *zaentrumv1alpha1.Zaentrum, live []metav1.PartialObjectMetadata, platform []*unstructured.Unstructured) []metav1.PartialObjectMetadata {
	type key struct {
		gk   schema.GroupKind
		name string
	}
	rendered := map[key]bool{}
	for _, o := range platform {
		rendered[key{o.GroupVersionKind().GroupKind(), o.GetName()}] = true
	}
	prunable := map[schema.GroupKind]bool{}
	for _, gvk := range templates.PruneKinds {
		prunable[gvk.GroupKind()] = true
	}
	label := platformLabel(z)
	var out []metav1.PartialObjectMetadata
	for _, o := range live {
		gk := o.GroupVersionKind().GroupKind()
		owner := metav1.GetControllerOfNoCopy(&o)
		switch {
		case !prunable[gk]:
		case o.Namespace != z.Namespace:
		case o.Labels[templates.LabelPlatform] != label:
		case owner == nil || owner.UID != z.UID:
		case o.DeletionTimestamp != nil:
		case o.Annotations[keepPolicy] == "keep":
		case gk == (schema.GroupKind{Group: "apps", Kind: "Deployment"}) && o.Name == postgresDeployment:
			// The bundled Postgres: on an emptyDir its data is in its pod.
		case rendered[key{gk, o.Name}]:
		default:
			out = append(out, o)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// unservedLately says whether the cluster answered, less than unservedRecheck
// ago, that it does not serve gk.
func (r *ZaentrumReconciler) unservedLately(gk schema.GroupKind) bool {
	r.clusterMu.Lock()
	defer r.clusterMu.Unlock()
	at, ok := r.unserved[gk]
	return ok && r.now().Time.Sub(at) < unservedRecheck
}

// served records whether the cluster serves gk.
func (r *ZaentrumReconciler) served(gk schema.GroupKind, yes bool) {
	r.clusterMu.Lock()
	defer r.clusterMu.Unlock()
	if yes {
		delete(r.unserved, gk)
		return
	}
	if r.unserved == nil {
		r.unserved = map[schema.GroupKind]time.Time{}
	}
	r.unserved[gk] = r.now().Time
}
