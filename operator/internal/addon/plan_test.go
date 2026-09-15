package addon

import (
	"testing"

	"github.com/stretchr/testify/assert"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
)

func TestSummarize(t *testing.T) {
	objs, workloads := Summarize(renderExample(t, nil).Objects)
	assert.Equal(t, []zaentrumv1alpha1.AddonObject{
		{Kind: "Deployment", Name: "worker"},
		{Kind: "Secret", Name: "worker"},
		{Kind: "Service", Name: "worker"},
	}, objs)
	assert.Equal(t, []zaentrumv1alpha1.AddonWorkload{{
		Kind: "Deployment", Name: "worker",
		Images: []string{"registry.example.org/example/worker:1.0.0"},
		Ports:  []int32{8080},
	}}, workloads)
}

func TestChanges(t *testing.T) {
	rendered := renderExample(t, map[string]interface{}{
		"auth":  map[string]interface{}{"token": "t0ken"},
		"image": "registry.example.org/example/worker:1.1.0",
	}).Objects

	assert.Equal(t, &zaentrumv1alpha1.AddonPlanChanges{
		Added: []string{"Deployment/worker", "Secret/worker", "Service/worker"},
	}, Changes(rendered, nil), "nothing applied yet: everything is added")

	live := []LiveObject{
		{Kind: "Deployment", Name: "worker", Images: map[string]string{"worker": "registry.example.org/example/worker:1.0.0"}},
		{Kind: "Service", Name: "worker"},
		{Kind: "ConfigMap", Name: "worker-legacy"},
	}
	assert.Equal(t, &zaentrumv1alpha1.AddonPlanChanges{
		Added:   []string{"Secret/worker"},
		Removed: []string{"ConfigMap/worker-legacy"},
		Images: []string{
			"Deployment/worker: registry.example.org/example/worker:1.0.0 → registry.example.org/example/worker:1.1.0",
		},
	}, Changes(rendered, live))
}

// Prune selects exactly the applied objects the new render dropped.
func TestPrune(t *testing.T) {
	rendered := renderExample(t, nil).Objects
	live := []LiveObject{
		{Kind: "Deployment", Name: "worker"},
		{Kind: "ConfigMap", Name: "worker-legacy"},
		{Kind: "Job", Name: "migrate-v1"},
		{Kind: "Service", Name: "worker"},
		{Kind: "Secret", Name: "worker"},
	}
	assert.Equal(t, []LiveObject{
		{Kind: "ConfigMap", Name: "worker-legacy"},
		{Kind: "Job", Name: "migrate-v1"},
	}, Prune(rendered, live))
	assert.Empty(t, Prune(rendered, nil))
}

func TestLive(t *testing.T) {
	dep := findObject(renderExample(t, nil).Objects, "Deployment", "worker")
	assert.Equal(t, LiveObject{
		Kind: "Deployment", Name: "worker",
		Images: map[string]string{"worker": "registry.example.org/example/worker:1.0.0"},
	}, Live(dep))
}
