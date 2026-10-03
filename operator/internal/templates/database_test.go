package templates

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
)

// postgres is the rendered Postgres Deployment, typed.
func postgres(t *testing.T, objs []*unstructured.Unstructured) *appsv1.Deployment {
	t.Helper()
	u := find(t, objs, "Deployment", "postgres")
	require.NotNil(t, u, "no bundled Postgres")
	var dep appsv1.Deployment
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &dep))
	return &dep
}

// dataVolume is the Postgres's data volume.
func dataVolume(t *testing.T, dep *appsv1.Deployment) corev1.Volume {
	t.Helper()
	for _, v := range dep.Spec.Template.Spec.Volumes {
		if v.Name == "data" {
			return v
		}
	}
	t.Fatal("the Postgres has no data volume")
	return corev1.Volume{}
}

func renderWith(t *testing.T, z *zaentrumv1alpha1.Zaentrum, volume string, migrate bool) []*unstructured.Unstructured {
	t.Helper()
	v := NewValues(z)
	v.PostgresVolume = volume
	v.PostgresMigrate = migrate
	objs, err := Render(v)
	require.NoError(t, err)
	return objs
}

// A new install keeps its database on a claim the chart provisions: users,
// watch state and the catalog outlive the pod.
func TestPostgresKeepsItsDataOnAClaim(t *testing.T) {
	for name, objs := range map[string][]*unstructured.Unstructured{
		"operator, new install": renderWith(t, base("zaentrum"), "", false),
		"helm install":          helmRender(t, nil),
	} {
		vol := dataVolume(t, postgres(t, objs))
		require.NotNil(t, vol.PersistentVolumeClaim, name)
		assert.Equal(t, "postgres-data", vol.PersistentVolumeClaim.ClaimName, name)
		assert.Nil(t, vol.EmptyDir, name)

		pvc := find(t, objs, "PersistentVolumeClaim", "postgres-data")
		require.NotNil(t, pvc, name)
		var claim corev1.PersistentVolumeClaim
		require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(pvc.Object, &claim))
		assert.Equal(t, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, claim.Spec.AccessModes, name)
		assert.Equal(t, "10Gi", claim.Spec.Resources.Requests.Storage().String(), name)
		assert.Nil(t, claim.Spec.StorageClassName, "%s: the cluster's default StorageClass", name)
		assert.Equal(t, appsv1.RecreateDeploymentStrategyType, postgres(t, objs).Spec.Strategy.Type,
			"one Postgres at a time on a ReadWriteOnce claim")
		_, hooks := SplitHooks(objs)
		assert.Nil(t, PostgresMigrationJob(hooks), "%s: nothing to move", name)
	}
}

// Its size and class are the CR's; a claim the install brings is used, not
// made.
func TestPostgresClaimFromTheCR(t *testing.T) {
	z := base("zaentrum")
	z.Spec.Storage.ClassName = "standard"
	objs := renderWith(t, z, "", false)
	claim, _, _ := unstructured.NestedString(find(t, objs, "PersistentVolumeClaim", "postgres-data").Object, "spec", "storageClassName")
	assert.Equal(t, "standard", claim, "storage.className, for every platform claim")

	z.Spec.Storage.Postgres.ClassName = "fast"
	z.Spec.Storage.Postgres.Size = resource.MustParse("40Gi")
	objs = renderWith(t, z, "", false)
	pvc := find(t, objs, "PersistentVolumeClaim", "postgres-data")
	claim, _, _ = unstructured.NestedString(pvc.Object, "spec", "storageClassName")
	assert.Equal(t, "fast", claim)
	size, _, _ := unstructured.NestedString(pvc.Object, "spec", "resources", "requests", "storage")
	assert.Equal(t, "40Gi", size)

	z.Spec.Storage.Postgres.ClaimName = "pg-on-nfs"
	objs = renderWith(t, z, "pg-on-nfs", false)
	assert.Nil(t, find(t, objs, "PersistentVolumeClaim", "postgres-data"), "an install's own claim is not made")
	assert.Nil(t, find(t, objs, "PersistentVolumeClaim", "pg-on-nfs"))
	assert.Equal(t, "pg-on-nfs", dataVolume(t, postgres(t, objs)).PersistentVolumeClaim.ClaimName)
}

// An install whose Postgres runs on emptyDir gets exactly the Deployment it
// has: a change to its pod template would roll the pod, and the emptyDir and
// the whole database with it. testdata/postgres-emptydir.yaml is the render
// before the chart kept the database on a claim.
func TestPostgresOnEmptyDirRendersAsBefore(t *testing.T) {
	raw, err := os.ReadFile("testdata/postgres-emptydir.yaml")
	require.NoError(t, err)
	var want map[string]any
	require.NoError(t, yaml.Unmarshal(raw, &want))

	v := NewValues(demoCR("zaentrum-demo"))
	v.OpenShift = true
	v.PostgresVolume = "emptyDir"
	objs, err := Render(v)
	require.NoError(t, err)
	got := find(t, objs, "Deployment", "postgres")
	require.NotNil(t, got)
	assert.Equal(t, want, got.Object, "the emptyDir Postgres's Deployment changed: it would restart and lose its database")
	assert.Nil(t, find(t, objs, "PersistentVolumeClaim", "postgres-data"), "no claim it would not use")
	_, hooks := SplitHooks(objs)
	assert.Nil(t, PostgresMigrationJob(hooks))
}

// helm upgrade of a release whose Postgres runs on emptyDir, or on another
// claim, keeps it there: no upgrade moves a database by itself.
func TestPostgresStaysWhereItRunsUnderHelm(t *testing.T) {
	running := func(volume corev1.Volume) runtime.Object {
		return &appsv1.Deployment{
			TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
			ObjectMeta: metav1.ObjectMeta{Name: "postgres", Namespace: "zaentrum"},
			Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Volumes: []corev1.Volume{
				volume, {Name: "initdb", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "postgres-initdb"}}}},
			}}}},
		}
	}
	upgrade := func(live runtime.Object, vals map[string]interface{}) []*unstructured.Unstructured {
		objs, err := render(vals, "zaentrum", false, newCluster(t, live))
		require.NoError(t, err)
		return objs
	}

	emptyDir := corev1.Volume{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}
	objs := upgrade(running(emptyDir), nil)
	assert.NotNil(t, dataVolume(t, postgres(t, objs)).EmptyDir, "kept on its emptyDir")
	assert.Nil(t, find(t, objs, "PersistentVolumeClaim", "postgres-data"))

	other := corev1.Volume{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "old-claim"}}}
	objs = upgrade(running(other), nil)
	assert.Equal(t, "old-claim", dataVolume(t, postgres(t, objs)).PersistentVolumeClaim.ClaimName, "kept on its claim")

	// Step one of a move: the copy runs, the Postgres stays.
	objs = upgrade(running(emptyDir), map[string]interface{}{"storage": map[string]interface{}{"postgres": map[string]interface{}{"migrate": true}}})
	assert.NotNil(t, dataVolume(t, postgres(t, objs)).EmptyDir)
	assert.NotNil(t, find(t, objs, "PersistentVolumeClaim", "postgres-data"), "the claim the copy goes onto")
	_, hooks := SplitHooks(objs)
	require.NotNil(t, PostgresMigrationJob(hooks))
	// Step two: switched on purpose.
	objs = upgrade(running(emptyDir), map[string]interface{}{"storage": map[string]interface{}{"postgres": map[string]interface{}{"current": "postgres-data"}}})
	assert.Equal(t, "postgres-data", dataVolume(t, postgres(t, objs)).PersistentVolumeClaim.ClaimName)
}

// The copy: a post-upgrade hook, from the Postgres's own image and Secret,
// onto the claim mounted where the Postgres mounts it, while the Postgres
// stays where it is.
func TestPostgresMigrationJob(t *testing.T) {
	v := NewValues(demoCR("zaentrum-demo"))
	v.OpenShift = true
	v.PostgresVolume = "emptyDir"
	v.PostgresMigrate = true
	objs, err := Render(v)
	require.NoError(t, err)
	platform, hooks := SplitHooks(objs)

	assert.NotNil(t, dataVolume(t, postgres(t, platform)).EmptyDir, "nothing switches while the copy runs")
	assert.NotNil(t, find(t, platform, "PersistentVolumeClaim", "postgres-data"), "the claim the copy goes onto")
	u := PostgresMigrationJob(hooks)
	require.NotNil(t, u)
	assert.Nil(t, find(t, platform, "Job", PostgresMigrationJobName), "never applied with the platform")
	var job batchv1.Job
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &job))
	assert.Equal(t, "post-upgrade", job.Annotations["helm.sh/hook"])
	assert.Equal(t, "emptyDir", job.Annotations[AnnotationMigrationSource])
	assert.Equal(t, "postgres-data", job.Annotations[AnnotationMigrationTarget])
	require.NotNil(t, job.Spec.BackoffLimit)
	assert.Equal(t, int32(0), *job.Spec.BackoffLimit, "a failed copy is looked at, not repeated")
	assert.Nil(t, job.Spec.TTLSecondsAfterFinished, "kept until deleted: it decides whether the Postgres may switch")
	require.NotNil(t, job.Spec.Template.Spec.AutomountServiceAccountToken)
	assert.False(t, *job.Spec.Template.Spec.AutomountServiceAccountToken)

	pg := postgres(t, platform).Spec.Template.Spec
	c := job.Spec.Template.Spec.Containers[0]
	assert.Equal(t, PostgresMigrationContainer, c.Name)
	assert.Equal(t, pg.Containers[0].Image, c.Image, "the image the Postgres runs")
	assert.Equal(t, pg.SecurityContext, job.Spec.Template.Spec.SecurityContext, "the user the Postgres runs as")
	script, err := os.ReadFile("../../platform/chart/files/postgres-migrate.sh")
	require.NoError(t, err)
	assert.Equal(t, []string{"/bin/bash", "-c"}, c.Command)
	assert.Equal(t, []string{string(script)}, c.Args, "the script runs exactly as shipped")
	env := envByName(c)
	pgEnv := envByName(pg.Containers[0])
	for _, name := range []string{"POSTGRES_USER", "POSTGRES_PASSWORD", "POSTGRES_DB", "PGDATA"} {
		assert.Equal(t, pgEnv[name], env[name], "%s as the Postgres has it", name)
	}
	assert.Equal(t, "postgres", env["SOURCE_HOST"].Value, "the running Postgres, through its Service")
	require.Len(t, c.VolumeMounts, 1)
	assert.Equal(t, "/var/lib/postgresql/data", c.VolumeMounts[0].MountPath, "where the Postgres mounts it")
	require.Len(t, job.Spec.Template.Spec.Volumes, 1)
	assert.Equal(t, "postgres-data", job.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName)
	assert.Equal(t, corev1.TerminationMessageReadFile, c.TerminationMessagePolicy)

	// On the claim already, there is nothing to copy, migrate or not.
	_, hooks = SplitHooks(renderWith(t, demoCR("zaentrum-demo"), "postgres-data", true))
	assert.Nil(t, PostgresMigrationJob(hooks))
}

// A shared database cluster is the tenant's: no bundled Postgres, no claim,
// no copy.
func TestPostgresWithAnExternalDatabase(t *testing.T) {
	z := base("zaentrum-beta")
	z.Spec.Databases.Mode = "external"
	z.Spec.Databases.External.Host = "postgres.example.com"
	objs := renderWith(t, z, "", true)
	assert.Nil(t, find(t, objs, "Deployment", "postgres"))
	assert.Nil(t, find(t, objs, "PersistentVolumeClaim", "postgres-data"))
	_, hooks := SplitHooks(objs)
	assert.Nil(t, PostgresMigrationJob(hooks))
}
