package addon

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// workload wraps a pod spec (flow-style YAML) in a Deployment named worker.
func workload(podSpec string) string {
	return `apiVersion: apps/v1
kind: Deployment
metadata: {name: worker}
spec:
  selector: {matchLabels: {app: worker}}
  template:
    metadata: {labels: {app: worker}}
    spec: ` + podSpec + "\n"
}

func guard(t *testing.T, manifest string) []string {
	t.Helper()
	return Violations(objects(t, manifest), GuardInput{
		Namespace:  testNamespace,
		MediaClaim: "media",
		Reserved:   map[string]bool{"Secret/zaentrum-addon-example-generated": true},
	})
}

// Every refusal the guardrails make, one manifest each.
func TestGuardrailRefusals(t *testing.T) {
	for name, tc := range map[string]struct{ manifest, want string }{
		"ingress": {"apiVersion: networking.k8s.io/v1\nkind: Ingress\nmetadata: {name: web}\n",
			"Ingress/web: kind not allowed"},
		"route": {"apiVersion: route.openshift.io/v1\nkind: Route\nmetadata: {name: web}\n",
			"Route/web: kind not allowed"},
		"rbac": {"apiVersion: rbac.authorization.k8s.io/v1\nkind: Role\nmetadata: {name: admin}\n",
			"Role/admin: kind not allowed"},
		"crd": {"apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata: {name: things.example.org}\n",
			"CustomResourceDefinition/things.example.org: kind not allowed"},
		"cluster-scoped": {"apiVersion: v1\nkind: Namespace\nmetadata: {name: elsewhere}\n",
			"Namespace/elsewhere: kind not allowed"},
		"pod": {"apiVersion: v1\nkind: Pod\nmetadata: {name: bare}\n",
			"Pod/bare: kind not allowed"},
		"old apiVersion": {"apiVersion: extensions/v1beta1\nkind: Deployment\nmetadata: {name: old}\n",
			"Deployment/old: apiVersion extensions/v1beta1 not allowed (use apps/v1)"},
		"other namespace": {"apiVersion: v1\nkind: ConfigMap\nmetadata: {name: cfg, namespace: kube-system}\n",
			"ConfigMap/cfg: namespace kube-system not allowed (addons install into zaentrum-beta)"},
		"reserved name": {"apiVersion: v1\nkind: Secret\nmetadata: {name: zaentrum-addon-example-generated}\n",
			"Secret/zaentrum-addon-example-generated: name reserved for the addon's values"},
		"duplicate": {"apiVersion: v1\nkind: ConfigMap\nmetadata: {name: cfg}\n---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: cfg}\n",
			"ConfigMap/cfg: rendered more than once"},
		"no name": {"apiVersion: v1\nkind: ConfigMap\nmetadata: {}\n",
			"ConfigMap/: object without kind or metadata.name"},
		"hostPath": {workload("{volumes: [{name: data, hostPath: {path: /srv}}], containers: [{name: app, image: app}]}"),
			"Deployment/worker: hostPath volume not allowed (data)"},
		"other volume": {workload("{volumes: [{name: share, nfs: {server: nfs.example.org, path: /x}}], containers: [{name: app, image: app}]}"),
			"Deployment/worker: nfs volume not allowed (share)"},
		"foreign claim": {workload("{volumes: [{name: d, persistentVolumeClaim: {claimName: platform-db}}], containers: [{name: app, image: app}]}"),
			"Deployment/worker: persistentVolumeClaim platform-db not allowed (not rendered by the chart)"},
		"hostNetwork": {workload("{hostNetwork: true, containers: [{name: app, image: app}]}"),
			"Deployment/worker: hostNetwork not allowed"},
		"hostPID": {workload("{hostPID: true, containers: [{name: app, image: app}]}"),
			"Deployment/worker: hostPID not allowed"},
		"hostIPC": {workload("{hostIPC: true, containers: [{name: app, image: app}]}"),
			"Deployment/worker: hostIPC not allowed"},
		"privileged": {workload("{containers: [{name: app, image: app, securityContext: {privileged: true}}]}"),
			"Deployment/worker: container app: privileged not allowed"},
		"privilege escalation": {workload("{initContainers: [{name: init, image: app, securityContext: {allowPrivilegeEscalation: true}}], containers: [{name: app, image: app}]}"),
			"Deployment/worker: container init: allowPrivilegeEscalation not allowed"},
		"added capabilities": {workload("{containers: [{name: app, image: app, securityContext: {capabilities: {add: [NET_ADMIN, SYS_TIME]}}}]}"),
			"Deployment/worker: container app: added capabilities not allowed (NET_ADMIN, SYS_TIME)"},
		"root user (pod)": {workload("{securityContext: {runAsUser: 0}, containers: [{name: app, image: app}]}"),
			"Deployment/worker: runAsUser 0 not allowed"},
		"root user (container)": {workload("{containers: [{name: app, image: app, securityContext: {runAsUser: 0}}]}"),
			"Deployment/worker: container app: runAsUser 0 not allowed"},
		"runAsNonRoot false": {workload("{securityContext: {runAsNonRoot: false}, containers: [{name: app, image: app}]}"),
			"Deployment/worker: runAsNonRoot false not allowed"},
		"hostPort": {workload("{containers: [{name: app, image: app, ports: [{containerPort: 8080, hostPort: 80}]}]}"),
			"Deployment/worker: container app: hostPort 80 not allowed"},
		"foreign service account": {workload("{serviceAccountName: operator, containers: [{name: app, image: app}]}"),
			"Deployment/worker: serviceAccountName operator not allowed (not rendered by the chart)"},
		"job": {"apiVersion: batch/v1\nkind: Job\nmetadata: {name: once}\nspec:\n  template:\n    spec: {restartPolicy: Never, hostNetwork: true, containers: [{name: app, image: app}]}\n",
			"Job/once: hostNetwork not allowed"},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Contains(t, guard(t, tc.manifest), tc.want)
		})
	}
}

// What the guardrails allow: the chart's own claims and service accounts, the
// platform media claim, the default account and the harmless volume sources.
func TestGuardrailsAllow(t *testing.T) {
	manifest := `apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: worker-data}
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: worker}
---
apiVersion: v1
kind: Secret
metadata: {name: worker}
type: Opaque
---
apiVersion: v1
kind: ConfigMap
metadata: {name: cfg, namespace: zaentrum-beta}
---
apiVersion: v1
kind: Service
metadata: {name: worker}
spec: {type: ClusterIP, ports: [{port: 80}]}
---
` + workload(`{serviceAccountName: worker,
      automountServiceAccountToken: true,
      securityContext: {runAsNonRoot: true, runAsUser: 1000},
      imagePullSecrets: [{name: registry-pull}],
      volumes: [
        {name: own, persistentVolumeClaim: {claimName: worker-data}},
        {name: media, persistentVolumeClaim: {claimName: media}},
        {name: cfg, configMap: {name: cfg}},
        {name: sec, secret: {secretName: worker}},
        {name: tls, secret: {secretName: kafka-mtls}},
        {name: tmp, emptyDir: {}},
        {name: bare},
        {name: proj, projected: {sources: [{secret: {name: worker}}, {configMap: {name: cfg}}]}},
        {name: info, downwardAPI: {items: []}}],
      containers: [{name: app, image: app, ports: [{containerPort: 8080}],
        env: [{name: T, valueFrom: {secretKeyRef: {name: kafka-mtls, key: ca.crt}}},
              {name: C, valueFrom: {configMapKeyRef: {name: cfg, key: x}}}],
        envFrom: [{secretRef: {name: worker}}],
        securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}}]}`) + `---
apiVersion: batch/v1
kind: Job
metadata: {name: once}
spec:
  template:
    spec: {restartPolicy: Never, serviceAccountName: default, containers: [{name: app, image: app}]}
`
	// The media claim, the chart's own SA/Secret/ConfigMap/PVC, the platform
	// events TLS secret and the platform pull secret are all allowed.
	assert.Empty(t, Violations(objects(t, manifest), GuardInput{
		Namespace:       testNamespace,
		MediaClaim:      "media",
		EventsTLSSecret: "kafka-mtls",
		PullSecrets:     []string{"registry-pull"},
		Reserved:        map[string]bool{"Secret/zaentrum-addon-example-generated": true},
	}))
}

func TestGuardrailPrimaryService(t *testing.T) {
	in := GuardInput{Namespace: testNamespace, MediaClaim: "media", Primary: "worker"}
	assert.Equal(t, []string{"Service/worker: primary Service (zaentrum.io/primary) is not rendered"},
		Violations(objects(t, "apiVersion: v1\nkind: Service\nmetadata: {name: other}\nspec: {ports: [{port: 80}]}\n"), in))
	assert.Equal(t, []string{"Service/worker: the primary Service must expose port 80"},
		Violations(objects(t, "apiVersion: v1\nkind: Service\nmetadata: {name: worker}\nspec: {ports: [{port: 8080}]}\n"), in))
	assert.Empty(t, Violations(objects(t, "apiVersion: v1\nkind: Service\nmetadata: {name: worker}\nspec: {ports: [{port: 9090}, {port: 80}]}\n"), in))
}

func TestCollisions(t *testing.T) {
	const addonUID = types.UID("addon-uid")
	objs := objects(t, `apiVersion: v1
kind: Service
metadata: {name: free}
---
apiVersion: v1
kind: Service
metadata: {name: mine}
---
apiVersion: v1
kind: Service
metadata: {name: platform}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: handmade}
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata: {name: skipped}
`)
	yes := true
	owner := func(uid types.UID) *metav1.OwnerReference {
		return &metav1.OwnerReference{Kind: "Owner", Name: "x", UID: uid, Controller: &yes}
	}
	var looked []string
	violations, err := Collisions(objs, addonUID, func(o *unstructured.Unstructured) (bool, *metav1.OwnerReference, error) {
		looked = append(looked, Key(o))
		switch o.GetName() {
		case "mine":
			return true, owner(addonUID), nil
		case "platform":
			return true, owner("platform-uid"), nil
		case "handmade":
			return true, nil, nil
		}
		return false, nil, nil
	})
	require.NoError(t, err)
	assert.Equal(t, []string{
		"Service/platform: already exists and is not owned by this addon",
		"ConfigMap/handmade: already exists and is not owned by this addon",
	}, violations)
	assert.NotContains(t, looked, "Ingress/skipped", "refused kinds are never looked up")

	_, err = Collisions(objs, addonUID, func(*unstructured.Unstructured) (bool, *metav1.OwnerReference, error) {
		return false, nil, errors.New("api down")
	})
	assert.EqualError(t, err, "api down")
}

func TestDecorate(t *testing.T) {
	objs := objects(t, `apiVersion: v1
kind: Service
metadata:
  name: worker
  namespace: zaentrum-beta
  labels: {app.kubernetes.io/managed-by: Helm, tier: web}
---
`+workload(`{containers: [{name: app, image: app}], initContainers: [{name: init, image: app}]}`)+`---
apiVersion: batch/v1
kind: Job
metadata: {name: once}
spec:
  ttlSecondsAfterFinished: 60
  template:
    metadata: {labels: {job: once}}
    spec:
      securityContext: {runAsNonRoot: true, seccompProfile: {type: Localhost, localhostProfile: p.json}}
      restartPolicy: Never
      containers:
        - name: app
          image: app
          securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [NET_RAW]}}
`)
	Decorate(objs, Decoration{Name: "example", Namespace: testNamespace, PartOf: "zaentrum-beta-addons", Checksum: "abc"})

	svc := findObject(objs, "Service", "worker")
	assert.Equal(t, testNamespace, svc.GetNamespace())
	assert.Equal(t, map[string]string{
		"zaentrum.io/addon":            "example",
		"app.kubernetes.io/managed-by": "zaentrum-operator",
		"app.kubernetes.io/instance":   "example",
		"app.kubernetes.io/part-of":    "zaentrum-beta-addons",
		"tier":                         "web",
	}, svc.GetLabels())

	dep := findObject(objs, "Deployment", "worker")
	assert.Equal(t, testNamespace, dep.GetNamespace(), "an unset namespace is forced")
	assert.Equal(t, "worker", dep.GetLabels()["zaentrum.io/component"])
	podLabels, _, _ := unstructured.NestedStringMap(dep.Object, "spec", "template", "metadata", "labels")
	assert.Equal(t, map[string]string{"app": "worker", "zaentrum.io/addon": "example", "zaentrum.io/component": "worker"}, podLabels,
		"the chart's selector labels are kept; only new keys are added")
	checksum, _, _ := unstructured.NestedString(dep.Object, "spec", "template", "metadata", "annotations", "zaentrum.io/values-checksum")
	assert.Equal(t, "abc", checksum)
	pod, _, _ := unstructured.NestedMap(dep.Object, "spec", "template", "spec")
	assert.Equal(t, map[string]interface{}{"runAsNonRoot": true, "seccompProfile": map[string]interface{}{"type": "RuntimeDefault"}}, pod["securityContext"])
	for _, c := range containersOf(dep) {
		assert.Equal(t, map[string]interface{}{
			"allowPrivilegeEscalation": false,
			"capabilities":             map[string]interface{}{"drop": []interface{}{"ALL"}},
		}, c["securityContext"], "container %s defaulted", c["name"])
	}

	job := findObject(objs, "Job", "once")
	assert.Empty(t, job.GetLabels()["zaentrum.io/component"], "components are Deployments")
	_, hasTTL, _ := unstructured.NestedFieldNoCopy(job.Object, "spec", "ttlSecondsAfterFinished")
	assert.False(t, hasTTL, "a finished Job is kept, not deleted and re-run")
	checksum, _, _ = unstructured.NestedString(job.Object, "spec", "template", "metadata", "annotations", "zaentrum.io/values-checksum")
	assert.Equal(t, "abc", checksum)
	seccomp, _, _ := unstructured.NestedString(job.Object, "spec", "template", "spec", "securityContext", "seccompProfile", "type")
	assert.Equal(t, "Localhost", seccomp, "a setting the chart made is kept")
	drop, _, _ := unstructured.NestedSlice(containersOf(job)[0], "securityContext", "capabilities", "drop")
	assert.Equal(t, []interface{}{"NET_RAW"}, drop)
}

func TestApplyOrder(t *testing.T) {
	objs := objects(t, `apiVersion: apps/v1
kind: Deployment
metadata: {name: d}
---
apiVersion: v1
kind: Service
metadata: {name: s}
---
apiVersion: batch/v1
kind: Job
metadata: {name: j}
---
apiVersion: v1
kind: Secret
metadata: {name: b}
---
apiVersion: v1
kind: Secret
metadata: {name: a}
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: sa}
`)
	ApplyOrder(objs)
	var keys []string
	for _, o := range objs {
		keys = append(keys, Key(o))
	}
	assert.Equal(t, []string{"ServiceAccount/sa", "Secret/b", "Secret/a", "Service/s", "Deployment/d", "Job/j"}, keys)
}
