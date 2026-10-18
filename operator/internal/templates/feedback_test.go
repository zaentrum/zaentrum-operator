package templates

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// chino-api's bug reports take every setting from the optional Secret
// zaentrum-feedback, never from a literal, however the platform is installed:
// without the Secret the feature is off (chino-api answers 503 and /api/config
// says features.feedback false), with it the reports go where it says.
func TestBugReportSettingsComeFromTheOptionalFeedbackSecret(t *testing.T) {
	for _, objs := range map[string][]*unstructured.Unstructured{
		"operator":     renderCR(t, base("zaentrum")),
		"demo":         renderCR(t, demoCR("zaentrum-demo")),
		"helm install": helmRender(t, nil),
	} {
		chino := envByName(container(t, objs, "chino-api"))
		for env, key := range map[string]string{
			"OPENPROJECT_URL":         "url",
			"OPENPROJECT_TOKEN":       "token",
			"OPENPROJECT_PROJECT_ID":  "project-id",
			"OPENPROJECT_BUG_TYPE_ID": "bug-type-id",
		} {
			secretRef(t, chino, env, "zaentrum-feedback", key)
		}
	}
}
