package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
)

// This file says where the platform's mail stands, and removes the Secrets of
// the mail server's password that the resource no longer names.
//
// spec.mail names the SMTP server the platform sends its mail through. Mail is
// set up when it names a server (host) and a sender (from); the chart then
// gives portal-api the settings and, with the bundled identity provider, the
// realm Job writes them into the realm (templates/portal-api.yaml,
// templates/realm.yaml). The user is spec.mail.user; its password alone is in
// the Secret spec.mail.secretName names. The Mail condition says where it
// stands: NotSetUp; SecretMissing, when that Secret is not there or lacks its
// key, password; Configured. It never moves the phase or the Ready condition.
//
// portal-api keeps a new password in a new Secret, labelled zaentrum.io/mail,
// which the resource then names. The operator removes such a Secret once the
// resource has not named it for mailSecretGrace — counted from when this
// operator first saw it unnamed, so a Secret just made, or one the resource
// stopped naming a moment ago, stays — and never one another resource of the
// namespace names, one something else owns, or one without the label.

const (
	condTypeMail = "Mail"

	// labelMail marks a Secret portal-api made for the mail server's password.
	labelMail = "zaentrum.io/mail"

	// The key of the Secret spec.mail.secretName names: the password of
	// spec.mail.user.
	mailPasswordKey = "password"

	// mailSecretGrace is how long the resource has not named a mail Secret
	// before the operator removes it.
	mailSecretGrace = 10 * time.Minute
)

// unnamedMailSecret is a mail Secret the resource does not name: which one,
// and since when this operator has seen it so.
type unnamedMailSecret struct {
	uid   types.UID
	since time.Time
}

// mailSetUp reports whether the platform's mail is set up: a server and a
// sender, as the chart's z.mailSetUp says.
func mailSetUp(z *zaentrumv1alpha1.Zaentrum) bool {
	return z.Spec.Mail.Host != "" && z.Spec.Mail.From != ""
}

// mailSecretVersion is the resourceVersion of the Secret spec.mail.secretName
// names, while mail is set up, for the render (templates.Values): the chart
// puts it on portal-api's pod and on the realm Job, so that the Secret
// appearing, or a change of it, starts portal-api again and runs the realm Job
// again, each of which reads the password at its start. Only metadata is read.
// Empty while no Secret is named or the one named is not there. A Secret that
// cannot be read for any other reason is an error: the render waits for an
// answer rather than start portal-api again on a guess.
func (r *ZaentrumReconciler) mailSecretVersion(ctx context.Context, z *zaentrumv1alpha1.Zaentrum) (string, error) {
	if !mailSetUp(z) || z.Spec.Mail.SecretName == "" {
		return "", nil
	}
	sec := &metav1.PartialObjectMetadata{}
	sec.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
	err := r.reader().Get(ctx, types.NamespacedName{Namespace: z.Namespace, Name: z.Spec.Mail.SecretName}, sec)
	switch {
	case apierrors.IsNotFound(err):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("the Secret %s of the mail server's password could not be read: %w", z.Spec.Mail.SecretName, err)
	}
	return sec.ResourceVersion, nil
}

// reportMail writes the Mail condition. A Secret that cannot be read, for any
// reason but that it is not there, leaves the condition as it was; the next
// pass reads it again.
func (r *ZaentrumReconciler) reportMail(ctx context.Context, z *zaentrumv1alpha1.Zaentrum) {
	set := func(status metav1.ConditionStatus, reason, msg string) {
		setCondition(z, condTypeMail, status, reason, clip(msg, maxVerifyMessage))
	}
	m := z.Spec.Mail
	if !mailSetUp(z) {
		msg := "mail is not set up: spec.mail names no server, so the platform sends no mail"
		if m.Host != "" {
			msg = "mail is not set up: spec.mail names the server " + m.Host + " and no sender (from), so the platform sends no mail"
		}
		set(metav1.ConditionFalse, "NotSetUp", msg)
		return
	}
	if m.SecretName != "" {
		var sec corev1.Secret
		err := r.reader().Get(ctx, types.NamespacedName{Namespace: z.Namespace, Name: m.SecretName}, &sec)
		switch {
		case apierrors.IsNotFound(err):
			set(metav1.ConditionFalse, "SecretMissing", fmt.Sprintf("there is no Secret %s: create it with the "+
				"mail server's password under the key %s", m.SecretName, mailPasswordKey))
			return
		case err != nil:
			log.FromContext(ctx).Info("mail: the Secret of the mail server's password could not be read; trying again",
				"secret", m.SecretName, "error", err.Error())
			return
		}
		if _, ok := sec.Data[mailPasswordKey]; !ok {
			set(metav1.ConditionFalse, "SecretMissing", fmt.Sprintf("Secret %s has no key %s: put the mail server's password under it",
				m.SecretName, mailPasswordKey))
			return
		}
	} else if m.User != "" {
		// The resource's rule refuses a user without secretName; a resource
		// the rule did not run on may still have one.
		set(metav1.ConditionFalse, "SecretMissing", fmt.Sprintf("spec.mail names no Secret for the password of %s: "+
			"name it in secretName", m.User))
		return
	}
	signIn := "without signing in"
	if m.User != "" {
		signIn = fmt.Sprintf("signing in as %s with the password in Secret %s", m.User, m.SecretName)
	}
	set(metav1.ConditionTrue, "Configured", fmt.Sprintf("the platform sends its mail through %s (%s), %s",
		m.Host, orDefaultString(m.Encryption, "starttls"), signIn))
}

// collectMailSecrets removes the mail Secrets of z's namespace the resource
// has not named for mailSecretGrace. Housekeeping: it never fails the
// reconcile, and what it could not list or remove it tries again on the next
// pass. Only metadata is read.
func (r *ZaentrumReconciler) collectMailSecrets(ctx context.Context, z *zaentrumv1alpha1.Zaentrum) {
	logger := log.FromContext(ctx)
	secrets := &metav1.PartialObjectMetadataList{}
	secrets.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("SecretList"))
	if err := r.reader().List(ctx, secrets, client.InNamespace(z.Namespace), client.HasLabels{labelMail}); err != nil {
		logger.Info("mail: the mail Secrets could not be listed; trying again", "error", err.Error())
		return
	}
	named, err := r.namedMailSecrets(ctx, z)
	if err != nil {
		logger.Info("mail: the platforms of the namespace could not be listed; trying again", "error", err.Error())
		return
	}

	now := r.now().Time
	r.mailMu.Lock()
	defer r.mailMu.Unlock()
	if r.mailUnnamed == nil {
		r.mailUnnamed = map[types.NamespacedName]unnamedMailSecret{}
	}
	listed := map[types.NamespacedName]bool{}
	for i := range secrets.Items {
		s := &secrets.Items[i]
		key := types.NamespacedName{Namespace: s.Namespace, Name: s.Name}
		listed[key] = true
		if named[s.Name] || s.DeletionTimestamp != nil || !ownedOnlyBy(s, z.UID) {
			delete(r.mailUnnamed, key)
			continue
		}
		seen, ok := r.mailUnnamed[key]
		if !ok || seen.uid != s.UID {
			r.mailUnnamed[key] = unnamedMailSecret{uid: s.UID, since: now}
			continue
		}
		if now.Sub(seen.since) < mailSecretGrace {
			continue
		}
		s.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
		uid := s.UID
		if err := r.Delete(ctx, s, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
			logger.Info("mail: a mail Secret the resource no longer names could not be removed; trying again",
				"secret", s.Name, "error", err.Error())
			continue
		}
		delete(r.mailUnnamed, key)
		logger.Info("mail: removed a mail Secret the resource has not named for 10 minutes", "secret", s.Name)
	}
	for key := range r.mailUnnamed {
		if key.Namespace == z.Namespace && !listed[key] {
			delete(r.mailUnnamed, key)
		}
	}
}

// namedMailSecrets are the Secrets the platforms of z's namespace name for
// their mail server's password: z's, as read this pass, and any other's.
func (r *ZaentrumReconciler) namedMailSecrets(ctx context.Context, z *zaentrumv1alpha1.Zaentrum) (map[string]bool, error) {
	named := map[string]bool{}
	if z.Spec.Mail.SecretName != "" {
		named[z.Spec.Mail.SecretName] = true
	}
	var platforms zaentrumv1alpha1.ZaentrumList
	if err := r.reader().List(ctx, &platforms, client.InNamespace(z.Namespace)); err != nil {
		return nil, err
	}
	for _, p := range platforms.Items {
		if p.Name != z.Name && p.Spec.Mail.SecretName != "" {
			named[p.Spec.Mail.SecretName] = true
		}
	}
	return named, nil
}
