package addon

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"helm.sh/helm/v3/pkg/chart"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func containerEnv(t *testing.T, dep *unstructured.Unstructured) map[string]string {
	t.Helper()
	containers := containersOf(dep)
	require.NotEmpty(t, containers)
	env := map[string]string{}
	list, _ := containers[0]["env"].([]interface{})
	for _, e := range list {
		m := e.(map[string]interface{})
		v, _ := m["value"].(string)
		env[m["name"].(string)] = v
	}
	return env
}

// The example chart renders its worker Deployment, Service and Secret into the
// addon's namespace; test hooks and notes are not objects.
func TestRenderExampleChart(t *testing.T) {
	out := renderExample(t, map[string]interface{}{
		"auth": map[string]interface{}{"token": "t0ken"},
		// A user cannot override the reserved block.
		"zaentrum": map[string]interface{}{"issuer": "https://elsewhere.example.org"},
	})

	var keys []string
	for _, o := range out.Objects {
		keys = append(keys, Key(o))
	}
	assert.Equal(t, []string{"Secret/worker", "Deployment/worker", "Service/worker"}, keys,
		"files in name order; the helm test Pod and NOTES.txt are skipped")

	dep := findObject(out.Objects, "Deployment", "worker")
	env := containerEnv(t, dep)
	assert.Equal(t, "https://sso.example.org/realms/x", env["OIDC_ISSUER"], "platform issuer wins")
	assert.Equal(t, "broker.events.svc:9093", env["EVENT_BROKERS"])
	assert.Equal(t, "hello", env["GREETING"], "chart default")
	pull, _, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "imagePullSecrets")
	assert.Equal(t, []interface{}{map[string]interface{}{"name": "registry-pull"}}, pull)

	assert.Empty(t, Violations(out.Objects, GuardInput{Namespace: testNamespace, MediaClaim: "media", Primary: "worker"}),
		"the example chart passes every guardrail")
	assert.Len(t, out.Checksum, 64)
}

func TestRenderChecksumFollowsValues(t *testing.T) {
	a := renderExample(t, nil)
	b := renderExample(t, nil)
	assert.Equal(t, a.Checksum, b.Checksum, "same values, same checksum")
	c := renderExample(t, map[string]interface{}{"auth": map[string]interface{}{"token": "rotated"}, "greeting": "hi"})
	assert.NotEqual(t, a.Checksum, c.Checksum)
}

// A missing required secret input is a values error, and nothing renders.
func TestRenderSchemaErrors(t *testing.T) {
	vals := LayerValues(map[string]string{"auth.signingKey": "c2lnbmluZw=="}, map[string]interface{}{"greeting": "howdy"}, platformValues())
	out, errs := Render(RenderInput{Chart: exampleChart(t), Name: "example", Namespace: testNamespace, Values: vals})
	assert.Nil(t, out)
	assert.Contains(t, errs, "auth: token is required")
	assert.Contains(t, fmt.Sprint(errs), "greeting")
}

func TestRenderTemplateError(t *testing.T) {
	chrt := &chart.Chart{
		Metadata: &chart.Metadata{APIVersion: "v2", Name: "broken", Version: "0.1.0"},
		Templates: []*chart.File{{Name: "templates/cm.yaml", Data: []byte(
			"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ required \"name is required\" .Values.name }}\n")}},
	}
	out, errs := Render(RenderInput{Chart: chrt, Name: "broken", Namespace: testNamespace, Values: map[string]interface{}{}})
	assert.Nil(t, out)
	require.Len(t, errs, 1)
	assert.Contains(t, errs[0], "name is required")
}

func TestSchemaErrorsPerChart(t *testing.T) {
	err := errors.New("example:\n- (root): token is required\n- replicas: Invalid type\nsub:\n- port: Must be greater than 0\n")
	assert.Equal(t, []string{
		"(root): token is required",
		"replicas: Invalid type",
		"sub: port: Must be greater than 0",
	}, schemaErrors("example", err))
}

func TestCheckConventions(t *testing.T) {
	assert.Empty(t, CheckConventions(exampleChart(t)))

	chrt := exampleChart(t)
	chrt.Metadata.APIVersion = "v1"
	chrt.Metadata.Type = "library"
	chrt.Metadata.Annotations = nil
	chrt.Files = append(chrt.Files, &chart.File{Name: "crds/things.yaml", Data: []byte("kind: CustomResourceDefinition")})
	assert.Equal(t, []string{
		"Chart.yaml: apiVersion v1 not supported (v2 required)",
		"Chart.yaml: type library cannot be installed (application required)",
		`Chart.yaml: annotation zaentrum.io/addon: "true" is required`,
		"Chart.yaml: annotation zaentrum.io/primary is required",
		"example/crds/things.yaml: CustomResourceDefinition not allowed",
	}, CheckConventions(chrt))
}

func TestLoadChart(t *testing.T) {
	chrt, err := LoadChart(exampleArchive(t))
	require.NoError(t, err)
	assert.Equal(t, "example", chrt.Name())
	assert.Contains(t, string(chrt.Schema), "x-zaentrum-generate")

	// A tiny archive that unpacks past the limit is refused before Helm reads it.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	big := make([]byte, maxUnpackedSize+1)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "bomb/Chart.yaml", Mode: 0o644, Size: int64(len(big))}))
	_, err = tw.Write(big)
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	require.Less(t, buf.Len(), MaxArchiveSize)
	_, err = LoadChart(buf.Bytes())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unpacks to more than 20 MiB")
}
