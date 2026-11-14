package engine_test

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/intentd/kome/engine"
)

const key = "kome.intentd.io/migrate-field-snapshot"

func randomText(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(b)[:n]
}

func TestEncodeRecord_JSONWhenItFits(t *testing.T) {
	raw, err := engine.EncodeRecord(map[string]any{"replicas": 3}, key, map[string]string{"note": "x"})
	require.NoError(t, err)
	assert.Equal(t, `{"replicas":3}`, raw, "readable when it fits")

	var out map[string]any
	require.NoError(t, engine.DecodeRecord(raw, &out))
	assert.Equal(t, float64(3), out["replicas"])
}

func TestEncodeRecord_CompressedWhenOnlyThatFits(t *testing.T) {
	rec := map[string]any{"image": strings.Repeat("nginx:1.27 ", 30*1024)} // ~330 KiB, compresses well
	raw, err := engine.EncodeRecord(rec, key, nil)
	require.NoError(t, err)
	assert.False(t, strings.HasPrefix(raw, "{"))
	assert.Less(t, len(raw), 64*1024)

	var out map[string]any
	require.NoError(t, engine.DecodeRecord(raw, &out))
	assert.Equal(t, rec["image"], out["image"])
}

func TestEncodeRecord_OtherAnnotationsCount(t *testing.T) {
	// The record alone fits; with a large annotation beside it, only
	// compressed does.
	rec := map[string]any{"image": strings.Repeat("a", 100*1024)}
	ann := map[string]string{"kubectl.kubernetes.io/last-applied-configuration": randomText(t, 200*1024)}
	raw, err := engine.EncodeRecord(rec, key, ann)
	require.NoError(t, err)
	assert.False(t, strings.HasPrefix(raw, "{"))
}

func TestEncodeRecord_TooLargeEvenCompressed(t *testing.T) {
	_, err := engine.EncodeRecord(map[string]any{"image": randomText(t, 300*1024)}, key, nil)
	assert.ErrorContains(t, err, "the record is too large, even compressed")
}

func TestDecodeRecord_Invalid(t *testing.T) {
	var out map[string]any
	assert.Error(t, engine.DecodeRecord("not a record", &out))
	assert.Error(t, engine.DecodeRecord("{broken", &out))
}
