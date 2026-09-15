package addon

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"helm.sh/helm/v3/pkg/chartutil"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
)

// DefaultValuesKey is the valuesFrom key read when none is set: a whole
// YAML/JSON values document.
const DefaultValuesKey = "values.yaml"

// ValuesSource is one valuesFrom entry with the object it names, as read.
type ValuesSource struct {
	Ref zaentrumv1alpha1.AddonValuesReference
	// Found reports whether the object exists.
	Found bool
	// Data is the object's data (a ConfigMap's binaryData included).
	Data map[string][]byte
}

// UserValues merges spec.values and then the valuesFrom sources in order: the
// values a user supplied. Problems come back as values errors; a source with a
// problem contributes nothing.
func UserValues(values *apiextensionsv1.JSON, from []ValuesSource) (map[string]interface{}, []string) {
	out := map[string]interface{}{}
	var errs []string
	if values != nil && len(values.Raw) > 0 {
		var m map[string]interface{}
		if err := json.Unmarshal(values.Raw, &m); err != nil {
			errs = append(errs, fmt.Sprintf("spec.values must be an object: %v", err))
		} else {
			mergeInto(out, m)
		}
	}
	for _, src := range from {
		if err := applySource(out, src); err != nil {
			errs = append(errs, err.Error())
		}
	}
	return out, errs
}

func applySource(dst map[string]interface{}, src ValuesSource) error {
	ref := src.Ref
	key := ref.ValuesKey
	if key == "" {
		key = DefaultValuesKey
	}
	what := fmt.Sprintf("valuesFrom %s/%s", ref.Kind, ref.Name)
	if !src.Found {
		if ref.Optional {
			return nil
		}
		return fmt.Errorf("%s: not found", what)
	}
	raw, ok := src.Data[key]
	if !ok {
		if ref.Optional {
			return nil
		}
		return fmt.Errorf("%s: key %s not found", what, key)
	}
	if ref.TargetPath != "" {
		path, err := SplitPath(ref.TargetPath)
		if err != nil {
			return fmt.Errorf("%s: targetPath: %w", what, err)
		}
		SetPath(dst, path, string(raw))
		return nil
	}
	vals, err := chartutil.ReadValues(raw)
	if err != nil {
		return fmt.Errorf("%s: key %s is not a YAML/JSON values document: %v", what, key, err)
	}
	mergeInto(dst, vals)
	return nil
}

// LayerValues stacks an addon's values in precedence order: the generated
// values (dotted path → value) at the bottom, the user values over them, and
// the reserved platform block on top — replacing whatever a user set there.
func LayerValues(generated map[string]string, user, platform map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	paths := make([]string, 0, len(generated))
	for p := range generated {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if keys, err := SplitPath(p); err == nil {
			SetPath(out, keys, generated[p])
		}
	}
	mergeInto(out, copyMap(user))
	out[PlatformKey] = copyMap(platform)
	return out
}

// copyMap deep-copies the maps and slices of a values tree; leaves are shared.
func copyMap(m map[string]interface{}) map[string]interface{} {
	if m == nil {
		return nil
	}
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		out[k] = copyValue(v)
	}
	return out
}

func copyValue(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		return copyMap(t)
	case []interface{}:
		out := make([]interface{}, len(t))
		for i := range t {
			out[i] = copyValue(t[i])
		}
		return out
	default:
		return v
	}
}

// mergeInto merges src over dst: maps merge key by key, anything else replaces.
func mergeInto(dst, src map[string]interface{}) {
	for k, v := range src {
		sm, ok := v.(map[string]interface{})
		if !ok {
			dst[k] = v
			continue
		}
		dm, ok := dst[k].(map[string]interface{})
		if !ok {
			dm = map[string]interface{}{}
			dst[k] = dm
		}
		mergeInto(dm, sm)
	}
}

// SplitPath splits a dotted values path ("database.url") into its keys.
func SplitPath(p string) ([]string, error) {
	keys := strings.Split(p, ".")
	for _, k := range keys {
		if k == "" {
			return nil, fmt.Errorf("%q is not a dotted path of keys", p)
		}
	}
	return keys, nil
}

// SetPath sets the value at keys, creating parents and replacing any parent
// that is not a map.
func SetPath(m map[string]interface{}, keys []string, v interface{}) {
	for _, k := range keys[:len(keys)-1] {
		next, ok := m[k].(map[string]interface{})
		if !ok {
			next = map[string]interface{}{}
			m[k] = next
		}
		m = next
	}
	m[keys[len(keys)-1]] = v
}

// GetPath returns the value at keys.
func GetPath(m map[string]interface{}, keys []string) (interface{}, bool) {
	var cur interface{} = m
	for _, k := range keys {
		next, ok := cur.(map[string]interface{})
		if !ok {
			return nil, false
		}
		if cur, ok = next[k]; !ok {
			return nil, false
		}
	}
	return cur, true
}
