// genicon writes the Duolink icon files used for the .exe resources and docs.
// Run from the repo root: go run ./cmd/genicon
package main

import (
	"image/png"
	"log"
	"os"
	"path/filepath"

	"github.com/Wandraker/duolink/internal/icon"
)

func main() {
	dir := filepath.Join("cmd", "duolink", "winres")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "icon.ico"), icon.ICO(true, 16, 24, 32, 48, 64, 128, 256), 0o644); err != nil {
		log.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, "icon.png"))
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, icon.Render(256, true)); err != nil {
		log.Fatal(err)
	}
}
