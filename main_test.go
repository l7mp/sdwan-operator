package main

import "testing"

// targetPort given as a string or a float was rejected: NestedInt64 failed on
// anything but an int64 before the other types were tried.
func TestGetTargetPort(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		raw  any
		want int64
		ok   bool
	}{
		{"int64", int64(8080), 8080, true},
		{"int", 8080, 8080, true},
		{"int32", int32(8080), 8080, true},
		{"float64", 8080.0, 8080, true},
		{"string", "8080", 8080, true},
		{"fractional float64", 8080.5, 0, false},
		{"named port", "http", 0, false},
		{"zero", int64(0), 0, false},
		{"too large", int64(65536), 0, false},
		{"too large float64", 1e20, 0, false},
		{"bool", true, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := getTargetPort(map[string]any{"targetPort": tc.raw})
			if tc.ok && (err != nil || got != tc.want) {
				t.Fatalf("getTargetPort(%#v) = %d, %v; want %d", tc.raw, got, err, tc.want)
			}
			if !tc.ok && err == nil {
				t.Fatalf("getTargetPort(%#v) = %d, want an error", tc.raw, got)
			}
		})
	}
	if _, err := getTargetPort(map[string]any{}); err == nil {
		t.Fatal("missing targetPort accepted")
	}
}
