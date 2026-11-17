package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
	"github.com/zaentrum/zaentrum-operator/operator/internal/templates"
)

// newUpdatePlatform is a platform that runs v0.4.0, pinned, after one pass.
func newUpdatePlatform(t *testing.T, edit func(*zaentrumv1alpha1.Zaentrum), objs ...client.Object) *platformEnv {
	t.Helper()
	z := verifyCR()
	z.Spec.Version = "v0.4.0"
	if edit != nil {
		edit(z)
	}
	e := newRequestPlatform(t, z, objs...)
	e.pass()
	require.Equal(t, "v0.4.0", e.zaentrum().Status.CurrentVersion)
	return e
}

// updateTo asks for another version, as applying an update pins it.
func (e *platformEnv) updateTo(version string) {
	e.t.Helper()
	e.change(func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.Version = version })
}

// runs is the version the platform's parts run, by chino-api's image.
func (e *platformEnv) runs() string {
	e.t.Helper()
	var dep appsv1.Deployment
	require.NoError(e.t, e.c.Get(context.Background(), types.NamespacedName{Namespace: e.key.Namespace, Name: "chino-api"}, &dep))
	image := dep.Spec.Template.Spec.Containers[0].Image
	return image[strings.LastIndex(image, ":")+1:]
}

// beforeUpdate lists the backups' Jobs marked as made before an update.
func (e *platformEnv) beforeUpdate() []batchv1.Job {
	e.t.Helper()
	var out []batchv1.Job
	for _, j := range e.backupJobs() {
		if j.Annotations[annotationBackupBeforeUpdate] != "" {
			out = append(out, j)
		}
	}
	return out
}

// job reads a Job back.
func (e *platformEnv) job(name string) batchv1.Job {
	e.t.Helper()
	var job batchv1.Job
	require.NoError(e.t, e.c.Get(context.Background(), types.NamespacedName{Namespace: e.key.Namespace, Name: name}, &job))
	return job
}

// at is a time as the Backup condition writes it.
func at(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 MST") }

const goodSet = "2026-10-04T00-00-05Z: 4 databases, 12.0 MiB; 2 dumps kept, 4.0 GiB free"

// R.C12: another version waits for a backup of the version that runs. The
// pass that first holds it back renders the version that runs, names the one
// that waits in status.availableUpdate and suspends the CronJob; the next
// starts the backup, marked with the version it backs up and when the update
// was first held back, and the Backup condition keeps its state, its sentence
// ending with what the update waits for. Once the backup has ended the update
// is applied, the condition says the backup was made before it, and the
// schedule moves again.
func TestRC12AnUpdateWaitsForTheBackupBeforeIt(t *testing.T) {
	e := newUpdatePlatform(t, nil)
	e.scheduledRun("zaentrum-backup-29324160")
	e.endBackup("zaentrum-backup-29324160", 0, okSummary)
	e.pass()
	require.Equal(t, goodSet, e.backupCondition().Message)

	e.updateTo("v0.5.0")
	out, _ := e.pass()
	first := e.clock
	z := e.zaentrum()
	assert.Equal(t, "v0.4.0", z.Status.CurrentVersion, "the version that runs keeps running")
	assert.Equal(t, "v0.4.0", e.runs())
	assert.Equal(t, "v0.5.0", z.Status.AvailableUpdate, "the version that waits")
	assert.True(t, e.suspended(), "the schedule stands still")
	assert.Empty(t, e.beforeUpdate(), "the CronJob was not suspended yet")
	assert.NotContains(t, out, "update: backing up")
	assert.True(t, logged(out, "update: v0.5.0 waits for a backup of the version that runs", "running", "v0.4.0"), out)
	assert.True(t, logged(out, "update: the backup before v0.5.0 waits for the schedule to stand still", "running", "v0.4.0"), out)
	held := e.cron().Annotations
	assert.Equal(t, "v0.4.0", held[annotationBackupBeforeUpdate], "the CronJob keeps the count")
	assert.Equal(t, stamp(first), held[annotationBackupBeforeUpdateSince])

	out, res := e.pass()
	jobs := e.beforeUpdate()
	require.Len(t, jobs, 1)
	job := jobs[0]
	assert.True(t, metav1.IsControlledBy(&job, e.zaentrum()))
	assert.Equal(t, databaseBackup, job.Labels[labelDatabase])
	assert.Equal(t, "v0.4.0", job.Annotations[annotationBackupBeforeUpdate])
	assert.Equal(t, first.UTC().Format(time.RFC3339), job.Annotations[annotationBackupBeforeUpdateSince], "counted from the first hold")
	assert.Empty(t, job.Annotations[zaentrumv1alpha1.BackupRequestAnnotation], "no request waits")
	assert.True(t, logged(out, "update: backing up before applying v0.5.0", "job", job.Name, "running", "v0.4.0"), out)
	assert.NotContains(t, out, "waits for a backup of the version that runs", "the count goes on")
	assert.Equal(t, verifyRequeueAfter, res.RequeueAfter)
	assert.Equal(t, job.Name, e.backup().Running)
	c := e.backupCondition()
	assert.Equal(t, metav1.ConditionTrue, c.Status, "the condition keeps its state")
	assert.Equal(t, "Succeeded", c.Reason)
	assert.Equal(t, goodSet+"; "+job.Name+" backs up v0.4.0 before the update to v0.5.0: the update is applied once it has ended; "+
		"a backup that still runs at "+at(first.Add(30*time.Minute))+" is stopped", c.Message)
	e.pass()
	assert.Equal(t, "v0.4.0", e.runs(), "it waits while the backup runs")

	e.endBackup(job.Name, 0, okSummary)
	out, _ = e.pass()
	z = e.zaentrum()
	assert.Equal(t, "v0.5.0", z.Status.CurrentVersion, "applied once the backup has ended")
	assert.Equal(t, "v0.5.0", e.runs())
	assert.Empty(t, z.Status.AvailableUpdate)
	assert.True(t, logged(out, "update: the backup before v0.5.0 has ended; applying it", "job", job.Name), out)
	assert.Equal(t, goodSet+"; made before the update to v0.5.0", e.backupCondition().Message)
	assert.Equal(t, "v0.5.0", e.job(job.Name).Annotations[annotationBackupBeforeUpdateTo], "the version it was applied to")
	e.pass()
	assert.False(t, e.suspended(), "the schedule moves again")
	assert.Empty(t, e.cron().Annotations[annotationBackupBeforeUpdate], "the version that runs moved: the count goes")
	assert.Len(t, e.beforeUpdate(), 1, "nothing more starts, and the backup is kept")
	assert.Equal(t, goodSet+"; made before the update to v0.5.0", e.backupCondition().Message)
}

// R.C12: a backup before an update that fails does not stop the update, and
// the Backup condition says that it failed.
func TestRC12AFailedBackupDoesNotStopTheUpdate(t *testing.T) {
	e := newUpdatePlatform(t, nil)
	e.updateTo("v0.5.0")
	e.pass()
	e.pass()
	jobs := e.beforeUpdate()
	require.Len(t, jobs, 1)
	e.endBackup(jobs[0].Name, 1, "cannot dump the database katalog")
	out, _ := e.pass()
	assert.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion)
	assert.True(t, logged(out, "update: the backup before v0.5.0 failed; applying it", "job", jobs[0].Name), out)
	c := e.backupCondition()
	assert.Equal(t, "Failed", c.Reason)
	assert.Equal(t, jobs[0].Name+" failed: cannot dump the database katalog; the update to v0.5.0 was applied all the same; "+
		"no backup has succeeded yet", c.Message)
}

// R.C12: a backup that still runs 30 minutes after the update was first held
// back is stopped: its deadline is cut to one second, so that the Job
// controller ends it and it stays listed as one that failed; the update is
// applied once it has ended and its pod has stopped, or 5 minutes after the
// stop at the latest, and the Backup condition says so.
func TestRC12ABackupStillRunningAfter30MinutesIsStopped(t *testing.T) {
	start := func(t *testing.T) (*platformEnv, batchv1.Job, time.Time) {
		e := newUpdatePlatform(t, nil)
		e.scheduledRun("zaentrum-backup-29324160")
		e.endBackup("zaentrum-backup-29324160", 0, okSummary)
		e.pass()
		e.updateTo("v0.5.0")
		e.pass()
		first := e.clock
		e.pass()
		jobs := e.beforeUpdate()
		require.Len(t, jobs, 1)
		e.clock = first.Add(30*time.Minute - 20*time.Second)
		out, _ := e.pass()
		require.NotContains(t, out, "30 minutes", "at 30 minutes, not before")
		job := e.job(jobs[0].Name)
		require.Equal(t, int64(10800), *job.Spec.ActiveDeadlineSeconds, "the schedule's own deadline")
		out, _ = e.pass()
		assert.True(t, logged(out, "update: the backup before v0.5.0 has not ended in 30 minutes; stopped it and applying", "job", job.Name), out)
		job = e.job(job.Name)
		require.NotNil(t, job.Spec.ActiveDeadlineSeconds)
		assert.Equal(t, int64(1), *job.Spec.ActiveDeadlineSeconds, "the Job controller ends it")
		assert.Equal(t, e.clock.UTC().Format(time.RFC3339), job.Annotations[annotationBackupBeforeUpdateStopped])
		assert.Equal(t, "v0.4.0", e.zaentrum().Status.CurrentVersion, "applied once it has ended")
		return e, job, first
	}
	t.Run("it ends", func(t *testing.T) {
		e, job, first := start(t)
		e.pass()
		assert.Equal(t, "v0.4.0", e.zaentrum().Status.CurrentVersion)
		e.endBackup(job.Name, 1, "")
		out, _ := e.pass()
		assert.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion)
		assert.True(t, logged(out, "update: the stopped backup before v0.5.0 has ended; applying it", "job", job.Name), out)
		c := e.backupCondition()
		assert.Equal(t, "Failed", c.Reason, "a run that failed")
		assert.Equal(t, job.Name+" had not ended after 30 minutes and was stopped; the update to v0.5.0 was applied without a backup; "+
			"the latest that succeeded wrote 2026-10-04T00-00-05Z, "+at(first.Add(-20*time.Second)), c.Message)
	})
	t.Run("it finishes its set in its grace period, the update taken back meanwhile", func(t *testing.T) {
		e, job, first := start(t)
		e.updateTo("v0.4.0")
		// The Job controller fails it at its deadline; its pod finishes the set
		// in its grace period and leaves the run's summary.
		job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
			LastTransitionTime: metav1.NewTime(e.clock), Reason: "DeadlineExceeded"}}
		require.NoError(t, e.c.Status().Update(context.Background(), &job))
		require.NoError(t, e.c.Create(context.Background(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-x",
			Namespace: e.key.Namespace, Labels: map[string]string{batchv1.JobNameLabel: job.Name}},
			Status: corev1.PodStatus{Phase: corev1.PodSucceeded, ContainerStatuses: []corev1.ContainerStatus{{Name: templates.BackupContainer,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: okSummary, FinishedAt: metav1.NewTime(e.clock)}}}}}}))
		e.pass()
		assert.Equal(t, "v0.4.0", e.zaentrum().Status.CurrentVersion)
		c := e.backupCondition()
		assert.Equal(t, "Failed", c.Reason)
		assert.Equal(t, job.Name+" failed: stopped; the latest that succeeded wrote 2026-10-04T00-00-05Z, "+at(first.Add(-20*time.Second)),
			c.Message, "the summary of the set it finished is no reason")
	})
	t.Run("it does not end", func(t *testing.T) {
		e, job, _ := start(t)
		e.clock = e.clock.Add(5*time.Minute - 20*time.Second)
		e.pass()
		assert.Equal(t, "v0.4.0", e.zaentrum().Status.CurrentVersion)
		out, _ := e.pass()
		assert.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion, "5 minutes after the stop at the latest")
		assert.True(t, logged(out, "update: the backup before v0.5.0 has not ended in 35 minutes; applying it", "job", job.Name), out)
	})
}

// R.C12: a backup that already runs when the update is first held back is the
// backup before it, marked so; a request that waits behind it gets its own Job
// after the update.
func TestRC12ABackupThatRunsIsTheBackupBeforeTheUpdate(t *testing.T) {
	e := newUpdatePlatform(t, nil)
	e.scheduledRun("zaentrum-backup-29324160")
	e.askBackup("one")
	e.updateTo("v0.5.0")
	out, _ := e.pass()
	first := e.clock
	job := e.job("zaentrum-backup-29324160")
	assert.Equal(t, "v0.4.0", job.Annotations[annotationBackupBeforeUpdate], "marked")
	assert.Equal(t, first.UTC().Format(time.RFC3339), job.Annotations[annotationBackupBeforeUpdateSince])
	assert.True(t, logged(out, "update: backing up before applying v0.5.0", "job", job.Name), out)
	assert.True(t, logged(out, "backups: the backup asked for waits", "request", "one", "for", "the update to v0.5.0 and the backup before it"), out)
	e.pass()
	assert.Len(t, e.backupJobs(), 1, "no second backup")
	assert.Empty(t, e.carrying("one"), "the request waits")

	e.endBackup(job.Name, 0, okSummary)
	_, res := e.pass()
	assert.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion)
	assert.Empty(t, e.carrying("one"), "not on the pass that applies the update")
	assert.Equal(t, verifyRequeueAfter, res.RequeueAfter, "the request that waits brings the next pass soon")
	assert.Empty(t, e.backup().Request, "a run that began before the request does not answer it")
	e.pass()
	e.pass()
	asked := e.carrying("one")
	require.Len(t, asked, 1, "its own Job after the update")
	assert.Empty(t, asked[0].Annotations[annotationBackupBeforeUpdate])
}

// R.C12: the backup the operator starts before an update also answers a
// request that waits.
func TestRC12TheBackupBeforeAnUpdateAnswersARequestThatWaits(t *testing.T) {
	e := newUpdatePlatform(t, nil)
	e.askBackup("one")
	e.updateTo("v0.5.0")
	e.pass()
	out, _ := e.pass()
	jobs := e.beforeUpdate()
	require.Len(t, jobs, 1)
	assert.Equal(t, "one", jobs[0].Annotations[zaentrumv1alpha1.BackupRequestAnnotation])
	assert.True(t, logged(out, "update: backing up before applying v0.5.0", "job", jobs[0].Name, "request", "one"), out)
	assert.Len(t, e.backupJobs(), 1)
	e.endBackup(jobs[0].Name, 0, okSummary)
	e.pass()
	assert.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion)
	assert.Equal(t, "one", e.backup().Request)
	e.pass()
	e.pass()
	assert.Len(t, e.backupJobs(), 1, "one backup for both")
}

// R.C12: a target that moves while the update waits does not start the 30
// minutes again; the version applied is the one that waits then.
func TestRC12TheTargetMovingDoesNotStartThe30MinutesAgain(t *testing.T) {
	e := newUpdatePlatform(t, nil)
	e.updateTo("v0.5.0")
	e.pass()
	first := e.clock
	e.pass()
	jobs := e.beforeUpdate()
	require.Len(t, jobs, 1)
	e.updateTo("v0.6.0")
	e.pass()
	assert.Equal(t, "v0.6.0", e.zaentrum().Status.AvailableUpdate)
	assert.Equal(t, jobs[0].Annotations, e.job(jobs[0].Name).Annotations, "the same backup, the same time")
	assert.Len(t, e.beforeUpdate(), 1)
	e.clock = first.Add(30 * time.Minute)
	out, _ := e.pass()
	assert.True(t, logged(out, "update: the backup before v0.6.0 has not ended in 30 minutes; stopped it and applying"), out)
	e.endBackup(jobs[0].Name, 1, "")
	e.pass()
	assert.Equal(t, "v0.6.0", e.zaentrum().Status.CurrentVersion)
}

// R.C12: an operator that starts again counts the 30 minutes on from the Job.
func TestRC12ARestartedOperatorCountsOn(t *testing.T) {
	e := newUpdatePlatform(t, nil)
	e.updateTo("v0.5.0")
	e.pass()
	first := e.clock
	e.pass()
	jobs := e.beforeUpdate()
	require.Len(t, jobs, 1)
	e.r = &ZaentrumReconciler{Client: e.c, Scheme: e.r.Scheme, Now: e.r.Now}
	e.clock = first.Add(30 * time.Minute)
	e.pass()
	job := e.job(jobs[0].Name)
	assert.Equal(t, int64(1), *job.Spec.ActiveDeadlineSeconds, "stopped at 30 minutes from the first hold")
	assert.Len(t, e.beforeUpdate(), 1, "no second backup")
}

// R.C12: during a restore the update waits, with no backup and no count; once
// the restore has ended the 30 minutes start, with a backup first.
func TestRC12DuringARestoreTheUpdateWaits(t *testing.T) {
	e := newUpdatePlatform(t, nil)
	e.listOnClaim(aDump)
	e.change(func(z *zaentrumv1alpha1.Zaentrum) {
		z.Spec.Version = "v0.5.0"
		z.Annotations = map[string]string{zaentrumv1alpha1.RestoreRequestAnnotation: aDump}
	})
	e.pass()
	require.Equal(t, zaentrumv1alpha1.RestoreRunning, e.zaentrum().Status.Backup.Restore.Result)
	e.clock = e.clock.Add(40 * time.Minute)
	e.pass()
	assert.Equal(t, "v0.4.0", e.zaentrum().Status.CurrentVersion, "it waits for the restore")
	assert.Equal(t, "v0.5.0", e.zaentrum().Status.AvailableUpdate)
	assert.Empty(t, e.beforeUpdate(), "no backup while a restore runs")
	e.endRestoreJob()
	e.pass()
	require.Equal(t, zaentrumv1alpha1.RestoreSucceeded, e.zaentrum().Status.Backup.Restore.Result)
	assert.Equal(t, "v0.4.0", e.zaentrum().Status.CurrentVersion, "the pass that reads its end still restores")
	assert.Empty(t, e.beforeUpdate())
	e.pass()
	start := e.clock
	jobs := e.beforeUpdate()
	require.Len(t, jobs, 1, "a backup first")
	assert.Equal(t, start.UTC().Format(time.RFC3339), jobs[0].Annotations[annotationBackupBeforeUpdateSince], "the 30 minutes start then")
	assert.Empty(t, e.job(jobs[0].Name).Annotations[annotationBackupBeforeUpdateStopped], "not stopped: its 30 minutes have only begun")
	assert.Equal(t, "v0.4.0", e.zaentrum().Status.CurrentVersion)
}

// R.C12: no backup before an update where the tag does not change, where it is
// a new install's first version, where the platform makes no backups, and on
// the all-in-one image.
func TestRC12WhereNoBackupIsMadeBeforeAnUpdate(t *testing.T) {
	applied := func(t *testing.T, e *platformEnv) {
		t.Helper()
		e.updateTo("v0.5.0")
		e.pass()
		assert.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion, "applied at once")
		assert.Equal(t, "v0.5.0", e.runs())
		assert.Empty(t, e.beforeUpdate())
	}
	t.Run("backups are off", func(t *testing.T) {
		no := false
		applied(t, newUpdatePlatform(t, func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.Backup.Enabled = &no }))
	})
	t.Run("external databases", func(t *testing.T) {
		applied(t, newUpdatePlatform(t, func(z *zaentrumv1alpha1.Zaentrum) {
			z.Spec.Databases.Mode = "external"
			z.Spec.Databases.External.Host = "postgres.example.com"
			z.Spec.Databases.External.SSLMode = "require"
		}))
	})
	t.Run("the all-in-one image", func(t *testing.T) {
		selfEnv(t)
		t.Setenv(envInstallSource, string(zaentrumv1alpha1.InstallSourceAppliance))
		e := newUpdatePlatform(t, nil, managerPod("ghcr.io/zaentrum/operator:v0.4.0"), managerDeployment())
		require.Equal(t, zaentrumv1alpha1.InstallSourceAppliance, e.zaentrum().Status.Controller.Source)
		applied(t, e)
	})
	t.Run("a new install's first version", func(t *testing.T) {
		z := verifyCR()
		z.Spec.Version = "v0.5.0"
		e := newRequestPlatform(t, z)
		e.pass()
		assert.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion)
		assert.Empty(t, e.beforeUpdate())
	})
	t.Run("the same tag", func(t *testing.T) {
		e := newUpdatePlatform(t, nil)
		e.pass()
		e.pass()
		assert.Empty(t, e.beforeUpdate())
		assert.Empty(t, e.zaentrum().Status.AvailableUpdate)
	})
}

// R.C12: where no backup can be started in the 30 minutes, the update is
// applied then.
func TestRC12AnUpdateWaits30MinutesAtMostForABackupThatCannotStart(t *testing.T) {
	var e *platformEnv
	e = newRequestPlatform(t, func() *zaentrumv1alpha1.Zaentrum { z := verifyCR(); z.Spec.Version = "v0.4.0"; return z }())
	e.c = interceptor.NewClient(e.c, interceptor.Funcs{
		Patch: applyAsCreateOrUpdate,
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if u, ok := obj.(*unstructured.Unstructured); ok && u.GetKind() == "Job" && u.GetAnnotations()[annotationBackupBeforeUpdate] != "" {
				return fmt.Errorf("exceeded quota")
			}
			return c.Create(ctx, obj, opts...)
		},
	})
	e.r = &ZaentrumReconciler{Client: e.c, Scheme: e.r.Scheme, Now: e.r.Now}
	e.pass()
	e.updateTo("v0.5.0")
	e.pass()
	first := e.clock
	e.pass()
	assert.Equal(t, "v0.4.0", e.zaentrum().Status.CurrentVersion)
	e.clock = first.Add(30 * time.Minute)
	out, _ := e.pass()
	assert.True(t, logged(out, "update: no backup before v0.5.0 could be started in 30 minutes; applying it without one"), out)
	assert.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion)
}

// R.C12: in auto mode, where the channel's release would be applied at once,
// status.availableUpdate names it while the update waits for its backup, and
// the version that runs keeps running.
func TestRC12InAutoModeTheVersionThatWaitsIsNamed(t *testing.T) {
	z := verifyCR()
	z.Spec.Update.Mode = zaentrumv1alpha1.UpdateAuto
	z.Status.CurrentVersion = "v0.3.0"
	e := newRequestPlatform(t, z)
	e.pass()
	got := e.zaentrum()
	assert.Equal(t, "v0.3.0", got.Status.CurrentVersion)
	assert.Equal(t, "v0.4.0", got.Status.AvailableUpdate, "the stable channel's release")
	assert.Equal(t, "v0.3.0", e.runs())
	assert.True(t, e.suspended())
}

// tryPass takes one pass, ten seconds after the last, and gives what it logged
// and its error.
func (e *platformEnv) tryPass() (string, error) {
	e.t.Helper()
	e.clock = e.clock.Add(10 * time.Second)
	var buf bytes.Buffer
	ctx := log.IntoContext(context.Background(), zap.New(zap.WriteTo(&buf)))
	_, err := e.r.Reconcile(ctx, ctrl.Request{NamespacedName: e.key})
	return buf.String(), err
}

// passAt takes one pass at t, and gives what it logged.
func (e *platformEnv) passAt(t time.Time) string {
	e.t.Helper()
	e.clock = t.Add(-10 * time.Second)
	out, _ := e.pass()
	return out
}

// restart is the operator starting again: a reconciler of its own, which
// remembers nothing, over the same API server.
func (e *platformEnv) restart() {
	e.r = &ZaentrumReconciler{Client: e.c, Scheme: e.r.Scheme, Now: e.r.Now}
}

// refuse has the API server refuse, from now on, what fault returns an error
// for: verb is create, patch (an apply among them) or list, obj the object or
// the list, opts a list's options. The operator remembers what it did.
func (e *platformEnv) refuse(fault func(verb string, obj any, opts *client.ListOptions) error) {
	e.c = interceptor.NewClient(e.c, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if err := fault("create", obj, nil); err != nil {
				return err
			}
			return c.Create(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if err := fault("patch", obj, nil); err != nil {
				return err
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := fault("list", list, (&client.ListOptions{}).ApplyOptions(opts)); err != nil {
				return err
			}
			return c.List(ctx, list, opts...)
		},
	})
	e.r.Client = e.c
}

// refusedBackupJobs refuses to create a backup before an update.
func refusedBackupJobs(verb string, obj any, _ *client.ListOptions) error {
	if o, ok := obj.(client.Object); ok && verb == "create" && o.GetAnnotations()[annotationBackupBeforeUpdate] != "" {
		return errors.New("exceeded quota")
	}
	return nil
}

// heldBackUpdate asks for v0.5.0 and takes the passes that hold it back and
// start its backup; it gives that backup's name and when the update was first
// held back.
func (e *platformEnv) heldBackUpdate() (string, time.Time) {
	e.t.Helper()
	e.updateTo("v0.5.0")
	e.pass()
	first := e.clock
	e.pass()
	jobs := e.beforeUpdate()
	require.Len(e.t, jobs, 1)
	return jobs[0].Name, first
}

// R.C12: whatever holds an update back, it is applied 35 minutes after it was
// first held back at the latest, and the log says why.
func TestRC12AnUpdateIsAppliedBy35MinutesAtTheLatest(t *testing.T) {
	appliedAt35 := func(t *testing.T, e *platformEnv, first time.Time, job string) {
		t.Helper()
		e.passAt(first.Add(35*time.Minute - 10*time.Second))
		assert.Equal(t, "v0.4.0", e.zaentrum().Status.CurrentVersion, "not before")
		out, _ := e.pass()
		assert.True(t, logged(out, "update: the backup before v0.5.0 has not ended in 35 minutes; applying it", "job", job), out)
		assert.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion)
	}
	t.Run("the stop is refused", func(t *testing.T) {
		e := newUpdatePlatform(t, nil)
		e.refuse(func(verb string, obj any, _ *client.ListOptions) error {
			if j, ok := obj.(*batchv1.Job); ok && verb == "patch" && j.Spec.ActiveDeadlineSeconds != nil && *j.Spec.ActiveDeadlineSeconds == 1 {
				return errors.New("admission webhook refused it")
			}
			return nil
		})
		job, first := e.heldBackUpdate()
		out := e.passAt(first.Add(30 * time.Minute))
		assert.True(t, logged(out, "update: the backup before v0.5.0 could not be stopped; trying again", "job", job), out)
		appliedAt35(t, e, first, job)
	})
	t.Run("the mark is refused", func(t *testing.T) {
		e := newUpdatePlatform(t, nil)
		e.scheduledRun("zaentrum-backup-29324160")
		e.refuse(func(verb string, obj any, _ *client.ListOptions) error {
			if j, ok := obj.(*batchv1.Job); ok && verb == "patch" && j.Name == "zaentrum-backup-29324160" {
				return errors.New("admission webhook refused it")
			}
			return nil
		})
		e.updateTo("v0.5.0")
		out, _ := e.pass()
		first := e.clock
		assert.True(t, logged(out, "update: the backup that runs could not be marked as the one before v0.5.0; trying again"), out)
		e.pass()
		assert.Empty(t, e.beforeUpdate())
		assert.Len(t, e.backupJobs(), 1, "none of its own beside the one that runs")
		out = e.passAt(first.Add(30 * time.Minute))
		assert.True(t, logged(out, "update: the backup before v0.5.0 could not be stopped; trying again", "job", "zaentrum-backup-29324160"), out)
		appliedAt35(t, e, first, "zaentrum-backup-29324160")
		run := e.job("zaentrum-backup-29324160")
		assert.Empty(t, run.Annotations[annotationBackupBeforeUpdateTo], "the patch that stops it refused: nothing of the update on it")
		assert.Equal(t, int64(10800), *run.Spec.ActiveDeadlineSeconds)
	})
	t.Run("a restore asked for is refused meanwhile", func(t *testing.T) {
		e := newUpdatePlatform(t, nil)
		job, first := e.heldBackUpdate()
		e.clock = first.Add(10 * time.Minute)
		e.change(func(z *zaentrumv1alpha1.Zaentrum) {
			z.Annotations = map[string]string{zaentrumv1alpha1.RestoreRequestAnnotation: "the newest"}
		})
		e.pass()
		require.Equal(t, zaentrumv1alpha1.RestoreRefused, e.zaentrum().Status.Backup.Restore.Result)
		assert.Equal(t, stamp(first), e.job(job).Annotations[annotationBackupBeforeUpdateSince], "the count goes on")
		out := e.passAt(first.Add(30 * time.Minute))
		assert.True(t, logged(out, "update: the backup before v0.5.0 has not ended in 30 minutes; stopped it and applying", "job", job), out)
		appliedAt35(t, e, first, job)
	})
	t.Run("the resource is refused for 45 minutes", func(t *testing.T) {
		e := newUpdatePlatform(t, nil)
		job, first := e.heldBackUpdate()
		e.change(func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.Databases.Chino = "chi no" })
		e.passAt(first.Add(10 * time.Minute))
		e.passAt(first.Add(44 * time.Minute))
		e.change(func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.Databases.Chino = "chino" })
		out := e.passAt(first.Add(45 * time.Minute))
		assert.True(t, logged(out, "update: the backup before v0.5.0 has not ended in 35 minutes; applying it", "job", job), out)
		assert.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion, "on the first pass after the correction")
		e.stoppedAsApplied(t, out, job)
	})
	t.Run("the operator starts again after 45 minutes", func(t *testing.T) {
		e := newUpdatePlatform(t, nil)
		job, first := e.heldBackUpdate()
		e.restart()
		out := e.passAt(first.Add(45 * time.Minute))
		assert.True(t, logged(out, "update: the backup before v0.5.0 has not ended in 35 minutes; applying it", "job", job), out)
		assert.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion)
		assert.Equal(t, stamp(first), e.job(job).Annotations[annotationBackupBeforeUpdateSince], "never written again")
		e.stoppedAsApplied(t, out, job)
	})
	t.Run("the backups' Jobs cannot be listed", func(t *testing.T) {
		e := newUpdatePlatform(t, nil)
		e.refuse(func(verb string, obj any, opts *client.ListOptions) error {
			if _, ok := obj.(*batchv1.JobList); ok && verb == "list" && opts.LabelSelector != nil &&
				opts.LabelSelector.Matches(labels.Set{labelDatabase: databaseBackup}) {
				return errors.New("the server is currently unable to handle the request")
			}
			return nil
		})
		e.updateTo("v0.5.0")
		out, _ := e.pass()
		first := e.clock
		assert.True(t, logged(out, "update: v0.5.0 waits for a backup of the version that runs"), out)
		assert.True(t, logged(out, "update: the backups' Jobs could not be listed; the update waits", "target", "v0.5.0"), out)
		assert.Equal(t, stamp(first), e.cron().Annotations[annotationBackupBeforeUpdateSince], "counted all the same")
		e.passAt(first.Add(35*time.Minute - 10*time.Second))
		assert.Equal(t, "v0.4.0", e.zaentrum().Status.CurrentVersion)
		out, _ = e.pass()
		assert.True(t, logged(out, "update: the backups' Jobs could not be listed in 35 minutes; applying v0.5.0 without a backup"), out)
		assert.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion)
	})
	t.Run("the operator starts again every 20 minutes and no backup can start", func(t *testing.T) {
		e := newUpdatePlatform(t, nil)
		e.refuse(refusedBackupJobs)
		e.updateTo("v0.5.0")
		e.pass()
		first := e.clock
		e.pass()
		e.restart()
		out := e.passAt(first.Add(20 * time.Minute))
		assert.NotContains(t, out, "waits for a backup of the version that runs", "the CronJob keeps the count")
		assert.Equal(t, "v0.4.0", e.zaentrum().Status.CurrentVersion)
		e.restart()
		out = e.passAt(first.Add(30 * time.Minute))
		assert.True(t, logged(out, "update: no backup before v0.5.0 could be started in 30 minutes; applying it without one"), out)
		assert.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion)
	})
	t.Run("a backup that is being deleted has not ended", func(t *testing.T) {
		ctx := context.Background()
		e := newUpdatePlatform(t, nil)
		e.scheduledRun("zaentrum-backup-29324160")
		run := e.job("zaentrum-backup-29324160")
		run.Finalizers = []string{"example.org/hold"}
		require.NoError(t, e.c.Update(ctx, &run))
		require.NoError(t, e.c.Delete(ctx, &run))
		e.updateTo("v0.5.0")
		out, _ := e.pass()
		first := e.clock
		assert.True(t, logged(out, "update: a backup that is being deleted has not ended; the update to v0.5.0 waits for it",
			"job", run.Name), out)
		e.pass()
		e.pass()
		assert.Len(t, e.backupJobs(), 1, "none of its own beside it: it may still write")
		assert.Equal(t, "v0.4.0", e.zaentrum().Status.CurrentVersion)
		appliedAt35(t, e, first, run.Name)
	})
}

// R.C12: an update whose apply fails, after its backup has ended, is applied
// again on every pass, never held back again nor backed up again, however
// long it fails, by an operator that starts again too; nothing goes back to
// the version that ran.
func TestRC12AnUpdateWhoseApplyFailsIsAppliedAgainWithoutASecondBackup(t *testing.T) {
	e := newUpdatePlatform(t, nil)
	var applied []string
	failing := true
	e.refuse(func(verb string, obj any, _ *client.ListOptions) error {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok || verb != "patch" || u.GetKind() != "Deployment" || u.GetName() != "chino-api" {
			return nil
		}
		containers, _, _ := unstructured.NestedSlice(u.Object, "spec", "template", "spec", "containers")
		image := containers[0].(map[string]any)["image"].(string)
		applied = append(applied, image[strings.LastIndex(image, ":")+1:])
		if failing && strings.HasSuffix(image, ":v0.5.0") {
			return errors.New("admission webhook refused it")
		}
		return nil
	})
	job, first := e.heldBackUpdate()
	e.endBackup(job, 0, okSummary)
	applied = nil
	_, err := e.tryPass()
	require.Error(t, err, "the apply of v0.5.0 fails")
	assert.Equal(t, "v0.5.0", e.job(job).Annotations[annotationBackupBeforeUpdateTo], "recorded by the pass that applies it")
	for _, after := range []time.Duration{20 * time.Minute, 41 * time.Minute, 90 * time.Minute} {
		if after == 41*time.Minute {
			e.restart()
		}
		e.clock = first.Add(after - 10*time.Second)
		_, err = e.tryPass()
		require.Error(t, err, "%v: it still fails", after)
		assert.Equal(t, "v0.4.0", e.zaentrum().Status.CurrentVersion)
	}
	assert.NotEmpty(t, applied)
	assert.NotContains(t, applied, "v0.4.0", "nothing goes back to the version that ran")
	assert.Len(t, e.beforeUpdate(), 1, "no second backup")
	failing = false
	e.pass()
	assert.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion)
	assert.Equal(t, "v0.5.0", e.runs())
	assert.Equal(t, goodSet+"; made before the update to v0.5.0", e.backupCondition().Message)
}

// R.C12: the Backup condition names the update a backup was made before, also
// after a later update that had no backup of its own, and says nothing of an
// update once the version it was made of runs again.
func TestRC12TheBackupConditionNamesTheUpdateItsBackupWasMadeBefore(t *testing.T) {
	updated := func(t *testing.T) *platformEnv {
		e := newUpdatePlatform(t, nil)
		job, _ := e.heldBackUpdate()
		e.endBackup(job, 0, okSummary)
		e.pass()
		require.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion)
		e.refuse(refusedBackupJobs)
		return e
	}
	withoutBackup := func(t *testing.T, e *platformEnv, version string) {
		e.updateTo(version)
		e.pass()
		e.passAt(e.clock.Add(30 * time.Minute))
		require.Equal(t, version, e.zaentrum().Status.CurrentVersion)
		e.pass()
	}
	t.Run("a later update", func(t *testing.T) {
		e := updated(t)
		withoutBackup(t, e, "v0.6.0")
		assert.Equal(t, goodSet+"; made before the update to v0.5.0", e.backupCondition().Message)
	})
	t.Run("the version it was made of runs again", func(t *testing.T) {
		e := updated(t)
		withoutBackup(t, e, "v0.4.0")
		assert.Equal(t, goodSet, e.backupCondition().Message)
	})
}

// R.C12: the update's own backup holds the schedule still for as long as it
// has not ended, also once the update was applied without it.
func TestRC12TheUpdatesOwnBackupHoldsTheScheduleUntilItEnds(t *testing.T) {
	e := newUpdatePlatform(t, nil)
	job, first := e.heldBackUpdate()
	e.passAt(first.Add(30 * time.Minute))
	e.passAt(first.Add(35 * time.Minute))
	require.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion)
	for pass := 0; pass < 2; pass++ {
		e.pass()
		assert.True(t, e.suspended(), "pass %d: it may still write", pass)
	}
	e.endBackup(job, 1, "")
	e.pass()
	assert.False(t, e.suspended(), "ended: the schedule moves")
}

// R.C12: of the backups made before updates, the newest that has ended is
// kept and older ones are removed.
func TestRC12OlderBackupsBeforeAnUpdateAreRemoved(t *testing.T) {
	e := newUpdatePlatform(t, nil)
	update := func(version string) string {
		from := e.zaentrum().Status.CurrentVersion
		e.updateTo(version)
		e.pass()
		e.pass()
		var name string
		for _, j := range e.beforeUpdate() {
			if j.Annotations[annotationBackupBeforeUpdate] == from {
				name = j.Name
			}
		}
		require.NotEmpty(t, name)
		e.endBackup(name, 0, okSummary)
		e.pass()
		require.Equal(t, version, e.zaentrum().Status.CurrentVersion)
		return name
	}
	update("v0.5.0")
	second := update("v0.6.0")
	e.pass()
	var names []string
	for _, j := range e.backupJobs() {
		names = append(names, j.Name)
	}
	assert.Equal(t, []string{second}, names, "the newest kept, the one before removed")
}

// withdrawnBackup holds an update to v0.5.0 back, takes it back while its
// backup runs, and ends that backup: a backup before an update that was not
// applied. It gives the backup and when the update was first held back.
func withdrawnBackup(t *testing.T) (*platformEnv, string, time.Time) {
	t.Helper()
	e := newUpdatePlatform(t, nil)
	job, first := e.heldBackUpdate()
	e.updateTo("v0.4.0")
	e.pass()
	e.endBackup(job, 0, okSummary)
	e.pass()
	require.Equal(t, "v0.4.0", e.zaentrum().Status.CurrentVersion)
	require.Empty(t, e.cron().Annotations[annotationBackupBeforeUpdate], "taken back: the CronJob no longer keeps the count")
	require.Equal(t, goodSet, e.backupCondition().Message, "nothing of an update that was not applied")
	return e, job, first
}

// R.C12: a backup made before an update that was taken back counts for that
// update set again within 40 minutes of when it was first held back, and not
// after.
func TestRC12ABackupCountsFor40MinutesForATargetSetAgain(t *testing.T) {
	t.Run("within 40 minutes", func(t *testing.T) {
		e, job, first := withdrawnBackup(t)
		e.updateTo("v0.5.0")
		out := e.passAt(first.Add(39 * time.Minute))
		assert.True(t, logged(out, "update: the backup before v0.5.0 has ended; applying it", "job", job), out)
		assert.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion)
	})
	t.Run("after 40 minutes", func(t *testing.T) {
		e, _, first := withdrawnBackup(t)
		e.updateTo("v0.5.0")
		out := e.passAt(first.Add(41 * time.Minute))
		assert.True(t, logged(out, "update: v0.5.0 waits for a backup of the version that runs"), out)
		e.pass()
		assert.Len(t, e.beforeUpdate(), 2, "a backup of its own")
		assert.Equal(t, "v0.4.0", e.zaentrum().Status.CurrentVersion)
	})
}

// R.C12: a backup made before a restore ended does not count for an update
// held back after it.
func TestRC12ABackupFromBeforeARestoreDoesNotCount(t *testing.T) {
	e, _, _ := withdrawnBackup(t)
	e.listOnClaim(aDump)
	e.change(func(z *zaentrumv1alpha1.Zaentrum) {
		z.Annotations = map[string]string{zaentrumv1alpha1.RestoreRequestAnnotation: aDump}
	})
	e.pass()
	require.Equal(t, zaentrumv1alpha1.RestoreRunning, e.zaentrum().Status.Backup.Restore.Result)
	e.endRestoreJob()
	e.pass()
	require.Equal(t, zaentrumv1alpha1.RestoreSucceeded, e.zaentrum().Status.Backup.Restore.Result)
	e.updateTo("v0.5.0")
	e.pass()
	e.pass()
	assert.Len(t, e.beforeUpdate(), 2, "a backup of its own, after the restore")
	assert.Equal(t, "v0.4.0", e.zaentrum().Status.CurrentVersion)
}

// R.C12: a restore that starts while an update waits starts its count again,
// from the restore's end.
func TestRC12ARestoreStartsTheCountAgain(t *testing.T) {
	e := newUpdatePlatform(t, nil)
	e.refuse(refusedBackupJobs)
	e.updateTo("v0.5.0")
	e.pass()
	first := e.clock
	e.pass()
	e.clock = first.Add(10 * time.Minute)
	e.listOnClaim(aDump)
	e.change(func(z *zaentrumv1alpha1.Zaentrum) {
		z.Annotations = map[string]string{zaentrumv1alpha1.RestoreRequestAnnotation: aDump}
	})
	e.pass()
	require.Equal(t, zaentrumv1alpha1.RestoreRunning, e.zaentrum().Status.Backup.Restore.Result)
	assert.Empty(t, e.cron().Annotations[annotationBackupBeforeUpdate], "no count while the restore runs")
	e.clock = first.Add(25 * time.Minute)
	e.endRestoreJob()
	e.pass()
	require.Equal(t, zaentrumv1alpha1.RestoreSucceeded, e.zaentrum().Status.Backup.Restore.Result)
	out, _ := e.pass()
	start := e.clock
	assert.True(t, logged(out, "update: v0.5.0 waits for a backup of the version that runs"), out)
	assert.Equal(t, stamp(start), e.cron().Annotations[annotationBackupBeforeUpdateSince])
	e.passAt(first.Add(31 * time.Minute))
	assert.Equal(t, "v0.4.0", e.zaentrum().Status.CurrentVersion, "30 minutes from the restore's end, not from the first hold")
	e.passAt(start.Add(30 * time.Minute))
	assert.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion)
}

// R.C12: a backup that was stopped is waited for until its pod has stopped
// too, and the Backup condition no longer says a backup runs once its Job has
// ended.
func TestRC12AStoppedBackupIsWaitedForUntilItsPodHasStopped(t *testing.T) {
	ctx := context.Background()
	e := newUpdatePlatform(t, nil)
	name, first := e.heldBackUpdate()
	e.passAt(first.Add(30 * time.Minute))
	// The Job controller fails it at its deadline; its pod still stops.
	job := e.job(name)
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
		LastTransitionTime: metav1.NewTime(e.clock), Reason: "DeadlineExceeded"}}
	require.NoError(t, e.c.Status().Update(ctx, &job))
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name + "-x", Namespace: e.key.Namespace,
		Labels: map[string]string{batchv1.JobNameLabel: name}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	require.NoError(t, e.c.Create(ctx, pod))
	e.pass()
	assert.Equal(t, "v0.4.0", e.zaentrum().Status.CurrentVersion, "its pod may still write")
	assert.NotContains(t, e.backupCondition().Message, "backs up", "its Job has ended")
	pod.Status = corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{{Name: templates.BackupContainer,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 143, FinishedAt: metav1.NewTime(e.clock)}}}}}
	require.NoError(t, e.c.Status().Update(ctx, pod))
	out, _ := e.pass()
	assert.True(t, logged(out, "update: the stopped backup before v0.5.0 has ended; applying it", "job", name), out)
	assert.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion)
}

// R.C12: a stop counts only where the operator stopped the backup for the
// update it waits for: not a time set by hand on a Job whose deadline was not
// cut, nor one left from an update before.
func TestRC12AStopCountsOnlyWhereTheOperatorStoppedTheBackup(t *testing.T) {
	t.Run("set by hand", func(t *testing.T) {
		e := newUpdatePlatform(t, nil)
		name, first := e.heldBackUpdate()
		job := e.job(name)
		// Even a time after the 30 minutes: the deadline is what stops it.
		job.Annotations[annotationBackupBeforeUpdateStopped] = stamp(first.Add(31 * time.Minute))
		require.NoError(t, e.c.Update(context.Background(), &job))
		e.pass()
		e.pass()
		assert.Equal(t, "v0.4.0", e.zaentrum().Status.CurrentVersion, "its deadline was not cut: no stop")
		out := e.passAt(first.Add(30 * time.Minute))
		assert.True(t, logged(out, "update: the backup before v0.5.0 has not ended in 30 minutes; stopped it and applying", "job", name), out)
	})
	t.Run("a stopped backup that does not end, at the next update", func(t *testing.T) {
		e := newUpdatePlatform(t, nil)
		name, first := e.heldBackUpdate()
		e.passAt(first.Add(30 * time.Minute))
		e.passAt(first.Add(35 * time.Minute))
		require.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion)
		e.clock = first.Add(24 * time.Hour)
		e.updateTo("v0.6.0")
		out, _ := e.pass()
		next := e.clock
		assert.True(t, logged(out, "update: backing up before applying v0.6.0", "job", name), out)
		job := e.job(name)
		assert.Equal(t, "v0.5.0", job.Annotations[annotationBackupBeforeUpdate], "marked for the update from the version that runs")
		assert.Equal(t, stamp(next), job.Annotations[annotationBackupBeforeUpdateSince])
		assert.Empty(t, job.Annotations[annotationBackupBeforeUpdateStopped], "a new mark takes the stop off")
		assert.Empty(t, job.Annotations[annotationBackupBeforeUpdateTo])
		e.pass()
		assert.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion, "it waits for that backup")
		out = e.passAt(next.Add(30 * time.Minute))
		assert.True(t, logged(out, "update: the backup before v0.6.0 has not ended in 30 minutes; stopped it and applying", "job", name), out)
		out = e.passAt(next.Add(35 * time.Minute))
		assert.True(t, logged(out, "update: the backup before v0.6.0 has not ended in 35 minutes; applying it", "job", name), out)
		assert.Equal(t, "v0.6.0", e.zaentrum().Status.CurrentVersion)
	})
}

// R.C12: whether the operator is the one-machine install's goes by the install
// source it last read of itself: a pass that cannot read its own pod changes
// nothing of it, an operator that starts again goes by the one the status
// keeps, and one never read is a cluster install's.
func TestRC12TheOneMachineCheckGoesByTheSourceLastRead(t *testing.T) {
	ctx := context.Background()
	appliance := func(t *testing.T, objs ...client.Object) *platformEnv {
		selfEnv(t)
		t.Setenv(envInstallSource, string(zaentrumv1alpha1.InstallSourceAppliance))
		return newUpdatePlatform(t, nil, objs...)
	}
	t.Run("a pass that cannot read its own pod", func(t *testing.T) {
		e := appliance(t, managerPod("ghcr.io/zaentrum/operator:v0.4.0"), managerDeployment())
		require.Equal(t, zaentrumv1alpha1.InstallSourceAppliance, e.zaentrum().Status.Controller.Source)
		require.NoError(t, e.c.Delete(ctx, managerPod("ghcr.io/zaentrum/operator:v0.4.0")))
		e.pass()
		require.Equal(t, zaentrumv1alpha1.InstallSourceUnknown, e.zaentrum().Status.Controller.Source)
		e.updateTo("v0.5.0")
		e.pass()
		assert.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion, "applied at once")
		assert.Empty(t, e.beforeUpdate())
	})
	t.Run("an operator that starts again", func(t *testing.T) {
		e := appliance(t, managerPod("ghcr.io/zaentrum/operator:v0.4.0"), managerDeployment())
		require.NoError(t, e.c.Delete(ctx, managerPod("ghcr.io/zaentrum/operator:v0.4.0")))
		e.restart()
		e.updateTo("v0.5.0")
		e.pass()
		assert.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion, "the status keeps the source read before")
	})
	t.Run("never read", func(t *testing.T) {
		e := appliance(t)
		require.Equal(t, zaentrumv1alpha1.InstallSourceUnknown, e.zaentrum().Status.Controller.Source)
		e.updateTo("v0.5.0")
		e.pass()
		assert.Equal(t, "v0.4.0", e.zaentrum().Status.CurrentVersion, "a cluster install's: it waits for a backup")
	})
}

// R.C12: a long reason of a failed backup before an update is clipped, never
// what the sentence says after it.
func TestRC12ALongReasonIsClippedNotTheEndOfTheSentence(t *testing.T) {
	e := newUpdatePlatform(t, nil)
	job, _ := e.heldBackUpdate()
	e.endBackup(job, 1, strings.Repeat("cannot dump the database katalog ", 30))
	e.pass()
	msg := e.backupCondition().Message
	assert.True(t, strings.HasSuffix(msg, "; the update to v0.5.0 was applied all the same; no backup has succeeded yet"), msg)
	assert.Contains(t, msg, "…")
	assert.LessOrEqual(t, utf8.RuneCountInString(msg), maxVerifyMessage)
}

// stoppedAsApplied asserts that the pass that applied the update stopped the
// backup that still ran, in the patch that recorded the update on it.
func (e *platformEnv) stoppedAsApplied(t *testing.T, out, name string) {
	t.Helper()
	job := e.job(name)
	assert.Equal(t, int64(1), *job.Spec.ActiveDeadlineSeconds, "stopped")
	assert.Equal(t, stamp(e.clock), job.Annotations[annotationBackupBeforeUpdateStopped])
	assert.Equal(t, "v0.5.0", job.Annotations[annotationBackupBeforeUpdateTo])
	assert.True(t, logged(out, "update: the backup before v0.5.0 still runs after 35 minutes; stopped it", "job", name), out)
}

// R.C12: a backup that still runs when the update is applied at the 35
// minutes is stopped on that pass, in the patch that records the update on
// it; a set it writes after is not one made before the update. Where that
// patch is refused nothing of the update is written on it, and the next pass
// stops it.
func TestRC12ABackupThatStillRunsAtThe35MinutesIsStoppedAsTheUpdateIsApplied(t *testing.T) {
	t.Run("stopped", func(t *testing.T) {
		e := newUpdatePlatform(t, nil)
		job, first := e.heldBackUpdate()
		out := e.passAt(first.Add(45 * time.Minute))
		require.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion)
		e.stoppedAsApplied(t, out, job)
		// It ends well all the same, within its grace period.
		e.endBackup(job, 0, okSummary)
		e.pass()
		assert.Equal(t, goodSet, e.backupCondition().Message, "not made before the update")
	})
	t.Run("the patch is refused", func(t *testing.T) {
		e := newUpdatePlatform(t, nil)
		refusing := true
		e.refuse(func(verb string, obj any, _ *client.ListOptions) error {
			if j, ok := obj.(*batchv1.Job); ok && verb == "patch" && refusing && j.Annotations[annotationBackupBeforeUpdateTo] != "" {
				return errors.New("admission webhook refused it")
			}
			return nil
		})
		job, first := e.heldBackUpdate()
		out := e.passAt(first.Add(45 * time.Minute))
		require.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion, "applied at the 35 minutes all the same")
		assert.True(t, logged(out, "update: the backup before v0.5.0 could not be stopped; trying again", "job", job), out)
		run := e.job(job)
		assert.Empty(t, run.Annotations[annotationBackupBeforeUpdateTo], "nothing of the update on it")
		assert.Empty(t, run.Annotations[annotationBackupBeforeUpdateStopped])
		assert.Equal(t, int64(10800), *run.Spec.ActiveDeadlineSeconds)
		refusing = false
		out, _ = e.pass()
		run = e.job(job)
		assert.Equal(t, int64(1), *run.Spec.ActiveDeadlineSeconds, "stopped on the pass after")
		assert.Equal(t, "v0.5.0", run.Annotations[annotationBackupBeforeUpdateTo])
		assert.True(t, logged(out, "update: the backup before v0.5.0 still runs after 35 minutes; stopped it", "job", job), out)
	})
}

// R.C12: a backup made before an update that was applied does not count for a
// later update from the same version, even within 40 minutes: the version
// moved away and back meanwhile.
func TestRC12ABackupWhoseUpdateWasAppliedDoesNotCountAgain(t *testing.T) {
	e := newUpdatePlatform(t, nil)
	job, first := e.heldBackUpdate()
	e.endBackup(job, 0, okSummary)
	e.pass()
	require.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion)
	refusing := true
	e.refuse(func(verb string, obj any, opts *client.ListOptions) error {
		if refusing {
			return refusedBackupJobs(verb, obj, opts)
		}
		return nil
	})
	e.updateTo("v0.4.0")
	e.pass()
	back := e.clock
	e.passAt(back.Add(30 * time.Minute))
	require.Equal(t, "v0.4.0", e.zaentrum().Status.CurrentVersion, "back, without a backup")
	refusing = false
	e.updateTo("v0.6.0")
	require.Less(t, e.clock.Sub(first), 40*time.Minute)
	out, _ := e.pass()
	assert.True(t, logged(out, "update: v0.6.0 waits for a backup of the version that runs"), out)
	e.pass()
	assert.Len(t, e.beforeUpdate(), 2, "a backup of its own")
	assert.Equal(t, "v0.4.0", e.zaentrum().Status.CurrentVersion)
}

// R.C12: a restore refused at once does not start the count again, also for
// an operator that starts again with nothing but the CronJob to count by.
func TestRC12ARestoreRefusedLeavesTheCount(t *testing.T) {
	e := newUpdatePlatform(t, nil)
	e.refuse(refusedBackupJobs)
	e.updateTo("v0.5.0")
	e.pass()
	first := e.clock
	e.pass()
	e.clock = first.Add(10 * time.Minute)
	e.change(func(z *zaentrumv1alpha1.Zaentrum) {
		z.Annotations = map[string]string{zaentrumv1alpha1.RestoreRequestAnnotation: "the newest"}
	})
	e.pass()
	require.Equal(t, zaentrumv1alpha1.RestoreRefused, e.zaentrum().Status.Backup.Restore.Result)
	e.restart()
	out := e.passAt(first.Add(20 * time.Minute))
	assert.NotContains(t, out, "waits for a backup of the version that runs", "the count goes on")
	out = e.passAt(first.Add(30 * time.Minute))
	assert.True(t, logged(out, "update: no backup before v0.5.0 could be started in 30 minutes; applying it without one"), out)
	assert.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion)
}

// R.C12: a count the CronJob still carries from before a restore that ran is
// not the update's: the update held back after it counts from then.
func TestRC12ACountFromBeforeARestoreIsNotTaken(t *testing.T) {
	e := newUpdatePlatform(t, nil)
	e.listOnClaim(aDump)
	e.change(func(z *zaentrumv1alpha1.Zaentrum) {
		z.Annotations = map[string]string{zaentrumv1alpha1.RestoreRequestAnnotation: aDump}
	})
	e.pass()
	e.endRestoreJob()
	e.pass()
	require.Equal(t, zaentrumv1alpha1.RestoreSucceeded, e.zaentrum().Status.Backup.Restore.Result)
	e.pass()
	// As if a count from before the restore had stayed on the CronJob.
	cron := e.cron()
	cron.Annotations = map[string]string{annotationBackupBeforeUpdate: "v0.4.0",
		annotationBackupBeforeUpdateSince: stamp(e.clock.Add(-time.Hour))}
	require.NoError(t, e.c.Update(context.Background(), cron))
	e.updateTo("v0.5.0")
	out, _ := e.pass()
	assert.True(t, logged(out, "update: v0.5.0 waits for a backup of the version that runs"), out)
	assert.Equal(t, stamp(e.clock), e.cron().Annotations[annotationBackupBeforeUpdateSince], "counted from now")
	assert.Equal(t, "v0.4.0", e.zaentrum().Status.CurrentVersion)
}

// R.C12: a run found running only after the 30 minutes is marked and stopped
// in one patch.
func TestRC12ARunFoundOnlyAfterThe30MinutesIsMarkedAndStoppedAtOnce(t *testing.T) {
	e := newUpdatePlatform(t, nil)
	e.refuse(refusedBackupJobs)
	e.updateTo("v0.5.0")
	e.pass()
	first := e.clock
	e.passAt(first.Add(29*time.Minute + 50*time.Second))
	e.clock = e.clock.Add(5 * time.Second)
	e.scheduledRun("zaentrum-backup-29324160")
	out := e.passAt(first.Add(30 * time.Minute))
	assert.True(t, logged(out, "update: the backup before v0.5.0 has not ended in 30 minutes; stopped it and applying", "job", "zaentrum-backup-29324160"), out)
	run := e.job("zaentrum-backup-29324160")
	assert.Equal(t, "v0.4.0", run.Annotations[annotationBackupBeforeUpdate], "marked")
	assert.Equal(t, stamp(first), run.Annotations[annotationBackupBeforeUpdateSince])
	assert.Equal(t, stamp(e.clock), run.Annotations[annotationBackupBeforeUpdateStopped])
	assert.Equal(t, int64(1), *run.Spec.ActiveDeadlineSeconds)
}

// R.C12: the version an update was applied to is written on its backup again
// where the target moved while the apply failed.
func TestRC12TheVersionAppliedFollowsATargetThatMoved(t *testing.T) {
	e := newUpdatePlatform(t, nil)
	e.refuse(func(verb string, obj any, _ *client.ListOptions) error {
		if u, ok := obj.(*unstructured.Unstructured); ok && verb == "patch" && u.GetKind() == "Deployment" && u.GetName() == "chino-api" {
			containers, _, _ := unstructured.NestedSlice(u.Object, "spec", "template", "spec", "containers")
			if strings.HasSuffix(containers[0].(map[string]any)["image"].(string), ":v0.5.0") {
				return errors.New("admission webhook refused it")
			}
		}
		return nil
	})
	job, _ := e.heldBackUpdate()
	e.endBackup(job, 0, okSummary)
	_, err := e.tryPass()
	require.Error(t, err)
	require.Equal(t, "v0.5.0", e.job(job).Annotations[annotationBackupBeforeUpdateTo])
	e.updateTo("v0.6.0")
	e.pass()
	assert.Equal(t, "v0.6.0", e.zaentrum().Status.CurrentVersion)
	assert.Equal(t, "v0.6.0", e.job(job).Annotations[annotationBackupBeforeUpdateTo])
	assert.Equal(t, goodSet+"; made before the update to v0.6.0", e.backupCondition().Message)
}

// R.C12: a stop counts only from the 30 minutes on: a deadline cut and a time
// set by hand before them are no stop of the operator's.
func TestRC12AStopBeforeThe30MinutesIsNoStop(t *testing.T) {
	e := newUpdatePlatform(t, nil)
	name, first := e.heldBackUpdate()
	job := e.job(name)
	one := int64(1)
	job.Spec.ActiveDeadlineSeconds = &one
	job.Annotations[annotationBackupBeforeUpdateStopped] = stamp(first.Add(5 * time.Minute))
	require.NoError(t, e.c.Update(context.Background(), &job))
	e.passAt(first.Add(10 * time.Minute))
	assert.Equal(t, "v0.4.0", e.zaentrum().Status.CurrentVersion)
	out := e.passAt(first.Add(30 * time.Minute))
	assert.True(t, logged(out, "update: the backup before v0.5.0 has not ended in 30 minutes; stopped it and applying", "job", name), out)
}

// R.C12: the update's own backup, being deleted and not ended, is waited for
// and neither stopped nor marked as made before the update.
func TestRC12ABackupBeingDeletedIsNotStopped(t *testing.T) {
	ctx := context.Background()
	e := newUpdatePlatform(t, nil)
	name, first := e.heldBackUpdate()
	job := e.job(name)
	job.Finalizers = []string{"example.org/hold"}
	require.NoError(t, e.c.Update(ctx, &job))
	require.NoError(t, e.c.Delete(ctx, &job))
	out := e.passAt(first.Add(30 * time.Minute))
	assert.True(t, logged(out, "update: a backup that is being deleted has not ended; the update to v0.5.0 waits for it", "job", name), out)
	assert.Equal(t, int64(10800), *e.job(name).Spec.ActiveDeadlineSeconds, "not stopped")
	out = e.passAt(first.Add(35 * time.Minute))
	assert.True(t, logged(out, "update: the backup before v0.5.0 has not ended in 35 minutes; applying it", "job", name), out)
	assert.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion)
	job = e.job(name)
	assert.Equal(t, int64(10800), *job.Spec.ActiveDeadlineSeconds, "not stopped as the update is applied either")
	assert.Empty(t, job.Annotations[annotationBackupBeforeUpdateTo])
}

// R.C12: an update applied whose apply fails is applied again silently, by
// what this operator remembers where its backup could not be marked, and by
// the mark where an operator that starts again remembers nothing.
func TestRC12AnUpdateAppliedAgainIsAppliedSilently(t *testing.T) {
	refusingApply := func(verb string, obj any) error {
		if u, ok := obj.(*unstructured.Unstructured); ok && verb == "patch" && u.GetKind() == "Deployment" && u.GetName() == "chino-api" {
			containers, _, _ := unstructured.NestedSlice(u.Object, "spec", "template", "spec", "containers")
			if strings.HasSuffix(containers[0].(map[string]any)["image"].(string), ":v0.5.0") {
				return errors.New("admission webhook refused it")
			}
		}
		return nil
	}
	silent := func(t *testing.T, out string) {
		t.Helper()
		assert.NotContains(t, out, "has ended; applying it", out)
		assert.NotContains(t, out, "waits for a backup", out)
	}
	t.Run("by what the operator remembers", func(t *testing.T) {
		e := newUpdatePlatform(t, nil)
		e.refuse(func(verb string, obj any, _ *client.ListOptions) error {
			if j, ok := obj.(*batchv1.Job); ok && verb == "patch" && j.Annotations[annotationBackupBeforeUpdateTo] != "" {
				return errors.New("admission webhook refused it")
			}
			return refusingApply(verb, obj)
		})
		job, _ := e.heldBackUpdate()
		e.endBackup(job, 0, okSummary)
		out, err := e.tryPass()
		require.Error(t, err)
		require.True(t, logged(out, "update: the backup before v0.5.0 has ended; applying it", "job", job), out)
		require.Empty(t, e.job(job).Annotations[annotationBackupBeforeUpdateTo])
		out, err = e.tryPass()
		require.Error(t, err, "still applied")
		silent(t, out)
		assert.Len(t, e.beforeUpdate(), 1)
	})
	t.Run("by the mark", func(t *testing.T) {
		e := newUpdatePlatform(t, nil)
		e.refuse(func(verb string, obj any, _ *client.ListOptions) error { return refusingApply(verb, obj) })
		job, _ := e.heldBackUpdate()
		e.endBackup(job, 0, okSummary)
		_, err := e.tryPass()
		require.Error(t, err)
		e.restart()
		out, err := e.tryPass()
		require.Error(t, err, "still applied")
		silent(t, out)
	})
}

// R.C12: where the CronJob does not carry the count, the backup before the
// update does, for an operator that starts again.
func TestRC12TheCountIsTakenFromTheBackupWhereTheCronJobHasNone(t *testing.T) {
	e := newUpdatePlatform(t, nil)
	name, first := e.heldBackUpdate()
	e.restart()
	cron := e.cron()
	delete(cron.Annotations, annotationBackupBeforeUpdate)
	delete(cron.Annotations, annotationBackupBeforeUpdateSince)
	require.NoError(t, e.c.Update(context.Background(), cron))
	out := e.passAt(first.Add(31 * time.Minute))
	assert.True(t, logged(out, "update: the backup before v0.5.0 has not ended in 30 minutes; stopped it and applying", "job", name), out)
	assert.Equal(t, stamp(first), e.cron().Annotations[annotationBackupBeforeUpdateSince], "the CronJob carries it again")
}

// R.C12: an update applied whose platform then makes no backups is forgotten:
// a backup asked for is refused as backups are off, and waits for nothing.
func TestRC12AnUpdateAppliedIsForgottenWhereBackupsAreTurnedOff(t *testing.T) {
	e := newUpdatePlatform(t, nil)
	failing := true
	e.refuse(func(verb string, obj any, _ *client.ListOptions) error {
		if u, ok := obj.(*unstructured.Unstructured); ok && failing && verb == "patch" && u.GetKind() == "Deployment" && u.GetName() == "chino-api" {
			containers, _, _ := unstructured.NestedSlice(u.Object, "spec", "template", "spec", "containers")
			if strings.HasSuffix(containers[0].(map[string]any)["image"].(string), ":v0.5.0") {
				return errors.New("admission webhook refused it")
			}
		}
		return nil
	})
	job, _ := e.heldBackUpdate()
	e.endBackup(job, 0, okSummary)
	_, err := e.tryPass()
	require.Error(t, err)
	failing = false
	no := false
	e.change(func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.Backup.Enabled = &no })
	e.askBackup("one")
	out, _ := e.pass()
	assert.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion)
	assert.NotContains(t, out, "the backup before it", out)
	assert.True(t, logged(out, "backups: the backup asked for is refused: backups are off", "request", "one"), out)
}

// R.C12: the version an update was applied to is written on its backup on the
// pass after, where the pass that applied it could not.
func TestRC12TheVersionAppliedIsWrittenOnThePassAfter(t *testing.T) {
	e := newUpdatePlatform(t, nil)
	refused := 0
	e.refuse(func(verb string, obj any, _ *client.ListOptions) error {
		if j, ok := obj.(*batchv1.Job); ok && verb == "patch" && refused == 0 && j.Annotations[annotationBackupBeforeUpdateTo] != "" {
			refused++
			return errors.New("admission webhook refused it")
		}
		return nil
	})
	job, _ := e.heldBackUpdate()
	e.endBackup(job, 0, okSummary)
	out, _ := e.pass()
	require.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion)
	require.True(t, logged(out, "update: the backup before v0.5.0 could not be marked as made before it; trying again", "job", job), out)
	e.pass()
	assert.Equal(t, "v0.5.0", e.job(job).Annotations[annotationBackupBeforeUpdateTo], "written once the version moved")
	assert.Equal(t, goodSet+"; made before the update to v0.5.0", e.backupCondition().Message)
}

// R.C12: in auto mode, a pass that cannot look the channel up changes nothing
// of an update held back: the count goes on.
func TestRC12AFailedChannelLookupChangesNothing(t *testing.T) {
	z := verifyCR()
	z.Spec.Update.Mode = zaentrumv1alpha1.UpdateAuto
	z.Status.CurrentVersion = "v0.3.0"
	e := newRequestPlatform(t, z)
	e.pass()
	first := e.clock
	e.pass()
	jobs := e.beforeUpdate()
	require.Len(t, jobs, 1)
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	t.Cleanup(down.Close)
	e.r.ReleasesURL = down.URL
	e.pass()
	got := e.zaentrum()
	assert.Equal(t, "v0.3.0", got.Status.CurrentVersion)
	assert.Equal(t, "v0.4.0", got.Status.AvailableUpdate, "the version that waits")
	assert.Equal(t, stamp(first), e.cron().Annotations[annotationBackupBeforeUpdateSince], "the count goes on")
	e.r.ReleasesURL = ""
	out, _ := e.pass()
	assert.NotContains(t, out, "waits for a backup of the version that runs", "not held back anew")
	assert.Len(t, e.beforeUpdate(), 1, "the same backup")
	e.endBackup(jobs[0].Name, 0, okSummary)
	e.pass()
	assert.Equal(t, "v0.4.0", e.zaentrum().Status.CurrentVersion)
}

// R.C12: a run the operator could not mark before the 35 minutes is marked,
// stopped and recorded as the update is applied.
func TestRC12ARunNeverMarkedIsMarkedAndStoppedAsTheUpdateIsApplied(t *testing.T) {
	e := newUpdatePlatform(t, nil)
	e.scheduledRun("zaentrum-backup-29324160")
	refusing := true
	e.refuse(func(verb string, obj any, _ *client.ListOptions) error {
		if j, ok := obj.(*batchv1.Job); ok && refusing && verb == "patch" && j.Name == "zaentrum-backup-29324160" {
			return errors.New("admission webhook refused it")
		}
		return nil
	})
	e.updateTo("v0.5.0")
	e.pass()
	first := e.clock
	e.passAt(first.Add(30 * time.Minute))
	require.Empty(t, e.beforeUpdate(), "not marked yet")
	refusing = false
	out := e.passAt(first.Add(35 * time.Minute))
	assert.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion)
	run := e.job("zaentrum-backup-29324160")
	assert.Equal(t, "v0.4.0", run.Annotations[annotationBackupBeforeUpdate], "marked")
	assert.Equal(t, stamp(first), run.Annotations[annotationBackupBeforeUpdateSince])
	e.stoppedAsApplied(t, out, run.Name)
}

// failsWithItsPodRunning fails a backup Job as the Job controller does at its
// deadline, while its pod still runs, and gives that pod.
func (e *platformEnv) failsWithItsPodRunning(name string) *corev1.Pod {
	e.t.Helper()
	ctx := context.Background()
	job := e.job(name)
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
		LastTransitionTime: metav1.NewTime(e.clock), Reason: "DeadlineExceeded"}}
	require.NoError(e.t, e.c.Status().Update(ctx, &job))
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name + "-x", Namespace: e.key.Namespace,
		Labels: map[string]string{batchv1.JobNameLabel: name}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	require.NoError(e.t, e.c.Create(ctx, pod))
	return pod
}

// podEnds ends the pod.
func (e *platformEnv) podEnds(pod *corev1.Pod) {
	e.t.Helper()
	pod.Status = corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{{Name: templates.BackupContainer,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 143, FinishedAt: metav1.NewTime(e.clock)}}}}}
	require.NoError(e.t, e.c.Status().Update(context.Background(), pod))
}

// R.C12: a stop refused as the update is applied is owed on the cluster, and
// a backup that then ends before it is made is not marked as made before the
// update.
func TestRC12ABackupThatEndsBeforeItsOwedStopIsNotMadeBefore(t *testing.T) {
	e := newUpdatePlatform(t, nil)
	refusing := true
	e.refuse(func(verb string, obj any, _ *client.ListOptions) error {
		if j, ok := obj.(*batchv1.Job); ok && refusing && verb == "patch" && j.Annotations[annotationBackupBeforeUpdateTo] != "" {
			return errors.New("admission webhook refused it")
		}
		return nil
	})
	job, first := e.heldBackUpdate()
	e.passAt(first.Add(45 * time.Minute))
	require.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion)
	held := e.cron().Annotations
	assert.Equal(t, job, held[annotationBackupBeforeUpdateStopOwed], "the stop owed is kept on the cluster, with the Job")
	assert.Equal(t, "v0.5.0", held[annotationBackupBeforeUpdateTo])
	e.endBackup(job, 0, okSummary)
	refusing = false
	e.pass()
	assert.Empty(t, e.job(job).Annotations[annotationBackupBeforeUpdateTo], "it ended before its stop: not made before the update")
	assert.Equal(t, goodSet, e.backupCondition().Message)
	assert.Empty(t, e.cron().Annotations[annotationBackupBeforeUpdate], "nothing is owed any more: the record goes")
}

// R.C12: where the backups' Jobs cannot be listed at the 35 minutes, a stop is
// owed, and the first pass that can list stops the backup of that count that
// still runs.
func TestRC12ABackupNotListedAtThe35MinutesIsStoppedOnceItCanBe(t *testing.T) {
	e := newUpdatePlatform(t, nil)
	job, first := e.heldBackUpdate()
	listing := false
	e.refuse(func(verb string, obj any, opts *client.ListOptions) error {
		if _, ok := obj.(*batchv1.JobList); ok && !listing && verb == "list" && opts.LabelSelector != nil &&
			opts.LabelSelector.Matches(labels.Set{labelDatabase: databaseBackup}) {
			return errors.New("the server is currently unable to handle the request")
		}
		return nil
	})
	out := e.passAt(first.Add(35 * time.Minute))
	require.True(t, logged(out, "update: the backups' Jobs could not be listed in 35 minutes; applying v0.5.0 without a backup"), out)
	require.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion)
	assert.Equal(t, stopOwedUnknown, e.cron().Annotations[annotationBackupBeforeUpdateStopOwed], "the Jobs could not be listed")
	listing = true
	out, _ = e.pass()
	e.stoppedAsApplied(t, out, job)
	assert.Empty(t, e.cron().Annotations[annotationBackupBeforeUpdate], "done: the record goes")
}

// R.C12: a stop owed stays owed through a pass that applies the update again
// without listing the backups' Jobs.
func TestRC12AStopOwedStaysOwedWhereTheJobsCannotBeListed(t *testing.T) {
	e := newUpdatePlatform(t, nil)
	stopping, listing, applying := false, true, false
	e.refuse(func(verb string, obj any, opts *client.ListOptions) error {
		if j, ok := obj.(*batchv1.Job); ok && !stopping && verb == "patch" && j.Annotations[annotationBackupBeforeUpdateTo] != "" {
			return errors.New("admission webhook refused it")
		}
		if _, ok := obj.(*batchv1.JobList); ok && !listing && verb == "list" && opts.LabelSelector != nil &&
			opts.LabelSelector.Matches(labels.Set{labelDatabase: databaseBackup}) {
			return errors.New("the server is currently unable to handle the request")
		}
		return refusedApplyOf(verb, obj, "v0.5.0", !applying)
	})
	job, first := e.heldBackUpdate()
	e.clock = first.Add(45*time.Minute - 10*time.Second)
	_, err := e.tryPass()
	require.Error(t, err, "the apply of v0.5.0 fails")
	listing = false
	_, err = e.tryPass()
	require.Error(t, err)
	listing, applying, stopping = true, true, true
	e.endBackup(job, 0, okSummary)
	e.pass()
	assert.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion)
	assert.Empty(t, e.job(job).Annotations[annotationBackupBeforeUpdateTo], "still owed its stop as it ended: not made before")
	assert.Equal(t, goodSet, e.backupCondition().Message)
}

// refusedApplyOf refuses, where refusing, the apply of chino-api at the
// version.
func refusedApplyOf(verb string, obj any, version string, refusing bool) error {
	if u, ok := obj.(*unstructured.Unstructured); ok && refusing && verb == "patch" && u.GetKind() == "Deployment" && u.GetName() == "chino-api" {
		containers, _, _ := unstructured.NestedSlice(u.Object, "spec", "template", "spec", "containers")
		if strings.HasSuffix(containers[0].(map[string]any)["image"].(string), ":"+version) {
			return errors.New("admission webhook refused it")
		}
	}
	return nil
}

// R.C12: a stop refused as the update is applied is made by an operator that
// starts again: what is owed is kept on the CronJob, beside the count.
func TestRC12AStopOwedIsMadeByAnOperatorThatStartsAgain(t *testing.T) {
	e := newUpdatePlatform(t, nil)
	refusing := true
	e.refuse(func(verb string, obj any, _ *client.ListOptions) error {
		if j, ok := obj.(*batchv1.Job); ok && refusing && verb == "patch" && j.Annotations[annotationBackupBeforeUpdateTo] != "" {
			return errors.New("admission webhook refused it")
		}
		return nil
	})
	job, first := e.heldBackUpdate()
	e.passAt(first.Add(45 * time.Minute))
	require.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion)
	e.restart()
	refusing = false
	out, _ := e.pass()
	e.stoppedAsApplied(t, out, job)
}

// R.C12: a mark refused after the version moved is tried again on every pass
// until it is made.
func TestRC12AMarkRefusedIsTriedAgainOnEveryPass(t *testing.T) {
	e := newUpdatePlatform(t, nil)
	refused := 0
	e.refuse(func(verb string, obj any, _ *client.ListOptions) error {
		if j, ok := obj.(*batchv1.Job); ok && verb == "patch" && refused < 3 && j.Annotations[annotationBackupBeforeUpdateTo] != "" {
			refused++
			return errors.New("admission webhook refused it")
		}
		return nil
	})
	job, _ := e.heldBackUpdate()
	e.endBackup(job, 0, okSummary)
	e.pass()
	require.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion)
	e.pass()
	e.pass()
	require.Equal(t, 3, refused)
	require.Empty(t, e.job(job).Annotations[annotationBackupBeforeUpdateTo])
	e.pass()
	assert.Equal(t, "v0.5.0", e.job(job).Annotations[annotationBackupBeforeUpdateTo], "made on the fourth try")
	assert.Equal(t, goodSet+"; made before the update to v0.5.0", e.backupCondition().Message)
}

// R.C12: a backup is said to be made before the update only where it ended
// before the 35 minutes were over.
func TestRC12MadeBeforeTheUpdateEndsAtThe35Minutes(t *testing.T) {
	for _, c := range []struct {
		name    string
		ended   time.Duration
		claimed bool
	}{
		{"ended at 34:59", 34*time.Minute + 59*time.Second, true},
		{"ended at 35:00", 35 * time.Minute, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newUpdatePlatform(t, nil)
			job, first := e.heldBackUpdate()
			e.clock = first.Add(c.ended)
			e.endBackup(job, 0, okSummary)
			e.passAt(first.Add(35*time.Minute + 10*time.Second))
			require.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion)
			want := goodSet
			if c.claimed {
				want += "; made before the update to v0.5.0"
			}
			assert.Equal(t, want, e.backupCondition().Message)
		})
	}
}

// R.C12: in auto mode a failed channel lookup goes on with the version that
// waits only where an update is held back or applied, and never where the
// version is pinned.
func TestRC12AFailedLookupGoesOnOnlyWithAnUpdatePending(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	t.Cleanup(down.Close)
	t.Run("none pending", func(t *testing.T) {
		z := verifyCR()
		z.Spec.Update.Mode = zaentrumv1alpha1.UpdateAuto
		z.Status.CurrentVersion = "v0.3.0"
		z.Status.AvailableUpdate = "v0.4.0"
		e := newRequestPlatform(t, z)
		e.r.ReleasesURL = down.URL
		e.pass()
		e.pass()
		assert.Equal(t, "v0.3.0", e.zaentrum().Status.CurrentVersion)
		assert.Empty(t, e.cron().Annotations[annotationBackupBeforeUpdate], "no update is held back from what the status said")
		assert.Empty(t, e.beforeUpdate())
	})
	t.Run("pinned", func(t *testing.T) {
		z := verifyCR()
		z.Spec.Update.Mode = zaentrumv1alpha1.UpdateAuto
		z.Status.CurrentVersion = "v0.3.0"
		e := newRequestPlatform(t, z)
		e.pass()
		e.pass()
		require.Len(t, e.beforeUpdate(), 1)
		e.change(func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.Version = "v0.3.0" })
		e.pass()
		got := e.zaentrum()
		assert.Equal(t, "v0.3.0", got.Status.CurrentVersion)
		assert.Empty(t, got.Status.AvailableUpdate, "pinned to the version that runs: the update is taken back")
		assert.Empty(t, e.cron().Annotations[annotationBackupBeforeUpdate])
	})
}

// R.C12: no backup starts, and the schedule does not move, while a pod of a
// backup Job that has failed still runs: a request waits for it, and so does
// the backup before an update.
func TestRC12NoBackupStartsWhileAFailedBackupsPodRuns(t *testing.T) {
	t.Run("the schedule", func(t *testing.T) {
		e := newRequestPlatform(t, verifyCR())
		e.pass()
		e.scheduledRun("zaentrum-backup-29324160")
		pod := e.failsWithItsPodRunning("zaentrum-backup-29324160")
		e.pass()
		assert.True(t, e.suspended(), "its pod may still write")
		assert.Equal(t, "zaentrum-backup-29324160", e.backup().Running, "it runs while its pod does")
		e.podEnds(pod)
		e.pass()
		assert.False(t, e.suspended())
		assert.Empty(t, e.backup().Running)
	})
	t.Run("a pass that refuses the resource", func(t *testing.T) {
		e := newRequestPlatform(t, verifyCR())
		e.pass()
		e.scheduledRun("zaentrum-backup-29324160")
		pod := e.failsWithItsPodRunning("zaentrum-backup-29324160")
		e.pass()
		require.Equal(t, "zaentrum-backup-29324160", e.backup().Running)
		e.change(func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.Databases.Chino = "chi no" })
		e.pass()
		assert.Equal(t, "zaentrum-backup-29324160", e.backup().Running, "its pod still runs")
		e.podEnds(pod)
		e.pass()
		assert.Empty(t, e.backup().Running, "its pod has ended")
	})
	t.Run("a request", func(t *testing.T) {
		e := newRequestPlatform(t, verifyCR())
		e.pass()
		e.scheduledRun("zaentrum-backup-29324160")
		pod := e.failsWithItsPodRunning("zaentrum-backup-29324160")
		e.askBackup("one")
		e.pass()
		out, _ := e.pass()
		assert.True(t, logged(out, "backups: the backup asked for waits", "request", "one", "for", "zaentrum-backup-29324160"), out)
		assert.Empty(t, e.carrying("one"))
		e.podEnds(pod)
		e.pass()
		assert.Len(t, e.carrying("one"), 1, "it starts once the pod has ended")
	})
	t.Run("the backup before an update", func(t *testing.T) {
		e := newUpdatePlatform(t, nil)
		e.scheduledRun("zaentrum-backup-29324160")
		pod := e.failsWithItsPodRunning("zaentrum-backup-29324160")
		e.updateTo("v0.5.0")
		e.pass()
		out, _ := e.pass()
		assert.True(t, logged(out, "update: a backup that has ended still has a pod that runs; the update to v0.5.0 waits for it",
			"job", "zaentrum-backup-29324160"), out)
		assert.Empty(t, e.beforeUpdate())
		e.podEnds(pod)
		e.pass()
		assert.Len(t, e.beforeUpdate(), 1, "it starts once the pod has ended")
	})
}

// refusedApplyOfKind refuses, where refusing, the apply of the objects of a
// kind: one the platform applies after the backups' CronJob, so that the
// CronJob's record is written on the pass whose apply fails.
func refusedApplyOfKind(verb string, obj any, kind string, refusing bool) error {
	if u, ok := obj.(*unstructured.Unstructured); ok && refusing && verb == "patch" && u.GetKind() == kind {
		return errors.New("admission webhook refused it")
	}
	return nil
}

// R.C12: a run never marked whose stop is refused as the update is applied is
// owed its stop by name, and the first pass whose patch is taken stops it.
func TestRC12ARunNeverMarkedIsOwedItsStopByName(t *testing.T) {
	e := newUpdatePlatform(t, nil)
	e.scheduledRun("zaentrum-backup-29324160")
	refusing := true
	e.refuse(func(verb string, obj any, _ *client.ListOptions) error {
		if j, ok := obj.(*batchv1.Job); ok && refusing && verb == "patch" && j.Name == "zaentrum-backup-29324160" {
			return errors.New("admission webhook refused it")
		}
		return nil
	})
	e.updateTo("v0.5.0")
	e.pass()
	first := e.clock
	e.passAt(first.Add(36 * time.Minute))
	require.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion)
	held := e.cron().Annotations
	assert.Equal(t, "v0.5.0", held[annotationBackupBeforeUpdateTo])
	assert.Equal(t, "zaentrum-backup-29324160", held[annotationBackupBeforeUpdateStopOwed], "owed by name: it carries no mark")
	refusing = false
	out, _ := e.pass()
	run := e.job("zaentrum-backup-29324160")
	assert.Equal(t, int64(1), *run.Spec.ActiveDeadlineSeconds, "stopped")
	assert.Equal(t, "v0.4.0", run.Annotations[annotationBackupBeforeUpdate], "and marked")
	assert.True(t, logged(out, "update: the backup before v0.5.0 still runs after 35 minutes; stopped it", "job", run.Name), out)
	assert.Empty(t, e.cron().Annotations[annotationBackupBeforeUpdate], "done: the record goes")
}

// R.C12: a backup the operator stopped is never said to be made before the
// update, whatever the Job controller's stamp says.
func TestRC12AStoppedBackupIsNeverMadeBefore(t *testing.T) {
	e := newUpdatePlatform(t, nil)
	job, first := e.heldBackUpdate()
	e.passAt(first.Add(35 * time.Minute))
	require.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion)
	require.Equal(t, int64(1), *e.job(job).Spec.ActiveDeadlineSeconds)
	// The Job controller's clock behind the operator's: Complete at 34:50.
	e.clock = first.Add(34*time.Minute + 50*time.Second)
	e.endBackup(job, 0, okSummary)
	e.passAt(first.Add(36 * time.Minute))
	assert.Equal(t, goodSet, e.backupCondition().Message)
}

// R.C12: a running Job that carries the count's mark but is owed no stop is
// left as it is, and one being deleted is not the one settled.
func TestRC12OnlyTheBackupOwedIsSettled(t *testing.T) {
	t.Run("a mark by hand on a run owed no stop", func(t *testing.T) {
		e := newUpdatePlatform(t, nil)
		job, _ := e.heldBackUpdate()
		e.endBackup(job, 0, okSummary)
		e.pass()
		require.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion)
		e.scheduledRun("zaentrum-backup-29324160")
		run := e.job("zaentrum-backup-29324160")
		run.Annotations = e.job(job).Annotations
		delete(run.Annotations, annotationBackupBeforeUpdateTo)
		require.NoError(t, e.c.Update(context.Background(), &run))
		e.pass()
		e.pass()
		run = e.job(run.Name)
		assert.Equal(t, int64(10800), *run.Spec.ActiveDeadlineSeconds, "owed no stop: not stopped")
		assert.Empty(t, run.Annotations[annotationBackupBeforeUpdateTo], "nor marked: it is left as it is")
	})
	t.Run("a match being deleted", func(t *testing.T) {
		ctx := context.Background()
		e := newUpdatePlatform(t, nil)
		refusing := true
		e.refuse(func(verb string, obj any, _ *client.ListOptions) error {
			if j, ok := obj.(*batchv1.Job); ok && refusing && verb == "patch" && j.Annotations[annotationBackupBeforeUpdateTo] != "" {
				return errors.New("admission webhook refused it")
			}
			return nil
		})
		job, _ := e.heldBackUpdate()
		e.endBackup(job, 0, okSummary)
		e.pass()
		require.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion)
		require.Empty(t, e.job(job).Annotations[annotationBackupBeforeUpdateTo])
		e.scheduledRun("zaentrum-backup-29324160")
		copied := e.job("zaentrum-backup-29324160")
		copied.Annotations = e.job(job).Annotations
		copied.Finalizers = []string{"example.org/hold"}
		require.NoError(t, e.c.Update(ctx, &copied))
		require.NoError(t, e.c.Delete(ctx, &copied))
		refusing = false
		e.pass()
		assert.Equal(t, "v0.5.0", e.job(job).Annotations[annotationBackupBeforeUpdateTo], "the count's that is not being deleted")
	})
}

// R.C12: at the 35 minutes the update is applied, also while a failed
// backup's pod still runs.
func TestRC12TheUpdateIsAppliedAt35WhileAFailedBackupsPodRuns(t *testing.T) {
	e := newUpdatePlatform(t, nil)
	e.scheduledRun("zaentrum-backup-29324160")
	e.failsWithItsPodRunning("zaentrum-backup-29324160")
	e.updateTo("v0.5.0")
	e.pass()
	first := e.clock
	e.passAt(first.Add(35*time.Minute - 10*time.Second))
	require.Equal(t, "v0.4.0", e.zaentrum().Status.CurrentVersion)
	out := e.passAt(first.Add(35 * time.Minute))
	assert.True(t, logged(out, "update: no backup before v0.5.0 could be started in 30 minutes; applying it without one"), out)
	assert.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion)
}

// R.C12: what the CronJob carries makes an operator that starts again apply
// an update applied before again, silently, and owe the stop it owed, while
// the version that runs has not moved.
func TestRC12TheCronJobsRecordCountsWhileTheVersionHasNotMoved(t *testing.T) {
	t.Run("applied again", func(t *testing.T) {
		e := newUpdatePlatform(t, nil)
		refusing := false
		e.refuse(func(verb string, obj any, opts *client.ListOptions) error {
			if err := refusedBackupJobs(verb, obj, opts); err != nil {
				return err
			}
			return refusedApplyOfKind(verb, obj, "Ingress", refusing)
		})
		e.updateTo("v0.5.0")
		e.pass()
		first := e.clock
		refusing = true
		e.clock = first.Add(30*time.Minute - 10*time.Second)
		out, err := e.tryPass()
		require.Error(t, err)
		require.True(t, logged(out, "update: no backup before v0.5.0 could be started in 30 minutes; applying it without one"), out)
		require.Equal(t, "v0.5.0", e.cron().Annotations[annotationBackupBeforeUpdateTo], "the record is on the CronJob")
		e.restart()
		out, err = e.tryPass()
		require.Error(t, err)
		assert.NotContains(t, out, "could be started in 30 minutes", "applied again, silently")
	})
	t.Run("the stop owed", func(t *testing.T) {
		e := newUpdatePlatform(t, nil)
		refusing, stopping := false, false
		e.refuse(func(verb string, obj any, _ *client.ListOptions) error {
			if j, ok := obj.(*batchv1.Job); ok && !stopping && verb == "patch" && j.Annotations[annotationBackupBeforeUpdateTo] != "" {
				return errors.New("admission webhook refused it")
			}
			return refusedApplyOfKind(verb, obj, "Ingress", refusing)
		})
		job, first := e.heldBackUpdate()
		refusing = true
		e.clock = first.Add(36*time.Minute - 10*time.Second)
		_, err := e.tryPass()
		require.Error(t, err)
		require.Equal(t, job, e.cron().Annotations[annotationBackupBeforeUpdateStopOwed])
		e.restart()
		stopping = true
		e.endBackup(job, 0, okSummary)
		_, err = e.tryPass()
		require.Error(t, err, "the apply still fails")
		assert.Empty(t, e.job(job).Annotations[annotationBackupBeforeUpdateTo], "ended before its owed stop: not marked")
	})
}

// R.C12: pods that cannot be listed count as running: the update waits for a
// stopped backup until its pods can be seen to have ended.
func TestRC12PodsThatCannotBeListedCountAsRunning(t *testing.T) {
	e := newUpdatePlatform(t, nil)
	listing := true
	e.refuse(func(verb string, obj any, _ *client.ListOptions) error {
		if _, ok := obj.(*corev1.PodList); ok && !listing && verb == "list" {
			return errors.New("the server is currently unable to handle the request")
		}
		return nil
	})
	job, first := e.heldBackUpdate()
	e.passAt(first.Add(30 * time.Minute))
	e.endBackup(job, 1, "")
	listing = false
	e.pass()
	assert.Equal(t, "v0.4.0", e.zaentrum().Status.CurrentVersion, "its pods cannot be listed: they may still write")
	listing = true
	out, _ := e.pass()
	assert.True(t, logged(out, "update: the stopped backup before v0.5.0 has ended; applying it", "job", job), out)
}

// R.C12: a backup owed a stop that is being deleted once the version has moved
// is not settled: its pods end with it.
func TestRC12AnOwedBackupBeingDeletedIsNotSettled(t *testing.T) {
	ctx := context.Background()
	e := newUpdatePlatform(t, nil)
	refusing := true
	e.refuse(func(verb string, obj any, _ *client.ListOptions) error {
		if j, ok := obj.(*batchv1.Job); ok && refusing && verb == "patch" && j.Annotations[annotationBackupBeforeUpdateTo] != "" {
			return errors.New("admission webhook refused it")
		}
		return nil
	})
	name, first := e.heldBackUpdate()
	e.passAt(first.Add(36 * time.Minute))
	require.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion)
	job := e.job(name)
	job.Finalizers = []string{"example.org/hold"}
	require.NoError(t, e.c.Update(ctx, &job))
	require.NoError(t, e.c.Delete(ctx, &job))
	refusing = false
	e.pass()
	job = e.job(name)
	assert.Equal(t, int64(10800), *job.Spec.ActiveDeadlineSeconds, "not patched")
	assert.Empty(t, job.Annotations[annotationBackupBeforeUpdateTo])
	assert.Empty(t, e.cron().Annotations[annotationBackupBeforeUpdate], "nothing more is owed: the record goes")
}

// R.C12: a backup asked for does not wait while the CronJob still keeps the
// record of an update whose version has moved.
func TestRC12ARequestDoesNotWaitForAnUpdateAlreadyApplied(t *testing.T) {
	e := newUpdatePlatform(t, nil)
	e.refuse(func(verb string, obj any, _ *client.ListOptions) error {
		if j, ok := obj.(*batchv1.Job); ok && verb == "patch" && j.Annotations[annotationBackupBeforeUpdateTo] != "" {
			return errors.New("admission webhook refused it")
		}
		return nil
	})
	job, _ := e.heldBackUpdate()
	e.endBackup(job, 0, okSummary)
	e.pass()
	require.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion)
	e.askBackup("one")
	e.pass()
	out, _ := e.pass()
	require.Equal(t, "v0.5.0", e.cron().Annotations[annotationBackupBeforeUpdateTo], "the record is kept: its mark is refused")
	assert.NotContains(t, out, "the update to v0.5.0 and the backup before it", out)
	assert.Len(t, e.carrying("one"), 1, "it starts")
}

// R.C12: only a backup Job that failed holds a new one back by its pod; one
// that succeeded has ended, whatever its pod says.
func TestRC12OnlyAFailedBackupsPodHoldsANewOneBack(t *testing.T) {
	ctx := context.Background()
	e := newRequestPlatform(t, verifyCR())
	e.pass()
	e.scheduledRun("zaentrum-backup-29324160")
	e.endBackup("zaentrum-backup-29324160", 0, okSummary)
	var pod corev1.Pod
	require.NoError(t, e.c.Get(ctx, types.NamespacedName{Namespace: e.key.Namespace, Name: "zaentrum-backup-29324160-x"}, &pod))
	pod.Status.Phase = corev1.PodRunning
	require.NoError(t, e.c.Status().Update(ctx, &pod))
	e.askBackup("one")
	e.pass()
	e.pass()
	assert.Len(t, e.carrying("one"), 1, "it starts")
}

// R.C12: a stop owed that names no Job, as the backups' Jobs could not be
// listed, is owed, where no backup of the count's runs, to the newest backup
// that has not ended, and only to one that began before the 35 minutes were
// over.
func TestRC12AStopOwedToNoNameGoesToTheNewestBackupThatBeganInTime(t *testing.T) {
	since := time.Date(2026, 10, 4, 18, 0, 0, 0, time.UTC)
	u := updateBackup{from: "v0.4.0", to: "v0.5.0", since: since, applied: true, moved: true, stopOwed: stopOwedUnknown}
	run := func(name string, after time.Duration, ended bool) batchv1.Job {
		job := batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(since.Add(after))}}
		if ended {
			job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
		}
		return job
	}
	name := func(job *batchv1.Job) string {
		if job == nil {
			return ""
		}
		return job.Name
	}
	t.Run("beyond the count's, the newest that has not ended", func(t *testing.T) {
		jobs := []batchv1.Job{run("ended", 30*time.Minute, true), run("newer", 20*time.Minute, false), run("older", 10*time.Minute, false)}
		assert.Equal(t, "newer", name(owedJob(u, jobs)))
	})
	t.Run("only one that began before the 35 minutes were over", func(t *testing.T) {
		jobs := []batchv1.Job{run("after", 36*time.Minute, false), run("before", 5*time.Minute, false)}
		assert.Equal(t, "before", name(owedJob(u, jobs)))
		assert.Empty(t, name(owedJob(u, jobs[:1])), "none began in time")
	})
}

// R.C12: a stop refused to a backup the stop owed to no name found is owed to
// that backup by name: the record says which Job it is, and the pass after
// stops it.
func TestRC12ARefusedStopOfABackupFoundForNoNameNamesIt(t *testing.T) {
	e := newUpdatePlatform(t, nil)
	job, first := e.heldBackUpdate()
	listing, stopping := false, false
	e.refuse(func(verb string, obj any, opts *client.ListOptions) error {
		if _, ok := obj.(*batchv1.JobList); ok && !listing && verb == "list" && opts.LabelSelector != nil &&
			opts.LabelSelector.Matches(labels.Set{labelDatabase: databaseBackup}) {
			return errors.New("the server is currently unable to handle the request")
		}
		if j, ok := obj.(*batchv1.Job); ok && !stopping && verb == "patch" && j.Annotations[annotationBackupBeforeUpdateTo] != "" {
			return errors.New("admission webhook refused it")
		}
		return nil
	})
	e.passAt(first.Add(35 * time.Minute))
	require.Equal(t, "v0.5.0", e.zaentrum().Status.CurrentVersion)
	require.Equal(t, stopOwedUnknown, e.cron().Annotations[annotationBackupBeforeUpdateStopOwed])
	listing = true
	e.pass()
	assert.Equal(t, job, e.cron().Annotations[annotationBackupBeforeUpdateStopOwed], "the stop refused names the backup")
	stopping = true
	out, _ := e.pass()
	e.stoppedAsApplied(t, out, job)
}
