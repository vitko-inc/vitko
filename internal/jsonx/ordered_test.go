package jsonx

import (
	"bytes"
	"testing"
)

func TestTrimKeepsOrderAndSchema(t *testing.T) {
	v, err := FromValue(map[string]any{
		"schema": "vitko.x/v1",
		"b":      1,
		"a":      map[string]any{"x": 1, "y": 2},
		"list":   []any{map[string]any{"m": 1, "n": 2}, map[string]any{"m": 3, "n": 4}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := Trim(SchemaFirst(v), []string{"list.m", "a", "a.x"})
	var b bytes.Buffer
	if err := Encode(&b, got, false); err != nil {
		t.Fatal(err)
	}
	want := `{"schema":"vitko.x/v1","a":{"x":1,"y":2},"list":[{"m":1},{"m":3}]}` + "\n"
	if b.String() != want {
		t.Errorf("got %s want %s", b.String(), want)
	}
}

func TestNoHTMLEscaping(t *testing.T) {
	v, _ := FromValue(map[string]any{"schema": "s", "t": "a <b> & c"})
	var b bytes.Buffer
	_ = Encode(&b, v, false)
	if b.String() != `{"schema":"s","t":"a <b> & c"}`+"\n" {
		t.Errorf("got %s", b.String())
	}
}
