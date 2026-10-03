package phone

import "testing"

func TestExtract(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
		ok   bool
	}{
		{
			name: "одиннадцать цифр с плюсом",
			text: "Контактный номер: +79207939333",
			want: "+79207939333",
			ok:   true,
		},
		{
			name: "одиннадцать цифр без плюса",
			text: "Номер: 79139165288",
			want: "+79139165288",
			ok:   true,
		},
		{
			name: "номер в конце строки с пробелом",
			text: "Номер: 79688226404 ",
			want: "+79688226404",
			ok:   true,
		},
		{
			name: "восьмёрка в начале",
			text: "тел. 89207939333",
			want: "+79207939333",
			ok:   true,
		},
		{
			name: "форматированный с плюсом",
			text: "Тел: +7 (920) 793-93-33",
			want: "+79207939333",
			ok:   true,
		},
		{
			name: "форматированный с восьмёркой",
			text: "Тел: 8-920-793-93-33",
			want: "+79207939333",
			ok:   true,
		},
		{
			name: "десятизначный мобильный с разделителями",
			text: "моб. 920 793-93-33",
			want: "+79207939333",
			ok:   true,
		},
		{
			name: "первый из нескольких",
			text: "Номер: 79139165288\nДоп: +79207939333",
			want: "+79139165288",
			ok:   true,
		},
		{
			name: "HTML-разметка",
			text: "<p>Тел: <b>+7 920 793-93-33</b></p>",
			want: "+79207939333",
			ok:   true,
		},
		{
			name: "HTML-сущность неразрывного пробела",
			text: "8&nbsp;920&nbsp;793&nbsp;93&nbsp;33",
			want: "+79207939333",
			ok:   true,
		},
		{
			name: "номер дома и фрагмент e-mail не телефон",
			text: "Адрес: д. 4Б/2 кв 350, почта user6122@bk.ru",
			want: "",
			ok:   false,
		},
		{
			name: "одиннадцатизначное число не на 7/8 не телефон",
			text: "Заказ №12345678901",
			want: "",
			ok:   false,
		},
		{
			name: "пустой текст",
			text: "",
			want: "",
			ok:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Extract(tc.text)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("Extract(%q) = (%q, %v), want (%q, %v)", tc.text, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestExtractSampleDescriptions(t *testing.T) {
	// Примеры реальных описаний заявок (см. обсуждение задачи).
	samples := []struct {
		text string
		want string
	}{
		{"Контактное лицо: Алексей\nКонтактный номер: +79207939333", "+79207939333"},
		{"Адрес: Нагатинская наб, д. 4Б/2 кв 350\nНомер: 79139165288", "+79139165288"},
		{"Почта: marsafonova6122@bk.ru\nНомер: 79882883180", "+79882883180"},
		{"ФИО: Володин Вадим Анатольевич\nНомер: 79688226404 ", "+79688226404"},
	}

	for _, s := range samples {
		got, ok := Extract(s.text)
		if !ok || got != s.want {
			t.Fatalf("Extract(%q) = (%q, %v), want %q", s.text, got, ok, s.want)
		}
	}
}

func TestNormalize(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  string
		ok    bool
	}{
		{name: "одиннадцать цифр без плюса", value: "79207939333", want: "+79207939333", ok: true},
		{name: "одиннадцать цифр с плюсом", value: "+79207939333", want: "+79207939333", ok: true},
		{name: "восьмёрка в начале", value: "89207939333", want: "+79207939333", ok: true},
		{name: "форматированный с восьмёркой", value: "8 (920) 793-93-33", want: "+79207939333", ok: true},
		{name: "десятизначный мобильный без разделителей", value: "9207939333", want: "+79207939333", ok: true},
		{name: "десятизначный мобильный с разделителями", value: "920 793-93-33", want: "+79207939333", ok: true},
		{name: "пустая строка", value: "", want: "", ok: false},
		{name: "слишком короткий", value: "350", want: "", ok: false},
		{name: "не телефон", value: "доб. 123", want: "", ok: false},
		{name: "иностранный номер", value: "+1 202 555 0147", want: "", ok: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Normalize(tc.value)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("Normalize(%q) = (%q, %v), want (%q, %v)", tc.value, got, ok, tc.want, tc.ok)
			}
		})
	}
}
