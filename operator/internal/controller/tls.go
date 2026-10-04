package controller

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
	"github.com/zaentrum/zaentrum-operator/operator/internal/templates"
)

// This file serves the platform's hosts over https with a certificate of its
// own (spec.tls), and says in the TLS condition how they are served.
//
// The certificate is a kubernetes.io/tls Secret, brought or written by
// cert-manager from a Certificate the chart renders for spec.tls.issuerRef
// where the cluster serves cert-manager's API. The Ingress names the Secret;
// OpenShift Routes cannot, so the operator reads it every pass and the Routes
// carry it inline — a renewed certificate reaches them on the next pass. With
// spec.tls every URL the platform derives is https.
//
// The TLS condition never moves the phase or Ready: True where the platform's
// hosts are served over https — its certificate, valid and for them; the
// OpenShift router; a proxy in front (identity.issuerScheme https) — False
// where they are not or cannot be (plain http, a missing or wrong certificate,
// cert-manager absent, Routes serving https for an http issuer), Unknown while
// cert-manager issues.

const (
	condTypeTLS = "TLS"
	defaultTLS  = "zaentrum-tls"
)

// certificateCluster says whether the cluster serves cert-manager's
// Certificate API. Asked on every pass that needs it, as cert-manager may come
// after the platform. A lookup that fails for any reason but "no such API" is
// no answer, and the render waits for one, as it waits for OpenShift's
// (openshift.go): a render without the platform's Certificate would have it
// removed as no longer rendered (prune.go) — and where cert-manager owns the
// certificate's Secret, the certificate with it.
func (r *ZaentrumReconciler) certificateCluster() (bool, error) {
	_, err := r.RESTMapper().RESTMapping(schema.GroupKind{Group: "cert-manager.io", Kind: "Certificate"}, "v1")
	switch {
	case err == nil:
		return true, nil
	case meta.IsNoMatchError(err):
		return false, nil
	}
	return false, fmt.Errorf("could not tell whether the cluster serves cert-manager: %w", err)
}

// tlsSecretName is the Secret spec.tls names.
func tlsSecretName(z *zaentrumv1alpha1.Zaentrum) string {
	if z.Spec.TLS != nil && z.Spec.TLS.SecretName != "" {
		return z.Spec.TLS.SecretName
	}
	return defaultTLS
}

// platformCertificate reads what spec.tls asks for: the certificate's Secret,
// nil while it is not there, and whether cert-manager is there to issue it.
// The render needs both before it is made.
func (r *ZaentrumReconciler) platformCertificate(ctx context.Context, z *zaentrumv1alpha1.Zaentrum) (*corev1.Secret, bool, error) {
	t := z.Spec.TLS
	if t == nil {
		return nil, false, nil
	}
	issuing := false
	if t.IssuerRef != nil && t.IssuerRef.Name != "" {
		var err error
		if issuing, err = r.certificateCluster(); err != nil {
			return nil, false, err
		}
	}
	var sec corev1.Secret
	err := r.reader().Get(ctx, types.NamespacedName{Namespace: z.Namespace, Name: tlsSecretName(z)}, &sec)
	switch {
	case apierrors.IsNotFound(err):
		return nil, issuing, nil
	case err != nil:
		return nil, issuing, err
	}
	return &sec, issuing, nil
}

// certificateValues puts what platformCertificate found into the render: that
// cert-manager is there to issue, and for the Routes, which carry it inline,
// the certificate itself.
func certificateValues(v *templates.Values, z *zaentrumv1alpha1.Zaentrum, sec *corev1.Secret, issuing bool) {
	v.CertManager = issuing
	if sec == nil || z.Spec.Routing.ProvisionRoutes == nil || !*z.Spec.Routing.ProvisionRoutes {
		return
	}
	v.TLSCertificate = string(sec.Data[corev1.TLSCertKey])
	v.TLSKey = string(sec.Data[corev1.TLSPrivateKeyKey])
	v.TLSCA = string(sec.Data["ca.crt"])
}

// tlsHosts are the hosts the platform's certificate must be for.
func tlsHosts(z *zaentrumv1alpha1.Zaentrum) []string {
	hosts := []string{hostnameOf(z)}
	if z.Spec.Routing.Mode == "subdomains" && z.Spec.Routing.Hosts.Chino != "" {
		hosts = append(hosts, z.Spec.Routing.Hosts.Chino)
	}
	return hosts
}

func hostnameOf(z *zaentrumv1alpha1.Zaentrum) string {
	if z.Spec.Hostname != "" {
		return z.Spec.Hostname
	}
	return "zaentrum.localhost"
}

// reportTLS writes the TLS condition. sec and issuing are what
// platformCertificate found this pass.
func (r *ZaentrumReconciler) reportTLS(ctx context.Context, z *zaentrumv1alpha1.Zaentrum, sec *corev1.Secret, issuing bool) {
	set := func(status metav1.ConditionStatus, reason, msg string) {
		setCondition(z, condTypeTLS, status, reason, clip(msg, maxVerifyMessage))
	}
	host := hostnameOf(z)
	t := z.Spec.TLS
	if t == nil {
		routes := z.Spec.Routing.ProvisionRoutes != nil && *z.Spec.Routing.ProvisionRoutes
		https := z.Spec.Identity.IssuerScheme == "https"
		// identity.issuerScheme is the scheme of the issuer the platform
		// derives from its host; an issuer named outright is its own.
		derived := z.Spec.Identity.Issuer == ""
		switch {
		case routes && (https || !derived):
			set(metav1.ConditionTrue, "Router", "the OpenShift router serves "+host+" over https with its own certificate")
		case routes:
			set(metav1.ConditionFalse, "IssuerSchemeHTTP", "the Routes serve "+host+" over https, but identity.issuerScheme is http: "+
				"the tokens name an http issuer; set it to https, or give the platform spec.tls")
		case https:
			set(metav1.ConditionTrue, "TerminatedInFront", "TLS for "+host+" is terminated in front of the platform "+
				"(identity.issuerScheme https); the platform serves no certificate of its own")
		default:
			set(metav1.ConditionFalse, "PlainHTTP", "the platform answers over plain http at http://"+host+": a browser signs in "+
				"only on a localhost name, phones and TVs not at all; give it a certificate with spec.tls")
		}
		return
	}

	name := tlsSecretName(z)
	hosts := tlsHosts(z)
	byIssuer := t.IssuerRef != nil && t.IssuerRef.Name != ""
	if byIssuer && !issuing {
		set(metav1.ConditionFalse, "NoCertManager", fmt.Sprintf("spec.tls.issuerRef names %s, but the cluster serves no "+
			"cert-manager API: nothing issues the certificate for %s", t.IssuerRef.Name, strings.Join(hosts, ", ")))
		return
	}
	if sec == nil {
		if byIssuer {
			set(metav1.ConditionUnknown, "Issuing", fmt.Sprintf("cert-manager issues the certificate for %s into Secret %s: %s",
				strings.Join(hosts, ", "), name, r.certificateReadiness(ctx, z)))
			return
		}
		set(metav1.ConditionFalse, "SecretMissing", fmt.Sprintf("there is no Secret %s (tls.crt, tls.key) for %s: until there is, "+
			"the ingress serves its own default certificate", name, strings.Join(hosts, ", ")))
		return
	}
	cert, err := leafCertificate(sec.Data[corev1.TLSCertKey])
	switch {
	case err != nil:
		set(metav1.ConditionFalse, "BadCertificate", fmt.Sprintf("Secret %s holds no certificate the platform can serve: %v", name, err))
		return
	case len(sec.Data[corev1.TLSPrivateKeyKey]) == 0:
		set(metav1.ConditionFalse, "BadCertificate", fmt.Sprintf("Secret %s holds no tls.key", name))
		return
	}
	for _, h := range hosts {
		if err := cert.VerifyHostname(h); err != nil {
			set(metav1.ConditionFalse, "WrongHost", fmt.Sprintf("the certificate in Secret %s is for %s, not %s",
				name, strings.Join(certificateNames(cert), ", "), h))
			return
		}
	}
	now := r.now().Time
	if now.After(cert.NotAfter) {
		set(metav1.ConditionFalse, "Expired", fmt.Sprintf("the certificate in Secret %s expired on %s", name, cert.NotAfter.UTC().Format("2006-01-02")))
		return
	}
	from := "Secret " + name
	if byIssuer {
		from = fmt.Sprintf("Secret %s, which cert-manager keeps from %s %s", name, orDefaultString(t.IssuerRef.Kind, "Issuer"), t.IssuerRef.Name)
	}
	set(metav1.ConditionTrue, "Certificate", fmt.Sprintf("%s served over https with the certificate in %s, valid until %s",
		strings.Join(hosts, ", "), from, cert.NotAfter.UTC().Format("2006-01-02")))
}

// certificateReadiness is what cert-manager says of the platform's Certificate.
func (r *ZaentrumReconciler) certificateReadiness(ctx context.Context, z *zaentrumv1alpha1.Zaentrum) string {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"})
	if err := r.reader().Get(ctx, types.NamespacedName{Namespace: z.Namespace, Name: templates.CertificateName}, u); err != nil {
		return "the Certificate is not there yet"
	}
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		cm, _ := c.(map[string]interface{})
		if cm["type"] == "Ready" {
			msg, _ := cm["message"].(string)
			return orDefaultString(msg, "not ready")
		}
	}
	return "it has not answered yet"
}

// leafCertificate parses the first certificate of a PEM bundle.
func leafCertificate(bundle []byte) (*x509.Certificate, error) {
	for {
		var block *pem.Block
		block, bundle = pem.Decode(bundle)
		if block == nil {
			return nil, fmt.Errorf("tls.crt has no PEM certificate")
		}
		if block.Type == "CERTIFICATE" {
			return x509.ParseCertificate(block.Bytes)
		}
	}
}

// certificateNames are the names a certificate is for.
func certificateNames(cert *x509.Certificate) []string {
	names := append([]string{}, cert.DNSNames...)
	if len(names) == 0 && cert.Subject.CommonName != "" {
		names = append(names, cert.Subject.CommonName)
	}
	if len(names) == 0 {
		names = []string{"no name"}
	}
	return names
}

func orDefaultString(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
