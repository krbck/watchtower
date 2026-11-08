// Package ui embeds the faz-1 web UI static assets.
//
// NOTE (D-013): this is a functional prototype. Visual design tokens will
// come from the Figma MCP flow (D-005) before the UI hardens.
package ui

import "embed"

//go:embed index.html app.js graphkit.js style.css vendor
var FS embed.FS
