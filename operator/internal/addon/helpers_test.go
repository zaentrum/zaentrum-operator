package addon

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
)

const testNamespace = "zaentrum-beta"

// exampleChart loads the neutral example addon chart from testdata.
func exampleChart(t *testing.T) *chart.Chart {
	t.Helper()
	chrt, err := loader.LoadDir("testdata/example")
	require.NoError(t, err)
	return chrt
}

// exampleArchive packages the example chart the way `helm package` does.
func exampleArchive(t *testing.T) []byte {
	t.Helper()
	path, err := chartutil.Save(exampleChart(t), t.TempDir())
	require.NoError(t, err)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return data
}

// platformValues is a representative reserved zaentrum block.
func platformValues() map[string]interface{} {
	return map[string]interface{}{
		"namespace":         testNamespace,
		"hostname":          "zaentrum.beta.example.org",
		"issuer":            "https://sso.example.org/realms/x",
		"issuerHostAliasIP": "",
		"imagePullSecrets":  []interface{}{"registry-pull"},
		"partOf":            "zaentrum-beta-addons",
		"events": map[string]interface{}{
			"brokers":     "broker.events.svc:9093",
			"topicPrefix": "zaentrum-beta.",
			"tlsSecret":   "kafka-mtls",
		},
		"media": map[string]interface{}{"claimName": "media"},
	}
}

// renderExample renders the example chart with a token input, a generated
// signing key and the platform block.
func renderExample(t *testing.T, user map[string]interface{}) *Rendered {
	t.Helper()
	if user == nil {
		user = map[string]interface{}{"auth": map[string]interface{}{"token": "t0ken"}}
	}
	vals := LayerValues(map[string]string{"auth.signingKey": "c2lnbmluZw=="}, user, platformValues())
	out, errs := Render(RenderInput{Chart: exampleChart(t), Name: "example", Namespace: testNamespace, Values: vals})
	require.Empty(t, errs)
	require.NotNil(t, out)
	return out
}

// objects decodes a multi-document YAML manifest.
func objects(t *testing.T, manifest string) []*unstructured.Unstructured {
	t.Helper()
	var out []*unstructured.Unstructured
	dec := yaml.NewYAMLOrJSONDecoder(strings.NewReader(manifest), 4096)
	for {
		raw := map[string]interface{}{}
		if err := dec.Decode(&raw); err != nil {
			break
		}
		if len(raw) > 0 {
			out = append(out, &unstructured.Unstructured{Object: raw})
		}
	}
	return out
}

func findObject(objs []*unstructured.Unstructured, kind, name string) *unstructured.Unstructured {
	for _, o := range objs {
		if o.GetKind() == kind && o.GetName() == name {
			return o
		}
	}
	return nil
}
