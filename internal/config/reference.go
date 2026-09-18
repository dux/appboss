package config

import _ "embed"

// Reference is the annotated configuration reference shipped inside the binary, so the
// documentation an operator reads always matches the release they run.
//
//go:embed reference.yaml
var Reference string
