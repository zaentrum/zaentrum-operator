package addon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// maxSchemaSize bounds the values.schema.json the plan carries in status.
const maxSchemaSize = 256 << 10

// RenderInput is one addon render.
type RenderInput struct {
	Chart *chart.Chart
	// Name is the release name: the ZaentrumAddon's name.
	Name string
	// Namespace is the ZaentrumAddon's namespace.
	Namespace string
	// Values are the layered values (LayerValues).
	Values map[string]interface{}
}

// Rendered is a successful render.
type Rendered struct {
	// Objects in template order: files by name, documents in file order.
	Objects []*unstructured.Unstructured
	// Checksum is the sha256 of the final values, chart defaults included.
	Checksum string
}

// Render renders the chart in memory with the Helm engine — client-only: no
// release, no cluster lookups (lookup returns nothing). Schema validation and
// template failures come back as values errors.
func Render(in RenderInput) (out *Rendered, valuesErrors []string) {
	// Helm's value coalescing and schema walk can panic on values shaped
	// against the chart's expectations (a subchart's key set to a string);
	// that is a values error, not a reason to crash the reconcile.
	defer func() {
		if r := recover(); r != nil {
			out, valuesErrors = nil, []string{fmt.Sprintf("render: %v", r)}
		}
	}()

	chrt := in.Chart
	vals := copyMap(in.Values)
	if err := chartutil.ProcessDependenciesWithMerge(chrt, vals); err != nil {
		return nil, []string{fmt.Sprintf("chart dependencies: %v", err)}
	}
	final, err := chartutil.CoalesceValues(chrt, vals)
	if err != nil {
		return nil, []string{fmt.Sprintf("values: %v", err)}
	}
	// The reserved block overrides the chart's own defaults too.
	if platform, ok := in.Values[PlatformKey]; ok {
		final[PlatformKey] = platform
	}
	if err := chartutil.ValidateAgainstSchema(chrt, final); err != nil {
		return nil, schemaErrors(chrt.Name(), err)
	}

	top := chartutil.Values{
		"Chart":        chrt.Metadata,
		"Capabilities": chartutil.DefaultCapabilities,
		"Release": map[string]interface{}{
			"Name":      in.Name,
			"Namespace": in.Namespace,
			"IsUpgrade": false,
			"IsInstall": true,
			"Revision":  1,
			"Service":   "Helm",
		},
		"Values": final,
	}
	files, err := engine.Render(chrt, top)
	if err != nil {
		return nil, []string{err.Error()}
	}
	objs, errs := decodeRendered(files)
	if len(errs) > 0 {
		return nil, errs
	}
	body, err := json.Marshal(final)
	if err != nil {
		return nil, []string{fmt.Sprintf("values: %v", err)}
	}
	sum := sha256.Sum256(body)
	return &Rendered{Objects: objs, Checksum: hex.EncodeToString(sum[:])}, nil
}

// SchemaForStatus returns the raw values.schema.json for the plan, or a values
// error when it is too large to carry in the resource's status.
func SchemaForStatus(chrt *chart.Chart) (string, []string) {
	if len(chrt.Schema) > maxSchemaSize {
		return "", []string{fmt.Sprintf("values.schema.json is larger than %d KiB", maxSchemaSize>>10)}
	}
	return string(chrt.Schema), nil
}

// schemaErrors turns Helm's schema report ("chart:\n- error\n…", one section
// per chart) into one entry per error, prefixed with the subchart it is about.
func schemaErrors(chartName string, err error) []string {
	var out []string
	current := chartName
	for _, line := range strings.Split(err.Error(), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
		case strings.HasPrefix(line, "- "):
			msg := strings.TrimPrefix(line, "- ")
			if current != chartName {
				msg = current + ": " + msg
			}
			out = append(out, msg)
		case strings.HasSuffix(line, ":") && !strings.Contains(line, " "):
			current = strings.TrimSuffix(line, ":")
		default:
			out = append(out, line)
		}
	}
	return out
}

// decodeRendered splits the rendered files into objects, skipping notes, empty
// documents and test hooks (`helm test` pods are never installed).
func decodeRendered(files map[string]string) ([]*unstructured.Unstructured, []string) {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	var objs []*unstructured.Unstructured
	var errs []string
	for _, name := range names {
		body := files[name]
		if strings.HasSuffix(name, "NOTES.txt") || strings.TrimSpace(body) == "" {
			continue
		}
		dec := yaml.NewYAMLOrJSONDecoder(strings.NewReader(body), 4096)
		for {
			raw := map[string]interface{}{}
			err := dec.Decode(&raw)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", name, err))
				break
			}
			if len(raw) == 0 {
				continue
			}
			obj := &unstructured.Unstructured{Object: raw}
			if isTestHook(obj) {
				continue
			}
			objs = append(objs, obj)
		}
	}
	return objs, errs
}

func isTestHook(obj *unstructured.Unstructured) bool {
	for _, hook := range strings.Split(obj.GetAnnotations()["helm.sh/hook"], ",") {
		if strings.HasPrefix(strings.TrimSpace(hook), "test") {
			return true
		}
	}
	return false
}
