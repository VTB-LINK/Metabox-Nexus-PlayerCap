package cloudmusic

import (
	"testing"

	"Metabox-Nexus-PlayerCap/player/cloudmusic/cdp"
)

func TestSnapshotMatchesCurrentSong(t *testing.T) {
	tests := []struct {
		name       string
		snapshot   *cdp.ExtractionData
		songName   string
		songArtist string
		want       bool
	}{
		{
			name:       "exact match",
			snapshot:   newSnapshot("Blue Bird", "Ikimono Gakari"),
			songName:   "Blue Bird",
			songArtist: "Ikimono Gakari",
			want:       true,
		},
		{
			name:       "normalize whitespace and case",
			snapshot:   newSnapshot("  Blue   Bird  ", "IKIMONO   GAKARI"),
			songName:   "blue bird",
			songArtist: "ikimono gakari",
			want:       true,
		},
		{
			name:       "stale redux snapshot is rejected",
			snapshot:   newSnapshot("Old Song", "Old Artist"),
			songName:   "New Song",
			songArtist: "New Artist",
			want:       false,
		},
		{
			name:       "same song different artist is rejected",
			snapshot:   newSnapshot("Same Name", "Artist A"),
			songName:   "Same Name",
			songArtist: "Artist B",
			want:       false,
		},
		{
			name:       "nil snapshot",
			snapshot:   nil,
			songName:   "Blue Bird",
			songArtist: "Ikimono Gakari",
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := snapshotMatchesCurrentSong(tt.snapshot, tt.songName, tt.songArtist); got != tt.want {
				t.Fatalf("snapshotMatchesCurrentSong() = %v, want %v", got, tt.want)
			}
		})
	}
}

func newSnapshot(songName, artist string) *cdp.ExtractionData {
	data := &cdp.ExtractionData{CurPlaying: &cdp.CurPlayingObj{}}
	data.CurPlaying.Track.Name = songName
	if artist != "" {
		data.CurPlaying.Track.Artists = []struct {
			Name string `json:"name"`
		}{{Name: artist}}
	}
	return data
}
