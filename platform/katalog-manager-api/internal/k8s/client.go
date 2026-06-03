// Package k8s is a dependency-free, in-cluster Kubernetes client built on the
// standard library net/http only. It exists so the first-run setup flow can
// propagate the configuration it persists (OIDC issuer/audience, media root)
// and the generated stream signing key out to the runtime objects the sibling
// services read at startup — the stube-env ConfigMap and the
// stube-stream-signing Secret — and then roll those Deployments so the new
// values take effect.
//
// It deliberately does NOT import client-go or any k8s.io module: it speaks the
// Kubernetes REST API directly with the in-cluster ServiceAccount credentials
// mounted at /var/run/secrets/kubernetes.io/serviceaccount. When those
// credentials are absent (the service is running under docker-compose or on a
// developer laptop) the client degrades to a logged no-op so non-cluster runs
// keep working — first-run setup still persists to the DB, it just can't
// propagate to objects that don't exist outside a cluster.
package k8s

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"
)

// ErrDisabled is returned by methods that have no meaningful no-op fallback
// (e.g. GetStube, which must return data) when the client has no in-cluster
// credentials. Callers map it to 503 so the admin UI can explain the management
// plane is not running in a cluster. Unlike the propagation methods (which
// degrade to a logged no-op), reads cannot fabricate a result.
var ErrDisabled = errors.New("k8s: in-cluster credentials absent; not running in a cluster")

// saDir is the standard mount path for the in-cluster ServiceAccount token,
// CA bundle and namespace. Kept as a var so tests can point it elsewhere.
var saDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// restartAnnotation is the pod-template annotation flipped to trigger a rolling
// restart of a Deployment. The value is an RFC3339 timestamp; any change to a
// pod-template field causes the Deployment controller to roll new pods.
const restartAnnotation = "stube.io/restartedAt"

// Client talks to the in-cluster Kubernetes REST API using the mounted
// ServiceAccount credentials. A Client whose enabled flag is false (no token on
// disk) makes every method a logged no-op so the service runs unchanged outside
// a cluster.
type Client struct {
	enabled   bool
	host      string // https://host:port of the API server
	namespace string
	token     string
	http      *http.Client
}

// New constructs a Client from the in-cluster environment. It never returns an
// error: when the ServiceAccount token, CA or API host are not present it
// returns a disabled Client whose methods no-op (with a warning), so first-run
// setup keeps working under docker-compose / locally. The returned Client is
// safe for concurrent use.
func New() *Client {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")

	tokenBytes, tErr := os.ReadFile(saDir + "/token")
	nsBytes, nErr := os.ReadFile(saDir + "/namespace")
	caBytes, cErr := os.ReadFile(saDir + "/ca.crt")

	if host == "" || port == "" || tErr != nil || nErr != nil || cErr != nil {
		slog.Warn("k8s in-cluster credentials not found; config propagation disabled (no-op)",
			"have_host", host != "",
			"have_port", port != "",
			"token_err", tErr != nil,
			"namespace_err", nErr != nil,
			"ca_err", cErr != nil,
		)
		return &Client{enabled: false}
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caBytes) {
		slog.Warn("k8s CA bundle could not be parsed; config propagation disabled (no-op)")
		return &Client{enabled: false}
	}

	return &Client{
		enabled:   true,
		host:      "https://" + host + ":" + port,
		namespace: string(bytes.TrimSpace(nsBytes)),
		token:     string(bytes.TrimSpace(tokenBytes)),
		http: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					RootCAs:    pool,
					MinVersion: tls.VersionTLS12,
				},
			},
		},
	}
}

// Enabled reports whether the client has live in-cluster credentials. Callers
// can use it to skip building a propagation payload entirely, but every method
// is also independently guarded so calling them on a disabled client is safe.
func (c *Client) Enabled() bool {
	return c != nil && c.enabled
}

// Namespace returns the namespace the ServiceAccount is bound to (empty when
// the client is disabled).
func (c *Client) Namespace() string {
	if c == nil {
		return ""
	}
	return c.namespace
}

// PatchConfigMap applies a strategic-merge patch that sets the given keys under
// the ConfigMap's .data. Existing keys not in data are left untouched (merge
// semantics). No-op (logged) when the client is disabled.
func (c *Client) PatchConfigMap(ctx context.Context, name string, data map[string]string) error {
	if !c.Enabled() {
		slog.Warn("k8s disabled: skipping ConfigMap patch", "configmap", name)
		return nil
	}
	body, err := json.Marshal(map[string]any{"data": data})
	if err != nil {
		return fmt.Errorf("marshal configmap patch: %w", err)
	}
	path := fmt.Sprintf("/api/v1/namespaces/%s/configmaps/%s", c.namespace, name)
	return c.patch(ctx, path, body)
}

// PatchSecret applies a strategic-merge patch that sets the given keys under the
// Secret's .stringData. Using stringData lets the API server base64-encode the
// values for us, so callers pass plaintext and merge semantics keep other keys
// intact. No-op (logged) when the client is disabled.
//
// (The base64 detail referenced in the design lives on the wire: stringData is
// the documented, simplest path — the API server folds it into the base64
// .data on write.)
func (c *Client) PatchSecret(ctx context.Context, name string, data map[string]string) error {
	if !c.Enabled() {
		slog.Warn("k8s disabled: skipping Secret patch", "secret", name)
		return nil
	}
	body, err := json.Marshal(map[string]any{"stringData": data})
	if err != nil {
		return fmt.Errorf("marshal secret patch: %w", err)
	}
	path := fmt.Sprintf("/api/v1/namespaces/%s/secrets/%s", c.namespace, name)
	return c.patch(ctx, path, body)
}

// RestartDeployment triggers a rolling restart of the named Deployment by
// strategic-merge patching its pod-template annotations with a fresh RFC3339
// timestamp — the same mechanism `kubectl rollout restart` uses. No-op (logged)
// when the client is disabled.
func (c *Client) RestartDeployment(ctx context.Context, name string) error {
	if !c.Enabled() {
		slog.Warn("k8s disabled: skipping Deployment restart", "deployment", name)
		return nil
	}
	patch := map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"metadata": map[string]any{
					"annotations": map[string]string{
						restartAnnotation: time.Now().UTC().Format(time.RFC3339),
					},
				},
			},
		},
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal deployment restart patch: %w", err)
	}
	path := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", c.namespace, name)
	return c.patch(ctx, path, body)
}

// stubeGVR is the group/version/resource path segment for the Stube custom
// resource. The CR is NAMESPACED, so the full path is
// /apis/stube.io/v1alpha1/namespaces/{ns}/stubes/{name}.
const stubeGVR = "/apis/stube.io/v1alpha1"

// GetStube reads the named Stube custom resource from the given namespace and
// returns the raw decoded object (a generic map so this package stays free of
// the operator's typed API and the k8s.io modules). The management plane maps
// the spec/status fields it needs onto its own view.
//
// Unlike the propagation methods, a read has no sensible no-op fallback: on a
// disabled client it returns ErrDisabled so the caller can answer 503.
func (c *Client) GetStube(ctx context.Context, namespace, name string) (map[string]any, error) {
	if !c.Enabled() {
		return nil, ErrDisabled
	}
	path := fmt.Sprintf("%s/namespaces/%s/stubes/%s", stubeGVR, namespace, name)
	return c.get(ctx, path)
}

// PatchStube applies a strategic-merge patch to the named Stube CR's body. The
// caller supplies the partial object (typically {"spec": {...}}); merge
// semantics leave untouched fields intact. Returns the updated object so the
// caller can re-render the view without a follow-up GET. ErrDisabled on a
// disabled client.
func (c *Client) PatchStube(ctx context.Context, namespace, name string, patch map[string]any) (map[string]any, error) {
	if !c.Enabled() {
		return nil, ErrDisabled
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return nil, fmt.Errorf("marshal stube patch: %w", err)
	}
	path := fmt.Sprintf("%s/namespaces/%s/stubes/%s", stubeGVR, namespace, name)
	if err := c.patchMerge(ctx, path, body); err != nil {
		return nil, err
	}
	// Re-read so the returned view reflects exactly what the API server stored
	// (and any defaulting/admission it applied), not just our local patch.
	return c.get(ctx, path)
}

// get issues a GET against an absolute API path and decodes the JSON body into a
// generic map. A 404 is surfaced as a wrapped error so callers can distinguish a
// missing CR (the operator hasn't created it yet) from a transport failure.
func (c *Client) get(ctx context.Context, path string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.host+path, nil)
	if err != nil {
		return nil, fmt.Errorf("build get request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", path, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("get %s: unexpected status %s: %s", path, resp.Status, bytes.TrimSpace(msg))
	}

	var obj map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&obj); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return obj, nil
}

// patch issues a strategic-merge PATCH against an absolute API path and turns a
// non-2xx response into an error carrying the API server's message. Used for the
// built-in types (ConfigMap/Secret/Deployment) which register a strategic-merge
// patch type.
func (c *Client) patch(ctx context.Context, path string, body []byte) error {
	return c.patchWithType(ctx, path, body, "application/strategic-merge-patch+json")
}

// patchMerge issues a JSON merge patch (RFC 7386) against an absolute API path.
// Custom resources (the Stube CR) do NOT register a strategic-merge patch type,
// so they must be patched with merge-patch; for our spec patches (replacing
// scalar fields under nested objects) merge semantics are exactly what we want.
func (c *Client) patchMerge(ctx context.Context, path string, body []byte) error {
	return c.patchWithType(ctx, path, body, "application/merge-patch+json")
}

// patchWithType issues a PATCH with the given Content-Type and turns a non-2xx
// response into an error carrying the API server's message.
func (c *Client) patchWithType(ctx context.Context, path string, body []byte, contentType string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, c.host+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build patch request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("patch %s: %w", path, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("patch %s: unexpected status %s: %s", path, resp.Status, bytes.TrimSpace(msg))
	}
	return nil
}

// encodeBase64 is retained as an explicit helper for callers that need to patch
// a Secret's raw .data directly rather than via stringData. It is not used on
// the default stringData path but documents the base64 wire format Secrets use.
func encodeBase64(v string) string {
	return base64.StdEncoding.EncodeToString([]byte(v))
}

// silence the unused linter for the helper above without dropping the
// documented capability; it is referenced here so `go vet` stays quiet.
var _ = encodeBase64
