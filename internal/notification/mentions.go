package notification

import (
	"regexp"
	"strings"
)

// mentionRe matches @handle tokens in free text (plain-text projection of a
// page body, or a comment body).
var mentionRe = regexp.MustCompile(`@([A-Za-z0-9._-]+)`)

// parseMentions extracts the lowercased, de-duplicated set of @handles.
//
// NOTE: Docs stores no user handles — only the notification_recipients
// directory (email + name). ResolveMentions matches each handle against the
// email local-part or the name with spaces removed (case-insensitive). Same
// pragmatic heuristic as Track; if real handles are added later, only
// ResolveMentions changes.
func parseMentions(text string) []string {
	matches := mentionRe.FindAllStringSubmatch(text, -1)
	seen := map[string]bool{}
	var out []string
	for _, m := range matches {
		tok := strings.ToLower(m[1])
		if !seen[tok] {
			seen[tok] = true
			out = append(out, tok)
		}
	}
	return out
}
