// Package apperr описывает прикладные ошибки, которые можно показать клиенту.
//
// Доменный код не знает про HTTP: он сообщает только «вид» ошибки (не найдено,
// невалидный ввод, хранилище недоступно), а переводом вида в HTTP-код занимается
// один-единственный пакет httpx. Аналог в Python-версии — AppError со status_code,
// только здесь код ответа не зашит в доменную ошибку.
//
// В Go ошибки — это значения, а не исключения: функция возвращает error последним
// результатом, вызывающий проверяет его явно. Цепочку причин строят через
// fmt.Errorf("...: %w", err), а проверяют через errors.Is / errors.As
// (аналог `except SomeError` / `rescue SomeError`, но по всей цепочке обёрток).
package apperr

import "errors"

// Kind — категория ошибки с точки зрения клиента.
type Kind int

const (
	// KindInternal — всё непредвиденное. Детали клиенту не показываются.
	KindInternal Kind = iota
	// KindInvalid — клиент прислал некорректные данные.
	KindInvalid
	// KindNotFound — запрошенный объект не существует.
	KindNotFound
	// KindUnavailable — временно недоступна инфраструктура (Redis и т.п.).
	KindUnavailable
)

// Error — ошибка с видом и безопасным для клиента сообщением.
// Внутренняя причина (Err) пишется в лог, но не уходит клиенту.
type Error struct {
	Kind    Kind
	Message string
	Err     error
}

// New создаёт ошибку без внутренней причины. Удобно для «сигнальных» значений:
//
//	var ErrTaskNotFound = apperr.New(apperr.KindNotFound, "Task not found")
//
// Такие значения сравниваются через errors.Is (по указателю).
func New(kind Kind, message string) *Error {
	return &Error{Kind: kind, Message: message}
}

// Wrap создаёт ошибку, сохраняя внутреннюю причину для логов и errors.Is/As.
func Wrap(kind Kind, message string, err error) *Error {
	return &Error{Kind: kind, Message: message, Err: err}
}

func (e *Error) Error() string {
	if e.Err != nil {
		return e.Message + ": " + e.Err.Error()
	}
	return e.Message
}

// Unwrap позволяет errors.Is/As заглянуть во внутреннюю причину.
func (e *Error) Unwrap() error { return e.Err }

// As достаёт *Error из цепочки обёрток. Если его нет, ошибка считается внутренней.
func As(err error) (*Error, bool) {
	var appErr *Error
	if errors.As(err, &appErr) {
		return appErr, true
	}
	return nil, false
}
