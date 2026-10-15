// Package web holds the server-rendered UI's templates and static files,
// embedded into the binary.
package web

import "embed"

// Templates holds templates/*.html.
//
//go:embed templates/*.html
var Templates embed.FS

// Static holds static/*, served under /static/.
//
//go:embed static/*
var Static embed.FS
