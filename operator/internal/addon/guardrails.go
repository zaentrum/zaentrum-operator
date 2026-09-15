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

// allowedSecretTypes are the Secret types an addon chart may render. The point
// is to refuse kubernetes.io/service-account-token: the token controller mints
// a real token for the named ServiceAccount into such a Secret, so a chart
// could mint a platform SA's token into an addon-owned, addon-mounted Secret.
var allowedSecretTypes = map[string]bool{
	"":                              true,
	string(corev1.SecretTypeOpaque): true, // Opaque
	string(corev1.SecretTypeTLS):    true, // kubernetes.io/tls
	string(corev1.SecretTypeDockerConfigJson): true, // kubernetes.io/dockerconfigjson
	string(corev1.SecretTypeBasicAuth):        true, // kubernetes.io/basic-auth
	string(corev1.SecretTypeSSHAuth):          true, // kubernetes.io/ssh-auth
}

// saAnnotations bind a Secret (or any object) to a ServiceAccount by name/uid;
// refused everywhere so a chart cannot point one at a platform SA.
var saAnnotations = []string{
	"kubernetes.io/service-account.name",
	"kubernetes.io/service-account.uid",
}

// controlPlaneNodeRoles are the node-role labels an addon may not target with
// scheduling (nodeSelector/affinity/tolerations) — running on the control
// plane escapes the isolation addons are meant to have.
var controlPlaneNodeRoles = map[string]bool{
	"node-role.kubernetes.io/control-plane": true,
	"node-role.kubernetes.io/master":        true,
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
	// EventsTLSSecret is zaentrum.events.tlsSecret: a platform Secret a pod may
	// reference even though the chart does not render it ("" = none).
	EventsTLSSecret string
	// PullSecrets is zaentrum.imagePullSecrets: platform Secrets a pod may use
	// as imagePullSecrets, and reference, without rendering them.
	PullSecrets []string
}

// Violations checks every rendered object against the addon guardrails.
func Violations(objs []*unstructured.Unstructured, in GuardInput) []string {
	claims, accounts := map[string]bool{}, map[string]bool{}
	renderedSecrets, renderedConfigMaps := map[string]bool{}, map[string]bool{}
	for _, o := range objs {
		switch o.GetKind() {
		case "PersistentVolumeClaim":
			claims[o.GetName()] = true
		case "ServiceAccount":
			accounts[o.GetName()] = true
		case "Secret":
			renderedSecrets[o.GetName()] = true
		case "ConfigMap":
			renderedConfigMaps[o.GetName()] = true
		}
	}
	// Secrets a pod may reference: those the chart renders, plus the ones the
	// platform hands the addon in the zaentrum values.
	mountSecrets := copySet(renderedSecrets)
	if in.EventsTLSSecret != "" {
		mountSecrets[in.EventsTLSSecret] = true
	}
	pullSecrets := copySet(renderedSecrets)
	for _, s := range in.PullSecrets {
		mountSecrets[s] = true
		pullSecrets[s] = true
	}
	refs := podRefs{
		claims:       claims,
		accounts:     accounts,
		mediaClaim:   in.MediaClaim,
		mountSecrets: mountSecrets,
		configMaps:   renderedConfigMaps,
		pullSecrets:  pullSecrets,
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
		switch {
		case strings.HasPrefix(o.GetName(), ReservedNamePrefix):
			out = append(out, id+": name prefix "+ReservedNamePrefix+" is reserved for addon values")
		case in.Reserved[id]:
			out = append(out, id+": name reserved for the addon's values")
		}
		out = append(out, metadataViolations(o, id)...)
		switch o.GetKind() {
		case "Secret":
			out = append(out, secretViolations(o, id)...)
		case "Service":
			out = append(out, serviceViolations(o, id)...)
		case "PersistentVolumeClaim":
			out = append(out, pvcViolations(o, id)...)
		case "Deployment", "Job":
			out = append(out, podViolations(o, id, refs)...)
		}
	}
	return append(out, primaryViolations(objs, in.Primary)...)
}

// metadataViolations refuses object metadata that reaches beyond the addon: a
// chart-supplied ownerReference (would GC-adopt an arbitrary parent, or dodge
// the operator's own owner reference) and the ServiceAccount-token binding
// annotations (would target a platform ServiceAccount).
func metadataViolations(o *unstructured.Unstructured, id string) []string {
	var out []string
	if len(o.GetOwnerReferences()) > 0 {
		out = append(out, id+": metadata.ownerReferences not allowed (the operator sets the owner)")
	}
	ann := o.GetAnnotations()
	for _, key := range saAnnotations {
		if _, ok := ann[key]; ok {
			out = append(out, fmt.Sprintf("%s: annotation %s not allowed", id, key))
		}
	}
	return out
}

// secretViolations refuses a Secret type the token controller acts on
// (service-account-token) and anything not in the small allow-list.
func secretViolations(o *unstructured.Unstructured, id string) []string {
	t, _, _ := unstructured.NestedString(o.Object, "type")
	if !allowedSecretTypes[t] {
		return []string{fmt.Sprintf("%s: Secret type %s not allowed", id, t)}
	}
	return nil
}

// serviceViolations refuses Service fields that reach off-namespace or
// cluster-wide: a type other than ClusterIP, externalIPs (CVE-2020-8554),
// externalName and the load-balancer knobs.
func serviceViolations(o *unstructured.Unstructured, id string) []string {
	var out []string
	if t, _, _ := unstructured.NestedString(o.Object, "spec", "type"); t != "" && t != string(corev1.ServiceTypeClusterIP) {
		out = append(out, fmt.Sprintf("%s: Service type %s not allowed (ClusterIP only)", id, t))
	}
	for _, field := range []string{"externalIPs", "loadBalancerSourceRanges"} {
		if v, ok, _ := unstructured.NestedSlice(o.Object, "spec", field); ok && len(v) > 0 {
			out = append(out, fmt.Sprintf("%s: Service spec.%s not allowed", id, field))
		}
	}
	for _, field := range []string{"externalName", "loadBalancerIP"} {
		if v, ok, _ := unstructured.NestedString(o.Object, "spec", field); ok && v != "" {
			out = append(out, fmt.Sprintf("%s: Service spec.%s not allowed", id, field))
		}
	}
	return out
}

// pvcViolations refuses a PVC that binds a specific existing PV (volumeName) or
// clones another volume (dataSource / dataSourceRef).
func pvcViolations(o *unstructured.Unstructured, id string) []string {
	var out []string
	if v, _, _ := unstructured.NestedString(o.Object, "spec", "volumeName"); v != "" {
		out = append(out, id+": PVC spec.volumeName not allowed (binds a specific PV)")
	}
	for _, field := range []string{"dataSource", "dataSourceRef"} {
		if v, ok, _ := unstructured.NestedMap(o.Object, "spec", field); ok && len(v) > 0 {
			out = append(out, fmt.Sprintf("%s: PVC spec.%s not allowed", id, field))
		}
	}
	return out
}

// podRefs holds the object names a pod may reference, resolved from the render
// and the platform values.
type podRefs struct {
	claims       map[string]bool
	accounts     map[string]bool
	mediaClaim   string
	mountSecrets map[string]bool // secrets a pod may mount/read (rendered + tlsSecret + pull)
	configMaps   map[string]bool // configmaps a pod may reference (rendered only)
	pullSecrets  map[string]bool // secrets valid as imagePullSecrets (rendered + pull)
}

func podViolations(o *unstructured.Unstructured, id string, refs podRefs) []string {
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
	if spec.PriorityClassName != "" {
		add("priorityClassName not allowed")
	}
	if spec.NodeName != "" {
		add("nodeName not allowed")
	}
	for key := range spec.NodeSelector {
		if controlPlaneNodeRoles[key] {
			add("nodeSelector %s not allowed (control-plane placement)", key)
		}
	}
	out = append(out, tolerationViolations(id, spec.Tolerations)...)
	out = append(out, affinityViolations(id, spec.Affinity)...)
	out = append(out, podSecurityViolations(id, spec.SecurityContext)...)

	if sc := spec.SecurityContext; sc != nil {
		if sc.RunAsUser != nil && *sc.RunAsUser == 0 {
			add("runAsUser 0 not allowed")
		}
		if sc.RunAsNonRoot != nil && !*sc.RunAsNonRoot {
			add("runAsNonRoot false not allowed")
		}
	}
	for _, sa := range []string{spec.ServiceAccountName, spec.DeprecatedServiceAccount} {
		if sa != "" && sa != "default" && !refs.accounts[sa] {
			add("serviceAccountName %s not allowed (not rendered by the chart)", sa)
			break
		}
	}
	for _, ips := range spec.ImagePullSecrets {
		if !refs.pullSecrets[ips.Name] {
			add("imagePullSecrets %s not allowed (not rendered by the chart or a platform pull secret)", ips.Name)
		}
	}
	for _, v := range spec.Volumes {
		out = append(out, volumeViolations(id, v, refs)...)
	}
	all := append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...)
	for _, c := range all {
		out = append(out, containerViolations(id, c.Name, c.SecurityContext, c.Ports)...)
		out = append(out, containerRefViolations(id, c.Name, c.Env, c.EnvFrom, refs)...)
	}
	for _, c := range spec.EphemeralContainers {
		out = append(out, containerViolations(id, c.Name, c.SecurityContext, c.Ports)...)
		out = append(out, containerRefViolations(id, c.Name, c.Env, c.EnvFrom, refs)...)
	}
	return out
}

// volumeViolations checks one volume's source and the objects it names.
func volumeViolations(id string, v corev1.Volume, refs podRefs) []string {
	add := func(format string, args ...interface{}) []string {
		return []string{id + ": " + fmt.Sprintf(format, args...)}
	}
	switch source := volumeSource(v); source {
	case "persistentVolumeClaim":
		if claim := v.PersistentVolumeClaim.ClaimName; claim != refs.mediaClaim && !refs.claims[claim] {
			return add("persistentVolumeClaim %s not allowed (not rendered by the chart)", claim)
		}
	case "secret":
		if name := v.Secret.SecretName; !refs.mountSecrets[name] {
			return add("secret volume %s references %s, not rendered by the chart", v.Name, name)
		}
	case "configMap":
		if name := v.ConfigMap.Name; !refs.configMaps[name] {
			return add("configMap volume %s references %s, not rendered by the chart", v.Name, name)
		}
	case "projected":
		var out []string
		if v.Projected != nil {
			for _, s := range v.Projected.Sources {
				if s.Secret != nil && !refs.mountSecrets[s.Secret.Name] {
					out = append(out, id+": "+fmt.Sprintf("projected volume %s references secret %s, not rendered by the chart", v.Name, s.Secret.Name))
				}
				if s.ConfigMap != nil && !refs.configMaps[s.ConfigMap.Name] {
					out = append(out, id+": "+fmt.Sprintf("projected volume %s references configMap %s, not rendered by the chart", v.Name, s.ConfigMap.Name))
				}
			}
		}
		return out
	default:
		if !allowedVolumes[source] {
			return add("%s volume not allowed (%s)", source, v.Name)
		}
	}
	return nil
}

// containerRefViolations refuses env / envFrom references to Secrets and
// ConfigMaps the chart does not render (and the platform did not hand over).
func containerRefViolations(id, name string, env []corev1.EnvVar, envFrom []corev1.EnvFromSource, refs podRefs) []string {
	var out []string
	add := func(format string, args ...interface{}) {
		out = append(out, fmt.Sprintf("%s: container %s: ", id, name)+fmt.Sprintf(format, args...))
	}
	for _, e := range env {
		if e.ValueFrom == nil {
			continue
		}
		if r := e.ValueFrom.SecretKeyRef; r != nil && !refs.mountSecrets[r.Name] {
			add("env %s references secret %s, not rendered by the chart", e.Name, r.Name)
		}
		if r := e.ValueFrom.ConfigMapKeyRef; r != nil && !refs.configMaps[r.Name] {
			add("env %s references configMap %s, not rendered by the chart", e.Name, r.Name)
		}
	}
	for _, e := range envFrom {
		if r := e.SecretRef; r != nil && !refs.mountSecrets[r.Name] {
			add("envFrom references secret %s, not rendered by the chart", r.Name)
		}
		if r := e.ConfigMapRef; r != nil && !refs.configMaps[r.Name] {
			add("envFrom references configMap %s, not rendered by the chart", r.Name)
		}
	}
	return out
}

// tolerationViolations refuses tolerations that let a pod schedule onto the
// control plane: one keyed to a control-plane node-role taint, or a blanket
// toleration (empty key + Exists) that matches every taint.
func tolerationViolations(id string, tols []corev1.Toleration) []string {
	var out []string
	for _, t := range tols {
		if controlPlaneNodeRoles[t.Key] || (t.Key == "" && t.Operator == corev1.TolerationOpExists) {
			out = append(out, id+": toleration for control-plane taints not allowed")
		}
	}
	return out
}

// affinityViolations refuses nodeAffinity terms selecting control-plane nodes.
func affinityViolations(id string, aff *corev1.Affinity) []string {
	if aff == nil || aff.NodeAffinity == nil {
		return nil
	}
	na := aff.NodeAffinity
	var terms []corev1.NodeSelectorTerm
	if req := na.RequiredDuringSchedulingIgnoredDuringExecution; req != nil {
		terms = append(terms, req.NodeSelectorTerms...)
	}
	for _, p := range na.PreferredDuringSchedulingIgnoredDuringExecution {
		terms = append(terms, p.Preference)
	}
	for _, term := range terms {
		for _, expr := range append(append([]corev1.NodeSelectorRequirement{}, term.MatchExpressions...), term.MatchFields...) {
			if controlPlaneNodeRoles[expr.Key] {
				return []string{id + ": nodeAffinity for control-plane nodes not allowed"}
			}
		}
	}
	return nil
}

// podSecurityViolations checks the pod-level security context extras.
func podSecurityViolations(id string, sc *corev1.PodSecurityContext) []string {
	if sc == nil {
		return nil
	}
	var out []string
	add := func(msg string) { out = append(out, id+": "+msg) }
	if sc.SELinuxOptions != nil && sc.SELinuxOptions.Type != "" {
		add("seLinuxOptions.type not allowed")
	}
	if len(sc.Sysctls) > 0 {
		add("sysctls not allowed")
	}
	if sc.AppArmorProfile != nil && sc.AppArmorProfile.Type == corev1.AppArmorProfileTypeUnconfined {
		add("appArmorProfile Unconfined not allowed")
	}
	if sc.WindowsOptions != nil && sc.WindowsOptions.HostProcess != nil && *sc.WindowsOptions.HostProcess {
		add("windowsOptions.hostProcess not allowed")
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
		if sc.ProcMount != nil && *sc.ProcMount == corev1.UnmaskedProcMount {
			add("procMount Unmasked not allowed")
		}
		if sc.SELinuxOptions != nil && sc.SELinuxOptions.Type != "" {
			add("seLinuxOptions.type not allowed")
		}
		if sc.AppArmorProfile != nil && sc.AppArmorProfile.Type == corev1.AppArmorProfileTypeUnconfined {
			add("appArmorProfile Unconfined not allowed")
		}
		if sc.WindowsOptions != nil && sc.WindowsOptions.HostProcess != nil && *sc.WindowsOptions.HostProcess {
			add("windowsOptions.hostProcess not allowed")
		}
	}
	for _, p := range ports {
		if p.HostPort != 0 {
			add("hostPort %d not allowed", p.HostPort)
		}
	}
	return out
}

func copySet(m map[string]bool) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
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
	// Don't hand addon pods a ServiceAccount token unless the chart asks for
	// one: a mounted token is an escalation surface, and addon pods talk to the
	// platform through its APIs, not the Kubernetes API.
	setDefault(spec, "automountServiceAccountToken", false)
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
