package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
	"github.com/zaentrum/zaentrum-operator/operator/internal/templates"
	"github.com/zaentrum/zaentrum-operator/operator/internal/updates"
)

// setsOf is a backups' run's report listing sets, each [name, version], the
// version "" for a set without backup.json.
func setsOf(sets ...[2]string) string {
	var items []string
	for i, s := range sets {
		items = append(items, fmt.Sprintf(`[%q,%d,%q,true,true]`, s[0], 1024*(i+1), s[1]))
	}
	return fmt.Sprintf(`{"v":1,"size":5368709120,"free":4294967296,"fs":"c1","total":%d,"sets":[%s]}`, len(sets), strings.Join(items, ","))
}

// newCheckEnv is a platform with its claims bound, measured once, the
// backups' run having listed sets: no hourly run is due while a test asks for
// a restore.
func newCheckEnv(t *testing.T, z *zaentrumv1alpha1.Zaentrum, sets string) *storageEnv {
	t.Helper()
	e := newStorageEnv(t, z)
	e.claim("media", "50Gi", true)
	e.claim("backups", "5Gi", true)
	e.pass(false)
	e.end("media", "node-a", 0, libraryReport(50*gib, 20*gib, "b9e2"))
	e.end("backups", "node-a", 0, sets)
	e.pass(false)
	return e
}

func (e *storageEnv) annotate(key, value string) {
	ann := e.z.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[key] = value
	e.z.SetAnnotations(ann)
}

// restorePass takes a pass as Reconcile does: the plan before the render, the
// ask for the set's version after it, the restore's step, the measuring, and
// the version rule of each set listed.
func (e *storageEnv) restorePass() restorePlan {
	e.t.Helper()
	ctx := context.Background()
	e.clock = e.clock.Add(10 * time.Second)
	version := templates.NewValues(e.z).Version
	plan := e.r.planRestore(ctx, e.z, false, version)
	v := templates.NewValues(e.z)
	v.PostgresVolume = e.postgres
	v.RestoreDump = plan.dump
	objs, err := templates.Render(v)
	require.NoError(e.t, err)
	e.platform, e.hooks = templates.SplitHooks(objs)
	e.r.askForSet(ctx, e.z, e.hooks, plan)
	inFlight := e.r.stepRestore(ctx, e.z, e.platform, e.hooks)
	e.r.measureStorage(ctx, e.z, e.platform, e.hooks, inFlight || plan.dump != "")
	markVersionRules(e.z, version)
	return plan
}

// refusedOnce checks a request refused at the check: nothing stopped, no
// restore's Job, the message and the cause, and the request answered once.
func refusedOnce(t *testing.T, e *storageEnv, plan restorePlan, request, reason, message string) {
	t.Helper()
	assert.Empty(t, plan.dump, "nothing stops")
	assert.Equal(t, int64(1), rendersReplicas(t, e.platform, "chino-api"), "the clients run on")
	assert.Empty(t, restoreJobs(t, e.c, e.z.Namespace), "no restore's Job")
	rs := e.z.Status.Backup.Restore
	require.NotNil(t, rs)
	assert.Equal(t, request, rs.Request)
	assert.Equal(t, zaentrumv1alpha1.RestoreRefused, rs.Result)
	assert.Equal(t, message, rs.Message)
	c := restoreCondition(e.z)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, reason, c.Reason, "the cause, by which portal-api tells the refusals apart")
	assert.Equal(t, message, c.Message)
	first := *rs
	again := e.restorePass()
	assert.Empty(t, again.dump)
	assert.Equal(t, first, *e.z.Status.Backup.Restore, "answered once")
}

// goesOn checks a request the check let go on: the clients stop for it, and
// the restore is in flight.
func goesOn(t *testing.T, e *storageEnv, plan restorePlan, set, why string) {
	t.Helper()
	assert.Equal(t, set, plan.dump, "the clients stop: %s", why)
	require.NotNil(t, e.z.Status.Backup.Restore, why)
	assert.Contains(t, []zaentrumv1alpha1.RestoreResult{zaentrumv1alpha1.RestoreStopping, zaentrumv1alpha1.RestoreRunning},
		e.z.Status.Backup.Restore.Result, why)
}

func TestRC10ARestoreChecksTheVersionBeforeAnythingStops(t *testing.T) {
	release := func() *zaentrumv1alpha1.Zaentrum {
		z := verifyCR()
		z.Spec.Version = "v0.5.0"
		return z
	}

	t.Run("R.C10 a set of the release that runs, or of an older one, goes on", func(t *testing.T) {
		e := newCheckEnv(t, release(), setsOf([2]string{"2026-11-15T00-00-05Z", "v0.5.0"}, [2]string{aDump, "v0.4.0"}))
		e.annotate(zaentrumv1alpha1.RestoreRequestAnnotation, "2026-11-15T00-00-05Z")
		goesOn(t, e, e.restorePass(), "2026-11-15T00-00-05Z", "the release that runs")

		e = newCheckEnv(t, release(), setsOf([2]string{"2026-11-15T00-00-05Z", "v0.5.0"}, [2]string{aDump, "v0.4.0"}))
		e.annotate(zaentrumv1alpha1.RestoreRequestAnnotation, aDump+"#older")
		goesOn(t, e, e.restorePass(), aDump, "an older release is brought forward by the parts")
	})

	t.Run("R.C10 a set of a newer release is refused, whatever the annotation says", func(t *testing.T) {
		e := newCheckEnv(t, release(), setsOf([2]string{aDump, "v0.6.0"}))
		e.annotate(zaentrumv1alpha1.RestoreAnyVersionAnnotation, aDump)
		e.annotate(zaentrumv1alpha1.RestoreRequestAnnotation, aDump)
		plan := e.restorePass()
		refusedOnce(t, e, plan, aDump, "NewerVersion",
			"refused: "+aDump+" was made by v0.6.0, which is newer than this platform (v0.5.0): update the platform first; nothing was changed")
	})

	t.Run("R.C10 a version that cannot be compared goes on only where the annotation holds the set's name", func(t *testing.T) {
		e := newCheckEnv(t, release(), setsOf([2]string{aDump, ""}, [2]string{"2026-10-03T00-00-04Z", "latest"}))
		e.annotate(zaentrumv1alpha1.RestoreRequestAnnotation, aDump)
		plan := e.restorePass()
		refusedOnce(t, e, plan, aDump, "VersionNotComparable",
			"refused: the version of "+aDump+" cannot be compared with this platform's (v0.5.0): to restore it all the same, "+
				"set zaentrum.io/restore-any-version to "+aDump+" and ask again with a new value; nothing was changed")

		for i, holds := range []string{"true", "2026-10-03T00-00-04Z", aDump + "#2"} {
			e.annotate(zaentrumv1alpha1.RestoreAnyVersionAnnotation, holds)
			request := fmt.Sprintf("%s#%d", aDump, i+2)
			e.annotate(zaentrumv1alpha1.RestoreRequestAnnotation, request)
			assert.Empty(t, e.restorePass().dump, "the annotation holding %q lifts nothing", holds)
			assert.Equal(t, "VersionNotComparable", restoreCondition(e.z).Reason)
			assert.Equal(t, request, e.z.Status.Backup.Restore.Request)
		}

		e.annotate(zaentrumv1alpha1.RestoreAnyVersionAnnotation, "  "+aDump+" ")
		e.annotate(zaentrumv1alpha1.RestoreRequestAnnotation, aDump+"#again")
		goesOn(t, e, e.restorePass(), aDump, "the annotation holds the set's name, its spaces trimmed")
		assert.Equal(t, "  "+aDump+" ", e.z.GetAnnotations()[zaentrumv1alpha1.RestoreAnyVersionAnnotation], "read, never removed")

		e = newCheckEnv(t, release(), setsOf([2]string{aDump, "latest"}))
		e.annotate(zaentrumv1alpha1.RestoreRequestAnnotation, aDump)
		assert.Empty(t, e.restorePass().dump, "a release against another tag")
		assert.Equal(t, "VersionNotComparable", restoreCondition(e.z).Reason)
	})

	t.Run("R.C10 any other tag goes on into a platform that runs that very tag", func(t *testing.T) {
		for _, tag := range []string{"latest", "0.5", "sha-0123456789ab"} {
			z := verifyCR()
			z.Spec.Version = tag
			e := newCheckEnv(t, z, setsOf([2]string{aDump, tag}))
			e.annotate(zaentrumv1alpha1.RestoreRequestAnnotation, aDump)
			goesOn(t, e, e.restorePass(), aDump, tag)
		}
	})
}

func TestRC10ASetNotListedHasTheClaimReadFirst(t *testing.T) {
	const old = "2025-01-01T00-00-05Z"
	listed := setsOf([2]string{"2026-11-15T00-00-05Z", "v0.5.0"}, [2]string{"2026-11-14T00-00-05Z", "v0.5.0"})
	release := func() *zaentrumv1alpha1.Zaentrum {
		z := verifyCR()
		z.Spec.Version = "v0.5.0"
		return z
	}
	waiting := "the restore of " + old + " starts once the claim backups has been read: its version is checked before anything stops"

	// asked has the restore asked for, and checks it waits for the claim: a run
	// of the backups' Job, with the set's name, that carries the request.
	asked := func(t *testing.T, e *storageEnv, request string) {
		t.Helper()
		e.annotate(zaentrumv1alpha1.RestoreRequestAnnotation, request)
		plan := e.restorePass()
		assert.Empty(t, plan.dump, "nothing stops while the claim is read")
		assert.Nil(t, restoreStatus(e.z), "the request is not answered yet")
		c := restoreCondition(e.z)
		require.NotNil(t, c)
		assert.Equal(t, metav1.ConditionUnknown, c.Status)
		assert.Equal(t, "Waiting", c.Reason)
		assert.Equal(t, waiting, c.Message)
		run := e.runs("backups")[0]
		assert.Equal(t, request, run.Annotations[annotationMeasureSet], "the run carries the request it answers")
		env := map[string]string{}
		for _, v := range run.Spec.Template.Spec.Containers[0].Env {
			env[v.Name] = v.Value
		}
		assert.Equal(t, old, env["SET"], "the script is asked for the set's name")
	}

	t.Run("R.C10 the claim is read for the set, and the restore goes on once it says the version", func(t *testing.T) {
		e := newCheckEnv(t, release(), listed)
		asked(t, e, old+"#1")
		n := len(e.runs("backups"))
		e.clock = e.clock.Add(30 * time.Second)
		assert.Empty(t, e.restorePass().dump, "still read")
		assert.Len(t, e.runs("backups"), n, "asked once")
		assert.Nil(t, listedSet(e.z, old, "backups"))
		e.end("backups", "node-a", 0, `{"v":1,"size":5368709120,"free":4294967296,"fs":"c1","total":40,`+
			`"sets":[["2026-11-15T00-00-05Z",1024,"v0.5.0",true,true]],"set":["`+old+`",2048,"v0.4.0",true,true]}`)
		goesOn(t, e, e.restorePass(), old, "from the run's own report, not from the sets listed")
		assert.Equal(t, old+"#1", e.z.Status.Backup.Restore.Request)
	})

	t.Run("R.C10 the version the run read is the rule's, a newer release refused", func(t *testing.T) {
		e := newCheckEnv(t, release(), listed)
		asked(t, e, old)
		e.end("backups", "node-a", 0, `{"v":1,"size":5368709120,"free":4294967296,"fs":"c1","total":3,`+
			`"sets":[["2026-11-15T00-00-05Z",1024,"v0.5.0",true,true],["`+old+`",2048,"v0.7.0",true,true]]}`)
		refusedOnce(t, e, e.restorePass(), old, "NewerVersion",
			"refused: "+old+" was made by v0.7.0, which is newer than this platform (v0.5.0): update the platform first; nothing was changed")
	})

	t.Run("R.C10 a set the claim does not hold is refused, the newest it holds named", func(t *testing.T) {
		e := newCheckEnv(t, release(), listed)
		asked(t, e, old)
		e.end("backups", "node-a", 0, listed)
		refusedOnce(t, e, e.restorePass(), old, "NotOnClaim",
			"refused: there is no set "+old+" on the claim backups (it holds: 2026-11-15T00-00-05Z, 2026-11-14T00-00-05Z); nothing was changed")

		e = newCheckEnv(t, release(), setsOf())
		asked(t, e, old)
		e.end("backups", "node-a", 0, setsOf())
		refusedOnce(t, e, e.restorePass(), old, "NotOnClaim",
			"refused: there is no set "+old+" on the claim backups (it holds: none); nothing was changed")
	})

	t.Run("R.C10 a claim that could not be read is refused with the run's reason", func(t *testing.T) {
		e := newCheckEnv(t, release(), listed)
		asked(t, e, old)
		e.end("backups", "node-a", 1, `{"v":1,"failed":"cannot read the filesystem at /measure"}`)
		refusedOnce(t, e, e.restorePass(), old, "ClaimUnreadable",
			"refused: the claim backups could not be read, so the version of "+old+" is not known: "+
				"cannot read the filesystem at /measure; nothing was changed")
	})

	t.Run("R.C10 no answer in 2 minutes is refused, saying why", func(t *testing.T) {
		e := newCheckEnv(t, release(), listed)
		asked(t, e, old)
		e.clock = e.clock.Add(time.Minute)
		assert.Empty(t, e.restorePass().dump)
		assert.Nil(t, e.z.Status.Backup.Restore, "within 2 minutes it waits")
		run := e.runs("backups")[0]
		e.clock = e.clock.Add(time.Minute)
		refusedOnce(t, e, e.restorePass(), old, "ClaimUnreadable",
			"refused: the claim backups could not be read, so the version of "+old+" is not known: "+
				run.Name+" has not ended in 2 minutes; nothing was changed")

		e = newCheckEnv(t, release(), listed)
		asked(t, e, old+"#pod")
		run = e.runs("backups")[0]
		require.NoError(t, e.c.Create(context.Background(), &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: run.Name + "-x", Namespace: e.z.Namespace, Labels: map[string]string{batchv1.JobNameLabel: run.Name}},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: templates.MeasureContainer,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "Back-off pulling image"}}}}}}))
		e.clock = e.clock.Add(2 * time.Minute)
		refusedOnce(t, e, e.restorePass(), old+"#pod", "ClaimUnreadable",
			"refused: the claim backups could not be read, so the version of "+old+" is not known: "+
				run.Name+" has not started: the measure container is waiting: ImagePullBackOff: Back-off pulling image; nothing was changed")
	})

	t.Run("R.C10 a run of the backups' Job already in flight is waited for, then the claim is read for the set", func(t *testing.T) {
		e := newCheckEnv(t, release(), listed)
		e.clock = e.clock.Add(2 * time.Hour)
		e.pass(false)
		hourly := e.runs("backups")[0]
		e.annotate(zaentrumv1alpha1.RestoreRequestAnnotation, old)
		assert.Empty(t, e.restorePass().dump)
		assert.Equal(t, "Waiting", restoreCondition(e.z).Reason)
		assert.Equal(t, hourly.Name, e.runs("backups")[0].Name, "no second run while one is in flight")
		e.end("backups", "node-a", 0, listed)
		assert.Empty(t, e.restorePass().dump, "the hourly run was not asked for the set: the claim is read for it now")
		assert.Nil(t, e.z.Status.Backup.Restore)
		assert.Equal(t, old, e.runs("backups")[0].Annotations[annotationMeasureSet])
	})

}

func TestRC10AClaimThatIsNotThereOrNotBound(t *testing.T) {
	t.Run("R.C10 a claim that is not there is NoClaim", func(t *testing.T) {
		e := newStorageEnv(t, verifyCR())
		e.annotate(zaentrumv1alpha1.RestoreRequestAnnotation, aDump)
		refusedOnce(t, e, e.restorePass(), aDump, "NoClaim",
			"refused: there is no claim backups to restore from; nothing was changed")
		assert.Empty(t, e.runs("backups"), "nothing measured")
	})
	t.Run("R.C10 a claim no volume is bound to yet is NoClaim", func(t *testing.T) {
		e := newStorageEnv(t, verifyCR())
		e.claim("backups", "5Gi", false)
		e.annotate(zaentrumv1alpha1.RestoreRequestAnnotation, aDump)
		refusedOnce(t, e, e.restorePass(), aDump, "NoClaim",
			"refused: the claim backups is not bound to a volume yet; nothing was changed")
		assert.Empty(t, e.runs("backups"), "measuring never decides where a volume is made")
	})
}

// What the version rule says of each set listed is written on every pass,
// against the version that runs, and moves with it.
func TestRC10EachSetListedSaysWhatTheVersionRuleSaysOfIt(t *testing.T) {
	z := verifyCR()
	z.Spec.Version = "v0.5.0"
	e := newCheckEnv(t, z, setsOf([2]string{"2026-11-15T00-00-05Z", "v0.5.0"}, [2]string{"2026-11-14T00-00-05Z", "v0.6.0"},
		[2]string{"2026-11-13T00-00-05Z", "latest"}, [2]string{"2026-11-12T00-00-05Z", ""}))
	e.restorePass()
	rules := func() []string {
		var out []string
		for _, s := range e.z.Status.Backup.Sets {
			out = append(out, s.VersionRule)
		}
		return out
	}
	assert.Equal(t, []string{"ok", "newer", "not-comparable", "not-comparable"}, rules())
	e.z.Spec.Version = "v0.6.0"
	e.restorePass()
	assert.Equal(t, []string{"ok", "ok", "not-comparable", "not-comparable"}, rules(), "an update moves it")
	e.z.Spec.Version = "latest"
	e.restorePass()
	assert.Equal(t, []string{"not-comparable", "not-comparable", "ok", "not-comparable"}, rules())
}

// Through Reconcile: each set listed says what the rule says of it, in the
// resource's status.
func TestRC10ReconcileWritesTheVersionRuleOfEachSet(t *testing.T) {
	// spec.version follows the channel, whose release the operator renders.
	doc := newChannelDocument(t, `{"channels":{"stable":"v0.5.0","edge":"latest"}}`)
	z := verifyCR()
	z.Status.Backup = &zaentrumv1alpha1.BackupStatus{Sets: []zaentrumv1alpha1.BackupSet{
		{Name: "2026-11-15T00-00-05Z", Version: "v0.4.0", Whole: true},
		{Name: "2026-11-14T00-00-05Z", Version: "v0.6.0", Whole: true},
		{Name: "2026-11-13T00-00-05Z", Whole: true},
	}}
	listedOn(z, "backups")
	e := newPlatformEnv(t, z, nil)
	e.r.ReleasesURL, e.r.Updates = doc.srv.URL, updates.Client{HTTP: doc.srv.Client()}
	e.reconcile()
	got := e.zaentrum()
	require.Equal(t, "v0.5.0", got.Status.CurrentVersion, "the tag rendered")
	require.Empty(t, got.Spec.Version, "spec.version names none")
	var rules []string
	for _, s := range got.Status.Backup.Sets {
		rules = append(rules, s.VersionRule)
	}
	assert.Equal(t, []string{"ok", "newer", "not-comparable"}, rules, "against the tag rendered, not spec.version")
}

// Through Reconcile: a restore whose set is not listed has the backups' claim
// read for it, with the set's name, before anything stops.
func TestRC10ReconcileAsksForTheSet(t *testing.T) {
	z := verifyCR()
	z.Spec.Version = "v0.5.0"
	z.Annotations = map[string]string{zaentrumv1alpha1.RestoreRequestAnnotation: aDump + "#1"}
	clock := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	r, c := reconcileEnv(t, z, &clock, boundClaim("media"), boundClaim("postgres-data"), boundClaim("backups"))
	ctx := context.Background()
	key := types.NamespacedName{Namespace: verifyNS, Name: "zaentrum"}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	require.NoError(t, err)

	var got zaentrumv1alpha1.Zaentrum
	require.NoError(t, c.Get(ctx, key, &got))
	assert.NotEqual(t, "Restoring", got.Status.Phase, "nothing stops")
	cond := meta.FindStatusCondition(got.Status.Conditions, condTypeRestore)
	require.NotNil(t, cond)
	assert.Equal(t, "Waiting", cond.Reason)
	var jobs batchv1.JobList
	require.NoError(t, c.List(ctx, &jobs, client.InNamespace(verifyNS), client.MatchingLabels{templates.LabelMeasure: templates.VolumeBackups}))
	var asked []batchv1.Job
	for _, j := range jobs.Items {
		if j.Annotations[annotationMeasureSet] == aDump+"#1" {
			asked = append(asked, j)
		}
	}
	require.Len(t, asked, 1, "a run asked for the set")
	assert.Equal(t, "backups", runClaim(&asked[0]))
	env := map[string]string{}
	for _, v := range asked[0].Spec.Template.Spec.Containers[0].Env {
		env[v.Name] = v.Value
	}
	assert.Equal(t, aDump, env["SET"])
}

// listOnClaim has the claim backups bound, and the set listed on it, made by
// the version the platform runs, as the backups' measuring leaves them.
func (e *platformEnv) listOnClaim(set string) {
	e.t.Helper()
	ctx := context.Background()
	var pvc corev1.PersistentVolumeClaim
	require.NoError(e.t, e.c.Get(ctx, types.NamespacedName{Namespace: e.key.Namespace, Name: "backups"}, &pvc))
	pvc.Status.Phase = corev1.ClaimBound
	require.NoError(e.t, e.c.Status().Update(ctx, &pvc))
	z := e.zaentrum()
	if z.Status.Backup == nil {
		z.Status.Backup = &zaentrumv1alpha1.BackupStatus{}
	}
	z.Status.Backup.Sets = []zaentrumv1alpha1.BackupSet{{Name: set, Version: z.Status.CurrentVersion, Whole: true}}
	listedOn(z, "backups")
	require.NoError(e.t, e.c.Status().Update(ctx, z))
}

// listedOn has status.storage list the backups' volume on a claim, as the
// measuring writes it with the sets it found there.
func listedOn(z *zaentrumv1alpha1.Zaentrum, claim string) {
	if z.Status.Storage == nil {
		z.Status.Storage = &zaentrumv1alpha1.StorageStatus{}
	}
	for i := range z.Status.Storage.Volumes {
		if z.Status.Storage.Volumes[i].Name == templates.VolumeBackups {
			z.Status.Storage.Volumes[i].Claim = claim
			return
		}
	}
	z.Status.Storage.Volumes = append(z.Status.Storage.Volumes, zaentrumv1alpha1.VolumeStatus{Name: templates.VolumeBackups, Claim: claim})
}

func aRelease() *zaentrumv1alpha1.Zaentrum {
	z := verifyCR()
	z.Spec.Version = "v0.5.0"
	return z
}

// What is listed counts only for the claim the restore reads from: after a
// change of the claim, neither what was listed before nor a run of the claim
// before is trusted.
func TestRC10WhatIsListedCountsOnlyForTheClaimItWasListedOn(t *testing.T) {
	t.Run("R.C10 the sets listed on the claim before", func(t *testing.T) {
		e := newCheckEnv(t, aRelease(), setsOf([2]string{aDump, "v0.5.0"}))
		require.NotNil(t, listedSet(e.z, aDump, "backups"), "listed on backups")
		e.claim("kept", "5Gi", true)
		e.z.Spec.Backup.ClaimName = "kept"
		e.annotate(zaentrumv1alpha1.RestoreRequestAnnotation, aDump)
		assert.Empty(t, e.restorePass().dump, "nothing stops on what another claim held")
		assert.Equal(t, "Waiting", restoreCondition(e.z).Reason)
		run := e.runs("backups")[0]
		assert.Equal(t, aDump, run.Annotations[annotationMeasureSet])
		assert.Equal(t, "kept", runClaim(&run), "the claim the restore reads from is read")
	})
	t.Run("R.C10 a run asked for the set on the claim before", func(t *testing.T) {
		e := newCheckEnv(t, aRelease(), setsOf([2]string{"2026-11-15T00-00-05Z", "v0.5.0"}))
		e.annotate(zaentrumv1alpha1.RestoreRequestAnnotation, aDump)
		assert.Empty(t, e.restorePass().dump)
		e.end("backups", "node-a", 0, setsOf([2]string{"2026-11-15T00-00-05Z", "v0.5.0"}, [2]string{aDump, "v0.5.0"}))
		e.claim("kept", "5Gi", true)
		e.z.Spec.Backup.ClaimName = "kept"
		assert.Empty(t, e.restorePass().dump, "the run read the claim before")
		run := e.runs("backups")[0]
		assert.Equal(t, aDump, run.Annotations[annotationMeasureSet])
		assert.Equal(t, "kept", runClaim(&run), "asked again on the claim it reads from now")
	})
}

// The check waits 2 minutes at most for the run asked for the set, and 15
// minutes at most in all where another run has to end first or the run cannot
// be made; then it is ClaimUnreadable, with the reason.
func TestRC10TheWaitEndsIn15MinutesInAll(t *testing.T) {
	const set = "2025-01-01T00-00-05Z"
	head := "refused: the claim backups could not be read, so the version of " + set + " is not known: "
	listed := setsOf([2]string{"2026-11-15T00-00-05Z", "v0.5.0"})
	// waits takes the pass that begins the wait and one a minute after it for
	// 14 minutes, each still waiting; the caller's next minute ends it.
	waits := func(t *testing.T, e *storageEnv) {
		t.Helper()
		for i := 0; i <= 14; i++ {
			if i > 0 {
				e.clock = e.clock.Add(time.Minute - 10*time.Second)
			}
			assert.Empty(t, e.restorePass().dump)
			require.Nil(t, restoreStatus(e.z), "after %d minutes it still waits", i)
			assert.Equal(t, "Waiting", restoreCondition(e.z).Reason)
		}
		e.clock = e.clock.Add(time.Minute - 10*time.Second)
	}

	t.Run("R.C10 another run that does not end", func(t *testing.T) {
		e := newCheckEnv(t, aRelease(), listed)
		e.clock = e.clock.Add(2 * time.Hour)
		e.pass(false)
		hourly := e.runs("backups")[0]
		e.annotate(zaentrumv1alpha1.RestoreRequestAnnotation, set)
		waits(t, e)
		assert.Equal(t, hourly.Name, e.runs("backups")[0].Name, "no run of its own while one is in flight")
		refusedOnce(t, e, e.restorePass(), set, "ClaimUnreadable", head+hourly.Name+" has not ended in 2 minutes; nothing was changed")
	})

	t.Run("R.C10 a run the cluster refuses to create", func(t *testing.T) {
		e := newCheckEnv(t, aRelease(), listed)
		e.r.Client = interceptor.NewClient(e.c, interceptor.Funcs{Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if strings.HasPrefix(obj.GetName(), templates.MeasureJobPrefix+"-"+templates.VolumeBackups) {
				return fmt.Errorf("exceeded quota")
			}
			return cl.Create(ctx, obj, opts...)
		}})
		e.annotate(zaentrumv1alpha1.RestoreRequestAnnotation, set)
		waits(t, e)
		assert.Empty(t, e.runs("backups")[0].Annotations[annotationMeasureSet], "none of its own could be made")
		plan := e.restorePass()
		require.NotNil(t, restoreStatus(e.z))
		msg := restoreStatus(e.z).Message
		assert.True(t, strings.HasPrefix(msg, head+templates.MeasureJobPrefix+"-backups-"), msg)
		assert.True(t, strings.HasSuffix(msg, " has not started: exceeded quota; nothing was changed"), msg)
		refusedOnce(t, e, plan, set, "ClaimUnreadable", msg)
	})
}

// A refusal is cut where it would not fit, out of its {reason}: the ending
// "; nothing was changed" is kept whole.
func TestRC10ARefusalKeepsItsEndingWhole(t *testing.T) {
	const set = "2025-01-01T00-00-05Z"
	t.Run("R.C10 a long reason", func(t *testing.T) {
		e := newCheckEnv(t, aRelease(), setsOf())
		e.annotate(zaentrumv1alpha1.RestoreRequestAnnotation, set)
		e.restorePass()
		long := strings.Repeat("the filesystem would not answer; ", 30)
		e.end("backups", "node-a", 1, fmt.Sprintf(`{"v":1,"failed":%q}`, long))
		e.restorePass()
		msg := restoreStatus(e.z).Message
		assert.LessOrEqual(t, len([]rune(msg)), maxVerifyMessage)
		assert.True(t, strings.HasPrefix(msg, "refused: the claim backups could not be read, so the version of "+set+" is not known: the filesystem"), msg)
		assert.True(t, strings.HasSuffix(msg, "…; nothing was changed"), msg)
		assert.Equal(t, msg, restoreCondition(e.z).Message)
	})
	t.Run("R.C10 a request that names no dump, however long", func(t *testing.T) {
		e := newStorageEnv(t, verifyCR())
		e.annotate(zaentrumv1alpha1.RestoreRequestAnnotation, strings.Repeat("yesterday ", 80))
		e.restorePass()
		msg := restoreStatus(e.z).Message
		assert.LessOrEqual(t, len([]rune(msg)), maxVerifyMessage)
		assert.True(t, strings.HasPrefix(msg, `refused: "yesterday yesterday`), msg)
		assert.True(t, strings.HasSuffix(msg, "…; nothing was changed"), msg)
	})
	t.Run("R.C10 a long claim and many sets", func(t *testing.T) {
		z := aRelease()
		claim := strings.Repeat("k", 240)
		z.Spec.Backup.ClaimName = claim
		var sets [][2]string
		for i := 0; i < 10; i++ {
			sets = append(sets, [2]string{fmt.Sprintf("2026-11-%02dT00-00-05Z", 20-i), "v0.5.0"})
		}
		e := newStorageEnv(t, z)
		e.claim("media", "50Gi", true)
		e.claim(claim, "5Gi", true)
		e.pass(false)
		e.end("media", "node-a", 0, libraryReport(50*gib, 20*gib, "b9e2"))
		e.end("backups", "node-a", 0, setsOf(sets...))
		e.pass(false)
		e.annotate(zaentrumv1alpha1.RestoreRequestAnnotation, set)
		e.restorePass()
		e.end("backups", "node-a", 0, setsOf(sets...))
		e.restorePass()
		msg := restoreStatus(e.z).Message
		assert.LessOrEqual(t, len([]rune(msg)), maxVerifyMessage)
		assert.True(t, strings.HasPrefix(msg, "refused: there is no set "+set+" on the claim "+claim+" (it holds: 2026-11-20T00-00-05Z, "), msg)
		assert.True(t, strings.HasSuffix(msg, "…); nothing was changed"), msg)
		assert.Equal(t, "NotOnClaim", restoreCondition(e.z).Reason)
	})
}

// A request taken away while the operator waits takes the wait's condition
// with it: the wait for the claim to be read, and the one for the database's
// copy.
func TestRC10ARequestTakenAwayTakesTheWaitsConditionWithIt(t *testing.T) {
	t.Run("R.C10 while the claim is read for the set", func(t *testing.T) {
		e := newCheckEnv(t, aRelease(), setsOf())
		e.annotate(zaentrumv1alpha1.RestoreRequestAnnotation, aDump)
		e.restorePass()
		require.Equal(t, "Waiting", restoreCondition(e.z).Reason)
		delete(e.z.Annotations, zaentrumv1alpha1.RestoreRequestAnnotation)
		assert.Empty(t, e.restorePass().dump)
		assert.Nil(t, restoreCondition(e.z), "the wait's condition goes with its request")
		assert.Nil(t, restoreStatus(e.z))
	})
	t.Run("R.C10 while the database is copied", func(t *testing.T) {
		e := newBackupEnv(t, verifyCR())
		e.runningPlatform()
		e.askRestore(aDump)
		e.restorePass(true)
		require.Equal(t, "Waiting", restoreCondition(e.z).Reason)
		delete(e.z.Annotations, zaentrumv1alpha1.RestoreRequestAnnotation)
		e.restorePass(true)
		assert.Nil(t, restoreCondition(e.z))
	})
	t.Run("R.C10 replaced by a request already answered", func(t *testing.T) {
		e := newCheckEnv(t, aRelease(), setsOf([2]string{aDump, "v0.9.0"}))
		e.annotate(zaentrumv1alpha1.RestoreRequestAnnotation, aDump)
		e.restorePass()
		require.Equal(t, "NewerVersion", restoreCondition(e.z).Reason)
		e.annotate(zaentrumv1alpha1.RestoreRequestAnnotation, "2025-01-01T00-00-05Z")
		e.restorePass()
		require.Equal(t, "Waiting", restoreCondition(e.z).Reason)
		e.annotate(zaentrumv1alpha1.RestoreRequestAnnotation, aDump)
		e.restorePass()
		assert.Nil(t, restoreCondition(e.z), "no request waits")
		assert.Equal(t, aDump, restoreStatus(e.z).Request, "the answered one stays answered")
	})
}
