package controller

import (
	"regexp"
	"strings"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
)

// This file holds the version rule: which sets a platform restores, by the
// version that made the set and the version that runs.
//
//	the set's version  the platform's       the restore
//	a release          the same or newer    goes on                ok
//	a release          an older release     is refused             newer
//	any other tag      the same tag         goes on                ok
//	anything else: no backup.json, a        is refused, unless     not-comparable
//	release on one side and another tag     zaentrum.io/restore-
//	on the other, or two different tags     any-version holds the
//	                                        set's name
//
// A version is the tag the operator renders, status.currentVersion, on both
// sides, never spec.version. A release is v and a version number, a
// pre-release included, and releases are compared as such numbers are:
// v0.10.0 is newer than v0.9.0, and v0.5.0-rc.2 is older than v0.5.0. Any other
// tag, such as latest, 0.5 or a build's, goes on only into a platform that
// runs that very tag.

// releaseTag is a release: v, then major, minor and patch without leading
// zeros, then a pre-release where it has one. A tag holds no "+".
var releaseTag = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$`)

// release is a release's version number: major, minor and patch, and the
// pre-release's identifiers, nil for none.
type release struct {
	numbers [3]string
	pre     []string
}

// parseRelease reads a tag as a release, and says whether it is one.
func parseRelease(tag string) (release, bool) {
	m := releaseTag.FindStringSubmatch(tag)
	if m == nil {
		return release{}, false
	}
	r := release{numbers: [3]string{m[1], m[2], m[3]}}
	if m[4] != "" {
		r.pre = strings.Split(m[4], ".")
	}
	return r, true
}

// compareReleases is -1, 0 or 1 as a is older than b, the same, or newer.
func compareReleases(a, b release) int {
	for i := range a.numbers {
		if c := compareNumbers(a.numbers[i], b.numbers[i]); c != 0 {
			return c
		}
	}
	switch {
	case a.pre == nil && b.pre == nil:
		return 0
	case a.pre == nil:
		// A release is newer than each of its pre-releases.
		return 1
	case b.pre == nil:
		return -1
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		x, y := a.pre[i], b.pre[i]
		xn, yn := digitsOnly(x), digitsOnly(y)
		switch {
		case xn && yn:
			if c := compareNumbers(x, y); c != 0 {
				return c
			}
		case xn:
			// A number comes before a word: rc.1 is older than rc.beta.
			return -1
		case yn:
			return 1
		default:
			if c := strings.Compare(x, y); c != 0 {
				return c
			}
		}
	}
	return compareInts(len(a.pre), len(b.pre))
}

// compareNumbers compares two strings of digits as the numbers they are, of
// any length.
func compareNumbers(a, b string) int {
	a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
	if c := compareInts(len(a), len(b)); c != 0 {
		return c
	}
	return strings.Compare(a, b)
}

func compareInts(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func digitsOnly(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// versionRule is what the version rule says of a set that setVersion made
// ("" for a set without backup.json) on a platform that runs version: ok,
// newer or not-comparable.
func versionRule(setVersion, version string) string {
	setVersion, version = strings.TrimSpace(setVersion), strings.TrimSpace(version)
	if setVersion == "" {
		return zaentrumv1alpha1.VersionRuleNotComparable
	}
	set, setIsRelease := parseRelease(setVersion)
	runs, runsRelease := parseRelease(version)
	switch {
	case setIsRelease && runsRelease:
		if compareReleases(set, runs) > 0 {
			return zaentrumv1alpha1.VersionRuleNewer
		}
		return zaentrumv1alpha1.VersionRuleOK
	case !setIsRelease && !runsRelease && setVersion == version:
		return zaentrumv1alpha1.VersionRuleOK
	}
	return zaentrumv1alpha1.VersionRuleNotComparable
}

// markVersionRules writes what the version rule says of each set
// status.backup.sets lists, against the version that runs this pass.
func markVersionRules(z *zaentrumv1alpha1.Zaentrum, version string) {
	if z.Status.Backup == nil {
		return
	}
	for i := range z.Status.Backup.Sets {
		s := &z.Status.Backup.Sets[i]
		s.VersionRule = versionRule(s.Version, version)
	}
}
