//go:build !skiff

package config

import (
	"bytes"
	"encoding/json"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/curio/deps"
	depsconfig "github.com/filecoin-project/curio/deps/config"
)

func editorJSON(t *testing.T, layer string) map[string]any {
	t.Helper()
	m, err := uiLayerJSON(layer)
	require.NoError(t, err)
	b, err := json.Marshal(m)
	require.NoError(t, err)
	var submitted map[string]any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	require.NoError(t, d.Decode(&submitted))
	return submitted
}

func TestUICustomConfigRoundTrip(t *testing.T) {
	for _, profile := range []struct {
		name, interval string
		tasks          int
	}{{"pc1", "43m45s", 4}, {"pc1-sm", "25m20s", 6}} {
		t.Run(profile.name, func(t *testing.T) {
			text := `[Subsystems]
SealSDRMaxTasks = 4
SealSDRMinStartInterval = "43m45s"
SealSDRStartJitter = true
SealSDRMinTasks = 7
[Ingest]
MK20PipelineInsertBatch = 3
MK20PipelineInsertMaxActive = 17
MaxQueueSDR = 0
MaxQueueTrees = 9
MaxQueuePoRep = 11
MaxQueueDealSector = 12
MaxQueueDownload = 13
MaxQueueCommP = 14
MaxMarketRunningPipelines = 15
MaxQueueSnapEncode = 16
MaxQueueSnapProve = 18
MaxDealWaitTime = "2h3m"
`
			m := editorJSON(t, text)
			sub := m["Subsystems"].(map[string]any)
			sub["SealSDRMaxTasks"] = json.Number(strconv.Itoa(profile.tasks))
			sub["SealSDRMinStartInterval"] = profile.interval
			for _, edit := range []bool{false, true} {
				if edit {
					sub["SealSDRMaxTasks"] = json.Number("8")
				}
				out, err := uiPrepareLayerSave("synthetic", m, text)
				require.NoError(t, err)
				cfg := depsconfig.DefaultCurioConfig()
				_, err = depsconfig.LoadConfigWithUpgrades(out, cfg)
				require.NoError(t, err)
				want, _ := time.ParseDuration(profile.interval)
				require.Equal(t, want, cfg.Subsystems.SealSDRMinStartInterval)
				require.True(t, cfg.Subsystems.SealSDRStartJitter)
				require.Equal(t, 3, cfg.Ingest.MK20PipelineInsertBatch.Get())
				require.Equal(t, 17, cfg.Ingest.MK20PipelineInsertMaxActive.Get())
				require.Equal(t, 0, cfg.Ingest.MaxQueueSDR.Get())
				require.Equal(t, editorJSON(t, out), m, "every explicit setting survives, including default/zero values; no unrelated defaults added")
			}
		})
	}
}

func TestUILayerPreservesExplicitDefaults(t *testing.T) {
	const layer = `[Subsystems]
SealSDRMinStartInterval = "0s"
SealSDRStartJitter = false
[Ingest]
MK20PipelineInsertBatch = 0
MK20PipelineInsertMaxActive = 0
MaxQueueSDR = 8
`
	m := editorJSON(t, layer)
	out, err := uiPrepareLayerSave("override", m, layer)
	require.NoError(t, err)
	require.Equal(t, m, editorJSON(t, out), "a layer default is still an override of earlier layers")
}

func TestUILayerUnknownKeysFailClosed(t *testing.T) {
	const layer = "[Subsystems]\nSealSDRStartJitter=true\nFuturePersonalOption=23\n"
	_, err := uiLayerJSON(layer)
	require.Error(t, err)
	m := map[string]any{"Subsystems": map[string]any{"SealSDRStartJitter": true, "FuturePersonalOption": json.Number("23")}}
	_, err = uiPrepareLayerSave("unknown", m, layer)
	require.Error(t, err)
	delete(m["Subsystems"].(map[string]any), "FuturePersonalOption")
	_, err = uiPrepareLayerSave("unknown", m, layer)
	require.Error(t, err, "editor omission must not silently destroy an existing unknown key")
}

func TestUICustomSchemaSemantics(t *testing.T) {
	root := schemaMap(t, buildUISchema())
	node, err := schemaNode(root, root)
	require.NoError(t, err)
	for path, want := range map[string]string{
		"Subsystems.SealSDRMinStartInterval": "string", "Subsystems.SealSDRStartJitter": "boolean",
		"Subsystems.SealSDRMaxTasks": "integer", "Subsystems.SealSDRMinTasks": "integer",
		"Ingest.MK20PipelineInsertBatch": "integer", "Ingest.MK20PipelineInsertMaxActive": "integer",
		"Ingest.MaxQueueSDR": "integer", "Ingest.MaxQueueTrees": "integer", "Ingest.MaxQueuePoRep": "integer",
		"Ingest.MaxQueueDealSector": "integer", "Ingest.MaxMarketRunningPipelines": "integer",
	} {
		current := node
		for _, key := range strings.Split(path, ".") {
			props := current["properties"].(map[string]any)
			p, ok := props[key].(map[string]any)
			require.True(t, ok, path)
			current, err = schemaNode(root, p)
			require.NoError(t, err, path)
		}
		require.Equal(t, want, current["type"], path)
		if path == "Subsystems.SealSDRMinStartInterval" {
			re := regexp.MustCompile(current["pattern"].(string))
			for _, s := range []string{"43m45s", "25m20s", "0s", "0", ".5h"} {
				require.True(t, re.MatchString(s), s)
				_, err = time.ParseDuration(s)
				require.NoError(t, err)
			}
		}
	}
}

func TestUILayerCanonicalKeysAndNestedValues(t *testing.T) {
	const layer = `[subsystems]
sealsdrminstartinterval = "43m45s"
sealsdrstartjitter = true
[market.storagemarketconfig.mk12]
publishmsgperiod = "1m"
[[addresses]]
mineraddresses = ["t01000"]
[addresses.balancemanager.mk12collateral]
collaterallowthreshold = "3 FIL"
collateralhighthreshold = "7 FIL"
[[market.storagemarketconfig.piecelocator]]
URL = "https://example.invalid"
[market.storagemarketconfig.piecelocator.Headers]
X-Custom = ["", "value"]
`
	m := editorJSON(t, layer)
	require.Equal(t, "43m45s", m["Subsystems"].(map[string]any)["SealSDRMinStartInterval"])
	out, err := uiPrepareLayerSave("synthetic", m, layer)
	require.NoError(t, err)
	require.Equal(t, m, editorJSON(t, out))
	cfg := depsconfig.DefaultCurioConfig()
	_, err = depsconfig.LoadConfigWithUpgrades(out, cfg)
	require.NoError(t, err)
	require.Equal(t, "3 FIL", cfg.Addresses.Get()[0].BalanceManager.MK12Collateral.CollateralLowThreshold.String())
}

func TestUILayerDefaultOverridesEarlierLayer(t *testing.T) {
	const earlier = "[Subsystems]\nSealSDRStartJitter=true\nSealSDRMinStartInterval=\"1h\"\n[Ingest]\nMK20PipelineInsertBatch=9\n"
	const override = "[Subsystems]\nSealSDRStartJitter=false\nSealSDRMinStartInterval=\"0s\"\n[Ingest]\nMK20PipelineInsertBatch=0\n"
	out, err := uiPrepareLayerSave("override", editorJSON(t, override), override)
	require.NoError(t, err)
	cfg := depsconfig.DefaultCurioConfig()
	_, err = depsconfig.LoadConfigWithUpgrades(earlier, cfg)
	require.NoError(t, err)
	_, err = depsconfig.LoadConfigWithUpgrades(out, cfg)
	require.NoError(t, err)
	require.False(t, cfg.Subsystems.SealSDRStartJitter)
	require.Zero(t, cfg.Subsystems.SealSDRMinStartInterval)
	require.Zero(t, cfg.Ingest.MK20PipelineInsertBatch.Get())
}

func TestUILayerLegacyAddressesAndInvalidValues(t *testing.T) {
	const legacy = "[addresses]\nMinerAddresses=[\"t01000\"]\n"
	m := editorJSON(t, legacy)
	_, ok := m["Addresses"].([]any)
	require.True(t, ok, "legacy single address table must become a schema-compatible array")
	out, err := uiPrepareLayerSave("legacy", m, legacy)
	require.NoError(t, err)
	require.Equal(t, m, editorJSON(t, out))
	for _, invalid := range []string{
		"[Subsystems]\nSealSDRMinStartInterval=\"forever\"\n",
		"[Fees]\nDefaultMaxFee=\"not money\"\n",
		"[Subsystems]\nSealSDRStartJitter=\"true\"\n",
	} {
		_, err := uiLayerJSON(invalid)
		require.Error(t, err, "runtime decoder still validates typed values")
	}
}

func TestUIDefaultConfigurationDocumentation(t *testing.T) {
	// Match the generator's source of truth, not a hand-maintained field list.
	defaults, err := deps.GetDefaultConfig(true)
	require.NoError(t, err)
	doc, err := os.ReadFile("../../../documentation/en/configuration/default-curio-configuration.md")
	require.NoError(t, err)
	want := "---\ndescription: The default curio configuration\n---\n\n# Default Curio Configuration\n\n```toml\n" + defaults + "```\n"
	require.Equal(t, want, string(doc), "regenerate with the config-default command used by docsgen-cli")
}
