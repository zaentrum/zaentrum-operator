package controller

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
	"github.com/zaentrum/zaentrum-operator/operator/internal/templates"
)

var tlsNow = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

// certPEM is a self-signed certificate for names, valid until notAfter, and its key.
func certPEM(t *testing.T, notAfter time.Time, names ...string) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: notAfter.Add(-90 * 24 * time.Hour), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	kder, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})
}

func tlsSecretOf(name string, crt, key []byte) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: verifyNS}, Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{corev1.TLSCertKey: crt, corev1.TLSPrivateKeyKey: key}}
}

// tlsReconciler is a reconciler on a fake API server whose REST mapper serves
// cert-manager's Certificate when certManager says so.
func tlsReconciler(t *testing.T, certManager bool, objs ...client.Object) *ZaentrumReconciler {
	t.Helper()
	s := selfScheme(t)
	mapper := meta.NewDefaultRESTMapper(nil)
	if certManager {
		mapper.Add(schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"}, meta.RESTScopeNamespace)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithRESTMapper(mapper).WithObjects(objs...).Build()
	return &ZaentrumReconciler{Client: c, Scheme: s, Now: func() time.Time { return tlsNow }}
}

// The TLS condition says how the platform's hosts are served: the
// platform's certificate, valid and for them; the OpenShift router; a proxy in
// front — and what is wrong where they cannot be: plain http, Routes serving
// https for an http issuer, no Secret, a certificate for another host, an
// expired one, no key, an issuer named and no cert-manager to issue.
func TestTheTLSConditionSaysHowTheHostsAreServed(t *testing.T) {
	yes := true
	good, goodKey := certPEM(t, tlsNow.Add(60*24*time.Hour), "media.example.org", "watch.example.org")
	other, otherKey := certPEM(t, tlsNow.Add(60*24*time.Hour), "elsewhere.example.org")
	old, oldKey := certPEM(t, tlsNow.Add(-24*time.Hour), "media.example.org")

	cr := func(mut func(z *zaentrumv1alpha1.Zaentrum)) *zaentrumv1alpha1.Zaentrum {
		z := verifyCR()
		z.Spec.Identity.IssuerScheme = "http"
		mut(z)
		return z
	}
	withTLS := func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.TLS = &zaentrumv1alpha1.TLSSpec{} }
	for name, c := range map[string]struct {
		z           *zaentrumv1alpha1.Zaentrum
		secret      *corev1.Secret
		certManager bool
		status      metav1.ConditionStatus
		reason      string
		says        string
	}{
		"plain http": {z: cr(func(*zaentrumv1alpha1.Zaentrum) {}), status: metav1.ConditionFalse, reason: "PlainHTTP", says: "phones and TVs not at all"},
		"a proxy in front": {z: cr(func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.Identity.IssuerScheme = "https" }),
			status: metav1.ConditionTrue, reason: "TerminatedInFront"},
		"the OpenShift router": {z: cr(func(z *zaentrumv1alpha1.Zaentrum) {
			z.Spec.Routing.ProvisionRoutes = &yes
			z.Spec.Identity.IssuerScheme = "https"
		}), status: metav1.ConditionTrue, reason: "Router"},
		"Routes and an http issuer": {z: cr(func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.Routing.ProvisionRoutes = &yes }),
			status: metav1.ConditionFalse, reason: "IssuerSchemeHTTP", says: "identity.issuerScheme is http"},
		// As beta: an external issuer named outright, issuerScheme left at its
		// default, which then derives nothing.
		"Routes and an issuer of its own": {z: cr(func(z *zaentrumv1alpha1.Zaentrum) {
			z.Spec.Routing.ProvisionRoutes = &yes
			z.Spec.Identity.Mode = zaentrumv1alpha1.IdentityExternal
			z.Spec.Identity.Issuer = "https://sso.example.org/realms/example"
		}), status: metav1.ConditionTrue, reason: "Router"},
		"no Secret": {z: cr(withTLS), status: metav1.ConditionFalse, reason: "SecretMissing", says: "no Secret zaentrum-tls"},
		"the platform's certificate": {z: cr(withTLS), secret: tlsSecretOf("zaentrum-tls", good, goodKey),
			status: metav1.ConditionTrue, reason: "Certificate", says: "media.example.org served over https with the certificate in Secret zaentrum-tls, valid until 2026-12-03"},
		"and the chino host too": {z: cr(func(z *zaentrumv1alpha1.Zaentrum) {
			withTLS(z)
			z.Spec.Routing.Mode = "subdomains"
			z.Spec.Routing.Hosts.Chino = "watch.example.org"
		}), secret: tlsSecretOf("zaentrum-tls", good, goodKey), status: metav1.ConditionTrue, reason: "Certificate", says: "media.example.org, watch.example.org"},
		"a certificate for another host": {z: cr(withTLS), secret: tlsSecretOf("zaentrum-tls", other, otherKey),
			status: metav1.ConditionFalse, reason: "WrongHost", says: "is for elsewhere.example.org, not media.example.org"},
		"a chino host it is not for": {z: cr(func(z *zaentrumv1alpha1.Zaentrum) {
			withTLS(z)
			z.Spec.Routing.Mode = "subdomains"
			z.Spec.Routing.Hosts.Chino = "tv.example.org"
		}), secret: tlsSecretOf("zaentrum-tls", good, goodKey), status: metav1.ConditionFalse, reason: "WrongHost", says: "not tv.example.org"},
		"an expired certificate": {z: cr(withTLS), secret: tlsSecretOf("zaentrum-tls", old, oldKey),
			status: metav1.ConditionFalse, reason: "Expired", says: "expired on 2026-10-03"},
		"no key":         {z: cr(withTLS), secret: tlsSecretOf("zaentrum-tls", good, nil), status: metav1.ConditionFalse, reason: "BadCertificate", says: "no tls.key"},
		"no certificate": {z: cr(withTLS), secret: tlsSecretOf("zaentrum-tls", []byte("not a pem"), goodKey), status: metav1.ConditionFalse, reason: "BadCertificate"},
		"an issuer and no cert-manager": {z: cr(func(z *zaentrumv1alpha1.Zaentrum) {
			z.Spec.TLS = &zaentrumv1alpha1.TLSSpec{IssuerRef: &zaentrumv1alpha1.TLSIssuerRef{Name: "letsencrypt"}}
		}), status: metav1.ConditionFalse, reason: "NoCertManager", says: "serves no cert-manager API"},
		"cert-manager issuing": {z: cr(func(z *zaentrumv1alpha1.Zaentrum) {
			z.Spec.TLS = &zaentrumv1alpha1.TLSSpec{IssuerRef: &zaentrumv1alpha1.TLSIssuerRef{Name: "letsencrypt"}}
		}), certManager: true, status: metav1.ConditionUnknown, reason: "Issuing", says: "cert-manager issues the certificate for media.example.org into Secret zaentrum-tls"},
		"cert-manager's certificate": {z: cr(func(z *zaentrumv1alpha1.Zaentrum) {
			z.Spec.TLS = &zaentrumv1alpha1.TLSSpec{SecretName: "media-cert", IssuerRef: &zaentrumv1alpha1.TLSIssuerRef{Name: "letsencrypt", Kind: "ClusterIssuer"}}
		}), secret: tlsSecretOf("media-cert", good, goodKey), certManager: true, status: metav1.ConditionTrue, reason: "Certificate",
			says: "which cert-manager keeps from ClusterIssuer letsencrypt"},
	} {
		t.Run(name, func(t *testing.T) {
			objs := []client.Object{}
			if c.secret != nil {
				objs = append(objs, c.secret)
			}
			r := tlsReconciler(t, c.certManager, objs...)
			sec, issuing, err := r.platformCertificate(context.Background(), c.z)
			require.NoError(t, err)
			r.reportTLS(context.Background(), c.z, sec, issuing)
			got := meta.FindStatusCondition(c.z.Status.Conditions, condTypeTLS)
			require.NotNil(t, got)
			assert.Equal(t, c.status, got.Status, got.Message)
			assert.Equal(t, c.reason, got.Reason, got.Message)
			assert.Contains(t, got.Message, c.says)
		})
	}
}

// The render gets what the Routes need — the certificate inline, read from
// the Secret — only where there are Routes, and cert-manager only where the
// cluster serves it and an issuer is named.
func TestTheCertificateReachesTheRender(t *testing.T) {
	yes := true
	crt, key := certPEM(t, tlsNow.Add(time.Hour), "media.example.org")
	sec := tlsSecretOf("zaentrum-tls", crt, key)
	sec.Data["ca.crt"] = []byte("the chain")

	z := verifyCR()
	z.Spec.TLS = &zaentrumv1alpha1.TLSSpec{}
	r := tlsReconciler(t, true, sec)
	got, issuing, err := r.platformCertificate(context.Background(), z)
	require.NoError(t, err)
	assert.False(t, issuing, "no issuer named")
	v := templates.NewValues(z)
	certificateValues(&v, z, got, issuing)
	assert.Empty(t, v.TLSCertificate, "an Ingress names the Secret; nothing inline")

	z.Spec.Routing.ProvisionRoutes = &yes
	v = templates.NewValues(z)
	certificateValues(&v, z, got, issuing)
	assert.Equal(t, string(crt), v.TLSCertificate)
	assert.Equal(t, string(key), v.TLSKey)
	assert.Equal(t, "the chain", v.TLSCA)

	z.Spec.TLS.IssuerRef = &zaentrumv1alpha1.TLSIssuerRef{Name: "letsencrypt"}
	_, issuing, err = r.platformCertificate(context.Background(), z)
	require.NoError(t, err)
	assert.True(t, issuing, "cert-manager is served and an issuer named")
	_, issuing, err = tlsReconciler(t, false, sec).platformCertificate(context.Background(), z)
	require.NoError(t, err)
	assert.False(t, issuing, "no cert-manager")

	got, issuing, err = r.platformCertificate(context.Background(), verifyCR())
	require.NoError(t, err)
	assert.Nil(t, got, "no spec.tls, no Secret read")
	assert.False(t, issuing)
}
