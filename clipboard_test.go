//go:build windows

package main

import "testing"

func TestValidateDIBBounds(t *testing.T) {
	cases := []struct {
		name           string
		width, height  int32
		wantErr        bool
	}{
		{"zero width", 0, 100, true},
		{"zero height", 100, 0, true},
		{"negative width", -10, 100, true},
		{"large negative height ok (top-down)", 100, -1000, false},
		{"width over 32768", 40000, 100, true},
		{"height over 32768", 100, 40000, true},
		{"product over 100M", 20000, 20000, true},
		{"normal 1920x1080", 1920, 1080, false},
		{"top-down 800x-600", 800, -600, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDIBBounds(tc.width, tc.height)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateDIBBounds(%d, %d): wantErr=%v got %v", tc.width, tc.height, tc.wantErr, err)
			}
		})
	}
}
