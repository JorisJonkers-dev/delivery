package collector

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-yaml"

	"github.com/JorisJonkers-dev/delivery/internal/deploykit/clusterstate"
)

const (
	apiVersion    = "state.jorisjonkers.dev/v1"
	kind          = "ClusterState"
	schemaVersion = "1.0.0"
)

const header = `# The ClusterState snapshot: what the cluster alone can say, pinned by digest
# (deploy-kit spec/v1/20-resolved-deployment.md#cluster-state). Composition
# never reads the live cluster; a changed fact is a new capture and a new
# digest.
#
# The Collector writes this file, and only when a fact changes: capturedAt is
# when these facts were first captured, not when the Collector last ran.

`

// Read parses a committed snapshot and returns its cluster and its facts. It refuses a document
// deploy-kit's own schema would refuse: the Collector never builds on a file it cannot read.
func Read(document []byte) (string, Facts, error) {
	// The generated type decodes JSON, and enforces the schema's required fields while it does.
	asJSON, err := yaml.YAMLToJSON(document)
	if err != nil {
		return "", Facts{}, fmt.Errorf("read the committed snapshot: %w", err)
	}
	var snapshot clusterstate.Snapshot
	if err := json.Unmarshal(asJSON, &snapshot); err != nil {
		return "", Facts{}, fmt.Errorf("read the committed snapshot: %w", err)
	}
	if snapshot.ApiVersion != apiVersion || snapshot.Kind != kind {
		return "", Facts{}, fmt.Errorf("the committed document is %s %s, not a ClusterState snapshot", snapshot.ApiVersion, snapshot.Kind)
	}

	var facts Facts
	for _, b := range snapshot.Bindings {
		facts.Bindings = append(facts.Bindings, Binding{Claim: b.Claim, Node: b.Node})
	}
	for _, p := range snapshot.Placements {
		facts.Placements = append(facts.Placements, Placement{Process: p.Process, Node: p.Node})
	}
	return snapshot.Cluster, facts, nil
}

// Write is the snapshot document for facts captured at capturedAt. Every string is written
// quoted: a value carrying a colon is quoted in the documents the model reads, and quoting all
// of them means no name ever decides the question.
func Write(cluster string, capturedAt time.Time, facts Facts) []byte {
	var out strings.Builder
	out.WriteString(header)
	fmt.Fprintf(&out, "apiVersion: %s\nkind: %s\nschemaVersion: %s\n", apiVersion, kind, schemaVersion)
	fmt.Fprintf(&out, "cluster: %s\n", strconv.Quote(cluster))
	fmt.Fprintf(&out, "capturedAt: %s\n", strconv.Quote(capturedAt.UTC().Format(time.RFC3339)))

	out.WriteString("bindings:")
	if len(facts.Bindings) == 0 {
		out.WriteString(" []")
	}
	out.WriteString("\n")
	for _, b := range facts.Bindings {
		fmt.Fprintf(&out, "  - { claim: %s, node: %s }\n", strconv.Quote(b.Claim), strconv.Quote(b.Node))
	}

	out.WriteString("placements:")
	if len(facts.Placements) == 0 {
		out.WriteString(" []")
	}
	out.WriteString("\n")
	for _, p := range facts.Placements {
		fmt.Fprintf(&out, "  - { process: %s, node: %s }\n", strconv.Quote(p.Process), strconv.Quote(p.Node))
	}
	return []byte(out.String())
}
