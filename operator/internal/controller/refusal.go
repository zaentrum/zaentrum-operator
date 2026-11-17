package controller

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
)

// reasonResourceRefused is the Ready condition's reason while the stored
// resource holds a value of spec.databases the rules refuse: nothing new is
// applied until it is corrected.
const reasonResourceRefused = "ResourceRefused"

var (
	databaseName    = regexp.MustCompile(zaentrumv1alpha1.DatabaseNamePattern)
	databaseOneHost = regexp.MustCompile(zaentrumv1alpha1.DatabaseOneHostPattern)
)

// The operator's own refusals of spec.databases, in the Words' row "The
// operator's own check of spec.databases". None repeats the value it refuses;
// at a write the cluster refuses the same values by the schema, in its own
// words.
func databaseNameRefusal(key string) string {
	return key + " must be a name a database can have: at most 63 ASCII letters, digits, underscores, hyphens and dots, starting with a letter, a digit or an underscore"
}

const (
	databaseHostRefusal    = "host must be a host name or an IP address in ASCII, an IPv6 address in square brackets; several are separated by commas"
	databaseSSLModeRefusal = "sslmode must be disable, allow, prefer, require, verify-ca or verify-full"
)

// databaseHostHolds says whether host is what the rules take: one host or
// several, separated by commas, each a host DatabaseOneHostPattern takes of
// at most DatabaseHostMaxLength characters, a trailing dot not counted; or
// empty. Where the schema's pattern takes a host this refuses, it is for its
// length alone: a pattern cannot count it.
func databaseHostHolds(host string) bool {
	if host == "" {
		return true
	}
	for _, h := range strings.Split(host, ",") {
		if !databaseOneHost.MatchString(h) ||
			utf8.RuneCountInString(strings.TrimSuffix(h, ".")) > zaentrumv1alpha1.DatabaseHostMaxLength {
			return false
		}
	}
	return true
}

// refusals checks the values of spec.databases with the rules the schema
// holds, for a resource stored before the schema had them, and gives the
// refusals of those that fail, in the order of the fields. The names are
// checked always; the host and the sslmode only with external databases, the
// only place the chart reads them, so a value left there stops nothing. None
// for a resource the rules take.
func refusals(z *zaentrumv1alpha1.Zaentrum) []string {
	var out []string
	d := z.Spec.Databases
	for _, f := range []struct{ key, value string }{
		{"chino", d.Chino}, {"katalog", d.Katalog}, {"keycloak", d.Keycloak}, {"portal", d.Portal},
	} {
		if !databaseName.MatchString(f.value) {
			out = append(out, databaseNameRefusal(f.key))
		}
	}
	if d.Mode == "external" {
		if !databaseHostHolds(d.External.Host) {
			out = append(out, databaseHostRefusal)
		}
		if !slices.Contains(zaentrumv1alpha1.DatabaseSSLModes, d.External.SSLMode) {
			out = append(out, databaseSSLModeRefusal)
		}
	}
	return out
}

// refusedMessage is the Ready condition's message for the refusals.
func refusedMessage(refused []string) string {
	return strings.Join(refused, "; ") + "; nothing new is applied until the resource is corrected"
}

// restoreWhileRefused is the refusal of a restore asked for while the resource
// is refused, at once, so that it does not start once the resource is
// corrected.
const restoreWhileRefused = "the resource is refused, so nothing is restored; correct it and ask again"

// restoreHeld is the Restore condition's message for a restore in flight while
// the resource is refused.
func restoreHeld(dump string) string {
	return "the restore of " + dump + " waits: the resource is refused, and it goes on once the resource is corrected"
}

// answerRestoreWhileRefused refuses a restore asked for while the resource is
// refused. A restore in flight is not taken a step further: the pass applies
// nothing, so its clients stay stopped and its Job's end is not read; its
// condition says that it waits, and its status keeps what it said.
func (r *ZaentrumReconciler) answerRestoreWhileRefused(ctx context.Context, z *zaentrumv1alpha1.Zaentrum) {
	if dump, inFlight := restoring(z); inFlight {
		setCondition(z, condTypeRestore, metav1.ConditionUnknown, "Waiting", restoreHeld(dump))
		return
	}
	if request := restoreRequest(z); request != "" {
		r.refuseRestore(ctx, z, request, reasonRefused, restoreWhileRefused)
	}
}

// takeRefusalBack removes the refusal from Ready in a pass the check takes,
// before anything in it can fail, and says whether it did: while the pass
// runs, Ready says nothing until the pass writes what it finds. A restore in
// flight goes on, and its condition says again what its status says.
func takeRefusalBack(z *zaentrumv1alpha1.Zaentrum) bool {
	c := meta.FindStatusCondition(z.Status.Conditions, condTypeReady)
	if c == nil || c.Reason != reasonResourceRefused {
		return false
	}
	meta.RemoveStatusCondition(&z.Status.Conditions, condTypeReady)
	z.Status.Phase = "Reconciling"
	if _, inFlight := restoring(z); inFlight {
		setRestore(z, restoreStatus(z))
	}
	return true
}

// controllerWhileRefused is the controller's report in a refused pass, which
// consults no channel: the operator that runs, with the update the last pass
// that consulted the channel found for it, where it is the same operator.
func (r *ZaentrumReconciler) controllerWhileRefused(ctx context.Context, z *zaentrumv1alpha1.Zaentrum) *zaentrumv1alpha1.ControllerStatus {
	cur := r.controllerReport(ctx, z, "")
	if prev := z.Status.Controller; prev != nil && prev.Image == cur.Image {
		cur.AvailableUpdate = prev.AvailableUpdate
		if sameReading(prev, cur) {
			cur.ObservedAt = prev.ObservedAt
		}
	}
	return cur
}
