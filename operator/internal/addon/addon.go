// Package addon installs addon Helm charts next to the platform. It fetches a
// chart archive, layers the values (chart defaults < generated values <
// spec.values < valuesFrom < the reserved zaentrum block), renders the chart in
// memory with the Helm engine and checks every rendered object against the
// addon guardrails.
//
// Nothing here writes to the cluster: the ZaentrumAddon controller reads what
// these functions need, and applies what they return with server-side apply.
package addon

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
)

const (
	// FieldManager is the server-side-apply field manager for addon objects.
	FieldManager = "zaentrum-addon"

	// LabelAddon carries the addon name on every object that belongs to it.
	LabelAddon = "zaentrum.io/addon"
	// LabelComponent names the Deployment on the Deployment and its pods.
	LabelComponent = "zaentrum.io/component"
	// LabelKeep on a values Secret/ConfigMap keeps it when the addon is removed.
	LabelKeep = "zaentrum.io/keep"
	// LabelManagedBy, LabelInstance and LabelPartOf are the standard labels the
	// operator sets on every applied object.
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelInstance  = "app.kubernetes.io/instance"
	LabelPartOf    = "app.kubernetes.io/part-of"
	// ManagedBy is the managed-by value of applied objects. Prune only ever
	// considers objects carrying it, so values objects a user or the portal
	// labelled for an addon are never pruned.
	ManagedBy = "zaentrum-operator"
	// AnnotationValuesChecksum on pod templates rolls pods when values change.
	AnnotationValuesChecksum = "zaentrum.io/values-checksum"

	// ChartAnnotationAddon must be "true" on every addon chart.
	ChartAnnotationAddon = "zaentrum.io/addon"
	// ChartAnnotationPrimary names the Service (port 80) that serves the
	// addon's capability manifest.
	ChartAnnotationPrimary = "zaentrum.io/primary"

	// PlatformKey is the reserved top-level values key the operator sets.
	PlatformKey = "zaentrum"

	// MaxNameLength bounds an addon name, which is also its release name.
	MaxNameLength = 40
)

var digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// GeneratedSecretName is the Secret holding an addon's generated values.
func GeneratedSecretName(addon string) string {
	return "zaentrum-addon-" + addon + "-generated"
}

// ValuesObjectPrefix is the name prefix an addon's own values objects share
// (the portal writes <prefix>values, the operator <prefix>generated).
func ValuesObjectPrefix(addon string) string {
	return "zaentrum-addon-" + addon + "-"
}

// OwnsValuesObject reports whether a Secret/ConfigMap named name with labels
// labels is one this addon may read through valuesFrom or adopt: its name must
// start with the addon's values prefix AND it must carry zaentrum.io/addon=<name>.
// This blocks the confused-deputy read of a platform secret the operator can
// see but the addon has no claim to.
func OwnsValuesObject(addon, name string, labels map[string]string) bool {
	return strings.HasPrefix(name, ValuesObjectPrefix(addon)) && labels[LabelAddon] == addon
}

// Digest returns the sha256 digest of data as "sha256:<hex>".
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ValidateName checks an addon name: a DNS-1123 label of at most 40 characters.
func ValidateName(name string) error {
	if len(name) > MaxNameLength {
		return fmt.Errorf("addon name %q is longer than %d characters", name, MaxNameLength)
	}
	if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
		return fmt.Errorf("addon name %q is not a DNS-1123 label: %s", name, strings.Join(errs, "; "))
	}
	return nil
}

// ValidateChart checks a chart reference before anything is fetched.
func ValidateChart(c zaentrumv1alpha1.AddonChart) error {
	switch {
	case strings.HasPrefix(c.Ref, "oci://"):
		repo := strings.TrimPrefix(c.Ref, "oci://")
		slash := strings.LastIndex(repo, "/")
		if slash <= 0 || slash == len(repo)-1 {
			return fmt.Errorf("chart.ref %q names no chart repository", c.Ref)
		}
		if strings.ContainsAny(repo[slash+1:], ":@") {
			return fmt.Errorf("chart.ref %q must not carry a tag or digest: set chart.version (and chart.digest)", c.Ref)
		}
		if strings.TrimSpace(c.Version) == "" {
			return fmt.Errorf("chart.version is required for oci refs")
		}
	case strings.HasPrefix(c.Ref, "https://"):
		u, err := url.Parse(c.Ref)
		if err != nil || u.Host == "" {
			return fmt.Errorf("chart.ref %q is not a valid https URL", c.Ref)
		}
	default:
		return fmt.Errorf("chart.ref %q must be an oci:// reference or an https:// link", c.Ref)
	}
	if c.Digest != "" && !digestPattern.MatchString(c.Digest) {
		return fmt.Errorf("chart.digest %q must be sha256:<64 lowercase hex characters>", c.Digest)
	}
	return nil
}
