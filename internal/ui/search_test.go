package ui

import "testing"

func TestSearchResultStatus(t *testing.T) {
	tests := []struct {
		name   string
		server bool
		count  int
		more   bool
		want   string
	}{
		{name: "empty cache", want: "0 cached"},
		{name: "one cached result", count: 1, want: "1 cached"},
		{name: "more cached results", count: 100, more: true, want: "100+ cached"},
		{name: "provider results", server: true, count: 24, want: "24 all mail"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := searchResultStatus(tt.server, tt.count, tt.more); got != tt.want {
				t.Fatalf("searchResultStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}
