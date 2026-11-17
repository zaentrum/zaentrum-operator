package controller

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
)

// The settings of the platform's alerts in its resource, and the two facts the
// operator adds to the status for them (spec/portal/operations.md, Alerts):
// since when a part is not ready, and when the platform's certificate ends.

// canonicalSchema is the openAPIV3Schema of the canonical Zaentrum CRD, its
// descriptions kept.
func canonicalSchema(t *testing.T) any {
	t.Helper()
	for _, d := range docs(t, canonicalCRD) {
		if d["kind"] == "CustomResourceDefinition" && dig(d, "metadata", "name") == "zaentrums.zaentrum.io" {
			versions, _ := dig(d, "spec", "versions").([]any)
			require.Len(t, versions, 1)
			return dig(versions[0], "schema", "openAPIV3Schema")
		}
	}
	t.Fatalf("%s: no zaentrums.zaentrum.io CRD", canonicalCRD)
	return nil
}

// R.C6: spec.alerts holds the five settings of The five alerts: four
// thresholds, whole numbers that the cluster refuses outside their ranges with
// the schema's own minimum and maximum, in the cluster's words, and whether
// alerts are mailed. The resource holds no defaults — a setting left out means
// its default, which portal-api applies — and each field's description says
// what its default is.
func TestRC6TheAlertsSchemaChecksWhatTheSpecSays(t *testing.T) {
	alerts := dig(canonicalSchema(t), "properties", "spec", "properties", "alerts")
	require.NotNil(t, alerts, "spec.alerts")
	assert.Equal(t, "object", dig(alerts, "type"))
	props, _ := dig(alerts, "properties").(map[string]any)
	assert.ElementsMatch(t, []string{"partNotReadyMinutes", "volumeFullPercent", "certificateDays", "backupAgeDays", "mail"}, keysOf(props))

	for f, c := range map[string]struct{ least, most, def int }{
		"partNotReadyMinutes": {1, 1440, 10},
		"volumeFullPercent":   {50, 99, 90},
		"certificateDays":     {1, 90, 14},
		"backupAgeDays":       {1, 30, 2},
	} {
		assert.Equal(t, "integer", dig(props, f, "type"), f)
		assert.Equal(t, float64(c.least), dig(props, f, "minimum"), f)
		assert.Equal(t, float64(c.most), dig(props, f, "maximum"), f)
		description, _ := dig(props, f, "description").(string)
		assert.Contains(t, description, fmt.Sprintf("Default %d.", c.def), f)
	}
	assert.Equal(t, "boolean", dig(props, "mail", "type"))
	description, _ := dig(props, "mail", "description").(string)
	assert.Contains(t, description, "Default true.")
	assert.Contains(t, strings.Join(strings.Fields(description), " "),
		"to every admin whose account is enabled and has an address, and to every address of spec.mail.alertsTo",
		"who gets alert mail, as Alerts, By mail has it")

	for f := range props {
		assert.Nil(t, dig(props, f, "default"), "%s: no default in the resource", f)
		assert.Nil(t, dig(props, f, "x-kubernetes-validations"), "%s: the cluster's own words, no rule of ours", f)
	}
	assert.Nil(t, dig(alerts, "default"), "spec.alerts: no default in the resource")
	assert.Nil(t, dig(alerts, "required"))
	assert.Nil(t, dig(alerts, "x-kubernetes-validations"))
}

// R.C6: what the operator's types write of spec.alerts leaves a setting out
// that is left out, so that it means its default, and keeps mail: false, which
// means something: a resource that turns alert mail off keeps it off through
// every client that reads and writes it with these types.
func TestRC6AnAlertSettingLeftOutStaysOutAndMailOffStaysOff(t *testing.T) {
	for in, out := range map[string]string{
		`{}`:                         `{}`,
		`{"mail":false}`:             `{"mail":false}`,
		`{"mail":true}`:              `{"mail":true}`,
		`{"partNotReadyMinutes":30}`: `{"partNotReadyMinutes":30}`,
		`{"volumeFullPercent":95,"certificateDays":30,"backupAgeDays":7}`: `{"volumeFullPercent":95,"certificateDays":30,"backupAgeDays":7}`,
	} {
		var a zaentrumv1alpha1.AlertsSpec
		require.NoError(t, json.Unmarshal([]byte(in), &a), in)
		b, err := json.Marshal(a)
		require.NoError(t, err)
		assert.JSONEq(t, out, string(b), in)
	}
}

// R.C6: the status holds since when a part is not ready and when the
// platform's certificate ends, as times (Alerts, Where each fact comes from).
// Neither is required: notReadySince is absent while a part is ready, and the
// certificate where the platform has none of its own that can be read. A
// certificate there always says when it ends.
func TestRC6TheStatusSchemaHoldsSinceWhenAndUntilWhen(t *testing.T) {
	status := dig(canonicalSchema(t), "properties", "status", "properties")
	require.NotNil(t, status, "status")

	component := dig(status, "components", "items")
	since := dig(component, "properties", "notReadySince")
	require.NotNil(t, since, "status.components[].notReadySince")
	assert.Equal(t, "string", dig(since, "type"))
	assert.Equal(t, "date-time", dig(since, "format"))
	stalled := dig(component, "properties", "stalled")
	require.NotNil(t, stalled, "status.components[].stalled")
	assert.Equal(t, "boolean", dig(stalled, "type"))
	assert.Equal(t, []any{"name", "ready"}, dig(component, "required"), "neither notReadySince nor stalled is required")

	certificate := dig(status, "certificate")
	require.NotNil(t, certificate, "status.certificate")
	assert.Equal(t, "object", dig(certificate, "type"))
	props, _ := dig(certificate, "properties").(map[string]any)
	assert.ElementsMatch(t, []string{"notAfter", "names"}, keysOf(props))
	assert.Equal(t, "string", dig(props, "notAfter", "type"))
	assert.Equal(t, "date-time", dig(props, "notAfter", "format"))
	assert.Equal(t, "array", dig(props, "names", "type"))
	assert.Equal(t, "string", dig(props, "names", "items", "type"))
	assert.Equal(t, []any{"notAfter"}, dig(certificate, "required"))
	statusRequired, _ := dig(canonicalSchema(t), "properties", "status", "required").([]any)
	assert.NotContains(t, statusRequired, "certificate")
}
