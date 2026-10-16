// Package containerui holds what belongs to the whole repository rather than
// to one part of it: the third-party notices, embedded so the binary can show
// the licenses of the components it contains.
package containerui

import _ "embed"

// Notices is THIRD_PARTY_NOTICES.md: the open-source components compiled or
// embedded into the binary, with their licenses.
//
//go:embed THIRD_PARTY_NOTICES.md
var Notices string
