package addon

import (
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
)

// Summarize lists the rendered objects and workloads for the plan, sorted.
func Summarize(objs []*unstructured.Unstructured) ([]zaentrumv1alpha1.AddonObject, []zaentrumv1alpha1.AddonWorkload) {
	objects := make([]zaentrumv1alpha1.AddonObject, 0, len(objs))
	var workloads []zaentrumv1alpha1.AddonWorkload
	for _, o := range objs {
		objects = append(objects, zaentrumv1alpha1.AddonObject{Kind: o.GetKind(), Name: o.GetName()})
		if o.GetKind() != "Deployment" && o.GetKind() != "Job" {
			continue
		}
		w := zaentrumv1alpha1.AddonWorkload{Kind: o.GetKind(), Name: o.GetName()}
		for _, c := range containersOf(o) {
			if image, _ := c["image"].(string); image != "" && !contains(w.Images, image) {
				w.Images = append(w.Images, image)
			}
			ports, _ := c["ports"].([]interface{})
			for _, p := range ports {
				if pm, ok := p.(map[string]interface{}); ok {
					if port := toInt64(pm["containerPort"]); port > 0 {
						w.Ports = append(w.Ports, int32(port))
					}
				}
			}
		}
		workloads = append(workloads, w)
	}
	sort.SliceStable(objects, func(i, j int) bool {
		return objects[i].Kind+"/"+objects[i].Name < objects[j].Kind+"/"+objects[j].Name
	})
	sort.SliceStable(workloads, func(i, j int) bool {
		return workloads[i].Kind+"/"+workloads[i].Name < workloads[j].Kind+"/"+workloads[j].Name
	})
	return objects, workloads
}

// containersOf returns an object's init containers and containers, live.
func containersOf(o *unstructured.Unstructured) []map[string]interface{} {
	spec, _ := o.Object["spec"].(map[string]interface{})
	tmpl, _ := spec["template"].(map[string]interface{})
	pod, _ := tmpl["spec"].(map[string]interface{})
	var out []map[string]interface{}
	for _, key := range []string{"initContainers", "containers"} {
		list, _ := pod[key].([]interface{})
		for _, c := range list {
			if cm, ok := c.(map[string]interface{}); ok {
				out = append(out, cm)
			}
		}
	}
	return out
}

// LiveObject is an object currently applied for an addon.
type LiveObject struct {
	Kind string
	Name string
	// Images maps container name to image, for workloads.
	Images map[string]string
}

// Live summarises an applied object read back from the cluster.
func Live(o *unstructured.Unstructured) LiveObject {
	l := LiveObject{Kind: o.GetKind(), Name: o.GetName()}
	for _, c := range containersOf(o) {
		name, _ := c["name"].(string)
		image, _ := c["image"].(string)
		if l.Images == nil {
			l.Images = map[string]string{}
		}
		l.Images[name] = image
	}
	return l
}

// Changes compares the rendered objects with the applied ones. Nothing applied
// yet means every rendered object is added.
func Changes(objs []*unstructured.Unstructured, live []LiveObject) *zaentrumv1alpha1.AddonPlanChanges {
	applied := make(map[string]LiveObject, len(live))
	for _, l := range live {
		applied[l.Kind+"/"+l.Name] = l
	}
	rendered := make(map[string]bool, len(objs))
	changes := &zaentrumv1alpha1.AddonPlanChanges{}
	for _, o := range objs {
		id := Key(o)
		rendered[id] = true
		l, ok := applied[id]
		if !ok {
			changes.Added = append(changes.Added, id)
			continue
		}
		for _, c := range containersOf(o) {
			name, _ := c["name"].(string)
			image, _ := c["image"].(string)
			if old, ok := l.Images[name]; ok && old != image {
				changes.Images = append(changes.Images, fmt.Sprintf("%s: %s → %s", id, old, image))
			}
		}
	}
	for _, l := range live {
		if id := l.Kind + "/" + l.Name; !rendered[id] {
			changes.Removed = append(changes.Removed, id)
		}
	}
	sort.Strings(changes.Added)
	sort.Strings(changes.Removed)
	sort.Strings(changes.Images)
	return changes
}

// Prune returns the applied objects a new render no longer contains.
func Prune(objs []*unstructured.Unstructured, live []LiveObject) []LiveObject {
	rendered := make(map[string]bool, len(objs))
	for _, o := range objs {
		rendered[Key(o)] = true
	}
	var out []LiveObject
	for _, l := range live {
		if !rendered[l.Kind+"/"+l.Name] {
			out = append(out, l)
		}
	}
	return out
}
