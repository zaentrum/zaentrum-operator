package templates

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
)

// The catalog's base schema, files/sql/schema-postgres.sql, which
// katalog-manager-api's init container applies on every start (ConfigMap
// katalog-schema), makes the catalog's tables and their views — and no longer
// the job table of an integration the core does not carry, nor the view it was
// read through. katalog-manager's migration 035 drops that table while it is
// empty, and a base schema that made it again had it dropped, and logged, on
// every start. Every profile ships the same file, as it is.
func TestBaseSchemaMakesTheCatalogAndNoRetiredJobTable(t *testing.T) {
	file, err := os.ReadFile("../../platform/chart/files/sql/schema-postgres.sql")
	require.NoError(t, err)

	ext := base("zaentrum-beta")
	ext.Spec.Identity.Mode = zaentrumv1alpha1.IdentityExternal
	ext.Spec.Identity.Issuer = "https://sso.example.org/realms/example"
	ext.Spec.Databases.Mode = "external"
	ext.Spec.Databases.External.Host = "postgres.example.org"
	for name, objs := range map[string][]*unstructured.Unstructured{
		"operator":     renderCR(t, base("zaentrum")),
		"demo":         renderCR(t, demoCR("zaentrum-demo")),
		"external":     renderCR(t, ext),
		"helm install": helmRender(t, nil),
	} {
		cm := find(t, objs, "ConfigMap", "katalog-schema")
		require.NotNil(t, cm, name)
		schema, _, _ := unstructured.NestedString(cm.Object, "data", "schema-postgres.sql")
		assert.Equal(t, string(file), schema, "%s: the base schema as shipped", name)

		assert.Equal(t, []string{
			"com_nalet_katalog_Items", "com_nalet_katalog_ItemExternalIds", "com_nalet_katalog_ItemArtwork",
			"com_nalet_katalog_ItemArtworkData", "com_nalet_katalog_PlaybackAssets", "com_nalet_katalog_SubtitleAssets",
			"com_nalet_katalog_MediaSegments", "com_nalet_katalog_ItemTrailerLinks", "com_nalet_katalog_ItemDiagnostics",
			"com_nalet_katalog_ItemProcessingSteps", "com_nalet_katalog_ItemGenres", "com_nalet_katalog_Genres",
			"com_nalet_katalog_ItemPeople", "com_nalet_katalog_People", "com_nalet_katalog_ItemTags",
			"com_nalet_katalog_ScanJobs", "com_nalet_katalog_ItemChapters", "com_nalet_katalog_EnrichmentStatusCodes",
			"com_nalet_katalog_Settings", "com_nalet_katalog_EnrichmentJobs",
		}, made(schema, `CREATE TABLE IF NOT EXISTS (\S+) \(`), "%s: the tables", name)
		assert.Equal(t, []string{
			"KatalogService_Items", "KatalogService_ItemExternalIds", "KatalogService_ItemArtwork",
			"KatalogService_ItemArtworkData", "KatalogService_PlaybackAssets", "KatalogService_SubtitleAssets",
			"KatalogService_MediaSegments", "KatalogService_ItemTrailerLinks", "KatalogService_ItemDiagnostics",
			"KatalogService_ItemProcessingSteps", "KatalogService_ItemOverallStatus", "KatalogService_ItemGenres",
			"KatalogService_Genres", "KatalogService_ItemPeople", "KatalogService_People", "KatalogService_ItemTags",
			"KatalogService_ScanJobs", "KatalogService_ItemChapters", "KatalogService_EnrichmentStatusCodes",
			"KatalogService_Settings", "KatalogService_Movies", "KatalogService_Series", "KatalogService_Episodes",
			"KatalogService_Albums",
		}, made(schema, `CREATE OR REPLACE VIEW (\S+) AS`), "%s: the views", name)

		lower := strings.ToLower(schema)
		for _, gone := range []string{"downloadjobs", "wanteditemid", "clientjobid"} {
			assert.False(t, strings.Contains(lower, gone), "%s: the base schema makes the retired job table again (%s)", name, gone)
		}
		// The trailer links keep their columns: katalog-manager reads them.
		assert.True(t, strings.Contains(schema, "  downloadedAt TIMESTAMP,\n  localPath VARCHAR(2048),\n"),
			"%s: the trailer links lost a column katalog-manager reads", name)
	}
}

// made lists what the statements matching pattern make, in the file's order.
func made(schema, pattern string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`(?m)^`+pattern).FindAllStringSubmatch(schema, -1) {
		out = append(out, m[1])
	}
	return out
}
