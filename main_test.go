package main

import "testing"

func TestNormalizeSemver(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "empty", input: "", want: ""},
		{name: "stable", input: "3.0.0", want: "v3.0.0"},
		{name: "prefixed", input: "v3.0.0-beta.32", want: "v3.0.0-beta.32"},
		{name: "trim spaces", input: " 3.1.0-alpha.13 ", want: "v3.1.0-alpha.13"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeSemver(tt.input); got != tt.want {
				t.Fatalf("normalizeSemver(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestIsReleaseVersion(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{name: "default dev version", input: "0.0.0", want: false},
		{name: "stable", input: "3.0.0", want: true},
		{name: "prerelease", input: "3.0.0-beta.32", want: true},
		{name: "rc", input: "v3.0.0-rc.1.a", want: true},
		{name: "invalid dev build", input: "Metabox-Nexus-PlayerCap-20260426-deadbee", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isReleaseVersion(tt.input); got != tt.want {
				t.Fatalf("isReleaseVersion(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestIsForceReleaseName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{name: "plain", input: "v3.0.0-beta.32", want: false},
		{name: "forced", input: "v3.0.0-beta.32-force", want: true},
		{name: "forced uppercase", input: "v3.0.0-beta.32-FORCE", want: true},
		{name: "forced trim spaces", input: " v3.0.0-rc.1.a-force ", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isForceReleaseName(tt.input); got != tt.want {
				t.Fatalf("isForceReleaseName(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestDecideUpdate(t *testing.T) {
	tests := []struct {
		name        string
		current     string
		target      string
		releaseName string
		wantUpdate  bool
		wantReason  string
		wantErr     bool
	}{
		{name: "same stable", current: "3.0.0", target: "v3.0.0", releaseName: "v3.0.0", wantUpdate: false, wantReason: updateReasonSameVersion},
		{name: "same prerelease", current: "3.0.0-beta.32", target: "v3.0.0-beta.32", releaseName: "v3.0.0-beta.32-force", wantUpdate: false, wantReason: updateReasonSameVersion},
		{name: "alpha to beta", current: "3.0.0-alpha.7", target: "v3.0.0-beta.1", releaseName: "v3.0.0-beta.1", wantUpdate: true, wantReason: updateReasonNewerTarget},
		{name: "beta to rc", current: "3.0.0-beta.32", target: "v3.0.0-rc.1", releaseName: "v3.0.0-rc.1", wantUpdate: true, wantReason: updateReasonNewerTarget},
		{name: "rc to stable", current: "3.0.0-rc.1.a", target: "v3.0.0", releaseName: "v3.0.0", wantUpdate: true, wantReason: updateReasonNewerTarget},
		{name: "beta to next minor alpha", current: "3.0.0-beta.32", target: "v3.1.0-alpha.13", releaseName: "v3.1.0-alpha.13", wantUpdate: true, wantReason: updateReasonNewerTarget},
		{name: "stable downgrade blocked", current: "3.1.0", target: "v3.0.0", releaseName: "v3.0.0", wantUpdate: false, wantReason: updateReasonOlderTargetBlocked},
		{name: "stable downgrade forced", current: "3.1.0", target: "v3.0.0", releaseName: "v3.0.0-force", wantUpdate: true, wantReason: updateReasonOlderTargetForced},
		{name: "prerelease downgrade forced", current: "3.0.0-rc.1", target: "v3.0.0-beta.32", releaseName: "v3.0.0-beta.32-force", wantUpdate: true, wantReason: updateReasonOlderTargetForced},
		{name: "invalid current", current: "0.0.0", target: "v3.0.0", releaseName: "v3.0.0", wantErr: true},
		{name: "invalid target", current: "3.0.0", target: "latest", releaseName: "latest", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotUpdate, gotReason, err := decideUpdate(tt.current, tt.target, tt.releaseName)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("decideUpdate(%q, %q, %q) error = nil, want error", tt.current, tt.target, tt.releaseName)
				}
				return
			}
			if err != nil {
				t.Fatalf("decideUpdate(%q, %q, %q) error = %v", tt.current, tt.target, tt.releaseName, err)
			}
			if gotUpdate != tt.wantUpdate || gotReason != tt.wantReason {
				t.Fatalf("decideUpdate(%q, %q, %q) = (%v, %q), want (%v, %q)", tt.current, tt.target, tt.releaseName, gotUpdate, gotReason, tt.wantUpdate, tt.wantReason)
			}
		})
	}
}
