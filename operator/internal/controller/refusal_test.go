package controller

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/meta/testrestmapper"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
	"github.com/zaentrum/zaentrum-operator/operator/internal/templates"
)

// The schema's rules of spec.databases are the operator's: each name's pattern
// and maximum length, the host's pattern and the sslmode's enum, as plain
// keywords and no CEL rule.
func TestTheSchemasRulesAreTheOperators(t *testing.T) {
	for _, key := range []string{"chino", "katalog", "keycloak", "portal"} {
		rule := fieldRule(t, "databases", key)
		assert.Equal(t, zaentrumv1alpha1.DatabaseNamePattern, rule.pattern, key)
		assert.Equal(t, zaentrumv1alpha1.DatabaseNameMaxLength, rule.maxLength, key)
	}
	assert.Equal(t, zaentrumv1alpha1.DatabaseHostPattern, fieldRule(t, "databases", "external", "host").pattern)
	versions, _ := dig(zaentrumCRDSpec(t, canonicalCRD), "versions").([]any)
	require.Len(t, versions, 1)
	sslmode := dig(versions[0], "schema", "openAPIV3Schema", "properties", "spec", "properties", "databases", "properties",
		"external", "properties", "sslmode")
	var modes []any
	for _, m := range zaentrumv1alpha1.DatabaseSSLModes {
		modes = append(modes, m)
	}
	assert.Equal(t, modes, dig(sslmode, "enum"))
	assert.Nil(t, dig(sslmode, "x-kubernetes-validations"))
}

// The markers on the types, which controller-gen writes into the schema, hold
// the rules as databases_rules.go has them: the patterns, the names' maximum
// length and the sslmode's values have one source, which this test keeps.
func TestTheMarkersAreTheRules(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "../../api/v1alpha1/zaentrum_types.go", nil, parser.ParseComments)
	require.NoError(t, err)
	markers := map[string][]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || (ts.Name.Name != "DatabasesSpec" && ts.Name.Name != "DatabaseExternalSpec") {
			return true
		}
		for _, field := range ts.Type.(*ast.StructType).Fields.List {
			if field.Doc == nil || len(field.Names) != 1 {
				continue
			}
			for _, c := range field.Doc.List {
				if m, ok := strings.CutPrefix(c.Text, "// +kubebuilder:validation:"); ok {
					markers[ts.Name.Name+"."+field.Names[0].Name] = append(markers[ts.Name.Name+"."+field.Names[0].Name], m)
				}
			}
		}
		return false
	})
	for _, name := range []string{"Chino", "Katalog", "Keycloak", "Portal"} {
		assert.ElementsMatch(t, []string{"MaxLength=63", "Pattern=`" + zaentrumv1alpha1.DatabaseNamePattern + "`"},
			markers["DatabasesSpec."+name], name)
	}
	assert.Equal(t, 63, zaentrumv1alpha1.DatabaseNameMaxLength)
	assert.Equal(t, []string{"Pattern=`" + zaentrumv1alpha1.DatabaseHostPattern + "`"}, markers["DatabaseExternalSpec.Host"])
	assert.Equal(t, []string{`Enum="";` + strings.Join(zaentrumv1alpha1.DatabaseSSLModes[1:], ";")}, markers["DatabaseExternalSpec.SSLMode"])
	assert.Equal(t, "", zaentrumv1alpha1.DatabaseSSLModes[0], "the empty value, which the platform reads as require")
}

// refusedBy is the operator's refusals of a resource with the edit made.
func refusedBy(edit func(*zaentrumv1alpha1.DatabasesSpec)) []string {
	z := verifyCR()
	edit(&z.Spec.Databases)
	return refusals(z)
}

// external is an edit that makes the databases external and then edits.
func external(edit func(*zaentrumv1alpha1.DatabasesSpec)) func(*zaentrumv1alpha1.DatabasesSpec) {
	return func(d *zaentrumv1alpha1.DatabasesSpec) {
		d.Mode = "external"
		d.External.Host = "db.example.org"
		edit(d)
	}
}

var (
	namesTaken   = []string{"", "apps_2", "_x", "order", "1chino", "chino.beta", "media-chino", "Chino", "Media-Chino", strings.Repeat("a", 63)}
	namesRefused = []string{"chino$prod", "zürich", "-db", ".db", "chi'no", "kat alog", "key;cloak", "chi/no", "chi\x01no",
		strings.Repeat("a", 64), "chino\nx"}
)

// The operator refuses what the rules refuse, in its own words: a name always,
// the host and the sslmode only with external databases, the only place the
// chart reads them.
func TestTheOperatorRefusesWhatTheRulesRefuse(t *testing.T) {
	assert.Empty(t, refusals(verifyCR()), "the defaults")
	keys := map[string]func(*zaentrumv1alpha1.DatabasesSpec, string){
		"chino":    func(d *zaentrumv1alpha1.DatabasesSpec, v string) { d.Chino = v },
		"katalog":  func(d *zaentrumv1alpha1.DatabasesSpec, v string) { d.Katalog = v },
		"keycloak": func(d *zaentrumv1alpha1.DatabasesSpec, v string) { d.Keycloak = v },
		"portal":   func(d *zaentrumv1alpha1.DatabasesSpec, v string) { d.Portal = v },
	}
	for key, set := range keys {
		for _, name := range append([]string{key}, namesTaken...) {
			assert.Empty(t, refusedBy(func(d *zaentrumv1alpha1.DatabasesSpec) { set(d, name) }), "%s %q", key, name)
		}
		for _, name := range namesRefused {
			want := []string{key + " must be a name a database can have: at most 63 ASCII letters, digits, underscores, hyphens and dots, starting with a letter, a digit or an underscore"}
			assert.Equal(t, want, refusedBy(func(d *zaentrumv1alpha1.DatabasesSpec) { set(d, name) }), "%s %q", key, name)
			assert.Equal(t, want, refusedBy(external(func(d *zaentrumv1alpha1.DatabasesSpec) { set(d, name) })), "%s %q, external", key, name)
		}
	}

	const hostRefusal = "host must be a host name or an IP address in ASCII, an IPv6 address in square brackets; several are separated by commas"
	for _, host := range append(hostsTaken, host253+".", host253+","+host253+".") {
		assert.Empty(t, refusedBy(external(func(d *zaentrumv1alpha1.DatabasesSpec) { d.External.Host = host })), "%q", host)
	}
	for _, host := range append(hostsRefused, host253+"d", "db.example.org,"+host253+"d") {
		assert.Equal(t, []string{hostRefusal}, refusedBy(external(func(d *zaentrumv1alpha1.DatabasesSpec) { d.External.Host = host })), "%q", host)
		assert.Empty(t, refusedBy(func(d *zaentrumv1alpha1.DatabasesSpec) { d.External.Host = host }), "%q: not external, a leftover", host)
	}

	const sslmodeRefusal = "sslmode must be disable, allow, prefer, require, verify-ca or verify-full"
	for _, mode := range zaentrumv1alpha1.DatabaseSSLModes {
		assert.Empty(t, refusedBy(external(func(d *zaentrumv1alpha1.DatabasesSpec) { d.External.SSLMode = mode })), "%q", mode)
	}
	for _, mode := range []string{"x", "Require", "require;x", "require&connect_timeout=5"} {
		assert.Equal(t, []string{sslmodeRefusal}, refusedBy(external(func(d *zaentrumv1alpha1.DatabasesSpec) { d.External.SSLMode = mode })), "%q", mode)
		assert.Empty(t, refusedBy(func(d *zaentrumv1alpha1.DatabasesSpec) { d.External.SSLMode = mode }), "%q: not external, a leftover", mode)
	}

	all := refusedBy(external(func(d *zaentrumv1alpha1.DatabasesSpec) {
		d.Portal, d.Chino, d.External.SSLMode, d.External.Host = "por tal", "chi no", "Require", "db x"
	}))
	assert.Equal(t, "chino must be a name a database can have: at most 63 ASCII letters, digits, underscores, hyphens and dots, starting with a letter, a digit or an underscore; "+
		"portal must be a name a database can have: at most 63 ASCII letters, digits, underscores, hyphens and dots, starting with a letter, a digit or an underscore; "+
		hostRefusal+"; "+sslmodeRefusal+"; "+
		"nothing new is applied until the resource is corrected", refusedMessage(all), "in the order of the fields, then what follows")
}

// The schema and the operator's check take and refuse the same values, but
// for one: a host longer than 253 characters, without a trailing dot, which
// a pattern cannot count, passes the schema and the operator refuses it.
// The check of a stored value with external databases is the schema's at a
// write.
func TestTheSchemaAndTheOperatorAgree(t *testing.T) {
	for _, key := range []string{"chino", "portal"} {
		rule := fieldRule(t, "databases", key)
		for _, name := range append(append([]string{key}, namesTaken...), namesRefused...) {
			operator := len(refusedBy(func(d *zaentrumv1alpha1.DatabasesSpec) {
				if key == "chino" {
					d.Chino = name
				} else {
					d.Portal = name
				}
			})) == 0
			assert.Equal(t, rule.takes(name), operator, "%s %q", key, name)
		}
	}
	rule := fieldRule(t, "databases", "external", "host")
	for _, host := range append(append([]string{}, hostsTaken...), hostsRefused...) {
		operator := len(refusedBy(external(func(d *zaentrumv1alpha1.DatabasesSpec) { d.External.Host = host }))) == 0
		assert.Equal(t, rule.takes(host), operator, "%q", host)
	}
	long := host253 + "d"
	assert.True(t, rule.takes(long), "the schema takes a host of 254 characters")
	assert.NotEmpty(t, refusedBy(external(func(d *zaentrumv1alpha1.DatabasesSpec) { d.External.Host = long })), "the operator refuses it")
}

// No refusal of the operator's repeats the value it refuses: neither a name,
// nor a host, nor an sslmode reaches the condition's message.
func TestTheOperatorsRefusalsRepeatNoValue(t *testing.T) {
	const mark = "Refused-Value-7"
	for _, edit := range []func(*zaentrumv1alpha1.DatabasesSpec){
		func(d *zaentrumv1alpha1.DatabasesSpec) { d.Chino = "x " + mark },
		func(d *zaentrumv1alpha1.DatabasesSpec) { d.Katalog = "x " + mark },
		func(d *zaentrumv1alpha1.DatabasesSpec) { d.Keycloak = "x " + mark },
		func(d *zaentrumv1alpha1.DatabasesSpec) { d.Portal = "x " + mark },
		external(func(d *zaentrumv1alpha1.DatabasesSpec) { d.External.Host = "x " + mark }),
		external(func(d *zaentrumv1alpha1.DatabasesSpec) { d.External.SSLMode = mark }),
	} {
		refused := refusedBy(edit)
		require.Len(t, refused, 1)
		assert.NotContains(t, strings.ToLower(refusedMessage(refused)), strings.ToLower(mark))
	}
}

// readyOf is the Zaentrum's Ready condition.
func readyOf(t *testing.T, z *zaentrumv1alpha1.Zaentrum) metav1.Condition {
	t.Helper()
	c := meta.FindStatusCondition(z.Status.Conditions, condTypeReady)
	require.NotNil(t, c, "a Ready condition")
	return *c
}

// deploymentNames are the Deployments in the platform's namespace.
func deploymentNames(t *testing.T, e *platformEnv) []string {
	t.Helper()
	var deps appsv1.DeploymentList
	require.NoError(t, e.c.List(context.Background(), &deps, client.InNamespace(e.key.Namespace)))
	var names []string
	for _, d := range deps.Items {
		names = append(names, d.Name)
	}
	return names
}

// portalURL is portal-api's database URL as applied.
func portalURL(t *testing.T, e *platformEnv) string {
	t.Helper()
	var dep appsv1.Deployment
	require.NoError(t, e.c.Get(context.Background(), types.NamespacedName{Namespace: e.key.Namespace, Name: "portal-api"}, &dep))
	for _, c := range dep.Spec.Template.Spec.Containers {
		for _, env := range c.Env {
			if env.Name == "PG_URL" {
				return env.Value
			}
		}
	}
	return ""
}

const chinoRefused = "chino must be a name a database can have: at most 63 ASCII letters, digits, underscores, hyphens and dots, starting with a letter, a digit or an underscore; " +
	"nothing new is applied until the resource is corrected"

// A stored resource with a value the rules refuse is refused by the operator:
// Ready False, ResourceRefused, the refusals in the operator's words, on every
// pass. Nothing new is applied and nothing pruned while it holds: what runs
// keeps running as it was. The corrected resource is applied as any other.
func TestAStoredResourceTheRulesRefuseIsNotApplied(t *testing.T) {
	// A new install whose resource holds the value: nothing is applied.
	z := verifyCR()
	z.Spec.Databases.Chino = "chi no"
	e := newPlatformEnv(t, z, nil)
	e.reconcile()
	ready := readyOf(t, e.zaentrum())
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, reasonResourceRefused, ready.Reason)
	assert.Equal(t, chinoRefused, ready.Message)
	assert.Equal(t, "Error", e.zaentrum().Status.Phase)
	assert.Empty(t, deploymentNames(t, e), "nothing is applied")

	// A running platform: the value comes with other changes, which wait.
	z = verifyCR()
	z.Spec.Features.Pipeline = true
	z.Spec.Pipeline.Encoder = "cpu"
	e = newPlatformEnv(t, z, nil)
	e.reconcile()
	running := deploymentNames(t, e)
	require.Contains(t, running, "transcoder")
	before := portalURL(t, e)
	require.Contains(t, before, "/portal?")
	e.change(func(z *zaentrumv1alpha1.Zaentrum) {
		z.Spec.Databases.Chino = "chi no"
		z.Spec.Databases.Portal = "launchpad"
		z.Spec.Features.Pipeline = false
	})
	for pass := 1; pass <= 2; pass++ {
		e.reconcile()
		got := e.zaentrum()
		ready := readyOf(t, got)
		assert.Equal(t, reasonResourceRefused, ready.Reason, "pass %d", pass)
		assert.Equal(t, chinoRefused, ready.Message, "pass %d", pass)
		assert.Equal(t, got.Generation, ready.ObservedGeneration, "pass %d: of the resource as it is", pass)
		assert.Equal(t, running, deploymentNames(t, e), "pass %d: nothing is pruned", pass)
		assert.Equal(t, before, portalURL(t, e), "pass %d: nothing new is applied", pass)
	}

	// Corrected, it is applied: the changes that waited with it, too.
	e.change(func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.Databases.Chino = "chino" })
	e.reconcile()
	assert.NotEqual(t, reasonResourceRefused, readyOf(t, e.zaentrum()).Reason)
	assert.Contains(t, portalURL(t, e), "/launchpad?")
	assert.NotContains(t, deploymentNames(t, e), "transcoder", "what the platform no longer has goes")

	// A host and an sslmode left over where the databases are not external
	// stop nothing: the chart does not read them.
	e.change(func(z *zaentrumv1alpha1.Zaentrum) {
		z.Spec.Databases.External.Host = "db x"
		z.Spec.Databases.External.SSLMode = "x"
	})
	e.reconcile()
	assert.NotEqual(t, reasonResourceRefused, readyOf(t, e.zaentrum()).Reason)
}

// countingMapper counts the OpenShift check's discovery, the RESTMapping of
// the SecurityContextConstraints.
type countingMapper struct {
	meta.RESTMapper
	mu    sync.Mutex
	asked int
}

func (m *countingMapper) RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error) {
	if gk.Kind == "SecurityContextConstraints" {
		m.mu.Lock()
		m.asked++
		m.mu.Unlock()
	}
	return m.RESTMapper.RESTMapping(gk, versions...)
}

// A pass that refuses the resource discovers, renders, applies, starts and
// prunes nothing: in the platform's namespace it reads the resource and the
// backups' CronJob, whose schedule it lets move once nothing holds it, and
// writes its status, nothing else, and it asks neither the channel nor the
// cluster whether it is OpenShift. The status names the operator that runs.
func TestARefusedPassDoesNothingButSayIt(t *testing.T) {
	selfEnv(t)
	const image = "ghcr.io/zaentrum/operator:sha-0000000000000000000000000000000000000002"
	z := verifyCR()
	z.Spec.Databases.Chino = "chi no"
	z.Status.Controller = &zaentrumv1alpha1.ControllerStatus{Image: "ghcr.io/zaentrum/operator:sha-0000000000000000000000000000000000000001",
		Version: "sha-0000000000000000000000000000000000000001", Source: zaentrumv1alpha1.InstallSourceManifest}

	var mu sync.Mutex
	var calls []string
	record := func(what string, obj client.Object) {
		if obj.GetNamespace() != z.Namespace {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, fmt.Sprintf("%s %T %s", what, obj, obj.GetName()))
	}
	channel := 0
	releases := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		channel++
		mu.Unlock()
		http.Error(w, "no channel here", http.StatusNotFound)
	}))
	defer releases.Close()

	s := selfScheme(t)
	mapper := &countingMapper{RESTMapper: testrestmapper.TestOnlyStaticRESTMapper(s)}
	c := fake.NewClientBuilder().WithScheme(s).WithRESTMapper(mapper).
		WithObjects(z, managerPod(image), managerDeployment()).
		WithStatusSubresource(&zaentrumv1alpha1.Zaentrum{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				err := cl.Get(ctx, key, obj, opts...)
				obj.SetNamespace(key.Namespace)
				obj.SetName(key.Name)
				record("get", obj)
				return err
			},
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				mu.Lock()
				calls = append(calls, fmt.Sprintf("list %T", list))
				mu.Unlock()
				return cl.List(ctx, list, opts...)
			},
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				record("create", obj)
				return cl.Create(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				record("update", obj)
				return cl.Update(ctx, obj, opts...)
			},
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				record("patch", obj)
				return applyAsCreateOrUpdate(ctx, cl, obj, patch, opts...)
			},
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				record("delete", obj)
				return cl.Delete(ctx, obj, opts...)
			},
			SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				record("update "+sub, obj)
				return cl.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).Build()
	r := &ZaentrumReconciler{Client: c, Scheme: s, ReleasesURL: releases.URL}
	key := types.NamespacedName{Namespace: z.Namespace, Name: z.Name}
	for pass := 1; pass <= 2; pass++ {
		_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
		require.NoError(t, err)
	}

	one := []string{"get *v1alpha1.Zaentrum zaentrum", "get *v1.CronJob " + templates.BackupCronJobName, "update status *v1alpha1.Zaentrum zaentrum"}
	assert.Equal(t, append(one, one...), calls, "the resource, the backups' CronJob and the status, each pass")
	assert.Zero(t, channel, "the channel is not consulted")
	assert.Zero(t, mapper.asked, "nor the cluster, whether it is OpenShift")
	var got zaentrumv1alpha1.Zaentrum
	require.NoError(t, c.Get(context.Background(), key, &got))
	require.NotNil(t, got.Status.Controller)
	assert.Equal(t, image, got.Status.Controller.Image, "the operator that runs, not the one before it")
}

// A restore asked for while the resource is refused is refused at once, in
// the Words, and does not start once the resource is corrected; asked for
// again, it does.
func TestARestoreAskedForWhileRefusedIsRefused(t *testing.T) {
	z := verifyCR()
	z.Spec.Databases.Chino = "chi no"
	z.Annotations = map[string]string{zaentrumv1alpha1.RestoreRequestAnnotation: aDump}
	claim := restorable(z)
	e := newPlatformEnv(t, z, nil, claim)
	e.reconcile()
	rs := restoreStatus(e.zaentrum())
	require.NotNil(t, rs)
	assert.Equal(t, zaentrumv1alpha1.RestoreRefused, rs.Result)
	assert.Equal(t, aDump, rs.Request)
	assert.Equal(t, "refused: the resource is refused, so nothing is restored; correct it and ask again; nothing was changed", rs.Message)

	e.change(func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.Databases.Chino = "chino" })
	e.reconcile()
	rs = restoreStatus(e.zaentrum())
	require.NotNil(t, rs)
	assert.Equal(t, zaentrumv1alpha1.RestoreRefused, rs.Result, "the request was answered: it does not start")
	assert.NotEqual(t, "Restoring", e.zaentrum().Status.Phase)

	e.change(func(z *zaentrumv1alpha1.Zaentrum) {
		z.Annotations[zaentrumv1alpha1.RestoreRequestAnnotation] = aDump + "#again"
		listedOn(z, "backups")
	})
	e.reconcile()
	rs = restoreStatus(e.zaentrum())
	require.NotNil(t, rs)
	assert.Contains(t, []zaentrumv1alpha1.RestoreResult{zaentrumv1alpha1.RestoreStopping, zaentrumv1alpha1.RestoreRunning}, rs.Result,
		"asked again, it starts")
}

// A restore in flight when the resource becomes refused is not taken a step
// further: its Job's end is not read, its clients stay stopped and its status
// keeps what it said, and its condition Restore says, in the Words, that it
// waits. Corrected, the restore goes on from where it was, and its condition
// says what it says then: it ends, where its Job ended meanwhile; it runs on,
// with its own words, where its Job still runs.
func TestARestoreInFlightIsHeldWhileRefused(t *testing.T) {
	for _, jobEnds := range []bool{true, false} {
		t.Run(map[bool]string{true: "its Job ends meanwhile", false: "its Job still runs"}[jobEnds], func(t *testing.T) {
			ctx := context.Background()
			z := verifyCR()
			z.Annotations = map[string]string{zaentrumv1alpha1.RestoreRequestAnnotation: aDump}
			claim := restorable(z)
			e := newPlatformEnv(t, z, nil, claim)
			e.reconcile()
			rs := restoreStatus(e.zaentrum())
			require.NotNil(t, rs)
			require.Equal(t, zaentrumv1alpha1.RestoreRunning, rs.Result, "no client was up: the restore's Job runs")
			held := *rs
			keycloakReplicas := func() int32 {
				var dep appsv1.Deployment
				require.NoError(t, e.c.Get(ctx, types.NamespacedName{Namespace: z.Namespace, Name: "keycloak"}, &dep))
				return *dep.Spec.Replicas
			}
			require.Zero(t, keycloakReplicas(), "a client stopped for it")

			e.change(func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.Databases.Chino = "chi no" })
			if jobEnds {
				var job batchv1.Job
				require.NoError(t, e.c.Get(ctx, types.NamespacedName{Namespace: z.Namespace, Name: held.Job}, &job))
				job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
				require.NoError(t, e.c.Status().Update(ctx, &job))
				require.NoError(t, e.c.Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-x", Namespace: z.Namespace,
					Labels: map[string]string{batchv1.JobNameLabel: job.Name}},
					Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: templates.RestoreContainer,
						State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: "restored"}}}}}}))
			}
			for pass := 1; pass <= 2; pass++ {
				e.reconcile()
				got := e.zaentrum()
				assert.Equal(t, reasonResourceRefused, readyOf(t, got).Reason, "pass %d", pass)
				rs := restoreStatus(got)
				require.NotNil(t, rs)
				assert.Equal(t, held.Result, rs.Result, "pass %d: not a step further", pass)
				assert.Equal(t, held.Message, rs.Message, "pass %d: its status keeps what it said", pass)
				assert.Zero(t, keycloakReplicas(), "pass %d: its clients stay stopped", pass)
				c := meta.FindStatusCondition(got.Status.Conditions, condTypeRestore)
				require.NotNil(t, c)
				assert.Equal(t, "the restore of "+aDump+" waits: the resource is refused, and it goes on once the resource is corrected",
					c.Message, "pass %d", pass)
			}

			e.change(func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.Databases.Chino = "chino" })
			e.reconcile()
			got := e.zaentrum()
			rs = restoreStatus(got)
			require.NotNil(t, rs)
			c := meta.FindStatusCondition(got.Status.Conditions, condTypeRestore)
			require.NotNil(t, c)
			if jobEnds {
				assert.Equal(t, zaentrumv1alpha1.RestoreSucceeded, rs.Result, "corrected, it goes on from where it was")
			} else {
				assert.Equal(t, zaentrumv1alpha1.RestoreRunning, rs.Result, "corrected, it runs on")
			}
			assert.Equal(t, rs.Message, c.Message, "its condition says what it says then")
			assert.NotContains(t, c.Message, "waits: the resource is refused")
		})
	}
}

// A corrected resource whose next pass ends before it writes Ready no longer
// reads ResourceRefused: the pass took the refusal back first. One pass ends
// where the cluster cannot say whether it is OpenShift, and writes nothing
// itself; one fails where the Postgres is read.
func TestACorrectedPassTakesTheRefusalBackFirst(t *testing.T) {
	for _, where := range []string{"discovery", "postgres"} {
		t.Run(where, func(t *testing.T) {
			z := verifyCR()
			z.Spec.Databases.Chino = "chi no"
			s := selfScheme(t)
			var failing bool
			mapper := &discoveryFailing{RESTMapper: testrestmapper.TestOnlyStaticRESTMapper(s), failing: func() bool { return failing && where == "discovery" }}
			c := fake.NewClientBuilder().WithScheme(s).WithRESTMapper(mapper).WithObjects(z).
				WithStatusSubresource(&zaentrumv1alpha1.Zaentrum{}, &appsv1.Deployment{}, &batchv1.Job{}).
				WithInterceptorFuncs(interceptor.Funcs{
					Patch: applyAsCreateOrUpdate,
					Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if _, ok := obj.(*appsv1.Deployment); ok && failing && where == "postgres" && key.Name == "postgres" {
							return errors.New("the API server did not answer")
						}
						return cl.Get(ctx, key, obj, opts...)
					},
				}).Build()
			r := &ZaentrumReconciler{Client: c, Scheme: s}
			key := types.NamespacedName{Namespace: z.Namespace, Name: z.Name}
			_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
			require.NoError(t, err)
			var got zaentrumv1alpha1.Zaentrum
			require.NoError(t, c.Get(context.Background(), key, &got))
			require.Equal(t, reasonResourceRefused, readyOf(t, &got).Reason)

			got.Spec.Databases.Chino = "chino"
			require.NoError(t, c.Update(context.Background(), &got))
			failing = true
			_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
			require.NoError(t, c.Get(context.Background(), key, &got))
			if ready := meta.FindStatusCondition(got.Status.Conditions, condTypeReady); ready != nil {
				assert.NotEqual(t, reasonResourceRefused, ready.Reason)
			}
		})
	}

	// A restore in flight while refused says it waits; the corrected pass,
	// though it fails where the Postgres is read, before the restore's step,
	// gives its condition back its own words.
	t.Run("a restore in flight", func(t *testing.T) {
		z := verifyCR()
		z.Annotations = map[string]string{zaentrumv1alpha1.RestoreRequestAnnotation: aDump}
		claim := restorable(z)
		s := selfScheme(t)
		var failing bool
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(z, claim).
			WithStatusSubresource(&zaentrumv1alpha1.Zaentrum{}, &appsv1.Deployment{}, &batchv1.Job{}).
			WithInterceptorFuncs(interceptor.Funcs{
				Patch: applyAsCreateOrUpdate,
				Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*appsv1.Deployment); ok && failing && key.Name == "postgres" {
						return errors.New("the API server did not answer")
					}
					return cl.Get(ctx, key, obj, opts...)
				},
			}).Build()
		r := &ZaentrumReconciler{Client: c, Scheme: s}
		key := types.NamespacedName{Namespace: z.Namespace, Name: z.Name}
		pass := func() *zaentrumv1alpha1.Zaentrum {
			_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
			var got zaentrumv1alpha1.Zaentrum
			require.NoError(t, c.Get(context.Background(), key, &got))
			return &got
		}
		got := pass()
		require.Equal(t, zaentrumv1alpha1.RestoreRunning, restoreStatus(got).Result)
		said := restoreStatus(got).Message

		got.Spec.Databases.Chino = "chi no"
		require.NoError(t, c.Update(context.Background(), got))
		got = pass()
		require.Contains(t, meta.FindStatusCondition(got.Status.Conditions, condTypeRestore).Message, "waits: the resource is refused")

		got.Spec.Databases.Chino = "chino"
		require.NoError(t, c.Update(context.Background(), got))
		failing = true
		got = pass()
		c2 := meta.FindStatusCondition(got.Status.Conditions, condTypeRestore)
		require.NotNil(t, c2)
		assert.Equal(t, said, c2.Message, "the restore's own words, though the pass failed before its step")
	})
}

// discoveryFailing fails the OpenShift check's discovery while failing says so.
type discoveryFailing struct {
	meta.RESTMapper
	failing func() bool
}

func (m *discoveryFailing) RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error) {
	if gk.Kind == "SecurityContextConstraints" && m.failing() {
		return nil, errors.New("the discovery did not answer")
	}
	return m.RESTMapper.RESTMapping(gk, versions...)
}

// restorable makes the resource's restore one R.C10's check takes: the release
// it runs, the set of that release listed on the bound claim of the backups.
func restorable(z *zaentrumv1alpha1.Zaentrum) *corev1.PersistentVolumeClaim {
	z.Spec.Version = "v0.5.0"
	z.Status.Backup = &zaentrumv1alpha1.BackupStatus{Sets: []zaentrumv1alpha1.BackupSet{{Name: aDump, Version: "v0.5.0", Whole: true}}}
	listedOn(z, "backups")
	return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "backups", Namespace: z.Namespace},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}}
}
