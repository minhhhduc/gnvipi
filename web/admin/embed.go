// Package adminui embeds the standalone admin single-page application.
package adminui

import "embed"

// FS holds the SPA assets. They are read-only data in the binary and are
// streamed from here on demand; the server keeps no copy in the heap.
//
//go:embed index.html app.css app.js
var FS embed.FS
