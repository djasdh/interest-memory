package httpapi

import _ "embed"

// graphHTML is the embedded 3D visualization page served at
// GET /api/v1/{agent}/graph.html. It fetches the graph JSON from
// /api/v1/{agent}/graph itself (agent resolved from the URL).
//
//go:embed graph.html
var graphHTML string
