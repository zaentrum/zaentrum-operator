package addon

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// AllowedKinds are the only kinds an addon chart may render, in the order the
// controller applies them: what pods consume first, workloads last. Addon UIs
// are reached through the portal proxy, so there is no Route or Ingress; and
// no RBAC, CRDs or cluster-scoped objects.
var AllowedKinds = []schema.GroupVersionKind{
	{Version: "v1", Kind: "ServiceAccount"},
	{Version: "v1", Kind: "Secret"},
	{Version: "v1", Kind: "ConfigMap"},
	{Version: "v1", Kind: "PersistentVolumeClaim"},
	{Version: "v1", Kind: "Service"},
	{Group: "apps", Version: "v1", Kind: "Deployment"},
	{Group: "batch", Version: "v1", Kind: "Job"},
}

// allowedVolumes are the volume sources a pod may use.
var allowedVolumes = map[string]bool{
	"configMap":             true,
	"secret":                true,
	"emptyDir":              true,
	"projected":             true,
	"downwardAPI":           true,
	"persistentVolumeClaim": true,
}

// Key identifies a rendered object in plans and violations: "Kind/name".
func Key(o *unstructured.Unstructured) string {
	return o.GetKind() + "/" + o.GetName()
}

// allowedKind returns the kind's GroupVersionKind, or false if it is not allowed.
func allowedKind(kind string) (schema.GroupVersionKind, bool) {
	for _, gvk := range AllowedKinds {
		if gvk.Kind == kind {
			return gvk, true
		}
	}
	return schema.GroupVersionKind{}, false
}

// ApplyOrder sorts objects into apply order (AllowedKinds), stable within a kind.
func ApplyOrder(objs []*unstructured.Unstructured) {
	rank := func(o *unstructured.Unstructured) int {
		for i, gvk := range AllowedKinds {
			if gvk.Kind == o.GetKind() {
				return i
			}
		}
		return len(AllowedKinds)
	}
	sort.SliceStable(objs, func(i, j int) bool { return rank(objs[i]) < rank(objs[j]) })
}

// GuardInput is what rendered objects are checked against.
type GuardInput struct {
	// Namespace is the addon's namespace; objects may name no other.
	Namespace string
	// MediaClaim is the platform media claim, which pods may mount besides
	// the claims the chart renders itself.
	MediaClaim string
	// Primary is the zaentrum.io/primary Service name; "" skips the check
	// (CheckConventions already reports the missing annotation).
	Primary string
	// Reserved are "Kind/name" keys the chart must not render: the addon's
	// generated values Secret and its valuesFrom objects.
	Reserved map[string]bool
}

// Violations checks every rendered object against the addon guardrails.
func Violations(objs []*unstructured.Unstructured, in GuardInput) []string {
	claims, accounts := map[string]bool{}, map[string]bool{}
	for _, o := range objs {
		switch o.GetKind() {
		case "PersistentVolumeClaim":
			claims[o.GetName()] = true
		case "ServiceAccount":
			accounts[o.GetName()] = true
		}
	}

	var out []string
	seen := map[string]bool{}
	for _, o := range objs {
		id := Key(o)
		if o.GetKind() == "" || o.GetName() == "" {
			out = append(out, id+": object without kind or metadata.name")
			continue
		}
		gvk, ok := allowedKind(o.GetKind())
		if !ok {
			out = append(out, id+": kind not allowed")
			continue
		}
		if o.GetAPIVersion() != gvk.GroupVersion().String() {
			out = append(out, fmt.Sprintf("%s: apiVersion %s not allowed (use %s)", id, o.GetAPIVersion(), gvk.GroupVersion()))
			continue
		}
		if seen[id] {
			out = append(out, id+": rendered more than once")
		}
		seen[id] = true
		if ns := o.GetNamespace(); ns != "" && ns != in.Namespace {
			out = append(out, fmt.Sprintf("%s: namespace %s not allowed (addons install into %s)", id, ns, in.Namespace))
		}
		if in.Reserved[id] {
			out = append(out, id+": name reserved for the addon's values")
		}
		if o.GetKind() == "Deployment" || o.GetKind() == "Job" {
			out = append(out, podViolations(o, id, claims, accounts, in.MediaClaim)...)
		}
	}
	return append(out, primaryViolations(objs, in.Primary)...)
}

func podViolations(o *unstructured.Unstructured, id string, claims, accounts map[string]bool, mediaClaim string) []string {
	var out []string
	add := func(format string, args ...interface{}) {
		out = append(out, id+": "+fmt.Sprintf(format, args...))
	}
	tmpl, _, err := unstructured.NestedMap(o.Object, "spec", "template")
	if err != nil {
		add("invalid pod template: %v", err)
		return out
	}
	var pod corev1.PodTemplateSpec
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(tmpl, &pod); err != nil {
		add("invalid pod template: %v", err)
		return out
	}
	spec := pod.Spec

	if spec.HostNetwork {
		add("hostNetwork not allowed")
	}
	if spec.HostPID {
		add("hostPID not allowed")
	}
	if spec.HostIPC {
		add("hostIPC not allowed")
	}
	if sc := spec.SecurityContext; sc != nil {
		if sc.RunAsUser != nil && *sc.RunAsUser == 0 {
			add("runAsUser 0 not allowed")
		}
		if sc.RunAsNonRoot != nil && !*sc.RunAsNonRoot {
			add("runAsNonRoot false not allowed")
		}
	}
	for _, sa := range []string{spec.ServiceAccountName, spec.DeprecatedServiceAccount} {
		if sa != "" && sa != "default" && !accounts[sa] {
			add("serviceAccountName %s not allowed (not rendered by the chart)", sa)
			break
		}
	}
	for _, v := range spec.Volumes {
		switch source := volumeSource(v); {
		case source == "persistentVolumeClaim":
			if claim := v.PersistentVolumeClaim.ClaimName; claim != mediaClaim && !claims[claim] {
				add("persistentVolumeClaim %s not allowed (not rendered by the chart)", claim)
			}
		case !allowedVolumes[source]:
			add("%s volume not allowed (%s)", source, v.Name)
		}
	}
	for _, c := range append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...) {
		out = append(out, containerViolations(id, c.Name, c.SecurityContext, c.Ports)...)
	}
	for _, c := range spec.EphemeralContainers {
		out = append(out, containerViolations(id, c.Name, c.SecurityContext, c.Ports)...)
	}
	return out
}

func containerViolations(id, name string, sc *corev1.SecurityContext, ports []corev1.ContainerPort) []string {
	var out []string
	add := func(format string, args ...interface{}) {
		out = append(out, fmt.Sprintf("%s: container %s: ", id, name)+fmt.Sprintf(format, args...))
	}
	if sc != nil {
		if sc.Privileged != nil && *sc.Privileged {
			add("privileged not allowed")
		}
		if sc.AllowPrivilegeEscalation != nil && *sc.AllowPrivilegeEscalation {
			add("allowPrivilegeEscalation not allowed")
		}
		if sc.Capabilities != nil && len(sc.Capabilities.Add) > 0 {
			caps := make([]string, 0, len(sc.Capabilities.Add))
			for _, c := range sc.Capabilities.Add {
				caps = append(caps, string(c))
			}
			add("added capabilities not allowed (%s)", strings.Join(caps, ", "))
		}
		if sc.RunAsUser != nil && *sc.RunAsUser == 0 {
			add("runAsUser 0 not allowed")
		}
		if sc.RunAsNonRoot != nil && !*sc.RunAsNonRoot {
			add("runAsNonRoot false not allowed")
		}
	}
	for _, p := range ports {
		if p.HostPort != 0 {
			add("hostPort %d not allowed", p.HostPort)
		}
	}
	return out
}

// volumeSource names the source a volume uses ("hostPath", "nfs", …). A volume
// without a source is an emptyDir, as the API server defaults it.
func volumeSource(v corev1.Volume) string {
	body, err := json.Marshal(v.VolumeSource)
	if err != nil {
		return "unknown"
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return "unknown"
	}
	if len(fields) == 0 {
		return "emptyDir"
	}
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, "+")
}

func primaryViolations(objs []*unstructured.Unstructured, primary string) []string {
	if primary == "" {
		return nil
	}
	for _, o := range objs {
		if o.GetKind() != "Service" || o.GetName() != primary {
			continue
		}
		ports, _, _ := unstructured.NestedSlice(o.Object, "spec", "ports")
		for _, p := range ports {
			if pm, ok := p.(map[string]interface{}); ok && toInt64(pm["port"]) == 80 {
				return nil
			}
		}
		return []string{"Service/" + primary + ": the primary Service must expose port 80"}
	}
	return []string{"Service/" + primary + ": primary Service (zaentrum.io/primary) is not rendered"}
}

// OwnerLookup reports whether an object of the rendered object's kind and name
// exists in the addon's namespace, and its controller reference if it has one.
type OwnerLookup func(obj *unstructured.Unstructured) (exists bool, controller *metav1.OwnerReference, err error)

// Collisions refuses rendered objects whose name is taken by an object this
// addon does not control — a platform object, another addon's, one made by
// hand. The operator never forces its way over those.
func Collisions(objs []*unstructured.Unstructured, addonUID types.UID, lookup OwnerLookup) ([]string, error) {
	var out []string
	for _, o := range objs {
		if _, ok := allowedKind(o.GetKind()); !ok || o.GetName() == "" {
			continue
		}
		exists, controller, err := lookup(o)
		if err != nil {
			return nil, err
		}
		if exists && (controller == nil || controller.UID != addonUID) {
			out = append(out, Key(o)+": already exists and is not owned by this addon")
		}
	}
	return out, nil
}

// Decoration is what the operator stamps on every object it applies.
type Decoration struct {
	// Name is the addon name.
	Name      string
	Namespace string
	// PartOf is the platform's zaentrum.partOf value.
	PartOf string
	// Checksum is the values checksum pod templates carry.
	Checksum string
}

// Decorate forces the namespace, sets the addon labels, stamps the values
// checksum on pod templates and defaults their security settings where the
// chart leaves them unset: runAsNonRoot, RuntimeDefault seccomp, no privilege
// escalation, all capabilities dropped. It changes objs in place.
func Decorate(objs []*unstructured.Unstructured, d Decoration) {
	for _, o := range objs {
		o.SetNamespace(d.Namespace)
		labels := o.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels[LabelAddon] = d.Name
		labels[LabelManagedBy] = ManagedBy
		labels[LabelInstance] = d.Name
		labels[LabelPartOf] = d.PartOf
		if o.GetKind() == "Deployment" {
			labels[LabelComponent] = o.GetName()
		}
		o.SetLabels(labels)

		if o.GetKind() != "Deployment" && o.GetKind() != "Job" {
			continue
		}
		if o.GetKind() == "Job" {
			// The operator applies what the chart renders on every pass: a
			// finished Job the TTL controller deleted would be created — and
			// run — again. Finished addon Jobs stay until the chart drops them.
			unstructured.RemoveNestedField(o.Object, "spec", "ttlSecondsAfterFinished")
		}
		tmpl := childMap(o.Object, "spec", "template")
		// Only new label keys on the pod template: a chart's selector must keep
		// matching the labels it chose.
		podLabels := childMap(tmpl, "metadata", "labels")
		podLabels[LabelAddon] = d.Name
		if o.GetKind() == "Deployment" {
			podLabels[LabelComponent] = o.GetName()
		}
		childMap(tmpl, "metadata", "annotations")[AnnotationValuesChecksum] = d.Checksum
		defaultPodSecurity(childMap(tmpl, "spec"))
	}
}

func defaultPodSecurity(spec map[string]interface{}) {
	pod := childMap(spec, "securityContext")
	setDefault(pod, "runAsNonRoot", true)
	setDefault(pod, "seccompProfile", map[string]interface{}{"type": "RuntimeDefault"})
	for _, key := range []string{"initContainers", "containers"} {
		list, _ := spec[key].([]interface{})
		for _, c := range list {
			container, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			sc := childMap(container, "securityContext")
			setDefault(sc, "allowPrivilegeEscalation", false)
			setDefault(childMap(sc, "capabilities"), "drop", []interface{}{"ALL"})
		}
	}
}

// childMap walks m along keys, creating (or replacing non-map) levels, and
// returns the innermost map — live, not a copy.
func childMap(m map[string]interface{}, keys ...string) map[string]interface{} {
	for _, k := range keys {
		next, ok := m[k].(map[string]interface{})
		if !ok {
			next = map[string]interface{}{}
			m[k] = next
		}
		m = next
	}
	return m
}

func setDefault(m map[string]interface{}, key string, v interface{}) {
	if cur, ok := m[key]; !ok || cur == nil {
		m[key] = v
	}
}

func toInt64(v interface{}) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case int32:
		return int64(n)
	case float64:
		return int64(n)
	}
	return 0
}
