package plugin

import "testing"

func TestNormalizeClineChatPayload(t *testing.T) {
	in := []byte(`{"success":true,"data":{"id":"x","choices":[{"message":{"content":"好"}}]}}`)
	out := normalizeClineChatPayload(in)
	want := `{"id":"x","choices":[{"message":{"content":"好"}}]}`
	if string(out) != want {
		t.Fatalf("got %s", out)
	}
	plain := []byte(`{"id":"y","choices":[]}`)
	if string(normalizeClineChatPayload(plain)) != string(plain) {
		t.Fatal("plain passthrough")
	}
}
