package k8s

import (
	"context"
	"testing"
)

// TestNewDisabledOutsideCluster verifies that without in-cluster credentials
// the client degrades to a no-op rather than crashing, so first-run setup keeps
// working under docker-compose / on a developer laptop.
func TestNewDisabledOutsideCluster(t *testing.T) {
	// Point the SA mount at a path that does not exist and clear the API host.
	saDir = t.TempDir() + "/missing"
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")

	c := New()
	if c.Enabled() {
		t.Fatal("client should be disabled without in-cluster credentials")
	}
	if c.Namespace() != "" {
		t.Errorf("disabled client namespace = %q, want empty", c.Namespace())
	}

	ctx := context.Background()
	// Every method must no-op (return nil) on a disabled client.
	if err := c.PatchConfigMap(ctx, "stube-env", map[string]string{"OIDC_ISSUER": "x"}); err != nil {
		t.Errorf("PatchConfigMap on disabled client = %v, want nil", err)
	}
	if err := c.PatchSecret(ctx, "stube-stream-signing", map[string]string{"key": "x"}); err != nil {
		t.Errorf("PatchSecret on disabled client = %v, want nil", err)
	}
	if err := c.RestartDeployment(ctx, "chino-api"); err != nil {
		t.Errorf("RestartDeployment on disabled client = %v, want nil", err)
	}
}

// TestEncodeBase64 documents the Secret .data wire format the stringData path
// folds into; it keeps the helper exercised.
func TestEncodeBase64(t *testing.T) {
	if got := encodeBase64("abc"); got != "YWJj" {
		t.Errorf("encodeBase64(abc) = %q, want YWJj", got)
	}
}
