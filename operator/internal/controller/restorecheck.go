package controller

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
	"github.com/zaentrum/zaentrum-operator/operator/internal/templates"
)

// This file is a restore's second step: before anything stops, the operator
// checks the set a restore asks for. The backups' claim must be there and
// bound and hold the set, whole (its SHA256SUMS there), and the version rule
// (versionrule.go) must let the set be restored: a newer release never, a
// version that cannot be compared only where zaentrum.io/restore-any-version
// holds the set's name.
//
// The set's version is read from status.backup.sets, which counts only for the
// claim the restore reads from: the claim the backups' volume was listed with.
// A set that is not listed there has the backups' claim measured first, with
// the set's name, however old the set is (measureSet, storage.go): the run
// carries the request it answers and the claim it reads, and the check reads
// that run's report itself, so it counts on neither the sets being written from
// it nor the claim being listed among the volumes. It waits 2 minutes at most
// for that run's answer, and 15 minutes at most in all where another run has
// to end first or the run cannot be made, as while a backup holds the claim,
// which the refusal names then (backupHolding). A claim that is not there or not
// bound, a set the claim does not hold and a claim that could not be read are
// refused at this step, with nothing stopped, and the Restore condition's
// reason names the cause: NewerVersion, VersionNotComparable, NoClaim,
// NotOnClaim or ClaimUnreadable. What the restore's Job refuses stays Refused.
// A request taken away while the operator waits takes the wait's condition
// with it.

// The reasons of the Restore condition for what a restore's check refuses, and
// while a restore waits.
const (
	reasonNewerVersion         = "NewerVersion"
	reasonVersionNotComparable = "VersionNotComparable"
	reasonNoClaim              = "NoClaim"
	reasonNotOnClaim           = "NotOnClaim"
	reasonClaimUnreadable      = "ClaimUnreadable"
	// reasonRefused is every other refusal: the request's, and the Job's.
	reasonRefused = "Refused"
	// reasonWaiting is a restore that waits, for its set's version or for the
	// database's copy.
	reasonWaiting = "Waiting"
)

const (
	// setAnswerWait is how long the check waits for the run asked for the set.
	setAnswerWait = 2 * time.Minute
	// restoreWaitInAll is how long it waits in all where another run has to end
	// first or the run cannot be made.
	restoreWaitInAll = 15 * time.Minute

	// heldNames is how many of the claim's newest sets a refusal names.
	heldNames = 10

	// A refusal's words around its middle; the ending is kept whole.
	refusalStart  = "refused: "
	refusalEnding = "; nothing was changed"
)

// restorePlan is what a pass does about restoring, decided before the render.
type restorePlan struct {
	// dump is the dump this pass restores: the render stops the database's
	// clients for it. Empty while none is restored.
	dump string
	// ask is the request whose set the backups' claim is measured for after
	// the render (askForSet), empty where nothing is to be asked.
	ask string
}

// setVerdict is what a restore's check found.
type setVerdict int

const (
	// setGoesOn: the claim holds the set, and the version rule lets it go on.
	setGoesOn setVerdict = iota
	// setRefused: refused, for reason, with message.
	setRefused
	// setWaits: the claim is being read for the set; ask says a run is to be
	// started for it.
	setWaits
	// setUndecided: the cluster did not answer this pass; the next tries again,
	// and message says what it waits for.
	setUndecided
)

type setCheck struct {
	verdict setVerdict
	// reason is the Restore condition's reason of a refusal, and message what
	// stands between "refused: " and "; nothing was changed"; or, while it
	// waits, the condition's sentence.
	reason, message string
	ask             bool
}

// restoreWait is what the operator keeps between passes of a restore's check:
// the request it waits for, since when, and why the run asked for its set
// could not be made, where it could not.
type restoreWait struct {
	request string
	since   time.Time
	unmade  string
}

// waitFor is the wait for a request's set, begun now where it is another
// request's or none. An operator that starts again counts from what the
// cluster holds: since the Restore condition began to wait for that set.
func (r *ZaentrumReconciler) waitFor(z *zaentrumv1alpha1.Zaentrum, request, set string) restoreWait {
	r.restoreMu.Lock()
	defer r.restoreMu.Unlock()
	if r.restoreWaits == nil {
		r.restoreWaits = map[types.UID]restoreWait{}
	}
	w, ok := r.restoreWaits[z.UID]
	if !ok || w.request != request {
		w = restoreWait{request: request, since: r.now().Time}
		if c := meta.FindStatusCondition(z.Status.Conditions, condTypeRestore); !ok && c != nil &&
			c.Status == metav1.ConditionUnknown && c.Reason == reasonWaiting && strings.HasPrefix(c.Message, "the restore of "+set+" ") {
			w.since = c.LastTransitionTime.Time
		}
		r.restoreWaits[z.UID] = w
	}
	return w
}

// setWaiting has the Restore condition wait, saying msg, which begins "the
// restore of {set} "; where it begins to wait, or for another set, its time is
// this operator's clock, which a wait counts from after a restart (waitFor).
func (r *ZaentrumReconciler) setWaiting(z *zaentrumv1alpha1.Zaentrum, msg string) {
	before := meta.FindStatusCondition(z.Status.Conditions, condTypeRestore)
	setOf := func(m string) string {
		if f := strings.Fields(m); len(f) > 3 {
			return f[3]
		}
		return ""
	}
	begins := before == nil || before.Status != metav1.ConditionUnknown || setOf(before.Message) != setOf(msg)
	setCondition(z, condTypeRestore, metav1.ConditionUnknown, reasonWaiting, msg)
	if c := meta.FindStatusCondition(z.Status.Conditions, condTypeRestore); begins && c != nil {
		c.LastTransitionTime = r.now()
	}
}

// noteUnmade keeps why the run asked for a request's set could not be made,
// "" once it was.
func (r *ZaentrumReconciler) noteUnmade(z *zaentrumv1alpha1.Zaentrum, request, why string) {
	r.restoreMu.Lock()
	defer r.restoreMu.Unlock()
	if w, ok := r.restoreWaits[z.UID]; ok && w.request == request {
		w.unmade = why
		r.restoreWaits[z.UID] = w
	}
}

// endWait forgets the wait of a platform's restore.
func (r *ZaentrumReconciler) endWait(z *zaentrumv1alpha1.Zaentrum) {
	r.restoreMu.Lock()
	defer r.restoreMu.Unlock()
	delete(r.restoreWaits, z.UID)
}

// dropWaiting takes a wait's condition away, where the Restore condition is
// one: its request is gone.
func dropWaiting(z *zaentrumv1alpha1.Zaentrum) {
	if c := meta.FindStatusCondition(z.Status.Conditions, condTypeRestore); c != nil &&
		c.Status == metav1.ConditionUnknown && c.Reason == reasonWaiting {
		meta.RemoveStatusCondition(&z.Status.Conditions, condTypeRestore)
	}
}

// checkSet is a restore's second step for the set the request names: the
// claim, the set on it, and the version rule against version, the version
// that runs.
func (r *ZaentrumReconciler) checkSet(ctx context.Context, z *zaentrumv1alpha1.Zaentrum, request, set, version string) setCheck {
	claim := backupClaim(z)
	var pvc corev1.PersistentVolumeClaim
	err := r.reader().Get(ctx, types.NamespacedName{Namespace: z.Namespace, Name: claim}, &pvc)
	switch {
	case apierrors.IsNotFound(err):
		return refused(reasonNoClaim, "there is no claim "+claim+" to restore from", "", "")
	case err != nil:
		log.FromContext(ctx).Info("restore: the backups' claim could not be read; trying again", "claim", claim, "error", err.Error())
		return undecided(set, "the claim "+claim+" could not be read")
	case pvc.Status.Phase != corev1.ClaimBound:
		// Measuring never decides where a volume is made.
		return refused(reasonNoClaim, "the claim "+claim+" is not bound to a volume yet", "", "")
	}

	if s := listedSet(z, set, claim); s != nil {
		return wholeAndRuled(z, *s, version)
	}

	run, err := r.setRun(ctx, z, request, claim)
	if err != nil {
		log.FromContext(ctx).Info("restore: the measuring runs could not be listed; trying again", "error", err.Error())
		return undecided(set, "the measuring runs could not be listed")
	}
	wait := r.waitFor(z, request, set)
	if run == nil {
		// Another run has to end first, or the run could not be made.
		if !r.now().Time.Before(wait.since.Add(restoreWaitInAll)) {
			if why := r.whyNoRun(ctx, z, wait); why != "" {
				return claimUnreadable(claim, set, why)
			}
		}
		return waitsForClaim(set, claim, true)
	}
	done, failed := jobFinished(run)
	if !done {
		if r.now().Time.Before(run.CreationTimestamp.Add(setAnswerWait)) {
			return waitsForClaim(set, claim, false)
		}
		return claimUnreadable(claim, set, r.notAnswered(ctx, run))
	}
	read, err := r.measureRunOf(ctx, run, failed)
	if err != nil {
		log.FromContext(ctx).Info("restore: the run that measured the backups' claim could not be read; trying again", "job", run.Name, "error", err.Error())
		return undecided(set, run.Name+" could not be read")
	}
	if read.report == nil || read.report.Total == nil {
		// The run ended before it listed the claim's sets.
		why := read.reason
		if why == "" {
			why = run.Name + " left no report the operator can read"
		}
		return claimUnreadable(claim, set, why)
	}
	sets := backupSets(read.report)
	for _, s := range sets {
		if s.Name == set {
			return wholeAndRuled(z, s, version)
		}
	}
	return notOnClaim(set, claim, sets)
}

// wholeAndRuled refuses a set that is not whole, with no SHA256SUMS, as the
// restore's Job would once the clients had stopped; else it is what the
// version rule says (ruled).
func wholeAndRuled(z *zaentrumv1alpha1.Zaentrum, s zaentrumv1alpha1.BackupSet, version string) setCheck {
	if !s.Whole {
		return refused(reasonRefused, s.Name+" has no SHA256SUMS", "", "")
	}
	return ruled(z, s.Name, s.Version, version)
}

// listedSet is the set of that name status.backup.sets lists, or nil. What is
// listed counts only for the claim it was listed on, the backups' volume's:
// after a change of the claim, nothing listed before is trusted.
func listedSet(z *zaentrumv1alpha1.Zaentrum, name, claim string) *zaentrumv1alpha1.BackupSet {
	if z.Status.Backup == nil || z.Status.Storage == nil {
		return nil
	}
	listedOn := ""
	for _, v := range z.Status.Storage.Volumes {
		if v.Name == templates.VolumeBackups {
			listedOn = v.Claim
		}
	}
	if listedOn != claim {
		return nil
	}
	for i := range z.Status.Backup.Sets {
		if z.Status.Backup.Sets[i].Name == name {
			return &z.Status.Backup.Sets[i]
		}
	}
	return nil
}

// ruled is what the version rule says of the set, with the annotation
// zaentrum.io/restore-any-version, its spaces trimmed.
func ruled(z *zaentrumv1alpha1.Zaentrum, set, setVersion, version string) setCheck {
	switch versionRule(setVersion, version) {
	case zaentrumv1alpha1.VersionRuleNewer:
		return refused(reasonNewerVersion, fmt.Sprintf(
			"%s was made by %s, which is newer than this platform (%s): update the platform first", set, setVersion, version), "", "")
	case zaentrumv1alpha1.VersionRuleNotComparable:
		if strings.TrimSpace(z.GetAnnotations()[zaentrumv1alpha1.RestoreAnyVersionAnnotation]) == set {
			return setCheck{verdict: setGoesOn}
		}
		return refused(reasonVersionNotComparable, fmt.Sprintf(
			"the version of %s cannot be compared with this platform's (%s): to restore it all the same, "+
				"set zaentrum.io/restore-any-version to %s and ask again with a new value", set, version, set), "", "")
	}
	return setCheck{verdict: setGoesOn}
}

// refused is a refusal of the check: head, the part that may be cut, and tail.
// The whole fits what a message holds with its start and its ending, which
// are kept whole: the cut comes out of the part.
func refused(reason, head, part, tail string) setCheck {
	room := maxVerifyMessage - utf8.RuneCountInString(refusalStart+head+tail+refusalEnding)
	if part != "" {
		if room < 1 {
			room = 1
		}
		part = clip(part, room)
	}
	return setCheck{verdict: setRefused, reason: reason, message: head + part + tail}
}

// notOnClaim refuses a set the claim does not hold, naming the newest it does.
func notOnClaim(set, claim string, held []zaentrumv1alpha1.BackupSet) setCheck {
	names := make([]string, 0, heldNames)
	for _, s := range held {
		if len(names) == heldNames {
			break
		}
		names = append(names, s.Name)
	}
	list := strings.Join(names, ", ")
	if list == "" {
		list = "none"
	}
	return refused(reasonNotOnClaim, fmt.Sprintf("there is no set %s on the claim %s (it holds: ", set, claim), list, ")")
}

// claimUnreadable refuses a set whose claim could not be read, and says why.
func claimUnreadable(claim, set, why string) setCheck {
	return refused(reasonClaimUnreadable,
		fmt.Sprintf("the claim %s could not be read, so the version of %s is not known: ", claim, set), why, "")
}

// undecided is the check waiting for what it could not read this pass, which
// the next reads again.
func undecided(set, why string) setCheck {
	return setCheck{verdict: setUndecided, message: fmt.Sprintf("the restore of %s waits: %s; it is read again", set, why)}
}

// waitsForClaim is the check waiting for the claim to be read, ask saying a run
// is to be started for it.
func waitsForClaim(set, claim string, ask bool) setCheck {
	return setCheck{verdict: setWaits, ask: ask, message: fmt.Sprintf(
		"the restore of %s starts once the claim %s has been read: its version is checked before anything stops", set, claim)}
}

// setRun is the newest run of the backups' Job that a restore's request asked
// for on the claim the restore reads from, or nil: a run knows its claim.
func (r *ZaentrumReconciler) setRun(ctx context.Context, z *zaentrumv1alpha1.Zaentrum, request, claim string) (*batchv1.Job, error) {
	jobs, err := r.measureJobs(ctx, z)
	if err != nil {
		return nil, err
	}
	for i := range jobs[templates.VolumeBackups] {
		if job := &jobs[templates.VolumeBackups][i]; job.Annotations[annotationMeasureSet] == request && runClaim(job) == claim {
			return job, nil
		}
	}
	return nil, nil
}

// notAnswered says why a run has not answered: its pod has not started, and
// why, where it waited or the cluster says; else that it has not ended.
func (r *ZaentrumReconciler) notAnswered(ctx context.Context, run *batchv1.Job) string {
	why := run.Annotations[annotationMeasureWaiting]
	if why == "" {
		pods, _ := r.runPods(ctx, run)
		why = waitingHint(pods)
	}
	if why != "" {
		return run.Name + " has not started: " + why
	}
	return fmt.Sprintf("%s has not ended in %d minutes", run.Name, int(setAnswerWait/time.Minute))
}

// whyNoRun says why no run was asked for the set in all the time the check
// waited: a run of the backups' Job that has not ended, or why the run could
// not be made. "" where nothing says.
func (r *ZaentrumReconciler) whyNoRun(ctx context.Context, z *zaentrumv1alpha1.Zaentrum, wait restoreWait) string {
	jobs, err := r.measureJobs(ctx, z)
	if err == nil {
		for i := range jobs[templates.VolumeBackups] {
			if run := &jobs[templates.VolumeBackups][i]; !finished(run) {
				return r.notAnswered(ctx, run)
			}
		}
	}
	return wait.unmade
}

// askForSet has the backups' claim measured for the set a restore's request
// names, after the render: hooks are the pass's, backupsHeld says that the
// schedule is held for a backup. The run carries the request it answers
// (measureSet). A run of the backups' Job already in flight is waited for: the
// next pass asks again. Why a run could not be made, a backup that holds the
// claim among it, is kept, for the check to say once it has waited too long.
func (r *ZaentrumReconciler) askForSet(ctx context.Context, z *zaentrumv1alpha1.Zaentrum, hooks []*unstructured.Unstructured,
	plan restorePlan, backupsHeld bool) {
	if plan.ask == "" {
		return
	}
	before := r.backupsOutcome(z)
	started, err := r.measureSet(ctx, z, hooks, plan.ask, backupsHeld)
	switch {
	case err != nil:
		log.FromContext(ctx).Info("restore: the backups' claim could not be measured for the set", "request", plan.ask, "error", err.Error())
		r.noteUnmade(z, plan.ask, err.Error())
	case started:
		log.FromContext(ctx).Info("restore: reading the backups' claim for the set before anything stops", "request", plan.ask)
		r.noteUnmade(z, plan.ask, "")
	default:
		// A backup holds the claim; else the cluster refused to create the
		// run, where that is what the measuring kept just now; else a run is
		// in flight.
		if why := r.backupHolding(ctx, z, backupsHeld); why != "" {
			r.noteUnmade(z, plan.ask, why)
		} else if after := r.backupsOutcome(z); after != nil && after != before {
			r.noteUnmade(z, plan.ask, after.reason)
		}
	}
}

// backupHolding says which backup holds the backups' claim, so that no run
// for a set is made: "the backup {job} runs", where its Job has not ended or
// its pod still runs; "a backup is about to start", where the schedule is held
// for one (backupsHeld); or "".
func (r *ZaentrumReconciler) backupHolding(ctx context.Context, z *zaentrumv1alpha1.Zaentrum, backupsHeld bool) string {
	all, err := r.allBackupJobs(ctx, z)
	if err != nil {
		return "the backups' Jobs could not be listed"
	}
	name := runningBackup(all)
	if job := r.stillWrites(ctx, all); name == "" && job != nil {
		name = job.Name
	}
	switch {
	case name != "":
		return "the backup " + name + " runs"
	case backupsHeld:
		return "a backup is about to start"
	}
	return ""
}

// backupsOutcome is what the measuring keeps of the backups' latest run that
// left no Job to read, or nil.
func (r *ZaentrumReconciler) backupsOutcome(z *zaentrumv1alpha1.Zaentrum) *measureRun {
	r.measureMu.Lock()
	defer r.measureMu.Unlock()
	return r.measureMemoryOf(z).outcome[templates.VolumeBackups]
}
