package delivery

// Code generators hang off `go generate ./...`; `task gen` runs them and `task gen:check`
// fails when the committed output is stale. Each `//go:generate` directive sits beside the code
// it generates: today, the deploy-kit schema types under internal/deploykit.
