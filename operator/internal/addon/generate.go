package addon

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Generators accepted in a values.schema.json x-zaentrum-generate: 32 random
// bytes, base64- or hex-encoded.
const (
	GenerateRandomBase64 = "random-base64-32"
	GenerateRandomHex    = "random-hex-32"

	schemaGenerateKey = "x-zaentrum-generate"
)

// GeneratedField is a values.schema.json property the operator generates.
type GeneratedField struct {
	// Path is the dotted values path, e.g. "config.key".
	Path      string
	Generator string
}

// GeneratedFields returns every property carrying x-zaentrum-generate, nested
// properties included, sorted by path.
func GeneratedFields(schema []byte) ([]GeneratedField, error) {
	if len(bytes.TrimSpace(schema)) == 0 {
		return nil, nil
	}
	var root map[string]interface{}
	if err := json.Unmarshal(schema, &root); err != nil {
		return nil, fmt.Errorf("values.schema.json: %w", err)
	}
	var out []GeneratedField
	collectGenerated(root, "", &out)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func collectGenerated(node map[string]interface{}, prefix string, out *[]GeneratedField) {
	props, _ := node["properties"].(map[string]interface{})
	for name, raw := range props {
		child, ok := raw.(map[string]interface{})
		// A key containing a dot cannot be addressed by a dotted path.
		if !ok || name == "" || strings.Contains(name, ".") {
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		if gen, ok := child[schemaGenerateKey].(string); ok {
			*out = append(*out, GeneratedField{Path: path, Generator: gen})
		}
		collectGenerated(child, path, out)
	}
}

// GenerateMissing generates a value for every field that has no value from a
// user source and no key in the generated Secret yet. Existing keys are never
// generated again. An unknown generator is a values error.
func GenerateMissing(fields []GeneratedField, user map[string]interface{}, existing map[string][]byte) (map[string]string, []string, error) {
	generated := map[string]string{}
	var errs []string
	for _, f := range fields {
		if f.Generator != GenerateRandomBase64 && f.Generator != GenerateRandomHex {
			errs = append(errs, fmt.Sprintf("%s: unknown x-zaentrum-generate %q (use %s or %s)",
				f.Path, f.Generator, GenerateRandomBase64, GenerateRandomHex))
			continue
		}
		if _, ok := existing[f.Path]; ok {
			continue
		}
		keys, err := SplitPath(f.Path)
		if err != nil {
			continue
		}
		if v, ok := GetPath(user, keys); ok && v != nil && v != "" {
			continue
		}
		v, err := generate(f.Generator)
		if err != nil {
			return nil, nil, err
		}
		generated[f.Path] = v
	}
	return generated, errs, nil
}

// GeneratedValues picks the generated Secret's values for the fields the
// schema declares now. Keys of fields a chart has since dropped stay stored,
// but are not injected: a stricter schema could refuse them.
func GeneratedValues(fields []GeneratedField, data map[string][]byte) map[string]string {
	out := map[string]string{}
	for _, f := range fields {
		if v, ok := data[f.Path]; ok {
			out[f.Path] = string(v)
		}
	}
	return out
}

func generate(generator string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate value: %w", err)
	}
	if generator == GenerateRandomHex {
		return hex.EncodeToString(b), nil
	}
	return base64.StdEncoding.EncodeToString(b), nil
}
