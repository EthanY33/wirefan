// Package web embeds the static demo client served at /.
package web

import "embed"

//go:embed index.html client.js styles.css mark.svg
var Files embed.FS
