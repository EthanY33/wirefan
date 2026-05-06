// Package web embeds the static demo client served at /.
package web

import "embed"

//go:embed index.html client.js styles.css
var Files embed.FS
