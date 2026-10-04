package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
	"github.com/zaentrum/zaentrum-operator/operator/internal/templates"
)

const aDump = "2026-10-04T00-00-05Z"

func (e *backupEnv) askRestore(value string) {
	ann := e.z.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[zaentrumv1alpha1.RestoreRequestAnnotation] = value
	e.z.SetAnnotations(ann)
}

// restorePass takes a pass's restore steps as Reconcile does: the plan before
// the render, the step after it. copying is a database copy in flight.
func (e *backupEnv) restorePass(copying bool) (string, bool) {
	e.t.Helper()
	e.clock = e.clock.Add(10 * time.Second)
	dump := e.r.planRestore(context.Background(), e.z, copying)
	e.render(dump)
	return dump, e.r.stepRestore(context.Background(), e.z, e.platform, e.hooks)
}

// runPods gives every Deployment of the normal render a running pod, as a
// running platform has, and the claim backups.
func (e *backupEnv) runningPlatform() {
	e.t.Helper()
	ctx := context.Background()
	for _, o := range e.platform {
		if o.GetKind() != "Deployment" {
			continue
		}
		labels, _, _ := unstructured.NestedStringMap(o.Object, "spec", "selector", "matchLabels")
		require.NoError(e.t, e.c.Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: o.GetName() + "-abc", Namespace: e.z.Namespace, Labels: labels}}))
	}
	require.NoError(e.t, e.c.Create(ctx, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "backups", Namespace: e.z.Namespace}}))
}

// stopClients deletes the pods of the Deployments the render stopped, as the
// Deployment controller does.
func (e *backupEnv) stopClients() {
	e.t.Helper()
	for _, o := range e.platform {
		if o.GetKind() != "Deployment" {
			continue
		}
		if n, _, _ := unstructured.NestedFieldNoCopy(o.Object, "spec", "replicas"); asCount(n) != 0 {
			continue
		}
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: o.GetName() + "-abc", Namespace: e.z.Namespace}}
		require.NoError(e.t, client.IgnoreNotFound(e.c.Delete(context.Background(), pod)))
	}
}

// endRestore ends the restore Job as the Job controller and kubelet do.
func (e *backupEnv) endRestore(exit int32, message string) {
	e.t.Helper()
	ctx := context.Background()
	var job batchv1.Job
	require.NoError(e.t, e.c.Get(ctx, types.NamespacedName{Namespace: e.z.Namespace, Name: e.z.Status.Backup.Restore.Job}, &job))
	cond := batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}
	if exit != 0 {
		cond = batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"}
	}
	job.Status.Conditions = []batchv1.JobCondition{cond}
	require.NoError(e.t, e.c.Status().Update(ctx, &job))
	require.NoError(e.t, e.c.Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-x", Namespace: e.z.Namespace,
		Labels: map[string]string{batchv1.JobNameLabel: job.Name}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: templates.RestoreContainer,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exit, Message: message}}}}}}))
}

func restoreJobs(t *testing.T, c client.Client, ns string) []batchv1.Job {
	t.Helper()
	var jobs batchv1.JobList
	require.NoError(t, c.List(context.Background(), &jobs, client.InNamespace(ns), client.MatchingLabels{labelDatabase: databaseRestore}))
	return jobs.Items
}

func restoreCondition(z *zaentrumv1alpha1.Zaentrum) *metav1.Condition {
	return meta.FindStatusCondition(z.Status.Conditions, condTypeRestore)
}

// A restore, start to end: asked for with the annotation, it stops every
// client of the database and waits until none has a pod; then the restore Job
// starts, once, from the chart's hook; it is read when it ends; the clients
// start again, the realm Job runs again, and the request is answered — the
// same dump again takes a value of its own.
func TestARestoreStopsTheClientsRestoresAndStartsThemAgain(t *testing.T) {
	e := newBackupEnv(t, verifyCR())
	e.runningPlatform()
	realm0 := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "zaentrum-realm-abcde", Namespace: e.z.Namespace,
		Labels: map[string]string{labelRealm: realmRun}}}
	require.NoError(t, controllerRef(e, realm0))
	require.NoError(t, e.c.Create(context.Background(), realm0))

	dump, inFlight := e.restorePass(false)
	assert.Empty(t, dump, "nothing asked")
	assert.False(t, inFlight)

	e.askRestore(aDump)
	dump, inFlight = e.restorePass(false)
	assert.Equal(t, aDump, dump, "the render stops the clients")
	assert.True(t, inFlight)
	rs := e.z.Status.Backup.Restore
	assert.Equal(t, zaentrumv1alpha1.RestoreStopping, rs.Result)
	assert.Equal(t, aDump, rs.Request)
	assert.Contains(t, rs.Message, "still up: ")
	assert.Contains(t, rs.Message, "chino-api (1 pods)")
	assert.NotContains(t, rs.Message, "chino-web", "the web app is no client of the database")
	assert.Empty(t, restoreJobs(t, e.c, e.z.Namespace), "no restore while a client is up")
	assert.Equal(t, metav1.ConditionUnknown, restoreCondition(e.z).Status)

	e.stopClients()
	dump, inFlight = e.restorePass(false)
	assert.Equal(t, aDump, dump)
	assert.True(t, inFlight)
	rs = e.z.Status.Backup.Restore
	assert.Equal(t, zaentrumv1alpha1.RestoreRunning, rs.Result)
	jobs := restoreJobs(t, e.c, e.z.Namespace)
	require.Len(t, jobs, 1)
	job := jobs[0]
	assert.Equal(t, rs.Job, job.Name)
	assert.True(t, strings.HasPrefix(job.Name, templates.RestoreJobName+"-"))
	assert.True(t, metav1.IsControlledBy(&job, e.z))
	assert.Equal(t, aDump, job.Annotations[templates.AnnotationRestoreDump])
	for k := range job.Annotations {
		assert.False(t, strings.HasPrefix(k, "helm.sh/"), k)
	}
	dumpEnv := ""
	for _, ev := range job.Spec.Template.Spec.Containers[0].Env {
		if ev.Name == "DUMP" {
			dumpEnv = ev.Value
		}
	}
	assert.Equal(t, aDump, dumpEnv)

	_, inFlight = e.restorePass(false)
	assert.True(t, inFlight, "polled while it runs")
	assert.Len(t, restoreJobs(t, e.c, e.z.Namespace), 1, "started once")

	e.endRestore(0, "restored chino (4 tables), katalog (80 tables), keycloak (90 tables), portal (5 tables) from "+aDump)
	dump, inFlight = e.restorePass(false)
	assert.Equal(t, aDump, dump, "the pass that reads the end still renders the clients stopped")
	assert.False(t, inFlight)
	rs = e.z.Status.Backup.Restore
	assert.Equal(t, zaentrumv1alpha1.RestoreSucceeded, rs.Result)
	assert.Contains(t, rs.Message, "restored chino (4 tables)")
	require.NotNil(t, rs.FinishedAt)
	c := restoreCondition(e.z)
	assert.Equal(t, metav1.ConditionTrue, c.Status)
	assert.Equal(t, "Succeeded", c.Reason)
	var realm batchv1.JobList
	require.NoError(t, e.c.List(context.Background(), &realm, client.InNamespace(e.z.Namespace), client.MatchingLabels{labelRealm: realmRun}))
	assert.Empty(t, realm.Items, "the realm Job runs again over the restored realm")

	dump, inFlight = e.restorePass(false)
	assert.Empty(t, dump, "answered: the clients start again")
	assert.False(t, inFlight)
	assert.Equal(t, int64(1), int64(rendersReplicas(t, e.platform, "chino-api")))
	assert.Equal(t, zaentrumv1alpha1.RestoreSucceeded, e.z.Status.Backup.Restore.Result)

	e.askRestore(aDump + "#again")
	dump, inFlight = e.restorePass(false)
	assert.Equal(t, aDump, dump, "the same dump, asked for again")
	assert.True(t, inFlight)
	rs = e.z.Status.Backup.Restore
	assert.Equal(t, aDump+"#again", rs.Request)
	assert.Nil(t, rs.FinishedAt)
	again := restoreJobs(t, e.c, e.z.Namespace)
	require.Len(t, again, 1, "the previous restore's Job goes before the next one comes")
	assert.NotEqual(t, job.Name, again[0].Name)
	assert.Equal(t, rs.Job, again[0].Name)
}

func controllerRef(e *backupEnv, obj client.Object) error {
	return controllerutil.SetControllerReference(e.z, obj, e.r.Scheme)
}

func rendersReplicas(t *testing.T, objs []*unstructured.Unstructured, name string) int64 {
	t.Helper()
	for _, o := range objs {
		if o.GetKind() == "Deployment" && o.GetName() == name {
			n, _, _ := unstructured.NestedFieldNoCopy(o.Object, "spec", "replicas")
			return asCount(n)
		}
	}
	t.Fatalf("no Deployment %s", name)
	return -1
}

// What a restore will not do, it refuses — once, changing nothing: a value
// that names no dump, external databases, no claim to restore from, and what
// the restore script refuses (exit 3: a dump that fails its checksums). A Job
// that fails otherwise is a failure, and one that is gone too.
func TestRestoreRefusals(t *testing.T) {
	t.Run("a value that names no dump", func(t *testing.T) {
		e := newBackupEnv(t, verifyCR())
		e.askRestore("yesterday")
		dump, inFlight := e.restorePass(false)
		assert.Empty(t, dump, "nothing is stopped")
		assert.False(t, inFlight)
		rs := e.z.Status.Backup.Restore
		assert.Equal(t, zaentrumv1alpha1.RestoreRefused, rs.Result)
		assert.Equal(t, "yesterday", rs.Request)
		assert.Contains(t, rs.Message, "names no dump")
		assert.Equal(t, "Refused", restoreCondition(e.z).Reason)
		first := *rs
		_, _ = e.restorePass(false)
		assert.Equal(t, first, *e.z.Status.Backup.Restore, "answered once")
	})
	t.Run("external databases", func(t *testing.T) {
		z := verifyCR()
		z.Spec.Databases.Mode = "external"
		z.Spec.Databases.External.Host = "postgres.example.com"
		e := newBackupEnv(t, z)
		e.askRestore(aDump)
		dump, _ := e.restorePass(false)
		assert.Empty(t, dump)
		assert.Contains(t, e.z.Status.Backup.Restore.Message, "external")
		e.r.reportBackups(context.Background(), e.z, e.platform)
		assert.NotNil(t, e.z.Status.Backup.Restore, "the refusal stays readable")
	})
	t.Run("no claim to restore from", func(t *testing.T) {
		e := newBackupEnv(t, verifyCR())
		e.askRestore(aDump)
		_, _ = e.restorePass(false)
		_, inFlight := e.restorePass(false)
		assert.False(t, inFlight)
		rs := e.z.Status.Backup.Restore
		assert.Equal(t, zaentrumv1alpha1.RestoreRefused, rs.Result)
		assert.Contains(t, rs.Message, "no claim backups")
		assert.Empty(t, restoreJobs(t, e.c, e.z.Namespace))
	})
	for name, c := range map[string]struct {
		exit   int32
		msg    string
		result zaentrumv1alpha1.RestoreResult
		want   string
	}{
		"a dump that fails its checksums": {3, "refused: " + aDump + " does not match its checksums: katalog.dump: FAILED; nothing was changed",
			zaentrumv1alpha1.RestoreRefused, "does not match its checksums"},
		"a restore that fails half way": {1, "cannot restore the database katalog from " + aDump + " (1 of the databases were restored before it: chino); restore it again",
			zaentrumv1alpha1.RestoreFailed, "cannot restore the database katalog"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newBackupEnv(t, verifyCR())
			e.runningPlatform()
			e.askRestore(aDump)
			_, _ = e.restorePass(false)
			e.stopClients()
			_, _ = e.restorePass(false)
			e.endRestore(c.exit, c.msg)
			_, inFlight := e.restorePass(false)
			assert.False(t, inFlight)
			rs := e.z.Status.Backup.Restore
			assert.Equal(t, c.result, rs.Result)
			assert.Contains(t, rs.Message, c.want)
			dump, _ := e.restorePass(false)
			assert.Empty(t, dump, "the clients start again")
		})
	}
	t.Run("the Job is gone", func(t *testing.T) {
		e := newBackupEnv(t, verifyCR())
		e.runningPlatform()
		e.askRestore(aDump)
		_, _ = e.restorePass(false)
		e.stopClients()
		_, _ = e.restorePass(false)
		require.NoError(t, e.c.Delete(context.Background(), &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
			Name: e.z.Status.Backup.Restore.Job, Namespace: e.z.Namespace}}))
		_, inFlight := e.restorePass(false)
		assert.False(t, inFlight)
		assert.Equal(t, zaentrumv1alpha1.RestoreFailed, e.z.Status.Backup.Restore.Result)
		assert.Contains(t, e.z.Status.Backup.Restore.Message, "partly restored")
	})
}

// A restore waits for a database copy to end before it stops anything, and
// for a backup in flight before it restores.
func TestARestoreWaitsForACopyAndABackup(t *testing.T) {
	e := newBackupEnv(t, verifyCR())
	e.runningPlatform()
	e.askRestore(aDump)
	dump, _ := e.restorePass(true)
	assert.Empty(t, dump, "nothing stops while the database is copied")
	assert.Nil(t, e.z.Status.Backup, "the request is not answered yet")
	assert.Equal(t, "Waiting", restoreCondition(e.z).Reason)

	e.backupRun("zaentrum-backup-1", e.clock, -1, "")
	dump, _ = e.restorePass(false)
	assert.Equal(t, aDump, dump)
	e.stopClients()
	_, inFlight := e.restorePass(false)
	assert.True(t, inFlight)
	assert.Equal(t, zaentrumv1alpha1.RestoreStopping, e.z.Status.Backup.Restore.Result)
	assert.Contains(t, e.z.Status.Backup.Restore.Message, "waiting for the backup zaentrum-backup-1 to end")
	assert.Empty(t, restoreJobs(t, e.c, e.z.Namespace))

	require.NoError(t, e.c.Delete(context.Background(), &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "zaentrum-backup-1", Namespace: e.z.Namespace}}))
	_, _ = e.restorePass(false)
	assert.Equal(t, zaentrumv1alpha1.RestoreRunning, e.z.Status.Backup.Restore.Result)
}

// Through Reconcile: the pass that takes the request applies the clients
// stopped and the backups suspended, the phase is Restoring, Ready is False
// for it, and no verification starts.
func TestReconcileWhileARestoreRuns(t *testing.T) {
	z := verifyCR()
	z.Annotations = map[string]string{zaentrumv1alpha1.RestoreRequestAnnotation: aDump}
	s := selfScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(z).
		WithStatusSubresource(&zaentrumv1alpha1.Zaentrum{}, &appsv1.Deployment{}, &batchv1.Job{}).
		WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			return applyAsCreateOrUpdate(ctx, cl, obj, patch, opts...)
		}}).Build()
	r := &ZaentrumReconciler{Client: c, Scheme: s}
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: verifyNS, Name: "zaentrum"}}

	res, err := r.Reconcile(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, verifyRequeueAfter, res.RequeueAfter, "polled while it runs")
	var got zaentrumv1alpha1.Zaentrum
	require.NoError(t, c.Get(ctx, req.NamespacedName, &got))
	assert.Equal(t, "Restoring", got.Status.Phase)
	ready := meta.FindStatusCondition(got.Status.Conditions, condTypeReady)
	require.NotNil(t, ready)
	assert.Equal(t, "Restoring", ready.Reason)
	require.NotNil(t, got.Status.Backup)
	require.NotNil(t, got.Status.Backup.Restore)
	assert.Equal(t, zaentrumv1alpha1.RestoreRunning, got.Status.Backup.Restore.Result, "no pod was up: the restore started")
	assert.Nil(t, got.Status.Verification)

	var dep appsv1.Deployment
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: verifyNS, Name: "keycloak"}, &dep))
	assert.Equal(t, int32(0), *dep.Spec.Replicas, "applied stopped")
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: verifyNS, Name: "chino-web"}, &dep))
	assert.Equal(t, int32(1), *dep.Spec.Replicas)
	var cron batchv1.CronJob
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: verifyNS, Name: templates.BackupCronJobName}, &cron))
	assert.True(t, *cron.Spec.Suspend)

	// The claim with the backups outlives the platform: it is applied with no
	// owner, unlike the Postgres's own.
	var pvc corev1.PersistentVolumeClaim
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: verifyNS, Name: "backups"}, &pvc))
	assert.Empty(t, pvc.OwnerReferences)
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: verifyNS, Name: "postgres-data"}, &pvc))
	assert.NotEmpty(t, pvc.OwnerReferences)
}
