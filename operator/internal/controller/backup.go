package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
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
// run's — into status.backup and the Backup condition, a backup asked for
// (backuprequest.go) among them. The Jobs are read straight from the API
// server, as the verification's are.
//
// Backups are on by default wherever the bundled Postgres keeps its data on a
// claim. Turned off, the CronJob this Zaentrum owns is removed — here, so that
// one an operator applied before it labelled what it applies goes too, which
// the prune (prune.go) leaves alone — and the claim with the dumps stays. A
// backup asked for that runs is the Zaentrum's, not the CronJob's: it runs on,
// and it is read, as it is while backups are on.

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
// backups' Jobs, and answers the backup asked for (answerBackupRequest). A pass
// that cannot read the Jobs, or the pod of one it has to read, leaves the
// status as it is: the next pass reads them. platform is the pass's render.
func (r *ZaentrumReconciler) reportBackups(ctx context.Context, z *zaentrumv1alpha1.Zaentrum, platform []*unstructured.Unstructured) {
	if z.Spec.Databases.Mode == "external" {
		// The databases are the tenant's, and so are their backups. A restore
		// once refused here and a backup asked for, which is refused at once,
		// are all there is to say; no condition says it, as one would read as
		// backups that are off.
		meta.RemoveStatusCondition(&z.Status.Conditions, condTypeBackup)
		b := &zaentrumv1alpha1.BackupStatus{}
		if z.Status.Backup != nil {
			b.Restore, b.Request = z.Status.Backup.Restore, z.Status.Backup.Request
		}
		if request := backupRequest(z); request != "" {
			r.answerBackup(ctx, z, b, request, "backups: the backup asked for is refused: the databases are external")
		}
		z.Status.Backup = b
		if b.Restore == nil && b.Request == "" {
			z.Status.Backup = nil
		}
		return
	}
	on := rendered(platform, "CronJob", templates.BackupCronJobName)
	if !on {
		r.removeBackupSchedule(ctx, z)
	}

	jobs, err := r.backupJobs(ctx, z)
	if err != nil {
		log.FromContext(ctx).Info("backups: the runs could not be listed; status stays as it is", "error", err.Error())
		return
	}
	unread := func(job *batchv1.Job, err error) {
		log.FromContext(ctx).Info("backups: a run could not be read; status stays as it is", "job", job.Name, "error", err.Error())
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
	running := runningBackup(jobs)
	for i := range jobs {
		job := &jobs[i]
		done, failed := jobFinished(job)
		if !done {
			continue
		}
		end := metav1.NewTime(finishedAt(job))
		if failed == nil {
			if b.LastSuccess == nil || end.After(b.LastSuccess.Time) {
				rep, ok, err := r.backupSummary(ctx, job)
				if err != nil {
					unread(job, err)
					return
				}
				b.LastSuccess = &end
				if ok {
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
	b.Running = running

	schedule := z.Spec.Backup.Schedule
	if schedule == "" {
		schedule = "@daily"
	}
	var status metav1.ConditionStatus
	var reason, msg string
	switch {
	case !on:
	case latest == nil:
		status, reason = metav1.ConditionUnknown, "Scheduled"
		msg = "no backup has run yet: the first runs on the schedule " + schedule
		if running != "" {
			msg = running + " runs the first backup"
		}
	case latestFailed:
		why, err := r.backupFailure(ctx, latest)
		if err != nil {
			unread(latest, err)
			return
		}
		status, reason = metav1.ConditionFalse, "Failed"
		msg = fmt.Sprintf("%s failed: %s; ", latest.Name, why)
		if b.LastSuccess != nil && b.LastDump != "" {
			msg += fmt.Sprintf("the latest that succeeded wrote %s, %s", b.LastDump, b.LastSuccess.UTC().Format("2006-01-02 15:04 MST"))
		} else {
			msg += "no backup has succeeded yet"
		}
	default:
		rep, ok, err := r.backupSummary(ctx, latest)
		if err != nil {
			unread(latest, err)
			return
		}
		status, reason = metav1.ConditionTrue, "Succeeded"
		msg = fmt.Sprintf("%s wrote %s", latest.Name, b.LastDump)
		if ok {
			msg = fmt.Sprintf("%s: %d databases, %s; %d dumps kept, %s free", rep.Dump, len(rep.Databases),
				bytesOf(rep.Bytes), len(rep.Kept), bytesOf(rep.Free))
		}
	}

	// Every run that ended has been read.
	r.answerBackupRequest(ctx, z, b, jobs, on)
	if on || z.Status.Backup != nil || !reflect.DeepEqual(*b, zaentrumv1alpha1.BackupStatus{}) {
		z.Status.Backup = b
	}
	if !on {
		status, reason = metav1.ConditionFalse, "Disabled"
		msg = "the bundled Postgres keeps its data on an emptyDir and is not backed up: backups start once it is on " +
			"its claim (spec.storage.postgres.migrate), or with spec.backup.enabled true"
		refused := "; the backup asked for is not made"
		if e := z.Spec.Backup.Enabled; e != nil && !*e {
			msg = "the bundled Postgres is not backed up: spec.backup.enabled is false"
			refused = "; the backup asked for is not made: set spec.backup.enabled to true and ask again with a new value"
		}
		if backupRefusalStands(z, b, jobs) {
			msg += refused
		}
	}
	setCondition(z, condTypeBackup, status, reason, clip(msg, maxVerifyMessage))
	r.keepNewestAskedFor(ctx, z, jobs)
}

// answerBackupRequest writes into b the answer to the backup asked for, if it
// has one now, once every run that ended has been read into b: the Job the
// request started has ended; the Job this operator started for it is gone
// before it was read, which answers it as not read, the Backup condition
// keeping what it said; or backups are off (on is false) and the request
// started none, so it is refused. A request that waits, or whose Job runs,
// stays unanswered. jobs are the backups' Jobs, newest first.
func (r *ZaentrumReconciler) answerBackupRequest(ctx context.Context, z *zaentrumv1alpha1.Zaentrum, b *zaentrumv1alpha1.BackupStatus,
	jobs []batchv1.Job, on bool) {
	a := r.asked(z)
	if a.job != "" {
		for i := range jobs {
			if done, _ := jobFinished(&jobs[i]); done && jobs[i].Name == a.job {
				// Read: no longer one that could be gone unread.
				r.remember(z, func(a *askedBackup) { a.request, a.job = "", "" })
				break
			}
		}
	}
	request := backupRequest(z)
	if request == "" {
		return
	}
	switch job := askedJob(z, jobs, request); {
	case job != nil:
		done, failed := jobFinished(job)
		if !done {
			return
		}
		result := "succeeded"
		if failed != nil {
			result = "failed"
		}
		r.answerBackup(ctx, z, b, request, "backups: the backup asked for has ended", "job", job.Name, "result", result)
	case a.request == request:
		r.answerBackup(ctx, z, b, request, "backups: the backup asked for is answered as not read: its Job is gone before it was read",
			"job", a.job)
	case a.answered == request:
		// Answered before; the status the pass read did not keep it.
		r.answerBackup(ctx, z, b, request, "")
	case !on:
		r.answerBackup(ctx, z, b, request, "backups: the backup asked for is refused: backups are off")
	}
}

// backupRefusalStands says whether the Backup condition, while backups are
// off, says that the backup asked for is not made: the annotation holds the
// value answered, and no backup Job of the platform's carries that value.
func backupRefusalStands(z *zaentrumv1alpha1.Zaentrum, b *zaentrumv1alpha1.BackupStatus, jobs []batchv1.Job) bool {
	request := askedValue(z)
	return request != "" && request == b.Request && askedJob(z, jobs, request) == nil
}

// rendered says whether the render carries an object of that kind and name.
func rendered(objs []*unstructured.Unstructured, kind, name string) bool {
	return renderedObject(objs, kind, name) != nil
}

// removeBackupSchedule deletes the backup CronJob this Zaentrum owns, its Jobs
// with it: backups were turned off. The prune removes it as well once it
// carries the operator's label; this removes it whether or not it does. The
// claim with the dumps stays, and so does the Job of a backup asked for, which
// the Zaentrum owns.
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
// last pod left, or "" where none did; an error where its pods could not be
// listed.
func (r *ZaentrumReconciler) backupMessage(ctx context.Context, job *batchv1.Job) (string, error) {
	pods, err := r.runPods(ctx, job)
	if err != nil {
		return "", err
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
	return msg, nil
}

// backupSummary is a successful run's summary, when it left a readable one;
// an error where its pods could not be listed.
func (r *ZaentrumReconciler) backupSummary(ctx context.Context, job *batchv1.Job) (backupReport, bool, error) {
	msg, err := r.backupMessage(ctx, job)
	if err != nil {
		return backupReport{}, false, err
	}
	var rep backupReport
	if err := json.Unmarshal([]byte(msg), &rep); err != nil || rep.V != 1 || rep.Dump == "" {
		return backupReport{}, false, nil
	}
	return rep, true, nil
}

// backupFailure is why a failed run failed: its own message, else the Job's;
// an error where its pods could not be listed.
func (r *ZaentrumReconciler) backupFailure(ctx context.Context, job *batchv1.Job) (string, error) {
	msg, err := r.backupMessage(ctx, job)
	if err != nil {
		return "", err
	}
	if msg != "" {
		return msg, nil
	}
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == "True" {
			return strings.TrimSuffix(c.Reason+": "+c.Message, ": "), nil
		}
	}
	return "no reason given", nil
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
