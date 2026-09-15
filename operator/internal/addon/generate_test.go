package addon

import (
	"encoding/base64"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const nestedSchema = `{
  "type": "object",
  "properties": {
    "config": {
      "type": "object",
      "properties": {
        "key": {"type": "string", "x-zaentrum-generate": "random-hex-32"},
        "deeper": {"type": "object", "properties": {"salt": {"type": "string", "x-zaentrum-generate": "random-base64-32"}}}
      }
    },
    "a.b": {"type": "string", "x-zaentrum-generate": "random-hex-32"},
    "plain": {"type": "string"}
  }
}`

func TestGeneratedFields(t *testing.T) {
	fields, err := GeneratedFields([]byte(nestedSchema))
	require.NoError(t, err)
	assert.Equal(t, []GeneratedField{
		{Path: "config.deeper.salt", Generator: GenerateRandomBase64},
		{Path: "config.key", Generator: GenerateRandomHex},
	}, fields, "nested by dotted path; a dotted key cannot be addressed and is skipped")

	fields, err = GeneratedFields(exampleChart(t).Schema)
	require.NoError(t, err)
	assert.Equal(t, []GeneratedField{{Path: "auth.signingKey", Generator: GenerateRandomBase64}}, fields)

	fields, err = GeneratedFields(nil)
	require.NoError(t, err)
	assert.Empty(t, fields, "a chart without a schema generates nothing")

	_, err = GeneratedFields([]byte("{broken"))
	assert.Error(t, err)
}

func TestGenerateMissing(t *testing.T) {
	fields := []GeneratedField{
		{Path: "config.key", Generator: GenerateRandomHex},
		{Path: "config.salt", Generator: GenerateRandomBase64},
		{Path: "given", Generator: GenerateRandomHex},
		{Path: "empty", Generator: GenerateRandomHex},
		{Path: "stored", Generator: GenerateRandomHex},
	}
	user := map[string]interface{}{"given": "from-user", "empty": ""}
	existing := map[string][]byte{"stored": []byte("kept")}

	gen, errs, err := GenerateMissing(fields, user, existing)
	require.NoError(t, err)
	require.Empty(t, errs)

	assert.NotContains(t, gen, "given", "a user value is never replaced")
	assert.NotContains(t, gen, "stored", "an existing key is never generated again")
	assert.Contains(t, gen, "empty", "an empty value counts as no value")

	key, err := hex.DecodeString(gen["config.key"])
	require.NoError(t, err)
	assert.Len(t, key, 32)
	salt, err := base64.StdEncoding.DecodeString(gen["config.salt"])
	require.NoError(t, err)
	assert.Len(t, salt, 32)

	again, _, err := GenerateMissing(fields, user, existing)
	require.NoError(t, err)
	assert.NotEqual(t, gen["config.key"], again["config.key"], "values are random")

	_, errs, err = GenerateMissing([]GeneratedField{{Path: "x", Generator: "random-words"}}, nil, nil)
	require.NoError(t, err)
	require.Len(t, errs, 1)
	assert.Contains(t, errs[0], `x: unknown x-zaentrum-generate "random-words"`)
}

// Only keys for fields the schema declares are injected; a key left behind by
// an older chart version stays stored but out of the values.
func TestGeneratedValues(t *testing.T) {
	fields := []GeneratedField{{Path: "config.key", Generator: GenerateRandomHex}}
	data := map[string][]byte{"config.key": []byte("abc"), "dropped.field": []byte("old")}
	assert.Equal(t, map[string]string{"config.key": "abc"}, GeneratedValues(fields, data))
}

// Generated values satisfy a required schema property: generation happens
// before validation.
func TestGeneratedValueSatisfiesRequired(t *testing.T) {
	chrt := exampleChart(t)
	fields, err := GeneratedFields(chrt.Schema)
	require.NoError(t, err)
	user := map[string]interface{}{"auth": map[string]interface{}{"token": "t0ken"}}
	gen, errs, err := GenerateMissing(fields, user, nil)
	require.NoError(t, err)
	require.Empty(t, errs)

	vals := LayerValues(gen, user, platformValues())
	out, errs := Render(RenderInput{Chart: chrt, Name: "example", Namespace: testNamespace, Values: vals})
	assert.Empty(t, errs)
	assert.NotNil(t, out)

	_, errs = Render(RenderInput{Chart: exampleChart(t), Name: "example", Namespace: testNamespace,
		Values: LayerValues(nil, user, platformValues())})
	assert.NotEmpty(t, errs, "without the generated value the required signingKey is missing")
}
