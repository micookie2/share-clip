// Package assets holds the share-clip brand icon in every form the project
// needs: the vector source (also used by the web UI favicon and the READMEs),
// raster sizes for touch icons / previews, and a multi-size .ico that is
// embedded into the Windows executables.
package assets

import _ "embed"

// IconSVG is the canonical vector icon. It is the single source of truth:
// the PNG/ICO files below are generated from it (see `make icon`).
//
//go:embed icon.svg
var IconSVG []byte

// IconICO is the multi-size Windows icon (16–256 px), used both as the web
// UI's /favicon.ico fallback and as the .exe icon resource.
//
//go:embed icon.ico
var IconICO []byte

// Icon192 and Icon512 are square PNG renderings for web manifests and
// previews.
//
//go:embed icon-192.png
var Icon192 []byte

//go:embed icon-512.png
var Icon512 []byte

// AppleTouchIcon is the 180px PNG used by iOS home-screen bookmarks.
//
//go:embed apple-touch-icon.png
var AppleTouchIcon []byte
