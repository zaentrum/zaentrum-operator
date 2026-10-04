package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
	"github.com/zaentrum/zaentrum-operator/operator/internal/templates"
)

// This file reports the bundled Postgres's backups.
//
// The chart's CronJob (templates/backup.yaml) runs them: the operator applies
// it with the platform, and reads what its Jobs leave behind — each run's
// summary, the termination message of its pod, as it reads a verification
// run's — into status.backup and the Backup condition. The Jobs are read
// straight from the API server, as the verification's are.
//
// Backups are on by default wherever the bundled Postgres keeps its data on a
// claim. Turned off, the CronJob this Zaentrum owns is removed — here, so that
// one an operator applied before it labelled what it applies goes too, which
// the prune (prune.go) leaves alone — and the claim with the dumps stays.

const (
	condTypeBackup = "Backup"

	// The zaentrum.io/database values of the backup Jobs and the restore Jobs.
	databaseBackup  = "backup"
	databaseRestore = "restore"

	// maxBackupDumps is how many dump names status.backup.dumps holds.
	maxBackupDumps = 50
)

// backupReport is the summary files/postgres-backup.sh leaves as its
// termination message.
type backupReport struct {
	V         int                       `json:"v"`
	Dump      string                    `json:"dump"`
	Bytes     int64                     `json:"bytes"`
	Databases map[string]backupDatabase `json:"databases"`
	Kept      []string                  `json:"kept"`
	Free      int64                     `json:"free"`
}

type backupDatabase struct {
	Tables int64 `json:"tables"`
	Bytes  int64 `json:"bytes"`
}

// reportBackups writes status.backup and the Backup condition from the
// backups' Jobs. platform is the pass's render.
func (r *ZaentrumReconciler) reportBackups(ctx context.Context, z *zaentrumv1alpha1.Zaentrum, platform []*unstructured.Unstructured) {
	if z.Spec.Databases.Mode == "external" {
		// The databases are the tenant's, and so are their backups. A restore
		// once refused here is all there is to say.
		meta.RemoveStatusCondition(&z.Status.Conditions, condTypeBackup)
		if b := z.Status.Backup; b != nil && b.Restore != nil {
			z.Status.Backup = &zaentrumv1alpha1.BackupStatus{Restore: b.Restore}
		} else {
			z.Status.Backup = nil
		}
		return
	}
	if !rendered(platform, "CronJob", templates.BackupCronJobName) {
		r.removeBackupSchedule(ctx, z)
		msg := "the bundled Postgres keeps its data on an emptyDir and is not backed up: backups start once it is on " +
			"its claim (spec.storage.postgres.migrate), or with spec.backup.enabled true"
		if e := z.Spec.Backup.Enabled; e != nil && !*e {
			msg = "the bundled Postgres is not backed up: spec.backup.enabled is false"
		}
		setCondition(z, condTypeBackup, metav1.ConditionFalse, "Disabled", msg)
		return
	}

	jobs, err := r.backupJobs(ctx, z)
	if err != nil {
		log.FromContext(ctx).Info("backups: the runs could not be listed; status stays as it is", "error", err.Error())
		return
	}
	b := &zaentrumv1alpha1.BackupStatus{}
	if z.Status.Backup != nil {
		b = z.Status.Backup.DeepCopy()
	}
	if len(jobs) > 0 {
		b.Job = jobs[0].Name
	}
	var latest *batchv1.Job
	var latestFailed bool
	running := ""
	for i := range jobs {
		job := &jobs[i]
		done, failed := jobFinished(job)
		if !done {
			if running == "" {
				running = job.Name
			}
			continue
		}
		end := metav1.NewTime(finishedAt(job))
		if failed == nil {
			if b.LastSuccess == nil || end.After(b.LastSuccess.Time) {
				b.LastSuccess = &end
				if rep, ok := r.backupSummary(ctx, job); ok {
					b.LastDump = rep.Dump
					b.Dumps = rep.Kept
					if len(b.Dumps) > maxBackupDumps {
						b.Dumps = b.Dumps[:maxBackupDumps]
					}
				}
			}
		} else if b.LastFailure == nil || end.After(b.LastFailure.Time) {
			b.LastFailure = &end
		}
		if latest == nil {
			latest, latestFailed = job, failed != nil
		}
	}
	z.Status.Backup = b

	schedule := z.Spec.Backup.Schedule
	if schedule == "" {
		schedule = "@daily"
	}
	switch {
	case latest == nil:
		msg := "no backup has run yet: the first runs on the schedule " + schedule
		if running != "" {
			msg = running + " runs the first backup"
		}
		setCondition(z, condTypeBackup, metav1.ConditionUnknown, "Scheduled", msg)
	case latestFailed:
		msg := fmt.Sprintf("%s failed: %s; ", latest.Name, r.backupFailure(ctx, latest))
		if b.LastSuccess != nil && b.LastDump != "" {
			msg += fmt.Sprintf("the latest that succeeded wrote %s, %s", b.LastDump, b.LastSuccess.UTC().Format("2006-01-02 15:04 MST"))
		} else {
			msg += "no backup has succeeded yet"
		}
		setCondition(z, condTypeBackup, metav1.ConditionFalse, "Failed", clip(msg, maxVerifyMessage))
	default:
		msg := fmt.Sprintf("%s wrote %s", latest.Name, b.LastDump)
		if rep, ok := r.backupSummary(ctx, latest); ok {
			msg = fmt.Sprintf("%s: %d databases, %s; %d dumps kept, %s free", rep.Dump, len(rep.Databases),
				bytesOf(rep.Bytes), len(rep.Kept), bytesOf(rep.Free))
		}
		setCondition(z, condTypeBackup, metav1.ConditionTrue, "Succeeded", clip(msg, maxVerifyMessage))
	}
}

// rendered says whether the render carries an object of that kind and name.
func rendered(objs []*unstructured.Unstructured, kind, name string) bool {
	for _, o := range objs {
		if o.GetKind() == kind && o.GetName() == name {
			return true
		}
	}
	return false
}

// removeBackupSchedule deletes the backup CronJob this Zaentrum owns, its Jobs
// with it: backups were turned off. The prune removes it as well once it
// carries the operator's label; this removes it whether or not it does. The
// claim with the dumps stays.
func (r *ZaentrumReconciler) removeBackupSchedule(ctx context.Context, z *zaentrumv1alpha1.Zaentrum) {
	var cron batchv1.CronJob
	err := r.reader().Get(ctx, types.NamespacedName{Namespace: z.Namespace, Name: templates.BackupCronJobName}, &cron)
	if err != nil || !metav1.IsControlledBy(&cron, z) || cron.DeletionTimestamp != nil {
		return
	}
	if err := r.Delete(ctx, &cron, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
		log.FromContext(ctx).Info("backups: the schedule of backups turned off could not be removed; trying again", "error", err.Error())
		return
	}
	log.FromContext(ctx).Info("backups: turned off; removed their schedule", "cronjob", cron.Name)
}

// backupJobs lists the backups' Jobs in the platform's namespace, newest first.
func (r *ZaentrumReconciler) backupJobs(ctx context.Context, z *zaentrumv1alpha1.Zaentrum) ([]batchv1.Job, error) {
	var jobs batchv1.JobList
	if err := r.reader().List(ctx, &jobs, client.InNamespace(z.Namespace),
		client.MatchingLabels{labelDatabase: databaseBackup}); err != nil {
		return nil, err
	}
	out := make([]batchv1.Job, 0, len(jobs.Items))
	for _, j := range jobs.Items {
		if j.DeletionTimestamp == nil {
			out = append(out, j)
		}
	}
	sort.SliceStable(out, func(a, b int) bool {
		return out[b].CreationTimestamp.Before(&out[a].CreationTimestamp)
	})
	return out, nil
}

// backupMessage is the termination message the backup container of the Job's
// last pod left, or "".
func (r *ZaentrumReconciler) backupMessage(ctx context.Context, job *batchv1.Job) string {
	pods, err := r.runPods(ctx, job)
	if err != nil {
		return ""
	}
	var last *metav1.Time
	msg := ""
	for i := range pods {
		for _, cs := range pods[i].Status.ContainerStatuses {
			t := cs.State.Terminated
			if cs.Name != templates.BackupContainer || t == nil {
				continue
			}
			if last == nil || t.FinishedAt.After(last.Time) {
				finished := t.FinishedAt
				last, msg = &finished, strings.TrimSpace(t.Message)
			}
		}
	}
	return msg
}

// backupSummary is a successful run's summary, when it left a readable one.
func (r *ZaentrumReconciler) backupSummary(ctx context.Context, job *batchv1.Job) (backupReport, bool) {
	var rep backupReport
	if err := json.Unmarshal([]byte(r.backupMessage(ctx, job)), &rep); err != nil || rep.V != 1 || rep.Dump == "" {
		return backupReport{}, false
	}
	return rep, true
}

// backupFailure is why a failed run failed: its own message, else the Job's.
func (r *ZaentrumReconciler) backupFailure(ctx context.Context, job *batchv1.Job) string {
	if msg := r.backupMessage(ctx, job); msg != "" {
		return msg
	}
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == "True" {
			return strings.TrimSuffix(c.Reason+": "+c.Message, ": ")
		}
	}
	return "no reason given"
}

// backupClaim is the claim the dumps are kept on.
func backupClaim(z *zaentrumv1alpha1.Zaentrum) string {
	if z.Spec.Backup.ClaimName != "" {
		return z.Spec.Backup.ClaimName
	}
	return "backups"
}

// bytesOf is a byte count as people read it.
func bytesOf(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
