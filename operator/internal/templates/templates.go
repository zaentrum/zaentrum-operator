// Package templates embeds the Stube platform manifests as Go text/templates
// and renders them for a given Stube CR.
//
// The templates are derived 1:1 from `kubectl kustomize deploy/base` (35
// objects) with two deliberate edits:
//
//  1. UN-kustomized names. Kustomize hashes ConfigMap/Secret names
//     (stube-env-bc6ctgt9bg, stube-db-k88fgb59hk, …). The operator owns the
//     full set and applies atomically, so we use STABLE names (stube-env,
//     stube-db, stube-stream-signing, stube-keycloak, stube-keycloak-admin)
//     and reference them by plain name from every pod.
//  2. CR-driven fields are parameterized: image tag -> {{.Version}}, the
//     issuer/host -> {{.Hostname}} / {{.Identity}}, media PVC size ->
//     {{.Storage.MediaSize}} (+ storageClassName), GPU overlay gated on
//     {{.Features.GPU}}, kafka resources gated on {{.Features.Kafka}}.
//
// Every boot fix from deploy/base is preserved verbatim: keycloak Service on
// :80, mgmt-port (:9000) /auth/health probes, wait-for-oidc initContainers,
// base64 stube-stream-signing key, the stube realm ConfigMap, and the
// CoreDNS-friendly http://<host>/auth/realms/stube issuer.
package templates

import (
	"bytes"
	"embed"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/template"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"

	stubev1alpha1 "github.com/zaentrum/stube/operator/api/v1alpha1"
)

//go:embed data/*.yaml
var fs embed.FS

// FieldManager is the server-side-apply field manager the operator owns.
const FieldManager = "stube-operator"

// Values is the flattened, defaulted view of a Stube spec handed to the
// templates. Defaulting happens in NewValues so templates stay branch-light.
type Values struct {
	Namespace string

	Version  string
	Hostname string

	// Issuer is the fully-resolved OIDC issuer URL (derived from Hostname in
	// bundled mode when spec.identity.issuer is empty).
	Issuer     string
	ClientID   string
	Audience   string
	Bundled    bool // identity.mode == bundled
	KCHostname string

	MediaSize string
	ClassName string // optional StorageClass; "" means cluster default
	HasClass  bool

	GPU   bool
	Kafka bool
}

// image returns a fully-qualified ghcr.io/zaentrum/<name> reference at the
// configured version. Exposed to templates as the `image` function.
func (v Values) image(name string) string {
	return fmt.Sprintf("ghcr.io/zaentrum/%s:%s", name, v.Version)
}

// NewValues flattens and defaults a Stube spec into template Values.
func NewValues(s *stubev1alpha1.Stube) Values {
	spec := s.Spec

	v := Values{
		Namespace: s.Namespace,
		Version:   orDefault(spec.Version, "latest"),
		Hostname:  orDefault(spec.Hostname, "stube.localhost"),
		ClientID:  orDefault(spec.Identity.ClientID, "chino-web"),
		Audience:  orDefault(spec.Identity.Audience, "chino"),
		Kafka:     spec.Features.Kafka,
		GPU:       spec.Features.GPU,
	}
	if v.Namespace == "" {
		v.Namespace = "stube"
	}

	mode := spec.Identity.Mode
	if mode == "" {
		mode = stubev1alpha1.IdentityBundled
	}
	v.Bundled = mode == stubev1alpha1.IdentityBundled

	// Issuer: explicit wins; otherwise derive from hostname in bundled mode.
	switch {
	case spec.Identity.Issuer != "":
		v.Issuer = spec.Identity.Issuer
	case v.Bundled:
		v.Issuer = fmt.Sprintf("http://%s/auth/realms/stube", v.Hostname)
	default:
		// external mode with no issuer is a misconfig; keep a derivable value
		// so rendering never panics — the controller surfaces the error.
		v.Issuer = fmt.Sprintf("http://%s/auth/realms/stube", v.Hostname)
	}
	// KC_HOSTNAME pins the bundled Keycloak's public base to <host>/auth so
	// the discovery doc issuer == OIDC_ISSUER == token iss.
	v.KCHostname = fmt.Sprintf("http://%s/auth", v.Hostname)

	mediaSize := "50Gi"
	if !spec.Storage.MediaSize.IsZero() {
		mediaSize = spec.Storage.MediaSize.String()
	}
	v.MediaSize = mediaSize
	if spec.Storage.ClassName != "" {
		v.ClassName = spec.Storage.ClassName
		v.HasClass = true
	}

	return v
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// templateNames lists the embedded manifest templates in apply-friendly order
// (namespace and RBAC first, data plane next, identity, then apps + ingress).
var templateNames = []string{
	"data/00-namespace.yaml",
	"data/01-rbac.yaml",
	"data/02-config.yaml",
	"data/03-secrets.yaml",
	"data/04-data.yaml",
	"data/05-kafka.yaml",
	"data/06-keycloak.yaml",
	"data/07-platform.yaml",
	"data/08-chino.yaml",
	"data/09-admin.yaml",
	"data/10-ingress.yaml",
}

// parse parses every embedded template with the supplied func map. The map
// must already contain `image` (bound to the concrete Values) because
// text/template resolves function names at parse time, not execute time.
func parse(funcs template.FuncMap) (*template.Template, error) {
	root := template.New("stube").Funcs(funcs)
	for _, name := range templateNames {
		b, err := fs.ReadFile(name)
		if err != nil {
			return nil, fmt.Errorf("read embedded template %s: %w", name, err)
		}
		if _, err := root.New(name).Parse(string(b)); err != nil {
			return nil, fmt.Errorf("parse template %s: %w", name, err)
		}
	}
	return root, nil
}

// Render renders all embedded templates with v and decodes the result into a
// slice of unstructured objects, ready for server-side apply. Empty documents
// (produced by feature gates that elide a whole template body) are skipped.
func Render(v Values) ([]*unstructured.Unstructured, error) {
	// `image` is bound to v and must be present at parse time.
	root, err := parse(template.FuncMap{
		"image": v.image,
	})
	if err != nil {
		return nil, err
	}

	var combined bytes.Buffer
	for _, name := range templateNames {
		var buf bytes.Buffer
		if err := root.ExecuteTemplate(&buf, name, v); err != nil {
			return nil, fmt.Errorf("execute template %s: %w", name, err)
		}
		if strings.TrimSpace(buf.String()) == "" {
			continue
		}
		combined.WriteString("\n---\n")
		combined.Write(buf.Bytes())
	}

	return decode(combined.Bytes())
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

// SortKey returns a deterministic ordering key for an object so apply ordering
// is stable across reconciles (cluster-scoped first, then by kind, then name).
func SortKey(u *unstructured.Unstructured) string {
	return fmt.Sprintf("%s/%s/%s", u.GetKind(), u.GetNamespace(), u.GetName())
}

// Sort orders objects deterministically (namespace/RBAC ahead of workloads is
// already enforced by template order; this is a stable tiebreaker for tests).
func Sort(objs []*unstructured.Unstructured) {
	sort.SliceStable(objs, func(i, j int) bool {
		return SortKey(objs[i]) < SortKey(objs[j])
	})
}
