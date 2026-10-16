package analysis

import "ytcomments/internal/youtube"

// GroupByCategory склеивает ответ классификатора с исходными комментариями.
//
// Ответ модели — недоверенные данные: id, которых не было во входе, и повторы
// отбрасываются, а текст берётся из исходного комментария, а не из ответа модели.
func GroupByCategory(comments []youtube.Comment, classifications []Classification) Result {
	texts := make(map[string]string, len(comments))
	for _, c := range comments {
		texts[c.ID] = c.Text
	}

	// Пустая map, а не nil: в JSON это {}, а не null (как в Python-версии).
	result := Result{}
	seen := make(map[string]struct{}, len(classifications)) // map[...]struct{} — идиоматичный set
	for _, item := range classifications {
		text, known := texts[item.ID]
		if _, dup := seen[item.ID]; !known || dup || !item.Category.Valid() {
			continue
		}
		seen[item.ID] = struct{}{}
		result[item.Category] = append(result[item.Category], CommentOut{ID: item.ID, Text: text})
	}
	return result
}
