// Package platform embeds the canonical zaentrum platform Helm chart so the
// operator renders the SAME chart self-hosters install with `helm install`. One
// source of truth — the operator uses Helm only as a template engine (client-only
// render), then applies via server-side apply (see internal/templates).
package platform

import "embed"

//go:embed all:chart
var Chart embed.FS
