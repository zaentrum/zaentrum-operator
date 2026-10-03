package templates

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// cluster answers the chart's lookup calls from a fake API server holding
// objs, the way `helm install` and `helm upgrade` answer them from a real one.
type cluster struct {
	client *dynamicfake.FakeDynamicClient
}

func newCluster(t *testing.T, objs ...runtime.Object) cluster {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	return cluster{dynamicfake.NewSimpleDynamicClient(s, objs...)}
}

func (c cluster) GetClientFor(apiVersion, kind string) (dynamic.NamespaceableResourceInterface, bool, error) {
	if apiVersion != "v1" || kind != "Secret" {
		return nil, false, fmt.Errorf("the chart looks up %s %s; only Secrets are expected", apiVersion, kind)
	}
	return c.client.Resource(schema.GroupVersionResource{Version: "v1", Resource: "secrets"}), true, nil
}

// secretData is a rendered Secret's stringData.
func secretData(t *testing.T, objs []*unstructured.Unstructured, name string) map[string]string {
	t.Helper()
	sec := find(t, objs, "Secret", name)
	require.NotNil(t, sec, "Secret %s", name)
	data, _, err := unstructured.NestedStringMap(sec.Object, "stringData")
	require.NoError(t, err)
	return data
}

var chartSecrets = []string{"zaentrum-db", "zaentrum-stream-signing", "zaentrum-keycloak", "zaentrum-keycloak-admin", "zaentrum-demo-user"}

// The values every install of an earlier chart shared.
var publishedValues = []string{"zaentrum-dev-change-me", "dev-change-me", "zaentrum-manager-dev-change-me",
	"ZGV2LW9ubHktY2hhbmdlLW1lLXN0cmVhbS1zaWduaW5nLWtleQ=="}

// A plain Helm install makes its own Secrets: random, of the shape each reader
// expects, and the same for no two installs.
func TestChartSecretsAreRandom(t *testing.T) {
	first := helmRender(t, nil)
	second := helmRender(t, nil)
	for _, name := range chartSecrets {
		a, b := secretData(t, first, name), secretData(t, second, name)
		for k, v := range a {
			if (name == "zaentrum-db" && k == "user") || (name == "zaentrum-keycloak-admin" && k == "username") {
				continue
			}
			assert.NotEqual(t, v, b[k], "%s/%s is the same for two installs", name, k)
			assert.NotEmpty(t, v)
		}
	}
	db := secretData(t, first, "zaentrum-db")
	assert.Equal(t, "zaentrum", db["user"])
	assert.Len(t, db["password"], 32)
	key, err := base64.StdEncoding.DecodeString(secretData(t, first, "zaentrum-stream-signing")["key"])
	require.NoError(t, err, "the services read the signing key as base64")
	assert.Len(t, key, 32)
	admin := secretData(t, first, "zaentrum-keycloak-admin")
	assert.Equal(t, "admin", admin["username"])
	assert.Len(t, admin["password"], 32)
	assert.Len(t, admin["realm-admin-password"], 24)
	assert.Len(t, secretData(t, first, "zaentrum-demo-user")["password"], 32)

	all := fmt.Sprintf("%v", first)
	for _, p := range publishedValues {
		assert.NotContains(t, all, p, "a value every earlier install shared")
	}
}

// An upgrade reads every value back from the cluster, so it rotates nothing,
// and makes only a key that is missing.
func TestChartSecretsSurviveAnUpgrade(t *testing.T) {
	have := func(name string, data map[string]string) runtime.Object {
		sec := &corev1.Secret{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "zaentrum"},
			Data:       map[string][]byte{},
		}
		for k, v := range data {
			sec.Data[k] = []byte(v)
		}
		return sec
	}
	live := newCluster(t,
		have("zaentrum-db", map[string]string{"user": "zaentrum", "password": "Kept0db0password"}),
		have("zaentrum-stream-signing", map[string]string{"key": "a2VwdC1zaWduaW5nLWtleS0xMjM0NQ=="}),
		have("zaentrum-keycloak", map[string]string{"client-secret": "Kept0client0secret"}),
		have("zaentrum-keycloak-admin", map[string]string{"username": "root", "password": "Kept0admin0password"}),
	)
	objs, err := render(map[string]interface{}{}, "zaentrum", false, live)
	require.NoError(t, err)

	assert.Equal(t, map[string]string{"user": "zaentrum", "password": "Kept0db0password"}, secretData(t, objs, "zaentrum-db"))
	assert.Equal(t, map[string]string{"key": "a2VwdC1zaWduaW5nLWtleS0xMjM0NQ=="}, secretData(t, objs, "zaentrum-stream-signing"))
	assert.Equal(t, map[string]string{"client-secret": "Kept0client0secret"}, secretData(t, objs, "zaentrum-keycloak"))
	admin := secretData(t, objs, "zaentrum-keycloak-admin")
	assert.Equal(t, "root", admin["username"], "a value that is there is kept, whatever it is")
	assert.Equal(t, "Kept0admin0password", admin["password"])
	assert.Len(t, admin["realm-admin-password"], 24, "the missing key is made")
	assert.Len(t, secretData(t, objs, "zaentrum-demo-user")["password"], 32, "the missing Secret is made")
}

// Provided secrets, and the operator's render, carry none of them; external
// identity needs no Keycloak Secret.
func TestChartSecretsOnlyWhereTheChartMakesThem(t *testing.T) {
	objs := helmRender(t, map[string]interface{}{"secrets": map[string]interface{}{"external": true}})
	assert.Zero(t, count(objs, "Secret"), "secrets.external")

	objs = helmRender(t, map[string]interface{}{"identity": map[string]interface{}{"mode": "external", "issuer": "https://sso.example.org/realms/x"}})
	var names []string
	for _, o := range objs {
		if o.GetKind() == "Secret" && !IsTestHook(o) {
			names = append(names, o.GetName())
		}
	}
	assert.Equal(t, []string{"zaentrum-db", "zaentrum-stream-signing"}, names)

	for _, z := range []string{"self-host", "demo"} {
		cr := base("zaentrum")
		if z == "demo" {
			cr = demoCR("zaentrum-demo")
		}
		objs, err := Render(NewValues(cr))
		require.NoError(t, err)
		assert.Zero(t, count(objs, "Secret"), "the operator's render (%s) carries no Secret", z)
	}
}

// helm install says how to read the first administrator's password, once —
// and the operator, which keeps status instead, never renders NOTES.txt.
func TestChartNotesSayWhereTheFirstPasswordIs(t *testing.T) {
	chrt, err := loadChart()
	require.NoError(t, err)
	var notes string
	for _, f := range chrt.Templates {
		if strings.HasSuffix(f.Name, "NOTES.txt") {
			notes = string(f.Data)
		}
	}
	require.NotEmpty(t, notes)
	assert.Contains(t, notes, "get secret zaentrum-keycloak-admin -o jsonpath='{.data.realm-admin-password}' | base64 -d")
	objs, err := Render(NewValues(base("zaentrum")))
	require.NoError(t, err)
	for _, o := range objs {
		assert.NotEmpty(t, o.GetKind(), "NOTES.txt is text, not an object")
	}
}
