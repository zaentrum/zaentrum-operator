package controller

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// openShift reports whether the cluster serves OpenShift's security API, the
// SecurityContextConstraints that give every pod its user. The chart names a
// user for its pods only where the cluster does not (templates.Values.OpenShift).
//
// It is discovered once and kept: a cluster does not turn into OpenShift. A
// lookup that fails for any reason but "no such API" is not an answer, and the
// caller defers the render rather than guess — either guess breaks the
// platform: a user named on OpenShift falls outside the namespace's range and
// the SCC refuses the pod; none named elsewhere, and the kubelet refuses every
// image whose user is not a number.
func (r *ZaentrumReconciler) openShift() (bool, error) {
	r.clusterMu.Lock()
	defer r.clusterMu.Unlock()
	if r.OpenShift != nil {
		return *r.OpenShift, nil
	}
	_, err := r.RESTMapper().RESTMapping(
		schema.GroupKind{Group: "security.openshift.io", Kind: "SecurityContextConstraints"}, "v1")
	switch {
	case err == nil:
		yes := true
		r.OpenShift = &yes
	case meta.IsNoMatchError(err):
		no := false
		r.OpenShift = &no
	default:
		return false, fmt.Errorf("could not tell whether the cluster is OpenShift: %w", err)
	}
	return *r.OpenShift, nil
}
