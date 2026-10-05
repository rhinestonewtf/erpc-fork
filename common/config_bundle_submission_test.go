package common

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// Fork patch (RHI-7827) tests. Loading the YAML below also fails if a sync
// drops the config fields: config decoding is strict (KnownFields).

// A well-known throwaway development key (Hardhat account #0). Tests only.
const bundleTestSigningKey = "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"

func bundleSubmissionYaml(networkEvm, networkDefaults, relayJsonRpc string) string {
	return fmt.Sprintf(`
logLevel: warn
projects:
  - id: main
%s
    networks:
      - architecture: evm
        evm:
          chainId: 1
%s
    upstreams:
      - id: public
        endpoint: http://rpc1.localhost
        evm:
          chainId: 1
      - id: flashbots-relay
        endpoint: http://relay.localhost
        evm:
          chainId: 1
        tags: [bundle-relay]
        ignoreMethods: ['*']
        allowMethods: [eth_sendBundle]
        jsonRpc:
%s
`, networkDefaults, networkEvm, relayJsonRpc)
}

const (
	bundleSubmissionBlock = `          bundleSubmission:
            useUpstream: bundle-relay
            bundleFields:
              builders: [flashbots, Titan]`
	signingJsonRpc = `          supportsBatch: false
          flashbotsSigningKey: "` + bundleTestSigningKey + `"`
)

func loadBundleTestConfig(t *testing.T, content string) (*Config, error) {
	t.Helper()
	fs := afero.NewMemMapFs()
	f, err := afero.TempFile(fs, "", "erpc.yaml")
	require.NoError(t, err)
	_, err = f.WriteString(content)
	require.NoError(t, err)
	return LoadConfig(fs, f.Name(), &DefaultOptions{})
}

func TestBundleSubmissionConfig_LoadsFromYaml(t *testing.T) {
	cfg, err := loadBundleTestConfig(t, bundleSubmissionYaml(bundleSubmissionBlock, "", signingJsonRpc))
	require.NoError(t, err)

	var bs *BundleSubmissionConfig
	for _, n := range cfg.Projects[0].Networks {
		if n.Evm != nil && n.Evm.ChainId == 1 {
			bs = n.Evm.BundleSubmission
		}
	}
	require.NotNil(t, bs)
	assert.Equal(t, "bundle-relay", bs.UseUpstream)
	assert.Equal(t, DefaultBundleSubmissionTargetBlocks, bs.TargetBlocks, "targetBlocks defaults to 3")
	assert.Equal(t, DefaultBundleSubmissionResubmitFor, bs.ResubmitFor, "resubmitFor defaults to 5m")
	assert.Equal(t, []interface{}{"flashbots", "Titan"}, bs.BundleFields["builders"])

	var key SecretString
	for _, u := range cfg.Projects[0].Upstreams {
		if u.Id == "flashbots-relay" {
			key = u.JsonRpc.FlashbotsSigningKey
		}
	}
	assert.Equal(t, SecretString(bundleTestSigningKey), key)
}

func TestBundleSubmissionConfig_RejectedInNetworkDefaults(t *testing.T) {
	defaults := `    networkDefaults:
      evm:
` + strings.ReplaceAll(bundleSubmissionBlock, "          ", "        ")

	_, err := loadBundleTestConfig(t, bundleSubmissionYaml("", defaults, signingJsonRpc))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "networkDefaults.evm.bundleSubmission is not allowed")
}

func TestBundleSubmissionConfig_Validate(t *testing.T) {
	valid := func() *BundleSubmissionConfig {
		return &BundleSubmissionConfig{UseUpstream: "bundle-relay", TargetBlocks: 3}
	}
	tests := map[string]struct {
		mutate  func(c *BundleSubmissionConfig)
		wantErr string
	}{
		"valid":                {mutate: func(c *BundleSubmissionConfig) {}},
		"missing useUpstream":  {mutate: func(c *BundleSubmissionConfig) { c.UseUpstream = " " }, wantErr: "useUpstream is required"},
		"targetBlocks below 1": {mutate: func(c *BundleSubmissionConfig) { c.TargetBlocks = -1 }, wantErr: "targetBlocks must be at least 1"},
		"negative resubmitFor": {mutate: func(c *BundleSubmissionConfig) { c.ResubmitFor = -1 }, wantErr: "resubmitFor must not be negative"},
		"bundleFields sets txs": {
			mutate:  func(c *BundleSubmissionConfig) { c.BundleFields = map[string]interface{}{"txs": []string{"0x01"}} },
			wantErr: `must not set "txs"`,
		},
		"bundleFields sets blockNumber": {
			mutate:  func(c *BundleSubmissionConfig) { c.BundleFields = map[string]interface{}{"blockNumber": "0x1"} },
			wantErr: `must not set "blockNumber"`,
		},
		"other bundleFields pass": {
			mutate: func(c *BundleSubmissionConfig) {
				c.BundleFields = map[string]interface{}{"builders": []string{"flashbots"}, "refundPercent": 10}
			},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			c := valid()
			tc.mutate(c)
			err := c.Validate()
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestBundleSubmissionConfig_SetDefaultsKeepsExplicitValues(t *testing.T) {
	c := &BundleSubmissionConfig{UseUpstream: "r", TargetBlocks: 5, ResubmitFor: Duration(time.Minute)}
	c.SetDefaults()
	assert.Equal(t, 5, c.TargetBlocks)
	assert.Equal(t, Duration(time.Minute), c.ResubmitFor)
}

func TestBundleSubmissionConfig_ResubmitForFromYaml(t *testing.T) {
	block := bundleSubmissionBlock + "\n            resubmitFor: 2m"
	cfg, err := loadBundleTestConfig(t, bundleSubmissionYaml(block, "", signingJsonRpc))
	require.NoError(t, err)
	for _, n := range cfg.Projects[0].Networks {
		if n.Evm != nil && n.Evm.ChainId == 1 {
			assert.Equal(t, Duration(2*time.Minute), n.Evm.BundleSubmission.ResubmitFor)
		}
	}
}

func TestFlashbotsSigningKey_ValidationRefusesUnsignableTransports(t *testing.T) {
	for name, tc := range map[string]struct {
		jsonRpc string
		wantErr string
	}{
		"gzip": {
			jsonRpc: signingJsonRpc + "\n          enableGzip: true",
			wantErr: "cannot be combined with jsonRpc.enableGzip",
		},
		"batch": {
			jsonRpc: strings.Replace(signingJsonRpc, "supportsBatch: false", "supportsBatch: true\n          batchMaxSize: 10\n          batchMaxWait: 10ms", 1),
			wantErr: "cannot be combined with jsonRpc.supportsBatch",
		},
		"malformed key": {
			jsonRpc: `          flashbotsSigningKey: "0x` + strings.Repeat("ab", 31) + `zz"`,
			wantErr: "must be a 32-byte hex private key",
		},
		"short key": {
			jsonRpc: `          flashbotsSigningKey: "0xabcdef"`,
			wantErr: "must be a 32-byte hex private key",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadBundleTestConfig(t, bundleSubmissionYaml(bundleSubmissionBlock, "", tc.jsonRpc))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.NotContains(t, err.Error(), "abab", "a private key must never appear in an error")
		})
	}
}

func TestFlashbotsSigningKey_NeverSerialized(t *testing.T) {
	cfg, err := loadBundleTestConfig(t, bundleSubmissionYaml(bundleSubmissionBlock, "", signingJsonRpc))
	require.NoError(t, err)
	secret := strings.TrimPrefix(bundleTestSigningKey, "0x")

	// The startup config log (erpc/init.go) and the admin erpc_config method both
	// marshal the config with SonicCfg.
	sonicOut, err := SonicCfg.Marshal(cfg)
	require.NoError(t, err)
	assert.NotContains(t, string(sonicOut), secret)
	assert.Contains(t, string(sonicOut), `"flashbotsSigningKey":"REDACTED"`)

	// `erpc config --format json|yaml` dumps.
	jsonOut, err := json.Marshal(cfg)
	require.NoError(t, err)
	assert.NotContains(t, string(jsonOut), secret)
	yamlOut, err := yaml.Marshal(cfg)
	require.NoError(t, err)
	assert.NotContains(t, string(yamlOut), secret)

	assert.Equal(t, "REDACTED", fmt.Sprintf("%v", SecretString(bundleTestSigningKey)))
	assert.Equal(t, "", fmt.Sprintf("%v", SecretString("")), "an unset secret stays visibly unset")
}
