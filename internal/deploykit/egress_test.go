package deploykit_test

import (
	"encoding/json"
	"os"
	"path"
	"testing"

	"github.com/JorisJonkers-dev/delivery/internal/deploykit/resolved"
)

// The corpus names no case with an egress rule beyond the first three, so the rules deploy-kit
// 1.2.0 added (scrape-target, flux-metrics) would decode or not without the corpus test noticing.
// This reads the rules from the vendored schema itself, so a rule a later release adds is held too.
func TestEveryEgressRuleTheSchemaNamesDecodes(t *testing.T) {
	root, err := os.OpenRoot(vendored)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	raw, err := root.ReadFile(path.Join("schemas", "resolved-deployment.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Defs struct {
			EgressRule struct {
				Enum []string `json:"enum"`
			} `json:"EgressRule"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	if len(schema.Defs.EgressRule.Enum) == 0 {
		t.Fatal("the vendored schema names no egress rule, so nothing below is checked")
	}
	for _, rule := range schema.Defs.EgressRule.Enum {
		t.Run(rule, func(t *testing.T) {
			data, err := json.Marshal(rule)
			if err != nil {
				t.Fatal(err)
			}
			var decoded resolved.EgressRule
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatalf("an egress rule the schema names does not decode: %v", err)
			}
			if string(decoded) != rule {
				t.Fatalf("decoded %q as %q", rule, decoded)
			}
		})
	}
}
