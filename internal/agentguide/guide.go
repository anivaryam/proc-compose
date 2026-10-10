// Package agentguide embeds the version-matched operating guide in the binary.
package agentguide

import _ "embed"

// Text is also published as Markdown in the repository. Keep one source for
// offline CLI discovery and the web documentation.
//
//go:embed GUIDE.md
var Text string
