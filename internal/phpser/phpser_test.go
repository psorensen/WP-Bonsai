package phpser

import "testing"

func TestUnserialize(t *testing.T) {
	v, err := Unserialize([]byte(`a:4:{s:4:"type";s:7:"gallery";i:0;i:-3;s:5:"café";d:1.5;s:3:"sub";a:2:{i:0;b:1;i:1;N;}}`))
	if err != nil {
		t.Fatal(err)
	}
	if typ, ok := v.Lookup("type"); !ok || string(typ.Str) != "gallery" {
		t.Errorf("type = %+v", typ)
	}
	if f, ok := v.Lookup("café"); !ok || f.Float != 1.5 {
		t.Errorf("multi-byte key lookup failed: %+v", f)
	}
	var kinds []Kind
	Walk(v, func(x Value) { kinds = append(kinds, x.Kind) })
	want := []Kind{String, Int, Float, Bool, Null}
	if len(kinds) != len(want) {
		t.Fatalf("Walk visited %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Errorf("Walk visit %d = %v, want %v", i, kinds[i], want[i])
		}
	}
}

func TestUnserializeRejects(t *testing.T) {
	for _, in := range []string{
		``, `a`, `a:1:{`, `a:1:{i:0;s:5:"abc";}`, `s:3:"abc"`, `a:1:{i:0;i:1;}x`,
		`a:99999999999:{}`, `i:abc;`, `x:1;`,
	} {
		if _, err := Unserialize([]byte(in)); err == nil {
			t.Errorf("Unserialize(%q) did not fail", in)
		}
	}
}

func FuzzUnserialize(f *testing.F) {
	f.Add([]byte(`a:2:{i:0;s:3:"123";s:1:"k";a:1:{i:0;O:8:"stdClass":1:{s:1:"a";d:0.5;}}}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		v, err := Unserialize(b)
		if err == nil {
			Walk(v, func(Value) {})
		}
	})
}

func TestSerializeInts(t *testing.T) {
	got := SerializeInts([]int64{5, 12})
	if got != "a:2:{i:0;i:5;i:1;i:12;}" {
		t.Errorf("SerializeInts = %s", got)
	}
	if _, err := Unserialize([]byte(got)); err != nil {
		t.Error(err)
	}
}
