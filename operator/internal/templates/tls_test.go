package templates

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
)

const (
	testCert = "-----BEGIN CERTIFICATE-----\nMIIB-the-platform-cert\n-----END CERTIFICATE-----\n"
	testKey  = "-----BEGIN PRIVATE KEY-----\nMIIE-the-platform-key\n-----END PRIVATE KEY-----\n"
	testCA   = "-----BEGIN CERTIFICATE-----\nMIIB-an-intermediate\n-----END CERTIFICATE-----\n"
)

func typedIngress(t *testing.T, objs []*unstructured.Unstructured, name string) *networkingv1.Ingress {
	t.Helper()
	u := find(t, objs, "Ingress", name)
	require.NotNil(t, u, "no Ingress %s", name)
	var ing networkingv1.Ingress
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &ing))
	return &ing
}

// routeTLS is every Route's tls, by name.
func routeTLS(objs []*unstructured.Unstructured) map[string]map[string]interface{} {
	out := map[string]map[string]interface{}{}
	for _, o := range objs {
		if o.GetKind() == "Route" {
			tls, _, _ := unstructured.NestedMap(o.Object, "spec", "tls")
			out[o.GetName()] = tls
		}
	}
	return out
}

// Without spec.tls nothing changes: the Ingress has no tls, every Route is
// edge-terminated by the router's own certificate as before, and the scheme
// is identity.issuerScheme's.
func TestWithoutTLSTheHostsAreServedAsBefore(t *testing.T) {
	objs := renderCR(t, base("zaentrum"))
	assert.Empty(t, typedIngress(t, objs, "zaentrum").Spec.TLS)
	assert.Equal(t, "http://zaentrum.localhost/auth/realms/zaentrum", envValue(t, objs, "chino-api", "OIDC_ISSUER"))
	assert.Nil(t, find(t, objs, "Certificate", CertificateName))

	sub := demoCR("zaentrum-demo")
	sub.Spec.Routing.Mode = "subdomains"
	sub.Spec.Routing.Hosts.Chino = "chino.example.org"
	routes := routeTLS(renderCR(t, sub))
	require.NotEmpty(t, routes)
	for name, tls := range routes {
		policy := "Allow"
		if name == "chino-sub-root" || name == "chino-sub-api" || name == "chino-sub-auth-callback" {
			policy = "Redirect"
		}
		assert.Equal(t, map[string]interface{}{"termination": "edge", "insecureEdgeTerminationPolicy": policy}, tls, name)
	}
}

// A certificate of the platform's own: the Ingresses serve the Secret for
// their hosts, and every URL the platform derives is https — the issuer,
// Keycloak's hostname, the checks' public URL, the sign-in redirects — even
// with identity.issuerScheme at its default, http.
func TestTLSFromASecretServesTheHostsOverHTTPS(t *testing.T) {
	z := base("zaentrum")
	z.Spec.Hostname = "media.example.org"
	z.Spec.TLS = &zaentrumv1alpha1.TLSSpec{}
	z.Spec.Identity.IssuerScheme = "http"
	objs := renderCR(t, z)

	ing := typedIngress(t, objs, "zaentrum")
	require.Len(t, ing.Spec.TLS, 1)
	assert.Equal(t, networkingv1.IngressTLS{Hosts: []string{"media.example.org"}, SecretName: "zaentrum-tls"}, ing.Spec.TLS[0])
	assert.Equal(t, "https://media.example.org/auth/realms/zaentrum", envValue(t, objs, "chino-api", "OIDC_ISSUER"))
	cm := find(t, objs, "ConfigMap", "zaentrum-keycloak-config")
	kc, _, _ := unstructured.NestedString(cm.Object, "data", "KC_HOSTNAME")
	assert.Equal(t, "https://media.example.org/auth", kc)
	_, hooks := SplitHooks(objs)
	job := VerifyJob(hooks)
	require.NotNil(t, job)
	containers, _, _ := unstructured.NestedSlice(job.Object, "spec", "template", "spec", "containers")
	args, _, _ := unstructured.NestedStringSlice(containers[0].(map[string]interface{}), "args")
	assert.Contains(t, args, "https://media.example.org", "the checks meet the platform over https")
	assert.Equal(t, []string{"https://media.example.org/auth/callback"}, realmClients(t, objs)["chino-web"].RedirectURIs)
	assert.Nil(t, find(t, objs, "Certificate", CertificateName), "a Secret brought, nothing issued")

	z.Spec.TLS.SecretName = "media-cert"
	z.Spec.Routing.Mode = "subdomains"
	z.Spec.Routing.Hosts.Chino = "watch.example.org"
	objs = renderCR(t, z)
	assert.Equal(t, "media-cert", typedIngress(t, objs, "zaentrum").Spec.TLS[0].SecretName)
	sub := typedIngress(t, objs, "chino-subdomain")
	require.Len(t, sub.Spec.TLS, 1)
	assert.Equal(t, networkingv1.IngressTLS{Hosts: []string{"watch.example.org"}, SecretName: "media-cert"}, sub.Spec.TLS[0])
}

// On OpenShift the Routes, which cannot name a Secret, carry the certificate
// the operator read from it — each one, with its http policy as before —
// and the router's own until there is one.
func TestTLSOnTheRoutes(t *testing.T) {
	z := demoCR("zaentrum-demo")
	z.Spec.TLS = &zaentrumv1alpha1.TLSSpec{}
	v := NewValues(z)
	objs, err := Render(v)
	require.NoError(t, err)
	for name, tls := range routeTLS(objs) {
		assert.Equal(t, map[string]interface{}{"termination": "edge", "insecureEdgeTerminationPolicy": "Allow"}, tls,
			"%s: the router's certificate until the Secret is there", name)
	}

	v.TLSCertificate, v.TLSKey, v.TLSCA = testCert, testKey, testCA
	objs, err = Render(v)
	require.NoError(t, err)
	routes := routeTLS(objs)
	require.NotEmpty(t, routes)
	for name, tls := range routes {
		assert.Equal(t, map[string]interface{}{"termination": "edge", "insecureEdgeTerminationPolicy": "Allow",
			"certificate": testCert, "key": testKey, "caCertificate": testCA}, tls, name)
	}
	assert.Equal(t, "https://zaentrum.demo.nalet.cloud/auth/realms/zaentrum", envValue(t, objs, "chino-api", "OIDC_ISSUER")) // neutrality-guard:allow

	// helm reads the Secret itself.
	sec := &corev1.Secret{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: "zaentrum-tls", Namespace: "zaentrum"},
		Data:       map[string][]byte{"tls.crt": []byte(testCert), "tls.key": []byte(testKey)}}
	objs, err = render(map[string]interface{}{
		"tls":     map[string]interface{}{"enabled": true},
		"routing": map[string]interface{}{"provisionIngress": false, "provisionRoutes": true},
	}, "zaentrum", false, newCluster(t, sec))
	require.NoError(t, err)
	for name, tls := range routeTLS(objs) {
		assert.Equal(t, testCert, tls["certificate"], name)
		assert.Equal(t, testKey, tls["key"], name)
		assert.NotContains(t, tls, "caCertificate", name)
	}
}

// With an issuer named and cert-manager's API served, the chart asks
// cert-manager for the certificate of the platform's hosts into the Secret;
// where cert-manager is not, nothing is rendered for it.
func TestTLSFromCertManager(t *testing.T) {
	z := base("zaentrum")
	z.Spec.Hostname = "media.example.org"
	z.Spec.Routing.Mode = "subdomains"
	z.Spec.Routing.Hosts.Chino = "watch.example.org"
	z.Spec.TLS = &zaentrumv1alpha1.TLSSpec{IssuerRef: &zaentrumv1alpha1.TLSIssuerRef{Name: "letsencrypt"}}

	v := NewValues(z)
	objs, err := Render(v)
	require.NoError(t, err)
	assert.Nil(t, find(t, objs, "Certificate", CertificateName), "no cert-manager, no Certificate")

	v.CertManager = true
	objs, err = Render(v)
	require.NoError(t, err)
	cert := find(t, objs, "Certificate", CertificateName)
	require.NotNil(t, cert)
	assert.Equal(t, CertManagerAPI, cert.GetAPIVersion())
	spec, _, _ := unstructured.NestedMap(cert.Object, "spec")
	assert.Equal(t, map[string]interface{}{
		"secretName": "zaentrum-tls",
		"dnsNames":   []interface{}{"media.example.org", "watch.example.org"},
		"issuerRef":  map[string]interface{}{"name": "letsencrypt", "kind": "Issuer", "group": "cert-manager.io"},
	}, spec)
	assert.Equal(t, "zaentrum-tls", typedIngress(t, objs, "zaentrum").Spec.TLS[0].SecretName, "the Ingress serves what cert-manager writes")

	z.Spec.TLS.IssuerRef.Kind = "ClusterIssuer"
	z.Spec.TLS.SecretName = "media-cert"
	v = NewValues(z)
	v.CertManager = true
	objs, err = Render(v)
	require.NoError(t, err)
	kind, _, _ := unstructured.NestedString(find(t, objs, "Certificate", CertificateName).Object, "spec", "issuerRef", "kind")
	assert.Equal(t, "ClusterIssuer", kind)
	secret, _, _ := unstructured.NestedString(find(t, objs, "Certificate", CertificateName).Object, "spec", "secretName")
	assert.Equal(t, "media-cert", secret)
}
