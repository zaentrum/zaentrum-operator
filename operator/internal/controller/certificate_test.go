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
	"sigs.k8s.io/controller-runtime/pkg/client"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
)

// When the platform's own certificate ends, status.certificate
// (spec/portal/operations.md, Alerts, Where each fact comes from): notAfter
// and the names it is for, there whenever the Secret of spec.tls holds a
// certificate that can be read, whatever else the condition TLS says of it.

// namedCertPEM is a self-signed certificate valid until notAfter, with the
// common name cn and the DNS names dns, either of them possibly empty.
func namedCertPEM(t *testing.T, notAfter time.Time, cn string, dns ...string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: cn}, DNSNames: dns,
		NotBefore: notAfter.Add(-90 * 24 * time.Hour), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// R.C6: status.certificate is there whenever the Secret of spec.tls holds a
// certificate that can be read — valid and for the platform's hosts, for other
// names, ended, without its key, or left by a cert-manager the cluster no
// longer serves — with when it ends and its DNS names, or its common name where
// it has none. It is absent without spec.tls (the router's certificate, a
// proxy's, plain http), while there is no Secret, cert-manager still issuing
// included, and when the Secret holds no certificate.
func TestRC6TheStatusSaysWhenTheCertificateEnds(t *testing.T) {
	yes := true
	ends := tlsNow.Add(60 * 24 * time.Hour)
	ended := tlsNow.Add(-24 * time.Hour)
	good, goodKey := certPEM(t, ends, "media.example.org", "watch.example.org")
	other, otherKey := certPEM(t, ends, "elsewhere.example.org")
	old, oldKey := certPEM(t, ended, "media.example.org")
	commonName := namedCertPEM(t, ends, "media.example.org")
	nameless := namedCertPEM(t, ends, "")

	cr := func(mut func(z *zaentrumv1alpha1.Zaentrum)) *zaentrumv1alpha1.Zaentrum {
		z := verifyCR()
		z.Spec.Identity.IssuerScheme = "http"
		mut(z)
		return z
	}
	withTLS := func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.TLS = &zaentrumv1alpha1.TLSSpec{} }
	byIssuer := func(z *zaentrumv1alpha1.Zaentrum) {
		z.Spec.TLS = &zaentrumv1alpha1.TLSSpec{IssuerRef: &zaentrumv1alpha1.TLSIssuerRef{Name: "letsencrypt"}}
	}
	for name, c := range map[string]struct {
		z           *zaentrumv1alpha1.Zaentrum
		secret      *corev1.Secret
		certManager bool
		reason      string // what the condition TLS says, whatever the status holds
		notAfter    time.Time
		names       []string
		absent      bool
	}{
		"plain http":       {z: cr(func(*zaentrumv1alpha1.Zaentrum) {}), reason: "PlainHTTP", absent: true},
		"a proxy in front": {z: cr(func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.Identity.IssuerScheme = "https" }), reason: "TerminatedInFront", absent: true},
		"the OpenShift router": {z: cr(func(z *zaentrumv1alpha1.Zaentrum) {
			z.Spec.Routing.ProvisionRoutes = &yes
			z.Spec.Identity.IssuerScheme = "https"
		}), secret: tlsSecretOf("zaentrum-tls", good, goodKey), reason: "Router", absent: true},
		"no Secret": {z: cr(withTLS), reason: "SecretMissing", absent: true},
		"the platform's certificate": {z: cr(withTLS), secret: tlsSecretOf("zaentrum-tls", good, goodKey), reason: "Certificate",
			notAfter: ends, names: []string{"media.example.org", "watch.example.org"}},
		"a certificate for another host": {z: cr(withTLS), secret: tlsSecretOf("zaentrum-tls", other, otherKey), reason: "WrongHost",
			notAfter: ends, names: []string{"elsewhere.example.org"}},
		"an expired certificate": {z: cr(withTLS), secret: tlsSecretOf("zaentrum-tls", old, oldKey), reason: "Expired",
			notAfter: ended, names: []string{"media.example.org"}},
		"no key": {z: cr(withTLS), secret: tlsSecretOf("zaentrum-tls", good, nil), reason: "BadCertificate",
			notAfter: ends, names: []string{"media.example.org", "watch.example.org"}},
		"no certificate": {z: cr(withTLS), secret: tlsSecretOf("zaentrum-tls", []byte("not a pem"), goodKey), reason: "BadCertificate", absent: true},
		"a common name and no DNS name": {z: cr(withTLS), secret: tlsSecretOf("zaentrum-tls", commonName, goodKey), reason: "WrongHost",
			notAfter: ends, names: []string{"media.example.org"}},
		"no name at all": {z: cr(withTLS), secret: tlsSecretOf("zaentrum-tls", nameless, goodKey), reason: "WrongHost",
			notAfter: ends},
		"an issuer and no cert-manager, and the Secret there": {z: cr(byIssuer), secret: tlsSecretOf("zaentrum-tls", good, goodKey),
			reason: "NoCertManager", notAfter: ends, names: []string{"media.example.org", "watch.example.org"}},
		"an issuer and no cert-manager, and no Secret": {z: cr(byIssuer), reason: "NoCertManager", absent: true},
		"cert-manager issuing":                         {z: cr(byIssuer), certManager: true, reason: "Issuing", absent: true},
		"cert-manager's certificate": {z: cr(func(z *zaentrumv1alpha1.Zaentrum) {
			z.Spec.TLS = &zaentrumv1alpha1.TLSSpec{SecretName: "media-cert", IssuerRef: &zaentrumv1alpha1.TLSIssuerRef{Name: "letsencrypt", Kind: "ClusterIssuer"}}
		}), secret: tlsSecretOf("media-cert", good, goodKey), certManager: true, reason: "Certificate",
			notAfter: ends, names: []string{"media.example.org", "watch.example.org"}},
	} {
		t.Run("R.C6 "+name, func(t *testing.T) {
			objs := []client.Object{}
			if c.secret != nil {
				objs = append(objs, c.secret)
			}
			r := tlsReconciler(t, c.certManager, objs...)
			// What an earlier pass wrote is replaced, or removed.
			c.z.Status.Certificate = &zaentrumv1alpha1.CertificateStatus{NotAfter: metav1.NewTime(tlsNow), Names: []string{"before"}}
			sec, issuing, err := r.platformCertificate(context.Background(), c.z)
			require.NoError(t, err)
			r.reportTLS(context.Background(), c.z, sec, issuing)
			cond := meta.FindStatusCondition(c.z.Status.Conditions, condTypeTLS)
			require.NotNil(t, cond)
			assert.Equal(t, c.reason, cond.Reason, cond.Message)

			got := c.z.Status.Certificate
			if c.absent {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.True(t, got.NotAfter.Time.Equal(c.notAfter), "ends %s, not %s", got.NotAfter.Time.UTC(), c.notAfter.UTC())
			assert.Equal(t, c.names, got.Names)
		})
	}
}

// R.C6: every pass writes status.certificate from the Secret it reads: a
// renewed certificate moves notAfter on the next pass, and the field goes when
// the Secret goes or the resource no longer names spec.tls.
func TestRC6APassWritesWhenTheCertificateEnds(t *testing.T) {
	z := verifyCR()
	z.Spec.TLS = &zaentrumv1alpha1.TLSSpec{}
	e := newPlatformEnv(t, z, nil)
	first := e.clock.Add(30 * 24 * time.Hour)
	crt, key := certPEM(t, first, "media.example.org")
	require.NoError(t, e.c.Create(context.Background(), tlsSecretOf("zaentrum-tls", crt, key)))

	e.reconcile()
	got := e.zaentrum().Status.Certificate
	require.NotNil(t, got)
	assert.True(t, got.NotAfter.Time.Equal(first), "ends %s", got.NotAfter.Time.UTC())
	assert.Equal(t, []string{"media.example.org"}, got.Names)

	renewed := e.clock.Add(90 * 24 * time.Hour)
	crt, key = certPEM(t, renewed, "media.example.org")
	var sec corev1.Secret
	require.NoError(t, e.c.Get(context.Background(), client.ObjectKey{Namespace: verifyNS, Name: "zaentrum-tls"}, &sec))
	sec.Data[corev1.TLSCertKey], sec.Data[corev1.TLSPrivateKeyKey] = crt, key
	require.NoError(t, e.c.Update(context.Background(), &sec))
	e.clock = e.clock.Add(time.Minute)
	e.reconcile()
	got = e.zaentrum().Status.Certificate
	require.NotNil(t, got)
	assert.True(t, got.NotAfter.Time.Equal(renewed), "renewed: ends %s", got.NotAfter.Time.UTC())

	require.NoError(t, e.c.Delete(context.Background(), &sec))
	e.reconcile()
	assert.Nil(t, e.zaentrum().Status.Certificate, "the Secret is gone")

	crt, key = certPEM(t, renewed, "media.example.org")
	require.NoError(t, e.c.Create(context.Background(), tlsSecretOf("zaentrum-tls", crt, key)))
	e.reconcile()
	require.NotNil(t, e.zaentrum().Status.Certificate, "the Secret is back")
	e.change(func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.TLS = nil })
	e.reconcile()
	assert.Nil(t, e.zaentrum().Status.Certificate, "no spec.tls: no certificate of its own")
}
