package digest

import (
	"context"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Match decides whether an image reference should be digest-pinned. The operator
// only pins its OWN moving tags; third-party images (postgres, valkey, keycloak)
// are left alone — their registries and auth are not ours to speak for, and they
// are already pinned to explicit versions in the chart.
type Match func(Ref) bool

// ZaentrumImages matches ghcr.io/zaentrum/* references on a moving tag. A
// reference already carrying a digest is skipped by the caller.
func ZaentrumImages(ref Ref) bool {
	return ref.Registry == "ghcr.io" && strings.HasPrefix(ref.Repo, "zaentrum/")
}

// PinImages rewrites every container/initContainer image in the rendered objects
// that Match accepts, replacing its tag with the current digest. It never fails
// the reconcile: a reference it cannot resolve keeps its tag and the failure is
// returned so the caller can log it. Returns the number of images pinned and any
// per-image errors.
func (rv *Resolver) PinImages(ctx context.Context, objs []*unstructured.Unstructured, match Match) (int, []error) {
	var pinned int
	var errs []error
	for _, obj := range objs {
		containers, ok := podContainers(obj)
		if !ok {
			continue
		}
		for _, list := range containers {
			for _, c := range list {
				cm, ok := c.(map[string]any)
				if !ok {
					continue
				}
				image, _ := cm["image"].(string)
				if image == "" {
					continue
				}
				ref := Parse(image)
				if ref.Pinned() || !match(ref) {
					continue
				}
				resolved, err := rv.Resolve(ctx, image)
				if err != nil {
					errs = append(errs, err)
					continue // keep the tag; better a stale image than a failed apply
				}
				if resolved != image {
					cm["image"] = resolved
					pinned++
				}
			}
		}
	}
	return pinned, errs
}

// podContainers returns the LIVE containers + initContainers slices from any
// object carrying a pod template (Deployment, StatefulSet, Job, …), or ok=false.
// It walks the raw maps directly rather than via unstructured.NestedMap, which
// deep-copies — a copy would make the image rewrite a no-op.
func podContainers(obj *unstructured.Unstructured) ([][]any, bool) {
	spec := mapAt(mapAt(mapAt(obj.Object, "spec"), "template"), "spec")
	if spec == nil {
		return nil, false
	}
	var out [][]any
	for _, key := range []string{"containers", "initContainers"} {
		if raw, ok := spec[key].([]any); ok && len(raw) > 0 {
			out = append(out, raw)
		}
	}
	return out, len(out) > 0
}

func mapAt(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	sub, _ := m[key].(map[string]any)
	return sub
}
