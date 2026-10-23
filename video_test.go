package main

import "testing"

func TestParseProbe(t *testing.T) {
	w, h, s := parseProbe([]byte(`{"streams":[{"width":1344,"height":768}],"format":{"duration":"5.167000"}}`))
	if w != 1344 || h != 768 || s != 6 {
		t.Fatalf("got %dx%d %ds, want 1344x768 6s", w, h, s)
	}
	if w, h, s := parseProbe([]byte(`not json`)); w != 0 || h != 0 || s != 0 {
		t.Fatalf("bad input gave %dx%d %ds", w, h, s)
	}
}

func TestReadVideoRefusesOtherFormats(t *testing.T) {
	if _, err := readVideo("clip.mov"); err == nil {
		t.Fatal("a .mov must be refused before reading the file")
	}
}
