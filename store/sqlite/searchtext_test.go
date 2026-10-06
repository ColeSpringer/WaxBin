package sqlite

import (
	"testing"

	"golang.org/x/text/unicode/norm"
)

func TestSearchAltForms(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"Pi'erre Bourne", "pierre"},
		{"Cocaine 80's", "80s"},
		{"Big K.R.I.T.", "krit"},
		{"A.CHAL", "achal"},
		{"That’s What I Like", "thats"},
		{"Jay-Z", "jayz"},
		{"Ty Dolla $ign", "sign"},
		{"Ke$ha", "kesha"},
		{"Curren$y", "currensy"},
		{"Joey Bada$$", "badass"},
		// A dollar sign beside no letter is punctuation.
		{"$100 Bill", ""},
		// The pairs run over the hiragana fold, so either syllabary finds a word inside a
		// run, and the run's last character stands alone, so every character begins a
		// word the index holds.
		{"東京スパイス", "東京すぱいす 東京 京す すぱ ぱい いす す"},
		{"ガンダム", "がんだむ がん んだ だむ む"},
		// A two-character run that is a word already is its own pair; one inside a
		// longer word is not.
		{"東京", "京"},
		{"AKB48東京", "東京 京"},
		// Latin glued to a run stands alone too, as does a lone character inside a word.
		{"マシーンLOVE", "ましーんlove まし しー ーん ん love"},
		{"第1話", "1 話"},
		{"Ｆｕｌｌ Ｍｏｏｎ", "full moon"},
		{"ｶﾞﾝﾀﾞﾑ", "ガンダム がんだむ がん んだ だむ む"},
		{"हिन्दी", ""},
		// Thai runs its words together too; a unit is a letter with its marks.
		{"สวัสดีครับ", "สวั วัส สดี ดีค ครั รับ บ"},
		{norm.NFD.String("Beyoncé"), ""},
		{"Radiohead Paranoid Android", ""},
		{"!!! & --", ""},
		{"\u0301 a\u0301-\u0302", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := SearchAltForms(c.in); got != c.want {
			t.Errorf("SearchAltForms(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSearchColumnText(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"Jay-Z & Alicia Keys", "Jay-Z & Alicia Keys jayz"},
		{"Radiohead", "Radiohead"},
		{norm.NFD.String("Beyoncé"), "Beyoncé"},
		{"", ""},
	}
	for _, c := range cases {
		got := searchColumnText(c.in)
		if got != c.want {
			t.Errorf("searchColumnText(%q) = %q, want %q", c.in, got, c.want)
		}
		if !norm.NFC.IsNormalString(got) {
			t.Errorf("searchColumnText(%q) = %q is not NFC", c.in, got)
		}
	}
}
