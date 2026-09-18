package sitetype

import (
	"context"
	"fmt"
	"strings"
)

// maxBodyBytes bounds what is sent to the model. The distinguishing evidence —
// title, description, headings — is at the top of a page; the body is only
// there to break ties, and sending a whole site would cost more and decide no
// more.
const maxBodyBytes = 2000

// Input is the already-extracted text for one site. It comes from
// site_search_documents, which the search indexer populates; classification
// never reads the filesystem.
type Input struct {
	SiteName    string
	Title       string
	Description string
	Headings    string
	BodyText    string
}

// Empty reports whether there is anything worth classifying. A site with no
// text is left unclassified rather than guessed at.
func (in Input) Empty() bool {
	return strings.TrimSpace(in.Title+in.Description+in.Headings+in.BodyText) == ""
}

// Render builds the user message. Field order is fixed so the same site
// produces the same request.
func (in Input) Render() string {
	body := strings.TrimSpace(in.BodyText)
	if len(body) > maxBodyBytes {
		// Cut on a rune boundary; a split rune would reach the model as U+FFFD.
		body = body[:maxBodyBytes]
		for len(body) > 0 && !isBoundary(body[len(body)-1]) {
			body = body[:len(body)-1]
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "URL name: %s\n", in.SiteName)
	fmt.Fprintf(&b, "Title: %s\n", strings.TrimSpace(in.Title))
	fmt.Fprintf(&b, "Description: %s\n", strings.TrimSpace(in.Description))
	fmt.Fprintf(&b, "Headings: %s\n", truncate(strings.TrimSpace(in.Headings), 1000))
	fmt.Fprintf(&b, "Body: %s", body)
	return b.String()
}

func isBoundary(c byte) bool { return c&0xC0 != 0x80 }

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	s = s[:limit]
	for len(s) > 0 && !isBoundary(s[len(s)-1]) {
		s = s[:len(s)-1]
	}
	return s
}

// Classifier turns one site's text into a type. It is an interface so the
// worker can be tested without AWS credentials, which the rest of the suite
// also runs without. A nil Classifier disables classification entirely.
type Classifier interface {
	Classify(ctx context.Context, in Input) (Type, error)
}
