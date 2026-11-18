package controller

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache/informertest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllertest"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
	"github.com/zaentrum/zaentrum-operator/operator/internal/templates"
)

// newRequestPlatform is a platform on the fake API server, as newPlatformEnv
// has it, whose creations carry the env's clock as their creation time, as
// the API server stamps them: the fake client leaves it empty, and the
// backups' Jobs are read newest first by it.
func newRequestPlatform(t *testing.T, z *zaentrumv1alpha1.Zaentrum, objs ...client.Object) *platformEnv {
	t.Helper()
	var e *platformEnv
	e = newPlatformEnv(t, z, &interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if created := obj.GetCreationTimestamp(); created.IsZero() {
			obj.SetCreationTimestamp(metav1.NewTime(e.clock))
		}
		return c.Create(ctx, obj, opts...)
	}}, objs...)
	return e
}

// pass takes one pass, ten seconds after the last, and gives what it logged
// and when it comes back.
func (e *platformEnv) pass() (string, ctrl.Result) {
	e.t.Helper()
	e.clock = e.clock.Add(10 * time.Second)
	var buf bytes.Buffer
	ctx := log.IntoContext(context.Background(), zap.New(zap.WriteTo(&buf)))
	res, err := e.r.Reconcile(ctx, ctrl.Request{NamespacedName: e.key})
	require.NoError(e.t, err)
	return buf.String(), res
}

// askBackup sets zaentrum.io/backup-request, as kubectl annotate does.
func (e *platformEnv) askBackup(value string) {
	e.t.Helper()
	e.change(func(z *zaentrumv1alpha1.Zaentrum) {
		if z.Annotations == nil {
			z.Annotations = map[string]string{}
		}
		z.Annotations[zaentrumv1alpha1.BackupRequestAnnotation] = value
	})
}

// backup is status.backup as the API server holds it, never nil.
func (e *platformEnv) backup() zaentrumv1alpha1.BackupStatus {
	e.t.Helper()
	if b := e.zaentrum().Status.Backup; b != nil {
		return *b
	}
	return zaentrumv1alpha1.BackupStatus{}
}

// cron is the backups' CronJob as applied.
func (e *platformEnv) cron() *batchv1.CronJob {
	e.t.Helper()
	var cron batchv1.CronJob
	require.NoError(e.t, e.c.Get(context.Background(), types.NamespacedName{Namespace: e.key.Namespace, Name: templates.BackupCronJobName}, &cron))
	return &cron
}

func (e *platformEnv) suspended() bool {
	e.t.Helper()
	s := e.cron().Spec.Suspend
	return s != nil && *s
}

// backupJobs lists the backups' Jobs by their label, as whatever reads the
// backups lists them.
func (e *platformEnv) backupJobs() []batchv1.Job {
	e.t.Helper()
	var jobs batchv1.JobList
	require.NoError(e.t, e.c.List(context.Background(), &jobs, client.InNamespace(e.key.Namespace),
		client.MatchingLabels{labelDatabase: databaseBackup}))
	return jobs.Items
}

// requested lists the backups' Jobs a request started.
func (e *platformEnv) requested() []batchv1.Job {
	e.t.Helper()
	var out []batchv1.Job
	for _, j := range e.backupJobs() {
		if _, ok := j.Annotations[zaentrumv1alpha1.BackupRequestAnnotation]; ok {
			out = append(out, j)
		}
	}
	return out
}

// scheduledRun makes one of the CronJob's runs, unfinished, as its controller
// makes them: owned by the CronJob, labelled from its job template, and named
// after the minute it was due.
func (e *platformEnv) scheduledRun(name string) {
	e.t.Helper()
	cron := e.cron()
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: e.key.Namespace,
		CreationTimestamp: metav1.NewTime(e.clock), Labels: cron.Spec.JobTemplate.Labels,
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "CronJob", Name: cron.Name, UID: cron.UID,
			Controller: ptrTo(true)}}},
		Spec: cron.Spec.JobTemplate.Spec}
	require.NoError(e.t, e.c.Create(context.Background(), job))
}

// endBackup ends a backup Job as the Job controller and the kubelet do: exit
// 0 with the summary, or exit 1 with the reason.
func (e *platformEnv) endBackup(name string, exit int32, message string) {
	e.t.Helper()
	ctx := context.Background()
	var job batchv1.Job
	require.NoError(e.t, e.c.Get(ctx, types.NamespacedName{Namespace: e.key.Namespace, Name: name}, &job))
	ended := metav1.NewTime(e.clock)
	cond := batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: ended}
	if exit != 0 {
		cond = batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, LastTransitionTime: ended, Reason: "BackoffLimitExceeded"}
	}
	job.Status.Conditions = []batchv1.JobCondition{cond}
	require.NoError(e.t, e.c.Status().Update(ctx, &job))
	require.NoError(e.t, e.c.Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name + "-x", Namespace: e.key.Namespace,
		Labels: map[string]string{batchv1.JobNameLabel: name}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: templates.BackupContainer,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exit, Message: message, FinishedAt: ended}}}}}}))
}

func (e *platformEnv) backupCondition() *metav1.Condition {
	e.t.Helper()
	return backupCondition(e.zaentrum())
}

// logged says whether the log holds the line, with these keys and values.
func logged(log, msg string, kv ...string) bool {
	for _, line := range strings.Split(log, "\n") {
		if !strings.Contains(line, `"msg":"`+msg+`"`) {
			continue
		}
		all := true
		for i := 0; i+1 < len(kv); i += 2 {
			all = all && strings.Contains(line, `"`+kv[i]+`":"`+kv[i+1]+`"`)
		}
		if all {
			return true
		}
	}
	return false
}

// R.C11: a backup asked for, start to end. The pass that takes the request up
// finds the CronJob not yet suspended: it applies it suspended and starts no
// Job. The next pass, which finds it suspended and no backup running, starts
// one Job, the schedule's own job template made into a Job of the platform's,
// with the request's value; status.backup.running names it. Once it has ended
// and been read, the request is answered, and the last backup and the Backup
// condition are that run's; the schedule moves again on the pass after the
// answer.
func TestRC11ABackupAskedForStartsOnceTheScheduleStandsStill(t *testing.T) {
	e := newRequestPlatform(t, verifyCR())
	e.pass()
	require.False(t, e.suspended(), "nothing asked: the schedule runs")

	e.askBackup("  one  ")
	out, res := e.pass()
	assert.Empty(t, e.backupJobs(), "a pass that finds the CronJob not yet suspended starts no backup")
	assert.True(t, e.suspended(), "it suspends it")
	assert.True(t, logged(out, "backups: the backup asked for waits", "request", "one", "for", "the schedule to stand still"), out)
	assert.Equal(t, verifyRequeueAfter, res.RequeueAfter, "the next pass comes soon")
	assert.Empty(t, e.backup().Request)
	assert.Empty(t, e.backup().Running)

	out, _ = e.pass()
	jobs := e.requested()
	require.Len(t, jobs, 1, "a pass that finds it suspended starts the backup")
	job := jobs[0]
	assert.True(t, strings.HasPrefix(job.Name, templates.BackupCronJobName+"-"), job.Name)
	assert.Len(t, strings.TrimPrefix(job.Name, templates.BackupCronJobName+"-"), 5, job.Name)
	assert.True(t, metav1.IsControlledBy(&job, e.zaentrum()), "the platform's own Job")
	assert.Equal(t, "one", job.Annotations[zaentrumv1alpha1.BackupRequestAnnotation], "the value, trimmed")
	cron := e.cron()
	assert.Equal(t, cron.Spec.JobTemplate.Labels, job.Labels, "the schedule's labels, zaentrum.io/database: backup among them")
	assert.Equal(t, databaseBackup, job.Labels[labelDatabase])
	assert.Equal(t, cron.Spec.JobTemplate.Spec, job.Spec, "the schedule's own job: the same script and mounts")
	assert.True(t, logged(out, "backups: started a backup on request", "request", "one", "job", job.Name), out)
	b := e.backup()
	assert.Equal(t, job.Name, b.Running)
	assert.Equal(t, job.Name, b.Job)
	assert.Empty(t, b.Request, "it runs: not answered yet")
	assert.Equal(t, job.Name+" runs the first backup", e.backupCondition().Message)
	assert.True(t, e.suspended())

	e.pass()
	assert.Len(t, e.requested(), 1, "started once")

	e.endBackup(job.Name, 0, okSummary)
	out, _ = e.pass()
	b = e.backup()
	assert.Equal(t, "one", b.Request, "answered once it has ended and been read")
	assert.Empty(t, b.Running)
	assert.Equal(t, "2026-10-04T00-00-05Z", b.LastDump)
	require.NotNil(t, b.LastSuccess)
	assert.True(t, e.clock.Add(-10*time.Second).Equal(b.LastSuccess.Time), "when it ended")
	c := e.backupCondition()
	assert.Equal(t, metav1.ConditionTrue, c.Status)
	assert.Equal(t, "Succeeded", c.Reason)
	assert.Equal(t, "2026-10-04T00-00-05Z: 4 databases, 12.0 MiB; 2 dumps kept, 4.0 GiB free", c.Message, "that run's")
	assert.True(t, logged(out, "backups: the backup asked for has ended", "request", "one", "job", job.Name, "result", "succeeded"), out)
	assert.True(t, e.suspended(), "the pass that answers it was rendered before the answer")

	out, _ = e.pass()
	assert.False(t, e.suspended(), "answered: the schedule moves again")
	assert.Len(t, e.requested(), 1, "nothing more starts, and the one that has ended is kept")
	assert.Equal(t, "one", e.backup().Request)
	assert.NotContains(t, out, "has ended", "answered once")
}

// R.C11: status.backup.running names a run of the schedule while it runs and
// is empty once it has ended; the Backup condition's status and reason stay
// what the last backup that ended says meanwhile.
func TestRC11TheRunningFieldFollowsTheSchedulesRun(t *testing.T) {
	e := newRequestPlatform(t, verifyCR())
	e.pass()
	e.scheduledRun("zaentrum-backup-29324160")
	e.endBackup("zaentrum-backup-29324160", 0, okSummary)
	e.pass()
	assert.Empty(t, e.backup().Running)
	require.Equal(t, "Succeeded", e.backupCondition().Reason)

	e.scheduledRun("zaentrum-backup-29325600")
	e.pass()
	assert.Equal(t, "zaentrum-backup-29325600", e.backup().Running)
	c := e.backupCondition()
	assert.Equal(t, metav1.ConditionTrue, c.Status, "a run does not move the condition")
	assert.Equal(t, "Succeeded", c.Reason)

	e.endBackup("zaentrum-backup-29325600", 1, "cannot dump the database katalog")
	e.pass()
	assert.Empty(t, e.backup().Running)
	assert.Equal(t, "Failed", e.backupCondition().Reason)
}

// R.C11: a backup asked for while the schedule's run runs waits for it, and
// is never refused for it: that run, which began before the request, is read
// but does not answer it. The backup asked for starts on the pass that finds
// it ended, and answers the request when it has ended itself.
func TestRC11ABackupAskedForWaitsForABackupThatRuns(t *testing.T) {
	e := newRequestPlatform(t, verifyCR())
	e.pass()
	e.scheduledRun("zaentrum-backup-29324160")
	e.askBackup("one")
	e.pass()
	out, _ := e.pass()
	assert.True(t, e.suspended())
	assert.Empty(t, e.requested(), "not while a backup runs")
	assert.True(t, logged(out, "backups: the backup asked for waits", "request", "one", "for", "zaentrum-backup-29324160"), out)
	assert.Equal(t, "zaentrum-backup-29324160", e.backup().Running)
	assert.Empty(t, e.backup().Request, "waits, not refused")

	e.endBackup("zaentrum-backup-29324160", 0, okSummary)
	e.pass()
	b := e.backup()
	assert.Empty(t, b.Request, "a backup that began before the request does not answer it")
	assert.NotNil(t, b.LastSuccess, "it is read")
	jobs := e.requested()
	require.Len(t, jobs, 1, "the one asked for starts once it has ended")
	assert.Equal(t, jobs[0].Name, b.Running)

	e.endBackup(jobs[0].Name, 1, "cannot dump the database katalog")
	e.pass()
	b = e.backup()
	assert.Equal(t, "one", b.Request, "answered: it ended, if not well")
	c := e.backupCondition()
	assert.Equal(t, "Failed", c.Reason)
	assert.Contains(t, c.Message, jobs[0].Name+" failed: cannot dump the database katalog; the latest that succeeded wrote 2026-10-04T00-00-05Z")
}

// R.C11: a backup asked for waits while a restore stops the platform or has it
// stopped, through the pass that reads the restore's end, and starts on the
// next.
func TestRC11ABackupAskedForWaitsForARestore(t *testing.T) {
	z := verifyCR()
	z.Annotations = map[string]string{zaentrumv1alpha1.RestoreRequestAnnotation: aDump}
	e := newRequestPlatform(t, z)
	e.pass()
	rs := e.zaentrum().Status.Backup.Restore
	require.Equal(t, zaentrumv1alpha1.RestoreRunning, rs.Result)
	e.askBackup("one")
	out, _ := e.pass()
	out2, _ := e.pass()
	assert.Empty(t, e.requested(), "not while a restore runs")
	assert.True(t, logged(out+out2, "backups: the backup asked for waits", "request", "one", "for", "the restore of "+aDump), out2)

	var job batchv1.Job
	require.NoError(t, e.c.Get(context.Background(), types.NamespacedName{Namespace: verifyNS, Name: rs.Job}, &job))
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	require.NoError(t, e.c.Status().Update(context.Background(), &job))
	e.pass()
	require.Equal(t, zaentrumv1alpha1.RestoreSucceeded, e.zaentrum().Status.Backup.Restore.Result)
	assert.Empty(t, e.requested(), "the pass that reads the restore's end still has the platform stopped")

	e.pass()
	assert.Len(t, e.requested(), 1, "the restore has ended: the backup starts")
}

// R.C11: where backups are off, a backup asked for is answered and refused at
// once, and the Backup condition says so, in the Words, while the annotation
// holds that value and no backup carries it. A backup asked for that runs when
// backups are turned off runs on, and answers its request when it has ended;
// the condition then does not say it was refused.
func TestRC11ABackupAskedForWhileBackupsAreOffIsRefused(t *testing.T) {
	const off = "the bundled Postgres is not backed up: spec.backup.enabled is false"
	no := false
	z := verifyCR()
	z.Spec.Backup.Enabled = &no
	e := newRequestPlatform(t, z)
	e.pass()
	e.askBackup("one")
	out, _ := e.pass()
	assert.Equal(t, "one", e.backup().Request, "answered at once")
	assert.Empty(t, e.backupJobs())
	c := e.backupCondition()
	assert.Equal(t, "Disabled", c.Reason)
	assert.Equal(t, off+"; the backup asked for is not made: set spec.backup.enabled to true and ask again with a new value", c.Message)
	assert.True(t, logged(out, "backups: the backup asked for is refused: backups are off", "request", "one"), out)
	e.pass()
	assert.Equal(t, off+"; the backup asked for is not made: set spec.backup.enabled to true and ask again with a new value",
		e.backupCondition().Message, "for as long as the annotation holds the value")

	e.change(func(z *zaentrumv1alpha1.Zaentrum) { delete(z.Annotations, zaentrumv1alpha1.BackupRequestAnnotation) })
	e.pass()
	assert.Equal(t, off, e.backupCondition().Message)

	// On again, a backup asked for runs; turned off while it runs, it runs on.
	e.change(func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.Backup.Enabled = nil })
	e.pass()
	e.askBackup("two")
	e.pass()
	e.pass()
	jobs := e.requested()
	require.Len(t, jobs, 1)
	e.change(func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.Backup.Enabled = &no })
	e.pass()
	require.Len(t, e.requested(), 1, "turning backups off does not stop it")
	b := e.backup()
	assert.Equal(t, "one", b.Request, "the one asked for runs: not answered, nor refused")
	assert.Equal(t, jobs[0].Name, b.Running)
	assert.Equal(t, off, e.backupCondition().Message, "a backup carries the value: nothing was refused")

	e.endBackup(jobs[0].Name, 0, okSummary)
	e.pass()
	b = e.backup()
	assert.Equal(t, "two", b.Request)
	assert.Empty(t, b.Running)
	assert.Equal(t, "2026-10-04T00-00-05Z", b.LastDump)
	assert.Equal(t, off, e.backupCondition().Message)
}

// R.C11: while the bundled Postgres keeps its data on an emptyDir, a backup
// asked for is refused, with the second sentence's ending.
func TestRC11ABackupAskedForOnAnEmptyDirIsRefused(t *testing.T) {
	e := newBackupEnv(t, verifyCR())
	e.z.Annotations = map[string]string{zaentrumv1alpha1.BackupRequestAnnotation: "one"}
	v := templates.NewValues(e.z)
	v.PostgresVolume = "emptyDir"
	v.BackupSuspend = true
	objs, err := templates.Render(v)
	require.NoError(t, err)
	platform, _ := templates.SplitHooks(objs)
	e.r.reportBackups(context.Background(), e.z, platform)
	require.NotNil(t, e.z.Status.Backup)
	assert.Equal(t, "one", e.z.Status.Backup.Request)
	assert.Equal(t, "the bundled Postgres keeps its data on an emptyDir and is not backed up: backups start once it is on "+
		"its claim (spec.storage.postgres.migrate), or with spec.backup.enabled true; the backup asked for is not made",
		backupCondition(e.z).Message)
}

// R.C11: with external databases a backup asked for is answered and refused
// at once, and no Backup condition says so: status.backup keeps the request
// beside the restore, pass after pass, and the log says why.
func TestRC11ABackupAskedForWithExternalDatabasesIsRefusedWithoutACondition(t *testing.T) {
	z := verifyCR()
	z.Spec.Databases.Mode = "external"
	z.Spec.Databases.External.Host = "postgres.example.com"
	z.Annotations = map[string]string{zaentrumv1alpha1.BackupRequestAnnotation: "one"}
	refused := &zaentrumv1alpha1.RestoreStatus{Request: aDump, Result: zaentrumv1alpha1.RestoreRefused}
	z.Status.Backup = &zaentrumv1alpha1.BackupStatus{Restore: refused}
	e := newBackupEnv(t, z)
	var buf bytes.Buffer
	ctx := log.IntoContext(context.Background(), zap.New(zap.WriteTo(&buf)))
	for pass := 1; pass <= 2; pass++ {
		e.r.reportBackups(ctx, e.z, e.platform)
		require.NotNil(t, e.z.Status.Backup, "pass %d", pass)
		assert.Equal(t, "one", e.z.Status.Backup.Request, "pass %d: the request is kept", pass)
		assert.Equal(t, refused, e.z.Status.Backup.Restore, "pass %d: beside the restore", pass)
		assert.Nil(t, backupCondition(e.z), "pass %d: no Backup condition", pass)
	}
	assert.True(t, logged(buf.String(), "backups: the backup asked for is refused: the databases are external", "request", "one"), buf.String())
	assert.Equal(t, 1, strings.Count(buf.String(), "refused"), "answered once")
	assert.False(t, e.r.startBackupRequest(ctx, e.z, e.platform, backupAsk{request: "two", suspended: true}, ""), "nothing waits")
	var jobs batchv1.JobList
	require.NoError(t, e.c.List(ctx, &jobs))
	assert.Empty(t, jobs.Items, "and nothing starts")
}

// R.C11: an operator that starts again finds the Job a request started by its
// annotation: it starts no second one, and answers the request from it.
func TestRC11ARestartedOperatorFindsTheJobItsRequestStarted(t *testing.T) {
	e := newRequestPlatform(t, verifyCR())
	e.askBackup("one")
	e.pass()
	e.pass()
	require.Len(t, e.requested(), 1)
	name := e.requested()[0].Name

	e.r = &ZaentrumReconciler{Client: e.c, Scheme: e.r.Scheme, Now: e.r.Now}
	e.pass()
	assert.Len(t, e.requested(), 1, "no second backup")
	assert.Equal(t, name, e.backup().Running)
	e.endBackup(name, 0, okSummary)
	e.pass()
	assert.Equal(t, "one", e.backup().Request)
}

// R.C11: the newest Job asked for that has ended is kept; the ones before it
// go once that one has been read, never a run of the schedule's. While the
// next one runs, the one before stays, and so does the Backup condition.
func TestRC11TheNewestJobAskedForThatHasEndedIsKept(t *testing.T) {
	e := newRequestPlatform(t, verifyCR())
	e.pass()
	e.scheduledRun("zaentrum-backup-29324160")
	e.endBackup("zaentrum-backup-29324160", 0, okSummary)
	e.askBackup("one")
	e.pass()
	e.pass()
	first := e.requested()
	require.Len(t, first, 1)
	e.endBackup(first[0].Name, 0, okSummary)
	e.pass()
	require.Equal(t, "one", e.backup().Request)
	assert.Len(t, e.requested(), 1, "kept once it has ended")

	second := e.runAsked("two")
	require.Len(t, e.requested(), 2, "the one before stays while the next runs")
	e.endBackup(second.Name, 0, okSummary)
	e.pass()
	jobs := e.requested()
	require.Len(t, jobs, 1, "the one before goes once the next has been read")
	assert.Equal(t, second.Name, jobs[0].Name)
	names := []string{}
	for _, j := range e.backupJobs() {
		names = append(names, j.Name)
	}
	assert.Contains(t, names, "zaentrum-backup-29324160", "the schedule's run is the CronJob's to keep")
}

// R.C11: while the resource is refused, a backup asked for waits, unanswered,
// and nothing starts; once it is corrected, the backup starts.
func TestRC11ABackupAskedForWhileTheResourceIsRefusedWaits(t *testing.T) {
	e := newRequestPlatform(t, verifyCR())
	e.pass()
	e.change(func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.Databases.Chino = "chi no" })
	e.askBackup("one")
	e.pass()
	e.pass()
	assert.Empty(t, e.backupJobs())
	assert.Empty(t, e.backup().Request, "waits, unanswered")
	assert.False(t, e.suspended(), "a refused pass applies nothing")

	e.change(func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.Databases.Chino = "chino" })
	e.pass()
	e.pass()
	assert.Len(t, e.requested(), 1)
}

// R.C11: the backups' Jobs, the CronJob's runs and the ones asked for, are
// watched for the platform beside them, and of the Jobs the manager's cache
// holds theirs alone.
func TestRC11TheBackupsJobsAreWatchedForThePlatform(t *testing.T) {
	s := selfScheme(t)
	other := verifyCR()
	other.Namespace = "elsewhere"
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(verifyCR(), other).Build()
	r := &ZaentrumReconciler{Client: c, Scheme: s}
	job := func(lbls map[string]string) *metav1.PartialObjectMetadata {
		j := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: "zaentrum-backup-29324160", Namespace: verifyNS, Labels: lbls}}
		j.SetGroupVersionKind(batchv1.SchemeGroupVersion.WithKind("Job"))
		return j
	}
	got := r.platformsOfBackupJob(context.Background(), job(map[string]string{labelDatabase: databaseBackup}))
	assert.Equal(t, []ctrl.Request{{NamespacedName: types.NamespacedName{Namespace: verifyNS, Name: "zaentrum"}}}, got)
	assert.Empty(t, r.platformsOfBackupJob(context.Background(), job(map[string]string{labelDatabase: databaseRestore})))
	assert.Empty(t, r.platformsOfBackupJob(context.Background(), job(nil)))

	opts := CacheOptions()
	require.Len(t, opts.ByObject, 1)
	for obj, by := range opts.ByObject {
		assert.IsType(t, &batchv1.Job{}, obj)
		assert.True(t, by.Label.Matches(labels.Set{labelDatabase: databaseBackup}))
		assert.False(t, by.Label.Matches(labels.Set{labelDatabase: databaseRestore}))
		assert.False(t, by.Label.Matches(labels.Set{labelVerification: verificationRun}))
		assert.False(t, by.Label.Matches(labels.Set{}))
	}
}

// carrying lists the backups' Jobs that carry the value.
func (e *platformEnv) carrying(value string) []batchv1.Job {
	e.t.Helper()
	var out []batchv1.Job
	for _, j := range e.backupJobs() {
		if j.Annotations[zaentrumv1alpha1.BackupRequestAnnotation] == value {
			out = append(out, j)
		}
	}
	return out
}

// dropAsk removes zaentrum.io/backup-request.
func (e *platformEnv) dropAsk() {
	e.t.Helper()
	e.change(func(z *zaentrumv1alpha1.Zaentrum) { delete(z.Annotations, zaentrumv1alpha1.BackupRequestAnnotation) })
}

// runAsked asks for a backup with that value and takes the passes up to its
// Job, which runs.
func (e *platformEnv) runAsked(value string) batchv1.Job {
	e.t.Helper()
	e.askBackup(value)
	e.pass()
	e.pass()
	jobs := e.carrying(value)
	require.Len(e.t, jobs, 1, "the Job of %q", value)
	done, _ := jobFinished(&jobs[0])
	require.False(e.t, done)
	return jobs[0]
}

// R.C11: the schedule stands still for as long as a backup asked for runs,
// whatever becomes of its request meanwhile: the annotation removed; set back
// to the value already answered while a newer value's Job runs; backups turned
// off with another value, which is refused and so answered, and on again. It
// moves again once that Job has ended.
func TestRC11TheScheduleStandsStillWhileABackupAskedForRuns(t *testing.T) {
	t.Run("the annotation removed", func(t *testing.T) {
		e := newRequestPlatform(t, verifyCR())
		e.pass()
		job := e.runAsked("one")
		e.dropAsk()
		e.pass()
		assert.True(t, e.suspended(), "its Job runs")
		e.endBackup(job.Name, 0, okSummary)
		e.pass()
		assert.False(t, e.suspended(), "its Job has ended")
	})
	t.Run("set back to the value answered", func(t *testing.T) {
		e := newRequestPlatform(t, verifyCR())
		e.pass()
		one := e.runAsked("one")
		e.endBackup(one.Name, 0, okSummary)
		e.pass()
		require.Equal(t, "one", e.backup().Request)
		two := e.runAsked("two")
		e.askBackup("one")
		e.pass()
		assert.True(t, e.suspended(), "two's Job runs")
		e.endBackup(two.Name, 0, okSummary)
		e.pass()
		assert.False(t, e.suspended())
	})
	t.Run("off with another value, and on again", func(t *testing.T) {
		no := false
		e := newRequestPlatform(t, verifyCR())
		e.pass()
		one := e.runAsked("one")
		e.change(func(z *zaentrumv1alpha1.Zaentrum) {
			z.Spec.Backup.Enabled = &no
			z.Annotations[zaentrumv1alpha1.BackupRequestAnnotation] = "two"
		})
		e.pass()
		require.Equal(t, "two", e.backup().Request, "refused, so answered")
		e.change(func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.Backup.Enabled = nil })
		e.pass()
		assert.True(t, e.suspended(), "applied suspended: one's Job runs")
		e.endBackup(one.Name, 0, okSummary)
		e.pass()
		assert.False(t, e.suspended())
	})
	t.Run("the backups' Jobs cannot be listed", func(t *testing.T) {
		fail := false
		e := newPlatformEnv(t, verifyCR(), &interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			lo := &client.ListOptions{}
			lo.ApplyOptions(opts)
			if _, ok := list.(*batchv1.JobList); ok && fail && lo.LabelSelector != nil && lo.LabelSelector.String() == labelDatabase+"="+databaseBackup {
				return fmt.Errorf("the API server is busy")
			}
			return c.List(ctx, list, opts...)
		}})
		e.reconcile()
		require.False(t, e.suspended())
		fail = true
		e.reconcile()
		assert.True(t, e.suspended(), "whether a backup asked for runs is not known")
		fail = false
		e.reconcile()
		assert.False(t, e.suspended())
	})
}

// R.C11: starting the next backup asked for deletes nothing and moves no
// condition: while it runs the Backup condition says what the last one that
// ended said, with a failed run of the schedule's before it or with none; a
// value changed while a Job runs has that Job read when it ends; and a Job
// that cannot be created leaves the one before it and the condition as they
// were.
func TestRC11TheNextBackupAskedForMovesNothingBeforeItEnds(t *testing.T) {
	t.Run("a failed run of the schedule's before", func(t *testing.T) {
		e := newRequestPlatform(t, verifyCR())
		e.pass()
		e.scheduledRun("zaentrum-backup-29324160")
		e.endBackup("zaentrum-backup-29324160", 1, "cannot dump the database katalog")
		e.pass()
		one := e.runAsked("one")
		e.endBackup(one.Name, 0, okSummary)
		e.pass()
		before := *e.backupCondition()
		require.Equal(t, "Succeeded", before.Reason)
		e.runAsked("two")
		c := e.backupCondition()
		assert.Equal(t, before.Status, c.Status)
		assert.Equal(t, before.Reason, c.Reason)
		assert.Equal(t, before.Message, c.Message)
	})
	t.Run("no run of the schedule's", func(t *testing.T) {
		e := newRequestPlatform(t, verifyCR())
		e.pass()
		one := e.runAsked("one")
		e.endBackup(one.Name, 0, okSummary)
		e.pass()
		e.runAsked("two")
		c := e.backupCondition()
		assert.Equal(t, "Succeeded", c.Reason, "not the first backup: %s", c.Message)
	})
	t.Run("the value changed while its Job runs", func(t *testing.T) {
		e := newRequestPlatform(t, verifyCR())
		e.pass()
		one := e.runAsked("one")
		e.askBackup("two")
		e.pass()
		assert.Len(t, e.carrying("two"), 0, "two waits for one's Job")
		e.endBackup(one.Name, 0, okSummary)
		e.pass()
		b := e.backup()
		assert.Equal(t, "2026-10-04T00-00-05Z", b.LastDump, "one's run is read")
		require.NotNil(t, b.LastSuccess)
		assert.Len(t, e.carrying("one"), 1, "and kept")
		assert.Len(t, e.carrying("two"), 1, "two starts once it has ended")
	})
	t.Run("the next Job cannot be created", func(t *testing.T) {
		fail := false
		var e *platformEnv
		e = newPlatformEnv(t, verifyCR(), &interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if j, ok := obj.(*unstructured.Unstructured); ok && j.GetKind() == "Job" && fail &&
				j.GetAnnotations()[zaentrumv1alpha1.BackupRequestAnnotation] != "" {
				return fmt.Errorf("exceeded quota")
			}
			if created := obj.GetCreationTimestamp(); created.IsZero() {
				obj.SetCreationTimestamp(metav1.NewTime(e.clock))
			}
			return c.Create(ctx, obj, opts...)
		}})
		e.pass()
		one := e.runAsked("one")
		e.endBackup(one.Name, 0, okSummary)
		e.pass()
		before := *e.backupCondition()
		fail = true
		e.askBackup("two")
		e.pass()
		out, _ := e.pass()
		assert.True(t, logged(out, "backups: the backup asked for waits", "request", "two", "for", "its Job to be created"), out)
		assert.Len(t, e.carrying("one"), 1, "the one before is kept")
		assert.Equal(t, before.Message, e.backupCondition().Message)
		assert.Empty(t, e.carrying("two"))
	})
}

// R.C11: a run whose pod cannot be listed on the pass that finds it ended is
// not read on that pass: the status stays as it is, the request has no
// answer, and the schedule stands still, so no run of it can end first; the
// next pass reads it, and answers, and the schedule moves on the pass after.
func TestRC11ARunNotReadIsReadOnTheNextPass(t *testing.T) {
	failPods := false
	var e *platformEnv
	e = newPlatformEnv(t, verifyCR(), &interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if created := obj.GetCreationTimestamp(); created.IsZero() {
				obj.SetCreationTimestamp(metav1.NewTime(e.clock))
			}
			return c.Create(ctx, obj, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1.PodList); ok && failPods {
				return fmt.Errorf("the API server is busy")
			}
			return c.List(ctx, list, opts...)
		},
	})
	e.pass()
	e.scheduledRun("zaentrum-backup-29324160")
	e.endBackup("zaentrum-backup-29324160", 0, strings.ReplaceAll(okSummary, "2026-10-04T00-00-05Z", "2026-10-03T00-00-04Z"))
	e.pass()
	before := e.backup()
	require.Equal(t, "2026-10-03T00-00-04Z", before.LastDump)
	job := e.runAsked("one")
	e.endBackup(job.Name, 0, okSummary)
	failPods = true
	out, _ := e.pass()
	b := e.backup()
	assert.True(t, logged(out, "backups: a run could not be read; status stays as it is", "job", job.Name), out)
	assert.Empty(t, b.Request, "not read: no answer")
	assert.Equal(t, before.LastSuccess, b.LastSuccess)
	assert.True(t, e.suspended(), "the schedule stands still until the answer")
	failPods = false
	e.pass()
	b = e.backup()
	assert.Equal(t, "one", b.Request)
	assert.Equal(t, "2026-10-04T00-00-05Z", b.LastDump, "its own set")
	assert.Equal(t, "2026-10-04T00-00-05Z: 4 databases, 12.0 MiB; 2 dumps kept, 4.0 GiB free", e.backupCondition().Message)
	assert.True(t, e.suspended())
	e.pass()
	assert.False(t, e.suspended(), "answered: the schedule moves")
}

// R.C11: a Job asked for that is gone before it was read answers its request
// as not read, with its own line in the log; the Backup condition keeps what
// it said, and no second backup starts for it.
func TestRC11AJobGoneBeforeItWasReadIsAnsweredAsNotRead(t *testing.T) {
	e := newRequestPlatform(t, verifyCR())
	e.pass()
	e.scheduledRun("zaentrum-backup-29324160")
	e.endBackup("zaentrum-backup-29324160", 0, okSummary)
	e.pass()
	before := *e.backupCondition()
	job := e.runAsked("one")
	e.endBackup(job.Name, 0, okSummary)
	require.NoError(t, e.c.Delete(context.Background(), &job, client.PropagationPolicy(metav1.DeletePropagationBackground)))
	out, _ := e.pass()
	assert.True(t, logged(out, "backups: the backup asked for is answered as not read: its Job is gone before it was read",
		"request", "one", "job", job.Name), out)
	assert.Equal(t, "one", e.backup().Request)
	c := e.backupCondition()
	assert.Equal(t, before.Reason, c.Reason)
	assert.Equal(t, before.Message, c.Message)
	for pass := 0; pass < 3; pass++ {
		e.pass()
	}
	assert.Empty(t, e.requested(), "no second backup")
	assert.False(t, e.suspended())
}

// R.C11: where the status cannot keep status.backup.request (a CRD older than
// the operator), the ended Job that carries the value is its answer: the
// schedule moves again on the pass after the answer, no second backup starts,
// "has ended" is logged once, and that the status did not keep the answer,
// once.
func TestRC11AnEndedJobAnswersWhereTheStatusCannotKeepTheField(t *testing.T) {
	e := newRequestPlatform(t, verifyCR())
	e.c = interceptor.NewClient(e.c, interceptor.Funcs{
		Patch: applyAsCreateOrUpdate,
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if created := obj.GetCreationTimestamp(); created.IsZero() {
				obj.SetCreationTimestamp(metav1.NewTime(e.clock))
			}
			return c.Create(ctx, obj, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if z, ok := obj.(*zaentrumv1alpha1.Zaentrum); ok && z.Status.Backup != nil {
				z.Status.Backup.Request, z.Status.Backup.Running = "", ""
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	e.r = &ZaentrumReconciler{Client: e.c, Scheme: e.r.Scheme, Now: e.r.Now}
	e.pass()
	job := e.runAsked("one")
	e.endBackup(job.Name, 0, okSummary)
	all, _ := e.pass()
	assert.True(t, e.suspended(), "the pass that answers it")
	for pass := 1; pass < 4; pass++ {
		out, _ := e.pass()
		all += out
		assert.False(t, e.suspended(), "pass %d: the schedule moves", pass)
	}
	assert.Len(t, e.requested(), 1, "no second backup")
	assert.Equal(t, 1, strings.Count(all, "backups: the backup asked for has ended"), all)
	assert.Equal(t, 1, strings.Count(all, "the status did not keep the answer to the backup asked for"), all)
}

// R.C11: a pass that refuses the resource clears status.backup.running once
// the Job it names has ended.
func TestRC11ARefusedPassClearsTheRunOnceItHasEnded(t *testing.T) {
	e := newRequestPlatform(t, verifyCR())
	e.pass()
	job := e.runAsked("one")
	e.change(func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.Databases.Chino = "chi no" })
	e.pass()
	require.Equal(t, job.Name, e.backup().Running)
	e.endBackup(job.Name, 0, okSummary)
	e.pass()
	assert.Empty(t, e.backup().Running)
	assert.Empty(t, e.backup().Request, "answered once the resource is corrected")
	e.change(func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.Databases.Chino = "chino" })
	e.pass()
	assert.Equal(t, "one", e.backup().Request)
}

// R.C11: the log shows at most 64 characters of a value; the Job carries it
// whole.
func TestRC11TheLogShowsAtMost64CharactersOfAValue(t *testing.T) {
	long := strings.Repeat("x", 70)
	e := newRequestPlatform(t, verifyCR())
	e.pass()
	e.askBackup(long)
	out, _ := e.pass()
	out2, _ := e.pass()
	assert.NotContains(t, out+out2, long[:65])
	assert.True(t, logged(out2, "backups: started a backup on request", "request", long[:63]+"…"), out2)
	require.Len(t, e.carrying(long), 1, "the Job carries the value whole")
}

// R.C11: the whole flow over a cached client that holds no Job, CronJob or
// Pod: every read of them goes to the API server.
func TestRC11EveryJobReadGoesToTheAPIServer(t *testing.T) {
	e := newRequestPlatform(t, verifyCR())
	refuse := func(what any) error { return fmt.Errorf("%T read through the cache", what) }
	cached := interceptor.NewClient(e.c, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			switch obj.(type) {
			case *batchv1.Job, *batchv1.CronJob, *corev1.Pod:
				return refuse(obj)
			}
			return c.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			switch list.(type) {
			case *batchv1.JobList, *batchv1.CronJobList, *corev1.PodList:
				return refuse(list)
			}
			return c.List(ctx, list, opts...)
		},
		Patch: applyAsCreateOrUpdate,
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if created := obj.GetCreationTimestamp(); created.IsZero() {
				obj.SetCreationTimestamp(metav1.NewTime(e.clock))
			}
			return c.Create(ctx, obj, opts...)
		},
	})
	e.r = &ZaentrumReconciler{Client: cached, APIReader: e.c, Scheme: e.r.Scheme, Now: e.r.Now}
	all := ""
	pass := func() {
		out, _ := e.pass()
		all += out
	}
	pass()
	e.scheduledRun("zaentrum-backup-29325600")
	e.askBackup("one")
	pass()
	pass()
	e.endBackup("zaentrum-backup-29325600", 0, okSummary)
	pass()
	jobs := e.requested()
	require.Len(t, jobs, 1)
	e.endBackup(jobs[0].Name, 0, okSummary)
	pass()
	pass()
	assert.NotContains(t, all, "read through the cache")
	assert.Equal(t, "one", e.backup().Request)
	assert.False(t, e.suspended())
}

// R.C11: through Reconcile, with external databases a backup asked for is
// answered at once with no Backup condition, and on an emptyDir it is refused
// with the second sentence's ending; nothing starts for either.
func TestRC11RefusedRequestsThroughReconcile(t *testing.T) {
	z := verifyCR()
	z.Spec.Databases.Mode = "external"
	z.Spec.Databases.External.Host = "postgres.example.com"
	z.Spec.Databases.External.SSLMode = "require"
	e := newRequestPlatform(t, z)
	e.pass()
	e.askBackup("one")
	out, _ := e.pass()
	assert.True(t, logged(out, "backups: the backup asked for is refused: the databases are external", "request", "one"), out)
	assert.Equal(t, "one", e.backup().Request)
	assert.Nil(t, e.backupCondition())
	out, _ = e.pass()
	assert.NotContains(t, out, "refused", "answered once")
	assert.Empty(t, e.backupJobs())

	e = newRequestPlatform(t, verifyCR(), livePostgres("emptyDir", 1))
	e.pass()
	e.askBackup("one")
	out, _ = e.pass()
	assert.True(t, logged(out, "backups: the backup asked for is refused: backups are off", "request", "one"), out)
	assert.Equal(t, "one", e.backup().Request)
	assert.Equal(t, "the bundled Postgres keeps its data on an emptyDir and is not backed up: backups start once it is on "+
		"its claim (spec.storage.postgres.migrate), or with spec.backup.enabled true; the backup asked for is not made",
		e.backupCondition().Message)
	e.dropAsk()
	e.pass()
	assert.NotContains(t, e.backupCondition().Message, "asked for")
	assert.Empty(t, e.backupJobs())
}

// registeringInformer is a fake informer that says when a handler was added.
type registeringInformer struct {
	*controllertest.FakeInformer
	registered chan struct{}
}

func (i *registeringInformer) AddEventHandler(h toolscache.ResourceEventHandler) (toolscache.ResourceEventHandlerRegistration, error) {
	reg, err := i.FakeInformer.AddEventHandler(h)
	close(i.registered)
	return reg, err
}

// jobsCache is the manager's cache for the test: fake informers, the Jobs' by
// their metadata the one the test sends events through.
type jobsCache struct {
	*informertest.FakeInformers
	jobs *registeringInformer
}

func (c jobsCache) GetInformer(ctx context.Context, obj client.Object, opts ...cache.InformerGetOption) (cache.Informer, error) {
	if m, ok := obj.(*metav1.PartialObjectMetadata); ok && m.GroupVersionKind() == batchv1.SchemeGroupVersion.WithKind("Job") {
		return c.jobs, nil
	}
	return c.FakeInformers.GetInformer(ctx, obj, opts...)
}

// R.C11: through the manager's setup, an event of a backup Job, by its
// metadata, brings a pass of the platform beside it, which names the run in
// status.backup.running. Nothing reaches an API server: the manager's cache is
// fake, and the reconciler works on the fake client.
func TestRC11AnEventOfABackupJobBringsAPass(t *testing.T) {
	e := newRequestPlatform(t, verifyCR())
	e.pass()
	e.scheduledRun("zaentrum-backup-29324160")
	var job batchv1.Job
	require.NoError(t, e.c.Get(context.Background(), types.NamespacedName{Namespace: verifyNS, Name: "zaentrum-backup-29324160"}, &job))

	ctx, cancel := context.WithCancel(context.Background())
	informers := &informertest.FakeInformers{Scheme: e.r.Scheme}
	// Made before the manager starts, which then only reads them.
	for _, obj := range []client.Object{&zaentrumv1alpha1.Zaentrum{}, &appsv1.Deployment{}} {
		_, err := informers.FakeInformerFor(ctx, obj)
		require.NoError(t, err)
	}
	jobs := &registeringInformer{FakeInformer: &controllertest.FakeInformer{}, registered: make(chan struct{})}
	mgr, err := ctrl.NewManager(&rest.Config{Host: "https://127.0.0.1:1"}, ctrl.Options{
		Scheme:     e.r.Scheme,
		Metrics:    metricsserver.Options{BindAddress: "0"},
		Controller: config.Controller{SkipNameValidation: ptrTo(true)},
		Cache:      CacheOptions(),
		NewCache: func(*rest.Config, cache.Options) (cache.Cache, error) {
			return jobsCache{FakeInformers: informers, jobs: jobs}, nil
		},
	})
	require.NoError(t, err)
	r := &ZaentrumReconciler{Client: e.c, Scheme: e.r.Scheme, Now: e.r.Now}
	require.NoError(t, r.SetupWithManager(mgr))
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		assert.NoError(t, mgr.Start(ctx))
	}()
	defer func() {
		cancel()
		<-stopped
	}()

	select {
	case <-jobs.registered:
	case <-time.After(30 * time.Second):
		t.Fatal("the manager watches no Jobs by their metadata")
	}
	require.Empty(t, e.backup().Running)
	meta := &metav1.PartialObjectMetadata{ObjectMeta: job.ObjectMeta}
	meta.SetGroupVersionKind(batchv1.SchemeGroupVersion.WithKind("Job"))
	jobs.Add(meta)
	assert.Eventually(t, func() bool { return e.backup().Running == job.Name }, 30*time.Second, 50*time.Millisecond,
		"the event brings a pass that names the run")
}

// R.C11: a Job asked for that is being deleted and has not ended holds the
// schedule still, and status.backup.running names it, until its object is
// gone; it is not read, so its request is answered as not read.
func TestRC11AJobAskedForBeingDeletedHoldsTheSchedule(t *testing.T) {
	ctx := context.Background()
	e := newRequestPlatform(t, verifyCR())
	e.pass()
	job := e.runAsked("one")
	job.Finalizers = []string{"example.org/hold"}
	require.NoError(t, e.c.Update(ctx, &job))
	require.NoError(t, e.c.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground)))
	out, _ := e.pass()
	assert.True(t, logged(out, "backups: the backup asked for is answered as not read: its Job is gone before it was read",
		"request", "one", "job", job.Name), out)
	for pass := 0; pass < 2; pass++ {
		assert.True(t, e.suspended(), "pass %d: its pod may still write", pass)
		assert.Equal(t, job.Name, e.backup().Running, "pass %d", pass)
		e.pass()
	}
	assert.Empty(t, e.carrying("two"))

	require.NoError(t, e.c.Get(ctx, types.NamespacedName{Namespace: verifyNS, Name: job.Name}, &job))
	job.Finalizers = nil
	require.NoError(t, e.c.Update(ctx, &job))
	e.pass()
	assert.False(t, e.suspended(), "gone: the schedule moves")
	assert.Empty(t, e.backup().Running)
	assert.Empty(t, e.requested(), "and no second backup starts")
}

// R.C11: an answer counts once the status that holds it was written. A status
// write that fails on the answering pass is no status that cannot keep the
// field: the next pass answers again, without saying so; and where the status
// does drop the field later, that is said, once.
func TestRC11AFailedStatusWriteIsNotAStatusThatCannotKeepTheField(t *testing.T) {
	fail, prune := false, false
	e := newRequestPlatform(t, verifyCR())
	e.c = interceptor.NewClient(e.c, interceptor.Funcs{
		Patch: applyAsCreateOrUpdate,
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if created := obj.GetCreationTimestamp(); created.IsZero() {
				obj.SetCreationTimestamp(metav1.NewTime(e.clock))
			}
			return c.Create(ctx, obj, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			z, ok := obj.(*zaentrumv1alpha1.Zaentrum)
			switch {
			case ok && fail:
				fail = false
				return apierrors.NewConflict(schema.GroupResource{Group: "zaentrum.io", Resource: "zaentrums"}, z.Name,
					fmt.Errorf("the object has been modified"))
			case ok && prune && z.Status.Backup != nil:
				z.Status.Backup.Request, z.Status.Backup.Running = "", ""
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	e.r = &ZaentrumReconciler{Client: e.c, Scheme: e.r.Scheme, Now: e.r.Now}
	e.pass()
	job := e.runAsked("one")
	e.endBackup(job.Name, 0, okSummary)

	var buf bytes.Buffer
	fail = true
	_, err := e.r.Reconcile(log.IntoContext(context.Background(), zap.New(zap.WriteTo(&buf))), ctrl.Request{NamespacedName: e.key})
	require.Error(t, err, "the answering pass's status write fails")
	all := buf.String()
	out, _ := e.pass()
	all += out
	assert.Equal(t, "one", e.backup().Request, "answered again, and written")
	assert.NotContains(t, all, "did not keep", all)
	e.pass()
	assert.False(t, e.suspended())

	prune = true
	two := e.runAsked("two")
	e.endBackup(two.Name, 0, okSummary)
	all = ""
	for pass := 0; pass < 3; pass++ {
		out, _ := e.pass()
		all += out
	}
	assert.Equal(t, 1, strings.Count(all, "the status did not keep the answer to the backup asked for"), all)
	assert.False(t, e.suspended())
	assert.Len(t, e.carrying("two"), 1, "no second backup")
}
