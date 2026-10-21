package analysis_test

import (
	"errors"
	"testing"

	"ytcomments/internal/modules/analysis"
)

// Табличный тест — стандартный приём в Go (аналог pytest.mark.parametrize):
// таблица случаев и один цикл с t.Run, чтобы каждый случай был отдельным подтестом.
func TestParseVideoID(t *testing.T) {
	const id = "dQw4w9WgXcQ"
	tests := []struct {
		name string
		url  string
		want string // пусто — ожидаем ErrInvalidVideoURL
	}{
		{"watch", "https://www.youtube.com/watch?v=" + id, id},
		{"watch with extra params", "https://youtube.com/watch?v=" + id + "&t=42s&list=x", id},
		{"mobile", "https://m.youtube.com/watch?v=" + id, id},
		{"music", "https://music.youtube.com/watch?v=" + id, id},
		{"http scheme", "http://www.youtube.com/watch?v=" + id, id},
		{"uppercase host", "https://WWW.YouTube.com/watch?v=" + id, id},
		{"short link", "https://youtu.be/" + id, id},
		{"short link with query", "https://youtu.be/" + id + "?si=abc", id},
		{"shorts", "https://www.youtube.com/shorts/" + id, id},
		{"embed", "https://www.youtube.com/embed/" + id, id},
		{"live", "https://www.youtube.com/live/" + id, id},
		{"trailing slash", "https://www.youtube.com/shorts/" + id + "/", id},
		{"surrounding spaces", "  https://youtu.be/" + id + "  ", id},

		{"empty", "", ""},
		{"not a url", "not a url", ""},
		{"no scheme", "youtube.com/watch?v=" + id, ""},
		{"ftp scheme", "ftp://youtube.com/watch?v=" + id, ""},
		{"other host", "https://example.com/watch?v=" + id, ""},
		{"lookalike host", "https://youtube.com.evil.com/watch?v=" + id, ""},
		{"watch without v", "https://www.youtube.com/watch", ""},
		{"id too short", "https://youtu.be/abc", ""},
		{"id with bad chars", "https://youtu.be/dQw4w9WgX$Q", ""},
		{"channel page", "https://www.youtube.com/@channel", ""},
		{"unknown prefix", "https://www.youtube.com/channel/" + id, ""},
		{"youtu.be root", "https://youtu.be/", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := analysis.ParseVideoID(tt.url)
			if tt.want == "" {
				if !errors.Is(err, analysis.ErrInvalidVideoURL) {
					t.Fatalf("ParseVideoID(%q) = %q, %v; want ErrInvalidVideoURL", tt.url, got, err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("ParseVideoID(%q) = %q, %v; want %q", tt.url, got, err, tt.want)
			}
		})
	}
}
