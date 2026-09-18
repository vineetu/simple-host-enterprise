package sitetype

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestParseAcceptsOnlyTheClosedSet(t *testing.T) {
	for _, known := range All {
		if got := Parse(string(known.Type)); got != known.Type {
			t.Errorf("Parse(%q) = %q, want %q", known.Type, got, known.Type)
		}
	}
	// The model is told to answer with one word; anything else must not become
	// a label, or the showcase grows a group of one per stray reply.
	for _, raw := range []string{
		"", "  ", "documentation", "Docs & specs", "dashboard.", "unsorted",
		"I think this is a dashboard", "docs\nplan", "<script>", "DOCS ",
	} {
		if got := Parse(raw); got != "" && raw != "DOCS " {
			t.Errorf("Parse(%q) = %q, want empty", raw, got)
		}
	}
	// Case and surrounding space are the one tolerance.
	if got := Parse("  DOCS "); got != Docs {
		t.Errorf("Parse with case and space = %q, want docs", got)
	}
}

func TestInputEmptyDetectsNothingToClassify(t *testing.T) {
	if !(Input{SiteName: "x"}).Empty() {
		t.Error("input with only a name should be empty")
	}
	if !(Input{Title: "   ", BodyText: "\n"}).Empty() {
		t.Error("whitespace-only input should be empty")
	}
	if (Input{Title: "Spec"}).Empty() {
		t.Error("input with a title is not empty")
	}
}

func TestRenderBoundsBodyAndKeepsRunesIntact(t *testing.T) {
	in := Input{
		SiteName: "demo",
		Title:    "T",
		BodyText: strings.Repeat("é", maxBodyBytes),
	}
	rendered := in.Render()
	if len(rendered) > maxBodyBytes+2000 {
		t.Errorf("rendered prompt is %d bytes, expected the body to be bounded", len(rendered))
	}
	// A body cut mid-rune would reach the model as replacement characters.
	if strings.Contains(rendered, "�") {
		t.Error("body was cut mid-rune")
	}
	for _, want := range []string{"URL name: demo", "Title: T", "Body: "} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered prompt missing %q", want)
		}
	}
}

type stubClassifier struct {
	reply Type
	err   error
	calls int
}

func (s *stubClassifier) Classify(context.Context, Input) (Type, error) {
	s.calls++
	return s.reply, s.err
}

// A nil classifier must switch the feature off rather than start a worker that
// fails on every pass — that is what happens when Bedrock is unreachable.
func TestStartWorkerWithoutClassifierIsDisabled(t *testing.T) {
	if w := StartWorker(context.Background(), nil, &stubClassifier{}); w != nil {
		t.Error("worker started with no database")
	}
	// A nil worker must tolerate the full lifecycle being called on it.
	var w *Worker
	w.Stop()
	if err := w.Wait(context.Background()); err != nil {
		t.Errorf("Wait on nil worker = %v, want nil", err)
	}
	if _, err := w.RunOnce(context.Background(), 5); err != nil {
		t.Errorf("RunOnce on nil worker = %v, want nil", err)
	}
}

func TestClassifierErrorsAreContained(t *testing.T) {
	stub := &stubClassifier{err: errors.New("bedrock unavailable")}
	if _, err := stub.Classify(context.Background(), Input{Title: "x"}); err == nil {
		t.Fatal("stub should surface its error")
	}
	if stub.calls != 1 {
		t.Errorf("calls = %d, want 1", stub.calls)
	}
}

func TestLabelsAreStableAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, known := range All {
		if known.Label == "" {
			t.Errorf("type %q has no label", known.Type)
		}
		if seen[known.Label] {
			t.Errorf("duplicate label %q", known.Label)
		}
		seen[known.Label] = true
		if !known.Type.Valid() {
			t.Errorf("type %q is not valid by its own test", known.Type)
		}
	}
	if Type("unsorted").Valid() {
		t.Error("unsorted must not be a member of the closed set")
	}
	if Type("").Valid() {
		t.Error("the empty type must not be valid")
	}
}
