package cover

// Explicit opt-in integration probe: fetches one known RID and verifies that
// the production downloader returns decodable, normalized 800x800 artwork.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"image"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLiveFetch(t *testing.T) {
	if os.Getenv("PLAYERCAP_LIVE") != "1" {
		t.Skip("set PLAYERCAP_LIVE=1 KUWO_RID=MUSIC_<id> for an online cover probe")
	}
	rid := os.Getenv("KUWO_RID")
	if rid == "" {
		t.Fatal("KUWO_RID required")
	}
	result, err := Fetch(context.Background(), rid, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, encoded, ok := strings.Cut(result.DataURI, ",")
	if !ok {
		t.Fatal("missing data URI payload")
	}
	b, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil || cfg.Width != 800 || cfg.Height != 800 {
		t.Fatalf("decoded image %dx%d %s: %v", cfg.Width, cfg.Height, format, err)
	}
	t.Logf("RID=%s URL=%s format=%s dimensions=%dx%d bytes=%d sha256=%x", result.RID, result.URL, format, cfg.Width, cfg.Height, len(b), sha256.Sum256(b))
}
