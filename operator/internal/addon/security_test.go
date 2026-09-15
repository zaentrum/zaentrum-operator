package addon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// guardPlatform runs the guardrails with the platform allowances the controller
// passes (media claim, events TLS secret, one pull secret).
func guardPlatform(t *testing.T, manifest string) []string {
	t.Helper()
	return Violations(objects(t, manifest), GuardInput{
		Namespace:       testNamespace,
		MediaClaim:      "media",
		EventsTLSSecret: "kafka-mtls",
		PullSecrets:     []string{"registry-pull"},
		Reserved:        map[string]bool{"Secret/zaentrum-addon-example-generated": true},
	})
}

// Blocker 1: a Secret the token controller acts on (service-account-token),
// and the ServiceAccount-binding annotations, are refused.
func TestGuardSecretTypeAndSAAnnotations(t *testing.T) {
	saToken := `apiVersion: v1
kind: Secret
metadata:
  name: worker
  annotations: {kubernetes.io/service-account.name: portal-api}
type: kubernetes.io/service-account-token
`
	v := guardPlatform(t, saToken)
	assert.Contains(t, v, "Secret/worker: Secret type kubernetes.io/service-account-token not allowed")
	assert.Contains(t, v, "Secret/worker: annotation kubernetes.io/service-account.name not allowed")

	uid := `apiVersion: v1
kind: ConfigMap
metadata: {name: cfg, annotations: {kubernetes.io/service-account.uid: "123"}}
`
	assert.Contains(t, guardPlatform(t, uid), "ConfigMap/cfg: annotation kubernetes.io/service-account.uid not allowed")

	// The ordinary Secret types are accepted.
	for _, typ := range []string{"", "Opaque", "kubernetes.io/tls", "kubernetes.io/dockerconfigjson", "kubernetes.io/basic-auth", "kubernetes.io/ssh-auth"} {
		m := "apiVersion: v1\nkind: Secret\nmetadata: {name: worker}\n"
		if typ != "" {
			m += "type: " + typ + "\n"
		}
		assert.Empty(t, guardPlatform(t, m), "Secret type %q should be accepted", typ)
	}
}

// Blocker 2: Service fields that reach off-namespace or cluster-wide.
func TestGuardServiceSpec(t *testing.T) {
	for _, tc := range []struct{ spec, want string }{
		{"{externalIPs: [\"10.0.0.53\"], ports: [{port: 80}]}", "Service/worker: Service spec.externalIPs not allowed"},
		{"{type: LoadBalancer, ports: [{port: 80}]}", "Service/worker: Service type LoadBalancer not allowed (ClusterIP only)"},
		{"{type: NodePort, ports: [{port: 80, nodePort: 30111}]}", "Service/worker: Service type NodePort not allowed (ClusterIP only)"},
		{"{type: ExternalName, externalName: attacker.example.org}", "Service/worker: Service type ExternalName not allowed (ClusterIP only)"},
		{"{type: ExternalName, externalName: attacker.example.org}", "Service/worker: Service spec.externalName not allowed"},
		{"{loadBalancerIP: 10.0.0.9, ports: [{port: 80}]}", "Service/worker: Service spec.loadBalancerIP not allowed"},
		{"{loadBalancerSourceRanges: [\"0.0.0.0/0\"], ports: [{port: 80}]}", "Service/worker: Service spec.loadBalancerSourceRanges not allowed"},
	} {
		m := "apiVersion: v1\nkind: Service\nmetadata: {name: worker}\nspec: " + tc.spec + "\n"
		assert.Contains(t, guardPlatform(t, m), tc.want, tc.spec)
	}
	// A plain ClusterIP Service is fine.
	assert.Empty(t, guardPlatform(t, "apiVersion: v1\nkind: Service\nmetadata: {name: worker}\nspec: {type: ClusterIP, ports: [{port: 80}]}\n"))
}

// Blocker 6: a PVC that binds a specific PV or clones a volume.
func TestGuardPVCSpec(t *testing.T) {
	m := `apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: grab}
spec:
  accessModes: [ReadWriteOnce]
  volumeName: someone-elses-released-volume
  dataSource: {kind: PersistentVolumeClaim, name: platform-db-data}
  dataSourceRef: {kind: PersistentVolumeClaim, name: platform-db-data}
  resources: {requests: {storage: 1Gi}}
`
	v := guardPlatform(t, m)
	assert.Contains(t, v, "PersistentVolumeClaim/grab: PVC spec.volumeName not allowed (binds a specific PV)")
	assert.Contains(t, v, "PersistentVolumeClaim/grab: PVC spec.dataSource not allowed")
	assert.Contains(t, v, "PersistentVolumeClaim/grab: PVC spec.dataSourceRef not allowed")

	assert.Empty(t, guardPlatform(t, "apiVersion: v1\nkind: PersistentVolumeClaim\nmetadata: {name: ok}\nspec: {accessModes: [ReadWriteOnce], resources: {requests: {storage: 1Gi}}}\n"))
}

// Blocker 8: a pod may not reference a Secret/ConfigMap the chart does not
// render, by volume, projected source, env valueFrom or envFrom.
func TestGuardArbitrarySecretConfigMapRefs(t *testing.T) {
	m := workload(`{
      volumes: [
        {name: creds, secret: {secretName: chino-db-credentials}},
        {name: pcfg, configMap: {name: platform-config}},
        {name: proj, projected: {sources: [{secret: {name: keycloak-admin}}, {configMap: {name: platform-config}}]}}],
      containers: [{name: app, image: app,
        envFrom: [{secretRef: {name: zaentrum-worker-oidc}}, {configMapRef: {name: platform-config}}],
        env: [{name: PW, valueFrom: {secretKeyRef: {name: keycloak-admin, key: password}}},
              {name: CF, valueFrom: {configMapKeyRef: {name: platform-config, key: x}}}],
        volumeMounts: [{name: creds, mountPath: /creds}]}]}`)
	v := guardPlatform(t, m)
	for _, want := range []string{
		"secret volume creds references chino-db-credentials, not rendered by the chart",
		"configMap volume pcfg references platform-config, not rendered by the chart",
		"projected volume proj references secret keycloak-admin, not rendered by the chart",
		"projected volume proj references configMap platform-config, not rendered by the chart",
		"envFrom references secret zaentrum-worker-oidc, not rendered by the chart",
		"envFrom references configMap platform-config, not rendered by the chart",
		"env PW references secret keycloak-admin, not rendered by the chart",
		"env CF references configMap platform-config, not rendered by the chart",
	} {
		assert.Contains(t, joinViolations(v), want)
	}
}

// The platform-provided secrets (events TLS, pull secret) may be referenced;
// so may the chart's own rendered objects.
func TestGuardAllowsPlatformAndRenderedRefs(t *testing.T) {
	m := `apiVersion: v1
kind: Secret
metadata: {name: own}
type: Opaque
---
apiVersion: v1
kind: ConfigMap
metadata: {name: owncfg}
---
` + workload(`{
      volumes: [{name: tls, secret: {secretName: kafka-mtls}},
                {name: own, secret: {secretName: own}},
                {name: cfg, configMap: {name: owncfg}}],
      containers: [{name: app, image: app,
        env: [{name: T, valueFrom: {secretKeyRef: {name: kafka-mtls, key: ca.crt}}}],
        envFrom: [{secretRef: {name: registry-pull}}]}]}`)
	assert.Empty(t, guardPlatform(t, m))
}

// imagePullSecrets must be a chart-rendered Secret or a platform pull secret.
func TestGuardImagePullSecrets(t *testing.T) {
	bad := workload(`{imagePullSecrets: [{name: some-other-registry}], containers: [{name: app, image: app}]}`)
	assert.Contains(t, guardPlatform(t, bad),
		"Deployment/worker: imagePullSecrets some-other-registry not allowed (not rendered by the chart or a platform pull secret)")
	ok := workload(`{imagePullSecrets: [{name: registry-pull}], containers: [{name: app, image: app}]}`)
	assert.Empty(t, guardPlatform(t, ok))
}

// Minor: scheduling knobs that place a pod on the control plane, or preempt.
func TestGuardScheduling(t *testing.T) {
	for _, tc := range []struct{ spec, want string }{
		{"{nodeName: master-0, containers: [{name: app, image: app}]}", "Deployment/worker: nodeName not allowed"},
		{"{priorityClassName: system-cluster-critical, containers: [{name: app, image: app}]}", "Deployment/worker: priorityClassName not allowed"},
		{"{nodeSelector: {node-role.kubernetes.io/control-plane: \"\"}, containers: [{name: app, image: app}]}", "Deployment/worker: nodeSelector node-role.kubernetes.io/control-plane not allowed (control-plane placement)"},
		{"{tolerations: [{key: node-role.kubernetes.io/control-plane, operator: Exists}], containers: [{name: app, image: app}]}", "Deployment/worker: toleration for control-plane taints not allowed"},
		{"{tolerations: [{operator: Exists}], containers: [{name: app, image: app}]}", "Deployment/worker: toleration for control-plane taints not allowed"},
		{"{affinity: {nodeAffinity: {requiredDuringSchedulingIgnoredDuringExecution: {nodeSelectorTerms: [{matchExpressions: [{key: node-role.kubernetes.io/master, operator: Exists}]}]}}}, containers: [{name: app, image: app}]}", "Deployment/worker: nodeAffinity for control-plane nodes not allowed"},
	} {
		assert.Contains(t, guardPlatform(t, workload(tc.spec)), tc.want, tc.spec)
	}
	// A benign nodeSelector and a normal toleration are fine.
	assert.Empty(t, guardPlatform(t, workload(`{nodeSelector: {disktype: ssd}, tolerations: [{key: workload, operator: Equal, value: media, effect: NoSchedule}], containers: [{name: app, image: app}]}`)))
	// hostAliases and dnsConfig are not refused (not in scope).
	assert.Empty(t, guardPlatform(t, workload(`{hostAliases: [{ip: 10.0.0.9, hostnames: [x]}], dnsConfig: {nameservers: [10.6.6.6]}, containers: [{name: app, image: app}]}`)))
}

// Minor: security-context escape hatches at pod and container level.
func TestGuardSecurityContextExtras(t *testing.T) {
	pod := workload(`{
      securityContext: {seLinuxOptions: {type: spc_t}, sysctls: [{name: net.core.somaxconn, value: "1024"}], appArmorProfile: {type: Unconfined}, windowsOptions: {hostProcess: true}},
      containers: [{name: app, image: app}]}`)
	pv := guardPlatform(t, pod)
	for _, want := range []string{
		"Deployment/worker: seLinuxOptions.type not allowed",
		"Deployment/worker: sysctls not allowed",
		"Deployment/worker: appArmorProfile Unconfined not allowed",
		"Deployment/worker: windowsOptions.hostProcess not allowed",
	} {
		assert.Contains(t, joinViolations(pv), want)
	}

	ctr := workload(`{containers: [{name: app, image: app, securityContext: {procMount: Unmasked, seLinuxOptions: {type: spc_t}, appArmorProfile: {type: Unconfined}, windowsOptions: {hostProcess: true}}}]}`)
	cv := guardPlatform(t, ctr)
	for _, want := range []string{
		"container app: procMount Unmasked not allowed",
		"container app: seLinuxOptions.type not allowed",
		"container app: appArmorProfile Unconfined not allowed",
		"container app: windowsOptions.hostProcess not allowed",
	} {
		assert.Contains(t, joinViolations(cv), want)
	}

	// An init container is checked too.
	init := workload(`{initContainers: [{name: setup, image: app, securityContext: {procMount: Unmasked}}], containers: [{name: app, image: app}]}`)
	assert.Contains(t, joinViolations(guardPlatform(t, init)), "container setup: procMount Unmasked not allowed")
}

// Minor: a chart-supplied ownerReference is a violation (never applied).
func TestGuardOwnerReferences(t *testing.T) {
	m := `apiVersion: v1
kind: ConfigMap
metadata:
  name: cfg
  ownerReferences: [{apiVersion: v1, kind: ConfigMap, name: parent, uid: "abc"}]
`
	assert.Contains(t, guardPlatform(t, m), "ConfigMap/cfg: metadata.ownerReferences not allowed (the operator sets the owner)")
}

// Blocker 8 (default): an addon pod does not get a ServiceAccount token unless
// the chart asks for one.
func TestDecorateDefaultsAutomountFalse(t *testing.T) {
	objs := objects(t, workload(`{containers: [{name: app, image: app}]}`))
	Decorate(objs, Decoration{Name: "example", Namespace: testNamespace, PartOf: "p", Checksum: "c"})
	v, found, _ := unstructured.NestedBool(objs[0].Object, "spec", "template", "spec", "automountServiceAccountToken")
	assert.True(t, found)
	assert.False(t, v)

	// A chart that opts in is left alone.
	optIn := objects(t, workload(`{automountServiceAccountToken: true, containers: [{name: app, image: app}]}`))
	Decorate(optIn, Decoration{Name: "example", Namespace: testNamespace, PartOf: "p", Checksum: "c"})
	v, _, _ = unstructured.NestedBool(optIn[0].Object, "spec", "template", "spec", "automountServiceAccountToken")
	assert.True(t, v, "an explicit opt-in is preserved")
}

func joinViolations(v []string) string {
	out := ""
	for _, s := range v {
		out += s + "\n"
	}
	return out
}
