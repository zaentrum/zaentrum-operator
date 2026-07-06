// Package templates renders the zaentrum platform from the canonical embedded
// Helm chart (operator/platform/chart) and returns the manifests as unstructured
// objects for the controller's server-side apply.
//
// Helm is used ONLY as a template engine — a client-only render (no release, no
// Helm-side apply, no release Secret). The operator keeps its own machinery
// unchanged: SSA with FieldManager "zaentrum-operator" + ForceOwnership,
// ownerRef-cascade GC, status.components roll-up, and the Stage-2 channel-update
// tag override. This chart is the SINGLE source of truth — the same one
// self-hosters `helm install`.
package templates

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
	"github.com/zaentrum/zaentrum-operator/operator/platform"
)

// FieldManager is the server-side-apply field manager the operator owns.
const FieldManager = "zaentrum-operator"

// Values is the minimal render context the controller manipulates: the CR plus
// the version to render (the controller overrides Version with the channel
// decision's tag). Render() maps it onto the chart's values.yaml.
type Values struct {
	cr        *zaentrumv1alpha1.Zaentrum
	Version   string
	Namespace string
}

// NewValues builds the render context from a Zaentrum CR.
func NewValues(z *zaentrumv1alpha1.Zaentrum) Values {
	ns := z.Namespace
	if ns == "" {
		ns = "zaentrum"
	}
	return Values{cr: z, Version: orDefault(z.Spec.Version, "latest"), Namespace: ns}
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func derefBool(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// chartValues maps the CR onto the chart's values.yaml structure (the same keys
// values-demo.yaml / values.yaml use).
func (v Values) chartValues() map[string]interface{} {
	spec := v.cr.Spec
	partOf := spec.PartOf
	if partOf == "" {
		partOf = v.Namespace
	}
	mediaSize := "50Gi"
	if !spec.Storage.MediaSize.IsZero() {
		mediaSize = spec.Storage.MediaSize.String()
	}
	pull := make([]interface{}, 0, len(spec.ImagePullSecrets))
	for _, s := range spec.ImagePullSecrets {
		pull = append(pull, s)
	}
	services := map[string]interface{}{}
	for name, n := range spec.Replicas {
		services[name] = map[string]interface{}{"replicas": int(n)}
	}
	return map[string]interface{}{
		"global": map[string]interface{}{
			"version":          v.Version,
			"hostname":         orDefault(spec.Hostname, "zaentrum.localhost"),
			"partOf":           partOf,
			"imagePullSecrets": pull,
		},
		"identity": map[string]interface{}{
			"mode":         orDefault(string(spec.Identity.Mode), "bundled"),
			"issuer":       spec.Identity.Issuer,
			"issuerScheme": orDefault(spec.Identity.IssuerScheme, "http"),
			"clientId":     orDefault(spec.Identity.ClientID, "chino-web"),
			"audience":     orDefault(spec.Identity.Audience, "chino"),
			"loginTheme":   spec.Identity.LoginTheme,
		},
		"features": map[string]interface{}{
			"kafka":    spec.Features.Kafka,
			"gpu":      spec.Features.GPU,
			"pipeline": spec.Features.Pipeline,
		},
		"storage": map[string]interface{}{
			"mediaSize":      mediaSize,
			"className":      spec.Storage.ClassName,
			"provisionMedia": derefBool(spec.Storage.ProvisionMedia, true),
		},
		"network": map[string]interface{}{
			"issuerHostAliasIP": spec.Network.IssuerHostAliasIP,
		},
		"routing": map[string]interface{}{
			"provisionIngress": derefBool(spec.Routing.ProvisionIngress, true),
			"provisionRoutes":  derefBool(spec.Routing.ProvisionRoutes, false),
		},
		"secrets": map[string]interface{}{
			"external": spec.Secrets.External,
		},
		// seed/scan/enqueue Jobs are demo choreography applied externally; the
		// operator never renders them.
		"jobs": map[string]interface{}{"seed": false},
		"databases": map[string]interface{}{
			"mode":     orDefault(spec.Databases.Mode, "perApp"),
			"chino":    orDefault(spec.Databases.Chino, "chino"),
			"katalog":  orDefault(spec.Databases.Katalog, "katalog"),
			"keycloak": orDefault(spec.Databases.Keycloak, "keycloak"),
			"portal":   orDefault(spec.Databases.Portal, "portal"),
		},
		"keycloak": map[string]interface{}{
			"image": orDefault(spec.Keycloak.Image, "quay.io/keycloak/keycloak:26.0.7"),
		},
		"services": services,
	}
}

// loadChart loads the embedded chart into a *chart.Chart.
func loadChart() (*chart.Chart, error) {
	const root = "chart"
	var files []*loader.BufferedFile
	err := fs.WalkDir(platform.Chart, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, rerr := platform.Chart.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		files = append(files, &loader.BufferedFile{Name: strings.TrimPrefix(p, root+"/"), Data: data})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read embedded chart: %w", err)
	}
	return loader.LoadFiles(files)
}

// Render renders the platform chart with the CR-derived values and returns the
// objects. Client-only (no Helm release/apply). Every object is namespaced to the
// CR's namespace (templates that omit the field still land correctly).
func Render(v Values) ([]*unstructured.Unstructured, error) {
	chrt, err := loadChart()
	if err != nil {
		return nil, err
	}
	// Populate Capabilities so a client-only render can gate on the OpenShift
	// Route GVK. Release name/namespace back .Release.* in the templates.
	caps := chartutil.DefaultCapabilities.Copy()
	caps.APIVersions = append(caps.APIVersions, "route.openshift.io/v1", "route.openshift.io/v1/Route")
	relOpts := chartutil.ReleaseOptions{Name: "zaentrum", Namespace: v.Namespace}
	renderVals, err := chartutil.ToRenderValues(chrt, v.chartValues(), relOpts, caps)
	if err != nil {
		return nil, fmt.Errorf("build render values: %w", err)
	}
	rendered, err := engine.Render(chrt, renderVals)
	if err != nil {
		return nil, fmt.Errorf("render chart: %w", err)
	}

	names := make([]string, 0, len(rendered))
	for name := range rendered {
		names = append(names, name)
	}
	sort.Strings(names)
	var combined bytes.Buffer
	for _, name := range names {
		if strings.HasSuffix(name, "NOTES.txt") || strings.HasSuffix(name, "_helpers.tpl") {
			continue
		}
		body := rendered[name]
		if strings.TrimSpace(body) == "" {
			continue
		}
		combined.WriteString("\n---\n")
		combined.WriteString(body)
	}

	objs, err := decode(combined.Bytes())
	if err != nil {
		return nil, err
	}
	for _, o := range objs {
		if o.GetNamespace() == "" {
			o.SetNamespace(v.Namespace)
		}
	}
	return objs, nil
}

// decode splits a multi-document YAML stream into unstructured objects,
// skipping empty/whitespace-only documents.
func decode(data []byte) ([]*unstructured.Unstructured, error) {
	var out []*unstructured.Unstructured
	dec := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		raw := map[string]interface{}{}
		err := dec.Decode(&raw)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode rendered yaml: %w", err)
		}
		if len(raw) == 0 {
			continue
		}
		out = append(out, &unstructured.Unstructured{Object: raw})
	}
	return out, nil
}

// SortKey returns a deterministic ordering key for an object.
func SortKey(u *unstructured.Unstructured) string {
	return fmt.Sprintf("%s/%s/%s", u.GetKind(), u.GetNamespace(), u.GetName())
}

// Sort orders objects deterministically (stable tiebreaker for tests).
func Sort(objs []*unstructured.Unstructured) {
	sort.SliceStable(objs, func(i, j int) bool {
		return SortKey(objs[i]) < SortKey(objs[j])
	})
}
