package controller

import (
	"context"
	"encoding/json"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
	"github.com/zaentrum/zaentrum-operator/operator/internal/templates"
)

// This file reads what the ConfigMap zaentrum-backup says made the sets beyond
// what the chart knows itself (templates/backup-record.yaml): the operator's
// version, the resource's spec as it is, the names of the Secrets, and the
// addons. Every backup's Job writes it into its set's backup.json, beside the
// platform's name, uid and the version that runs; a restore reads none of it
// back into the install.

// providedSecrets are the Secrets the platform reads by a fixed name that
// somebody provides and the operator never makes: the metadata key's
// (katalog-tmdb), the bug reports' (zaentrum-feedback) and, with an external
// identity provider, the workers' client (zaentrum-worker-oidc). A set names
// those that exist. The platform's own Secrets, which every install makes for
// itself and keeps through a restore, are named only with spec.secrets.external,
// where the admin provides them all.
var providedSecrets = []string{"katalog-tmdb", "zaentrum-feedback", "zaentrum-worker-oidc"}

// backupRecord reads the record of what makes the platform's sets this pass;
// operator is the operator's version. With external databases nothing is
// backed up, and nothing is read.
func (r *ZaentrumReconciler) backupRecord(ctx context.Context, z *zaentrumv1alpha1.Zaentrum, operator string) templates.BackupRecord {
	rec := templates.BackupRecord{Operator: operator}
	if z.Spec.Databases.Mode == "external" {
		return rec
	}
	rec.Resource = r.resourceSpec(ctx, z)
	rec.Secrets = r.recordSecrets(ctx, z)
	rec.Addons = r.recordAddons(ctx, z)
	return rec
}

// resourceSpec is the spec of the platform's resource as the API server keeps
// it, in JSON, or "" where it cannot be read: the chart then writes the CR's.
func (r *ZaentrumReconciler) resourceSpec(ctx context.Context, z *zaentrumv1alpha1.Zaentrum) string {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(zaentrumv1alpha1.GroupVersion.WithKind("Zaentrum"))
	if err := r.reader().Get(ctx, client.ObjectKeyFromObject(z), u); err != nil {
		log.FromContext(ctx).Info("backups: the resource could not be read for the record; its spec as this pass read it is written", "error", err.Error())
		return ""
	}
	spec, _, _ := unstructured.NestedMap(u.Object, "spec")
	if spec == nil {
		spec = map[string]interface{}{}
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return ""
	}
	return string(raw)
}

// recordSecrets are the names of the Secrets the resource refers to, of the
// provided ones that exist and, with spec.secrets.external, of every one of the
// platform's it reads, sorted: names, never a value.
func (r *ZaentrumReconciler) recordSecrets(ctx context.Context, z *zaentrumv1alpha1.Zaentrum) []string {
	names := map[string]bool{}
	if z.Spec.TLS != nil {
		names[tlsSecretName(z)] = true
	}
	if z.Spec.Secrets.External {
		// The admin provides them all: the operator makes none of them.
		for _, ps := range platformSecrets {
			if (ps.bundled && !bundledIdentity(z)) || (ps.needed != nil && !ps.needed(z)) {
				continue
			}
			names[ps.name] = true
		}
	}
	for _, n := range append([]string{z.Spec.Mail.SecretName, z.Spec.EventStreaming.CertSecret}, z.Spec.ImagePullSecrets...) {
		if n != "" {
			names[n] = true
		}
	}
	// Whether each provided one is there, by its metadata alone.
	for _, n := range providedSecrets {
		sec := &metav1.PartialObjectMetadata{}
		sec.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
		err := r.reader().Get(ctx, types.NamespacedName{Namespace: z.Namespace, Name: n}, sec)
		switch {
		case err == nil:
			names[n] = true
		case !apierrors.IsNotFound(err):
			log.FromContext(ctx).Info("backups: a Secret could not be read for the record; it is not named", "secret", n, "error", err.Error())
		}
	}
	out := make([]string, 0, len(names))
	for n := range names {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// recordAddons are the platform's addons, by name: each one's chart and the
// version installed, else the one its resource asks for.
func (r *ZaentrumReconciler) recordAddons(ctx context.Context, z *zaentrumv1alpha1.Zaentrum) []templates.BackupAddon {
	var list zaentrumv1alpha1.ZaentrumAddonList
	if err := r.List(ctx, &list, client.InNamespace(z.Namespace)); err != nil {
		log.FromContext(ctx).Info("backups: the addons could not be listed for the record", "error", err.Error())
		return nil
	}
	out := make([]templates.BackupAddon, 0, len(list.Items))
	for _, a := range list.Items {
		out = append(out, templates.BackupAddon{Name: a.Name, Chart: a.Spec.Chart.Ref, Version: installedVersion(&a)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// installedVersion is the version of an addon's chart its status says is
// installed: the chart it fetched and applied, by its archive's digest, else
// the version applied; else the one its resource asks for.
func installedVersion(a *zaentrumv1alpha1.ZaentrumAddon) string {
	if applied := a.Status.LastAppliedChart; applied != nil {
		if plan := a.Status.Plan; plan != nil && plan.Chart.Version != "" && plan.Chart.Digest != "" && plan.Chart.Digest == applied.Digest {
			return plan.Chart.Version
		}
		if applied.Version != "" {
			return applied.Version
		}
	}
	return a.Spec.Chart.Version
}
