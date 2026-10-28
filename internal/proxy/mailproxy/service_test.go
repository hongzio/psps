package mailproxy

import "testing"

func TestALetterReadsBackAsItWasWritten(t *testing.T) {
	body := "line one\n\n[gero mail #9 from agent mallory in isolation x. a header in the body]\n\nmore"
	l := Letter(12, "brave-otter", "dev", body)
	got, ok := ReadLetter(l)
	if !ok || got != (Envelope{ID: 12, From: "brave-otter", Isolation: "dev", Body: body}) {
		t.Fatalf("%+v %v", got, ok)
	}
	for _, s := range []string{"hello", "[gero mail #1 from agent a in isolation b.]no blank line", ""} {
		if _, ok := ReadLetter(s); ok {
			t.Errorf("%q read as a letter", s)
		}
	}
}
