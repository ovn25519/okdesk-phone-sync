// Package phone извлекает российский телефонный номер из текста описания
// заявки Okdesk и приводит его к единому виду +7XXXXXXXXXX.
//
// Разбор консервативный: принимаются 11-значные номера, начинающиеся с 7 или 8
// (с разделителями или без), и отформатированные 10-значные мобильные номера,
// начинающиеся с 9. Короткие числовые последовательности (номера домов,
// фрагменты e-mail, номера договоров) игнорируются. Если в тексте несколько
// номеров, возвращается первый по позиции в документе.
package phone

import (
	"regexp"
	"strings"
)

var (
	// reTags вырезает HTML-теги из описания.
	reTags = regexp.MustCompile(`<[^>]*>`)

	// rePhone11 ищет 11-значный номер: необязательный «+», затем 7 или 8 и
	// десять цифр. Между цифрами допускаются пробелы, дефисы, скобки и точки
	// (в том числе несколько подряд, как в «+7 (920) 793-93-33»).
	rePhone11 = regexp.MustCompile(`\+?[78](?:[\s\-().]*\d){10}`)

	// rePhone9 ищет 10-значный мобильный номер, начинающийся с 9, с
	// разделителями или без них.
	rePhone9 = regexp.MustCompile(`9(?:[\s\-().]*\d){9}`)
)

// entityReplacer нормализует HTML-сущности и типографские символы, которые
// могут разделять цифры номера.
var entityReplacer = strings.NewReplacer(
	"&nbsp;", " ",
	"&#160;", " ",
	"&amp;", "&",
	"\u00a0", " ", // неразрывный пробел
	"\u2009", " ", // тонкий пробел
	"\u202f", " ", // узкий неразрывный пробел
	"\u2013", "-", // короткое тире
	"\u2014", "-", // длинное тире
)

// Extract ищет первый телефонный номер в тексте и возвращает его в
// нормализованном виде +7XXXXXXXXXX. Второе значение равно false, если номер
// не найден.
func Extract(text string) (string, bool) {
	clean := normalizeText(text)

	bestStart := -1
	bestValue := ""
	consider := func(start, end int) {
		// Номер не должен быть «вклеен» в более длинную последовательность
		// цифр (иначе фрагмент длинного числа был бы принят за телефон).
		if isDigit(clean, start-1) || isDigit(clean, end) {
			return
		}
		value, ok := normalizeDigits(digitsOnly(clean[start:end]))
		if !ok {
			return
		}
		if bestStart == -1 || start < bestStart {
			bestStart = start
			bestValue = value
		}
	}

	for _, span := range findSpans(rePhone11, clean) {
		consider(span[0], span[1])
	}
	for _, span := range findSpans(rePhone9, clean) {
		consider(span[0], span[1])
	}

	if bestStart == -1 {
		return "", false
	}
	return bestValue, true
}

// Normalize приводит телефон из карточки контакта к единому виду +7XXXXXXXXXX.
// Принимает 11-значные номера, начинающиеся с 7 или 8, и 10-значные мобильные,
// начинающиеся с 9; разделители (пробелы, дефисы, скобки, точки, «+») игнорируются.
// Второе значение равно false, если формат не распознан.
func Normalize(value string) (string, bool) {
	return normalizeDigits(digitsOnly(value))
}

// normalizeText убирает HTML-разметку и нормализует сущности и пробелы.
func normalizeText(s string) string {
	s = reTags.ReplaceAllString(s, " ")
	return entityReplacer.Replace(s)
}

// findSpans возвращает все вхождения re, допуская перекрывающиеся начала: это
// позволяет найти настоящий номер, даже если перед ним стоит «лишняя» цифра.
func findSpans(re *regexp.Regexp, s string) [][2]int {
	var spans [][2]int
	for i := 0; i < len(s); {
		loc := re.FindStringIndex(s[i:])
		if loc == nil {
			break
		}
		start, end := i+loc[0], i+loc[1]
		spans = append(spans, [2]int{start, end})
		i = start + 1
	}
	return spans
}

// normalizeDigits проверяет число цифр и приводит номер к виду +7XXXXXXXXXX.
func normalizeDigits(digits string) (string, bool) {
	switch {
	case len(digits) == 11 && (digits[0] == '7' || digits[0] == '8'):
		return "+7" + digits[1:], true
	case len(digits) == 10 && digits[0] == '9':
		return "+7" + digits, true
	default:
		return "", false
	}
}

// digitsOnly оставляет в строке только цифры.
func digitsOnly(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// isDigit сообщает, что позиция i в строке s — цифра.
func isDigit(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return false
	}
	return s[i] >= '0' && s[i] <= '9'
}
