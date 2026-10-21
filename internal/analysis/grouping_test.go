package analysis_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"ytcomments/internal/analysis"
)

func TestGroupByCategory(t *testing.T) {
	comments := []analysis.Comment{
		{ID: "1", Text: "Thanks a lot!"},
		{ID: "2", Text: "How did you do that?"},
		{ID: "3", Text: "Great video, thank you"},
	}
	tests := []struct {
		name            string
		classifications []analysis.Classification
		want            analysis.Result
	}{
		{
			name: "groups by category and keeps original text",
			classifications: []analysis.Classification{
				{ID: "1", Category: analysis.CategoryThanks},
				{ID: "2", Category: analysis.CategoryQuestions},
				{ID: "3", Category: analysis.CategoryThanks},
			},
			want: analysis.Result{
				analysis.CategoryThanks: {
					{ID: "1", Text: "Thanks a lot!"},
					{ID: "3", Text: "Great video, thank you"},
				},
				analysis.CategoryQuestions: {{ID: "2", Text: "How did you do that?"}},
			},
		},
		{
			name: "drops ids invented by the model",
			classifications: []analysis.Classification{
				{ID: "1", Category: analysis.CategoryThanks},
				{ID: "999", Category: analysis.CategoryBots},
			},
			want: analysis.Result{analysis.CategoryThanks: {{ID: "1", Text: "Thanks a lot!"}}},
		},
		{
			name: "keeps only the first classification of a duplicated id",
			classifications: []analysis.Classification{
				{ID: "2", Category: analysis.CategoryQuestions},
				{ID: "2", Category: analysis.CategoryFeedback},
			},
			want: analysis.Result{analysis.CategoryQuestions: {{ID: "2", Text: "How did you do that?"}}},
		},
		{
			name:            "drops unknown categories",
			classifications: []analysis.Classification{{ID: "1", Category: "Spam"}},
			want:            analysis.Result{},
		},
		{
			name:            "empty input gives empty (not nil) result",
			classifications: nil,
			want:            analysis.Result{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := analysis.GroupByCategory(comments, tt.classifications)
			if got == nil {
				t.Fatal("result is nil, want empty map (it must serialize as {})")
			}
			// cmp.Diff печатает понятную разницу (-want +got) — удобнее reflect.DeepEqual.
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("GroupByCategory() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
