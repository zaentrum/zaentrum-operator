package templates

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// backupCron is the rendered backup CronJob, typed, or nil.
func backupCron(t *testing.T, objs []*unstructured.Unstructured) *batchv1.CronJob {
	t.Helper()
	u := find(t, objs, "CronJob", BackupCronJobName)
	if u == nil {
		return nil
	}
	var cron batchv1.CronJob
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &cron))
	return &cron
}

// Backups run wherever the bundled Postgres keeps its data on a claim — a new
// install, through the operator or helm — and not where it still runs on an
// emptyDir (the demo), nor with external databases (beta); the CR turns them
// on or off against that.
func TestBackupsRunWhereThePostgresKeepsItsDataOnAClaim(t *testing.T) {
	yes, no := true, false
	external := base("zaentrum-beta")
	external.Spec.Databases.Mode = "external"
	external.Spec.Databases.External.Host = "postgres.example.com"
	off := base("zaentrum")
	off.Spec.Backup.Enabled = &no
	on := demoCR("zaentrum-demo")
	on.Spec.Backup.Enabled = &yes

	for name, c := range map[string]struct {
		objs []*unstructured.Unstructured
		want bool
	}{
		"a new install":                       {renderWith(t, base("zaentrum"), "", false), true},
		"on its claim":                        {renderWith(t, base("zaentrum"), "postgres-data", false), true},
		"on another claim":                    {renderWith(t, base("zaentrum"), "old-claim", false), true},
		"helm install":                        {helmRender(t, nil), true},
		"the demo, on an emptyDir":            {renderWith(t, demoCR("zaentrum-demo"), "emptyDir", false), false},
		"external databases":                  {renderWith(t, external, "", false), false},
		"turned off":                          {renderWith(t, off, "", false), false},
		"turned on, the Postgres on emptyDir": {renderWith(t, on, "emptyDir", false), true},
	} {
		assert.Equal(t, c.want, backupCron(t, c.objs) != nil, "%s: the backup CronJob", name)
		assert.Equal(t, c.want, find(t, c.objs, "PersistentVolumeClaim", "backups") != nil, "%s: the claim backups", name)
	}

	// The demo renders as it did: nothing of backups, nothing stopped.
	demo := renderWith(t, demoCR("zaentrum-demo"), "emptyDir", false)
	_, hooks := SplitHooks(demo)
	assert.Nil(t, RestoreJob(hooks))
	assert.NotContains(t, fmt.Sprintf("%v", demo), "zaentrum-backup")
}

// The CronJob: daily by default, one run at a time, every platform database
// of the CR's, the newest seven kept, from the image the Postgres runs and the
// script as shipped, restricted like every platform pod; and the claim: 5Gi,
// ReadWriteOnce, the Postgres's StorageClass, kept when the platform goes.
func TestBackupCronJobAndItsClaim(t *testing.T) {
	z := base("zaentrum")
	objs := renderWith(t, z, "", false)
	cron := backupCron(t, objs)
	require.NotNil(t, cron)
	assert.Equal(t, "@daily", cron.Spec.Schedule)
	assert.Equal(t, batchv1.ForbidConcurrent, cron.Spec.ConcurrencyPolicy)
	require.NotNil(t, cron.Spec.Suspend)
	assert.False(t, *cron.Spec.Suspend)
	assert.Equal(t, "backup", cron.Spec.JobTemplate.Labels["zaentrum.io/database"], "the operator finds its Jobs by it")
	pod := cron.Spec.JobTemplate.Spec.Template.Spec
	assert.Equal(t, corev1.RestartPolicyNever, pod.RestartPolicy)
	require.NotNil(t, pod.AutomountServiceAccountToken)
	assert.False(t, *pod.AutomountServiceAccountToken)
	require.NotNil(t, pod.SecurityContext)
	assert.True(t, *pod.SecurityContext.RunAsNonRoot)
	assert.Equal(t, int64(65532), *pod.SecurityContext.RunAsUser)
	assert.Equal(t, int64(65532), *pod.SecurityContext.FSGroup, "the dumps' group owns what it writes")

	require.Len(t, pod.Containers, 1)
	c := pod.Containers[0]
	assert.Equal(t, BackupContainer, c.Name)
	assert.Equal(t, postgres(t, objs).Spec.Template.Spec.Containers[0].Image, c.Image, "pg_dump of the server's version")
	script, err := os.ReadFile("../../platform/chart/files/postgres-backup.sh")
	require.NoError(t, err)
	assert.Equal(t, []string{"/bin/bash", "-c"}, c.Command)
	assert.Equal(t, []string{string(script)}, c.Args, "the script runs exactly as shipped")
	assert.True(t, *c.SecurityContext.ReadOnlyRootFilesystem)
	assert.False(t, *c.SecurityContext.AllowPrivilegeEscalation)
	assert.Equal(t, corev1.TerminationMessageReadFile, c.TerminationMessagePolicy, "its summary is the termination message")
	env := envByName(c)
	assert.Equal(t, "postgres", env["PGHOST"].Value)
	assert.Equal(t, "chino katalog keycloak portal", env["DATABASES"].Value)
	assert.Equal(t, "7", env["RETENTION"].Value)
	assert.Equal(t, "zaentrum-db", env["PGPASSWORD"].ValueFrom.SecretKeyRef.Name)
	assert.Equal(t, "backups", volumeClaim(pod, "backups"))

	pvc := find(t, objs, "PersistentVolumeClaim", "backups")
	require.NotNil(t, pvc)
	var claim corev1.PersistentVolumeClaim
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(pvc.Object, &claim))
	assert.Equal(t, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, claim.Spec.AccessModes)
	assert.Equal(t, "5Gi", claim.Spec.Resources.Requests.Storage().String())
	assert.Nil(t, claim.Spec.StorageClassName, "the cluster's default, as postgres-data's")
	assert.Equal(t, "keep", claim.Annotations["helm.sh/resource-policy"], "the backups outlive the platform")

	// The CR's schedule, retention, databases, size and class; the class
	// follows the Postgres's unless the CR names one.
	z.Spec.Backup.Schedule = "30 3 * * *"
	z.Spec.Backup.Retention = 14
	z.Spec.Backup.Size = resource.MustParse("20Gi")
	z.Spec.Databases.Chino, z.Spec.Databases.Portal = "media", "launchpad"
	z.Spec.Storage.ClassName = "standard"
	z.Spec.Storage.Postgres.ClassName = "fast"
	objs = renderWith(t, z, "", false)
	cron = backupCron(t, objs)
	assert.Equal(t, "30 3 * * *", cron.Spec.Schedule)
	env = envByName(cron.Spec.JobTemplate.Spec.Template.Spec.Containers[0])
	assert.Equal(t, "14", env["RETENTION"].Value)
	assert.Equal(t, "media katalog keycloak launchpad", env["DATABASES"].Value)
	class, _, _ := unstructured.NestedString(find(t, objs, "PersistentVolumeClaim", "backups").Object, "spec", "storageClassName")
	assert.Equal(t, "fast", class, "the Postgres's class")
	size, _, _ := unstructured.NestedString(find(t, objs, "PersistentVolumeClaim", "backups").Object, "spec", "resources", "requests", "storage")
	assert.Equal(t, "20Gi", size)
	z.Spec.Backup.ClassName = "archive"
	class, _, _ = unstructured.NestedString(find(t, renderWith(t, z, "", false), "PersistentVolumeClaim", "backups").Object, "spec", "storageClassName")
	assert.Equal(t, "archive", class)

	// A claim of the install's own: none is made, the CronJob mounts it.
	z.Spec.Backup.ClaimName = "kept-backups"
	objs = renderWith(t, z, "", false)
	assert.Nil(t, find(t, objs, "PersistentVolumeClaim", "backups"))
	assert.Equal(t, "kept-backups", volumeClaim(backupCron(t, objs).Spec.JobTemplate.Spec.Template.Spec, "backups"))

	// On OpenShift the SCC gives the user and the group.
	v := NewValues(base("zaentrum"))
	v.OpenShift = true
	objs, err = Render(v)
	require.NoError(t, err)
	sc := backupCron(t, objs).Spec.JobTemplate.Spec.Template.Spec.SecurityContext
	assert.Nil(t, sc.RunAsUser)
	assert.Nil(t, sc.FSGroup)
}

// volumeClaim is the claim the pod mounts as the volume of that name.
func volumeClaim(pod corev1.PodSpec, volume string) string {
	for _, v := range pod.Volumes {
		if v.Name == volume && v.PersistentVolumeClaim != nil {
			return v.PersistentVolumeClaim.ClaimName
		}
	}
	return ""
}

// readsTheDatabase says whether a container reads Secret zaentrum-db: a client
// of the bundled Postgres.
func readsTheDatabase(pod corev1.PodSpec) bool {
	for _, c := range append(append([]corev1.Container{}, pod.InitContainers...), pod.Containers...) {
		for _, e := range c.Env {
			if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil && e.ValueFrom.SecretKeyRef.Name == "zaentrum-db" {
				return true
			}
		}
	}
	return false
}

// While a restore runs, every client of the database is stopped — each
// Deployment with a session in it, and the pipeline's workers and
// katalog-ingest, which write through katalog-manager-api — the backups are
// suspended, and the restore Job is among the hooks; the rest of the platform
// stays up. Without a restore, nothing of it.
func TestARestoreStopsTheDatabasesClients(t *testing.T) {
	z := base("zaentrum")
	z.Spec.Features.Pipeline = true
	z.Spec.Pipeline.Encoder = "cpu"
	v := NewValues(z)
	v.RestoreDump = "2026-10-04T00-00-05Z"
	objs, err := Render(v)
	require.NoError(t, err)
	platform, hooks := SplitHooks(objs)
	normal := renderCR(t, z)

	stopped := map[string]bool{}
	for _, o := range platform {
		if o.GetKind() != "Deployment" {
			continue
		}
		dep := typedDeployment(t, platform, o.GetName())
		if *dep.Spec.Replicas == 0 {
			stopped[o.GetName()] = true
			continue
		}
		assert.Equal(t, replicas(t, normal, o.GetName()), int64(*dep.Spec.Replicas), "%s keeps its replicas", o.GetName())
		assert.False(t, readsTheDatabase(dep.Spec.Template.Spec) && o.GetName() != "postgres",
			"%s reads the database and is not stopped for a restore", o.GetName())
	}
	assert.Equal(t, map[string]bool{
		"chino-api": true, "katalog-api": true, "katalog-manager-api": true, "keycloak": true, "portal-api": true,
		"analyzer": true, "transcoder": true, "packager": true, "katalog-ingest": true,
	}, stopped)
	assert.True(t, *backupCron(t, platform).Spec.Suspend, "no backup while a restore runs")

	u := RestoreJob(hooks)
	require.NotNil(t, u)
	assert.Nil(t, find(t, platform, "Job", RestoreJobName), "never applied with the platform")
	var job batchv1.Job
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &job))
	assert.Equal(t, "post-upgrade", job.Annotations["helm.sh/hook"])
	assert.Equal(t, "2026-10-04T00-00-05Z", job.Annotations[AnnotationRestoreDump])
	assert.Equal(t, int32(0), *job.Spec.BackoffLimit, "a refusal or a failure is read, not repeated")
	pod := job.Spec.Template.Spec
	assert.False(t, *pod.AutomountServiceAccountToken)
	c := pod.Containers[0]
	assert.Equal(t, RestoreContainer, c.Name)
	script, err := os.ReadFile("../../platform/chart/files/postgres-restore.sh")
	require.NoError(t, err)
	assert.Equal(t, []string{string(script)}, c.Args, "the script runs exactly as shipped")
	env := envByName(c)
	assert.Equal(t, "2026-10-04T00-00-05Z", env["DUMP"].Value)
	assert.Equal(t, "chino katalog keycloak portal", env["DATABASES"].Value)
	assert.Equal(t, "backups", volumeClaim(pod, "backups"))
	for _, m := range c.VolumeMounts {
		if m.Name == "backups" {
			assert.True(t, m.ReadOnly, "a restore never writes the dumps")
		}
	}

	_, hooks = SplitHooks(normal)
	assert.Nil(t, RestoreJob(hooks), "no restore asked for, none rendered")
	assert.False(t, *backupCron(t, normal).Spec.Suspend)

	// helm: backup.restore stops the clients and renders the hook.
	objs = helmRender(t, map[string]interface{}{"backup": map[string]interface{}{"restore": "2026-10-04T00-00-05Z"}})
	assert.Equal(t, int64(0), replicas(t, objs, "keycloak"))
	_, hooks = SplitHooks(objs)
	assert.NotNil(t, RestoreJob(hooks))

	// External databases: nothing of the platform's to restore, nothing stopped.
	ext := base("zaentrum-beta")
	ext.Spec.Databases.Mode = "external"
	ext.Spec.Databases.External.Host = "postgres.example.com"
	v = NewValues(ext)
	v.RestoreDump = "2026-10-04T00-00-05Z"
	objs, err = Render(v)
	require.NoError(t, err)
	assert.Equal(t, int64(1), replicas(t, objs, "chino-api"))
	_, hooks = SplitHooks(objs)
	assert.Nil(t, RestoreJob(hooks))
}

// The restore's names the operator relies on are the chart's.
func TestRestoreNamesAreTheCharts(t *testing.T) {
	raw, err := os.ReadFile("../../platform/chart/templates/postgres-restore.yaml")
	require.NoError(t, err)
	assert.True(t, strings.Contains(string(raw), "name: "+RestoreJobName))
	assert.True(t, strings.Contains(string(raw), "- name: "+RestoreContainer))
	raw, err = os.ReadFile("../../platform/chart/files/postgres-restore.sh")
	require.NoError(t, err)
	assert.Contains(t, string(raw), fmt.Sprintf("exit %d", RestoreRefusedExitCode), "a refusal's exit code")
}
