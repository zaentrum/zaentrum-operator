package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
	"github.com/zaentrum/zaentrum-operator/operator/internal/templates"
)

// This file decides where the bundled Postgres keeps its data, and moves it
// there only by a copy.
//
// A new install keeps it on a claim (spec.storage.postgres). An install whose
// Postgres already runs somewhere else — on an emptyDir, where the chart kept
// it before, or on another claim — stays there, pass after pass: the decision
// is read from the running Postgres Deployment before every render, and a
// Postgres started on a new volume would start empty. The DatabasePersistent
// condition says so while it lasts.
//
// spec.storage.postgres.migrate moves it. The render then carries the claim and
// the chart's migration Job (templates/postgres-migrate.yaml), which the
// operator starts itself, as a Job of its own, once the running Postgres is
// available; the copy reads the running Postgres over the network and writes
// only into a volume that holds no database of anyone else's. Only once it has
// succeeded does a render switch the Postgres onto the claim — within
// migrationSwitchWindow of its end, as a copy older than that has missed what
// was written since, and is taken again instead. A copy that failed is left
// for a person to read: the Postgres stays where it was, and deleting the
// failed Job asks for another.
//
// While a copy is in flight neither a realm run nor a verification run starts:
// what they wrote to the database in the meantime would not be carried across.

const (
	condTypeDatabasePersistent = "DatabasePersistent"

	// labelDatabase marks the Jobs of database copies.
	labelDatabase     = "zaentrum.io/database"
	databaseMigration = "migration"

	postgresDeployment = "postgres"
	postgresDataVolume = "data"
	postgresEmptyDir   = "emptyDir"

	// migrationSwitchWindow is how long after a copy ended the Postgres may
	// still switch onto it. The switch normally follows on the next pass.
	migrationSwitchWindow = 5 * time.Minute
)

// databasePlan is this pass's decision about the bundled Postgres.
type databasePlan struct {
	// volume is where the Postgres's data is this render: "emptyDir" or a
	// claim's name; empty without a bundled Postgres.
	volume string
	// migrate renders the copy onto the claim; start says the operator starts
	// it this pass.
	migrate, start bool
	// copying is a copy in flight, or about to be.
	copying bool
}

// planDatabase decides where the bundled Postgres's data is this pass and
// writes the DatabasePersistent condition. An error means the running
// Postgres could not be read, and nothing may be rendered: any guess could
// start it on an empty volume.
func (r *ZaentrumReconciler) planDatabase(ctx context.Context, z *zaentrumv1alpha1.Zaentrum) (databasePlan, error) {
	if z.Spec.Databases.Mode == "external" {
		meta.RemoveStatusCondition(&z.Status.Conditions, condTypeDatabasePersistent)
		return databasePlan{}, nil
	}
	want := z.Spec.Storage.Postgres.ClaimName
	if want == "" {
		want = "postgres-data"
	}

	var dep appsv1.Deployment
	err := r.Get(ctx, types.NamespacedName{Namespace: z.Namespace, Name: postgresDeployment}, &dep)
	switch {
	case apierrors.IsNotFound(err):
		// A new install: it starts on the claim.
		r.persistent(ctx, z, want)
		return databasePlan{volume: want}, nil
	case err != nil:
		return databasePlan{}, fmt.Errorf("read the running Postgres: %w", err)
	}
	live, err := postgresVolume(&dep)
	if err != nil {
		setCondition(z, condTypeDatabasePersistent, metav1.ConditionFalse, "Unknown", err.Error())
		return databasePlan{}, err
	}
	if live == want {
		r.persistent(ctx, z, want)
		return databasePlan{volume: want}, nil
	}

	where := "an emptyDir"
	if live != postgresEmptyDir {
		where = "claim " + live
	}
	if !z.Spec.Storage.Postgres.Migrate {
		reason, msg := "EmptyDir", fmt.Sprintf("the bundled Postgres keeps its data on %s: a reschedule of its pod "+
			"loses every user, all watch state and the catalog. spec.storage.postgres.migrate copies it onto claim %s "+
			"and then switches; writes made while it copies are not carried across", where, want)
		if live != postgresEmptyDir {
			reason, msg = "OtherClaim", fmt.Sprintf("the bundled Postgres runs on %s, not on claim %s; "+
				"it moves only by a copy, with spec.storage.postgres.migrate", where, want)
		}
		setCondition(z, condTypeDatabasePersistent, metav1.ConditionFalse, reason, msg)
		return databasePlan{volume: live}, nil
	}

	copies, err := r.migrations(ctx, z, live, want)
	if err != nil {
		return databasePlan{}, fmt.Errorf("list the database copies: %w", err)
	}
	if len(copies) > 0 {
		latest := &copies[0]
		done, failed := jobFinished(latest)
		switch {
		case !done:
			msg := fmt.Sprintf("%s copies the bundled Postgres from %s onto claim %s", latest.Name, where, want)
			if pods, err := r.runPods(ctx, latest); err == nil {
				if hint := waitingHint(pods); hint != "" {
					msg += "; " + hint
				}
			}
			setCondition(z, condTypeDatabasePersistent, metav1.ConditionFalse, "Migrating", clip(msg, maxVerifyMessage))
			return databasePlan{volume: live, migrate: true, copying: true}, nil
		case failed != nil:
			setCondition(z, condTypeDatabasePersistent, metav1.ConditionFalse, "MigrationFailed",
				clip(fmt.Sprintf("%s could not copy the bundled Postgres onto claim %s: %s. It stays on %s; "+
					"delete Job %s to try again", latest.Name, want, r.migrationReport(ctx, latest, "no reason given"),
					where, latest.Name), maxVerifyMessage))
			return databasePlan{volume: live}, nil
		case r.now().Time.Sub(finishedAt(latest)) <= migrationSwitchWindow:
			// The copy has just succeeded: switch.
			log.FromContext(ctx).Info("database: switching the bundled Postgres onto its claim", "claim", want, "from", live, "job", latest.Name)
			setCondition(z, condTypeDatabasePersistent, metav1.ConditionTrue, "Migrated",
				clip(fmt.Sprintf("the bundled Postgres moves from %s onto claim %s: %s", where, want,
					r.migrationReport(ctx, latest, "copied")), maxVerifyMessage))
			return databasePlan{volume: want, copying: true}, nil
		default:
			// A copy that was never switched to has missed what was written
			// since it ended: take it again.
			if err := r.Delete(ctx, latest, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
				return databasePlan{}, fmt.Errorf("remove a stale copy: %w", err)
			}
		}
	}

	if ok, why := postgresAvailable(&dep); !ok {
		setCondition(z, condTypeDatabasePersistent, metav1.ConditionFalse, "Migrating",
			fmt.Sprintf("the bundled Postgres is copied onto claim %s once it is available: %s", want, why))
		return databasePlan{volume: live, migrate: true, copying: true}, nil
	}
	setCondition(z, condTypeDatabasePersistent, metav1.ConditionFalse, "Migrating",
		fmt.Sprintf("copying the bundled Postgres from %s onto claim %s", where, want))
	return databasePlan{volume: live, migrate: true, start: true, copying: true}, nil
}

// persistent writes the condition of a Postgres on its claim, naming the copy
// that moved it there while that Job is kept.
func (r *ZaentrumReconciler) persistent(ctx context.Context, z *zaentrumv1alpha1.Zaentrum, claim string) {
	msg := "the bundled Postgres keeps its data on claim " + claim
	if copies, err := r.migrations(ctx, z, "", claim); err == nil {
		for i := range copies {
			if done, failed := jobFinished(&copies[i]); done && failed == nil {
				msg += fmt.Sprintf(", where %s moved it from %s: %s", copies[i].Name,
					copies[i].Annotations[templates.AnnotationMigrationSource], r.migrationReport(ctx, &copies[i], "copied"))
				break
			}
		}
	}
	setCondition(z, condTypeDatabasePersistent, metav1.ConditionTrue, "OnClaim", clip(msg, maxVerifyMessage))
}

// startMigration starts the copy the render carries, as a Job of its own. The
// claim it writes was applied with the platform just before.
func (r *ZaentrumReconciler) startMigration(ctx context.Context, z *zaentrumv1alpha1.Zaentrum, hooks []*unstructured.Unstructured) error {
	hook := templates.PostgresMigrationJob(hooks)
	if hook == nil {
		return fmt.Errorf("the platform chart renders no migration Job")
	}
	job := hook.DeepCopy()
	job.SetName(templates.PostgresMigrationJobName + "-" + utilrand.String(verifyJobSuffix))
	job.SetNamespace(z.Namespace)
	job.SetResourceVersion("")
	annotations := map[string]string{}
	for k, v := range job.GetAnnotations() {
		if !strings.HasPrefix(k, "helm.sh/") {
			annotations[k] = v
		}
	}
	job.SetAnnotations(annotations)
	lbls := job.GetLabels()
	if lbls == nil {
		lbls = map[string]string{}
	}
	lbls[labelDatabase] = databaseMigration
	job.SetLabels(lbls)
	if err := controllerutil.SetControllerReference(z, job, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, job); err != nil {
		return err
	}
	log.FromContext(ctx).Info("database: started a copy", "job", job.GetName(),
		"from", annotations[templates.AnnotationMigrationSource], "onto", annotations[templates.AnnotationMigrationTarget])
	setCondition(z, condTypeDatabasePersistent, metav1.ConditionFalse, "Migrating",
		fmt.Sprintf("%s copies the bundled Postgres from %s onto claim %s", job.GetName(),
			annotations[templates.AnnotationMigrationSource], annotations[templates.AnnotationMigrationTarget]))
	return nil
}

// migrations lists the copies onto claim this Zaentrum owns, from source (any
// source when empty), newest first.
func (r *ZaentrumReconciler) migrations(ctx context.Context, z *zaentrumv1alpha1.Zaentrum, source, claim string) ([]batchv1.Job, error) {
	var jobs batchv1.JobList
	if err := r.reader().List(ctx, &jobs, client.InNamespace(z.Namespace),
		client.MatchingLabels{labelDatabase: databaseMigration}); err != nil {
		return nil, err
	}
	var out []batchv1.Job
	for _, j := range jobs.Items {
		if !metav1.IsControlledBy(&j, z) || j.DeletionTimestamp != nil ||
			j.Annotations[templates.AnnotationMigrationTarget] != claim ||
			(source != "" && j.Annotations[templates.AnnotationMigrationSource] != source) {
			continue
		}
		out = append(out, j)
	}
	sort.SliceStable(out, func(a, b int) bool {
		return out[b].CreationTimestamp.Before(&out[a].CreationTimestamp)
	})
	return out, nil
}

// migrationReport is the copy's termination message — what it copied, or why
// it could not — else fallback.
func (r *ZaentrumReconciler) migrationReport(ctx context.Context, job *batchv1.Job, fallback string) string {
	pods, err := r.runPods(ctx, job)
	if err != nil {
		return fallback
	}
	for _, pod := range pods {
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Name == templates.PostgresMigrationContainer && cs.State.Terminated != nil {
				if msg := strings.TrimSpace(cs.State.Terminated.Message); msg != "" {
					return msg
				}
			}
		}
	}
	return fallback
}

// postgresVolume is where the running Postgres Deployment keeps its data:
// "emptyDir" or a claim's name.
func postgresVolume(dep *appsv1.Deployment) (string, error) {
	for _, v := range dep.Spec.Template.Spec.Volumes {
		if v.Name != postgresDataVolume {
			continue
		}
		switch {
		case v.EmptyDir != nil:
			return postgresEmptyDir, nil
		case v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName != "":
			return v.PersistentVolumeClaim.ClaimName, nil
		}
		return "", fmt.Errorf("the running Postgres keeps its data on a volume the operator does not know; " +
			"it renders nothing that could move it")
	}
	return "", fmt.Errorf("the running Postgres has no data volume; the operator renders nothing that could move its data")
}

// postgresAvailable says whether the running Postgres answers, else why not.
func postgresAvailable(dep *appsv1.Deployment) (bool, string) {
	if dep.Status.AvailableReplicas < 1 {
		return false, fmt.Sprintf("%d replicas available", dep.Status.AvailableReplicas)
	}
	return true, ""
}
