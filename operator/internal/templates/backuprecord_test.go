package templates

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The ConfigMap zaentrum-backup holds the record each set's backup.json is
// written from: platform (the resource's name and uid, the version that runs,
// the operator's), the resource's spec, the Secrets' names and the addons; and
// the backup's Job reads it, mounted where it may be missing.
func TestRC10TheConfigMapHoldsWhatMadeTheSets(t *testing.T) {
	z := measured()
	z.Spec.Version = "v0.5.0"
	v := NewValues(z)
	v.Record = BackupRecord{
		Operator: "v0.5.1",
		Resource: `{"hostname":"media.example.org","version":"v0.5.0"}`,
		Secrets:  []string{"katalog-tmdb", "zaentrum-tls"},
		Addons:   []BackupAddon{{Name: "tv-guide", Chart: "oci://registry.example.org/charts/tv-guide", Version: "1.2.0"}},
	}
	objs, err := Render(v)
	require.NoError(t, err)
	platform, _ := SplitHooks(objs)
	cm := find(t, platform, "ConfigMap", BackupRecordName)
	require.NotNil(t, cm, "applied with the platform")
	data, _, _ := unstructured.NestedStringMap(cm.Object, "data")

	var p map[string]string
	require.NoError(t, json.Unmarshal([]byte(data["platform"]), &p))
	assert.Equal(t, map[string]string{"name": "zaentrum", "uid": platformUID, "version": "v0.5.0", "operator": "v0.5.1"}, p)
	assert.JSONEq(t, `{"hostname":"media.example.org","version":"v0.5.0"}`, data["resource"], "the spec as the operator read it")
	assert.JSONEq(t, `["katalog-tmdb","zaentrum-tls"]`, data["secrets"])
	assert.JSONEq(t, `[{"name":"tv-guide","chart":"oci://registry.example.org/charts/tv-guide","version":"1.2.0"}]`, data["addons"])

	cron := backupCron(t, platform)
	require.NotNil(t, cron)
	pod := cron.Spec.JobTemplate.Spec.Template.Spec
	var mounted bool
	for _, vol := range pod.Volumes {
		if vol.Name == "record" {
			require.NotNil(t, vol.ConfigMap)
			assert.Equal(t, BackupRecordName, vol.ConfigMap.Name)
			require.NotNil(t, vol.ConfigMap.Optional)
			assert.True(t, *vol.ConfigMap.Optional, "where it is missing, the set is made without backup.json")
			mounted = true
		}
	}
	assert.True(t, mounted, "the record is a volume of the backup's pod")
	c := pod.Containers[0]
	assert.Equal(t, "/record", envOf(c)["RECORD_DIR"].Value)
	var at string
	for _, m := range c.VolumeMounts {
		if m.Name == "record" {
			at = m.MountPath
			assert.True(t, m.ReadOnly)
		}
	}
	assert.Equal(t, "/record", at)

	t.Run("R.C10 without a spec from the operator, the CR's own is written", func(t *testing.T) {
		objs, err := Render(NewValues(z))
		require.NoError(t, err)
		platform, _ := SplitHooks(objs)
		data, _, _ := unstructured.NestedStringMap(find(t, platform, "ConfigMap", BackupRecordName).Object, "data")
		var spec map[string]any
		require.NoError(t, json.Unmarshal([]byte(data["resource"]), &spec))
		assert.Equal(t, "v0.5.0", spec["version"])
		assert.JSONEq(t, `[]`, data["secrets"])
		assert.JSONEq(t, `[]`, data["addons"])
	})
}

// A URL in the record is written without its user, its password and its query:
// every part between commas that has a scheme; the rest stays as it is.
func TestRC10AURLIsWrittenBare(t *testing.T) {
	for in, want := range map[string]string{
		"https://someone:pa55@id.example.org/realms/home?prompt=login#top":    "https://id.example.org/realms/home#top",
		"oci://someone@registry.example.org/charts/tv-guide":                  "oci://registry.example.org/charts/tv-guide",
		"oci://registry.example.org/charts/tv-guide?ticket=abc":               "oci://registry.example.org/charts/tv-guide",
		"https://charts.example.org/arrivals-0.3.0.tgz?":                      "https://charts.example.org/arrivals-0.3.0.tgz",
		"SASL_SSL://someone:pa55@k1.example.org:9093?x=1,k2.example.org:9093": "SASL_SSL://k1.example.org:9093,k2.example.org:9093",
		"https://%zz:pa55@odd.example.org/path":                               "https://odd.example.org/path",
		"socks5h://proxy.example.org:1080":                                    "socks5h://proxy.example.org:1080",
		"https://id.example.org/a@b/c":                                        "https://id.example.org/a@b/c",
		"zaentrum@example.org":                                                "zaentrum@example.org",
		"media.example.org":                                                   "media.example.org",
		"source,720p":                                                         "source,720p",
	} {
		assert.Equal(t, want, bareURL(in), in)
	}
}
