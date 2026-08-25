package deliverable

import "strings"

// LooksLikeHTMLDocument reports whether content is a standalone HTML document
// rather than Markdown. Deliverables keep their content_type honest so
// previews render in a sandboxed frame and downloads carry a .html extension.
func LooksLikeHTMLDocument(content string) bool {
	head := strings.ToLower(strings.TrimSpace(content))
	return strings.HasPrefix(head, "<!doctype html") || strings.HasPrefix(head, "<html")
}
