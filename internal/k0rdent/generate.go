package k0rdent

// The generated files come from spec/openapi.yaml and spec/SOURCE.json, and
// nothing else. `make client-check` runs these and fails if the output
// differs from what is committed. See README.md.

//go:generate go run ../tools/clientspec -contract
//go:generate go run ../tools/clientlimits
//go:generate go tool oapi-codegen -config oapi-codegen.types.yaml spec/openapi.yaml
//go:generate go tool oapi-codegen -config oapi-codegen.client.yaml spec/openapi.yaml
