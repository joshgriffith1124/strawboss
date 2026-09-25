// Package clipboard reads images from the system clipboard by shelling out to platform tools.
package clipboard

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// ErrNoImage means the clipboard holds no image (maybe text, maybe empty).
var ErrNoImage = errors.New("no image on the clipboard")

// run is the command runner; tests replace it. It returns stdout.
var run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// lookPath is exec.LookPath; tests replace it.
var lookPath = exec.LookPath

// goos is runtime.GOOS; tests replace it.
var goos = runtime.GOOS

// isWSL reports whether we are running under WSL; tests replace it.
var isWSL = func() bool {
	return os.Getenv("WSL_DISTRO_NAME") != "" || fileExists("/proc/sys/fs/binfmt_misc/WSLInterop")
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func have(name string) bool {
	_, err := lookPath(name)
	return err == nil
}

// imageTypes lists clipboard image MIME types in preference order — only
// ones Fit can decode (no webp: the standard library has no decoder).
var imageTypes = []string{"image/png", "image/jpeg", "image/gif"}

const wslScript = "Add-Type -AssemblyName System.Windows.Forms,System.Drawing; $i=[System.Windows.Forms.Clipboard]::GetImage(); if($i){$m=New-Object System.IO.MemoryStream; $i.Save($m,[System.Drawing.Imaging.ImageFormat]::Png); [Convert]::ToBase64String($m.ToArray())}"

// ReadImage returns the clipboard image's encoded bytes (usually PNG).
func ReadImage(ctx context.Context) ([]byte, error) {
	var fetch func(context.Context) ([]byte, error)
	switch {
	case goos == "darwin":
		fetch = readDarwin
	case isWSL() && have("powershell.exe"):
		fetch = readWSL
	case os.Getenv("WAYLAND_DISPLAY") != "" && have("wl-paste"):
		fetch = readWlPaste
	case have("xclip"):
		fetch = readXClip
	default:
		return nil, fmt.Errorf("clipboard: no image-capable clipboard tool found (install wl-clipboard or xclip)")
	}
	data, err := fetch(ctx)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, ErrNoImage
	}
	return data, nil
}

// readDarwin uses osascript, which returns `«data PNGf<hex>»` for image clipboards.
func readDarwin(ctx context.Context) ([]byte, error) {
	out, err := run(ctx, "osascript", "-e", "the clipboard as «class PNGf»")
	if err != nil {
		return nil, ErrNoImage
	}
	s := strings.TrimSpace(string(out))
	i := strings.Index(s, "«data PNGf")
	if i < 0 {
		return nil, ErrNoImage
	}
	payload := s[i+len("«data PNGf"):]
	if j := strings.Index(payload, "»"); j >= 0 {
		payload = payload[:j]
	}
	data, err := hex.DecodeString(strings.TrimSpace(payload))
	if err != nil {
		return nil, fmt.Errorf("clipboard: darwin: %w", err)
	}
	return data, nil
}

// readWSL grabs the image from the Windows clipboard via PowerShell (base64 PNG out).
func readWSL(ctx context.Context) ([]byte, error) {
	out, err := run(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-STA", "-Command", wslScript)
	if err != nil {
		return nil, fmt.Errorf("clipboard: wsl: %w", err)
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return nil, ErrNoImage
	}
	data, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("clipboard: wsl: %w", err)
	}
	return data, nil
}

// readWlPaste reads via wl-paste on Wayland.
func readWlPaste(ctx context.Context) ([]byte, error) {
	out, err := run(ctx, "wl-paste", "--list-types")
	if err != nil {
		return nil, fmt.Errorf("clipboard: wayland: %w", err)
	}
	t := pickType(out)
	if t == "" {
		return nil, ErrNoImage
	}
	data, err := run(ctx, "wl-paste", "--no-newline", "--type", t)
	if err != nil {
		return nil, fmt.Errorf("clipboard: wayland: %w", err)
	}
	return data, nil
}

// readXClip reads via xclip on X11.
func readXClip(ctx context.Context) ([]byte, error) {
	out, err := run(ctx, "xclip", "-selection", "clipboard", "-t", "TARGETS", "-o")
	if err != nil {
		return nil, fmt.Errorf("clipboard: x11: %w", err)
	}
	t := pickType(out)
	if t == "" {
		return nil, ErrNoImage
	}
	data, err := run(ctx, "xclip", "-selection", "clipboard", "-t", t, "-o")
	if err != nil {
		return nil, fmt.Errorf("clipboard: x11: %w", err)
	}
	return data, nil
}

// pickType returns the first of imageTypes present in the listed MIME types, or "".
func pickType(list []byte) string {
	seen := make(map[string]bool)
	for _, line := range strings.Split(string(list), "\n") {
		seen[strings.TrimSpace(line)] = true
	}
	for _, t := range imageTypes {
		if seen[t] {
			return t
		}
	}
	return ""
}
