package addon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"helm.sh/helm/v3/pkg/chart"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
)

func secretRef(name, key, target string) zaentrumv1alpha1.AddonValuesReference {
	return zaentrumv1alpha1.AddonValuesReference{Kind: "Secret", Name: name, ValuesKey: key, TargetPath: target}
}

// layersChart renders every top-level value into a ConfigMap, so a test can
// see which layer won each key. Its own defaults are the lowest layer.
func layersChart() *chart.Chart {
	return &chart.Chart{
		Metadata: &chart.Metadata{APIVersion: "v2", Name: "layers", Version: "0.1.0"},
		Values: map[string]interface{}{
			"a": "chart", "b": "chart", "c": "chart", "d": "chart", "e": "chart",
			"zaentrum": map[string]interface{}{"issuer": "chart", "fromChart": "chart"},
		},
		Templates: []*chart.File{{Name: "templates/layers.yaml", Data: []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: layers
data:
  a: {{ .Values.a | quote }}
  b: {{ .Values.b | quote }}
  c: {{ .Values.c | quote }}
  d: {{ .Values.d | quote }}
  e: {{ .Values.e | quote }}
  issuer: {{ .Values.zaentrum.issuer | quote }}
  fromChart: {{ .Values.zaentrum.fromChart | default "" | quote }}
`)}},
	}
}

// chart defaults < generated < spec.values < valuesFrom (in order) < zaentrum.
func TestValuePrecedence(t *testing.T) {
	generated := map[string]string{"b": "generated", "c": "generated", "d": "generated", "e": "generated"}
	values := &apiextensionsv1.JSON{Raw: []byte(`{"c":"values","d":"values","e":"values","zaentrum":{"issuer":"user"}}`)}
	from := []ValuesSource{
		{Ref: secretRef("doc", "", ""), Found: true, Data: map[string][]byte{"values.yaml": []byte("d: secret\ne: secret\n")}},
		{
			Ref:   zaentrumv1alpha1.AddonValuesReference{Kind: "ConfigMap", Name: "one", ValuesKey: "e", TargetPath: "e"},
			Found: true, Data: map[string][]byte{"e": []byte("configmap")},
		},
	}
	user, errs := UserValues(values, from)
	require.Empty(t, errs)
	vals := LayerValues(generated, user, map[string]interface{}{"issuer": "platform"})

	out, errs := Render(RenderInput{Chart: layersChart(), Name: "layers", Namespace: testNamespace, Values: vals})
	require.Empty(t, errs)
	require.Len(t, out.Objects, 1)
	data, _, err := unstructured.NestedStringMap(out.Objects[0].Object, "data")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"a": "chart", "b": "generated", "c": "values", "d": "secret", "e": "configmap",
		// The platform block replaces what a user AND the chart set there.
		"issuer": "platform", "fromChart": "",
	}, data)
}

func TestValuesFromTargetPath(t *testing.T) {
	from := []ValuesSource{
		// Creates the parents.
		{Ref: secretRef("s", "url", "database.url"), Found: true, Data: map[string][]byte{"url": []byte("postgres://db.example.org/x")}},
		// The raw string, never parsed as YAML.
		{Ref: secretRef("s", "doc", "raw"), Found: true, Data: map[string][]byte{"doc": []byte("a: 1")}},
		// Replaces a parent that is not a map.
		{Ref: secretRef("s", "v", "flat.child"), Found: true, Data: map[string][]byte{"v": []byte("x")}},
	}
	values := &apiextensionsv1.JSON{Raw: []byte(`{"database":{"pool":5},"flat":"scalar"}`)}
	user, errs := UserValues(values, from)
	require.Empty(t, errs)
	assert.Equal(t, map[string]interface{}{
		"database": map[string]interface{}{"pool": float64(5), "url": "postgres://db.example.org/x"},
		"raw":      "a: 1",
		"flat":     map[string]interface{}{"child": "x"},
	}, user)

	_, errs = UserValues(nil, []ValuesSource{{Ref: secretRef("s", "v", "a..b"), Found: true, Data: map[string][]byte{"v": []byte("x")}}})
	require.Len(t, errs, 1)
	assert.Contains(t, errs[0], "targetPath")
}

func TestValuesFromKeysAndOptional(t *testing.T) {
	doc := map[string][]byte{"values.yaml": []byte("greeting: hi\n"), "other.yaml": []byte("greeting: hey\n")}

	user, errs := UserValues(nil, []ValuesSource{{Ref: secretRef("s", "", ""), Found: true, Data: doc}})
	require.Empty(t, errs)
	assert.Equal(t, "hi", user["greeting"], "valuesKey defaults to values.yaml")

	user, errs = UserValues(nil, []ValuesSource{{Ref: secretRef("s", "other.yaml", ""), Found: true, Data: doc}})
	require.Empty(t, errs)
	assert.Equal(t, "hey", user["greeting"])

	for name, tc := range map[string]struct {
		src  ValuesSource
		want string
	}{
		"missing object": {ValuesSource{Ref: secretRef("gone", "", "")}, "valuesFrom Secret/gone: not found"},
		"missing key":    {ValuesSource{Ref: secretRef("s", "nope", ""), Found: true, Data: doc}, "key nope not found"},
		"not a document": {ValuesSource{Ref: secretRef("s", "", ""), Found: true, Data: map[string][]byte{"values.yaml": []byte("just words")}}, "not a YAML/JSON values document"},
	} {
		_, errs := UserValues(nil, []ValuesSource{tc.src})
		require.Len(t, errs, 1, name)
		assert.Contains(t, errs[0], tc.want, name)

		optional := tc.src
		optional.Ref.Optional = true
		_, errs = UserValues(nil, []ValuesSource{optional})
		if name == "not a document" {
			assert.Len(t, errs, 1, "optional tolerates absence, not a broken document")
		} else {
			assert.Empty(t, errs, "%s tolerated when optional", name)
		}
	}

	_, errs = UserValues(&apiextensionsv1.JSON{Raw: []byte(`["not","an","object"]`)}, nil)
	require.Len(t, errs, 1)
	assert.Contains(t, errs[0], "spec.values must be an object")
}

// Layering copies: rendering never writes back into the CR's values or the
// platform block shared across addons.
func TestLayerValuesCopies(t *testing.T) {
	user := map[string]interface{}{"nested": map[string]interface{}{"k": "v"}}
	platform := platformValues()
	vals := LayerValues(nil, user, platform)
	vals["nested"].(map[string]interface{})["k"] = "changed"
	vals[PlatformKey].(map[string]interface{})["issuer"] = "changed"
	assert.Equal(t, "v", user["nested"].(map[string]interface{})["k"])
	assert.Equal(t, "https://sso.example.org/realms/x", platform["issuer"])
}
