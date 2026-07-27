package ui

import _ "embed"

// ProviderPage is a static shell. Runtime configuration and credentials are
// loaded only from authenticated Management API routes.
//
//go:embed index.html
var ProviderPage []byte
