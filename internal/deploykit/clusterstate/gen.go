// Package clusterstate holds the Go types of the ClusterState snapshot,
// generated from deploy-kit's published schema. Never edit types_gen.go: change the vendored
// schema and run `task gen`.
package clusterstate

//go:generate go tool go-jsonschema --tags json --capitalization ID,HTTP,TCP,URL --schema-root-type =Snapshot -p clusterstate -o types_gen.go ../../../third_party/deploy-kit/schemas/cluster-state-snapshot.schema.json
