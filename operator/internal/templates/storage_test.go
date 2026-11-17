package templates

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
)

const platformUID = "6b2f0c1e-0000-4000-8000-000000000001"

// measured is a platform the operator renders: with its uid.
func measured() *zaentrumv1alpha1.Zaentrum {
	z := base("zaentrum")
	z.UID = types.UID(platformUID)
	return z
}

// measureRender renders the platform as the operator does, its Postgres on
// the volume given ("" for a new install's claim), and splits off the hooks.
func measureRender(t *testing.T, z *zaentrumv1alpha1.Zaentrum, postgres string, openShift bool) (platform, hooks []*unstructured.Unstructured) {
	t.Helper()
	v := NewValues(z)
	v.PostgresVolume = postgres
	v.OpenShift = openShift
	objs, err := Render(v)
	require.NoError(t, err)
	return SplitHooks(objs)
}

func measureJobOf(t *testing.T, hooks []*unstructured.Unstructured, volume string) *batchv1.Job {
	t.Helper()
	hook := MeasureJob(hooks, volume)
	if hook == nil {
		return nil
	}
	var job batchv1.Job
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(hook.Object, &job))
	return &job
}

func envOf(c corev1.Container) map[string]corev1.EnvVar {
	out := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		out[e.Name] = e
	}
	return out
}

func TestRC7MeasuringJobs(t *testing.T) {
	t.Run("R.C7 plain Helm renders no measuring Job and no ConfigMap zaentrum-backup", func(t *testing.T) {
		objs := helmRender(t, nil)
		for _, o := range objs {
			assert.NotEqual(t, "zaentrum-measure", o.GetLabels()["app.kubernetes.io/name"], "%s/%s", o.GetKind(), o.GetName())
		}
		assert.Nil(t, find(t, objs, "ConfigMap", BackupRecordName))
		_, hooks := measureRender(t, base("zaentrum"), "", false)
		assert.Nil(t, MeasureJob(hooks, VolumeMedia), "nor the operator without the resource's uid")
	})

	t.Run("R.C7 one hook for each claim the platform uses, never applied with it", func(t *testing.T) {
		platform, hooks := measureRender(t, measured(), "", false)
		for _, o := range platform {
			assert.NotEqual(t, "zaentrum-measure", o.GetLabels()["app.kubernetes.io/name"], "%s/%s is applied", o.GetKind(), o.GetName())
		}
		media, backups := measureJobOf(t, hooks, VolumeMedia), measureJobOf(t, hooks, VolumeBackups)
		require.NotNil(t, media)
		require.NotNil(t, backups)
		assert.Nil(t, measureJobOf(t, hooks, VolumeKafka), "the event broker has no claim")
		assert.Equal(t, "media", media.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName)
		assert.Equal(t, "backups", backups.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName)
		assert.Equal(t, VolumeMedia, media.Labels[LabelMeasure])

		z := measured()
		z.Spec.Backup.ClaimName = "kept-backups"
		z.Spec.Storage.KafkaPVC = "kafka-log"
		z.Spec.Storage.KafkaNode = "node-a"
		_, hooks = measureRender(t, z, "", false)
		assert.Equal(t, "kept-backups", measureJobOf(t, hooks, VolumeBackups).Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName)
		kafka := measureJobOf(t, hooks, VolumeKafka)
		require.NotNil(t, kafka)
		assert.Equal(t, "kafka-log", kafka.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName)
		assert.Equal(t, map[string]string{"kubernetes.io/hostname": "node-a"}, kafka.Spec.Template.Spec.NodeSelector,
			"where the broker's node-local volume is")
		assert.Empty(t, measureJobOf(t, hooks, VolumeMedia).Spec.Template.Spec.NodeSelector)

		z.Spec.EventStreaming.Mode = "external"
		z.Spec.EventStreaming.Bootstrap = "kafka.example.org:9093"
		_, hooks = measureRender(t, z, "", false)
		assert.Nil(t, measureJobOf(t, hooks, VolumeKafka), "a broker outside the platform is not its to measure")

		no := false
		z = measured()
		z.Spec.Backup.Enabled = &no
		_, hooks = measureRender(t, z, "", false)
		assert.NotNil(t, measureJobOf(t, hooks, VolumeBackups), "with backups off the backups' claim is measured for a restore")
	})

	t.Run("R.C7 each mounts its claim read-only, at the claim too, under the platform's own security", func(t *testing.T) {
		for _, openShift := range []bool{false, true} {
			z := measured()
			z.Spec.Storage.KafkaPVC = "kafka-log"
			_, hooks := measureRender(t, z, "", openShift)
			for _, volume := range []string{VolumeMedia, VolumeBackups, VolumeKafka} {
				job := measureJobOf(t, hooks, volume)
				require.NotNil(t, job, volume)
				pod := job.Spec.Template.Spec
				require.Len(t, pod.Volumes, 2)
				assert.True(t, pod.Volumes[0].PersistentVolumeClaim.ReadOnly, "%s: read-only at the claim", volume)
				require.Len(t, pod.Containers, 1)
				c := pod.Containers[0]
				assert.Equal(t, MeasureContainer, c.Name)
				assert.Equal(t, "postgres:16-alpine", c.Image)
				assert.True(t, c.VolumeMounts[0].ReadOnly, "%s: read-only where it is mounted", volume)
				assert.Equal(t, "/measure", c.VolumeMounts[0].MountPath)
				assert.Equal(t, corev1.TerminationMessageReadFile, c.TerminationMessagePolicy)
				assert.Nil(t, pod.SecurityContext.FSGroup, "%s: no fsGroup, so nothing on the volume changes owner", volume)
				assert.True(t, *pod.SecurityContext.RunAsNonRoot)
				if openShift {
					assert.Nil(t, pod.SecurityContext.RunAsUser, "%s: the SCC gives the user", volume)
				} else {
					assert.Equal(t, int64(65532), *pod.SecurityContext.RunAsUser, volume)
				}
				assert.False(t, *pod.AutomountServiceAccountToken)
				assert.Equal(t, int32(0), *job.Spec.BackoffLimit)
				assert.Equal(t, int64(600), *job.Spec.ActiveDeadlineSeconds)
				assert.Equal(t, "/measure", envOf(c)["MEASURE_DIR"].Value)
			}
		}
	})

	t.Run("R.C7 the library's asks the bundled Postgres, for its volume where it has a claim", func(t *testing.T) {
		_, hooks := measureRender(t, measured(), "postgres-data", false)
		env := envOf(measureJobOf(t, hooks, VolumeMedia).Spec.Template.Spec.Containers[0])
		assert.Equal(t, "postgres", env["PGHOST"].Value)
		assert.Equal(t, "zaentrum-db", env["PGPASSWORD"].ValueFrom.SecretKeyRef.Name)
		for key, name := range map[string]string{"DB_CHINO": "chino", "DB_KATALOG": "katalog", "DB_KEYCLOAK": "keycloak", "DB_PORTAL": "portal"} {
			assert.Equal(t, name, env[key].Value, "each database by its key, whole")
		}
		assert.NotContains(t, env, "DATABASES", "no list of names to split")
		assert.Equal(t, "true", env["POSTGRES_VOLUME"].Value)
		for _, volume := range []string{VolumeBackups, VolumeKafka} {
			if job := measureJobOf(t, hooks, volume); job != nil {
				assert.NotContains(t, envOf(job.Spec.Template.Spec.Containers[0]), "PGPASSWORD", "%s holds no database password", volume)
			}
		}

		z := measured()
		z.Spec.Databases.Katalog = "katalog beta"
		_, hooks = measureRender(t, z, "", false)
		assert.Equal(t, "katalog beta", envOf(measureJobOf(t, hooks, VolumeMedia).Spec.Template.Spec.Containers[0])["DB_KATALOG"].Value,
			"a name with a space stays one value")

		_, hooks = measureRender(t, measured(), "emptyDir", false)
		env = envOf(measureJobOf(t, hooks, VolumeMedia).Spec.Template.Spec.Containers[0])
		assert.Contains(t, env, "DB_CHINO", "its databases are measured")
		assert.NotContains(t, env, "POSTGRES_VOLUME", "a Postgres without a claim of its own has no volume listed")

		z = measured()
		z.Spec.Databases.Mode = "external"
		z.Spec.Databases.External.Host = "postgres.example.org"
		platform, hooks := measureRender(t, z, "", false)
		env = envOf(measureJobOf(t, hooks, VolumeMedia).Spec.Template.Spec.Containers[0])
		assert.NotContains(t, env, "PGHOST", "with external databases nothing is asked")
		assert.Nil(t, measureJobOf(t, hooks, VolumeBackups))
		assert.Nil(t, find(t, platform, "ConfigMap", BackupRecordName))
	})

	t.Run("R.C7 the backups' reads which platform it is from the ConfigMap zaentrum-backup", func(t *testing.T) {
		z := measured()
		z.Spec.Version = "v0.5.0"
		platform, hooks := measureRender(t, z, "", false)
		env := envOf(measureJobOf(t, hooks, VolumeBackups).Spec.Template.Spec.Containers[0])
		assert.Equal(t, "true", env["SETS"].Value)
		ref := env["PLATFORM"].ValueFrom.ConfigMapKeyRef
		require.NotNil(t, ref)
		assert.Equal(t, BackupRecordName, ref.Name)
		assert.Equal(t, "platform", ref.Key)
		assert.True(t, *ref.Optional, "without it, nothing says who made a set")

		cm := find(t, platform, "ConfigMap", BackupRecordName)
		require.NotNil(t, cm, "applied with the platform")
		raw, _, _ := unstructured.NestedString(cm.Object, "data", "platform")
		var p map[string]string
		require.NoError(t, json.Unmarshal([]byte(raw), &p))
		assert.Equal(t, map[string]string{"name": "zaentrum", "uid": platformUID, "version": "v0.5.0"}, p,
			"the resource's name and uid, and the version that runs")
	})
}
