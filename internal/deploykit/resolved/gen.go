// Package resolved holds the Go types of the Resolved Deployment,
// generated from deploy-kit's published schema. Never edit types_gen.go: change the vendored
// schema and run `task gen`.
package resolved

//go:generate go tool go-jsonschema --tags json --capitalization ID,HTTP,TCP,URL -p resolved -o types_gen.go ../../../third_party/deploy-kit/schemas/resolved-deployment.schema.json
