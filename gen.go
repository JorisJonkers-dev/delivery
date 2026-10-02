package templatego

// Code generators hang off `go generate ./...`; `task gen` runs them and `task gen:check`
// fails when the committed output is stale. Add a `//go:generate` directive beside the code
// it generates. There is none yet, so both tasks are no-ops.
