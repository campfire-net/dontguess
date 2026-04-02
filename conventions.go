// Package dontguess embeds convention declarations for the exchange.
//
// This file lives at the repo root so go:embed can reach docs/convention/.
// Import as github.com/campfire-net/dontguess and use ConventionFS.
package dontguess

import "embed"

// ConventionFS embeds the exchange convention declarations (exchange-core/
// and exchange-scrip/ sub-directories). Used by exchange.Init when no
// external --convention-dir is provided.
//
//go:embed docs/convention/exchange-core/*.json docs/convention/exchange-scrip/*.json
var ConventionFS embed.FS
