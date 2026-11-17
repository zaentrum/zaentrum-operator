package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

// This file measures the platform's volumes.
//
// The operator cannot see how full a volume is: only a node knows, and the
// operator holds no permission on nodes. So the chart has one measuring Job
// for each claim the platform uses (templates/storage.yaml), the library's,
// the backups' and the event broker's where it has one, which mounts the
// claim read-only and reports the filesystem's size, what is free and its id;
// the library's also asks the bundled Postgres, which reads the volume of its
// data itself. They are hooks: the operator starts each as a Job of its own,
// never two runs of one at once:
//
//   - every hour;
//   - every one at once for a zaentrum.io/storage-request value other than
//     the last one answered, which it answers in status.storage.request once
//     every run the request started has ended and been read; while a backup
//     holds the backups' claim, without its run, as StorageMeasured says;
//   - the backups' when a backup has ended;
//   - none while a restore runs, and none on a claim that is not bound yet, so
//     that no measuring decides where a volume is made.
//
// The backups' claim has one reader or writer at a time. No run of the
// backups' Job starts while a backup runs or may start: the schedule held for
// a backup, a backup asked for that waits, an update held back for the backup
// before it, a backup Job that has not ended, one being deleted among them, or
// one that has failed while its pod may still write (backupActivity). The
// other way round, a backup asked for and the backup before an update wait for
// a run of the backups' Job, the schedule stands still while one runs, and so
// does a restore wait (backupsMeasuring). A run holds its claim until its pod
// has ended, and never past its deadline and its pod's grace: one that has
// not ended by then, as on a node that is gone, is removed and counted as
// failed (removeRun).
//
// The backups' claim is measured wherever it is there, with backups on or
// off, and for a restore, which asks for one set by name (measureSet). The
// operator reads each run's report, the termination message of its pod, as
// it reads a backup's, into status.storage, status.backup.sets and the
// condition StorageMeasured. A pass that cannot read a claim or a run changes
// nothing it would have read from it, answers nothing, and reads again.

const (
	condTypeStorageMeasured = "StorageMeasured"

	// annotationMeasureSet is the set a backups' run was asked for by name.
	annotationMeasureSet = "zaentrum.io/measure-set"
	// annotationMeasureAnswered is, on a run a request started, the request
	// that had been answered when it started: a value asked again after
	// another was answered is a new request, which an earlier run of the same
	// value does not answer.
	annotationMeasureAnswered = "zaentrum.io/measure-answered"
	// annotationMeasureWaiting is why a run's pod has not started, as the
	// operator last saw it while the run waited.
	annotationMeasureWaiting = "zaentrum.io/measure-waiting"

	// measureEvery is how often each volume is measured.
	measureEvery = time.Hour

	// measureReportVersion is the report format ("v") this operator reads, and
	// maxMeasureReport the most of one it parses at all: the kubelet keeps
	// 4096 bytes of a termination message.
	measureReportVersion = 1
	maxMeasureReport     = 64 << 10

	// measureJobSuffix is the length of the random end of a run's name.
	measureJobSuffix = 5

	// timeOfFigures is how a condition writes the time of figures that stay.
	timeOfFigures = "2006-01-02 15:04 MST"
)

// measuredVolumes are the volumes in the order status lists them.
var measuredVolumes = []string{templates.VolumeMedia, templates.VolumePostgres, templates.VolumeBackups, templates.VolumeKafka}

// measureReport is the report files/measure-volume.sh leaves as its
// termination message.
type measureReport struct {
	V         int              `json:"v"`
	Size      *int64           `json:"size"`
	Free      *int64           `json:"free"`
	FS        string           `json:"fs"`
	Databases map[string]int64 `json:"databases"`
	Postgres  *struct {
		Size *int64 `json:"size"`
		Free *int64 `json:"free"`
		FS   string `json:"fs"`
		// Refused is psql's error line where the server may not read its
		// own volume: no failure.
		Refused *string `json:"refused"`
	} `json:"postgres"`
	Total  *int                `json:"total"`
	Sets   [][]json.RawMessage `json:"sets"`
	Set    []json.RawMessage   `json:"set"`
	Failed string              `json:"failed"`
}

// readable says whether a report is one this operator reads: its format, and
// no size below zero.
func (rep *measureReport) readable() bool {
	if rep.V != measureReportVersion {
		return false
	}
	for _, n := range []*int64{rep.Size, rep.Free} {
		if n != nil && *n < 0 {
			return false
		}
	}
	if p := rep.Postgres; p != nil {
		for _, n := range []*int64{p.Size, p.Free} {
			if n != nil && *n < 0 {
				return false
			}
		}
	}
	for _, n := range rep.Databases {
		if n < 0 {
			return false
		}
	}
	if rep.Total != nil && *rep.Total < 0 {
		return false
	}
	for _, s := range backupSets(rep) {
		if s.Bytes < 0 {
			return false
		}
	}
	return true
}

// measured says whether the report holds the figures of the claim itself.
func (rep *measureReport) measured() bool {
	return rep != nil && rep.Size != nil && rep.Free != nil
}

// postgresMeasured says whether it holds those of the Postgres's volume, and
// postgresRefused whether the server said it may not read them.
func (rep *measureReport) postgresMeasured() bool {
	return rep != nil && rep.Postgres != nil && rep.Postgres.Size != nil && rep.Postgres.Free != nil
}

func (rep *measureReport) postgresRefused() bool {
	return rep != nil && rep.Postgres != nil && rep.Postgres.Refused != nil
}

// volumePlan is a volume the platform uses this pass.
type volumePlan struct {
	name, claim string
	// hook is the Job that measures it; nil for the Postgres's, which the
	// library's Job measures.
	hook *unstructured.Unstructured
	// pvc is its claim; missing says there is no claim of that name, and
	// unreadable that it could not be read this pass.
	pvc                 *corev1.PersistentVolumeClaim
	missing, unreadable bool
}

// bound says whether a run may start on the volume's claim: it is there and
// bound, so that the run decides nothing about where its volume is made.
func (p volumePlan) bound() bool {
	return p.pvc != nil && p.pvc.Status.Phase == corev1.ClaimBound
}

// measureRun is one ended run of a volume's Job as the operator read it, or
// one that left no Job to read: a run the cluster refused to create, or one
// gone before it was read.
type measureRun struct {
	// job is the run's Job; nil where it left none.
	job *batchv1.Job
	// name is the run's name; claim the claim it measured; request the
	// storage request it was started for and answered the one answered then.
	name, claim, request, answered string
	report                         *measureReport
	// node is the node its pod ran on, and at when it ended.
	node string
	at   metav1.Time
	// reason is why it failed, "" where it did not.
	reason string
}

// measureMemory is what the operator keeps between passes of one platform's
// runs: the run in flight and the run last read of each volume, to tell a run
// gone before it was read, and the outcome of a run that left no Job to read;
// and when a request was last answered without a run of the backups' claim,
// which a backup held.
type measureMemory struct {
	inFlight   map[string]*measureRun
	read       map[string]string
	outcome    map[string]*measureRun
	unmeasured *metav1.Time
}

// measureMemoryOf is the memory of a platform's runs.
func (r *ZaentrumReconciler) measureMemoryOf(z *zaentrumv1alpha1.Zaentrum) *measureMemory {
	if r.measured == nil {
		r.measured = map[types.UID]*measureMemory{}
	}
	m := r.measured[z.UID]
	if m == nil {
		m = &measureMemory{inFlight: map[string]*measureRun{}, read: map[string]string{}, outcome: map[string]*measureRun{}}
		r.measured[z.UID] = m
	}
	return m
}

// measureStorage takes this pass's step of the measuring and writes
// status.storage, status.backup.sets and the condition StorageMeasured.
// platform and hooks are the pass's render; restoring says whether a restore
// is in flight, during which no run starts; backupsHeld that the backups'
// schedule is held for a backup, which may start (backuprequest.go), during
// which no run of the backups' Job starts. It reports whether to come back
// soon: a run is in flight, a request waits for its answer, or something
// could not be read.
//
// It returns no error: what goes wrong is the measuring's, never the
// reconcile's.
func (r *ZaentrumReconciler) measureStorage(ctx context.Context, z *zaentrumv1alpha1.Zaentrum,
	platform, hooks []*unstructured.Unstructured, restoring, backupsHeld bool) bool {
	r.measureMu.Lock()
	defer r.measureMu.Unlock()
	mem := r.measureMemoryOf(z)

	plan := r.storagePlan(ctx, z, platform, hooks)
	jobs, err := r.measureJobs(ctx, z)
	if err != nil {
		log.FromContext(ctx).Info("storage: the measuring runs could not be listed; status stays as it is", "error", err.Error())
		return true
	}
	claims := map[string]string{}
	unread := false
	for _, p := range plan {
		claims[p.name] = p.claim
		unread = unread || p.unreadable
	}

	// The latest run of each volume that has ended on the claim the volume is
	// on now, and the runs in flight, with why their pod waits.
	latest := map[string]*measureRun{}
	inFlight := map[string]*measureRun{}
	unreadVolume := map[string]bool{}
	listed := map[string]bool{}
	for _, name := range measuredVolumes {
		for i := range jobs[name] {
			job := &jobs[name][i]
			listed[job.Name] = true
			done, failed := jobFinished(job)
			if r.pastGrace(job) && (!done || (failed != nil && r.podRuns(ctx, job))) {
				r.removeRun(ctx, mem, name, job)
				continue
			}
			if !done {
				if inFlight[name] == nil {
					inFlight[name] = runOf(job)
				}
				if err := r.noteWaiting(ctx, job); err != nil {
					unreadVolume[name] = true
				}
				continue
			}
			if latest[name] != nil || runClaim(job) != claims[name] {
				continue
			}
			run, err := r.measureRunOf(ctx, job, failed)
			if err != nil {
				unreadVolume[name] = true
				continue
			}
			latest[name] = run
		}
	}
	for name := range unreadVolume {
		unread = true
		delete(latest, name)
	}

	// A run in flight at the last pass that is not listed now, nor was read,
	// is gone before it was read.
	now := r.now()
	for _, name := range measuredVolumes {
		if prev := mem.inFlight[name]; prev != nil && !listed[prev.name] && mem.read[name] != prev.name {
			gone := *prev
			gone.reason, gone.at = prev.name+" is gone before it was read", now
			mem.outcome[name] = &gone
		}
		mem.inFlight[name] = inFlight[name]
		if run := latest[name]; run != nil {
			mem.read[name] = run.name
		}
		if o := mem.outcome[name]; o != nil && o.claim != claims[name] {
			delete(mem.outcome, name)
		}
	}

	if !r.writeStorage(ctx, z, plan, latest, unreadVolume) {
		unread = true
	}
	r.removeStaleMeasureJobs(ctx, z, plan, jobs)

	request := strings.TrimSpace(z.GetAnnotations()[zaentrumv1alpha1.StorageRequestAnnotation])
	answered := ""
	if z.Status.Storage != nil {
		answered = z.Status.Storage.Request
	}
	pending := request != "" && request != answered
	if !restoring {
		pending = r.startMeasuring(ctx, z, mem, plan, jobs, inFlight, request, answered, pending, unread, backupsHeld)
	}
	// What left no Job, this pass's included, takes the place of an older run
	// of the same claim.
	for _, name := range measuredVolumes {
		if o := mem.outcome[name]; o != nil {
			if run := latest[name]; run == nil || o.at.After(run.at.Time) {
				latest[name] = o
			}
		}
	}
	if !unread {
		storageCondition(z, plan, latest, mem.unmeasured)
	}
	return len(inFlight) > 0 || pending || unread
}

// runOf is what a run's Job says of it before it has been read.
func runOf(job *batchv1.Job) *measureRun {
	return &measureRun{job: job, name: job.Name, claim: runClaim(job),
		request: job.Annotations[zaentrumv1alpha1.StorageRequestAnnotation], answered: job.Annotations[annotationMeasureAnswered]}
}

// storagePlan is what the platform measures this pass: the volumes its
// render uses, in status's order. The backups' claim is among them where it is
// there, with backups on or off.
func (r *ZaentrumReconciler) storagePlan(ctx context.Context, z *zaentrumv1alpha1.Zaentrum,
	platform, hooks []*unstructured.Unstructured) []volumePlan {
	var plan []volumePlan
	for _, name := range measuredVolumes {
		var p volumePlan
		switch name {
		case templates.VolumePostgres:
			// Measured by the library's Job, where the Postgres keeps its data
			// on a claim.
			claim := deploymentClaim(platform, "postgres", "data")
			if claim == "" || templates.MeasureJob(hooks, templates.VolumeMedia) == nil {
				continue
			}
			p = volumePlan{name: name, claim: claim}
		default:
			hook := templates.MeasureJob(hooks, name)
			if hook == nil {
				continue
			}
			p = volumePlan{name: name, claim: jobClaim(hook), hook: hook}
		}
		p.pvc, p.missing, p.unreadable = r.readClaim(ctx, z, p.claim)
		if p.missing && name == templates.VolumeBackups {
			// Where there is no claim for the backups, there is no volume of
			// theirs, and no set.
			continue
		}
		plan = append(plan, p)
	}
	return plan
}

// deploymentClaim is the claim a rendered Deployment mounts as a volume of
// that name, or "".
func deploymentClaim(objs []*unstructured.Unstructured, name, volume string) string {
	for _, o := range objs {
		if o.GetKind() == "Deployment" && o.GetName() == name {
			return podClaim(o, volume)
		}
	}
	return ""
}

// jobClaim is the claim a measuring hook mounts.
func jobClaim(hook *unstructured.Unstructured) string { return podClaim(hook, "claim") }

// runClaim is the claim a measuring run mounted.
func runClaim(job *batchv1.Job) string {
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == "claim" && v.PersistentVolumeClaim != nil {
			return v.PersistentVolumeClaim.ClaimName
		}
	}
	return ""
}

// podClaim is the claim of a pod template's volume of that name, or "".
func podClaim(o *unstructured.Unstructured, volume string) string {
	vols, _, _ := unstructured.NestedSlice(o.Object, "spec", "template", "spec", "volumes")
	for _, v := range vols {
		vm, _ := v.(map[string]interface{})
		if vm["name"] != volume {
			continue
		}
		claim, _, _ := unstructured.NestedString(vm, "persistentVolumeClaim", "claimName")
		return claim
	}
	return ""
}

// readClaim reads a claim of the platform's namespace: the claim, whether
// there is none of that name, and whether it could not be read.
func (r *ZaentrumReconciler) readClaim(ctx context.Context, z *zaentrumv1alpha1.Zaentrum, name string) (*corev1.PersistentVolumeClaim, bool, bool) {
	if name == "" {
		return nil, true, false
	}
	var pvc corev1.PersistentVolumeClaim
	err := r.reader().Get(ctx, types.NamespacedName{Namespace: z.Namespace, Name: name}, &pvc)
	switch {
	case apierrors.IsNotFound(err):
		return nil, true, false
	case err != nil:
		log.FromContext(ctx).Info("storage: a claim could not be read; read again on the next pass", "claim", name, "error", err.Error())
		return nil, false, true
	}
	return &pvc, false, false
}

// measureJobs lists the measuring runs this Zaentrum owns, by volume, newest
// first. A Job being deleted is no run any more.
func (r *ZaentrumReconciler) measureJobs(ctx context.Context, z *zaentrumv1alpha1.Zaentrum) (map[string][]batchv1.Job, error) {
	var list batchv1.JobList
	if err := r.reader().List(ctx, &list, client.InNamespace(z.Namespace), client.HasLabels{templates.LabelMeasure}); err != nil {
		return nil, err
	}
	out := map[string][]batchv1.Job{}
	for _, j := range list.Items {
		if metav1.IsControlledBy(&j, z) && j.DeletionTimestamp == nil {
			name := j.Labels[templates.LabelMeasure]
			out[name] = append(out[name], j)
		}
	}
	for name := range out {
		runs := out[name]
		sort.SliceStable(runs, func(a, b int) bool {
			return runs[b].CreationTimestamp.Before(&runs[a].CreationTimestamp)
		})
	}
	return out, nil
}

// noteWaiting writes on a run in flight why its pod has not started, as its
// pods say now, so that the run says it once it has ended, whatever is left
// of its pods then. It fails where its pods could not be read.
func (r *ZaentrumReconciler) noteWaiting(ctx context.Context, job *batchv1.Job) error {
	pods, err := r.runPods(ctx, job)
	if err != nil {
		return err
	}
	hint := waitingHint(pods)
	if hint == "" || hint == job.Annotations[annotationMeasureWaiting] {
		return nil
	}
	patch := client.MergeFrom(job.DeepCopy())
	if job.Annotations == nil {
		job.Annotations = map[string]string{}
	}
	job.Annotations[annotationMeasureWaiting] = hint
	if err := r.Patch(ctx, job, patch); client.IgnoreNotFound(err) != nil {
		log.FromContext(ctx).Info("storage: why a run waits could not be written on it", "job", job.Name, "error", err.Error())
	}
	return nil
}

// measureRunOf is an ended run as its pod left it: its report, where it left
// one this operator reads, and why it failed. It fails where the run's pods
// could not be read.
func (r *ZaentrumReconciler) measureRunOf(ctx context.Context, job *batchv1.Job, failed *batchv1.JobCondition) (*measureRun, error) {
	pods, err := r.runPods(ctx, job)
	if err != nil {
		return nil, err
	}
	run := runOf(job)
	run.at = metav1.NewTime(finishedAt(job))
	var term *corev1.ContainerStateTerminated
	for i := range pods {
		for _, cs := range pods[i].Status.ContainerStatuses {
			t := cs.State.Terminated
			if cs.Name != templates.MeasureContainer || t == nil {
				continue
			}
			if term == nil || t.FinishedAt.After(term.FinishedAt.Time) {
				term, run.node = t, pods[i].Spec.NodeName
			}
		}
	}
	if term == nil {
		why := job.Annotations[annotationMeasureWaiting]
		if why == "" {
			why = waitingHint(pods)
		}
		if why == "" && failed != nil {
			why = jobReason(failed)
		}
		if why != "" {
			run.reason = job.Name + " has not started: " + why
		} else {
			run.reason = job.Name + " left no report the operator can read"
		}
		return run, nil
	}
	if !term.FinishedAt.IsZero() {
		run.at = term.FinishedAt
	}
	msg := strings.TrimSpace(term.Message)
	var rep measureReport
	if len(msg) > maxMeasureReport || json.Unmarshal([]byte(msg), &rep) != nil || !rep.readable() {
		run.reason = job.Name + " left no report the operator can read"
		return run, nil
	}
	run.report = &rep
	run.reason = rep.Failed
	if run.reason == "" && failed != nil {
		run.reason = job.Name + " left no report the operator can read"
	}
	return run, nil
}

// jobReason is a Job's own failure, "Reason: message".
func jobReason(c *batchv1.JobCondition) string {
	return strings.TrimSuffix(c.Reason+": "+c.Message, ": ")
}

// writeStorage writes status.storage and status.backup.sets from the plan and
// the latest ended runs: what a run measured, where it is newer than the
// figures status holds; the rest as status holds it. A claim the platform no
// longer uses leaves the list; a volume that could not be read this pass, or
// whose run could not, keeps what it held. It reports whether it read all it
// needed: the Postgres's node, for the volume of its data.
func (r *ZaentrumReconciler) writeStorage(ctx context.Context, z *zaentrumv1alpha1.Zaentrum,
	plan []volumePlan, latest map[string]*measureRun, unreadVolume map[string]bool) bool {
	read := true
	s := &zaentrumv1alpha1.StorageStatus{}
	if z.Status.Storage != nil {
		s = z.Status.Storage.DeepCopy()
	}
	prev := map[string]zaentrumv1alpha1.VolumeStatus{}
	for _, v := range s.Volumes {
		prev[v.Name] = v
	}
	volumes := make([]zaentrumv1alpha1.VolumeStatus, 0, len(plan))
	backups := false
	for _, p := range plan {
		v := zaentrumv1alpha1.VolumeStatus{Name: p.name, Claim: p.claim}
		old, ok := prev[p.name]
		switch {
		case ok && old.Claim == p.claim:
			v = old
		case ok && p.name == templates.VolumeBackups && z.Status.Backup != nil:
			// Another claim's figures, and its sets, say nothing of this one.
			z.Status.Backup.Sets = nil
		}
		if !p.unreadable {
			v.RequestBytes = requestBytes(p.pvc)
		}
		backups = backups || p.name == templates.VolumeBackups
		volumes = append(volumes, v)
	}
	if !backups && z.Status.Backup != nil {
		z.Status.Backup.Sets = nil
	}
	at := func(name string) *zaentrumv1alpha1.VolumeStatus {
		for i := range volumes {
			if volumes[i].Name == name {
				return &volumes[i]
			}
		}
		return nil
	}
	newer := func(v *zaentrumv1alpha1.VolumeStatus, run *measureRun) bool {
		return v.MeasuredAt == nil || run.at.After(v.MeasuredAt.Time)
	}
	bundled := z.Spec.Databases.Mode != "external"
	if !bundled {
		s.Databases = nil
	}

	for _, name := range []string{templates.VolumeMedia, templates.VolumeBackups, templates.VolumeKafka} {
		run, v := latest[name], at(name)
		if run == nil || v == nil || run.report == nil || unreadVolume[name] {
			continue
		}
		rep := run.report
		fresh := newer(v, run)
		if rep.measured() && fresh {
			put(v, *rep.Size, *rep.Free, filesystemID(run.node, rep.FS), run.at)
		}
		switch name {
		case templates.VolumeMedia:
			if pg := at(templates.VolumePostgres); pg != nil && newer(pg, run) {
				switch {
				case rep.postgresMeasured():
					node, err := r.postgresNode(ctx, z)
					if err != nil {
						// Read again on the next pass, with the node.
						read = false
						break
					}
					put(pg, *rep.Postgres.Size, *rep.Postgres.Free, filesystemID(node, rep.Postgres.FS), run.at)
				case rep.postgresRefused():
					// The server may not read its own volume: it is listed
					// with the size its claim asks for alone.
					pg.SizeBytes, pg.FreeBytes, pg.Filesystem, pg.MeasuredAt = nil, nil, "", nil
				}
			}
			if bundled && rep.Databases != nil && fresh {
				s.Databases = databaseSizes(rep.Databases)
			}
		case templates.VolumeBackups:
			if rep.Total != nil && fresh && bundled {
				if z.Status.Backup == nil {
					z.Status.Backup = &zaentrumv1alpha1.BackupStatus{}
				}
				z.Status.Backup.Sets = backupSets(rep)
			}
		}
	}

	s.Volumes = volumes
	if len(s.Volumes) == 0 {
		s.Volumes = nil
	}
	if s.Volumes == nil && s.Databases == nil && s.Request == "" {
		z.Status.Storage = nil
	} else {
		z.Status.Storage = s
	}
	return read
}

// put writes what a run found of a volume.
func put(v *zaentrumv1alpha1.VolumeStatus, size, free int64, fs string, at metav1.Time) {
	v.SizeBytes, v.FreeBytes, v.Filesystem = &size, &free, fs
	v.MeasuredAt = &at
}

// requestBytes is the size a claim asks for, or nil.
func requestBytes(pvc *corev1.PersistentVolumeClaim) *int64 {
	if pvc == nil {
		return nil
	}
	q, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if !ok {
		return nil
	}
	n := q.Value()
	return &n
}

// filesystemID is a filesystem's id with the node it was measured on in
// front, or "" where the filesystem reports none, as a network share does, or
// the node is not known: such a volume joins no other.
func filesystemID(node, fs string) string {
	fs = strings.TrimLeft(strings.ToLower(strings.TrimSpace(fs)), "0")
	if fs == "" || node == "" {
		return ""
	}
	return node + ":" + fs
}

// postgresNode is the node the bundled Postgres runs on, or "" where it runs
// on none; it fails where its pods could not be read.
func (r *ZaentrumReconciler) postgresNode(ctx context.Context, z *zaentrumv1alpha1.Zaentrum) (string, error) {
	var pods corev1.PodList
	if err := r.reader().List(ctx, &pods, client.InNamespace(z.Namespace), client.MatchingLabels{"app": "postgres"}); err != nil {
		return "", err
	}
	node := ""
	for _, p := range pods.Items {
		if p.Spec.NodeName == "" || p.DeletionTimestamp != nil {
			continue
		}
		if p.Status.Phase == corev1.PodRunning {
			return p.Spec.NodeName, nil
		}
		node = p.Spec.NodeName
	}
	return node, nil
}

// databaseKeys are the keys of spec.databases, in the order status lists them.
var databaseKeys = []string{"chino", "katalog", "keycloak", "portal"}

// databaseSizes are the sizes of the platform's databases by their key, as
// the report gives them.
func databaseSizes(got map[string]int64) []zaentrumv1alpha1.DatabaseSize {
	var out []zaentrumv1alpha1.DatabaseSize
	for _, key := range databaseKeys {
		if n, ok := got[key]; ok {
			out = append(out, zaentrumv1alpha1.DatabaseSize{Name: key, Bytes: n})
		}
	}
	return out
}

// backupSets are the sets a backups' run listed, newest first: the newest it
// had room for, and the one it was asked for by name where that is older.
func backupSets(rep *measureReport) []zaentrumv1alpha1.BackupSet {
	var out []zaentrumv1alpha1.BackupSet
	add := func(item []json.RawMessage) {
		if len(item) != 5 {
			return
		}
		var s zaentrumv1alpha1.BackupSet
		var own *bool
		if json.Unmarshal(item[0], &s.Name) != nil || json.Unmarshal(item[1], &s.Bytes) != nil ||
			json.Unmarshal(item[2], &s.Version) != nil || json.Unmarshal(item[3], &s.Whole) != nil ||
			json.Unmarshal(item[4], &own) != nil || s.Name == "" {
			return
		}
		s.Own = own
		out = append(out, s)
	}
	for _, item := range rep.Sets {
		add(item)
	}
	if rep.Set != nil {
		add(rep.Set)
	}
	return out
}

// startMeasuring starts this pass's runs: those a request asks for, the
// backups' after a backup, and each volume's on the hour, never two of one
// at once, none on a claim that is not there, not bound yet or could not be
// read, and none of the backups' while a backup runs or may start
// (backupsHeld). It answers the request once every run it started has ended
// and been read, and not in a pass that could not read something; a request
// is answered without a run of the backups' claim that a backup holds
// (mem.unmeasured). It reports whether the request still waits.
func (r *ZaentrumReconciler) startMeasuring(ctx context.Context, z *zaentrumv1alpha1.Zaentrum, mem *measureMemory,
	plan []volumePlan, jobs map[string][]batchv1.Job, inFlight map[string]*measureRun,
	request, answered string, pending, unread, backupsHeld bool) bool {
	backupRuns, lastBackup := r.backupActivity(ctx, z)
	backupRuns = backupRuns || backupsHeld
	now := r.now()
	all, unmeasured := true, false
	for _, p := range plan {
		if p.hook == nil || p.missing || p.unreadable || !p.bound() {
			continue
		}
		runs := jobs[p.name]
		if pending {
			if done, found := requestState(runs, mem.outcome[p.name], request, answered); found {
				all = all && done
				continue
			}
			if p.name == templates.VolumeBackups && backupRuns {
				unmeasured = true
				continue
			}
			all = false
			if inFlight[p.name] == nil {
				r.startMeasure(ctx, z, mem, p, runs, request, answered, "")
				inFlight[p.name] = &measureRun{}
			}
			continue
		}
		if inFlight[p.name] != nil {
			continue
		}
		last := lastAttempt(runs, mem.outcome[p.name], p.claim)
		switch {
		case p.name == templates.VolumeBackups && backupRuns:
			// The backups' hourly run is skipped while a backup runs or may
			// start; the run after it measures the claim.
		case p.name == templates.VolumeBackups && lastBackup != nil && (last == nil || lastBackup.After(*last)):
			r.startMeasure(ctx, z, mem, p, runs, "", "", "")
			inFlight[p.name] = &measureRun{}
		case last == nil || !now.Time.Before(last.Add(measureEvery)):
			r.startMeasure(ctx, z, mem, p, runs, "", "", "")
			inFlight[p.name] = &measureRun{}
		}
	}
	if pending && all && !unread {
		if z.Status.Storage == nil {
			z.Status.Storage = &zaentrumv1alpha1.StorageStatus{}
		}
		z.Status.Storage.Request = request
		if unmeasured {
			mem.unmeasured = &now
		}
		return false
	}
	return pending
}

// requestState says whether a request started a run of the volume, one of
// its Jobs or one that left none, and whether that run has ended. One that
// left none, or was removed (removeRun), has ended.
func requestState(runs []batchv1.Job, outcome *measureRun, request, answered string) (done, found bool) {
	if outcome != nil && outcome.request == request && outcome.answered == answered {
		return true, true
	}
	for i := range runs {
		if runs[i].Annotations[zaentrumv1alpha1.StorageRequestAnnotation] == request &&
			runs[i].Annotations[annotationMeasureAnswered] == answered {
			ended, _ := jobFinished(&runs[i])
			return ended, true
		}
	}
	return false, false
}

// lastAttempt is when a run of the volume on its claim last started, or one
// that left no Job was last tried, or nil.
func lastAttempt(runs []batchv1.Job, outcome *measureRun, claim string) *time.Time {
	var last *time.Time
	for i := range runs {
		if runClaim(&runs[i]) == claim {
			t := runs[i].CreationTimestamp.Time
			last = &t
			break
		}
	}
	if outcome != nil && outcome.claim == claim && (last == nil || outcome.at.After(*last)) {
		t := outcome.at.Time
		last = &t
	}
	return last
}

// backupActivity says whether a backup runs: a backup Job that has not ended,
// one being deleted among them, or one that has failed while its pod may
// still write (stillWrites); or whether that could not be read. It gives when
// the latest one that ended ended.
func (r *ZaentrumReconciler) backupActivity(ctx context.Context, z *zaentrumv1alpha1.Zaentrum) (bool, *time.Time) {
	jobs, err := r.allBackupJobs(ctx, z)
	if err != nil {
		return true, nil
	}
	running := r.stillWrites(ctx, jobs) != nil
	var last *time.Time
	for i := range jobs {
		done, _ := jobFinished(&jobs[i])
		if !done {
			running = true
			continue
		}
		if end := finishedAt(&jobs[i]); last == nil || end.After(*last) {
			last = &end
		}
	}
	return running, last
}

// measureSet has the backups' claim measured now, with the set name asked for
// as well, however old it is: what a restore asks before it checks a set's
// version. hooks are the pass's render; backupsHeld says that the schedule is
// held for a backup. It starts nothing while a backup runs or may start, nor
// while a run of the backups' Job holds the claim (holdsClaim), nor on a claim
// that is not there or not bound, and reports whether it started one; the set
// is in status.backup.sets once the run has ended and been read.
func (r *ZaentrumReconciler) measureSet(ctx context.Context, z *zaentrumv1alpha1.Zaentrum,
	hooks []*unstructured.Unstructured, set string, backupsHeld bool) (bool, error) {
	hook := templates.MeasureJob(hooks, templates.VolumeBackups)
	if hook == nil {
		return false, fmt.Errorf("the platform chart renders no Job that measures the backups' claim")
	}
	p := volumePlan{name: templates.VolumeBackups, claim: jobClaim(hook), hook: hook}
	p.pvc, p.missing, p.unreadable = r.readClaim(ctx, z, p.claim)
	if p.unreadable {
		return false, fmt.Errorf("the claim %s could not be read", p.claim)
	}
	if !p.bound() {
		return false, nil
	}
	if running, _ := r.backupActivity(ctx, z); running || backupsHeld {
		return false, nil
	}
	jobs, err := r.measureJobs(ctx, z)
	if err != nil {
		return false, err
	}
	if r.holdsClaim(ctx, jobs[p.name]) != nil {
		return false, nil
	}
	r.measureMu.Lock()
	defer r.measureMu.Unlock()
	return r.startMeasure(ctx, z, r.measureMemoryOf(z), p, jobs[p.name], "", "", set), nil
}

// startMeasure starts a run of a volume's Job: for a request, or with a set
// asked for by name, or neither. The volume's earlier runs go but the latest
// that ended, which status was written from. A run the cluster refuses to
// create is the volume's latest run, which has not started, until the next
// one is tried. None starts while an earlier run holds the claim, as one that
// failed while its pod still runs (holdsClaim). It reports whether it started.
func (r *ZaentrumReconciler) startMeasure(ctx context.Context, z *zaentrumv1alpha1.Zaentrum, mem *measureMemory, p volumePlan,
	runs []batchv1.Job, request, answered, set string) bool {
	if r.holdsClaim(ctx, runs) != nil {
		return false
	}
	kept := false
	for i := range runs {
		done, _ := jobFinished(&runs[i])
		if !done {
			continue
		}
		if !kept {
			kept = true
			continue
		}
		if err := r.Delete(ctx, &runs[i], client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
			log.FromContext(ctx).Info("storage: an earlier run could not be removed", "job", runs[i].Name, "error", err.Error())
		}
	}
	job, err := r.measureJob(z, p, request, answered, set)
	if err == nil {
		err = r.Create(ctx, job)
	}
	if err != nil {
		name := p.hook.GetName()
		if job != nil {
			name = job.GetName()
		}
		mem.outcome[p.name] = &measureRun{name: name, claim: p.claim, request: request, answered: answered,
			at: r.now(), reason: name + " has not started: " + err.Error()}
		log.FromContext(ctx).Info("storage: a measuring run could not be started; tried again on the hour", "volume", p.name, "error", err.Error())
		return false
	}
	delete(mem.outcome, p.name)
	log.FromContext(ctx).Info("storage: measuring", "volume", p.name, "job", job.GetName(), "request", request, "set", set)
	return true
}

// measureJob turns a volume's measuring hook into a run's Job: a name of its
// own, owned by the Zaentrum, without the Helm hook annotations, with the
// request it answers and the set it was asked for.
func (r *ZaentrumReconciler) measureJob(z *zaentrumv1alpha1.Zaentrum, p volumePlan, request, answered, set string) (*unstructured.Unstructured, error) {
	job := p.hook.DeepCopy()
	job.SetName(templates.MeasureJobPrefix + "-" + p.name + "-" + utilrand.String(measureJobSuffix))
	job.SetNamespace(z.Namespace)
	job.SetResourceVersion("")
	annotations := map[string]string{}
	for k, v := range job.GetAnnotations() {
		if !strings.HasPrefix(k, "helm.sh/") {
			annotations[k] = v
		}
	}
	if request != "" {
		annotations[zaentrumv1alpha1.StorageRequestAnnotation] = request
		annotations[annotationMeasureAnswered] = answered
	}
	if set != "" {
		annotations[annotationMeasureSet] = set
		if err := addEnv(job, templates.MeasureContainer, "SET", set); err != nil {
			return job, err
		}
	}
	job.SetAnnotations(annotations)
	lbls := job.GetLabels()
	if lbls == nil {
		lbls = map[string]string{}
	}
	lbls[templates.LabelMeasure] = p.name
	job.SetLabels(lbls)
	if err := controllerutil.SetControllerReference(z, job, r.Scheme); err != nil {
		return job, err
	}
	return job, nil
}

// addEnv adds a variable to a container of a Job's pod template.
func addEnv(job *unstructured.Unstructured, container, name, value string) error {
	containers, _, err := unstructured.NestedSlice(job.Object, "spec", "template", "spec", "containers")
	if err != nil {
		return err
	}
	for i, c := range containers {
		cm, _ := c.(map[string]interface{})
		if cm["name"] != container {
			continue
		}
		env, _ := cm["env"].([]interface{})
		cm["env"] = append(env, map[string]interface{}{"name": name, "value": value})
		containers[i] = cm
		return unstructured.SetNestedSlice(job.Object, containers, "spec", "template", "spec", "containers")
	}
	return fmt.Errorf("the Job %s has no container %s", job.GetName(), container)
}

// removeStaleMeasureJobs removes the runs of a volume the platform no longer
// measures: an event broker without its claim, or databases made external.
func (r *ZaentrumReconciler) removeStaleMeasureJobs(ctx context.Context, z *zaentrumv1alpha1.Zaentrum,
	plan []volumePlan, jobs map[string][]batchv1.Job) {
	used := map[string]bool{}
	for _, p := range plan {
		used[p.name] = true
	}
	for name, runs := range jobs {
		if used[name] {
			continue
		}
		for i := range runs {
			if err := r.Delete(ctx, &runs[i], client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
				log.FromContext(ctx).Info("storage: a run of a volume no longer measured could not be removed",
					"job", runs[i].Name, "error", err.Error())
			}
		}
	}
}

// backupsMeasuring says which run of the backups' Job holds the claim
// (holdsClaim), or "", or that the runs could not be listed. A restore waits
// for it, and so do a backup asked for and the backup before an update, and
// the backups' schedule stands still while it runs.
func (r *ZaentrumReconciler) backupsMeasuring(ctx context.Context, z *zaentrumv1alpha1.Zaentrum) string {
	jobs, err := r.measureJobs(ctx, z)
	if err != nil {
		return "the measuring runs could not be listed"
	}
	if job := r.holdsClaim(ctx, jobs[templates.VolumeBackups]); job != nil {
		return job.Name
	}
	return ""
}

// holdsClaim is the run among a volume's runs that holds its claim, or nil:
// one that has not ended, or that has failed while its pod may still read, as
// at its deadline; never one past its deadline and its pod's grace.
func (r *ZaentrumReconciler) holdsClaim(ctx context.Context, runs []batchv1.Job) *batchv1.Job {
	for i := range runs {
		if done, failed := jobFinished(&runs[i]); !r.pastGrace(&runs[i]) && (!done || (failed != nil && r.podRuns(ctx, &runs[i]))) {
			return &runs[i]
		}
	}
	return nil
}

// pastGrace says whether a run is past its deadline and its pod's grace after
// it, counted from its start: one that has not ended by then, as on a node
// that is gone, holds nothing and is removed (removeRun).
func (r *ZaentrumReconciler) pastGrace(job *batchv1.Job) bool {
	if job.Spec.ActiveDeadlineSeconds == nil {
		return false
	}
	start := job.CreationTimestamp
	if job.Status.StartTime != nil {
		start = *job.Status.StartTime
	}
	grace := int64(corev1.DefaultTerminationGracePeriodSeconds)
	if g := job.Spec.Template.Spec.TerminationGracePeriodSeconds; g != nil {
		grace = *g
	}
	return !r.now().Time.Before(start.Add(time.Duration(*job.Spec.ActiveDeadlineSeconds+grace) * time.Second))
}

// removeRun deletes a run past its deadline and its pod's grace that has not
// ended, and keeps it as failed: "{job} had not ended at its deadline and was
// removed". A pod left on a node that is gone may still read; nothing waits
// for it.
func (r *ZaentrumReconciler) removeRun(ctx context.Context, mem *measureMemory, volume string, job *batchv1.Job) {
	if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
		log.FromContext(ctx).Info("storage: a run past its deadline could not be removed", "job", job.Name, "error", err.Error())
		return
	}
	run := runOf(job)
	run.reason, run.at = job.Name+" had not ended at its deadline and was removed", r.now()
	mem.outcome[volume] = run
	log.FromContext(ctx).Info("storage: removed a run that had not ended at its deadline", "volume", volume, "job", job.Name)
}

// failure is one failed run, as the condition writes it: "{job} failed:
// {reason}" and its ending.
type failure struct{ job, reason, ending string }

// storageCondition writes the condition StorageMeasured from the latest run of
// each Job: Measured with what it found, or Failed with why, for every run
// that failed and every claim that is not there; and, kept whole, that the
// backups' claim was not measured, where a request was answered without it
// since its latest run (unmeasured). It is absent until the first run has
// ended, or such a request has been answered.
func storageCondition(z *zaentrumv1alpha1.Zaentrum, plan []volumePlan, latest map[string]*measureRun, unmeasured *metav1.Time) {
	var failed []failure
	ended := false
	measuredNow := map[string]bool{}
	note := ""
	for _, p := range plan {
		if p.hook == nil {
			continue
		}
		if b := latest[p.name]; p.name == templates.VolumeBackups && unmeasured != nil && (b == nil || b.at.Before(unmeasured)) {
			note = p.claim + " was not measured: a backup holds it" + ending(z, plan, p.name, nil)
		}
		measuredNow[p.name] = true
		if p.name == templates.VolumeMedia {
			measuredNow[templates.VolumePostgres] = true
		}
		if p.missing {
			failed = append(failed, failure{p.hook.GetName(), "there is no claim " + p.claim, ending(z, plan, p.name, nil)})
			continue
		}
		run := latest[p.name]
		if run == nil {
			continue
		}
		ended = true
		if run.reason != "" {
			failed = append(failed, failure{run.name, run.reason, ending(z, plan, p.name, run.report)})
		}
	}
	n := maxVerifyMessage
	if note != "" {
		n -= utf8.RuneCountInString(note) + len("; ")
	}
	switch {
	case len(failed) > 0:
		setCondition(z, condTypeStorageMeasured, metav1.ConditionFalse, "Failed", withNote(failedMessage(failed, n), note))
	case ended || note != "":
		setCondition(z, condTypeStorageMeasured, metav1.ConditionTrue, "Measured",
			withNote(measuredMessage(z, measuredNow, latest[templates.VolumeMedia], latest[templates.VolumeBackups], n), note))
	}
}

// withNote is a condition's message with a note after it, where there is one.
func withNote(message, note string) string {
	if message == "" || note == "" {
		return message + note
	}
	return message + "; " + note
}

// ending is how a failed run's part of the condition ends, by the figures of
// its job's volumes: "; the figures of {time} stay" where what the run did not
// measure is kept from before; "; nothing is measured yet" where the status
// holds no figure of that job's at all; nothing where the run measured a part
// and nothing is kept from before for the rest. rep is the run's report, nil
// where it left none.
func ending(z *zaentrumv1alpha1.Zaentrum, plan []volumePlan, volume string, rep *measureReport) string {
	volumes := map[string]*zaentrumv1alpha1.VolumeStatus{}
	if z.Status.Storage != nil {
		for i := range z.Status.Storage.Volumes {
			volumes[z.Status.Storage.Volumes[i].Name] = &z.Status.Storage.Volumes[i]
		}
	}
	parts := map[string]bool{volume: rep.measured()}
	if volume == templates.VolumeMedia {
		for _, p := range plan {
			if p.name == templates.VolumePostgres {
				parts[templates.VolumePostgres] = rep.postgresMeasured() || rep.postgresRefused()
			}
		}
	}
	var kept *metav1.Time
	any := volume == templates.VolumeMedia && z.Status.Storage != nil && len(z.Status.Storage.Databases) > 0
	for name, measuredNow := range parts {
		v := volumes[name]
		if v == nil || v.MeasuredAt == nil {
			continue
		}
		any = true
		if !measuredNow && (kept == nil || v.MeasuredAt.After(kept.Time)) {
			kept = v.MeasuredAt
		}
	}
	switch {
	case kept != nil:
		return "; the figures of " + kept.UTC().Format(timeOfFigures) + " stay"
	case !any:
		return "; nothing is measured yet"
	}
	return ""
}

// failedMessage joins the failures as the condition writes them, at most n
// characters: what is cut is taken from the reasons, and every ending is kept
// whole however long the reasons are.
func failedMessage(failed []failure, n int) string {
	fixed := 0
	for i, f := range failed {
		fixed += utf8.RuneCountInString(f.job+" failed: ") + utf8.RuneCountInString(f.ending)
		if i > 0 {
			fixed += len("; ")
		}
	}
	room := n - fixed
	// The reasons share what is left, the shorter ones whole.
	lengths := make([]int, len(failed))
	order := make([]int, len(failed))
	for i, f := range failed {
		lengths[i] = utf8.RuneCountInString(strings.TrimSpace(f.reason))
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return lengths[order[a]] < lengths[order[b]] })
	share := make([]int, len(failed))
	for k, i := range order {
		left := len(order) - k
		fair := room / left
		if fair < 1 {
			fair = 1
		}
		share[i] = min(lengths[i], fair)
		room -= share[i]
	}
	parts := make([]string, len(failed))
	for i, f := range failed {
		parts[i] = f.job + " failed: " + clip(f.reason, share[i]) + f.ending
	}
	return strings.Join(parts, "; ")
}

// measuredMessage says what the volumes measured now hold, at most n
// characters: each disk's free space and size, its claims named together where
// they share it; what the databases take; how many sets are on the backups'
// claim; and, kept whole, why the databases' volume is not measured, where the
// server may not read it.
func measuredMessage(z *zaentrumv1alpha1.Zaentrum, measuredNow map[string]bool, media, backups *measureRun, n int) string {
	type disk struct {
		claims     []string
		free, size int64
	}
	var disks []*disk
	byID := map[string]*disk{}
	var parts []string
	s := z.Status.Storage
	if s == nil {
		return ""
	}
	for _, v := range s.Volumes {
		if !measuredNow[v.Name] || v.SizeBytes == nil || v.FreeBytes == nil {
			continue
		}
		if d := byID[v.Filesystem]; v.Filesystem != "" && d != nil {
			d.claims = append(d.claims, v.Claim)
			continue
		}
		d := &disk{claims: []string{v.Claim}, free: *v.FreeBytes, size: *v.SizeBytes}
		disks = append(disks, d)
		if v.Filesystem != "" {
			byID[v.Filesystem] = d
		}
	}
	for _, d := range disks {
		if len(d.claims) == 1 {
			parts = append(parts, fmt.Sprintf("%s %s free of %s", d.claims[0], bytesOf(d.free), bytesOf(d.size)))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s on one disk: %s free of %s", joinAnd(d.claims), bytesOf(d.free), bytesOf(d.size)))
	}
	if len(s.Databases) > 0 {
		var total int64
		for _, d := range s.Databases {
			total += d.Bytes
		}
		parts = append(parts, "the databases take "+bytesOf(total))
	}
	if backups != nil && backups.report != nil && backups.report.Total != nil && measuredNow[templates.VolumeBackups] {
		claim := ""
		for _, v := range s.Volumes {
			if v.Name == templates.VolumeBackups {
				claim = v.Claim
			}
		}
		total, listed := *backups.report.Total, len(backups.report.Sets)
		switch {
		case listed < total:
			parts = append(parts, fmt.Sprintf("%d sets on %s, the newest %d listed", total, claim, listed))
		case total == 1:
			parts = append(parts, "1 set on "+claim)
		default:
			parts = append(parts, fmt.Sprintf("%d sets on %s", total, claim))
		}
	}
	message := strings.Join(parts, "; ")
	if media != nil && media.report.postgresRefused() && measuredNow[templates.VolumePostgres] {
		why := "the databases' volume is not measured: " + strings.TrimSpace(*media.report.Postgres.Refused)
		why = clip(why, n)
		room := n - utf8.RuneCountInString(why) - len("; ")
		switch {
		case message == "":
			return why
		case room < 1:
			return why
		}
		return clip(message, room) + "; " + why
	}
	return clip(message, n)
}

// joinAnd joins names with commas and "and": "a and b", "a, b and c".
func joinAnd(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}
