package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/review"
	"github.com/faustbrian/reviewd/internal/store"
)

func TestPublicationPresentationShowsRecordedNotJustProposedDelivery(t *testing.T) {
	url := "https://github.com/owner/repo/pull/17#pullrequestreview-71"
	inline := "https://github.com/owner/repo/pull/17#discussion_r72"
	for _, verified := range []bool{true, false} {
		outcome, _ := json.Marshal(review.PublicationOutcome{Review: github.RemoteReview{ID: 71, State: "COMMENTED", URL: url}, Comments: []github.RemoteComment{{ID: 72, URL: inline}}, Verified: verified, ReviewedHead: strings.Repeat("a", 40), CurrentHead: strings.Repeat("b", 40), Failure: "A later context change requires reassessment."})
		var output bytes.Buffer
		if err := (&execution{}).presentRun(&output, store.Run{ID: "run", State: store.Stale, Outcome: outcome}); err != nil {
			t.Fatal(err)
		}
		text := output.String()
		for _, want := range []string{url, inline, "COMMENTED", strings.Repeat("a", 40), strings.Repeat("b", 40), "requires reassessment"} {
			if !strings.Contains(text, want) {
				t.Fatalf("actual publication detail %q missing from %s", want, text)
			}
		}
		if verified && !strings.Contains(text, "GitHub readback: verified") {
			t.Fatal("verified readback omitted")
		}
		if !verified && !strings.Contains(text, "GitHub readback: not verified") {
			t.Fatal("unverified outcome presented as verified")
		}
	}
}
