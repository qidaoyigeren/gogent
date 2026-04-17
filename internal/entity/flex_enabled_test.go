package entity

import (
	"testing"
)

func TestFlexEnabledScan(t *testing.T) {
	var f FlexEnabled
	for _, tc := range []struct {
		in   any
		want int
	}{
		{true, 1},
		{false, 0},
		{int64(1), 1},
		{int64(0), 0},
		{[]byte("t"), 1},
		{[]byte("f"), 0},
	} {
		if err := (&f).Scan(tc.in); err != nil {
			t.Fatalf("Scan(%v): %v", tc.in, err)
		}
		if int(f) != tc.want {
			t.Fatalf("Scan(%v) got %d want %d", tc.in, int(f), tc.want)
		}
	}
}

func TestFlexEnabledValue(t *testing.T) {
	v, err := FlexEnabled(1).Value()
	if err != nil {
		t.Fatal(err)
	}
	if b, ok := v.(bool); !ok || !b {
		t.Fatalf("Value(1) = %v (%T), want true bool", v, v)
	}
	v, err = FlexEnabled(0).Value()
	if err != nil {
		t.Fatal(err)
	}
	if b, ok := v.(bool); !ok || b {
		t.Fatalf("Value(0) = %v (%T), want false bool", v, v)
	}
}
