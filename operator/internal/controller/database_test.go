package controller

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
	"github.com/zaentrum/zaentrum-operator/operator/internal/templates"
)

// livePostgres is the running Postgres Deployment, its data on volume
// ("emptyDir" or a claim), with available of its one replica available.
func livePostgres(volume string, available int32) *appsv1.Deployment {
	data := corev1.Volume{Name: "data"}
	if volume == "emptyDir" {
		data.EmptyDir = &corev1.EmptyDirVolumeSource{}
	} else {
		data.PersistentVolumeClaim = &corev1.PersistentVolumeClaimVolumeSource{ClaimName: volume}
	}
	one := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "postgres", Namespace: verifyNS},
		Spec: appsv1.DeploymentSpec{Replicas: &one, Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "postgres", Image: "postgres:16-alpine"}},
			Volumes:    []corev1.Volume{data},
		}}},
		Status: appsv1.DeploymentStatus{AvailableReplicas: available, UpdatedReplicas: available},
	}
}

// dbEnv is a platform through Reconcile, over a fake API server, with a clock.
type dbEnv struct {
	t       *testing.T
	c       client.WithWatch
	r       *ZaentrumReconciler
	clock   time.Time
	applied []string
	// patches records every patch that is not an apply, in order.
	patches []string
}

func newDBEnv(t *testing.T, z *zaentrumv1alpha1.Zaentrum, funcs *interceptor.Funcs, objs ...client.Object) *dbEnv {
	t.Helper()
	s := selfScheme(t)
	e := &dbEnv{t: t, clock: time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)}
	f := interceptor.Funcs{Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		if patch.Type() == types.JSONPatchType {
			e.patches = append(e.patches, "json-patch Deployment/"+obj.GetName())
		} else {
			e.applied = append(e.applied, obj.GetObjectKind().GroupVersionKind().Kind+"/"+obj.GetName())
		}
		return applyAsCreateOrUpdate(ctx, cl, obj, patch, opts...)
	}}
	if funcs != nil && funcs.Get != nil {
		f.Get = funcs.Get
	}
	e.c = fake.NewClientBuilder().WithScheme(s).WithObjects(append(objs, z)...).
		WithStatusSubresource(&zaentrumv1alpha1.Zaentrum{}, &appsv1.Deployment{}, &batchv1.Job{}).
		WithInterceptorFuncs(f).Build()
	e.r = &ZaentrumReconciler{Client: e.c, Scheme: s, Now: func() time.Time { return e.clock }}
	return e
}

func (e *dbEnv) reconcile() (ctrl.Result, error) {
	e.t.Helper()
	e.clock = e.clock.Add(10 * time.Second)
	return e.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: verifyNS, Name: "zaentrum"}})
}

func (e *dbEnv) z() *zaentrumv1alpha1.Zaentrum {
	e.t.Helper()
	var z zaentrumv1alpha1.Zaentrum
	require.NoError(e.t, e.c.Get(context.Background(), types.NamespacedName{Namespace: verifyNS, Name: "zaentrum"}, &z))
	return &z
}

func (e *dbEnv) cond() *metav1.Condition {
	return meta.FindStatusCondition(e.z().Status.Conditions, condTypeDatabasePersistent)
}

func (e *dbEnv) postgres() *appsv1.Deployment {
	e.t.Helper()
	var dep appsv1.Deployment
	require.NoError(e.t, e.c.Get(context.Background(), types.NamespacedName{Namespace: verifyNS, Name: "postgres"}, &dep))
	return &dep
}

func (e *dbEnv) volume() string {
	e.t.Helper()
	v, err := postgresVolume(e.postgres())
	require.NoError(e.t, err)
	return v
}

func (e *dbEnv) copies() []batchv1.Job {
	e.t.Helper()
	var jobs batchv1.JobList
	require.NoError(e.t, e.c.List(context.Background(), &jobs, client.InNamespace(verifyNS), client.MatchingLabels{labelDatabase: databaseMigration}))
	return jobs.Items
}

func (e *dbEnv) jobs(label string) int {
	e.t.Helper()
	var jobs batchv1.JobList
	require.NoError(e.t, e.c.List(context.Background(), &jobs, client.InNamespace(verifyNS), client.HasLabels{label}))
	return len(jobs.Items)
}

func (e *dbEnv) claim(name string) bool {
	e.t.Helper()
	var pvc corev1.PersistentVolumeClaim
	err := e.c.Get(context.Background(), types.NamespacedName{Namespace: verifyNS, Name: name}, &pvc)
	if apierrors.IsNotFound(err) {
		return false
	}
	require.NoError(e.t, err)
	return true
}

// available makes every Deployment the platform applied available, as the
// Deployment controller would; the live Postgres keeps its own volume.
func (e *dbEnv) available() {
	e.t.Helper()
	var deps appsv1.DeploymentList
	require.NoError(e.t, e.c.List(context.Background(), &deps, client.InNamespace(verifyNS)))
	for i := range deps.Items {
		d := &deps.Items[i]
		d.Status.ObservedGeneration = d.Generation
		d.Status.AvailableReplicas = 1
		d.Status.UpdatedReplicas = 1
		if d.Spec.Replicas != nil {
			d.Status.AvailableReplicas = *d.Spec.Replicas
			d.Status.UpdatedReplicas = *d.Spec.Replicas
		}
		require.NoError(e.t, e.c.Status().Update(context.Background(), d))
	}
}

// end finishes a copy the way the Job controller and the kubelet do.
func (e *dbEnv) end(job batchv1.Job, ok bool, message string) {
	e.t.Helper()
	ctx := context.Background()
	cond := batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(e.clock)}
	exit := int32(0)
	if !ok {
		cond = batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded",
			LastTransitionTime: metav1.NewTime(e.clock)}
		exit = 1
	}
	job.Status.Conditions = append(job.Status.Conditions, cond)
	require.NoError(e.t, e.c.Status().Update(ctx, &job))
	require.NoError(e.t, e.c.Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-p1", Namespace: verifyNS, Labels: map[string]string{batchv1.JobNameLabel: job.Name}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: templates.PostgresMigrationContainer,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exit, Message: message}}}}},
	}))
}

func migrateCR() *zaentrumv1alpha1.Zaentrum {
	z := verifyCR()
	z.Spec.Storage.Postgres.Migrate = true
	return z
}

// A new install keeps its database on the claim from its first pass.
func TestDatabaseNewInstallStartsOnTheClaim(t *testing.T) {
	e := newDBEnv(t, verifyCR(), nil)
	_, err := e.reconcile()
	require.NoError(t, err)
	assert.Equal(t, "postgres-data", e.volume())
	assert.True(t, e.claim("postgres-data"))
	c := e.cond()
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionTrue, c.Status)
	assert.Equal(t, "OnClaim", c.Reason)
	assert.Equal(t, "the bundled Postgres keeps its data on claim postgres-data", c.Message)
	assert.Empty(t, e.copies())
}

// The demo's case: its Postgres runs on an emptyDir. Every pass renders it
// exactly as it runs — the Deployment of before this change, so nothing rolls
// and nothing is lost — makes no claim it would not use, and says so.
func TestDatabaseOnEmptyDirStaysThere(t *testing.T) {
	raw, err := os.ReadFile("../templates/testdata/postgres-emptydir.yaml")
	require.NoError(t, err)
	var golden appsv1.Deployment
	require.NoError(t, yaml.Unmarshal(raw, &golden))
	golden.Namespace = verifyNS
	golden.Status = appsv1.DeploymentStatus{AvailableReplicas: 1}

	z := verifyCR()
	z.Spec.PartOf = "zaentrum-demo"
	e := newDBEnv(t, z, nil, &golden)
	for i := 0; i < 3; i++ {
		_, err := e.reconcile()
		require.NoError(t, err)
		live := e.postgres()
		assert.Equal(t, golden.Spec.Template, live.Spec.Template, "pass %d: the emptyDir Postgres's pod template changed — it would restart empty", i)
		assert.False(t, e.claim("postgres-data"), "no claim it would not use")
		assert.Empty(t, e.copies())
	}
	c := e.cond()
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, "EmptyDir", c.Reason)
	assert.Contains(t, c.Message, "spec.storage.postgres.migrate", "it says how to move it")
	assert.Contains(t, c.Message, "loses every user, all watch state and the catalog")
}

// Changing the claim of a Postgres that runs on one is no switch either.
func TestDatabaseClaimChangeIsNoSwitch(t *testing.T) {
	z := verifyCR()
	z.Spec.Storage.Postgres.ClaimName = "new-claim"
	e := newDBEnv(t, z, nil, livePostgres("old-claim", 1))
	_, err := e.reconcile()
	require.NoError(t, err)
	assert.Equal(t, "old-claim", e.volume())
	c := e.cond()
	assert.Equal(t, "OtherClaim", c.Reason)
	assert.Contains(t, c.Message, "runs on claim old-claim, not on claim new-claim")
}

// migrate: copy first, switch after — and only after the copy succeeded.
func TestDatabaseMigratesByACopy(t *testing.T) {
	e := newDBEnv(t, migrateCR(), nil, livePostgres("emptyDir", 1))

	res, err := e.reconcile()
	require.NoError(t, err)
	assert.Equal(t, verifyRequeueAfter, res.RequeueAfter, "a copy in flight is polled")
	assert.Equal(t, "emptyDir", e.volume(), "nothing switches before the copy")
	assert.True(t, e.claim("postgres-data"), "the claim the copy goes onto")
	copies := e.copies()
	require.Len(t, copies, 1)
	job := copies[0]
	assert.True(t, strings.HasPrefix(job.Name, templates.PostgresMigrationJobName+"-"), job.Name)
	assert.True(t, metav1.IsControlledBy(&job, e.z()))
	assert.Equal(t, "emptyDir", job.Annotations[templates.AnnotationMigrationSource])
	assert.Equal(t, "postgres-data", job.Annotations[templates.AnnotationMigrationTarget])
	for k := range job.Annotations {
		assert.False(t, strings.HasPrefix(k, "helm.sh/"), k)
	}
	assert.Nil(t, job.Spec.TTLSecondsAfterFinished, "kept: it decides the switch")
	for _, a := range e.applied {
		assert.NotEqual(t, "Job/"+templates.PostgresMigrationJobName, a, "the hook was applied with the platform")
	}
	assert.Equal(t, "Migrating", e.cond().Reason)

	// Everything is available meanwhile: still no realm run and no check — what
	// they wrote now would not be carried across.
	e.available()
	_, err = e.reconcile()
	require.NoError(t, err)
	assert.Len(t, e.copies(), 1, "one copy at a time")
	assert.Equal(t, "emptyDir", e.volume())
	assert.Zero(t, e.jobs(labelRealm))
	assert.Zero(t, e.jobs(labelVerification))
	assert.Equal(t, "Waiting", meta.FindStatusCondition(e.z().Status.Conditions, condTypeRealmConfigured).Reason)

	e.end(e.copies()[0], true, "copied 5 databases (12 MiB) onto postgres-data: chino (4 tables, 120 rows)")
	e.patches = nil
	_, err = e.reconcile()
	require.NoError(t, err)
	assert.Equal(t, "postgres-data", e.volume(), "switched once the copy succeeded")
	require.NotEmpty(t, e.patches)
	assert.Equal(t, "json-patch Deployment/postgres", e.patches[0],
		"the volume entry is replaced whole first: an apply alone keeps an emptyDir another manager co-owns")
	data := e.postgres().Spec.Template.Spec.Volumes[0]
	assert.Nil(t, data.EmptyDir, "no emptyDir left beside the claim")
	c := e.cond()
	assert.Equal(t, metav1.ConditionTrue, c.Status)
	assert.Equal(t, "Migrated", c.Reason)
	assert.Contains(t, c.Message, "copied 5 databases (12 MiB) onto postgres-data")

	_, err = e.reconcile()
	require.NoError(t, err)
	c = e.cond()
	assert.Equal(t, "OnClaim", c.Reason)
	assert.Contains(t, c.Message, "where "+job.Name+" moved it from emptyDir: copied 5 databases")
	assert.Len(t, e.copies(), 1, "migrate left on copies nothing more")
}

// A failed copy leaves the Postgres where it is and says why; it is not
// repeated by itself, and deleting it asks for another.
func TestDatabaseFailedCopyIsNotRepeatedByItself(t *testing.T) {
	e := newDBEnv(t, migrateCR(), nil, livePostgres("emptyDir", 1))
	_, err := e.reconcile()
	require.NoError(t, err)
	first := e.copies()[0]
	e.end(first, false, "postgres-data already holds a database this migration did not write; it is not overwritten")

	for i := 0; i < 3; i++ {
		_, err = e.reconcile()
		require.NoError(t, err)
		assert.Equal(t, "emptyDir", e.volume())
		assert.Len(t, e.copies(), 1)
	}
	c := e.cond()
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, "MigrationFailed", c.Reason)
	assert.Contains(t, c.Message, "already holds a database this migration did not write")
	assert.Contains(t, c.Message, "delete Job "+first.Name+" to try again")

	require.NoError(t, e.c.Delete(context.Background(), &first))
	_, err = e.reconcile()
	require.NoError(t, err)
	copies := e.copies()
	require.Len(t, copies, 1)
	assert.NotEqual(t, first.Name, copies[0].Name, "asked again")
}

// A copy nothing switched to in time has missed what was written since: it is
// taken again, never switched to.
func TestDatabaseStaleCopyIsTakenAgain(t *testing.T) {
	e := newDBEnv(t, migrateCR(), nil, livePostgres("emptyDir", 1))
	_, err := e.reconcile()
	require.NoError(t, err)
	first := e.copies()[0]
	e.end(first, true, "copied 5 databases")
	e.clock = e.clock.Add(migrationSwitchWindow + time.Minute)

	_, err = e.reconcile()
	require.NoError(t, err)
	assert.Equal(t, "emptyDir", e.volume(), "a stale copy is not switched to")
	copies := e.copies()
	require.Len(t, copies, 1)
	assert.NotEqual(t, first.Name, copies[0].Name, "copied again")
}

// A copy waits for the Postgres it reads.
func TestDatabaseCopyWaitsForThePostgres(t *testing.T) {
	e := newDBEnv(t, migrateCR(), nil, livePostgres("emptyDir", 0))
	_, err := e.reconcile()
	require.NoError(t, err)
	assert.Empty(t, e.copies())
	assert.Equal(t, "emptyDir", e.volume())
	assert.Contains(t, e.cond().Message, "once it is available")
}

// Where the running Postgres cannot be read, or keeps its data on something
// the operator does not know, nothing is rendered or applied: any guess could
// start it on an empty volume.
func TestDatabaseUnknownMeansNothingIsApplied(t *testing.T) {
	failing := &interceptor.Funcs{Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if _, ok := obj.(*appsv1.Deployment); ok && key.Name == "postgres" {
			return apierrors.NewServiceUnavailable("etcd is busy")
		}
		return cl.Get(ctx, key, obj, opts...)
	}}
	e := newDBEnv(t, verifyCR(), failing)
	_, err := e.reconcile()
	require.Error(t, err)
	assert.Empty(t, e.applied)
	assert.Equal(t, "Error", e.z().Status.Phase)

	odd := livePostgres("emptyDir", 1)
	odd.Spec.Template.Spec.Volumes[0] = corev1.Volume{Name: "data", VolumeSource: corev1.VolumeSource{
		HostPath: &corev1.HostPathVolumeSource{Path: "/srv/pg"}}}
	e = newDBEnv(t, verifyCR(), nil, odd)
	_, err = e.reconcile()
	require.Error(t, err)
	assert.Empty(t, e.applied)
	c := e.cond()
	require.NotNil(t, c)
	assert.Contains(t, c.Message, "a volume the operator does not know")
}

// A shared database cluster is the tenant's: no claim, no copy, no condition.
func TestDatabaseExternal(t *testing.T) {
	z := migrateCR()
	z.Spec.Databases.Mode = "external"
	z.Spec.Databases.External.Host = "postgres.example.com"
	z.Status.Conditions = []metav1.Condition{{Type: condTypeDatabasePersistent, Status: metav1.ConditionFalse, Reason: "EmptyDir"}}
	e := newDBEnv(t, z, nil)
	_, err := e.reconcile()
	require.NoError(t, err)
	assert.Nil(t, e.cond())
	assert.False(t, e.claim("postgres-data"))
	assert.Empty(t, e.copies())
	var dep appsv1.Deployment
	err = e.c.Get(context.Background(), types.NamespacedName{Namespace: verifyNS, Name: "postgres"}, &dep)
	assert.True(t, apierrors.IsNotFound(err), "no bundled Postgres")
}

// A verification run does not start while the database is copied, whatever
// else holds or does not: what it wrote would not be carried across, and the
// Postgres is about to restart.
func TestDatabaseCopyHoldsTheVerification(t *testing.T) {
	z := migrateCR()
	z.Spec.Identity.Mode = zaentrumv1alpha1.IdentityExternal // no realm run to wait for
	z.Spec.Identity.Issuer = "https://sso.example.org/realms/x"
	e := newDBEnv(t, z, nil, livePostgres("emptyDir", 1))
	_, err := e.reconcile()
	require.NoError(t, err)
	e.available()
	_, err = e.reconcile()
	require.NoError(t, err)
	assert.Equal(t, "Ready", e.z().Status.Phase)
	assert.Zero(t, e.jobs(labelVerification), "no check while the copy runs")

	e.end(e.copies()[0], true, "copied 5 databases")
	_, err = e.reconcile()
	require.NoError(t, err)
	assert.Equal(t, "postgres-data", e.volume())
	e.available()
	_, err = e.reconcile()
	require.NoError(t, err)
	assert.Equal(t, 1, e.jobs(labelVerification), "once the Postgres has moved, the platform is checked")
}
