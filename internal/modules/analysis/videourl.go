package analysis

import (
	"net/url"
	"regexp"
	"slices"
	"strings"
)

var (
	videoIDRe    = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
	youtubeHosts = []string{"youtube.com", "www.youtube.com", "m.youtube.com", "music.youtube.com"}
	pathPrefixes = []string{"shorts", "embed", "live"}
)

// ParseVideoID достаёт ID видео из ссылки. Поддерживаются
// youtube.com/watch?v=<id>, youtu.be/<id>, /shorts/<id>, /embed/<id>, /live/<id>.
func ParseVideoID(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", ErrInvalidVideoURL
	}
	host := strings.ToLower(u.Hostname())
	parts := strings.FieldsFunc(u.Path, func(r rune) bool { return r == '/' })

	var candidate string
	switch {
	case host == "youtu.be" && len(parts) > 0:
		candidate = parts[0]
	case slices.Contains(youtubeHosts, host):
		if len(parts) == 1 && parts[0] == "watch" {
			candidate = u.Query().Get("v")
		} else if len(parts) >= 2 && slices.Contains(pathPrefixes, parts[0]) {
			candidate = parts[1]
		}
	}
	if !videoIDRe.MatchString(candidate) {
		return "", ErrInvalidVideoURL
	}
	return candidate, nil
}
