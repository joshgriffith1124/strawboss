package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunCheck(t *testing.T) {
	for _, tt := range []struct {
		name    string
		proj    string // .strawboss.toml contents; "" = no file
		args    []string
		want    []string
		wantErr bool
	}{
		{
			name: "flag true",
			args: []string{"--verify", "true"},
			want: []string{"[verified:"},
		},
		{
			name:    "flag failing",
			args:    []string{"--verify", "echo boom; exit 2"},
			want:    []string{"[verify FAILED:", "boom"},
			wantErr: true,
		},
		{
			name: "project file only",
			proj: "verify = \"true\"\n",
			want: []string{"[verified:"},
		},
		{
			name: "no check defined",
			want: []string{"no verify check defined"},
		},
		{
			name:    "verify off",
			args:    []string{"--verify", "off"},
			wantErr: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.proj != "" {
				if err := os.WriteFile(filepath.Join(dir, ".strawboss.toml"), []byte(tt.proj), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			args := append([]string{"--dir", dir}, tt.args...)
			var out strings.Builder
			err := runCheck(args, &out)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, out = %q", err, out.String())
			}
			for _, w := range tt.want {
				if !strings.Contains(out.String(), w) {
					t.Errorf("output missing %q:\n%s", w, out.String())
				}
			}
		})
	}
}
