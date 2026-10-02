// Package lock holds the Go types of the composition lock,
// generated from deploy-kit's published schema. Never edit types_gen.go: change the vendored
// schema and run `task gen`.
package lock

//go:generate go tool go-jsonschema --tags json --capitalization ID,HTTP,TCP,URL -p lock -o types_gen.go ../../../third_party/deploy-kit/schemas/composition-lock.schema.json
