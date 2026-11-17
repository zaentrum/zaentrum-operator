package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
)

// The version rule, as the chapter's table has it: a release into the same
// release or a newer one goes on, into an older one never; any other tag goes
// on only into that very tag; anything else cannot be compared. Releases are
// compared as version numbers are, pre-releases included.
func TestRC10VersionRule(t *testing.T) {
	const (
		ok            = zaentrumv1alpha1.VersionRuleOK
		newer         = zaentrumv1alpha1.VersionRuleNewer
		notComparable = zaentrumv1alpha1.VersionRuleNotComparable
	)
	for _, c := range []struct{ set, runs, want, why string }{
		{"v0.5.0", "v0.5.0", ok, "the same release"},
		{"v0.4.0", "v0.5.0", ok, "an older release into a newer one"},
		{"v0.9.0", "v0.10.0", ok, "v0.10.0 is newer than v0.9.0"},
		{"v0.10.0", "v0.9.0", newer, "a newer release, compared as numbers are"},
		{"v1.0.0", "v0.99.9", newer, "the major number first"},
		{"v0.5.0-rc.2", "v0.5.0", ok, "v0.5.0-rc.2 is older than v0.5.0"},
		{"v0.5.0", "v0.5.0-rc.2", newer, "a release is newer than its pre-release"},
		{"v0.5.0-rc.1", "v0.5.0-rc.2", ok, "an older pre-release"},
		{"v0.5.0-rc.10", "v0.5.0-rc.9", newer, "a pre-release's numbers as numbers"},
		{"v0.5.0-rc.1", "v0.5.0-rc.beta", ok, "a number before a word"},
		{"v0.5.0-rc", "v0.5.0-rc.1", ok, "fewer parts before more"},
		{"v0.6.0-rc.1", "v0.5.0", newer, "a pre-release of a newer release is newer"},
		{"latest", "latest", ok, "the same other tag"},
		{"0.5", "0.5", ok, "a minor version's tag, the same"},
		{"sha-0123456789ab", "sha-0123456789ab", ok, "a build's tag, the same"},
		{"latest", "v0.5.0", notComparable, "another tag against a release"},
		{"v0.5.0", "latest", notComparable, "a release against another tag"},
		{"0.5", "v0.5.0", notComparable, "a minor version's tag against a release"},
		{"latest", "0.5", notComparable, "two different tags"},
		{"sha-1", "sha-2", notComparable, "two builds"},
		{"", "v0.5.0", notComparable, "a set without backup.json has no version"},
		{"", "latest", notComparable, "nor against another tag"},
		{"v0.5", "v0.5.0", notComparable, "v and two numbers is no release"},
		{"0.5.0", "v0.5.0", notComparable, "without its v, no release"},
		{"v01.2.3", "v1.2.3", notComparable, "a leading zero is no version number"},
	} {
		assert.Equal(t, c.want, versionRule(c.set, c.runs), "R.C10 %s into %s: %s", c.set, c.runs, c.why)
	}
}
