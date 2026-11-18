package controller

import (
	"context"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
	"github.com/zaentrum/zaentrum-operator/operator/internal/templates"
)

// This file makes a backup before an update.
//
// Another version is another tag: this pass's render tag differs from
// status.currentVersion, both set. New images under the same tag, another
// operator, another image for the identity provider and a new install's first
// version are applied as before, without a backup first. Before the operator
// applies another version, where this pass renders the backups' CronJob and
// the operator is not the one-machine install's (onAppliance; its updater
// keeps a copy of the databases itself), it holds the update back: the render
// keeps the version that runs, so that the backup is of that version,
// status.availableUpdate names the version that waits, and the backups'
// schedule stands still (backupScheduleHeld). During a restore the update
// waits for the restore to end, and the count starts then.
//
// The count starts when the operator first holds the update back, whatever it
// can read then, and it is kept on the cluster: from that pass until the
// version that runs moves, the update is taken back or a restore starts, the
// CronJob the operator applies carries the version that runs and that time
// (zaentrum.io/backup-before-update and -since), and so does the backup before
// the update, which is how a pass knows that backup again. A pass whose
// CronJob carries neither yet (the first, or one whose CronJob could not be
// applied) takes the newest backup Job marked with the version that runs that
// was marked in the last updateBackupWindow and, where it has ended, after the
// last restore and for an update not applied; or what this operator remembers;
// or now.
//
// The update is applied once the backup before it has ended, well or not: a
// backup that fails does not stop it. A backup that still runs 30 minutes
// after the count started is stopped, so that no set is written while the
// databases change under it: its Job's deadline is cut to one second, so that
// the Job controller ends it, and it stays listed as a run that failed. The
// update is then applied once that Job has ended and no pod of it runs. Where
// no backup could be started in the 30 minutes, it is applied then, without
// one. Whatever else happens (a mark, a stop or a list the API server refuses,
// a backup that is being deleted and has not ended, an operator that starts
// again), it is applied 35 minutes after the count started at the latest
// (updateHoldBound).
//
// The pass that applies the update records it, in memory and on the backup
// before it (zaentrum.io/backup-before-update-to, the version applied), so
// that a pass that finds the update not yet applied, as its apply failed,
// applies it again without holding it back or backing up again, for as long
// as the version that runs has not moved; the Backup condition names the
// version recorded.
//
// A backup Job that already runs when the operator first holds the update back
// is the backup before it, marked so; one that is being deleted is waited for,
// as it may still write, but neither marked nor stopped. Otherwise the
// operator starts one as a backup asked for starts (backuprequest.go), once
// the CronJob is suspended; it also answers a request that waits. A request
// that waits behind a run that counts gets its own Job after the update.

const (
	// annotationBackupBeforeUpdate is the version a backup was made of, before
	// an update from it; on the backups' CronJob, the version that runs of an
	// update the operator holds back or has applied while that version still
	// runs.
	annotationBackupBeforeUpdate = "zaentrum.io/backup-before-update"
	// annotationBackupBeforeUpdateSince is when the operator first held that
	// update back, in RFC 3339: the 30 minutes count from it.
	annotationBackupBeforeUpdateSince = "zaentrum.io/backup-before-update-since"
	// annotationBackupBeforeUpdateStopped is when the operator stopped the
	// backup, in RFC 3339, because it had not ended after 30 minutes.
	annotationBackupBeforeUpdateStopped = "zaentrum.io/backup-before-update-stopped"
	// annotationBackupBeforeUpdateTo is the version the update was applied to
	// after this backup, written by the pass that applied it; on the backups'
	// CronJob, the version the update was applied to.
	annotationBackupBeforeUpdateTo = "zaentrum.io/backup-before-update-to"
	// annotationBackupBeforeUpdateStopOwed, on the backups' CronJob, names the
	// backup Job the update was applied while it still ran and whose stop was
	// refused, or is stopOwedUnknown where the backups' Jobs could not be
	// listed: the first pass that can, stops it.
	annotationBackupBeforeUpdateStopOwed = "zaentrum.io/backup-before-update-stop-owed"
	// stopOwedUnknown is the stop owed to a backup that could not be listed as
	// the update was applied: the newest of the count's that has not ended,
	// else the newest backup that has not ended and began before the 35
	// minutes were over (owedJob).
	stopOwedUnknown = "*"

	// updateBackupWait is how long an update waits for the backup before it.
	updateBackupWait = 30 * time.Minute
	// updateStopWait is how long, after the 30 minutes, the update waits for
	// a backup that was stopped to end and its pod to stop.
	updateStopWait = 5 * time.Minute
	// updateHoldBound is when, after the count started, an update is applied
	// at the latest, whatever else holds it.
	updateHoldBound = updateBackupWait + updateStopWait
	// updateBackupWindow is how old a backup before an update may be and
	// still be the one of the update that waits, where the CronJob does not
	// say which one it is: one marked before that is of an update given up
	// before.
	updateBackupWindow = updateHoldBound + 5*time.Minute

	// stopDeadline is the deadline, in seconds, the stop cuts a backup's Job
	// to.
	stopDeadline = 1
)

// updateStep is what a pass does for the backup before an update, after the
// platform was applied.
type updateStep int

const (
	updateWait  updateStep = iota // nothing but wait
	updateMark                    // mark the backup that runs as the one before the update
	updateStart                   // start the backup before the update
	updateStop                    // stop the backup that has run for 30 minutes
)

// updateBackup is the backup before an update, as a pass finds it before it
// renders.
type updateBackup struct {
	// from is the version that runs and to the one that waits; both "" when
	// no update waits for a backup or has been applied while from runs.
	from, to string
	// hold says that the render keeps from: the update waits.
	hold bool
	// since is when the operator first held the update back; zero while a
	// restore runs.
	since time.Time
	// job is the backup before the update, or the one to mark as it.
	job  *batchv1.Job
	step updateStep
	// applied says that the update has been applied, to to; moved that the
	// version that runs has moved to to, and all that is left of the update is
	// what its backup is still owed.
	applied, moved bool
	// stopOwed names the backup a stop is owed to, or is stopOwedUnknown; ""
	// where none is owed.
	stopOwed string
}

// waits says whether an update waits for the backup before it, or is applied
// on this pass while the version it is from still runs.
func (u updateBackup) waits() bool { return u.from != "" && !u.moved }

// record is what the backups' CronJob carries of the update: its count while
// it waits (heldBack), and once it is applied, until the version that runs
// has moved and the backup before it has been marked and stopped as it owes,
// the version it was applied to and whether a stop is owed.
func (u updateBackup) record() map[string]string {
	m := u.heldBack()
	if m == nil || !u.applied {
		return m
	}
	m[annotationBackupBeforeUpdateTo] = u.to
	if u.stopOwed != "" {
		m[annotationBackupBeforeUpdateStopOwed] = u.stopOwed
	}
	return m
}

// updateRecord is the record the backups' CronJob carries (record).
type updateRecord struct {
	from, to, stopOwed string
	since              time.Time
}

// deadline is when a backup before the update that still runs is stopped.
func (u updateBackup) deadline() time.Time { return u.since.Add(updateBackupWait) }

// heldBack is what the backups' CronJob carries of the update: the version
// that runs and when the update was first held back; nil where no update
// waits or has been applied while that version runs, or where the count has
// not started, as a restore runs.
func (u updateBackup) heldBack() map[string]string {
	if u.from == "" || u.since.IsZero() {
		return nil
	}
	return map[string]string{annotationBackupBeforeUpdate: u.from, annotationBackupBeforeUpdateSince: stamp(u.since)}
}

// stamp is a time as the annotations hold it.
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// planUpdateBackup decides, before the render, whether this pass holds an
// update back for the backup before it, and what it does for that backup.
// target is this pass's render tag; restoreDump is the dump a restore restores
// this pass, or "".
func (r *ZaentrumReconciler) planUpdateBackup(ctx context.Context, z *zaentrumv1alpha1.Zaentrum, target, restoreDump string) updateBackup {
	from := z.Status.CurrentVersion
	rec := r.cronRecord(ctx, z)
	if target == "" || from == "" || target == from || r.onAppliance(z) {
		return r.settleMoved(ctx, z, from, rec)
	}
	u := updateBackup{from: from, to: target, hold: true}
	if restoreDump != "" {
		// The update waits for the restore; the count starts after it.
		r.forgetUpdate(z)
		return u
	}
	logger := log.FromContext(ctx).WithValues("running", from)
	now := r.now().Time
	var held time.Time
	if rec.from == from {
		held = rec.since
	}
	all, err := r.allBackupJobs(ctx, z)
	if err == nil {
		u.job = beforeUpdateJob(z, all, from, held, now)
	}
	a := r.asked(z)
	switch {
	case !held.IsZero():
		u.since = held
	case u.job != nil:
		u.since, _ = sinceOf(u.job)
	case a.updateFrom == from && !a.updateSince.IsZero():
		u.since = a.updateSince
	default:
		u.since = now.UTC().Truncate(time.Second)
		logger.Info("update: " + target + " waits for a backup of the version that runs")
	}
	ours := rec.from == from && rec.since.Equal(u.since)
	remembered := a.updateApplied && a.updateFrom == from && a.updateSince.Equal(u.since)
	switch {
	case ours && rec.stopOwed != "":
		u.stopOwed = rec.stopOwed
	case remembered:
		u.stopOwed = a.updateStopOwed
	}
	r.remember(z, func(a *askedBackup) {
		if a.updateFrom != from || !a.updateSince.Equal(u.since) {
			a.updateFrom, a.updateSince, a.updateApplied, a.updateTo, a.updateStopOwed = from, u.since, false, "", ""
		}
	})
	if (ours && rec.to != "") || remembered || (u.job != nil && u.job.Annotations[annotationBackupBeforeUpdateTo] != "") {
		// Applied by a pass before whose apply did not get through: applied
		// again, without a second backup.
		r.applyUpdate(ctx, z, &u, all, err == nil, true)
		return u
	}
	bound := !now.Before(u.since.Add(updateHoldBound))
	if err != nil {
		if bound {
			logger.Info("update: the backups' Jobs could not be listed in 35 minutes; applying "+target+" without a backup",
				"error", err.Error())
			r.applyUpdate(ctx, z, &u, nil, false, false)
			return u
		}
		logger.Info("update: the backups' Jobs could not be listed; the update waits", "target", target, "error", err.Error())
		return u
	}
	_, stopped := stoppedAt(u.job)
	switch {
	case u.job == nil:
		switch running, writes := runningJob(all), r.stillWrites(ctx, all); {
		case running == nil && writes != nil && !bound:
			// A backup that has failed with a pod that may still write: no
			// backup starts beside it.
			logger.Info("update: a backup that has ended still has a pod that runs; the update to "+target+" waits for it",
				"job", writes.Name)
		case running == nil && now.Before(u.deadline()):
			u.step = updateStart
		case running == nil:
			logger.Info("update: no backup before " + target + " could be started in 30 minutes; applying it without one")
			r.applyUpdate(ctx, z, &u, all, true, false)
		case bound:
			logger.Info("update: the backup before "+target+" has not ended in 35 minutes; applying it", "job", running.Name)
			if running.DeletionTimestamp == nil {
				// Marked and stopped as the update is applied.
				u.job = running
			}
			r.applyUpdate(ctx, z, &u, all, true, false)
		case running.DeletionTimestamp != nil:
			logger.Info("update: a backup that is being deleted has not ended; the update to "+target+" waits for it", "job", running.Name)
		case now.Before(u.deadline()):
			u.job, u.step = running, updateMark
		default:
			// Found running only after the 30 minutes: marked and stopped
			// at once.
			u.job, u.step = running, updateStop
		}
	case !finished(u.job) || (stopped && r.podRuns(ctx, u.job)):
		// It runs, or a pod of the one stopped may still write.
		switch {
		case bound:
			logger.Info("update: the backup before "+target+" has not ended in 35 minutes; applying it", "job", u.job.Name)
			r.applyUpdate(ctx, z, &u, all, true, false)
		case stopped:
		case u.job.DeletionTimestamp != nil:
			logger.Info("update: a backup that is being deleted has not ended; the update to "+target+" waits for it", "job", u.job.Name)
		case !now.Before(u.deadline()):
			u.step = updateStop
		}
	case stopped:
		logger.Info("update: the stopped backup before "+target+" has ended; applying it", "job", u.job.Name)
		r.applyUpdate(ctx, z, &u, all, true, false)
	case failedJob(u.job):
		logger.Info("update: the backup before "+target+" failed; applying it", "job", u.job.Name)
		r.applyUpdate(ctx, z, &u, all, true, false)
	default:
		logger.Info("update: the backup before "+target+" has ended; applying it", "job", u.job.Name)
		r.applyUpdate(ctx, z, &u, all, true, false)
	}
	return u
}

// applyUpdate applies the update from this pass on, and records it: in this
// operator's memory, and on the backups' CronJob, which carries the version it
// is applied to and the backup a stop is owed to (record), and on the backup
// before it (settle). A backup of the count's that still runs as the update is
// first applied, not stopped nor being deleted, is owed a stop; where the
// backups' Jobs could not be listed then, one is owed to a backup that may run
// (stopOwedUnknown). all are the backups' Jobs, listed says that they could be
// listed, again that the update was applied on a pass before; where they could
// not be listed on a pass after, what is owed stays as it was.
func (r *ZaentrumReconciler) applyUpdate(ctx context.Context, z *zaentrumv1alpha1.Zaentrum, u *updateBackup,
	all []batchv1.Job, listed, again bool) {
	u.hold, u.step, u.applied = false, updateWait, true
	if job := u.job; !again && job != nil && job.DeletionTimestamp == nil && !finished(job) {
		if _, stopped := stoppedAt(job); !stopped {
			u.stopOwed = job.Name
		}
	}
	switch {
	case !listed && !again:
		u.stopOwed = stopOwedUnknown
	case !listed:
	case u.stopOwed != "":
		if u.job = owedJob(*u, all); u.job == nil {
			u.stopOwed = ""
		} else {
			u.stopOwed, _ = r.settle(ctx, *u)
		}
	case u.job != nil && u.job.DeletionTimestamp == nil:
		_, _ = r.settle(ctx, *u)
	}
	r.remember(z, func(a *askedBackup) {
		a.updateApplied, a.updateTo, a.updateStopOwed = true, u.to, u.stopOwed
	})
}

// owedJob is the backup u's stop is owed to: the Job it names, or where it
// names none, as the Jobs could not be listed, the newest of the count's that
// has not ended, else the newest backup that has not ended and began before
// the 35 minutes were over; nil where that is gone, has ended unnamed, or is
// being deleted, whose pods end with it. jobs are all the backups' Jobs,
// newest first.
func owedJob(u updateBackup, jobs []batchv1.Job) *batchv1.Job {
	if u.stopOwed != stopOwedUnknown {
		for i := range jobs {
			if jobs[i].Name == u.stopOwed {
				if jobs[i].DeletionTimestamp != nil {
					return nil
				}
				return &jobs[i]
			}
		}
		return nil
	}
	if job := countJob(u, jobs, false); job != nil {
		return job
	}
	bound := u.since.Add(updateHoldBound)
	for i := range jobs {
		if jobs[i].DeletionTimestamp == nil && !finished(&jobs[i]) && jobs[i].CreationTimestamp.Time.Before(bound) {
			return &jobs[i]
		}
	}
	return nil
}

// countJob is the newest backup marked with u's count that is not being
// deleted, of those that have ended too where ended says so, or nil.
func countJob(u updateBackup, jobs []batchv1.Job, ended bool) *batchv1.Job {
	for i := range jobs {
		job := &jobs[i]
		if t, err := sinceOf(job); err != nil || job.Annotations[annotationBackupBeforeUpdate] != u.from || !t.Equal(u.since) ||
			job.DeletionTimestamp != nil || (!ended && finished(job)) {
			continue
		}
		return job
	}
	return nil
}

// settle gives u's backup, once its update is applied, what it owes. One a
// stop is owed to that still runs, not stopped, is stopped, marked where it was
// not, and marked with the version its update was applied to, in one patch;
// one owed none that still runs is left as it is. One that ended before the
// stop it was owed is not marked, as what it wrote was not waited for, so it
// is not said to be made before the update; any other that ended, or was
// stopped, is marked with that version. It gives the backup a stop is still
// owed to, and the error of a patch refused, which it logs; a pass after tries
// again.
func (r *ZaentrumReconciler) settle(ctx context.Context, u updateBackup) (string, error) {
	job := u.job
	_, stopped := stoppedAt(job)
	runs := !finished(job)
	stop := runs && !stopped && u.stopOwed != ""
	switch {
	case stop:
	case runs && !stopped:
		return "", nil
	case !stopped && u.stopOwed != "":
		return "", nil
	case job.Annotations[annotationBackupBeforeUpdateTo] == u.to:
		return "", nil
	}
	logger := log.FromContext(ctx).WithValues("running", u.from, "job", job.Name)
	patch := client.MergeFrom(job.DeepCopy())
	set := map[string]string{annotationBackupBeforeUpdateTo: u.to}
	if stop {
		for k, v := range u.heldBack() {
			set[k] = v
		}
		set[annotationBackupBeforeUpdateStopped] = stamp(r.now().Time)
		one := int64(stopDeadline)
		job.Spec.ActiveDeadlineSeconds = &one
	}
	setAnnotations(job, set)
	err := r.Patch(ctx, job, patch)
	switch {
	case err != nil && stop:
		logger.Info("update: the backup before "+u.to+" could not be stopped; trying again", "error", err.Error())
		return job.Name, err
	case err != nil:
		logger.Info("update: the backup before "+u.to+" could not be marked as made before it; trying again", "error", err.Error())
	case stop:
		logger.Info("update: the backup before " + u.to + " still runs after 35 minutes; stopped it")
	}
	return "", err
}

// settleMoved does, once the version an update was applied to runs, what the
// passes that applied it could not: it settles the backup a stop is owed to
// (owedJob), or else the count's newest (settle), on every pass until that is
// done, and keeps the record on the backups' CronJob until then; then it
// forgets the update. from is the version that runs and rec the CronJob's
// record; where the CronJob carries none, what this operator remembers.
func (r *ZaentrumReconciler) settleMoved(ctx context.Context, z *zaentrumv1alpha1.Zaentrum, from string, rec updateRecord) updateBackup {
	if rec.to == "" || rec.to != from {
		a := r.asked(z)
		if !a.updateApplied || a.updateTo != from || a.updateFrom == "" || a.updateSince.IsZero() {
			r.forgetUpdate(z)
			return updateBackup{}
		}
		rec = updateRecord{from: a.updateFrom, since: a.updateSince, to: a.updateTo, stopOwed: a.updateStopOwed}
	}
	u := updateBackup{from: rec.from, to: rec.to, since: rec.since, applied: true, stopOwed: rec.stopOwed, moved: true}
	keep := func() updateBackup {
		r.remember(z, func(a *askedBackup) {
			a.updateFrom, a.updateSince, a.updateApplied, a.updateTo, a.updateStopOwed = u.from, u.since, true, u.to, u.stopOwed
		})
		return u
	}
	all, err := r.allBackupJobs(ctx, z)
	if err != nil {
		log.FromContext(ctx).Info("update: the backups' Jobs could not be listed; the backup before "+u.to+" is marked on a pass after",
			"error", err.Error())
		return keep()
	}
	if u.stopOwed != "" {
		u.job = owedJob(u, all)
	} else {
		u.job = countJob(u, all, true)
	}
	if u.job != nil {
		var err error
		if u.stopOwed, err = r.settle(ctx, u); err != nil {
			return keep()
		}
	}
	r.forgetUpdate(z)
	return updateBackup{}
}

// stepUpdateBackup takes this pass's step of the backup before an update,
// after the platform was applied with the update held back and the schedule
// held still: it marks the backup that runs, starts one, or stops one that has
// run for 30 minutes. platform is the pass's render; ask is the backup asked
// for, which a Job it starts answers too. It gives the backup before the
// update as it then is, and whether the update waits, so that the next pass
// comes soon.
func (r *ZaentrumReconciler) stepUpdateBackup(ctx context.Context, z *zaentrumv1alpha1.Zaentrum,
	platform []*unstructured.Unstructured, u updateBackup, ask backupAsk) (updateBackup, bool) {
	if !u.hold {
		return u, false
	}
	logger := log.FromContext(ctx).WithValues("running", u.from)
	switch u.step {
	case updateMark:
		patch := client.MergeFrom(u.job.DeepCopy())
		// A new mark: what an earlier one left goes with it.
		setAnnotations(u.job, u.heldBack(), annotationBackupBeforeUpdateStopped, annotationBackupBeforeUpdateTo)
		if err := r.Patch(ctx, u.job, patch); err != nil {
			logger.Info("update: the backup that runs could not be marked as the one before "+u.to+"; trying again",
				"job", u.job.Name, "error", err.Error())
			return u, true
		}
		logger.Info("update: backing up before applying "+u.to, "job", u.job.Name)
	case updateStart:
		cron := renderedObject(platform, "CronJob", templates.BackupCronJobName)
		if cron == nil {
			return u, true
		}
		if !ask.suspended {
			logger.Info("update: the backup before " + u.to + " waits for the schedule to stand still")
			return u, true
		}
		extra := u.heldBack()
		if ask.request != "" {
			extra[zaentrumv1alpha1.BackupRequestAnnotation] = ask.request
		}
		job, err := r.platformBackupJob(z, cron, extra)
		if err == nil {
			err = r.Create(ctx, job)
		}
		if err != nil {
			logger.Info("update: the backup before "+u.to+" could not be started; trying again", "error", err.Error())
			return u, true
		}
		u.job = &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: job.GetName(), Annotations: job.GetAnnotations()}}
		if ask.request != "" {
			r.remember(z, func(a *askedBackup) { a.request, a.job = ask.request, job.GetName() })
			logger = logger.WithValues("request", loggedRequest(ask.request))
		}
		logger.Info("update: backing up before applying "+u.to, "job", job.GetName())
	case updateStop:
		patch := client.MergeFrom(u.job.DeepCopy())
		// Marked in the same patch, where it was found running only after
		// the 30 minutes.
		set := u.heldBack()
		set[annotationBackupBeforeUpdateStopped] = stamp(r.now().Time)
		setAnnotations(u.job, set, annotationBackupBeforeUpdateTo)
		one := int64(stopDeadline)
		u.job.Spec.ActiveDeadlineSeconds = &one
		if err := r.Patch(ctx, u.job, patch); err != nil {
			logger.Info("update: the backup before "+u.to+" could not be stopped; trying again", "job", u.job.Name, "error", err.Error())
			return u, true
		}
		logger.Info("update: the backup before "+u.to+" has not ended in 30 minutes; stopped it and applying", "job", u.job.Name)
	}
	return u, true
}

// setAnnotations sets these annotations on the Job and takes those named off.
func setAnnotations(job *batchv1.Job, set map[string]string, remove ...string) {
	annotations := job.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	for _, k := range remove {
		delete(annotations, k)
	}
	for k, v := range set {
		annotations[k] = v
	}
	job.SetAnnotations(annotations)
}

// cronRecord is the record of an update the backups' CronJob carries
// (record), or none: where it carries none, or one from before a restore that
// ran since.
func (r *ZaentrumReconciler) cronRecord(ctx context.Context, z *zaentrumv1alpha1.Zaentrum) updateRecord {
	var cron batchv1.CronJob
	err := r.reader().Get(ctx, types.NamespacedName{Namespace: z.Namespace, Name: templates.BackupCronJobName}, &cron)
	if err != nil || !metav1.IsControlledBy(&cron, z) || cron.Annotations[annotationBackupBeforeUpdate] == "" {
		return updateRecord{}
	}
	t, err := time.Parse(time.RFC3339, cron.Annotations[annotationBackupBeforeUpdateSince])
	if err != nil {
		return updateRecord{}
	}
	if rs := restoreStatus(z); rs != nil && rs.FinishedAt != nil && rs.Result != zaentrumv1alpha1.RestoreRefused &&
		t.Before(rs.FinishedAt.Time) {
		return updateRecord{}
	}
	return updateRecord{from: cron.Annotations[annotationBackupBeforeUpdate], since: t,
		to: cron.Annotations[annotationBackupBeforeUpdateTo], stopOwed: cron.Annotations[annotationBackupBeforeUpdateStopOwed]}
}

// beforeUpdateJob is the backup before the update from the version that runs:
// where the CronJob carries when that update was first held back (held), the
// newest backup Job marked with that version and that time; otherwise the
// newest marked with that version in the last updateBackupWindow that has not
// ended, or that has ended, was marked after the last restore ended, and
// whose update was not applied. jobs are all the backups' Jobs, newest first.
func beforeUpdateJob(z *zaentrumv1alpha1.Zaentrum, jobs []batchv1.Job, from string, held, now time.Time) *batchv1.Job {
	var after time.Time
	if rs := restoreStatus(z); rs != nil && rs.FinishedAt != nil {
		after = rs.FinishedAt.Time
	}
	for i := range jobs {
		job := &jobs[i]
		t, err := sinceOf(job)
		if job.Annotations[annotationBackupBeforeUpdate] != from || err != nil {
			continue
		}
		switch {
		case !held.IsZero():
			if t.Equal(held) {
				return job
			}
		case now.Sub(t) > updateBackupWindow:
		case !finished(job):
			return job
		case !t.Before(after) && job.Annotations[annotationBackupBeforeUpdateTo] == "":
			return job
		}
	}
	return nil
}

// sinceOf is when the update the backup is marked with was first held back.
func sinceOf(job *batchv1.Job) (time.Time, error) {
	return time.Parse(time.RFC3339, job.Annotations[annotationBackupBeforeUpdateSince])
}

// runningJob is the newest of the Jobs that has not ended, or nil.
func runningJob(jobs []batchv1.Job) *batchv1.Job {
	for i := range jobs {
		if !finished(&jobs[i]) {
			return &jobs[i]
		}
	}
	return nil
}

func finished(job *batchv1.Job) bool {
	done, _ := jobFinished(job)
	return done
}

func failedJob(job *batchv1.Job) bool {
	_, failed := jobFinished(job)
	return failed != nil
}

// stoppedAt is when the operator stopped the backup for the update it is
// marked with, if it did: the annotation holds a time at that update's 30
// minutes or later, and the Job's deadline is the stop's. A time set by hand on
// a Job whose deadline was not cut is no stop, nor is one from another update:
// a new mark takes it off.
func stoppedAt(job *batchv1.Job) (time.Time, bool) {
	if job == nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, job.Annotations[annotationBackupBeforeUpdateStopped])
	since, serr := sinceOf(job)
	if d := job.Spec.ActiveDeadlineSeconds; err != nil || serr != nil || d == nil || *d != stopDeadline ||
		t.Before(since.Add(updateBackupWait)) {
		return time.Time{}, false
	}
	return t, true
}

// podRuns says whether a pod of the Job still runs, or might: one that has
// not ended, or whose pods cannot be listed.
func (r *ZaentrumReconciler) podRuns(ctx context.Context, job *batchv1.Job) bool {
	pods, err := r.runPods(ctx, job)
	if err != nil {
		return true
	}
	for i := range pods {
		if p := pods[i].Status.Phase; p != corev1.PodSucceeded && p != corev1.PodFailed {
			return true
		}
	}
	return false
}

// forgetUpdate forgets the update this operator held back or applied.
func (r *ZaentrumReconciler) forgetUpdate(z *zaentrumv1alpha1.Zaentrum) {
	r.remember(z, func(a *askedBackup) {
		a.updateFrom, a.updateSince, a.updateApplied, a.updateTo, a.updateStopOwed = "", time.Time{}, false, "", ""
	})
}

// updatePending says whether an update from the version that runs is held
// back or applied while that version still runs, as this operator remembers it
// or the backups' CronJob carries it.
func (r *ZaentrumReconciler) updatePending(ctx context.Context, z *zaentrumv1alpha1.Zaentrum) bool {
	from := z.Status.CurrentVersion
	return from != "" && (r.asked(z).updateFrom == from || r.cronRecord(ctx, z).from == from)
}

// onAppliance says whether this operator is the one-machine install's: by the
// install source it last read of itself (controllerReport), or, before it has
// read one, by the one the status keeps from a reading. One never read is a
// cluster install's.
func (r *ZaentrumReconciler) onAppliance(z *zaentrumv1alpha1.Zaentrum) bool {
	source := r.readSource()
	if source == "" && z.Status.Controller != nil && z.Status.Controller.Source != zaentrumv1alpha1.InstallSourceUnknown {
		source = z.Status.Controller.Source
	}
	return source == zaentrumv1alpha1.InstallSourceAppliance
}

// updateEnding is what the Backup condition's sentence ends with while an
// update waits for the backup before it, or "": jobs are the backups' Jobs as
// the pass reads them, so that a backup that has ended meanwhile is not said
// to run.
func updateEnding(u updateBackup, jobs []batchv1.Job) string {
	if !u.hold || u.job == nil {
		return ""
	}
	for i := range jobs {
		if jobs[i].Name == u.job.Name && !finished(&jobs[i]) {
			return "; " + u.job.Name + " backs up " + u.from + " before the update to " + u.to +
				": the update is applied once it has ended; a backup that still runs at " +
				u.deadline().UTC().Format("2006-01-02 15:04 MST") + " is stopped"
		}
	}
	return ""
}
