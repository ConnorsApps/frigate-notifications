package notifier

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ConnorsApps/frigate-notifications/internal/config"
	"github.com/ConnorsApps/frigate-notifications/internal/media"
	"github.com/ConnorsApps/frigate-notifications/internal/rules"
)

func TestFirstSentence(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{name: "empty", in: "", max: 140, want: ""},
		{name: "no terminator, under max", in: "walking toward the door", max: 140, want: "walking toward the door"},
		{name: "trims surrounding space", in: "  approaching the porch  ", max: 140, want: "approaching the porch"},
		{
			name: "cuts after the first sentence",
			in:   "A person walks up the driveway. They try the door handle, then leave.",
			max:  140,
			want: "A person walks up the driveway.",
		},
		{name: "an abbreviation is not a sentence break", in: "Mr. Smith walks up. He knocks.", max: 140, want: "Mr. Smith walks up."},
		{name: "a lone initial-style word still ends a sentence", in: "It is him. Then more.", max: 140, want: "It is him."},
		{
			name: "truncation cuts at a word boundary",
			in:   "A person in a long dark coat walks slowly up the driveway toward the door",
			max:  30,
			want: "A person in a long dark coat…",
		},
		{name: "first of several with mixed terminators", in: "Someone's here! Is it the mail?", max: 140, want: "Someone's here!"},
		{
			name: "a decimal point is not a sentence break",
			in:   "A person about 1.8 m tall is walking.",
			max:  140,
			want: "A person about 1.8 m tall is walking.",
		},
		{
			name: "long single sentence is truncated with an ellipsis",
			in:   strings.Repeat("a ", 100),
			max:  20,
			want: strings.TrimSpace(strings.Repeat("a ", 10)) + "…",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := firstSentence(tc.in, tc.max); got != tc.want {
				t.Errorf("firstSentence(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
		})
	}
}

// fakeProber answers per kind and records what it was asked, so tests can
// assert both the choice and that nothing past the winner was probed.
type fakeProber struct {
	errs  map[media.Kind]error
	asked []media.Kind
}

func (f *fakeProber) Check(_ context.Context, kind media.Kind, _ string, _ int64) error {
	f.asked = append(f.asked, kind)
	return f.errs[kind]
}

func TestAutoMediaSelection(t *testing.T) {
	errGone := errors.New("upstream 404")

	tests := []struct {
		name      string
		phase     rules.Phase
		preset    config.Preset
		errs      map[media.Kind]error
		wantVideo string // substring of the video link, "" for none
		wantImage string // substring of the image link, "" for none
		wantClip  bool
		wantAsked []media.Kind
	}{
		{
			name: "end resolves the clip and a still", phase: rules.PhaseEnd,
			wantVideo: "/m/clip/", wantImage: "/m/preview/", wantClip: true,
			wantAsked: []media.Kind{media.KindClip, media.KindPreview},
		},
		{
			name: "clip too big for iOS falls back to the gif but stays linkable", phase: rules.PhaseEnd,
			errs:      map[media.Kind]error{media.KindClip: media.ErrTooLarge},
			wantImage: "/m/preview/", wantClip: true,
			wantAsked: []media.Kind{media.KindClip, media.KindPreview},
		},
		{
			name: "missing clip falls back to the gif and is not linked", phase: rules.PhaseEnd,
			errs:      map[media.Kind]error{media.KindClip: errGone},
			wantImage: "/m/preview/",
			wantAsked: []media.Kind{media.KindClip, media.KindPreview},
		},
		{
			name: "no clip or gif falls back to the snapshot", phase: rules.PhaseEnd,
			errs:      map[media.Kind]error{media.KindClip: errGone, media.KindPreview: errGone},
			wantImage: "/m/snapshot/",
			wantAsked: []media.Kind{media.KindClip, media.KindPreview, media.KindSnapshot},
		},
		{
			name: "nothing available is text", phase: rules.PhaseEnd,
			errs:      map[media.Kind]error{media.KindClip: errGone, media.KindPreview: errGone, media.KindSnapshot: errGone},
			wantAsked: []media.Kind{media.KindClip, media.KindPreview, media.KindSnapshot},
		},
		{
			name: "new sends the snapshot and never asks for a clip", phase: rules.PhaseNew,
			wantImage: "/m/snapshot/",
			wantAsked: []media.Kind{media.KindSnapshot},
		},
		{
			name: "text preset attaches nothing but still links the clip", phase: rules.PhaseEnd, preset: config.PresetText,
			wantClip:  true,
			wantAsked: []media.Kind{media.KindClip},
		},
		{
			name: "liveview picks media like auto", phase: rules.PhaseEnd, preset: config.PresetLiveView,
			wantVideo: "/m/clip/", wantImage: "/m/preview/", wantClip: true,
			wantAsked: []media.Kind{media.KindClip, media.KindPreview},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t)
			n, _ := newTestNotifier(t, cfg)
			prober := &fakeProber{errs: tc.errs}
			n.prober = prober

			rule := cfg.Rules[0]
			if tc.preset != "" {
				rule.Preset = tc.preset
			}
			review := loadReview(t, "review-end.json").After
			eventID := review.PrimaryEventID()

			c := n.buildContent(context.Background(), review, rule, tc.phase, "tag", eventID, "")

			check := func(what, got, want string) {
				t.Helper()
				if (want == "") != (got == "") || !strings.Contains(got, want) {
					t.Errorf("%s = %q, want it to contain %q", what, got, want)
				}
			}
			check("video", c.Video, tc.wantVideo)
			check("image", c.Image, tc.wantImage)
			if (c.ClipURL != "") != tc.wantClip {
				t.Errorf("clipURL = %q, want present=%v", c.ClipURL, tc.wantClip)
			}
			if !slices.Equal(prober.asked, tc.wantAsked) {
				t.Errorf("probed %v, want %v", prober.asked, tc.wantAsked)
			}
		})
	}
}

// The clip covers the whole review, not just its first detection.
func TestClipLinkCoversTheReview(t *testing.T) {
	cfg := testConfig(t)
	n, _ := newTestNotifier(t, cfg)
	review := loadReview(t, "review-end.json").After

	c := n.buildContent(context.Background(), review, cfg.Rules[0], rules.PhaseEnd, "tag", review.PrimaryEventID(), "")

	// review-end.json: garage, start 1787614651, end 1787614663, padded 2s/3s.
	if !strings.Contains(c.Video, "/m/clip/1787614649-1787614666-garage.mp4") {
		t.Errorf("video = %q, want a review-span clip id", c.Video)
	}
}
