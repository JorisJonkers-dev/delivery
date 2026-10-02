// Package pins holds the Go types of a pin file's annotations,
// generated from deploy-kit's published schema. Never edit types_gen.go: change the vendored
// schema and run `task gen`.
package pins

//go:generate go tool go-jsonschema --tags json --capitalization ID,HTTP,TCP,URL -p pins -o types_gen.go ../../../third_party/deploy-kit/schemas/pin-annotations.schema.json
