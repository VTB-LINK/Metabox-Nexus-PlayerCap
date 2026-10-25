package cdp

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"testing"
)

// Captured 2026-10-23 from the running 9.6.0.0_W1 CEF bridge, using only
// GetPlayStatus/GetNowplaySong/GetVer. The single captured status is reused on
// both sides here to test decoding, not to claim an atomic runtime observation.
func TestCapturedBridgeMetadata(t *testing.T) {
	b, err := os.ReadFile("testdata/bridge-9.6.0.0_W1.json")
	if err != nil {
		t.Fatal(err)
	}
	var raw struct{ Status, Song, Version string }
	if err = json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	// The capture is a W1 record; the accepted suffix set is wider (bridgeBuild).
	if build, ok := bridgeBuild(raw.Version); !ok || build != "W1" {
		t.Fatalf("fixture version %q", raw.Version)
	}
	s, err := parseSnapshot(raw.Status, raw.Song, raw.Status)
	if err != nil || s.RID != "MUSIC_398013033" || s.Status != 1 || s.Title != "白鸽乌鸦相爱的戏码" || s.Artist != "潘成（皮卡潘）" || s.Album != "白鸽乌鸦相爱的戏码" {
		t.Fatalf("captured metadata: %+v %v", s, err)
	}
}

func encodedSong(rid, title, artist string) string {
	inner := url.Values{"rid": {rid}, "name": {title}, "artist": {artist}, "album": {"专辑 + & 100%"}, "list": {"正在播放"}}.Encode()
	return url.Values{"playtype": {"0"}, "quality": {"2"}, "song": {inner}}.Encode()
}

func TestSnapshotPreservesNestedEscapes(t *testing.T) {
	raw := encodedSong("MUSIC_568213041", "A & B=你好 + 100%", "X/Y & Z")
	s, err := parseSnapshot("Status=1&SongId=568213041", raw, "Status=1&SongId=568213041")
	if err != nil || s.RID != "MUSIC_568213041" || s.Title != "A & B=你好 + 100%" || s.Artist != "X/Y & Z" || s.Album != "专辑 + & 100%" || s.Status != 1 {
		t.Fatalf("got %+v, %v", s, err)
	}
}

func TestSnapshotRejectsMixedIdentityAndMalformedFields(t *testing.T) {
	for _, tc := range []struct {
		name, before, song, after string
		want                      error
	}{
		{"changed song", "Status=1&SongId=1", encodedSong("MUSIC_1", "one", "artist"), "Status=1&SongId=2", ErrSongChanged},
		{"changed status", "Status=1&SongId=1", encodedSong("MUSIC_1", "one", "artist"), "Status=2&SongId=1", ErrSongChanged},
		{"stale metadata", "Status=1&SongId=1", encodedSong("MUSIC_2", "two", "artist"), "Status=1&SongId=1", ErrSongChanged},
		// Captured live on every song switch: both status reads return "" for
		// 0.2-0.5s while the song call already shows the new song. A state change in
		// progress (retried), not a protocol violation. 变异自证：去掉 parseSnapshot
		// 开头的空串判定，这三例变红（实测）。
		{"switching: both statuses empty", "", encodedSong("MUSIC_2", "two", "artist"), "", ErrSongChanged},
		{"switching: first status empty", "", encodedSong("MUSIC_2", "two", "artist"), "Status=1&SongId=2", ErrSongChanged},
		{"switching: last status empty", "Status=1&SongId=2", encodedSong("MUSIC_2", "two", "artist"), "", ErrSongChanged},
		// An empty side must not mask a malformed other side, in either direction.
		// 变异自证：恢复“任一侧为空即返回”的短路写法，这四例变红（实测）。
		{"empty first, malformed last (bad rid)", "", encodedSong("MUSIC_2", "two", "artist"), "Status=1&SongId=a", ErrUnavailable},
		{"malformed first (bad rid), empty last", "Status=1&SongId=a", encodedSong("MUSIC_2", "two", "artist"), "", ErrUnavailable},
		{"empty first, duplicate key last", "", encodedSong("MUSIC_2", "two", "artist"), "Status=1&Status=3&SongId=2", ErrUnavailable},
		{"blank first, empty last", " ", encodedSong("MUSIC_2", "two", "artist"), "", ErrUnavailable},
		// Only a truly empty status is transient: blank or malformed stays a violation.
		{"blank status is not empty", " ", encodedSong("MUSIC_2", "two", "artist"), " ", ErrUnavailable},
		{"status without keys", "x", encodedSong("MUSIC_2", "two", "artist"), "x", ErrUnavailable},
		{"bad percent", "Status=1&SongId=1", "song=%XX", "Status=1&SongId=1", ErrUnavailable},
		{"duplicate state", "Status=1&Status=3&SongId=1", "", "Status=1&SongId=1", ErrUnavailable},
		{"bad rid", "Status=1&SongId=a", "", "Status=1&SongId=a", ErrUnavailable},
		{"empty metadata", "Status=1&SongId=1", "", "Status=1&SongId=1", ErrUnavailable},
		{"invalid UTF8", "Status=1&SongId=1", encodedSong("MUSIC_1", string([]byte{255}), "artist"), "Status=1&SongId=1", ErrUnavailable},
		{"NUL in title", "Status=1&SongId=1", encodedSong("MUSIC_1", "title\x00tail", "artist"), "Status=1&SongId=1", ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseSnapshot(tc.before, tc.song, tc.after)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
		})
	}
}

func TestSnapshotIdleAndUnknownStatusRemainRaw(t *testing.T) {
	s, err := parseSnapshot("Status=3&SongId=", "", "Status=3&SongId=")
	if err != nil || s.RID != "" || s.Status != 3 {
		t.Fatalf("idle %+v %v", s, err)
	}
	s, err = parseSnapshot("Status=99&SongId=1", encodedSong("MUSIC_1", "one", "a"), "Status=99&SongId=1")
	if err != nil || s.Status != 99 {
		t.Fatalf("unknown status was interpreted %+v %v", s, err)
	}
}
