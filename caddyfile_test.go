package formward

import (
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

func TestUnmarshalCaddyfile(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantPath string
		wantErr  bool
	}{
		{
			name:     "path only",
			input:    `formward /test`,
			wantPath: "/test",
		},
		{
			name:     "path with empty block",
			input:    "formward /test {\n}",
			wantPath: "/test",
		},
		{
			name:    "missing path",
			input:   `formward`,
			wantErr: true,
		},
		{
			name:    "extra positional arg",
			input:   `formward /test /other`,
			wantErr: true,
		},
		{
			name:    "unknown subdirective rejected",
			input:   "formward /test {\n  bogus value\n}",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := caddyfile.NewTestDispenser(tt.input)
			var m Module
			err := m.UnmarshalCaddyfile(d)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && m.Path != tt.wantPath {
				t.Errorf("Path = %q, want %q", m.Path, tt.wantPath)
			}
		})
	}
}
