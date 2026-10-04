package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
	"github.com/zaentrum/zaentrum-operator/operator/internal/templates"
)

// backupEnv is a platform on a fake API server, its render, and a clock.
type backupEnv struct {
	t        *testing.T
	c        client.WithWatch
	r        *ZaentrumReconciler
	z        *zaentrumv1alpha1.Zaentrum
	platform []*unstructured.Unstructured
	hooks    []*unstructured.Unstructured
	clock    time.Time
}

func newBackupEnv(t *testing.T, z *zaentrumv1alpha1.Zaentrum, objs ...client.Object) *backupEnv {
	t.Helper()
	s := selfScheme(t)
	e := &backupEnv{t: t, z: z, clock: time.Date(2026, 10, 4, 3, 0, 0, 0, time.UTC)}
	e.c = fake.NewClientBuilder().WithScheme(s).WithObjects(append(objs, z)...).
		WithStatusSubresource(&batchv1.Job{}).Build()
	e.r = &ZaentrumReconciler{Client: e.c, Scheme: s, Now: func() time.Time { return e.clock }}
	e.render("")
	return e
}

// render renders the platform as the reconciler would, restoring dump.
func (e *backupEnv) render(dump string) {
	e.t.Helper()
	v := templates.NewValues(e.z)
	v.RestoreDump = dump
	objs, err := templates.Render(v)
	require.NoError(e.t, err)
	e.platform, e.hooks = templates.SplitHooks(objs)
}

// backupRun makes one of the CronJob's Jobs, ended as the kubelet leaves it:
// exit 0 with the summary, or exit 1 with the reason; running when exit < 0.
func (e *backupEnv) backupRun(name string, created time.Time, exit int32, message string) {
	e.t.Helper()
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: e.z.Namespace,
		CreationTimestamp: metav1.NewTime(created), Labels: map[string]string{labelDatabase: databaseBackup}}}
	require.NoError(e.t, e.c.Create(context.Background(), job))
	if exit < 0 {
		return
	}
	ended := metav1.NewTime(created.Add(time.Minute))
	cond := batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: ended}
	if exit != 0 {
		cond = batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, LastTransitionTime: ended, Reason: "BackoffLimitExceeded"}
	}
	job.Status.Conditions = []batchv1.JobCondition{cond}
	require.NoError(e.t, e.c.Status().Update(context.Background(), job))
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name + "-x", Namespace: e.z.Namespace,
		Labels: map[string]string{batchv1.JobNameLabel: name}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: templates.BackupContainer,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exit, Message: message, FinishedAt: ended}}}}}}
	require.NoError(e.t, e.c.Create(context.Background(), pod))
}

func backupCondition(z *zaentrumv1alpha1.Zaentrum) *metav1.Condition {
	return meta.FindStatusCondition(z.Status.Conditions, condTypeBackup)
}

const okSummary = `{"v":1,"dump":"2026-10-04T00-00-05Z","bytes":12582912,"databases":{"chino":{"tables":4,"bytes":1},"katalog":{"tables":80,"bytes":1},"keycloak":{"tables":90,"bytes":1},"portal":{"tables":5,"bytes":1}},"kept":["2026-10-04T00-00-05Z","2026-10-03T00-00-04Z"],"free":4294967296}`

// status.backup and the Backup condition are what the backups' own Jobs say:
// none yet, one that succeeded — its dump, the dumps kept — and one that
// failed after it, with its reason and the last good dump.
func TestBackupStatusIsWhatTheRunsSay(t *testing.T) {
	e := newBackupEnv(t, verifyCR())
	ctx := context.Background()

	e.r.reportBackups(ctx, e.z, e.platform)
	c := backupCondition(e.z)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionUnknown, c.Status)
	assert.Equal(t, "Scheduled", c.Reason)
	assert.Contains(t, c.Message, "@daily")

	e.backupRun("zaentrum-backup-1", e.clock.Add(-time.Hour), -1, "")
	e.r.reportBackups(ctx, e.z, e.platform)
	assert.Contains(t, backupCondition(e.z).Message, "zaentrum-backup-1 runs the first backup")

	require.NoError(t, e.c.DeleteAllOf(ctx, &batchv1.Job{}, client.InNamespace(e.z.Namespace)))
	e.backupRun("zaentrum-backup-1", e.clock.Add(-time.Hour), 0, okSummary)
	e.r.reportBackups(ctx, e.z, e.platform)
	b := e.z.Status.Backup
	require.NotNil(t, b)
	assert.Equal(t, "2026-10-04T00-00-05Z", b.LastDump)
	assert.Equal(t, []string{"2026-10-04T00-00-05Z", "2026-10-03T00-00-04Z"}, b.Dumps)
	require.NotNil(t, b.LastSuccess)
	assert.True(t, e.clock.Add(-time.Hour+time.Minute).Equal(b.LastSuccess.Time), "when the run ended: %s", b.LastSuccess)
	assert.Nil(t, b.LastFailure)
	assert.Equal(t, "zaentrum-backup-1", b.Job)
	c = backupCondition(e.z)
	assert.Equal(t, metav1.ConditionTrue, c.Status)
	assert.Equal(t, "Succeeded", c.Reason)
	assert.Equal(t, "2026-10-04T00-00-05Z: 4 databases, 12.0 MiB; 2 dumps kept, 4.0 GiB free", c.Message)

	e.backupRun("zaentrum-backup-2", e.clock, 1, "cannot dump the database katalog")
	e.clock = e.clock.Add(2 * time.Minute)
	e.r.reportBackups(ctx, e.z, e.platform)
	b = e.z.Status.Backup
	require.NotNil(t, b.LastFailure)
	assert.Equal(t, "2026-10-04T00-00-05Z", b.LastDump, "the last good dump stays")
	assert.Equal(t, "zaentrum-backup-2", b.Job)
	c = backupCondition(e.z)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, "Failed", c.Reason)
	assert.Contains(t, c.Message, "zaentrum-backup-2 failed: cannot dump the database katalog")
	assert.Contains(t, c.Message, "the latest that succeeded wrote 2026-10-04T00-00-05Z")

	// The CronJob's history goes; what status learnt stays.
	require.NoError(t, e.c.DeleteAllOf(ctx, &batchv1.Job{}, client.InNamespace(e.z.Namespace)))
	e.r.reportBackups(ctx, e.z, e.platform)
	assert.Equal(t, "2026-10-04T00-00-05Z", e.z.Status.Backup.LastDump)
	assert.NotNil(t, e.z.Status.Backup.LastSuccess)
	assert.NotNil(t, e.z.Status.Backup.LastFailure)
}

// Off — turned off, or the Postgres on an emptyDir — the condition says so
// and why, and the schedule this Zaentrum owns is removed (the operator prunes
// nothing else it stops rendering); a CronJob it does not own is left alone.
// External databases: no condition at all.
func TestBackupsTurnedOff(t *testing.T) {
	z := verifyCR()
	no := false
	z.Spec.Backup.Enabled = &no
	owned := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: templates.BackupCronJobName, Namespace: z.Namespace,
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "zaentrum.io/v1alpha1", Kind: "Zaentrum", Name: z.Name, UID: z.UID,
			Controller: ptrTo(true)}}}}
	e := newBackupEnv(t, z, owned)
	ctx := context.Background()
	assert.False(t, rendered(e.platform, "CronJob", templates.BackupCronJobName))
	e.r.reportBackups(ctx, e.z, e.platform)
	c := backupCondition(e.z)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, "Disabled", c.Reason)
	assert.Contains(t, c.Message, "spec.backup.enabled is false")
	err := e.c.Get(ctx, types.NamespacedName{Namespace: z.Namespace, Name: templates.BackupCronJobName}, &batchv1.CronJob{})
	assert.True(t, isGone(err), "the schedule of backups turned off is removed")

	foreign := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: templates.BackupCronJobName, Namespace: z.Namespace}}
	e = newBackupEnv(t, verifyCR(), foreign)
	e.z.Spec.Backup.Enabled = &no
	e.render("")
	e.r.reportBackups(ctx, e.z, e.platform)
	assert.NoError(t, e.c.Get(ctx, types.NamespacedName{Namespace: z.Namespace, Name: templates.BackupCronJobName}, &batchv1.CronJob{}),
		"a CronJob the Zaentrum does not own is not its to remove")

	e = newBackupEnv(t, verifyCR())
	e.render("")
	v := templates.NewValues(e.z)
	v.PostgresVolume = "emptyDir"
	objs, err := templates.Render(v)
	require.NoError(t, err)
	e.r.reportBackups(ctx, e.z, objs)
	assert.Contains(t, backupCondition(e.z).Message, "emptyDir")

	ext := verifyCR()
	ext.Spec.Databases.Mode = "external"
	ext.Spec.Databases.External.Host = "postgres.example.com"
	ext.Status.Backup = &zaentrumv1alpha1.BackupStatus{LastDump: "2026-10-04T00-00-05Z"}
	setCondition(ext, condTypeBackup, metav1.ConditionTrue, "Succeeded", "x")
	e = newBackupEnv(t, ext)
	e.r.reportBackups(ctx, e.z, e.platform)
	assert.Nil(t, backupCondition(e.z))
	assert.Nil(t, e.z.Status.Backup)
}

func ptrTo[T any](v T) *T { return &v }

func isGone(err error) bool { return err != nil && client.IgnoreNotFound(err) == nil }
