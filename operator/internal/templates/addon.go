package templates

import (
	"fmt"
	"path"
	"strings"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	"sigs.k8s.io/yaml"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
)

// addonPlatformTemplate renders the reserved `zaentrum` values block addon
// charts read platform facts from. It is evaluated with the platform chart's
// own helpers and the same CR-derived values the platform renders with, so an
// addon sees exactly the issuer, brokers, topic prefix and claim the platform
// services are configured with — never a second derivation that can drift.
const addonPlatformTemplate = `namespace: {{ .Release.Namespace | toJson }}
hostname: {{ .Values.global.hostname | toJson }}
issuer: {{ include "z.issuer" . | toJson }}
issuerHostAliasIP: {{ .Values.network.issuerHostAliasIP | toJson }}
imagePullSecrets: {{ .Values.global.imagePullSecrets | toJson }}
partOf: {{ printf "%s-addons" (include "z.partOf" .) | toJson }}
events:
  brokers: {{ include "z.kafkaBrokers" . | toJson }}
  topicPrefix: {{ include "z.topicPrefix" . | toJson }}
  tlsSecret: {{ include "z.kafkaCertSecret" . | toJson }}
media:
  claimName: {{ include "z.mediaClaimName" . | toJson }}
`

const addonPlatformTemplateName = "templates/zaentrum-addon-platform.yaml"

// AddonPlatformValues returns the reserved `zaentrum` values block for addons
// installed next to the platform described by z (contract: addon charts, §1).
// Only the chart's partials are rendered, not the platform itself.
func AddonPlatformValues(z *zaentrumv1alpha1.Zaentrum) (map[string]interface{}, error) {
	chrt, err := loadChart()
	if err != nil {
		return nil, err
	}
	var tpls []*chart.File
	for _, f := range chrt.Templates {
		if strings.HasPrefix(path.Base(f.Name), "_") {
			tpls = append(tpls, f)
		}
	}
	chrt.Templates = append(tpls, &chart.File{Name: addonPlatformTemplateName, Data: []byte(addonPlatformTemplate)})

	v := NewValues(z)
	relOpts := chartutil.ReleaseOptions{Name: "zaentrum", Namespace: v.Namespace}
	renderVals, err := chartutil.ToRenderValues(chrt, v.chartValues(), relOpts, nil)
	if err != nil {
		return nil, fmt.Errorf("build render values: %w", err)
	}
	rendered, err := engine.Render(chrt, renderVals)
	if err != nil {
		return nil, fmt.Errorf("render addon platform values: %w", err)
	}
	var body string
	for name, out := range rendered {
		if strings.HasSuffix(name, "/"+addonPlatformTemplateName) {
			body = out
		}
	}
	vals := map[string]interface{}{}
	if err := yaml.Unmarshal([]byte(body), &vals); err != nil {
		return nil, fmt.Errorf("decode addon platform values: %w", err)
	}
	if len(vals) == 0 {
		return nil, fmt.Errorf("addon platform values rendered empty")
	}
	return vals, nil
}
