package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
	"github.com/zaentrum/zaentrum-operator/operator/internal/templates"
)

// storageEnv is a platform, its claims and the measuring passes taken over
// it, on a fake API server that stamps what it creates with the env's clock,
// as an API server stamps its time. It can refuse to create a run, and fail to
// read a claim or a run's pods.
type storageEnv struct {
	t        *testing.T
	c        client.WithWatch
	r        *ZaentrumReconciler
	z        *zaentrumv1alpha1.Zaentrum
	platform []*unstructured.Unstructured
	hooks    []*unstructured.Unstructured
	clock    time.Time
	postgres string

	refuse        string          // a create of a run is refused, saying this
	unreadClaims  map[string]bool // these claims cannot be read
	unreadPods    bool            // no pod can be listed
	unreadRuns    bool            // no Job can be listed
	refuseDeletes int             // so many deletes of a run are refused
	creates       int             // the runs it was asked to create
}

func newStorageEnv(t *testing.T, z *zaentrumv1alpha1.Zaentrum, objs ...client.Object) *storageEnv {
	t.Helper()
	s := selfScheme(t)
	e := &storageEnv{t: t, z: z, clock: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC), postgres: "postgres-data",
		unreadClaims: map[string]bool{}}
	e.c = fake.NewClientBuilder().WithScheme(s).WithObjects(append(objs, z)...).
		WithStatusSubresource(&batchv1.Job{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, run := obj.GetLabels()[templates.LabelMeasure]; run {
					e.creates++
					if e.refuse != "" {
						return errors.New(e.refuse)
					}
				}
				if ts := obj.GetCreationTimestamp(); ts.IsZero() {
					obj.SetCreationTimestamp(metav1.NewTime(e.clock))
				}
				return c.Create(ctx, obj, opts...)
			},
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, claim := obj.(*corev1.PersistentVolumeClaim); claim && e.unreadClaims[key.Name] {
					return errors.New("the claim could not be read")
				}
				return c.Get(ctx, key, obj, opts...)
			},
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, run := obj.GetLabels()[templates.LabelMeasure]; run && e.refuseDeletes > 0 {
					e.refuseDeletes--
					return errors.New("the delete was refused")
				}
				return c.Delete(ctx, obj, opts...)
			},
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, pods := list.(*corev1.PodList); pods && e.unreadPods {
					return errors.New("the pods could not be listed")
				}
				if _, jobs := list.(*batchv1.JobList); jobs && e.unreadRuns {
					return errors.New("the Jobs could not be listed")
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()
	e.r = &ZaentrumReconciler{Client: e.c, Scheme: s, Now: func() time.Time { return e.clock }}
	e.render()
	return e
}

// render renders the platform as the reconciler does, its Postgres on
// e.postgres.
func (e *storageEnv) render() {
	e.t.Helper()
	v := templates.NewValues(e.z)
	v.PostgresVolume = e.postgres
	objs, err := templates.Render(v)
	require.NoError(e.t, err)
	e.platform, e.hooks = templates.SplitHooks(objs)
}

// claim is a claim of the platform's, asking for size, bound or not yet.
func (e *storageEnv) claim(name, size string, bound bool) {
	e.t.Helper()
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: e.z.Namespace},
		Spec: corev1.PersistentVolumeClaimSpec{Resources: corev1.VolumeResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)}}}}
	if bound {
		pvc.Status.Phase = corev1.ClaimBound
	}
	ctx := context.Background()
	var old corev1.PersistentVolumeClaim
	if err := e.c.Get(ctx, client.ObjectKeyFromObject(pvc), &old); err == nil {
		old.Spec, old.Status = pvc.Spec, pvc.Status
		require.NoError(e.t, e.c.Update(ctx, &old))
		return
	}
	require.NoError(e.t, e.c.Create(ctx, pvc))
}

// postgresPod is the running Postgres, on a node.
func (e *storageEnv) postgresPod(node string) {
	e.t.Helper()
	require.NoError(e.t, e.c.Create(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "postgres-abc", Namespace: e.z.Namespace, Labels: map[string]string{"app": "postgres"}},
		Spec:       corev1.PodSpec{NodeName: node}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}))
}

func (e *storageEnv) pass(restoring bool) bool {
	e.t.Helper()
	return e.r.measureStorage(context.Background(), e.z, e.platform, e.hooks, restoring, false)
}

// passHeld is a pass while the backups' schedule is held for a backup.
func (e *storageEnv) passHeld() bool {
	e.t.Helper()
	return e.r.measureStorage(context.Background(), e.z, e.platform, e.hooks, false, true)
}

// runs are the measuring runs of a volume, newest first.
func (e *storageEnv) runs(volume string) []batchv1.Job {
	e.t.Helper()
	jobs, err := e.r.measureJobs(context.Background(), e.z)
	require.NoError(e.t, err)
	return jobs[volume]
}

// end ends the newest run of a volume as the kubelet leaves it: exit 0 with
// the report, or exit 1 with it, on the node given; with exit < 0 the Job
// fails and its pod is left as it is.
func (e *storageEnv) end(volume, node string, exit int32, report string) string {
	e.t.Helper()
	runs := e.runs(volume)
	require.NotEmpty(e.t, runs, "a run of %s", volume)
	job := runs[0]
	e.clock = e.clock.Add(time.Minute)
	ended := metav1.NewTime(e.clock)
	cond := batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: ended}
	if exit != 0 {
		cond = batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, LastTransitionTime: ended, Reason: "BackoffLimitExceeded",
			Message: "Job has reached the specified backoff limit"}
	}
	job.Status.Conditions = []batchv1.JobCondition{cond}
	require.NoError(e.t, e.c.Status().Update(context.Background(), &job))
	if exit >= 0 {
		require.NoError(e.t, e.c.Create(context.Background(), &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-x", Namespace: e.z.Namespace, Labels: map[string]string{batchv1.JobNameLabel: job.Name}},
			Spec:       corev1.PodSpec{NodeName: node},
			Status: corev1.PodStatus{Phase: map[bool]corev1.PodPhase{true: corev1.PodSucceeded, false: corev1.PodFailed}[exit == 0],
				ContainerStatuses: []corev1.ContainerStatus{{Name: templates.MeasureContainer,
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exit, Message: report, FinishedAt: ended,
						Reason: map[bool]string{true: "Completed", false: "Error"}[exit == 0]}}}}}}))
	}
	return job.Name
}

func (e *storageEnv) volume(name string) *zaentrumv1alpha1.VolumeStatus {
	e.t.Helper()
	if e.z.Status.Storage == nil {
		return nil
	}
	for i := range e.z.Status.Storage.Volumes {
		if e.z.Status.Storage.Volumes[i].Name == name {
			return &e.z.Status.Storage.Volumes[i]
		}
	}
	return nil
}

func (e *storageEnv) condition() *metav1.Condition {
	return meta.FindStatusCondition(e.z.Status.Conditions, condTypeStorageMeasured)
}

func (e *storageEnv) answered() string {
	if e.z.Status.Storage == nil {
		return ""
	}
	return e.z.Status.Storage.Request
}

func (e *storageEnv) request(value string) {
	ann := e.z.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[zaentrumv1alpha1.StorageRequestAnnotation] = value
	e.z.SetAnnotations(ann)
}

// backupRun is a backup Job, running, or ended at the env's clock.
func (e *storageEnv) backupRun(name string, ended bool) {
	e.t.Helper()
	ctx := context.Background()
	var job batchv1.Job
	if err := e.c.Get(ctx, types.NamespacedName{Namespace: e.z.Namespace, Name: name}, &job); err != nil {
		job = batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: e.z.Namespace,
			Labels: map[string]string{labelDatabase: databaseBackup}}}
		require.NoError(e.t, e.c.Create(ctx, &job))
	}
	if ended {
		job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
			LastTransitionTime: metav1.NewTime(e.clock)}}
		require.NoError(e.t, e.c.Status().Update(ctx, &job))
	}
}

const gib = int64(1) << 30

const databasesReport = `"databases":{"chino":7635471,"katalog":9000000,"keycloak":12000000,"portal":8000000}`

func libraryReport(size, free int64, fs string) string {
	return fmt.Sprintf(`{"v":1,"size":%d,"free":%d,"fs":%q,%s,"postgres":{"size":%d,"free":%d,"fs":%q}}`,
		size, free, fs, databasesReport, 10*gib, 8*gib, fs)
}

const backupsReport = `{"v":1,"size":5368709120,"free":4294967296,"fs":"c1","total":0,"sets":[]}`
const kafkaReport = `{"v":1,"size":1073741824,"free":536870912,"fs":"d1"}`

func TestRC7EveryClaimIsListedAndNothingStartsOnAClaimNotBound(t *testing.T) {
	e := newStorageEnv(t, verifyCR())
	e.claim("media", "50Gi", true)
	e.claim("postgres-data", "10Gi", true)
	e.claim("backups", "5Gi", false)
	assert.True(t, e.pass(false), "a run is in flight")

	s := e.z.Status.Storage
	require.NotNil(t, s)
	var names []string
	for _, v := range s.Volumes {
		names = append(names, v.Name+"="+v.Claim)
		assert.Nil(t, v.SizeBytes, "%s is not measured yet", v.Name)
		assert.Nil(t, v.MeasuredAt)
	}
	assert.Equal(t, []string{"media=media", "postgres-data=postgres-data", "backups=backups"}, names)
	assert.Equal(t, 50*gib, *e.volume("media").RequestBytes, "the size the claim asks for, measured or not")
	assert.Equal(t, 5*gib, *e.volume("backups").RequestBytes)
	assert.Len(t, e.runs("media"), 1)
	assert.Empty(t, e.runs("backups"), "a claim not bound yet: no run decides where its volume is made")
	assert.Nil(t, e.condition(), "absent until the first run has ended")

	run := e.runs("media")[0]
	assert.True(t, strings.HasPrefix(run.Name, "zaentrum-measure-media-"))
	assert.True(t, metav1.IsControlledBy(&run, e.z))
	assert.NotContains(t, run.Annotations, "helm.sh/hook")
}

func TestRC7ARunsReportIsWrittenAndTheConditionSaysWhatItFound(t *testing.T) {
	e := newStorageEnv(t, verifyCR())
	e.claim("media", "50Gi", true)
	e.claim("postgres-data", "10Gi", true)
	e.postgresPod("node-a")
	e.pass(false)
	e.end("media", "node-a", 0, libraryReport(50*gib, 20*gib, "00b9e2"))
	assert.False(t, e.pass(false), "nothing in flight")

	media, pg := e.volume("media"), e.volume("postgres-data")
	require.NotNil(t, media.SizeBytes)
	assert.Equal(t, 50*gib, *media.SizeBytes)
	assert.Equal(t, 20*gib, *media.FreeBytes)
	assert.Equal(t, "node-a:b9e2", media.Filesystem, "the filesystem's own id, the node it was measured on in front")
	assert.True(t, e.clock.Equal(media.MeasuredAt.Time), "when the run ended")
	require.NotNil(t, pg.SizeBytes)
	assert.Equal(t, 10*gib, *pg.SizeBytes)
	assert.Equal(t, "node-a:b9e2", pg.Filesystem, "the Postgres's node, which read it")
	assert.Equal(t, []zaentrumv1alpha1.DatabaseSize{{Name: "chino", Bytes: 7635471}, {Name: "katalog", Bytes: 9000000},
		{Name: "keycloak", Bytes: 12000000}, {Name: "portal", Bytes: 8000000}}, e.z.Status.Storage.Databases, "by their key")

	c := e.condition()
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionTrue, c.Status)
	assert.Equal(t, "Measured", c.Reason)
	assert.Equal(t, "media and postgres-data on one disk: 20.0 GiB free of 50.0 GiB; the databases take 34.9 MiB", c.Message)

	t.Run("R.C7 the same id on another node is another disk, and a filesystem without one joins nothing", func(t *testing.T) {
		e.clock = e.clock.Add(time.Hour)
		e.pass(false)
		e.end("media", "node-b", 0, libraryReport(50*gib, 20*gib, "b9e2"))
		e.pass(false)
		assert.Equal(t, "node-b:b9e2", e.volume("media").Filesystem)
		assert.Equal(t, "media 20.0 GiB free of 50.0 GiB; postgres-data 8.0 GiB free of 10.0 GiB; the databases take 34.9 MiB", e.condition().Message)

		e.clock = e.clock.Add(time.Hour)
		e.pass(false)
		e.end("media", "node-a", 0, libraryReport(50*gib, 20*gib, "0"))
		e.pass(false)
		assert.Empty(t, e.volume("media").Filesystem, "a filesystem that reports no id, as a network share does")
		assert.Empty(t, e.volume("postgres-data").Filesystem)
		assert.Equal(t, "media 20.0 GiB free of 50.0 GiB; postgres-data 8.0 GiB free of 10.0 GiB; the databases take 34.9 MiB", e.condition().Message)
	})

	t.Run("R.C7 a zero that means something survives", func(t *testing.T) {
		e.clock = e.clock.Add(time.Hour)
		e.pass(false)
		e.end("media", "node-a", 0, libraryReport(50*gib, 0, "b9e2"))
		e.pass(false)
		raw, err := json.Marshal(e.z.Status.Storage)
		require.NoError(t, err)
		assert.Contains(t, string(raw), `"name":"media","claim":"media","requestBytes":53687091200,"sizeBytes":53687091200,"freeBytes":0`)
	})
}

func TestRC7AFailedRunKeepsWhatItDidNotMeasure(t *testing.T) {
	t.Run("R.C7 what a failed run did measure is written, the rest stays with its time", func(t *testing.T) {
		e := newStorageEnv(t, verifyCR())
		e.claim("media", "50Gi", true)
		e.claim("postgres-data", "10Gi", true)
		e.postgresPod("node-a")
		e.pass(false)
		e.end("media", "node-a", 0, libraryReport(50*gib, 20*gib, "b9e2"))
		e.pass(false)
		first := e.clock

		e.clock = e.clock.Add(time.Hour)
		e.pass(false)
		job := e.end("media", "node-a", 1, `{"v":1,"size":53687091200,"free":10737418240,"fs":"b9e2","failed":"the Postgres at postgres does not answer"}`)
		e.pass(false)
		assert.Equal(t, 10*gib, *e.volume("media").FreeBytes, "the library was measured")
		assert.True(t, e.clock.Equal(e.volume("media").MeasuredAt.Time))
		assert.Equal(t, 8*gib, *e.volume("postgres-data").FreeBytes, "the databases' volume stays")
		assert.True(t, first.Equal(e.volume("postgres-data").MeasuredAt.Time), "with its time")
		assert.Len(t, e.z.Status.Storage.Databases, 4, "and the databases' sizes")
		c := e.condition()
		assert.Equal(t, metav1.ConditionFalse, c.Status)
		assert.Equal(t, "Failed", c.Reason)
		assert.Equal(t, job+" failed: the Postgres at postgres does not answer; the figures of "+at(first)+" stay", c.Message)

		e.pass(false)
		assert.Equal(t, c.Message, e.condition().Message, "the same on every pass that reads the run again")
	})

	t.Run("R.C7 a run that measured a part, nothing kept from before for the rest, ends with its reason", func(t *testing.T) {
		e := newStorageEnv(t, verifyCR())
		e.claim("media", "50Gi", true)
		e.claim("postgres-data", "10Gi", true)
		e.pass(false)
		job := e.end("media", "node-a", 1, `{"v":1,"size":53687091200,"free":10737418240,"fs":"b9e2","failed":"the Postgres at postgres does not answer"}`)
		e.pass(false)
		assert.Equal(t, job+" failed: the Postgres at postgres does not answer", e.condition().Message,
			"the library just measured: not \"nothing is measured yet\"")
	})

	t.Run("R.C7 a first run that fails: nothing is measured yet", func(t *testing.T) {
		e := newStorageEnv(t, verifyCR())
		e.claim("media", "50Gi", true)
		e.pass(false)
		job := e.end("media", "node-a", 1, `{"v":1,"failed":"/measure is not a directory this measuring can read"}`)
		e.pass(false)
		assert.Nil(t, e.volume("media").SizeBytes)
		assert.Equal(t, job+" failed: /measure is not a directory this measuring can read; nothing is measured yet", e.condition().Message)
	})

	t.Run("R.C7 the ending is kept whole however long the reason is", func(t *testing.T) {
		e := newStorageEnv(t, verifyCR())
		e.claim("media", "50Gi", true)
		e.pass(false)
		e.end("media", "node-a", 0, libraryReport(50*gib, 20*gib, "b9e2"))
		e.pass(false)
		first := e.clock
		e.clock = e.clock.Add(time.Hour)
		e.pass(false)
		reason := "cannot read the filesystem at /measure/" + strings.Repeat("x", 700)
		job := e.end("media", "node-a", 1, `{"v":1,"failed":"`+reason+`"}`)
		e.pass(false)
		msg := e.condition().Message
		assert.True(t, strings.HasPrefix(msg, job+" failed: cannot read the filesystem at /measure/xxx"), msg)
		assert.True(t, strings.HasSuffix(msg, "…; the figures of "+at(first)+" stay"), msg)
		assert.LessOrEqual(t, utf8.RuneCountInString(msg), maxVerifyMessage)
	})

	t.Run("R.C7 an older run never takes the place of newer figures", func(t *testing.T) {
		e := newStorageEnv(t, verifyCR())
		e.claim("media", "50Gi", true)
		e.pass(false)
		e.end("media", "node-a", 0, libraryReport(50*gib, 20*gib, "b9e2"))
		e.pass(false)
		e.clock = e.clock.Add(time.Hour)
		e.pass(false)
		newest := e.end("media", "node-a", 0, libraryReport(50*gib, 10*gib, "b9e2"))
		e.pass(false)
		require.Equal(t, 10*gib, *e.volume("media").FreeBytes)
		require.NoError(t, e.c.Delete(context.Background(), &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: newest, Namespace: e.z.Namespace}}))
		e.clock = e.clock.Add(time.Hour)
		e.pass(false)
		assert.Equal(t, 10*gib, *e.volume("media").FreeBytes, "the run left is older than what status holds")
	})

	t.Run("R.C7 a run without a readable report: not JSON, cut, a size below zero, or none", func(t *testing.T) {
		for _, report := range []string{
			`not json`,
			`{"v":1,"size":53687091200,"free":214748`,
			`{"v":1,"size":-1,"free":0,"fs":"b9e2"}`,
			`{"v":2,"size":1,"free":1,"fs":"b9e2"}`,
			``,
		} {
			e := newStorageEnv(t, verifyCR())
			e.claim("media", "50Gi", true)
			e.pass(false)
			job := e.end("media", "node-a", 0, report)
			e.pass(false)
			assert.Equal(t, job+" failed: "+job+" left no report the operator can read; nothing is measured yet", e.condition().Message, report)
			assert.Nil(t, e.volume("media").SizeBytes, report)
		}
	})
}

// Where a run left no Job, no pod or no report to read, the operator says so
// in the words of the chapter.
func TestRC7WhereARunLeftNothingToRead(t *testing.T) {
	t.Run("R.C7 there is no claim", func(t *testing.T) {
		z := verifyCR()
		z.Spec.Storage.KafkaPVC = "kafka-log"
		e := newStorageEnv(t, z)
		e.claim("media", "50Gi", true)
		e.pass(false)
		e.end("media", "node-a", 0, libraryReport(50*gib, 20*gib, "b9e2"))
		e.pass(false)
		assert.Empty(t, e.runs("kafka"), "no run on a claim that is not there")
		v := e.volume("kafka")
		require.NotNil(t, v, "listed, not measured")
		assert.Nil(t, v.RequestBytes)
		assert.Equal(t, "zaentrum-measure-kafka failed: there is no claim kafka-log; nothing is measured yet", e.condition().Message)
	})

	t.Run("R.C7 a run the cluster refuses to create has not started, and is tried again on the hour", func(t *testing.T) {
		e := newStorageEnv(t, verifyCR())
		e.claim("media", "50Gi", true)
		e.refuse = `admission webhook "quota.example.org" denied the request: no Jobs today`
		e.pass(false)
		assert.Equal(t, 1, e.creates)
		msg := e.condition().Message
		m := regexp.MustCompile(`^(zaentrum-measure-media-[a-z0-9]{5}) failed: (zaentrum-measure-media-[a-z0-9]{5}) has not started: ` +
			`admission webhook "quota.example.org" denied the request: no Jobs today; nothing is measured yet$`).FindStringSubmatch(msg)
		require.NotNil(t, m, msg)
		assert.Equal(t, m[1], m[2])
		e.clock = e.clock.Add(10 * time.Second)
		e.pass(false)
		e.clock = e.clock.Add(30 * time.Minute)
		e.pass(false)
		assert.Equal(t, 1, e.creates, "not again on every pass")
		e.clock = e.clock.Add(31 * time.Minute)
		e.refuse = ""
		e.pass(false)
		assert.Equal(t, 2, e.creates, "on the hour")
		assert.Len(t, e.runs("media"), 1)
	})

	t.Run("R.C7 a run whose pod never starts has not started, saying why it waited", func(t *testing.T) {
		e := newStorageEnv(t, verifyCR())
		e.claim("media", "50Gi", true)
		e.pass(false)
		run := e.runs("media")[0]
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: run.Name + "-x", Namespace: e.z.Namespace,
			Labels: map[string]string{batchv1.JobNameLabel: run.Name}},
			Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled,
				Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: "0/2 nodes are available: 2 node(s) had volume node affinity conflict"}}}}
		require.NoError(t, e.c.Create(context.Background(), pod))
		e.clock = e.clock.Add(10 * time.Second)
		e.pass(false)
		assert.Equal(t, "the pod is not scheduled: Unschedulable: 0/2 nodes are available: 2 node(s) had volume node affinity conflict",
			e.runs("media")[0].Annotations[annotationMeasureWaiting], "noted on the run while it waits")

		require.NoError(t, e.c.Delete(context.Background(), pod))
		e.clock = e.clock.Add(10 * time.Minute)
		run = e.runs("media")[0]
		run.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "DeadlineExceeded",
			Message: "Job was active longer than specified deadline", LastTransitionTime: metav1.NewTime(e.clock)}}
		require.NoError(t, e.c.Status().Update(context.Background(), &run))
		e.pass(false)
		assert.Equal(t, run.Name+" failed: "+run.Name+" has not started: the pod is not scheduled: Unschedulable: "+
			"0/2 nodes are available: 2 node(s) had volume node affinity conflict; nothing is measured yet", e.condition().Message)

		e.clock = e.clock.Add(time.Hour)
		e.pass(false)
		run = e.runs("media")[0]
		run.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "DeadlineExceeded",
			Message: "Job was active longer than specified deadline", LastTransitionTime: metav1.NewTime(e.clock)}}
		require.NoError(t, e.c.Status().Update(context.Background(), &run))
		e.pass(false)
		assert.Equal(t, run.Name+" failed: "+run.Name+" has not started: DeadlineExceeded: Job was active longer than specified deadline; "+
			"nothing is measured yet", e.condition().Message, "where nothing was seen of it, the Job's own words")
	})

	t.Run("R.C7 a run gone before it was read", func(t *testing.T) {
		e := newStorageEnv(t, verifyCR())
		e.claim("media", "50Gi", true)
		e.pass(false)
		e.clock = e.clock.Add(10 * time.Second)
		e.pass(false)
		run := e.runs("media")[0]
		require.NoError(t, e.c.Delete(context.Background(), &run))
		e.clock = e.clock.Add(10 * time.Second)
		e.pass(false)
		assert.Equal(t, run.Name+" failed: "+run.Name+" is gone before it was read; nothing is measured yet", e.condition().Message)
		assert.Empty(t, e.runs("media"), "not again before the hour")
		e.clock = e.clock.Add(time.Hour)
		e.pass(false)
		assert.Len(t, e.runs("media"), 1)
	})
}

// A pass that cannot read a claim or a run changes nothing it would have read
// from it, answers nothing, and reads again.
func TestRC7APassThatCannotReadAnswersNothing(t *testing.T) {
	t.Run("R.C7 a claim that cannot be read keeps its size and holds the answer back", func(t *testing.T) {
		e := newStorageEnv(t, verifyCR())
		e.claim("media", "50Gi", true)
		e.request("r1")
		e.pass(false)
		e.end("media", "node-a", 0, libraryReport(50*gib, 20*gib, "b9e2"))
		e.unreadClaims["media"] = true
		assert.True(t, e.pass(false), "read again soon")
		assert.Empty(t, e.answered())
		assert.Equal(t, 50*gib, *e.volume("media").RequestBytes, "not taken for a claim that is not there")
		e.unreadClaims["media"] = false
		e.pass(false)
		assert.Equal(t, "r1", e.answered())
	})

	t.Run("R.C7 a run whose pods cannot be read is not read, and holds the answer back", func(t *testing.T) {
		e := newStorageEnv(t, verifyCR())
		e.claim("media", "50Gi", true)
		e.request("r1")
		e.pass(false)
		e.end("media", "node-a", 0, libraryReport(50*gib, 20*gib, "b9e2"))
		e.unreadPods = true
		e.pass(false)
		assert.Empty(t, e.answered())
		assert.Nil(t, e.volume("media").SizeBytes)
		assert.Nil(t, e.condition())
		e.unreadPods = false
		e.pass(false)
		assert.Equal(t, 20*gib, *e.volume("media").FreeBytes)
		assert.Equal(t, "r1", e.answered())
	})
}

func TestRC7EveryHourNeverTwoRunsAtOnce(t *testing.T) {
	e := newStorageEnv(t, verifyCR())
	e.claim("media", "50Gi", true)
	e.pass(false)
	require.Len(t, e.runs("media"), 1)
	e.pass(false)
	assert.Len(t, e.runs("media"), 1, "never two runs of one Job at once")
	e.end("media", "node-a", 0, libraryReport(50*gib, 20*gib, "b9e2"))

	e.clock = e.clock.Add(30 * time.Minute)
	e.pass(false)
	assert.Len(t, e.runs("media"), 1, "not before the hour")
	e.clock = e.clock.Add(31 * time.Minute)
	assert.True(t, e.pass(false))
	assert.Len(t, e.runs("media"), 2, "the hour: a new run, and the one before kept")
	e.clock = e.clock.Add(9 * time.Minute)
	e.pass(false)
	assert.Len(t, e.runs("media"), 2, "the run in flight, inside its deadline, is not joined by another")
	e.end("media", "node-a", 0, libraryReport(50*gib, 20*gib, "b9e2"))
	e.clock = e.clock.Add(time.Hour)
	e.pass(false)
	assert.Len(t, e.runs("media"), 2, "the earlier runs go but the latest that ended")
}

func TestRC7ARequestStartsEveryJobAndIsAnsweredOnceTheyHaveEnded(t *testing.T) {
	z := verifyCR()
	z.Spec.Storage.KafkaPVC = "kafka-log"
	e := newStorageEnv(t, z)
	e.claim("media", "50Gi", true)
	e.claim("backups", "5Gi", true)
	e.claim("kafka-log", "1Gi", true)
	e.pass(false)
	e.end("media", "node-a", 0, libraryReport(50*gib, 20*gib, "b9e2"))
	e.end("backups", "node-a", 0, backupsReport)
	e.end("kafka", "node-a", 0, kafkaReport)
	e.pass(false)

	e.clock = e.clock.Add(10 * time.Minute)
	e.request("r1")
	assert.True(t, e.pass(false))
	for _, volume := range []string{"media", "backups", "kafka"} {
		runs := e.runs(volume)
		require.Len(t, runs, 2, volume)
		assert.Equal(t, "r1", runs[0].Annotations[zaentrumv1alpha1.StorageRequestAnnotation], "%s: every Job at once", volume)
	}
	assert.Empty(t, e.answered())
	e.end("media", "node-a", 0, libraryReport(50*gib, 20*gib, "b9e2"))
	e.end("backups", "node-a", 0, backupsReport)
	assert.True(t, e.pass(false))
	assert.Empty(t, e.answered(), "the event broker's run still runs")
	e.end("kafka", "node-a", 0, kafkaReport)
	assert.False(t, e.pass(false))
	assert.Equal(t, "r1", e.answered(), "answered once every Job it started has ended")

	t.Run("R.C7 a request that comes while a run is in flight waits for it, and starts its own", func(t *testing.T) {
		e.clock = e.clock.Add(time.Hour)
		e.pass(false)
		hourly := e.runs("media")[0].Name
		e.end("backups", "node-a", 0, backupsReport)
		e.end("kafka", "node-a", 0, kafkaReport)
		e.request("r2")
		e.pass(false)
		assert.Equal(t, hourly, e.runs("media")[0].Name, "no second run beside the one in flight")
		assert.Equal(t, "r2", e.runs("backups")[0].Annotations[zaentrumv1alpha1.StorageRequestAnnotation])
		e.end("media", "node-a", 0, libraryReport(50*gib, 20*gib, "b9e2"))
		e.pass(false)
		assert.Equal(t, "r2", e.runs("media")[0].Annotations[zaentrumv1alpha1.StorageRequestAnnotation], "its own run, after the one before")
		assert.Equal(t, "r1", e.answered())
		e.end("media", "node-a", 0, libraryReport(50*gib, 20*gib, "b9e2"))
		e.end("backups", "node-a", 0, backupsReport)
		e.end("kafka", "node-a", 0, kafkaReport)
		e.pass(false)
		assert.Equal(t, "r2", e.answered())
	})
}

// A value asked again after another was answered is a new request: a run of
// the same value from before does not answer it.
func TestRC7AValueAskedAgainIsANewRequest(t *testing.T) {
	e := newStorageEnv(t, verifyCR())
	e.claim("media", "50Gi", true)
	e.request("r1")
	e.pass(false)
	first := e.runs("media")[0].Name
	e.end("media", "node-a", 0, libraryReport(50*gib, 20*gib, "b9e2"))
	e.pass(false)
	require.Equal(t, "r1", e.answered())
	e.request("r2")
	e.pass(false)
	e.end("media", "node-a", 0, libraryReport(50*gib, 20*gib, "b9e2"))
	e.pass(false)
	require.Equal(t, "r2", e.answered())
	require.Equal(t, first, e.runs("media")[1].Name, "the run that answered r1 is still there")

	e.request("r1")
	assert.True(t, e.pass(false))
	assert.Equal(t, "r2", e.answered(), "not answered by the run of the r1 before")
	run := e.runs("media")[0]
	assert.Equal(t, "r1", run.Annotations[zaentrumv1alpha1.StorageRequestAnnotation])
	assert.Equal(t, "r2", run.Annotations[annotationMeasureAnswered], "started after r2 was answered")
	e.end("media", "node-a", 0, libraryReport(50*gib, 20*gib, "b9e2"))
	e.pass(false)
	assert.Equal(t, "r1", e.answered())
}

func TestRC7NothingStartsWhileARestoreRuns(t *testing.T) {
	e := newStorageEnv(t, verifyCR())
	e.claim("media", "50Gi", true)
	e.claim("backups", "5Gi", true)
	e.request("during")
	assert.True(t, e.pass(true), "the request waits")
	assert.Empty(t, e.runs("media"), "no run starts while a restore runs")
	assert.Empty(t, e.runs("backups"))
	assert.Empty(t, e.answered())

	e.pass(false)
	assert.Equal(t, "during", e.runs("media")[0].Annotations[zaentrumv1alpha1.StorageRequestAnnotation], "answered after the restore")

	t.Run("R.C7 a restore waits for a run that still measures the backups' claim", func(t *testing.T) {
		assert.Equal(t, e.runs("backups")[0].Name, e.r.backupsMeasuring(context.Background(), e.z))
		e.end("backups", "node-a", 0, backupsReport)
		assert.Empty(t, e.r.backupsMeasuring(context.Background(), e.z))
	})
}

// A restore that is stopping the database's clients waits for a run on the
// backups' claim to end before its own Job starts.
func TestRC7ARestoreWaitsForTheBackupsClaimToBeMeasured(t *testing.T) {
	e := newBackupEnv(t, verifyCR())
	e.runningPlatform()
	ctx := context.Background()
	run := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "zaentrum-measure-backups-abcde", Namespace: e.z.Namespace,
		Labels: map[string]string{templates.LabelMeasure: templates.VolumeBackups},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "zaentrum.io/v1alpha1", Kind: "Zaentrum", Name: e.z.Name,
			UID: e.z.UID, Controller: ptrTo(true)}}}}
	require.NoError(t, e.c.Create(ctx, run))
	e.askRestore(aDump)
	dump, _ := e.restorePass(false)
	assert.Equal(t, aDump, dump)
	e.stopClients()
	_, inFlight := e.restorePass(false)
	assert.True(t, inFlight)
	assert.Equal(t, zaentrumv1alpha1.RestoreStopping, e.z.Status.Backup.Restore.Result)
	assert.Equal(t, "restoring "+aDump+": waiting for "+run.Name+", which measures the backups' claim, to end", e.z.Status.Backup.Restore.Message)
	assert.Empty(t, restoreJobs(t, e.c, e.z.Namespace))

	run.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(e.clock)}}
	require.NoError(t, e.c.Status().Update(ctx, run))
	_, _ = e.restorePass(false)
	assert.Equal(t, zaentrumv1alpha1.RestoreRunning, e.z.Status.Backup.Restore.Result)
}

func setsReport(total int, names ...string) string {
	var sets []string
	for i, n := range names {
		own := map[bool]string{true: "true", false: "false"}[i%2 == 0]
		if i == 2 {
			own = "null"
		}
		sets = append(sets, fmt.Sprintf(`[%q,%d,"v0.5.0",%t,%s]`, n, 1024*(i+1), i != 1, own))
	}
	return fmt.Sprintf(`{"v":1,"size":5368709120,"free":4294967296,"fs":"c1","total":%d,"sets":[%s]}`, total, strings.Join(sets, ","))
}

func TestRC7TheBackupsClaimAfterEachBackupAndItsSets(t *testing.T) {
	e := newStorageEnv(t, verifyCR())
	e.claim("media", "50Gi", true)
	e.claim("backups", "5Gi", true)
	e.backupRun("zaentrum-backup-1", false)
	e.pass(false)
	assert.Len(t, e.runs("media"), 1)
	assert.Empty(t, e.runs("backups"), "the backups' hourly run is skipped while a backup runs")
	e.end("media", "node-a", 0, libraryReport(50*gib, 20*gib, "b9e2"))

	e.clock = e.clock.Add(20 * time.Minute)
	e.backupRun("zaentrum-backup-1", true)
	e.pass(false)
	require.Len(t, e.runs("backups"), 1, "measured when a backup has ended")
	e.end("backups", "node-a", 0, setsReport(35, "2026-11-15T00-00-05Z", "2026-11-14T00-00-05Z", "2026-11-13T00-00-05Z"))
	e.pass(false)
	sets := e.z.Status.Backup.Sets
	require.Len(t, sets, 3)
	assert.Equal(t, zaentrumv1alpha1.BackupSet{Name: "2026-11-15T00-00-05Z", Bytes: 1024, Version: "v0.5.0", Whole: true, Own: ptrTo(true)}, sets[0])
	assert.Equal(t, zaentrumv1alpha1.BackupSet{Name: "2026-11-14T00-00-05Z", Bytes: 2048, Version: "v0.5.0", Whole: false, Own: ptrTo(false)}, sets[1])
	assert.Nil(t, sets[2].Own, "nothing says who made it")
	assert.Equal(t, "media 20.0 GiB free of 50.0 GiB; postgres-data 8.0 GiB free of 10.0 GiB; backups 4.0 GiB free of 5.0 GiB; "+
		"the databases take 34.9 MiB; 35 sets on backups, the newest 3 listed", e.condition().Message)
	raw, err := json.Marshal(sets[1])
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"whole":false`)

	t.Run("R.C7 a backup that ends within the hour has the claim measured again", func(t *testing.T) {
		e.clock = e.clock.Add(10 * time.Minute)
		e.backupRun("zaentrum-backup-2", false)
		e.pass(false)
		before := e.runs("backups")[0].Name
		e.clock = e.clock.Add(5 * time.Minute)
		e.backupRun("zaentrum-backup-2", true)
		e.pass(false)
		run := e.runs("backups")[0]
		assert.NotEqual(t, before, run.Name, "a new run, though the hour is not up")
		assert.True(t, e.clock.Equal(run.CreationTimestamp.Time))
		e.end("backups", "node-a", 0, setsReport(35, "2026-11-15T00-00-05Z", "2026-11-14T00-00-05Z", "2026-11-13T00-00-05Z"))
		e.pass(false)
	})

	t.Run("R.C7 one set: 1 set on the claim", func(t *testing.T) {
		e.clock = e.clock.Add(time.Hour)
		e.pass(false)
		e.end("backups", "node-a", 0, setsReport(1, "2026-11-15T00-00-05Z"))
		e.end("media", "node-a", 0, libraryReport(50*gib, 20*gib, "b9e2"))
		e.pass(false)
		assert.True(t, strings.HasSuffix(e.condition().Message, "; 1 set on backups"), e.condition().Message)
	})

	t.Run("R.C7 one set asked for by name, however old, as a restore asks", func(t *testing.T) {
		started, err := e.r.measureSet(context.Background(), e.z, e.hooks, "2026-01-01T00-00-05Z", false)
		require.NoError(t, err)
		assert.True(t, started)
		run := e.runs("backups")[0]
		assert.Equal(t, "2026-01-01T00-00-05Z", run.Annotations[annotationMeasureSet])
		env := map[string]string{}
		for _, v := range run.Spec.Template.Spec.Containers[0].Env {
			env[v.Name] = v.Value
		}
		assert.Equal(t, "2026-01-01T00-00-05Z", env["SET"])
		started, err = e.r.measureSet(context.Background(), e.z, e.hooks, "2026-01-01T00-00-05Z", false)
		require.NoError(t, err)
		assert.False(t, started, "never two runs at once")
		e.end("backups", "node-a", 0, `{"v":1,"size":5368709120,"free":4294967296,"fs":"c1","total":40,`+
			`"sets":[["2026-11-15T00-00-05Z",1024,"v0.5.0",true,true]],"set":["2026-01-01T00-00-05Z",2048,"v0.4.0",true,true]}`)
		e.pass(false)
		var names []string
		for _, s := range e.z.Status.Backup.Sets {
			names = append(names, s.Name)
		}
		assert.Equal(t, []string{"2026-11-15T00-00-05Z", "2026-01-01T00-00-05Z"}, names, "the newest, then the one asked for")
		assert.Contains(t, e.condition().Message, "40 sets on backups, the newest 1 listed")
	})

	t.Run("R.C7 backups off: the backups' claim is measured all the same", func(t *testing.T) {
		no := false
		e.z.Spec.Backup.Enabled = &no
		e.render()
		e.clock = e.clock.Add(3 * time.Hour)
		n := len(e.runs("backups"))
		e.request("off")
		e.pass(false)
		require.NotEmpty(t, e.runs("backups"))
		assert.Equal(t, "off", e.runs("backups")[0].Annotations[zaentrumv1alpha1.StorageRequestAnnotation], "on a request")
		assert.LessOrEqual(t, n, len(e.runs("backups"))+1)
		e.end("media", "node-a", 0, libraryReport(50*gib, 20*gib, "b9e2"))
		e.end("backups", "node-a", 0, setsReport(2, "2026-11-15T00-00-05Z", "2026-11-14T00-00-05Z"))
		e.pass(false)
		assert.Equal(t, "off", e.answered())
		assert.Len(t, e.z.Status.Backup.Sets, 2)
		assert.Contains(t, e.condition().Message, "; 2 sets on backups")
	})
}

// The backups' claim is measured wherever it is there: where there is none,
// it is not among the volumes and no set is listed.
func TestRC7TheBackupsClaimIsMeasuredWhereverItIsThere(t *testing.T) {
	e := newStorageEnv(t, verifyCR())
	e.claim("media", "50Gi", true)
	e.claim("backups", "5Gi", true)
	e.pass(false)
	e.end("backups", "node-a", 0, setsReport(2, "2026-11-15T00-00-05Z", "2026-11-14T00-00-05Z"))
	e.end("media", "node-a", 0, libraryReport(50*gib, 20*gib, "b9e2"))
	e.pass(false)
	require.Len(t, e.z.Status.Backup.Sets, 2)

	require.NoError(t, e.c.Delete(context.Background(), &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "backups", Namespace: e.z.Namespace}}))
	e.pass(false)
	assert.Nil(t, e.volume("backups"), "no claim: not among the volumes")
	assert.Empty(t, e.z.Status.Backup.Sets, "and no set")
	assert.NotContains(t, e.condition().Message, "backups", "nor a failure")
}

// Where the database server may not read its own volume, that is no failure:
// the volume is listed with the size its claim asks for alone, and the
// condition says so.
func TestRC7AServerThatMayNotReadItsOwnVolume(t *testing.T) {
	const refused = `{"v":1,"size":53687091200,"free":21474836480,"fs":"b9e2",` + databasesReport +
		`,"postgres":{"refused":"permission denied to COPY to or from an external program"}}`
	e := newStorageEnv(t, verifyCR())
	e.claim("media", "50Gi", true)
	e.claim("postgres-data", "10Gi", true)
	e.postgresPod("node-a")
	e.pass(false)
	e.end("media", "node-a", 0, libraryReport(50*gib, 20*gib, "b9e2"))
	e.pass(false)
	require.NotNil(t, e.volume("postgres-data").FreeBytes)

	e.clock = e.clock.Add(time.Hour)
	e.pass(false)
	e.end("media", "node-a", 0, refused)
	e.pass(false)
	assert.Equal(t, zaentrumv1alpha1.VolumeStatus{Name: "postgres-data", Claim: "postgres-data", RequestBytes: ptrTo(10 * gib)},
		*e.volume("postgres-data"), "the size its claim asks for alone: the figures before are gone")
	c := e.condition()
	assert.Equal(t, metav1.ConditionTrue, c.Status, "no failure")
	assert.Equal(t, "Measured", c.Reason)
	assert.Equal(t, "media 20.0 GiB free of 50.0 GiB; the databases take 34.9 MiB; "+
		"the databases' volume is not measured: permission denied to COPY to or from an external program", c.Message)

	t.Run("R.C7 the clip takes what is cut from the rest, never from why the databases' volume is not measured", func(t *testing.T) {
		long := func(c string) string {
			return strings.Join([]string{strings.Repeat(c, 60), strings.Repeat(c, 60), strings.Repeat(c, 60), strings.Repeat(c, 60)}, ".")
		}
		z := verifyCR()
		z.Spec.Storage.KafkaPVC = long("k")
		z.Spec.Backup.ClaimName = long("b")
		e := newStorageEnv(t, z)
		e.claim("media", "50Gi", true)
		e.claim("postgres-data", "10Gi", true)
		e.claim(long("k"), "1Gi", true)
		e.claim(long("b"), "5Gi", true)
		e.pass(false)
		e.end("kafka", "node-b", 0, kafkaReport)
		e.end("backups", "node-c", 0, backupsReport)
		e.end("media", "node-a", 0, refused)
		e.pass(false)
		msg := e.condition().Message
		assert.True(t, strings.HasSuffix(msg, "…; the databases' volume is not measured: permission denied to COPY to or from an external program"), msg)
		assert.LessOrEqual(t, utf8.RuneCountInString(msg), maxVerifyMessage)
	})
}

func TestRC7AVolumeNoLongerUsedLeavesTheList(t *testing.T) {
	z := verifyCR()
	z.Spec.Storage.KafkaPVC = "kafka-log"
	e := newStorageEnv(t, z)
	e.claim("media", "50Gi", true)
	e.claim("kafka-log", "1Gi", true)
	e.pass(false)
	require.Len(t, e.runs("kafka"), 1)
	e.z.Spec.Storage.KafkaPVC = ""
	e.render()
	e.pass(false)
	assert.Nil(t, e.volume("kafka"))
	assert.Empty(t, e.runs("kafka"), "its runs go")

	e.postgres = "emptyDir"
	e.render()
	e.pass(false)
	assert.Nil(t, e.volume("postgres-data"), "a database server without a claim of its own has no volume listed")
	assert.NotNil(t, e.volume("media"))
}

// A claim the resource names now is measured at once, and what a run of the
// claim before found says nothing of it: neither its figures nor its sets.
func TestRC7AnotherClaimIsMeasuredAfresh(t *testing.T) {
	e := newStorageEnv(t, verifyCR())
	e.claim("media", "50Gi", true)
	e.claim("backups", "5Gi", true)
	e.claim("kept-backups", "20Gi", true)
	e.pass(false)
	e.end("backups", "node-a", 0, setsReport(3, "2026-11-15T00-00-05Z", "2026-11-14T00-00-05Z", "2026-11-13T00-00-05Z"))
	e.end("media", "node-a", 0, libraryReport(50*gib, 20*gib, "b9e2"))
	e.pass(false)
	require.Len(t, e.z.Status.Backup.Sets, 3)
	require.Contains(t, e.condition().Message, "3 sets on backups")

	e.clock = e.clock.Add(5 * time.Minute)
	e.z.Spec.Backup.ClaimName = "kept-backups"
	e.render()
	e.pass(false)
	v := e.volume("backups")
	assert.Equal(t, "kept-backups", v.Claim)
	assert.Equal(t, 20*gib, *v.RequestBytes)
	assert.Nil(t, v.SizeBytes, "the figures of the claim before say nothing of this one")
	assert.Empty(t, e.z.Status.Backup.Sets, "nor do its sets")
	assert.NotContains(t, e.condition().Message, "sets on", "nor its run")
	run := e.runs("backups")[0]
	assert.Equal(t, "kept-backups", runClaim(&run), "measured at once, though the hour is not up")

	e.end("backups", "node-a", 0, setsReport(1, "2026-10-01T00-00-05Z"))
	e.pass(false)
	assert.Equal(t, 4*gib, *e.volume("backups").FreeBytes)
	require.Len(t, e.z.Status.Backup.Sets, 1)
	assert.Equal(t, "2026-10-01T00-00-05Z", e.z.Status.Backup.Sets[0].Name)
	assert.Contains(t, e.condition().Message, "1 set on kept-backups")
}

// On one machine every claim is a folder of one disk: the condition names
// them together, once.
func TestRC7OneDiskForEveryClaimOfOneMachine(t *testing.T) {
	e := newStorageEnv(t, verifyCR())
	e.claim("media", "50Gi", true)
	e.claim("postgres-data", "10Gi", true)
	e.claim("backups", "5Gi", true)
	e.postgresPod("appliance")
	e.pass(false)
	e.end("media", "appliance", 0, libraryReport(100*gib, 40*gib, "5ca1ab1e"))
	e.end("backups", "appliance", 0, `{"v":1,"size":107374182400,"free":42949672960,"fs":"5ca1ab1e","total":0,"sets":[]}`)
	e.pass(false)
	assert.Equal(t, "media, postgres-data and backups on one disk: 40.0 GiB free of 100.0 GiB; the databases take 34.9 MiB; "+
		"0 sets on backups", e.condition().Message)
}

// reconcileEnv is a whole pass's API server: what the operator applies is
// created or updated, claims keep the phase they are given, and what is
// created is stamped with the clock.
func reconcileEnv(t *testing.T, z *zaentrumv1alpha1.Zaentrum, clock *time.Time, objs ...client.Object) (*ZaentrumReconciler, client.WithWatch) {
	t.Helper()
	s := selfScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(append(objs, z)...).
		WithStatusSubresource(&zaentrumv1alpha1.Zaentrum{}, &appsv1.Deployment{}, &batchv1.Job{}, &corev1.PersistentVolumeClaim{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				return applyAsCreateOrUpdate(ctx, cl, obj, patch, opts...)
			},
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if ts := obj.GetCreationTimestamp(); ts.IsZero() {
					obj.SetCreationTimestamp(metav1.NewTime(*clock))
				}
				return cl.Create(ctx, obj, opts...)
			}}).Build()
	return &ZaentrumReconciler{Client: c, Scheme: s, Now: func() time.Time { return *clock }}, c
}

func boundClaim(name string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: verifyNS},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}}
}

// Through Reconcile: the pass renders the ConfigMap zaentrum-backup and the
// measuring hooks, applies the ConfigMap and never a hook, starts a run once
// a claim is bound, and polls while it is in flight.
func TestRC7ReconcileMeasures(t *testing.T) {
	clock := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	r, c := reconcileEnv(t, verifyCR(), &clock)
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: verifyNS, Name: "zaentrum"}}

	_, err := r.Reconcile(ctx, req)
	require.NoError(t, err)
	var cm corev1.ConfigMap
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: verifyNS, Name: templates.BackupRecordName}, &cm))
	assert.Contains(t, cm.Data["platform"], `"uid":"z-uid"`)
	var jobs batchv1.JobList
	require.NoError(t, c.List(ctx, &jobs, client.InNamespace(verifyNS), client.HasLabels{templates.LabelMeasure}))
	assert.Empty(t, jobs.Items, "no claim is bound yet")
	var got zaentrumv1alpha1.Zaentrum
	require.NoError(t, c.Get(ctx, req.NamespacedName, &got))
	require.NotNil(t, got.Status.Storage)
	assert.Equal(t, "media", got.Status.Storage.Volumes[0].Name)

	var pvc corev1.PersistentVolumeClaim
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: verifyNS, Name: "media"}, &pvc))
	pvc.Status.Phase = corev1.ClaimBound
	require.NoError(t, c.Status().Update(ctx, &pvc))
	res, err := r.Reconcile(ctx, req)
	require.NoError(t, err)
	require.NoError(t, c.List(ctx, &jobs, client.InNamespace(verifyNS), client.HasLabels{templates.LabelMeasure}))
	require.Len(t, jobs.Items, 1)
	assert.Equal(t, templates.VolumeMedia, jobs.Items[0].Labels[templates.LabelMeasure])
	assert.Equal(t, verifyRequeueAfter, res.RequeueAfter, "polled while it runs")
	var hook batchv1.Job
	assert.True(t, isGone(c.Get(ctx, types.NamespacedName{Namespace: verifyNS, Name: "zaentrum-measure-media"}, &hook)),
		"the hook itself is never applied")
}

// Through Reconcile: the pass that takes a restore's request tells the
// measuring so, and no run starts, though every claim is bound.
func TestRC7ReconcileStartsNoRunWhileARestoreRuns(t *testing.T) {
	z := verifyCR()
	z.Annotations = map[string]string{zaentrumv1alpha1.RestoreRequestAnnotation: aDump}
	// The set listed, of the release that runs: the restore goes on (R.C10).
	z.Spec.Version = "v0.5.0"
	z.Status.Backup = &zaentrumv1alpha1.BackupStatus{Sets: []zaentrumv1alpha1.BackupSet{{Name: aDump, Version: "v0.5.0", Whole: true}}}
	listedOn(z, "backups")
	clock := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	r, c := reconcileEnv(t, z, &clock, boundClaim("media"), boundClaim("postgres-data"), boundClaim("backups"))
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: verifyNS, Name: "zaentrum"}}

	_, err := r.Reconcile(ctx, req)
	require.NoError(t, err)
	var got zaentrumv1alpha1.Zaentrum
	require.NoError(t, c.Get(ctx, req.NamespacedName, &got))
	require.Equal(t, "Restoring", got.Status.Phase)
	var jobs batchv1.JobList
	require.NoError(t, c.List(ctx, &jobs, client.InNamespace(verifyNS), client.HasLabels{templates.LabelMeasure}))
	assert.Empty(t, jobs.Items, "no run starts while a restore runs")
}

// failedBackupRun fails the backup Job at its deadline while its pod still
// runs, and gives that pod.
func (e *storageEnv) failedBackupRun(name string) *corev1.Pod {
	e.t.Helper()
	ctx := context.Background()
	var job batchv1.Job
	require.NoError(e.t, e.c.Get(ctx, types.NamespacedName{Namespace: e.z.Namespace, Name: name}, &job))
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
		LastTransitionTime: metav1.NewTime(e.clock), Reason: "DeadlineExceeded"}}
	require.NoError(e.t, e.c.Status().Update(ctx, &job))
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name + "-x", Namespace: e.z.Namespace,
		Labels: map[string]string{batchv1.JobNameLabel: name}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	require.NoError(e.t, e.c.Create(ctx, pod))
	return pod
}

// R.C7: the backups' claim has one reader or writer at a time. No run of the
// backups' Job starts, for the hour, after a backup or for a request, while a
// backup Job has not ended, one being deleted among them, while a pod of a
// backup that has failed still runs, or while the schedule is held for a
// backup; the pass after it starts the run that waited. The other volumes are
// measured all the same, and a request is answered without the backups' run,
// as StorageMeasured says until the claim is measured again.
func TestRC7NoRunOfTheBackupsClaimWhileABackupRunsOrMayStart(t *testing.T) {
	start := func(t *testing.T) *storageEnv {
		e := newStorageEnv(t, verifyCR())
		e.claim("media", "50Gi", true)
		e.claim("backups", "5Gi", true)
		return e
	}
	t.Run("a backup Job being deleted that has not ended", func(t *testing.T) {
		e := start(t)
		ctx := context.Background()
		e.backupRun("zaentrum-backup-1", false)
		var job batchv1.Job
		require.NoError(t, e.c.Get(ctx, types.NamespacedName{Namespace: e.z.Namespace, Name: "zaentrum-backup-1"}, &job))
		job.Finalizers = []string{"example.org/hold"}
		require.NoError(t, e.c.Update(ctx, &job))
		require.NoError(t, e.c.Delete(ctx, &job))
		e.request("r1")
		assert.True(t, e.pass(false))
		assert.Len(t, e.runs("media"), 1)
		assert.Empty(t, e.runs("backups"), "it may still write")
		e.end("media", "node-a", 0, libraryReport(50*gib, 20*gib, "b9e2"))
		e.pass(false)
		assert.Empty(t, e.runs("backups"))
		assert.Equal(t, "r1", e.answered(), "answered without the run of the backups' claim, which the backup holds")
		assert.True(t, strings.HasSuffix(e.condition().Message, "; backups was not measured: a backup holds it; nothing is measured yet"),
			e.condition().Message)

		require.NoError(t, e.c.Get(ctx, types.NamespacedName{Namespace: e.z.Namespace, Name: "zaentrum-backup-1"}, &job))
		job.Finalizers = nil
		require.NoError(t, e.c.Update(ctx, &job))
		e.pass(false)
		require.Len(t, e.runs("backups"), 1, "once it is gone")
		assert.Empty(t, e.runs("backups")[0].Annotations[zaentrumv1alpha1.StorageRequestAnnotation], "the request has its answer")
		e.end("backups", "node-a", 0, backupsReport)
		e.pass(false)
		assert.NotContains(t, e.condition().Message, "was not measured", "measured since")
	})
	t.Run("a failed backup whose pod still runs", func(t *testing.T) {
		e := start(t)
		e.backupRun("zaentrum-backup-1", false)
		pod := e.failedBackupRun("zaentrum-backup-1")
		e.pass(false)
		assert.Len(t, e.runs("media"), 1)
		assert.Empty(t, e.runs("backups"), "its pod may still write")
		e.clock = e.clock.Add(time.Minute)
		pod.Status.Phase = corev1.PodFailed
		require.NoError(t, e.c.Status().Update(context.Background(), pod))
		e.pass(false)
		assert.Len(t, e.runs("backups"), 1, "once its pod has ended")
	})
	t.Run("the schedule held for a backup", func(t *testing.T) {
		e := start(t)
		e.request("r1")
		assert.True(t, e.passHeld())
		assert.Len(t, e.runs("media"), 1)
		assert.Empty(t, e.runs("backups"), "a backup may start")
		e.end("media", "node-a", 0, libraryReport(50*gib, 20*gib, "b9e2"))
		e.passHeld()
		assert.Equal(t, "r1", e.answered(), "answered without the run of the backups' claim")
		e.clock = e.clock.Add(2 * time.Hour)
		e.passHeld()
		assert.Empty(t, e.runs("backups"), "nor on the hour")
		e.pass(false)
		require.Len(t, e.runs("backups"), 1, "once the schedule is not held for a backup")
	})
}

// R.C7: a run of the backups' Job holds the claim until its pod has ended: one
// that has failed at its deadline while its pod still runs holds it too.
func TestRC7ARunOfTheBackupsJobHoldsTheClaimUntilItsPodHasEnded(t *testing.T) {
	e := newStorageEnv(t, verifyCR())
	e.claim("backups", "5Gi", true)
	e.pass(false)
	run := e.runs("backups")[0]
	e.end("backups", "", -1, "")
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: run.Name + "-x", Namespace: e.z.Namespace,
		Labels: map[string]string{batchv1.JobNameLabel: run.Name}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	require.NoError(t, e.c.Create(context.Background(), pod))
	assert.Equal(t, run.Name, e.r.backupsMeasuring(context.Background(), e.z), "its pod may still read")
	pod.Status.Phase = corev1.PodFailed
	require.NoError(t, e.c.Status().Update(context.Background(), pod))
	assert.Empty(t, e.r.backupsMeasuring(context.Background(), e.z))
}

// R.C7: the run a restore asks for a set (measureSet) keeps the backups' claim
// to one reader or writer too: none starts while a backup runs, while its pod
// still runs after it failed, while the schedule is held for a backup, or
// while a run of the backups' Job that failed still has its pod running.
func TestRC7NoRunForASetWhileABackupRunsOrMayStart(t *testing.T) {
	const set = "2026-01-01T00-00-05Z"
	e := newStorageEnv(t, verifyCR())
	e.claim("backups", "5Gi", true)
	ctx := context.Background()
	measure := func(held bool) bool {
		t.Helper()
		started, err := e.r.measureSet(ctx, e.z, e.hooks, set, held)
		require.NoError(t, err)
		return started
	}
	e.backupRun("zaentrum-backup-1", false)
	assert.False(t, measure(false), "a backup runs")
	pod := e.failedBackupRun("zaentrum-backup-1")
	assert.False(t, measure(false), "its pod may still write")
	pod.Status.Phase = corev1.PodFailed
	require.NoError(t, e.c.Status().Update(ctx, pod))
	assert.False(t, measure(true), "the schedule is held for a backup")
	assert.Empty(t, e.runs("backups"))

	require.True(t, measure(false), "nothing holds the claim")
	e.end("backups", "", -1, "")
	run := e.runs("backups")[0]
	runPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: run.Name + "-x", Namespace: e.z.Namespace,
		Labels: map[string]string{batchv1.JobNameLabel: run.Name}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	require.NoError(t, e.c.Create(ctx, runPod))
	assert.False(t, measure(false), "a run that failed at its deadline while its pod still reads")
	runPod.Status.Phase = corev1.PodFailed
	require.NoError(t, e.c.Status().Update(ctx, runPod))
	assert.True(t, measure(false))
}

// R.C7: a run that has not ended past its deadline and its pod's grace, as on
// a node that is gone, holds nothing from then on, and is removed and counted
// as failed; until then no other run of its Job starts beside it.
func TestRC7ARunPastItsDeadlineAndGraceIsRemoved(t *testing.T) {
	ctx := context.Background()
	graceEnds := func(run batchv1.Job) time.Time { return run.CreationTimestamp.Add(600*time.Second + 30*time.Second) }
	t.Run("a run whose Job never ends", func(t *testing.T) {
		e := newStorageEnv(t, verifyCR())
		e.claim("backups", "5Gi", true)
		e.request("r1")
		e.pass(false)
		run := e.runs("backups")[0]
		e.clock = graceEnds(run).Add(-time.Second)
		assert.Equal(t, run.Name, e.r.backupsMeasuring(ctx, e.z), "it holds the claim up to its deadline and grace")
		e.clock = graceEnds(run)
		assert.Empty(t, e.r.backupsMeasuring(ctx, e.z), "past them, it holds nothing")
		e.pass(false)
		assert.Empty(t, e.runs("backups"), "removed")
		assert.Equal(t, "r1", e.answered(), "the request it was started for is answered")
		c := e.condition()
		assert.Equal(t, "Failed", c.Reason)
		assert.True(t, strings.HasSuffix(c.Message, "; "+run.Name+" failed: "+run.Name+" had not ended at its deadline and was removed; nothing is measured yet"),
			c.Message)
	})
	t.Run("a failed run whose pod still runs", func(t *testing.T) {
		e := newStorageEnv(t, verifyCR())
		e.claim("backups", "5Gi", true)
		e.pass(false)
		run := e.runs("backups")[0]
		e.end("backups", "", -1, "")
		require.NoError(t, e.c.Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: run.Name + "-x", Namespace: e.z.Namespace,
			Labels: map[string]string{batchv1.JobNameLabel: run.Name}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}))
		e.request("r1")
		e.pass(false)
		assert.Len(t, e.runs("backups"), 1, "no second reader beside its pod")
		assert.Empty(t, e.answered())
		e.clock = graceEnds(run)
		e.pass(false)
		runs := e.runs("backups")
		require.Len(t, runs, 1, "removed, and the request's run started")
		assert.NotEqual(t, run.Name, runs[0].Name)
		assert.Equal(t, "r1", runs[0].Annotations[zaentrumv1alpha1.StorageRequestAnnotation])
	})
}

// R.C7: a request answered while a backup holds the backups' claim, with no
// other claim measured yet, leaves StorageMeasured saying so all the same.
func TestRC7ARequestDuringABackupWithNothingMeasuredYet(t *testing.T) {
	e := newStorageEnv(t, verifyCR())
	e.claim("media", "50Gi", false)
	e.claim("backups", "5Gi", true)
	e.backupRun("zaentrum-backup-1", false)
	e.request("r1")
	e.pass(false)
	require.Equal(t, "r1", e.answered(), "answered at once: no run to wait for")
	c := e.condition()
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status, "nothing is measured")
	assert.Equal(t, "NotMeasured", c.Reason)
	assert.Equal(t, "backups was not measured: a backup holds it; nothing is measured yet", c.Message)
}

// R.C7: an operator that starts again keeps the note that the backups' claim
// was not measured, until that claim has been measured since.
func TestRC7TheNoteSurvivesARestart(t *testing.T) {
	e := newStorageEnv(t, verifyCR())
	e.claim("media", "50Gi", true)
	e.claim("backups", "5Gi", true)
	e.pass(false)
	e.end("media", "node-a", 0, libraryReport(50*gib, 20*gib, "b9e2"))
	e.end("backups", "node-a", 0, backupsReport)
	e.pass(false)
	measured := e.volume("backups").MeasuredAt
	e.clock = e.clock.Add(10 * time.Minute)
	e.backupRun("zaentrum-backup-1", false)
	e.request("r1")
	e.pass(false)
	e.end("media", "node-a", 0, libraryReport(50*gib, 20*gib, "b9e2"))
	e.pass(false)
	require.Equal(t, "r1", e.answered())
	note := "; backups" + notMeasuredNote + "; the figures of " + measured.UTC().Format(timeOfFigures) + " stay"
	require.True(t, strings.HasSuffix(e.condition().Message, note), e.condition().Message)
	e.r = &ZaentrumReconciler{Client: e.c, Scheme: e.r.Scheme, Now: e.r.Now}
	e.clock = e.clock.Add(time.Minute)
	e.pass(false)
	assert.True(t, strings.HasSuffix(e.condition().Message, note), "kept by the operator that started again: "+e.condition().Message)
	e.clock = e.clock.Add(time.Minute)
	e.backupRun("zaentrum-backup-1", true)
	e.pass(false)
	e.end("backups", "node-a", 0, backupsReport)
	e.pass(false)
	assert.Equal(t, "Measured", e.condition().Reason)
	assert.NotContains(t, e.condition().Message, notMeasuredNote, "measured since")
}

// R.C7: a run whose removal was refused at first, and done later, is dated
// at the end of its grace: a run that has ended since stays the volume's
// latest.
func TestRC7ARunRemovedLateIsNotTheLatest(t *testing.T) {
	e := newStorageEnv(t, verifyCR())
	e.claim("media", "50Gi", false)
	e.claim("backups", "5Gi", true)
	e.pass(false)
	hung := e.runs("backups")[0]
	end, _ := graceEnds(&hung)
	e.clock = end
	e.refuseDeletes = 1
	e.request("r1")
	e.pass(false)
	require.Len(t, e.runs("backups"), 2, "its removal refused, the request's run beside one that holds nothing")
	e.end("backups", "node-a", 0, backupsReport)
	e.pass(false)
	assert.Len(t, e.runs("backups"), 1, "removed now")
	assert.Equal(t, "r1", e.answered())
	c := e.condition()
	assert.Equal(t, "Measured", c.Reason, c.Message)
	assert.NotContains(t, c.Message, hung.Name, "the run that ended since is the latest")
}

// R.C7: a pass that refuses the resource lets the backups' schedule move again
// once the run that measured the backups' claim and held it has ended. Each
// thing that holds it otherwise keeps it held, alone: a backup asked for and
// not answered, the platform's own backup that has not ended, the update's
// count on the CronJob, a restore, a failed backup's pod that may still
// write, and the backups' Jobs that cannot be listed.
func TestRC7ARefusedPassLetsTheScheduleMoveOnceNothingHoldsIt(t *testing.T) {
	refuse := func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.Databases.Chino = "chi no" }
	t.Run("a run of the backups' Job that has ended", func(t *testing.T) {
		e := newRequestPlatform(t, verifyCR())
		e.pass()
		run := e.measuringBackups()
		e.pass()
		require.True(t, e.suspended(), "held for the run")
		e.change(refuse)
		e.pass()
		assert.True(t, e.suspended(), "held while the run reads the claim")
		e.endBackup(run.Name, 0, "")
		out, _ := e.pass()
		assert.False(t, e.suspended(), "the run has ended: the schedule moves, though the resource is refused")
		assert.True(t, logged(out, "backups: the schedule moves again while the resource is refused"), out)
	})
	// Each of these holds the schedule alone; a refused pass keeps it held.
	heldWhileRefused := func(t *testing.T, e *platformEnv, why string) {
		t.Helper()
		require.True(t, e.suspended(), "held before the refusal")
		e.change(refuse)
		out, _ := e.pass()
		out2, _ := e.pass()
		assert.True(t, e.suspended(), why)
		assert.NotContains(t, out+out2, "the schedule moves again")
	}
	t.Run("a backup asked for, not yet answered", func(t *testing.T) {
		e := newRequestPlatform(t, verifyCR())
		e.pass()
		e.askBackup("one")
		e.pass()
		heldWhileRefused(t, e, "the backup asked for waits for the resource, and holds the schedule")
		assert.Empty(t, e.backupJobs())
	})
	t.Run("a backup asked for that runs, its request taken away", func(t *testing.T) {
		e := newRequestPlatform(t, verifyCR())
		e.pass()
		e.runAsked("one")
		e.dropAsk()
		e.pass()
		heldWhileRefused(t, e, "the platform's own backup has not ended")
	})
	t.Run("the backup before an update, not yet started", func(t *testing.T) {
		e := newUpdatePlatform(t, nil)
		e.updateTo("v0.5.0")
		e.pass()
		require.NotEmpty(t, e.cron().Annotations[annotationBackupBeforeUpdate], "the count is on the CronJob")
		require.Empty(t, e.beforeUpdate())
		heldWhileRefused(t, e, "the update waits for its backup")
	})
	t.Run("a restore", func(t *testing.T) {
		e := newRequestPlatform(t, verifyCR())
		e.pass()
		e.listOnClaim(aDump)
		e.change(func(z *zaentrumv1alpha1.Zaentrum) {
			z.Annotations = map[string]string{zaentrumv1alpha1.RestoreRequestAnnotation: aDump}
		})
		e.pass()
		require.Equal(t, zaentrumv1alpha1.RestoreRunning, e.zaentrum().Status.Backup.Restore.Result)
		heldWhileRefused(t, e, "the restore holds it")
	})
	t.Run("a failed backup whose pod may still write", func(t *testing.T) {
		e := newRequestPlatform(t, verifyCR())
		e.pass()
		e.scheduledRun("zaentrum-backup-29324160")
		e.failsWithItsPodRunning("zaentrum-backup-29324160")
		e.pass()
		heldWhileRefused(t, e, "its pod may still write")
	})
	t.Run("the backups' Jobs that cannot be listed", func(t *testing.T) {
		e := newRequestPlatform(t, verifyCR())
		e.pass()
		run := e.measuringBackups()
		e.pass()
		e.endBackup(run.Name, 0, "")
		e.refuse(func(verb string, obj any, opts *client.ListOptions) error {
			if _, jobs := obj.(*batchv1.JobList); jobs && verb == "list" && opts.LabelSelector != nil &&
				opts.LabelSelector.String() == backupJobSelector().String() {
				return errors.New("the Jobs could not be listed")
			}
			return nil
		})
		heldWhileRefused(t, e, "what cannot be read holds it")
	})
}

// R.C7: a request answered while a backup holds the backups' claim keeps the
// claim's figures from before, and StorageMeasured says when they are of.
func TestRC7ARequestDuringABackupKeepsTheBackupsFigures(t *testing.T) {
	e := newStorageEnv(t, verifyCR())
	e.claim("media", "50Gi", true)
	e.claim("backups", "5Gi", true)
	e.pass(false)
	e.end("media", "node-a", 0, libraryReport(50*gib, 20*gib, "b9e2"))
	e.end("backups", "node-a", 0, backupsReport)
	e.pass(false)
	measured := e.volume("backups").MeasuredAt
	require.NotNil(t, measured)

	e.clock = e.clock.Add(10 * time.Minute)
	e.backupRun("zaentrum-backup-1", false)
	e.request("r1")
	e.pass(false)
	assert.Len(t, e.runs("backups"), 1, "no run of its own")
	e.end("media", "node-a", 0, libraryReport(50*gib, 20*gib, "b9e2"))
	e.pass(false)
	require.Equal(t, "r1", e.answered())
	c := e.condition()
	assert.Equal(t, "Measured", c.Reason)
	assert.True(t, strings.HasSuffix(c.Message, "; backups was not measured: a backup holds it; the figures of "+
		measured.UTC().Format(timeOfFigures)+" stay"), c.Message)
}

// measuringBackups is a run of the backups' Job in flight, as the operator
// starts one.
func (e *platformEnv) measuringBackups() batchv1.Job {
	e.t.Helper()
	z := e.zaentrum()
	run := batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "zaentrum-measure-backups-abcde", Namespace: e.key.Namespace,
		Labels: map[string]string{templates.LabelMeasure: templates.VolumeBackups},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "zaentrum.io/v1alpha1", Kind: "Zaentrum", Name: z.Name,
			UID: z.UID, Controller: ptrTo(true)}}}}
	require.NoError(e.t, e.c.Create(context.Background(), &run))
	return e.job(run.Name)
}

// backupsClaimBound has the API server give the backups' claim as bound, as a
// cluster binds it: the fake one keeps no phase the apply does not write.
func (e *platformEnv) backupsClaimBound() {
	e.c = interceptor.NewClient(e.c, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			err := c.Get(ctx, key, obj, opts...)
			if pvc, ok := obj.(*corev1.PersistentVolumeClaim); ok && err == nil && key.Name == "backups" {
				pvc.Status.Phase = corev1.ClaimBound
			}
			return err
		}})
	e.r.Client = e.c
}

// backupsRuns are the runs of the backups' Job.
func (e *platformEnv) backupsRuns() []batchv1.Job {
	e.t.Helper()
	var jobs batchv1.JobList
	require.NoError(e.t, e.c.List(context.Background(), &jobs, client.InNamespace(e.key.Namespace),
		client.MatchingLabels{templates.LabelMeasure: templates.VolumeBackups}))
	return jobs.Items
}

// R.C7: while a run of the backups' Job measures the claim, the backups'
// schedule stands still, and a backup asked for waits for it.
func TestRC7NoBackupStartsWhileTheBackupsClaimIsMeasured(t *testing.T) {
	t.Run("the schedule", func(t *testing.T) {
		e := newRequestPlatform(t, verifyCR())
		e.pass()
		require.False(t, e.suspended())
		run := e.measuringBackups()
		e.pass()
		assert.True(t, e.suspended(), "one reader or writer of the claim at a time")
		e.endBackup(run.Name, 0, "")
		e.pass()
		assert.False(t, e.suspended(), "the schedule moves again")
	})
	t.Run("a request", func(t *testing.T) {
		e := newRequestPlatform(t, verifyCR())
		e.pass()
		run := e.measuringBackups()
		e.askBackup("one")
		e.pass()
		out, _ := e.pass()
		assert.True(t, logged(out, "backups: the backup asked for waits", "request", "one", "for", run.Name), out)
		assert.Empty(t, e.carrying("one"))
		pod := e.failsWithItsPodRunning(run.Name)
		out, _ = e.pass()
		assert.True(t, logged(out, "backups: the backup asked for waits", "request", "one", "for", run.Name), "its pod may still read: "+out)
		e.podEnds(pod)
		e.pass()
		assert.Len(t, e.carrying("one"), 1, "it starts once the run's pod has ended")
	})
}

// R.C7: an update first held back while a run of the backups' Job measures
// the claim: the backup before it waits for that run, with a line of its own,
// and starts once it has ended, inside the same 30 minutes, which count from
// the first hold.
func TestRC7TheBackupBeforeAnUpdateWaitsForTheBackupsClaimToBeMeasured(t *testing.T) {
	t.Run("a run that ends", func(t *testing.T) {
		e := newUpdatePlatform(t, nil)
		run := e.measuringBackups()
		e.updateTo("v0.5.0")
		out, _ := e.pass()
		first := e.clock
		assert.True(t, logged(out, "update: the backup before v0.5.0 waits for the backups' claim to be measured",
			"for", run.Name, "running", "v0.4.0"), out)
		e.clock = e.clock.Add(3 * time.Minute)
		e.pass()
		assert.Empty(t, e.beforeUpdate(), "it waits while the run reads the claim")
		assert.Equal(t, "v0.4.0", e.runs())
		e.endBackup(run.Name, 0, "")
		out, _ = e.pass()
		jobs := e.beforeUpdate()
		require.Len(t, jobs, 1, "it starts once the run has ended")
		assert.True(t, logged(out, "update: backing up before applying v0.5.0", "job", jobs[0].Name), out)
		assert.Equal(t, stamp(first), jobs[0].Annotations[annotationBackupBeforeUpdateSince], "the 30 minutes count the wait")
		assert.Contains(t, e.backupCondition().Message, "a backup that still runs at "+at(first.Add(30*time.Minute))+" is stopped")
	})
	t.Run("a run that hangs to its deadline", func(t *testing.T) {
		e := newUpdatePlatform(t, nil)
		run := e.measuringBackups()
		e.clock = e.clock.Add(time.Minute)
		e.updateTo("v0.5.0")
		e.pass()
		first := e.clock
		// The Job controller fails it at its deadline, 600 seconds after it
		// started; its pod has its grace period still.
		e.clock = run.CreationTimestamp.Add(10 * time.Minute)
		pod := e.failsWithItsPodRunning(run.Name)
		out, _ := e.pass()
		assert.True(t, logged(out, "update: the backup before v0.5.0 waits for the backups' claim to be measured", "for", run.Name), out)
		assert.Empty(t, e.beforeUpdate())
		e.clock = e.clock.Add(30 * time.Second)
		e.podEnds(pod)
		e.pass()
		require.Len(t, e.beforeUpdate(), 1, "it starts once the run's pod has ended")
		assert.GreaterOrEqual(t, first.Add(updateBackupWait).Sub(e.clock), 19*time.Minute, "at least nineteen of the 30 minutes left")
	})
}

// R.C7: no run of the backups' Job starts while a backup may start, though one
// is due: an update held back for the backup before it, or a backup asked
// for, not before that backup has started, nor while it runs; the claim is
// measured once the backup has ended and nothing holds the schedule for one.
func TestRC7NoRunOfTheBackupsClaimStartsWhileABackupMayStart(t *testing.T) {
	t.Run("an update held back", func(t *testing.T) {
		e := newUpdatePlatform(t, nil)
		e.backupsClaimBound()
		e.updateTo("v0.5.0")
		out, _ := e.pass()
		require.True(t, logged(out, "update: the backup before v0.5.0 waits for the schedule to stand still"), out)
		assert.Empty(t, e.backupsRuns(), "the claim has never been measured, and the backup may start")
		e.pass()
		jobs := e.beforeUpdate()
		require.Len(t, jobs, 1)
		e.clock = e.clock.Add(20 * time.Minute)
		e.pass()
		assert.Empty(t, e.backupsRuns(), "nor while it runs")
		e.endBackup(jobs[0].Name, 0, okSummary)
		e.pass()
		require.Equal(t, "v0.5.0", e.runs())
		assert.Len(t, e.backupsRuns(), 1, "the claim is measured after the backup")
	})
	t.Run("a backup asked for", func(t *testing.T) {
		e := newRequestPlatform(t, verifyCR())
		e.pass()
		e.backupsClaimBound()
		e.askBackup("one")
		e.pass()
		assert.Empty(t, e.backupsRuns(), "the backup may start")
		e.pass()
		jobs := e.carrying("one")
		require.Len(t, jobs, 1)
		assert.Empty(t, e.backupsRuns(), "nor while it runs")
		e.endBackup(jobs[0].Name, 0, okSummary)
		e.pass()
		require.Equal(t, "one", e.backup().Request)
		e.pass()
		assert.Len(t, e.backupsRuns(), 1, "the claim is measured once the answer is written")
	})
}
