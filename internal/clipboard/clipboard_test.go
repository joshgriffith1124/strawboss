package clipboard

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type call struct {
	name string
	args []string
}

func TestReadImage(t *testing.T) {
	// 8-byte PNG signature: 0x89 'P' 'N' 'G' \r \n 0x1a \n
	pngBytes := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	pngHex := hex.EncodeToString(pngBytes)
	pngB64 := base64.StdEncoding.EncodeToString(pngBytes)

	tests := []struct {
		name      string
		goos      string
		wsl       bool
		env       map[string]string
		tools     map[string]bool // tool names that lookPath resolves
		runStub   func(name string, args []string) ([]byte, error)
		want      []byte
		wantErr   string // substring that must appear in the error; "" means success expected
		wantNoImg bool   // the error must satisfy errors.Is(err, ErrNoImage)
		wantCalls []call
	}{
		{
			name:  "darwin hex output decodes",
			goos:  "darwin",
			tools: map[string]bool{},
			runStub: func(name string, args []string) ([]byte, error) {
				return []byte("«data PNGf" + pngHex + "»\n"), nil
			},
			want: pngBytes,
			wantCalls: []call{
				{name: "osascript", args: []string{"-e", "the clipboard as «class PNGf»"}},
			},
		},
		{
			name:  "darwin text-only clipboard is no image",
			goos:  "darwin",
			tools: map[string]bool{},
			runStub: func(name string, args []string) ([]byte, error) {
				return []byte("some plain text\n"), nil
			},
			wantErr:   "no image on the clipboard",
			wantNoImg: true,
			wantCalls: []call{
				{name: "osascript", args: []string{"-e", "the clipboard as «class PNGf»"}},
			},
		},
		{
			name:  "darwin osascript failure is no image",
			goos:  "darwin",
			tools: map[string]bool{},
			runStub: func(name string, args []string) ([]byte, error) {
				return nil, fmt.Errorf("osascript: execution error")
			},
			wantErr:   "no image on the clipboard",
			wantNoImg: true,
		},
		{
			name:  "wsl base64 with trailing CRLF decodes",
			goos:  "linux",
			wsl:   true,
			tools: map[string]bool{"powershell.exe": true},
			runStub: func(name string, args []string) ([]byte, error) {
				if name != "powershell.exe" {
					return nil, fmt.Errorf("unexpected command %q", name)
				}
				return []byte(pngB64 + "\r\n"), nil
			},
			want: pngBytes,
			wantCalls: []call{
				{name: "powershell.exe", args: []string{"-NoProfile", "-NonInteractive", "-STA", "-Command", wslScript}},
			},
		},
		{
			name:  "wsl empty output is no image",
			goos:  "linux",
			wsl:   true,
			tools: map[string]bool{"powershell.exe": true},
			runStub: func(name string, args []string) ([]byte, error) {
				return []byte("\r\n"), nil
			},
			wantErr:   "no image on the clipboard",
			wantNoImg: true,
		},
		{
			name:  "wayland picks image/png and fetches it",
			goos:  "linux",
			env:   map[string]string{"WAYLAND_DISPLAY": "wayland-0"},
			tools: map[string]bool{"wl-paste": true},
			runStub: func(name string, args []string) ([]byte, error) {
				if name != "wl-paste" {
					return nil, fmt.Errorf("unexpected command %q", name)
				}
				switch {
				case reflect.DeepEqual(args, []string{"--list-types"}):
					return []byte("text/plain\nimage/png\n"), nil
				case reflect.DeepEqual(args, []string{"--no-newline", "--type", "image/png"}):
					return pngBytes, nil
				}
				return nil, fmt.Errorf("unexpected wl-paste args %v", args)
			},
			want: pngBytes,
			wantCalls: []call{
				{name: "wl-paste", args: []string{"--list-types"}},
				{name: "wl-paste", args: []string{"--no-newline", "--type", "image/png"}},
			},
		},
		{
			name:  "wayland text-only is no image",
			goos:  "linux",
			env:   map[string]string{"WAYLAND_DISPLAY": "wayland-0"},
			tools: map[string]bool{"wl-paste": true},
			runStub: func(name string, args []string) ([]byte, error) {
				return []byte("text/plain\n"), nil
			},
			wantErr:   "no image on the clipboard",
			wantNoImg: true,
		},
		{
			name:  "xclip when WAYLAND_DISPLAY is empty",
			goos:  "linux",
			env:   map[string]string{"WAYLAND_DISPLAY": ""},
			tools: map[string]bool{"xclip": true},
			runStub: func(name string, args []string) ([]byte, error) {
				if name != "xclip" {
					return nil, fmt.Errorf("unexpected command %q", name)
				}
				switch {
				case reflect.DeepEqual(args, []string{"-selection", "clipboard", "-t", "TARGETS", "-o"}):
					// jpeg is listed too, but png must win the preference order.
					return []byte("image/jpeg\nimage/png\n"), nil
				case reflect.DeepEqual(args, []string{"-selection", "clipboard", "-t", "image/png", "-o"}):
					return pngBytes, nil
				}
				return nil, fmt.Errorf("unexpected xclip args %v", args)
			},
			want: pngBytes,
			wantCalls: []call{
				{name: "xclip", args: []string{"-selection", "clipboard", "-t", "TARGETS", "-o"}},
				{name: "xclip", args: []string{"-selection", "clipboard", "-t", "image/png", "-o"}},
			},
		},
		{
			name:      "no image-capable tool found",
			goos:      "linux",
			env:       map[string]string{"WAYLAND_DISPLAY": ""},
			tools:     map[string]bool{},
			wantErr:   "no image-capable clipboard tool found (install wl-clipboard or xclip)",
			wantNoImg: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			origRun, origLookPath, origGOOS, origIsWSL := run, lookPath, goos, isWSL
			t.Cleanup(func() {
				run, lookPath, goos, isWSL = origRun, origLookPath, origGOOS, origIsWSL
			})

			var calls []call
			run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
				calls = append(calls, call{name: name, args: args})
				if tc.runStub == nil {
					return nil, fmt.Errorf("unexpected command %q", name)
				}
				return tc.runStub(name, args)
			}
			lookPath = func(name string) (string, error) {
				if tc.tools[name] {
					return "/usr/bin/" + name, nil
				}
				return "", fmt.Errorf("exec: %q not found in PATH", name)
			}
			goos = tc.goos
			isWSL = func() bool { return tc.wsl }

			got, err := ReadImage(context.Background())

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("ReadImage() = %q, want error containing %q", got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ReadImage() error = %q, want substring %q", err, tc.wantErr)
				}
				if errors.Is(err, ErrNoImage) != tc.wantNoImg {
					t.Fatalf("ReadImage() error = %v, want ErrNoImage match = %v", err, tc.wantNoImg)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadImage() error = %v", err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("ReadImage() = %x, want %x", got, tc.want)
			}
			for _, wantCall := range tc.wantCalls {
				found := false
				for _, c := range calls {
					if c.name == wantCall.name && reflect.DeepEqual(c.args, wantCall.args) {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("wanted call %q %v; got calls: %+v", wantCall.name, wantCall.args, calls)
				}
			}
		})
	}
}
